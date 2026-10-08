// plane_test.go —— 控制面红线断言（内存 Store，无需真库）。
//
// 覆盖闸门：
//   G4  算法/数据源解耦（公式内嵌 source 即拒绝）
//   G4' 依赖未注册槽 ⇒ 拒绝登记
//   G5  fail-closed 传播（依赖 MISSING 槽 ⇒ 拒绝启用）
//   G6  仅重算受影响桶
//   G10 所有变更写审计
package admin

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/miccchwang/spark-cicada/backend/internal/gate"
)

// ── 内存 Store ──

type memStore struct {
	mu       sync.Mutex
	slots    map[string]Slot
	algos    map[string]Algorithm
	rules    map[string]RuleSet
	bucketV  map[string]map[string]int
	staleSet map[string]string
	failNext bool
}

func newMem() *memStore {
	return &memStore{
		slots:    map[string]Slot{},
		algos:    map[string]Algorithm{},
		rules:    map[string]RuleSet{},
		bucketV:  map[string]map[string]int{},
		staleSet: map[string]string{},
	}
}

func (m *memStore) UpsertSlot(_ context.Context, s Slot) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.slots[s.ID] = s
	return nil
}
func (m *memStore) GetSlot(_ context.Context, id string) (Slot, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.slots[id]
	return s, ok, nil
}
func (m *memStore) ListSlots(context.Context) ([]Slot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Slot
	for _, s := range m.slots {
		out = append(out, s)
	}
	return out, nil
}
func (m *memStore) UpsertAlgorithm(_ context.Context, a Algorithm) error {
	if m.failNext {
		m.failNext = false
		return errors.New("模拟落库失败")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.algos[a.ID] = a
	return nil
}
func (m *memStore) GetAlgorithm(_ context.Context, id string) (Algorithm, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.algos[id]
	return a, ok, nil
}
func (m *memStore) ListAlgorithms(context.Context) ([]Algorithm, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Algorithm
	for _, a := range m.algos {
		out = append(out, a)
	}
	return out, nil
}
func (m *memStore) UpsertRuleSet(_ context.Context, r RuleSet) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rules[r.ID] = r
	return nil
}
func (m *memStore) BucketAlgoVersions(_ context.Context, b string) (map[string]int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v := m.bucketV[b]
	if v == nil {
		return map[string]int{}, nil
	}
	return v, nil
}
func (m *memStore) MarkBucketStale(_ context.Context, b, reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.staleSet[b] = reason
	return nil
}

// ── 审计记录器 ──

type auditRec struct {
	mu   sync.Mutex
	rows []string
}

func (a *auditRec) sink(_ context.Context, actor, action, target string, _ map[string]any) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.rows = append(a.rows, actor+"|"+action+"|"+target)
	return nil
}
func (a *auditRec) all() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, len(a.rows))
	copy(out, a.rows)
	return out
}

// ═══════════════════ G4：算法/数据源解耦 ═══════════════════

func TestG4_FormulaWithDataSourceRejected(t *testing.T) {
	bad := []string{
		"revenue - cogs FROM fact_sales_daily",
		"SELECT sum(rev) FROM bucket_pnl_month",
		"slot.cogs * 1.0",
		"fetch('https://api.example.com/cogs')",
	}
	for _, f := range bad {
		if leaks := CheckFormulaNoDataSource(f); len(leaks) == 0 {
			t.Fatalf("G4 失败：公式 %q 内嵌数据源却未被告警", f)
		}
	}
	good := []string{
		"rev - cogs",
		"safe_div(gp, rev)",
		"net_contrib(rev, cogs, platform_fee)",
	}
	for _, f := range good {
		if leaks := CheckFormulaNoDataSource(f); len(leaks) > 0 {
			t.Fatalf("G4 误报：纯公式 %q 被判为含数据源 %v", f, leaks)
		}
	}
}

func TestG4_RegisterAlgorithmRejectsDataSourceLeak(t *testing.T) {
	p := New(newMem(), nil)
	err := p.RegisterAlgorithm(context.Background(), "admin", Algorithm{
		ID:      "algo.gp",
		Name:    "毛利",
		Formula: "rev - cogs FROM fact_sales_daily",
	})
	if err == nil {
		t.Fatal("G4 失败：含数据源的公式竟登记成功")
	}
}

func TestG4_UnregisteredSlotDependencyRejected(t *testing.T) {
	p := New(newMem(), nil)
	err := p.RegisterAlgorithm(context.Background(), "admin", Algorithm{
		ID:             "algo.gp",
		Name:           "毛利",
		Formula:        "rev - cogs",
		DependsOnSlots: []string{"slot.cogs"}, // 从未登记
	})
	if err == nil {
		t.Fatal("G4 失败：依赖未注册槽的算法竟被接受")
	}
}

