package gate_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/miccchwang/spark-cicada/backend/internal/gate"
)

// 本文件测的是 G4 第十一侧的两条判定函数：
//   * gate.CheckBucketIndexesDeclared —— 索引**声明**必须可解析（声明层）；
//   * gate.CheckBucketIndexesMatchDDL —— 索引声明必须与迁移 DDL **双向**同源。
//
// 与 CheckUnitVocabularyMatchesDB / CheckAlgorithmVersionMatchesDB 同纪律：
// 静态比对，不连数据库。

// ddlSample 是一段形似 0003_precompute.sql 的迁移文本（含表定义 + 四条索引）。
const ddlSample = `
CREATE TABLE IF NOT EXISTS bucket_pnl_month (
    id           bigserial PRIMARY KEY,
    month        date NOT NULL,
    channel_code text NOT NULL,
    shop_id      text NOT NULL,
    brand        text NULL,
    CONSTRAINT uq_x UNIQUE (month, channel_code, shop_id, brand)
);

CREATE INDEX IF NOT EXISTS idx_bucket_pnl_month_month   ON bucket_pnl_month(month);
CREATE INDEX IF NOT EXISTS idx_bucket_pnl_month_channel ON bucket_pnl_month(channel_code, month);
CREATE INDEX IF NOT EXISTS idx_bucket_pnl_month_shop    ON bucket_pnl_month(shop_id, month);
CREATE INDEX IF NOT EXISTS idx_bucket_pnl_month_brand   ON bucket_pnl_month(brand, month);
`

func TestParseCreateIndexes_ExtractsTableAndOrderedColumns(t *testing.T) {
	got := gate.ParseCreateIndexes(ddlSample)
	idx := got["bucket_pnl_month"]
	if len(idx) != 4 {
		t.Fatalf("应解析出 4 条索引，得 %d：%v（夹具没读到真 DDL？）", len(idx), idx)
	}
	// 顺序敏感：DDL 写的是 (channel_code, month)，不是 (month, channel_code)。
	if !reflect.DeepEqual(idx[1], []string{"channel_code", "month"}) {
		t.Errorf("第二条索引列序错：want [channel_code month]，got %v", idx[1])
	}
	// UNIQUE 约束（uq_x）不是 CREATE INDEX，不应被解析进来。
	for _, one := range idx {
		if strings.Join(one, ",") == "month,channel_code,shop_id,brand" {
			t.Error("UNIQUE 约束被误当成 CREATE INDEX —— 解析器把约束与索引混为一谈")
		}
	}
}

func TestParseCreateIndexes_HandlesUniqueSchemaAndOrderSuffix(t *testing.T) {
	ddl := `
CREATE UNIQUE INDEX IF NOT EXISTS idx_a ON public.t1(a, b DESC);
CREATE INDEX idx_b ON t2( started_at desc );
`
	got := gate.ParseCreateIndexes(ddl)
	if !reflect.DeepEqual(got["t1"], [][]string{{"a", "b"}}) {
		t.Errorf("schema 前缀 / UNIQUE / DESC 后缀未正确处理：%v", got["t1"])
	}
	if !reflect.DeepEqual(got["t2"], [][]string{{"started_at"}}) {
		t.Errorf("小写 desc 后缀未正确处理：%v", got["t2"])
	}
}

// ★ 夹具自证：`last_desc` 这样的列名**不能**被误剥成 `last_`
// —— 那会凭空造出一个幽灵列，把「列名写错」变成一条假红。
func TestParseCreateIndexes_DoesNotMisstripColumnNamedLikeSuffix(t *testing.T) {
	got := gate.ParseCreateIndexes(`CREATE INDEX i ON t(last_desc);`)
	if !reflect.DeepEqual(got["t"], [][]string{{"last_desc"}}) {
		t.Errorf("列名 last_desc 被误剥排序后缀：%v", got["t"])
	}
}

