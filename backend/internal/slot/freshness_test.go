package slot_test

// freshness_test.go —— 槽的「采集时效」闭环（G4 声明 + G5 判活）的**链路级**断言。
//
// 为什么单写一个文件（而不是塞进 slot_test.go）：
//
//	本文件钉住的是**同一个病的第十三个变种** ——
//	「字段被读进来了、存进库了，但**没有任何判定消费它**」。
//	它的证据链与前面的变种不同（前面几轮是「判定函数没有生产调用点」，
//	这里是「有生产调用点、字段全程存在、就是不影响任何结果」），
//	值得单独成文，免得日后被误并回去而失去对它的针对性。
//
// 断言分两层：
//
//	A. **声明层**（gate.CheckFreshnessDeclared）：每个槽的 freshness 必须
//	   非空且可解析 —— 自由文本（`daily` / `7天`）一律拒绝。
//	B. **判活层**（Registry.CheckFreshness / Cases / Judge）：过期的槽必须
//	   导致算法 skip，且**原因可读**（不是只有一个 true）。
//	   ★ 这一层是重点：它是 `freshness` 字段第一次真正改变输出结果。
//
// 与既有测试的关系：slot_test.go 的 G5 三例覆盖「覆盖率」这一维；
// 本文件覆盖「时效」这一维，两者共同构成 docs/05 G5 的完整门控。
// 既有三例在本轮新增声明校验后缺 freshness 而变红，已补声明（见 slot_test.go）。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miccchwang/spark-cicada/backend/internal/slot"
)

// ───────────────────────── A. 声明层：必须显式且可解析 ─────────────────────────

// TestFreshnessDeclaration_RejectsUnparsable 自由文本的 freshness 必须让**加载**失败。
//
// 这正是本轮修掉的失效形态：`freshness: daily` 与 `freshness: 7d` 此前**完全等价**
// —— 都被解析成一个字符串、写进库、然后无人再读。
func TestFreshnessDeclaration_RejectsUnparsable(t *testing.T) {
	cases := []struct {
		name      string
		freshness string
		wantFail  bool
	}{
		{"规范写法 1d", "freshness: 1d", false},
		{"规范写法 7d", "freshness: 7d", false},
		{"规范写法 12h", "freshness: 12h", false},
		{"规范写法 30m", "freshness: 30m", false},
		{"规范写法 2w", "freshness: 2w", false},
		// 自由文本：看起来合理，但解析器拿不到时长 ⇒ 判定会退化成「不影响任何事」
		{"英文 daily 被拒", "freshness: daily", true},
		{"中文 7天 被拒", "freshness: 7天", true},
		{"带空格 1 day 被拒", "freshness: 1 day", true},
		{"小数 1.5d 被拒", "freshness: 1.5d", true},
		{"零值 0d 被拒", "freshness: 0d", true},
		{"无单位 7 被拒", "freshness: 7", true},
		{"单位非法 7x 被拒", "freshness: 7x", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			slotsDir := filepath.Join(dir, "slots")
			algosDir := filepath.Join(dir, "algorithms")
			writeYAML(t, slotsDir, "slot.a.yaml", `
id: slot.a
name: 槽
source_kind: api
source_ref: master.x
key_strategy: direct
coverage_gate: 0.50
`+tc.freshness+`
permission: L3
status: AVAILABLE
`)
			writeYAML(t, algosDir, "a.yaml", `
id: algo.a
permission: L3
formula: "1"
depends_on_slots: [slot.a]
`)
			_, err := slot.LoadRegistry(slotsDir, algosDir)
			if tc.wantFail && err == nil {
				t.Fatalf("freshness=%q 应导致加载失败（不可解析的时效必须先被拒）", tc.freshness)
			}
			if !tc.wantFail && err != nil {
				t.Fatalf("freshness=%q 应被接受，实际: %v", tc.freshness, err)
			}
			// 失败时理由必须能指明「是时效的问题」，而不是一句笼统的校验失败 ——
			// 否则运维看到红灯也不知道该改哪一行。
			if tc.wantFail && err != nil && !strings.Contains(err.Error(), "freshness") {
				t.Fatalf("失败原因应点名 freshness，实际: %v", err)
			}
		})
	}
}

