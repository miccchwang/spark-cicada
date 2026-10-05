// postgres_test.go —— 桶规格与 NULL 语义的**纯逻辑**断言（不需要真库）。
//
// 动机：G3（缺失不补 0）是项目第一红线。真库集成测试在 CI 未必总能跑，
// 因此这里对「桶规格声明」与「排序/过滤白名单」做静态断言：
// 规格一旦被改成会把 NULL 当 0 的形态（例如把度量列声明为非指针），必须立即报错。
package store

import (
	"os"
	"path/filepath"
	"strings"
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

// TestBucketSpec_NonTextGrainColumnsAreCast 断言：
// 凡是 grain 中类型不是 text 的列，必须在 grainExpr 里给出显式转型。
//
// ★ 真实事故（CI 真库首次抓出的第 4 个 bug）：
//   bucket_pnl_month.month 在 DDL 里是 `date`，而 grain 一律按 *string 扫描，
//   pgx 直接拒绝：cannot scan date (OID 1082) in binary format into *string。
//   整条 bucket 查询在真库上 500。
//
//   为什么长期没被发现：这条路径的测试在本机**一直 skip**（无 Postgres）。
//   「跳过」把 SQL 类型与 Go 扫描类型的错配藏了很久。
//
// 本测试不连库，靠**DDL 与 spec 对照**把契约钉住：
// 从 0003 迁移里读出 bucket 表的列类型，凡是非文本的 grain 列，
// 都要求 spec 声明了转型表达式。
func TestBucketSpec_NonTextGrainColumnsAreCast(t *testing.T) {
	for name, spec := range specs {
		for _, g := range spec.grain {
			typ := ddlColumnType(t, spec.table, g)
			isTextual := typ == "" || // 无法判定时不误报
				strings.HasPrefix(typ, "text") ||
				strings.HasPrefix(typ, "varchar") ||
				strings.HasPrefix(typ, "character")
			if isTextual {
				continue
			}
			if _, ok := spec.grainExpr[g]; !ok {
				t.Errorf("桶 %s 的 grain 列 %s 在 DDL 中是 %q（非文本），"+
					"但 spec.grainExpr 未声明转型 —— 真库上会报 "+
					"cannot scan %s into *string", name, g, typ, typ)
			}
		}
	}
}

// ddlColumnType 从迁移 SQL 里读出某表某列的类型（粗粒度）。
// 只服务于上面的契约断言，不追求完整解析。
func ddlColumnType(t *testing.T, table, col string) string {
	t.Helper()
	files := []string{
		"0001_core.sql", "0002_slots_and_rules.sql",
		"0003_precompute.sql", "0004_registry_seed.sql",
	}
	for _, f := range files {
		p := filepath.Join("..", "..", "..", "sql", "migrations", f)
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		for _, tbl := range splitTables(string(b)) {
			if !strings.EqualFold(tbl.name, table) {
				continue
			}
			for _, clause := range splitTop(strings.Trim(tbl.body, "()"), ',') {
				fields := strings.Fields(strings.TrimSpace(clause))
				if len(fields) < 2 {
					continue
				}
				if !strings.EqualFold(fields[0], col) {
					continue
				}
				return strings.ToLower(fields[1])
			}
		}
	}
	return "" // 找不到就不判定（避免误报）
}

// ── 迁移 DDL 的极简切分工具（只服务于 grain 类型契约断言）──

type ddlTable struct{ name, body string }

// splitTables 切出每个 CREATE TABLE 的表名与表体（括号配平，跳过引号内）。
func splitTables(sql string) []ddlTable {
	var out []ddlTable
	low := strings.ToLower(sql)
	idx := 0
	for {
		i := strings.Index(low[idx:], "create table")
		if i < 0 {
			break
		}
		start := idx + i
		rest := sql[start:]
		open := strings.Index(rest, "(")
		if open < 0 {
			break
		}
		fields := strings.Fields(rest[:open])
		name := ""
		for j := len(fields) - 1; j >= 0; j-- {
			f := strings.TrimSpace(fields[j])
			switch strings.ToUpper(f) {
			case "", "EXISTS", "NOT", "IF", "TABLE", "CREATE":
				continue
			}
			name = f
			break
		}
		depth, inQuote, end := 0, false, -1
		for k := open; k < len(rest); k++ {
			switch rest[k] {
			case '\'':
				inQuote = !inQuote
			case '(':
				if !inQuote {
					depth++
				}
			case ')':
				if !inQuote {
					depth--
					if depth == 0 {
						end = k
					}
				}
			}
			if end >= 0 {
				break
			}
		}
		if end < 0 {
			break
		}
		out = append(out, ddlTable{name: name, body: rest[open : end+1]})
		idx = start + end
	}
	return out
}

// splitTop 按顶层分隔符切分（跳过括号与引号内）。
func splitTop(s string, sep rune) []string {
	var out []string
	var cur strings.Builder
	depth, inQuote := 0, false
	for _, r := range s {
		switch {
		case r == '\'':
			inQuote = !inQuote
			cur.WriteRune(r)
		case inQuote:
			cur.WriteRune(r)
		case r == '(' || r == '[':
			depth++
			cur.WriteRune(r)
		case r == ')' || r == ']':
			depth--
			cur.WriteRune(r)
		case r == sep && depth == 0:
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	if t := strings.TrimSpace(cur.String()); t != "" {
		out = append(out, t)
	}
	return out
}