// ★ 表达式索引 / 部分索引的「列清单」不是纯列名 ⇒ 整条跳过（不猜、不误报）。
func TestParseCreateIndexes_SkipsExpressionIndex(t *testing.T) {
	got := gate.ParseCreateIndexes(`
CREATE INDEX i1 ON t(lower(name));
CREATE INDEX i2 ON t(a) WHERE a > 0;
CREATE INDEX i3 ON t(b);
`)
	if _, ok := got["t"]; !ok {
		t.Fatal("普通列索引 i3 应当被解析出来")
	}
	// i1 是表达式（lower(name) 含括号 → 正则的 [^)]* 只截到 "lower(name"，含括号 ⇒ 非法列名 ⇒ 跳过）
	for _, one := range got["t"] {
		for _, c := range one {
			if strings.ContainsAny(c, "()") {
				t.Errorf("表达式索引被当成普通列索引：%v", one)
			}
		}
	}
	if len(got["t"]) != 1 {
		t.Errorf("应只剩 1 条普通列索引，得 %d：%v", len(got["t"]), got["t"])
	}
}

func TestParseBucketIndexes_RejectsMalformed(t *testing.T) {
	cases := []struct {
		name string
		raw  [][]string
		want string // 违规描述里应含的子串
	}{
		{"空索引", [][]string{{}}, "为空"},
		{"中文列名", [][]string{{"月份"}}, "非法列名"},
		{"含空格", [][]string{{"month day"}}, "非法列名"},
		{"以数字开头", [][]string{{"1month"}}, "非法列名"},
		{"索引内重复列", [][]string{{"month", "month"}}, "重复列"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, bad := gate.ParseBucketIndexes("b1", c.raw)
			if len(bad) == 0 {
				t.Fatalf("非法声明 %v 未被拒绝", c.raw)
			}
			if !strings.Contains(strings.Join(bad, " "), c.want) {
				t.Errorf("违规描述未含 %q：%v", c.want, bad)
			}
		})
	}
}

// 大小写归一：YAML 写 Month 与 DDL 写 month 是同一列，不该误报。
func TestParseBucketIndexes_NormalizesCase(t *testing.T) {
	norm, bad := gate.ParseBucketIndexes("b1", [][]string{{"Month", "Channel_Code"}})
	if len(bad) != 0 {
		t.Fatalf("大小写不同不该被判非法：%v", bad)
	}
	if !reflect.DeepEqual(norm, [][]string{{"month", "channel_code"}}) {
		t.Errorf("未归一为小写：%v", norm)
	}
}

func TestCheckBucketIndexesDeclared_RejectsDuplicateIndexAcrossBucket(t *testing.T) {
	docs := []gate.BucketDoc{{ID: "b1", Indexes: [][]string{{"month"}, {"month"}}}}
	bad := gate.CheckBucketIndexesDeclared(docs)
	if len(bad) == 0 || !strings.Contains(strings.Join(bad, " "), "重复声明") {
		t.Fatalf("同桶重复声明同一索引未被拒绝：%v", bad)
	}
}

func TestCheckBucketIndexesDeclared_AcceptsRealRepoShape(t *testing.T) {
	docs := []gate.BucketDoc{{ID: "pnl_month", Indexes: [][]string{
		{"month"}, {"channel_code", "month"}, {"shop_id", "month"}, {"brand", "month"},
	}}}
	if bad := gate.CheckBucketIndexesDeclared(docs); len(bad) != 0 {
		t.Fatalf("真仓库形状被误判为非法：%v", bad)
	}
}

func TestCheckBucketIndexesMatchDDL_RealShapeMatches(t *testing.T) {
	docs := []gate.BucketDoc{{ID: "pnl_month", Indexes: [][]string{
		{"month"}, {"channel_code", "month"}, {"shop_id", "month"}, {"brand", "month"},
	}}}
	if bad := gate.CheckBucketIndexesMatchDDL(docs, ddlSample); len(bad) != 0 {
		t.Fatalf("声明与 DDL 逐条一致却报违规：%v", bad)
	}
}

