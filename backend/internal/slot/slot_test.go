// slot_test.go —— M-SLOT 注册表与覆盖率门控链路测试。
//
// 本文件同时承担一个「接线」职责：证明 G4/G5 的判定函数**在真实生产路径上**
// 被调用（而不是只在 gate_test.go 里被自己的测试调用）。
package slot_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miccchwang/spark-cicada/backend/internal/gate"
	"github.com/miccchwang/spark-cicada/backend/internal/slot"
)

// writeYAML 在临时目录写一个 yaml 文件。
func writeYAML(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// repoDirs 定位仓库里的 slots/algorithms 真目录。
func repoDirs(t *testing.T) (string, string) {
	t.Helper()
	// backend/internal/slot → 仓库根
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(root, "slots"), filepath.Join(root, "algorithms")
}

// ───────────────────── 真仓库 YAML（最重要的一条） ─────────────────────

// TestLoadRegistry_RealRepoYAML 加载**仓库里真实的** slots/*.yaml 与
// algorithms/*.yaml，并断言它们通过 G4 闸门。
//
// 这是「G4 有真实生产调用点」的证据：判定对象是磁盘上的真文件。
func TestLoadRegistry_RealRepoYAML(t *testing.T) {
	slotsDir, algosDir := repoDirs(t)
	r, err := slot.LoadRegistry(slotsDir, algosDir)
	if err != nil {
		t.Fatalf("加载真仓库 YAML 失败（G4 违规或解析错误）：%v", err)
	}
	if len(r.Slots()) == 0 {
		t.Fatal("槽注册表为空")
	}
	if len(r.Algorithms()) == 0 {
		t.Fatal("算法注册表为空")
	}
	// 每个算法依赖的槽都必须在注册表内（G4 引用完整性，走真实函数）
	if v := gate.CheckSlotsRegistered(r.AlgoDocs(), r.RegisteredSlotIDs()); len(v) != 0 {
		t.Fatalf("G4 引用完整性失败: %v", v)
	}
	// 算法定义不得混入数据源字段（G4 分离，走真实函数）
	if v := gate.CheckAlgorithmNoDataSource(r.AlgoDocs()); len(v) != 0 {
		t.Fatalf("G4 算法/数据源分离失败: %v", v)
	}
}

// TestLoadRegistry_RealRepoHasAffiliateMissing 钉死当前仓库的存量事实：
// slot.affiliate 为 MISSING ⇒ 依赖它的 algo.net_contrib 必须 skip（G5）。
//
// 若哪天 affiliate 接上了，这条测试会失败并提醒更新 docs/03 —— 这是**有意的**，
// 因为「哪个槽缺」是业务事实，不该悄悄变化。
func TestLoadRegistry_RealRepoHasAffiliateMissing(t *testing.T) {
	slotsDir, algosDir := repoDirs(t)
	r, err := slot.LoadRegistry(slotsDir, algosDir)
	if err != nil {
		t.Fatal(err)
	}
	aff, ok := r.Slot("slot.affiliate")
	if !ok {
		t.Fatal("slot.affiliate 未注册")
	}
	if aff.Status != slot.StatusMissing {
		t.Fatalf("slot.affiliate 状态应为 MISSING，实际 %q（若已接入请更新 docs/03 与本测试）", aff.Status)
	}

	// 不带观测 ⇒ 未观测槽按 MISSING 处理 ⇒ net_contrib 必 skip
	verdicts := r.JudgesForAlgorithm(slot.Observation{})
	byID := map[string]slot.AlgoVerdict{}
	for _, v := range verdicts {
		byID[v.AlgoID] = v
	}
	nc, ok := byID["algo.net_contrib"]
	if !ok {
		t.Fatal("algo.net_contrib 未注册")
	}
	if !nc.Skip {
		t.Fatalf("algo.net_contrib 依赖 MISSING 槽，应 skip，实际 %+v", nc)
	}
	found := false
	for _, s := range nc.SkipSlots {
		if s == "slot.affiliate" {
			found = true
		}
	}
	if !found {
		t.Fatalf("skip 原因应包含 slot.affiliate，实际 %v", nc.SkipSlots)
	}
}

// ───────────────────── G4：算法混入数据源字段 ⇒ 加载即失败 ─────────────────────

func TestLoadRegistry_RejectsDataSourceInAlgorithm(t *testing.T) {
	dir := t.TempDir()
	slotsDir := filepath.Join(dir, "slots")
	algosDir := filepath.Join(dir, "algorithms")

	writeYAML(t, slotsDir, "slot.rev.yaml", `
id: slot.rev
name: 收入
source_kind: derived
source_ref: channel_sales
key_strategy: store_sku
coverage_gate: 0.95
permission: L3
status: AVAILABLE
`)
	// ★ 违规：算法里混入 table/source（G4 禁止）
	writeYAML(t, algosDir, "bad.yaml", `
id: algo.bad
permission: L3
name: 坏算法
formula: "rev"
depends_on_slots: [slot.rev]
table: fact_sales_daily
source: db
`)

	_, err := slot.LoadRegistry(slotsDir, algosDir)
	if err == nil {
		t.Fatal("G4：算法混入数据源字段应导致加载失败")
	}
	if !strings.Contains(err.Error(), "数据源字段") {
		t.Fatalf("错误信息应指出数据源字段问题，实际: %v", err)
	}
}

// ───────────────────── G4：依赖未注册槽 ⇒ 加载即失败 ─────────────────────

func TestLoadRegistry_RejectsUnregisteredSlotDependency(t *testing.T) {
	dir := t.TempDir()
	slotsDir := filepath.Join(dir, "slots")
	algosDir := filepath.Join(dir, "algorithms")

	writeYAML(t, slotsDir, "slot.rev.yaml", `
id: slot.rev
name: 收入
source_kind: derived
source_ref: channel_sales
key_strategy: store_sku
coverage_gate: 0.95
permission: L3
status: AVAILABLE
`)
	writeYAML(t, algosDir, "bad.yaml", `
id: algo.bad
permission: L3
name: 坏算法
formula: "rev"
depends_on_slots: [slot.rev, slot.ghost]
`)

	_, err := slot.LoadRegistry(slotsDir, algosDir)
	if err == nil {
		t.Fatal("G4：依赖未注册槽应导致加载失败")
	}
	if !strings.Contains(err.Error(), "未注册槽") {
		t.Fatalf("错误信息应指出未注册槽，实际: %v", err)
	}
}

// ───────────────────── 空目录不得静默通过（假闸门防护） ─────────────────────

func TestLoadRegistry_RejectsEmptyDirs(t *testing.T) {
	dir := t.TempDir()
	slotsDir := filepath.Join(dir, "slots")
	algosDir := filepath.Join(dir, "algorithms")
	if err := os.MkdirAll(slotsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(algosDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := slot.LoadRegistry(slotsDir, algosDir); err == nil {
		t.Fatal("空目录应报错，不得静默返回空注册表")
	}
}

// ───────────────────── 槽缺少 coverage_gate ⇒ 拒绝（G4） ─────────────────────

func TestLoadRegistry_RejectsSlotWithoutCoverageGate(t *testing.T) {
	dir := t.TempDir()
	slotsDir := filepath.Join(dir, "slots")
	algosDir := filepath.Join(dir, "algorithms")

	writeYAML(t, slotsDir, "slot.nogate.yaml", `
id: slot.nogate
name: 无门限
source_kind: api
source_ref: master.x
key_strategy: direct
permission: L3
`)
	writeYAML(t, algosDir, "a.yaml", `
id: algo.x
permission: L3
formula: "1"
depends_on_slots: [slot.nogate]
`)

	if _, err := slot.LoadRegistry(slotsDir, algosDir); err == nil {
		t.Fatal("G4：槽缺少 coverage_gate 应导致加载失败")
	}
}

// ───────────────────── G5：覆盖率门控（链路级） ─────────────────────

func TestCoverageGate_ByStatusAndCoverage(t *testing.T) {
	dir := t.TempDir()
	slotsDir := filepath.Join(dir, "slots")
	algosDir := filepath.Join(dir, "algorithms")

	writeYAML(t, slotsDir, "slot.cost.yaml", `
id: slot.cost
name: 成本
source_kind: master_table
source_ref: cost_master.v2
key_strategy: barcode_then_sku
coverage_gate: 0.80
freshness: 7d
permission: L4
status: AVAILABLE
`)
	writeYAML(t, algosDir, "gp.yaml", `
id: algo.gp
permission: L3
formula: "rev - cogs"
depends_on_slots: [slot.cost]
`)

	r, err := slot.LoadRegistry(slotsDir, algosDir)
	if err != nil {
		t.Fatal(err)
	}

	// ★ 2026-10-07：本用例只聚焦「覆盖率」这一维，故**必须显式提供新鲜度读数**。
	// 否则过期判定（fail-closed：无读数即过期）会先一步把槽判掉，
	// 掩盖掉本用例真正要测的覆盖率行为 —— 那样断言即便「通过」也测错了对象。
	fresh := map[string]int{"slot.cost": 60} // 1 小时龄，远小于声明的 7d

	// ① 覆盖率达标（0.93 ≥ 0.80）且数据新鲜 ⇒ 不跳过
	okVerdicts := r.JudgesForAlgorithm(slot.Observation{
		Coverage:   map[string]float64{"slot.cost": 0.93},
		AgeMinutes: fresh,
	})
	if len(okVerdicts) != 1 || okVerdicts[0].Skip {
		t.Fatalf("覆盖率达标且数据新鲜不应 skip，实际 %+v", okVerdicts)
	}

	// ② 覆盖率不足（0.42 < 0.80）⇒ 跳过，且原因可读
	lowVerdicts := r.JudgesForAlgorithm(slot.Observation{
		Coverage:   map[string]float64{"slot.cost": 0.42},
		AgeMinutes: fresh,
	})
	if len(lowVerdicts) != 1 || !lowVerdicts[0].Skip {
		t.Fatalf("覆盖率不足应 skip，实际 %+v", lowVerdicts)
	}
	if lowVerdicts[0].Reason == "" {
		t.Fatal("skip 必须携带可读原因（可审计）")
	}
	// 跳过的原因必须指向**覆盖率**，而不是被其它维度抢先导致（否则本用例名不副实）
	if !strings.Contains(lowVerdicts[0].Reason, "coverage") &&
		!strings.Contains(lowVerdicts[0].Reason, "覆盖率") {
		t.Fatalf("本用例期望因覆盖率不足而跳过，实际原因: %q", lowVerdicts[0].Reason)
	}

	// ③ 显式 MISSING ⇒ 硬跳过
	missVerdicts := r.JudgesForAlgorithm(slot.Observation{
		Coverage:   map[string]float64{"slot.cost": 0.99},
		Status:     map[string]string{"slot.cost": slot.StatusMissing},
		AgeMinutes: fresh,
	})
	if !missVerdicts[0].Skip {
		t.Fatal("显式 MISSING 应硬跳过（即便覆盖率很高）")
	}
}

// TestCoverageGate_UnobservedIsMissing 未观测的槽必须按 MISSING 处理（fail-closed），
// 而不是「没观测 ⇒ 默认通过」。
func TestCoverageGate_UnobservedIsMissing(t *testing.T) {
	dir := t.TempDir()
	slotsDir := filepath.Join(dir, "slots")
	algosDir := filepath.Join(dir, "algorithms")

	writeYAML(t, slotsDir, "slot.s.yaml", `
id: slot.s
name: 槽
source_kind: api
source_ref: master.x
key_strategy: direct
coverage_gate: 0.50
freshness: 1d
permission: L3
status: AVAILABLE
`)
	writeYAML(t, algosDir, "a.yaml", `
id: algo.a
permission: L3
formula: "1"
depends_on_slots: [slot.s]
`)

	r, err := slot.LoadRegistry(slotsDir, algosDir)
	if err != nil {
		t.Fatal(err)
	}

	// 没有任何观测 ⇒ 必须 skip（不能因为「没数据」而放行）
	v := r.Judge(slot.Observation{})
	if len(v) != 1 {
		t.Fatalf("应有 1 条槽结论，实际 %d", len(v))
	}
	if !v[0].Skip {
		t.Fatalf("未观测槽应视为 MISSING 并 skip，实际 %+v", v[0])
	}
	if v[0].Status != slot.StatusMissing {
		t.Fatalf("未观测槽状态应为 MISSING，实际 %q", v[0].Status)
	}
}

// TestCoverageGate_HighCoverageButPartialStatic 静态标 PARTIAL 且覆盖率达标 ⇒
// 运行时判 ACTIVE（覆盖率是运行时事实）；覆盖率不足 ⇒ DEGRADED ⇒ skip。
func TestCoverageGate_PartialStatic(t *testing.T) {
	dir := t.TempDir()
	slotsDir := filepath.Join(dir, "slots")
	algosDir := filepath.Join(dir, "algorithms")

	writeYAML(t, slotsDir, "slot.p.yaml", `
id: slot.p
name: 部分
source_kind: api
source_ref: master.x
key_strategy: direct
coverage_gate: 0.90
freshness: 1d
permission: L4
status: PARTIAL
`)
	writeYAML(t, algosDir, "a.yaml", `
id: algo.a
permission: L3
formula: "1"
depends_on_slots: [slot.p]
`)

	r, err := slot.LoadRegistry(slotsDir, algosDir)
	if err != nil {
		t.Fatal(err)
	}
	// ★ 同上：本用例聚焦覆盖率，须显式给出新鲜度读数（否则会被过期判定抢先判掉）。
	fresh := map[string]int{"slot.p": 30}

	// 覆盖率回到门限之上 + 数据新鲜 ⇒ 可用（静态 PARTIAL 不阻断运行时事实）
	good := r.Judge(slot.Observation{
		Coverage:   map[string]float64{"slot.p": 0.95},
		AgeMinutes: fresh,
	})
	if good[0].Skip {
		t.Fatalf("覆盖率达标且数据新鲜不应 skip，实际 %+v", good[0])
	}
	// 覆盖率低于门限 ⇒ skip
	bad := r.Judge(slot.Observation{
		Coverage:   map[string]float64{"slot.p": 0.88},
		AgeMinutes: fresh,
	})
	if !bad[0].Skip {
		t.Fatalf("覆盖率不足应 skip，实际 %+v", bad[0])
	}
}

// ───────────────────── 重复 ID / 缺字段：加载即失败 ─────────────────────

func TestLoadRegistry_RejectsDuplicateSlotID(t *testing.T) {
	dir := t.TempDir()
	slotsDir := filepath.Join(dir, "slots")
	algosDir := filepath.Join(dir, "algorithms")

	body := `
id: slot.dup
name: 重复
source_kind: api
source_ref: master.x
key_strategy: direct
coverage_gate: 0.5
permission: L3
status: AVAILABLE
`
	writeYAML(t, slotsDir, "a.yaml", body)
	writeYAML(t, slotsDir, "b.yaml", body)
	writeYAML(t, algosDir, "a.yaml", `
id: algo.a
permission: L3
formula: "1"
depends_on_slots: [slot.dup]
`)

	if _, err := slot.LoadRegistry(slotsDir, algosDir); err == nil {
		t.Fatal("重复槽 ID 应导致加载失败")
	}
}

func TestLoadRegistry_RejectsAlgorithmWithoutSlots(t *testing.T) {
	dir := t.TempDir()
	slotsDir := filepath.Join(dir, "slots")
	algosDir := filepath.Join(dir, "algorithms")

	writeYAML(t, slotsDir, "s.yaml", `
id: slot.s
name: 槽
source_kind: api
source_ref: master.x
key_strategy: direct
coverage_gate: 0.5
permission: L3
status: AVAILABLE
`)
	// 未声明 depends_on_slots ⇒ 拒绝（算法必须有数据来源）
	writeYAML(t, algosDir, "a.yaml", `
id: algo.a
permission: L3
formula: "1"
`)

	if _, err := slot.LoadRegistry(slotsDir, algosDir); err == nil {
		t.Fatal("算法未声明 depends_on_slots 应导致加载失败")
	}
}

func TestLoadRegistry_RejectsIllegalStatus(t *testing.T) {
	dir := t.TempDir()
	slotsDir := filepath.Join(dir, "slots")
	algosDir := filepath.Join(dir, "algorithms")

	writeYAML(t, slotsDir, "s.yaml", `
id: slot.s
name: 槽
source_kind: api
source_ref: master.x
key_strategy: direct
coverage_gate: 0.5
permission: L3
status: ACTIVE
`)
	writeYAML(t, algosDir, "a.yaml", `
id: algo.a
permission: L3
formula: "1"
depends_on_slots: [slot.s]
`)

	if _, err := slot.LoadRegistry(slotsDir, algosDir); err == nil {
		t.Fatal("槽定义里出现运行时状态 ACTIVE 应被拒（静态状态只允许 AVAILABLE/PARTIAL/MISSING）")
	}
}
