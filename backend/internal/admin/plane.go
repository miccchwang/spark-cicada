// Package admin —— M-ADMIN 集成控制面（Integration Control Plane）。
//
// 职责（docs/02 M-ADMIN、docs/05 G4/G6/G9）：
//   * 注册/启停**数据槽**（slot）与**算法**（algo），并检测两者解耦（G4）；
//   * 版本漂移检测与**受影响桶**重算触发（G6）；
//   * 模块挂载/停用（G9，零改主机）；
//   * 一切变更**写入审计**（G10，append-only）。
//
// **红线纪律**：
//   * 算法公式**不得**内嵌数据源（G4）：含 source/slot/table/FROM 等词即拒绝登记。
//   * 槽状态 MISSING ⇒ 依赖它的算法不可启用（fail-closed 传播）。
//   * 方案不达覆盖率门限 ⇒ 禁用而非静默降级（不静默用旧值）。
package admin

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/miccchwang/spark-cicada/backend/internal/gate"
)

// ───────────────────────────── 领域模型 ─────────────────────────────

// Slot 数据槽（「数据从哪来」的唯一声明）。
type Slot struct {
	ID            string
	Name          string
	SourceKind    string // api | master_table | db | file
	SourceRef     string
	KeyStrategy   string
	CoverageGate  float64 // 门限，默认 0.80
	Freshness     string
	Permission    string // L1..L4
	Status        string // ACTIVE | MISSING | DEGRADED | DISABLED
	Notes         string
	UpdatedAt     time.Time
}

// Algorithm 算法（纯公式，**不含**数据源）。
type Algorithm struct {
	ID             string
	Name           string
	Version        int
	Formula        string   // 纯公式；不得出现数据源引用
	Unit           string
	Permission     string
	DependsOnSlots []string // 引用 Slot.ID
	WritesBucket   string
	MissingPolicy  string // skip | null | error
	Trace          bool
	UpdatedAt      time.Time
}

// RuleSet 规则集（费率/口径），带版本。
type RuleSet struct {
	ID        string
	Version   int
	Scope     map[string]string
	Items     []RuleItem
	UpdatedAt time.Time
}

// RuleItem 单条规则项。
type RuleItem struct {
	ID            string
	Name          string
	Rate          *float64
	FlatPerOrder  *float64
	EffectiveFrom string
}

// AuditFunc 审计写入回调（由装配层接到 audit 包；测试可注入内存实现）。
type AuditFunc func(ctx context.Context, actor, action, target string, detail map[string]any) error

// Store 是控制面的持久化抽象（Postgres 实现见 store 包；测试用内存实现）。
type Store interface {
	UpsertSlot(ctx context.Context, s Slot) error
	GetSlot(ctx context.Context, id string) (Slot, bool, error)
	ListSlots(ctx context.Context) ([]Slot, error)

	UpsertAlgorithm(ctx context.Context, a Algorithm) error
	GetAlgorithm(ctx context.Context, id string) (Algorithm, bool, error)
	ListAlgorithms(ctx context.Context) ([]Algorithm, error)

	UpsertRuleSet(ctx context.Context, r RuleSet) error

	BucketAlgoVersions(ctx context.Context, bucket string) (map[string]int, error)
	MarkBucketStale(ctx context.Context, bucket string, reason string) error
}

// Plane 集成控制面。
type Plane struct {
	Store Store
	Audit AuditFunc
	// Registry 模块注册表（G9）。可为 nil（不启用模块管理）。
	Registry *gate.Registry

	mu sync.Mutex
}

// New 构造控制面。
func New(s Store, audit AuditFunc) *Plane {
	if audit == nil {
		audit = func(context.Context, string, string, string, map[string]any) error { return nil }
	}
	return &Plane{Store: s, Audit: audit}
}

// ───────────────────────────── G4：算法/数据源解耦 ─────────────────────────────

// dataSourceLeakRe 命中「公式里出现数据源」的迹象（G4 红线）。
//
// 例如 gp 公式写成 "revenue - cogs FROM fact_sales_daily" 即违规。
var dataSourceLeakRe = regexp.MustCompile(`(?i)\b(from|join|select|table|slot\.|api\.|db\.|http|s3://|gs://)\b`)

// CheckFormulaNoDataSource 断言公式不含数据源（G4）。
//
// 返回的 []string 为违规命中（空 = 通过）。
func CheckFormulaNoDataSource(formula string) []string {
	var out []string
	for _, m := range dataSourceLeakRe.FindAllString(formula, -1) {
		out = append(out, strings.ToLower(m))
	}
	return dedupe(out)
}