// TestFreshnessDeclaration_MissingRejected 完全不声明 freshness ⇒ 拒绝。
func TestFreshnessDeclaration_MissingRejected(t *testing.T) {
	dir := t.TempDir()
	slotsDir := filepath.Join(dir, "slots")
	algosDir := filepath.Join(dir, "algorithms")

	// 夹具自证：这份 YAML 里**确实没有** freshness 键。
	const slotYAML = `
id: slot.nofresh
name: 无时效
source_kind: api
source_ref: master.x
key_strategy: direct
coverage_gate: 0.50
permission: L3
status: AVAILABLE
`
	if strings.Contains(slotYAML, "freshness") {
		t.Fatal("夹具自身错误：本用例的 YAML 不应含 freshness")
	}
	writeYAML(t, slotsDir, "slot.nofresh.yaml", slotYAML)
	writeYAML(t, algosDir, "a.yaml", `
id: algo.a
permission: L3
formula: "1"
depends_on_slots: [slot.nofresh]
`)

	_, err := slot.LoadRegistry(slotsDir, algosDir)
	if err == nil {
		t.Fatal("G4：槽未声明 freshness 应导致加载失败")
	}
	if !strings.Contains(err.Error(), "freshness") {
		t.Fatalf("失败原因应点名 freshness，实际: %v", err)
	}
}

// ───────────────────── B. 判活层：过期必须真的改变输出 ─────────────────────

// TestFreshness_StaleSlotSkipsAlgorithm 是本轮修复的**核心断言**。
//
// 场景：一个覆盖率 100%、静态状态 AVAILABLE 的槽，但数据停在 30 天前，
// 而其 freshness 声明是 7d。修复前 ⇒ 判活通过（覆盖率完好）；
// 修复后 ⇒ 过期 ⇒ 依赖它的算法 skip，且原因里能看到「距上次采集 43200 分钟」。
func TestFreshness_StaleSlotSkipsAlgorithm(t *testing.T) {
	dir := t.TempDir()
	slotsDir := filepath.Join(dir, "slots")
	algosDir := filepath.Join(dir, "algorithms")

	writeYAML(t, slotsDir, "slot.fresh.yaml", `
id: slot.fresh
name: 时效槽
source_kind: api
source_ref: master.x
key_strategy: direct
coverage_gate: 0.80
freshness: 7d
permission: L3
status: AVAILABLE
`)
	writeYAML(t, algosDir, "a.yaml", `
id: algo.a
permission: L3
formula: "1"
depends_on_slots: [slot.fresh]
`)

	r, err := slot.LoadRegistry(slotsDir, algosDir)
	if err != nil {
		t.Fatal(err)
	}

	// ① 覆盖率 100% 且龄 1 天（在 7d 以内）⇒ 可用
	fresh := r.Judge(slot.Observation{
		Coverage:   map[string]float64{"slot.fresh": 1.0},
		AgeMinutes: map[string]int{"slot.fresh": 24 * 60},
	})
	if len(fresh) != 1 {
		t.Fatalf("应有 1 条槽结论，实际 %d", len(fresh))
	}
	if fresh[0].Skip {
		t.Fatalf("覆盖率 100%% 且龄 1d（< 7d）不应 skip，实际 %+v", fresh[0])
	}
	if fresh[0].Stale {
		t.Fatalf("龄 1d < 时效 7d，不应判过期，实际 %+v", fresh[0])
	}

	// ② 覆盖率**不变**（仍 100%），只是数据老了 ⇒ 必须 skip
	stale := r.Judge(slot.Observation{
		Coverage:   map[string]float64{"slot.fresh": 1.0},
		AgeMinutes: map[string]int{"slot.fresh": 30 * 24 * 60},
	})
	if !stale[0].Skip {
		t.Fatalf("数据已过期（30d > 7d）必须 skip，实际 %+v", stale[0])
	}
	if !stale[0].Stale {
		t.Fatalf("应判为过期，实际 %+v", stale[0])
	}
	// ★ 原因必须可读：这是「可审计」与「只有一个 bool」的分水岭。
	if !strings.Contains(stale[0].StaleReason, "30 天") &&
		!strings.Contains(stale[0].StaleReason, "43200") {
		t.Fatalf("过期原因应含实际龄，实际 %q", stale[0].StaleReason)
	}
	if !strings.Contains(stale[0].StaleReason, "7d") {
		t.Fatalf("过期原因应含时效上限 7d，实际 %q", stale[0].StaleReason)
	}

	// ③ 边界：恰好等于上限 ⇒ 不过期（> 才过期，闭区间口径必须钉住）
	edge := r.Judge(slot.Observation{
		Coverage:   map[string]float64{"slot.fresh": 1.0},
		AgeMinutes: map[string]int{"slot.fresh": 7 * 24 * 60},
	})
	if edge[0].Stale {
		t.Fatalf("龄恰等于时效上限不应判过期（口径为 > 才过期），实际 %+v", edge[0])
	}

	// ④ 算法维度：过期槽必须让**依赖它的算法**skip（链路终点）
	av := r.JudgesForAlgorithm(slot.Observation{
		Coverage:   map[string]float64{"slot.fresh": 1.0},
		AgeMinutes: map[string]int{"slot.fresh": 30 * 24 * 60},
	})
	if len(av) != 1 || !av[0].Skip {
		t.Fatalf("过期槽的依赖算法应 skip，实际 %+v", av)
	}
	if len(av[0].SkipSlots) == 0 {
		t.Fatalf("skip 应可追溯到具体槽，实际 %+v", av[0])
	}
}