// ═══════════════════ G5：fail-closed 传播 ═══════════════════

func TestG5_MissingSlotBlocksDependentAlgorithm(t *testing.T) {
	st := newMem()
	p := New(st, nil)
	ctx := context.Background()

	// 槽存在但状态 MISSING（如 slot.affiliate 未接入）
	if err := p.RegisterSlot(ctx, "admin", Slot{
		ID: "slot.affiliate", Name: "联盟佣金", SourceKind: "api",
		SourceRef: "affiliate.v1", KeyStrategy: "date+shop", Status: "MISSING",
	}); err != nil {
		t.Fatalf("登记 MISSING 槽不应失败（应允许声明「已知但不可用」）：%v", err)
	}

	err := p.RegisterAlgorithm(ctx, "admin", Algorithm{
		ID: "algo.net_contrib", Name: "净贡献", Formula: "gp - affiliate_fee",
		DependsOnSlots: []string{"slot.affiliate"},
	})
	if err == nil {
		t.Fatal("G5 失败：依赖 MISSING 槽的算法竟可启用（缺失被当成 0）")
	}
}

func TestG5_ActiveSlotAllowsAlgorithm(t *testing.T) {
	st := newMem()
	p := New(st, nil)
	ctx := context.Background()

	if err := p.RegisterSlot(ctx, "admin", Slot{
		ID: "slot.cogs", Name: "成本", SourceKind: "master_table",
		SourceRef: "master.cogs", KeyStrategy: "sku+month", Status: "ACTIVE",
	}); err != nil {
		t.Fatal(err)
	}
	if err := p.RegisterAlgorithm(ctx, "admin", Algorithm{
		ID: "algo.cogs", Name: "成本", Version: 1, Formula: "unit_cost * qty",
		DependsOnSlots: []string{"slot.cogs"}, WritesBucket: "pnl_month",
	}); err != nil {
		t.Fatalf("ACTIVE 槽应允许登记：%v", err)
	}
}

// ═══════════════════ G6：版本漂移与受影响桶 ═══════════════════

func TestG6_DriftDetectedAndOnlyAffectedRebuilt(t *testing.T) {
	st := newMem()
	// 桶 pnl_month 记的是 algo.gp@2 / algo.cogs@2
	st.bucketV["pnl_month"] = map[string]int{"algo.gp": 2, "algo.cogs": 2}

	aud := &auditRec{}
	p := New(st, aud.sink)
	ctx := context.Background()

	// 注册表已升到 algo.gp@3，cogs 不变
	rep, err := p.DetectDrift(ctx, "pnl_month", map[string]int{"algo.gp": 3, "algo.cogs": 2})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.NeedsRebuild {
		t.Fatal("G6 失败：版本漂移未被检出")
	}
	if len(rep.Drifted) != 1 || !strings.HasPrefix(rep.Drifted[0], "algo.gp:") {
		t.Fatalf("G6 漂移清单不符：%v", rep.Drifted)
	}

	// 仅 algo.gp 升级 ⇒ 只重算产出 gp 的桶
	bucketAlgos := map[string][]string{
		"pnl_month":     {"algo.gp", "algo.cogs"},
		"inventory_snap": {"algo.inventory_stock"}, // 与之无关
	}
	affected, err := p.RebuildAffected(ctx, "admin", bucketAlgos, "algo.gp")
	if err != nil {
		t.Fatal(err)
	}
	if len(affected) != 1 || affected[0] != "pnl_month" {
		t.Fatalf("G6 失败：受影响桶应为 [pnl_month]，得 %v", affected)
	}
	// 未受影响桶**不得**被标记
	if _, marked := st.staleSet["inventory_snap"]; marked {
		t.Fatal("G6 失败：未受影响的桶被误标记为 STALE（全量重刷）")
	}
	if audRows := aud.all(); len(audRows) == 0 {
		t.Fatal("G10 失败：重算未写审计")
	}
}

func TestG6_NoDriftWhenVersionsMatch(t *testing.T) {
	st := newMem()
	st.bucketV["pnl_month"] = map[string]int{"algo.gp": 3}
	p := New(st, nil)
	rep, err := p.DetectDrift(context.Background(), "pnl_month", map[string]int{"algo.gp": 3})
	if err != nil {
		t.Fatal(err)
	}
	if rep.NeedsRebuild {
		t.Fatal("G6 失败：版本一致却判定需重算")
	}
}