// ValidateAlgorithm 登记前校验（G4 + fail-closed 传播）。
//
// 校验项：
//  1. 公式非空且不含数据源；
//  2. 每个 dependsOn 槽**必须已注册**（否则拒绝：算法引用了不存在的数据源）；
//  3. 依赖槽全部非 MISSING/DISABLED（否则拒绝启用 —— fail-closed 传播）。
func (p *Plane) ValidateAlgorithm(ctx context.Context, a Algorithm) error {
	if strings.TrimSpace(a.ID) == "" {
		return fmt.Errorf("admin: 算法 ID 不能为空")
	}
	if strings.TrimSpace(a.Formula) == "" {
		return fmt.Errorf("admin: 算法 %s 公式为空（G4）", a.ID)
	}
	if leaks := CheckFormulaNoDataSource(a.Formula); len(leaks) > 0 {
		return fmt.Errorf("admin: 算法 %s 公式疑似内嵌数据源 %v —— 违反算法/数据源解耦（G4）", a.ID, leaks)
	}
	for _, slotID := range a.DependsOnSlots {
		s, ok, err := p.Store.GetSlot(ctx, slotID)
		if err != nil {
			return fmt.Errorf("admin: 读取槽 %s 失败: %w", slotID, err)
		}
		if !ok {
			return fmt.Errorf("admin: 算法 %s 依赖未注册的槽 %s（拒绝登记）", a.ID, slotID)
		}
		if s.Status == "MISSING" || s.Status == "DISABLED" {
			return fmt.Errorf(
				"admin: 算法 %s 依赖的槽 %s 状态为 %s —— 不可启用（fail-closed 传播，缺失不可当 0）",
				a.ID, slotID, s.Status)
		}
	}
	// 4. 版本号必须显式声明且为正整数（G4 第十侧）。
	//
	// ★ 此前本方法对 Version **零校验**，而 RegisterAlgorithm 在 Version<=0 时
	// 静默把它写成 1 —— 那是**缺省填充，不是校验**：它让「没写版本」与
	// 「版本就是 1」在行为上无法区分。版本是 G6「改了公式要重算哪些桶」的
	// **唯一判据**（gate.VersionDrift / AffectedBuckets），缺失或自由值会让
	// 漂移检测静默失真。现改为显式拒绝。
	if a.Version <= 0 {
		return fmt.Errorf(
			"admin: 算法 %s 的版本号必须为正整数（得 %d）—— 版本是 G6 漂移检测的唯一判据，"+
				"不得缺省填充（0 与『未声明』无法区分）", a.ID, a.Version)
	}
	// 5. 版本只能前进（G4 第十侧**唯一有依据的比较**）。
	//
	// ★ 版本的全部意义在于比较：G6 靠版本差判断「改了公式要重算哪些桶」。
	// 允许倒退（3 → 2）会让已重算过的桶被判为「版本超前」，重算结论随之失真。
	// 首次登记（store 里查不到）⇒ 无比较基准，放行。
	prev, exists, err := p.Store.GetAlgorithm(ctx, a.ID)
	if err != nil {
		return fmt.Errorf("admin: 读取既有算法 %s 失败: %w", a.ID, err)
	}
	if exists {
		if v := gate.CheckAlgorithmVersionNotRegressing(a.ID, prev.Version, a.Version); len(v) > 0 {
			return fmt.Errorf("admin: %s", strings.Join(v, "；"))
		}
	}
	return nil
}

// RegisterAlgorithm 登记算法（先校验，后落库，再审计）。
func (p *Plane) RegisterAlgorithm(ctx context.Context, actor string, a Algorithm) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if err := p.ValidateAlgorithm(ctx, a); err != nil {
		return err
	}
	// 版本号**不再**静默填 1：ValidateAlgorithm 已显式拒绝 Version<=0。
	// （缺省填充会让「没写版本」与「版本就是 1」在行为上无法区分，
	// 而版本是 G6 漂移检测的唯一判据。）
	if a.MissingPolicy == "" {
		a.MissingPolicy = "skip"
	}
	a.UpdatedAt = time.Now().UTC()
	if err := p.Store.UpsertAlgorithm(ctx, a); err != nil {
		return fmt.Errorf("admin: 落库算法 %s 失败: %w", a.ID, err)
	}
	return p.Audit(ctx, actor, "admin.algorithm.register", a.ID, map[string]any{
		"version":     a.Version,
		"writesBucket": a.WritesBucket,
		"slots":       a.DependsOnSlots,
	})
}

// RegisterSlot 登记/更新数据槽。
//
// 红线：门限必须在 (0,1]；MISSING 槽可登记（表示「已知但不可用」），
// 但登记时必须显式标注，避免被当成可用。
func (p *Plane) RegisterSlot(ctx context.Context, actor string, s Slot) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if strings.TrimSpace(s.ID) == "" {
		return fmt.Errorf("admin: 槽 ID 不能为空")
	}
	if s.ID != "global" && !strings.HasPrefix(s.ID, "slot.") {
		return fmt.Errorf("admin: 槽 ID 需以 slot. 前缀命名（得 %q）", s.ID)
	}
	if s.CoverageGate == 0 {
		s.CoverageGate = 0.80
	}
	if s.CoverageGate <= 0 || s.CoverageGate > 1 {
		return fmt.Errorf("admin: 槽 %s 覆盖率门限须在 (0,1]，得 %v", s.ID, s.CoverageGate)
	}
	if s.Status == "" {
		s.Status = "ACTIVE"
	}
	switch s.Status {
	case "ACTIVE", "MISSING", "DEGRADED", "DISABLED":
	default:
		return fmt.Errorf("admin: 槽 %s 状态非法 %q", s.ID, s.Status)
	}
	s.UpdatedAt = time.Now().UTC()
	if err := p.Store.UpsertSlot(ctx, s); err != nil {
		return fmt.Errorf("admin: 落库槽 %s 失败: %w", s.ID, err)
	}
	return p.Audit(ctx, actor, "admin.slot.register", s.ID, map[string]any{
		"status": s.Status,
		"gate":   s.CoverageGate,
	})
}