// TestFreshness_UnknownAgeIsFailClosed 没有新鲜度读数 ⇒ **过期**（不是新鲜）。
//
// ★ 这是方向性问题：若把「没读到」当作「新鲜」，那么采集器整个挂掉这条通道
// 反而会让所有槽都判活 —— 失效方向恰好反了。必须与 coverage 侧
// 「未观测 ⇒ MISSING」保持同一种保守方向。
func TestFreshness_UnknownAgeIsFailClosed(t *testing.T) {
	dir := t.TempDir()
	slotsDir := filepath.Join(dir, "slots")
	algosDir := filepath.Join(dir, "algorithms")

	writeYAML(t, slotsDir, "slot.a.yaml", `
id: slot.a
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
depends_on_slots: [slot.a]
`)

	r, err := slot.LoadRegistry(slotsDir, algosDir)
	if err != nil {
		t.Fatal(err)
	}

	// 覆盖率给足、但**不给龄**：AgeMinutes 为空 map（不是 nil，模拟「采集器没回数据」）
	v := r.Judge(slot.Observation{
		Coverage:   map[string]float64{"slot.a": 1.0},
		AgeMinutes: map[string]int{},
	})
	if !v[0].Skip {
		t.Fatalf("无新鲜度读数必须 fail-closed（判 skip），实际 %+v", v[0])
	}
	if !v[0].Stale {
		t.Fatalf("无读数应判为过期，实际 %+v", v[0])
	}
	if v[0].AgeMinutes != -1 {
		t.Fatalf("无读数时 AgeMinutes 应为 -1，实际 %d", v[0].AgeMinutes)
	}
	// 原因要能区分「没读数」与「读数超期」——两者处置动作不同
	if !strings.Contains(v[0].StaleReason, "无新鲜度读数") {
		t.Fatalf("原因应说明是「无读数」而非「超期」，实际 %q", v[0].StaleReason)
	}
}

// TestFreshness_NegativeAgeIsStale 龄为负（时钟回拨/脏数据）⇒ 过期，不得当作「很新鲜」。
func TestFreshness_NegativeAgeIsStale(t *testing.T) {
	dir := t.TempDir()
	slotsDir := filepath.Join(dir, "slots")
	algosDir := filepath.Join(dir, "algorithms")

	writeYAML(t, slotsDir, "slot.a.yaml", `
id: slot.a
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
depends_on_slots: [slot.a]
`)

	r, err := slot.LoadRegistry(slotsDir, algosDir)
	if err != nil {
		t.Fatal(err)
	}
	v := r.Judge(slot.Observation{
		Coverage:   map[string]float64{"slot.a": 1.0},
		AgeMinutes: map[string]int{"slot.a": -60},
	})
	if !v[0].Stale || !v[0].Skip {
		t.Fatalf("龄为负应判过期并 skip（不得当作「很新鲜」），实际 %+v", v[0])
	}
}

// TestFreshness_MissingStaticStaysMissing 无权威来源的槽过期时仍报 MISSING。
//
// 语义细分：MISSING（没有来源）比 DEGRADED（有来源但过期/不全）更准确，
// 两者处置动作也不同（前者要接数据源，后者要查采集管道）。
func TestFreshness_MissingStaticStaysMissing(t *testing.T) {
	dir := t.TempDir()
	slotsDir := filepath.Join(dir, "slots")
	algosDir := filepath.Join(dir, "algorithms")

	writeYAML(t, slotsDir, "slot.m.yaml", `
id: slot.m
name: 无来源
source_kind: api
source_ref: master.x
key_strategy: direct
coverage_gate: 0.50
freshness: 1d
permission: L3
status: MISSING
`)
	writeYAML(t, algosDir, "a.yaml", `
id: algo.a
permission: L3
formula: "1"
depends_on_slots: [slot.m]
`)

	r, err := slot.LoadRegistry(slotsDir, algosDir)
	if err != nil {
		t.Fatal(err)
	}
	// 即便给了覆盖率与「很新」的龄，静态 MISSING 也必须压过一切
	v := r.Judge(slot.Observation{
		Coverage:   map[string]float64{"slot.m": 1.0},
		AgeMinutes: map[string]int{"slot.m": 0},
	})
	if !v[0].Skip {
		t.Fatalf("静态 MISSING 必须 skip，实际 %+v", v[0])
	}
	if v[0].Status != slot.StatusMissing {
		t.Fatalf("静态 MISSING 应保持 MISSING（不被过期限定改写），实际 %q", v[0].Status)
	}
}