// ═══════════════════ G9：模块挂载 ═══════════════════

func TestG9_MountModuleAndDegrade(t *testing.T) {
	reg := gate.NewRegistry([]string{"cap.core"}) // 只有 core 就绪
	aud := &auditRec{}
	p := New(newMem(), aud.sink)
	p.Registry = reg
	ctx := context.Background()

	// 可选依赖缺失 ⇒ 挂载成功但降级
	if err := p.MountModule(ctx, "admin", gate.ModuleManifest{
		ID: "mod.roi", Name: "ROI", Requires: []string{"cap.core"}, Optional: []string{"slot.affiliate"},
	}); err != nil {
		t.Fatalf("G9 失败：可选能力缺失不应导致挂载失败：%v", err)
	}
	// 必需依赖缺失 ⇒ 拒绝挂载（fail-closed）
	if err := p.MountModule(ctx, "admin", gate.ModuleManifest{
		ID: "mod.bad", Name: "坏模块", Requires: []string{"cap.missing"},
	}); err == nil {
		t.Fatal("G9 失败：必需能力缺失却挂载成功")
	}
	// 降级模块仍可渲染（不崩）
	rendered := reg.RenderModules()
	if len(rendered) != 1 {
		t.Fatalf("G9 失败：期望 1 个活跃模块，得 %v", rendered)
	}
	// 独立停用
	if err := p.SetModuleEnabled(ctx, "admin", "mod.roi", false); err != nil {
		t.Fatal(err)
	}
	if got := reg.RenderModules(); len(got) != 0 {
		t.Fatalf("G9 失败：停用后仍渲染 %v", got)
	}
}

// ═══════════════════ G10：审计覆盖 ═══════════════════

func TestG10_AllMutationsAudited(t *testing.T) {
	st := newMem()
	aud := &auditRec{}
	p := New(st, aud.sink)
	ctx := context.Background()

	_ = p.RegisterSlot(ctx, "u1", Slot{ID: "slot.cogs", Name: "成本",
		SourceKind: "db", SourceRef: "x", KeyStrategy: "sku"})
	_ = p.RegisterAlgorithm(ctx, "u1", Algorithm{ID: "algo.cogs", Version: 1, Formula: "a*b",
		DependsOnSlots: []string{"slot.cogs"}})

	rows := aud.all()
	if len(rows) != 2 {
		t.Fatalf("G10 失败：期望 2 条审计（槽+算法），得 %d 条：%v", len(rows), rows)
	}
}

// ═══════════════════ 槽登记校验 ═══════════════════

func TestRegisterSlot_Validation(t *testing.T) {
	p := New(newMem(), nil)
	ctx := context.Background()

	// 前缀不规范
	if err := p.RegisterSlot(ctx, "a", Slot{ID: "cogs", Name: "x",
		SourceKind: "db", SourceRef: "x", KeyStrategy: "k"}); err == nil {
		t.Fatal("槽 ID 未加 slot. 前缀却通过")
	}
	// 门限越界
	if err := p.RegisterSlot(ctx, "a", Slot{ID: "slot.x", Name: "x",
		SourceKind: "db", SourceRef: "x", KeyStrategy: "k", CoverageGate: 1.5}); err == nil {
		t.Fatal("门限 >1 却通过")
	}
	// 默认门限 0.80
	if err := p.RegisterSlot(ctx, "a", Slot{ID: "slot.y", Name: "y",
		SourceKind: "db", SourceRef: "y", KeyStrategy: "k"}); err != nil {
		t.Fatal(err)
	}
	s, _, _ := p.Store.GetSlot(ctx, "slot.y")
	if s.CoverageGate != 0.80 {
		t.Fatalf("默认门限应为 0.80，得 %v", s.CoverageGate)
	}
}

// ═══════════════════ 快照 ═══════════════════

func TestSnapshot(t *testing.T) {
	st := newMem()
	p := New(st, nil)
	ctx := context.Background()
	_ = p.RegisterSlot(ctx, "a", Slot{ID: "slot.cogs", Name: "成本",
		SourceKind: "db", SourceRef: "x", KeyStrategy: "k"})
	_ = p.RegisterAlgorithm(ctx, "a", Algorithm{ID: "algo.cogs", Version: 1, Formula: "a*b",
		DependsOnSlots: []string{"slot.cogs"}})

	ov, err := p.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ov.Slots) != 1 || len(ov.Algorithms) != 1 {
		t.Fatalf("快照内容不符：%+v", ov)
	}
}