// ───────────────────────────── G6：版本漂移与重算 ─────────────────────────────

// DriftReport 一次漂移检测的结果。
type DriftReport struct {
	Bucket          string   `json:"bucket"`
	BucketVersions  map[string]int `json:"bucketVersions"`
	CurrentVersions map[string]int `json:"currentVersions"`
	Drifted         []string `json:"drifted"`
	NeedsRebuild    bool     `json:"needsRebuild"`
}

// DetectDrift 比对桶内算法版本与注册表当前版本（G6-1）。
//
// current 由调用方（控制面从 registry_algorithm 汇总）提供：
// 桶若落后 ⇒ NeedsRebuild=true，并给出漂移的算法清单。
func (p *Plane) DetectDrift(ctx context.Context, bucket string, current map[string]int) (DriftReport, error) {
	bv, err := p.Store.BucketAlgoVersions(ctx, bucket)
	if err != nil {
		return DriftReport{}, fmt.Errorf("admin: 读桶 %s 版本失败: %w", bucket, err)
	}
	drift := gate.VersionDrift(bv, current)
	return DriftReport{
		Bucket:          bucket,
		BucketVersions:  bv,
		CurrentVersions: current,
		Drifted:         drift,
		NeedsRebuild:    len(drift) > 0,
	}, nil
}

// RebuildAffected 标记受影响桶为 STALE（G6-3：仅重算受影响桶）。
//
// 返回被标记的桶（升序）。**不得**标记未受影响的桶（避免全量重刷）。
func (p *Plane) RebuildAffected(ctx context.Context, actor string,
	bucketAlgos map[string][]string, upgradedAlgo string) ([]string, error) {

	affected := gate.AffectedBuckets(bucketAlgos, upgradedAlgo)
	for _, b := range affected {
		if err := p.Store.MarkBucketStale(ctx, b, "算法 "+upgradedAlgo+" 升级"); err != nil {
			return nil, fmt.Errorf("admin: 标记桶 %s STALE 失败: %w", b, err)
		}
	}
	if len(affected) > 0 {
		if err := p.Audit(ctx, actor, "admin.bucket.rebuild", upgradedAlgo,
			map[string]any{"buckets": affected}); err != nil {
			return nil, err
		}
	}
	return affected, nil
}

// ───────────────────────────── G9：模块挂载 ─────────────────────────────

// MountModule 挂载模块（零改主机：仅需 manifest 声明能力）。
func (p *Plane) MountModule(ctx context.Context, actor string, m gate.ModuleManifest) error {
	if p.Registry == nil {
		return fmt.Errorf("admin: 模块注册表未启用")
	}
	if err := p.Registry.Mount(m); err != nil {
		return err
	}
	return p.Audit(ctx, actor, "admin.module.mount", m.ID, map[string]any{
		"requires": m.Requires,
		"optional": m.Optional,
	})
}

// SetModuleEnabled 启停模块（独立停用，不影响他者）。
func (p *Plane) SetModuleEnabled(ctx context.Context, actor, id string, enabled bool) error {
	if p.Registry == nil {
		return fmt.Errorf("admin: 模块注册表未启用")
	}
	if enabled {
		p.Registry.Enable(id)
	} else {
		p.Registry.Disable(id)
	}
	action := "admin.module.disable"
	if enabled {
		action = "admin.module.enable"
	}
	return p.Audit(ctx, actor, action, id, nil)
}

// ───────────────────────────── 只读视图 ─────────────────────────────

// Overview 控制面总览（供 M-ADMIN 首页渲染）。
type Overview struct {
	Slots      []Slot      `json:"slots"`
	Algorithms []Algorithm `json:"algorithms"`
	Modules    []string    `json:"modules"`
}

// Snapshot 汇总当前控制面状态。
func (p *Plane) Snapshot(ctx context.Context) (Overview, error) {
	slots, err := p.Store.ListSlots(ctx)
	if err != nil {
		return Overview{}, err
	}
	algos, err := p.Store.ListAlgorithms(ctx)
	if err != nil {
		return Overview{}, err
	}
	sort.Slice(slots, func(i, j int) bool { return slots[i].ID < slots[j].ID })
	sort.Slice(algos, func(i, j int) bool { return algos[i].ID < algos[j].ID })

	var mods []string
	if p.Registry != nil {
		mods = p.Registry.RenderModules()
	}
	return Overview{Slots: slots, Algorithms: algos, Modules: mods}, nil
}

func dedupe(xs []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range xs {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	sort.Strings(out)
	return out
}