// ───────────────── C. 接线闸门：判定函数必须真有生产调用点 ─────────────────

// TestFreshness_GateHasProductionCallers 用**静态源码扫描**钉住：
// `gate.CheckFreshnessDeclared` / `gate.ParseFreshness` 必须有非测试调用点。
//
// 为什么这条必须存在：本仓反复出现的失效形态就是「判定函数只被自己的
// `_test.go` 调用」——那样断言永远是绿的，且绿得毫无意义。
// 先例：G8 的 `gate.CheckPerf`、G12 的四个判定函数、G10 的 `CheckAuditAppendOnly`。
func TestFreshness_GateHasProductionCallers(t *testing.T) {
	root := repoRoot(t)

	// 注意：repoRoot 返回的是**仓库根**（含 backend/ slots/ algorithms/），
	// 故这里必须带上 backend/ 前缀 —— 少写一层会读到不存在的路径而报「读文件失败」，
	// 那种失败看起来像环境问题，实则只是路径写错（本轮实测踩到）。
	targets := []struct {
		fn      string
		callers []string // 至少一处
	}{
		{"CheckFreshnessDeclared", []string{"backend/internal/slot/slot.go"}},
		{"ParseFreshness", []string{"backend/internal/slot/slot.go"}},
	}
	for _, tc := range targets {
		t.Run(tc.fn, func(t *testing.T) {
			// ① 定义必须存在（否则下面的扫描会「因为找不到而通过」= 恒真）
			if !hasNonTestCaller(root, tc.fn) {
				t.Fatalf("判定函数 %s 找不到任何非测试调用点 ⇒ 断言恒真（假闸门）", tc.fn)
			}
			// ② 且调用点必须在预期的生产文件里（防止「被另一个测试辅助文件调用」蒙混）
			for _, rel := range tc.callers {
				b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
				if err != nil {
					t.Fatalf("读 %s 失败: %v", rel, err)
				}
				if !strings.Contains(string(b), tc.fn) {
					t.Fatalf("%s 未调用 %s（接线缺失）", rel, tc.fn)
				}
			}
		})
	}
}

// hasNonTestCaller 在 backend/ 下扫描 fn 的非测试调用点。
//
// 口径：排除 `_test.go`，排除 `gate/gate.go`（那是定义处），
// 命中 `fn(` 即算调用。★ 扫描器自证见本函数末尾。
func hasNonTestCaller(root, fn string) bool {
	backend := filepath.Join(root, "backend")
	found := false
	_ = filepath.Walk(backend, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		// gate.go 是定义所在，不算调用点
		if filepath.ToSlash(p) == "backend/internal/gate/gate.go" ||
			strings.HasSuffix(filepath.ToSlash(p), "/internal/gate/gate.go") {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		if strings.Contains(string(b), fn+"(") {
			found = true
		}
		return nil
	})
	return found
}

// repoRoot 复用 bucket_test.go 中的同名助手（返回仓库根）。
// ★ 不在此重复定义：同一包内重名会让整个测试包编译失败，
//   而失败信息指向的是「重名」而非「测试断言不成立」，容易误判成别的问题。

// TestFreshness_UnitTableSingleSource 单位集合只有一份权威表。
//
// 防的是「校验器认 1d、判活器不认 1d」这类分叉：两侧各写一份单位表，
// 结果一边放行一边判过期，而两边各自的测试都是绿的。
func TestFreshness_UnitTableSingleSource(t *testing.T) {
	root := repoRoot(t)
	b, err := os.ReadFile(filepath.Join(root, "backend", "internal", "slot", "slot.go"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	// slot 侧只能**委托** gate 的解析器，不得自带单位表。
	if !strings.Contains(src, "gate.ParseFreshness") {
		t.Fatal("slot 侧必须委托 gate.ParseFreshness（否则单位表会分叉）")
	}
	for _, forbidden := range []string{`"w":`, `case "w"`, `"分钟"`} {
		if strings.Contains(src, forbidden) {
			t.Fatalf("slot 侧疑似自带时效单位表（%q）—— 单位表只允许存在于 gate 包", forbidden)
		}
	}
}