func TestCheckBucketIndexesMatchDDL_ReportsBothDirections(t *testing.T) {
	// ① 声明了但 DDL 没有（少建索引 ⇒ 全表扫描）
	docs := []gate.BucketDoc{{ID: "pnl_month", Indexes: [][]string{
		{"month"}, {"channel_code", "month"}, {"shop_id", "month"}, {"brand", "month"},
		{"shop_id", "brand"}, // ← DDL 里没有
	}}}
	bad := gate.CheckBucketIndexesMatchDDL(docs, ddlSample)
	if len(bad) != 1 || !strings.Contains(bad[0], "没有对应的 CREATE INDEX") {
		t.Fatalf("「声明了但 DDL 没有」未被报出：%v", bad)
	}

	// ② DDL 有但 YAML 未声明（改 DDL 忘了同步 YAML）
	docs2 := []gate.BucketDoc{{ID: "pnl_month", Indexes: [][]string{
		{"month"}, {"channel_code", "month"}, {"shop_id", "month"},
	}}}
	bad2 := gate.CheckBucketIndexesMatchDDL(docs2, ddlSample)
	if len(bad2) != 1 || !strings.Contains(bad2[0], "未声明") {
		t.Fatalf("「DDL 有但未声明」未被报出：%v", bad2)
	}
}

// 顺序敏感：B-tree 列序不同即不同索引，`[month, channel_code]` ≠ `(channel_code, month)`。
func TestCheckBucketIndexesMatchDDL_OrderMatters(t *testing.T) {
	docs := []gate.BucketDoc{{ID: "pnl_month", Indexes: [][]string{
		{"month"}, {"month", "channel_code"}, {"shop_id", "month"}, {"brand", "month"},
	}}}
	bad := gate.CheckBucketIndexesMatchDDL(docs, ddlSample)
	if len(bad) == 0 {
		t.Fatal("列序不同未被报出（把 [month,channel_code] 当成 (channel_code,month)）")
	}
}

// 声明本身不合法时，本函数应**跳过该桶**（避免与声明层重复报错把真因淹没）。
func TestCheckBucketIndexesMatchDDL_SkipsMalformedDeclaration(t *testing.T) {
	docs := []gate.BucketDoc{{ID: "pnl_month", Indexes: [][]string{{"月份"}}}}
	if bad := gate.CheckBucketIndexesMatchDDL(docs, ddlSample); len(bad) != 0 {
		t.Fatalf("声明不合法时应跳过该桶、不重复报错，得：%v", bad)
	}
}

// 无索引声明的桶 + 无 DDL 索引 ⇒ 不报错（不是所有桶都必须建索引）。
func TestCheckBucketIndexesMatchDDL_NoIndexesBothSides(t *testing.T) {
	docs := []gate.BucketDoc{{ID: "small_bucket"}}
	if bad := gate.CheckBucketIndexesMatchDDL(docs, ddlSample); len(bad) != 0 {
		t.Fatalf("两侧都没有索引却报违规：%v", bad)
	}
}

func TestBucketTableName_UsesDocumentedPrefix(t *testing.T) {
	if got := gate.BucketTableName("pnl_month"); got != "bucket_pnl_month" {
		t.Fatalf("桶 ID → 表名约定变了：%q（本闸门依赖它，须同步 store/postgres.go 与 0003）", got)
	}
}

// ★ 被注释掉的 CREATE INDEX 不算数 —— 否则「声明 vs DDL」的比对会**恒真**（假绿）。
func TestParseCreateIndexes_IgnoresCommentedOutIndex(t *testing.T) {
	ddl := `
-- CREATE INDEX IF NOT EXISTS idx_gone ON bucket_pnl_month(shop_id, brand);
/* CREATE INDEX IF NOT EXISTS idx_also_gone ON bucket_pnl_month(brand); */
CREATE INDEX IF NOT EXISTS idx_real ON bucket_pnl_month(month);
`
	got := gate.ParseCreateIndexes(ddl)
	if len(got["bucket_pnl_month"]) != 1 {
		t.Fatalf("注释掉的索引被当成真索引：%v", got["bucket_pnl_month"])
	}
	if !reflect.DeepEqual(got["bucket_pnl_month"][0], []string{"month"}) {
		t.Errorf("保留下来的不是 idx_real：%v", got["bucket_pnl_month"][0])
	}
}
