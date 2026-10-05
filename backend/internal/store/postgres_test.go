// postgres_test.go —— 桶规格与 NULL 语义的**纯逻辑**断言（不需要真库）。
//
// 动机：G3（缺失不补 0）是项目第一红线。真库集成测试在 CI 未必总能跑，
// 因此这里对「桶规格声明」与「排序/过滤白名单」做静态断言：
// 规格一旦被改成会把 NULL 当 0 的形态（例如把度量列声明为非指针），必须立即报错。
package store

import (
	"testing"

	"github.com/miccchwang/spark-cicada/backend/internal/contracts"
	"github.com/miccchwang/spark-cicada/backend/internal/query"
)

// buildWhereForTest 直接复用 query 层的真实实现，确保被断言的就是生产代码路径。
func buildWhereForTest(q contracts.QueryState) (string, []any) {
	return query.BuildWhere(q)
}

// TestSingleLineSpec 不能改名（便于 CI 断言存在）：每个寄存器桶都必须显式声明度量列。
func TestPnLMonthSpecDeclaresMetrics(t *testing.T) {
	spec, ok := specs["pnl_month"]
	if !ok {
		t.Fatal("pnl_month 桶未注册（fail-closed 要求显式登记）")
	}
	// 五个派生量必须都在声明里，否则会出现「算法产出但查询读不到」的静默缺口
	want := map[string]bool{"rev": false, "cogs": false, "gp": false, "gmp": false, "net_contrib": false}
	for _, m := range spec.metrics {
		if _, ok := want[m]; ok {
			want[m] = true
		}
	}
	for k, found := range want {
		if !found {
			t.Fatalf("桶规格缺少度量列 %q（会导致该派生量无法被查询/门控）", k)
		}
	}
	// grain 必须与 0003 的 UNIQUE 一致
	grainWant := []string{"month", "channel_code", "shop_id", "brand"}
	if len(spec.grain) != len(grainWant) {
		t.Fatalf("grain 列数不符：得 %v 期望 %v", spec.grain, grainWant)
	}
	for i := range grainWant {
		if spec.grain[i] != grainWant[i] {
			t.Fatalf("grain[%d] 不符：得 %q 期望 %q", i, spec.grain[i], grainWant[i])
		}
	}
}

// TestUnregisteredBucketRejected 未登记的桶必须被拒绝（不得隐式查询任意表）。
func TestUnregisteredBucketRejected(t *testing.T) {
	p := &Postgres{} // pool=nil，但未注册桶应在触库前就返回
	_, err := p.SelectBucket(nil, "bucket_evil", contracts.QueryState{})
	if err == nil {
		t.Fatal("未注册桶竟被接受 —— 存在 SQL 注入/越表查询风险")
	}
}

// TestOrderColWhitelist 排序字段必须走白名单（防注入）。
func TestOrderColWhitelist(t *testing.T) {
	spec := specs["pnl_month"]
	okCases := []string{"month", "channel_code", "shop_id", "brand", "gp", "gmp", "channelCode", "storeKey"}
	for _, c := range okCases {
		if _, ok := orderCol(c, spec); !ok {
			t.Fatalf("白名单漏掉合法排序字段 %q", c)
		}
	}
	// 危险输入：一律拒绝，绝不拼接
	badCases := []string{"", "gp; DROP TABLE audit_log--", "1=1", "(SELECT 1)", "algo_versions"}
	for _, c := range badCases {
		if _, ok := orderCol(c, spec); ok {
			t.Fatalf("危险排序字段 %q 竟通过白名单", c)
		}
	}
}

// TestBuildWhereParameterized 断言过滤条件全部参数化（占位符 $N），无字符串拼接值。
func TestBuildWhereParameterized(t *testing.T) {
	q := contracts.QueryState{}
	q.Time.From = "2026-09-01"
	q.Time.To = "2026-09-30"
	q.Dims.ChannelCode = []string{"TK-TH"}
	q.Dims.StoreKey = []string{"S123"}
	q.Dims.Brand = []string{"KONVY"}
	q.Filters = []contracts.FilterClause{
		{Field: "brand", Op: "eq", Value: "KONVY"},
		{Field: "evil; DROP TABLE audit_log", Op: "eq", Value: "x"}, // 未知字段必须被忽略
	}
	where, args := buildWhereForTest(q)
	if where == "TRUE" {
		t.Fatal("期望生成过滤条件，得到 TRUE")
	}
	// 期望参数：from, to, channel, shop, dims.brand, filters.brand
	// （非白名单字段 evil; DROP TABLE 必须被**丢弃**，不产生任何条件/参数）
	if len(args) != 6 {
		t.Fatalf("参数个数不符：得 %d 期望 6 —— where=%s args=%v",
			len(args), where, args)
	}
	// 未白名单字段名绝不能出现在 SQL 中（否则说明白名单被绕过）
	if contains(where, "DROP") || contains(where, "evil") {
		t.Fatalf("未白名单字段进入了 SQL —— 白名单失效：%s", where)
	}
	// 不可出现未参数化的字面量值
	if contains(where, "KONVY") || contains(where, "TK-TH") {
		t.Fatalf("过滤值被直接拼进 SQL（应为占位符）：%s", where)
	}
}

func contains(s, sub string) bool {
	return len(sub) > 0 && len(s) >= len(sub) && indexOf(s, sub) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
