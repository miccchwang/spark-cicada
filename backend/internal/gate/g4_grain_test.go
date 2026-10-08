package gate_test

import (
	"strings"
	"testing"

	"github.com/miccchwang/spark-cicada/backend/internal/gate"
)

// ─────────────────────── ① 声明层：grain 可解析 ───────────────────────

func TestParseBucketGrain(t *testing.T) {
	cases := []struct {
		name    string
		raw     []string
		want    []string
		wantBad string // 非空 ⇒ 期望报错且信息含该子串
	}{
		{"合法", []string{"month", "channel_code", "shop_id", "brand"},
			[]string{"month", "channel_code", "shop_id", "brand"}, ""},
		{"大小写归一", []string{"Month", "BRAND"}, []string{"month", "brand"}, ""},
		{"带空白", []string{"  month  ", "brand"}, []string{"month", "brand"}, ""},
		{"空声明", nil, nil, "未声明 grain"},
		{"空切片", []string{}, nil, "未声明 grain"},
		{"自由文本", []string{"月份"}, nil, "非法列名"},
		{"含空格", []string{"month day"}, nil, "非法列名"},
		{"含逗号", []string{"month,brand"}, nil, "非法列名"},
		{"以数字开头", []string{"1st_month"}, nil, "非法列名"},
		{"重复列", []string{"month", "month"}, nil, "重复列"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, bad := gate.ParseBucketGrain("pnl_month", c.raw)
			if c.wantBad == "" {
				if len(bad) != 0 {
					t.Fatalf("期望合法，却报错：%v", bad)
				}
				if strings.Join(got, ",") != strings.Join(c.want, ",") {
					t.Fatalf("归一结果 = %v，期望 %v", got, c.want)
				}
				return
			}
			if len(bad) == 0 {
				t.Fatalf("期望报错（含 %q），却通过了", c.wantBad)
			}
			if !strings.Contains(strings.Join(bad, " "), c.wantBad) {
				t.Errorf("报错未含 %q：%v", c.wantBad, bad)
			}
		})
	}
}

func TestCheckBucketGrainDeclared(t *testing.T) {
	ok := gate.CheckBucketGrainDeclared([]gate.BucketDoc{
		{ID: "pnl_month", Grain: []string{"month", "brand"}},
	})
	if len(ok) != 0 {
		t.Fatalf("合法 grain 被报错：%v", ok)
	}

	bad := gate.CheckBucketGrainDeclared([]gate.BucketDoc{
		{ID: "pnl_month", Grain: nil},
		{ID: "b2", Grain: []string{"month day"}},
	})
	if len(bad) != 2 {
		t.Fatalf("期望 2 条违规（未声明 / 非法列名），实得 %d：%v", len(bad), bad)
	}
}

// ─────────────────── ② 与物理 DDL 同源：唯一键解析 ───────────────────

// syntheticBucketDDL 模拟真仓库的形态：表级 UNIQUE 约束 + 租户唯一索引 + 注释。
const syntheticBucketDDL = `
BEGIN;
-- 月度损益桶（对应 buckets/pnl_month.yaml）
CREATE TABLE IF NOT EXISTS bucket_pnl_month (
    id              bigserial   PRIMARY KEY,
    month           date        NOT NULL,
    channel_code    text        NOT NULL,
    shop_id         text        NOT NULL,
    brand           text        NULL,
    CONSTRAINT uq_bucket_pnl_month_spark_cicada_grain
        UNIQUE (month, channel_code, shop_id, brand)
);

-- ★ 这一条被注释掉了，不算数（否则比对恒真假绿）：
-- CREATE UNIQUE INDEX uq_ghost ON bucket_pnl_month (month);

-- 非唯一索引不该被当成唯一键
CREATE INDEX IF NOT EXISTS idx_bucket_pnl_month_month ON bucket_pnl_month(month);

-- 另一张表（不属于桶）：不应影响桶的比对
CREATE TABLE dim_org (
    id text PRIMARY KEY,
    scope text NOT NULL,
    UNIQUE (scope)
);
COMMIT;
`

// syntheticTenantDDL 模拟 0011 的租户收窄：DROP 全局键 + 建租户唯一索引（部分索引）。
const syntheticTenantDDL = `
DO $$
BEGIN
    DROP INDEX IF EXISTS uq_bucket_pnl_month_spark_cicada_grain;
    CREATE UNIQUE INDEX IF NOT EXISTS uq_bucket_pnl_month_tenant_grain
        ON bucket_pnl_month (tenant_id, month, channel_code, shop_id, brand)
        WHERE tenant_id IS NOT NULL;
END $$;
`

func TestParseUniqueKeys(t *testing.T) {
	keys := gate.ParseUniqueKeys(syntheticBucketDDL + syntheticTenantDDL)

	got := keys["bucket_pnl_month"]
	if len(got) != 2 {
		t.Fatalf("bucket_pnl_month 唯一键条数 = %d，期望 2（表级 UNIQUE + 租户唯一索引）：%v", len(got), got)
	}
	// 注释掉的那条**不得**被算进来。
	for _, k := range got {
		if strings.Join(k, ",") == "month" {
			t.Fatalf("被注释掉的 CREATE UNIQUE INDEX 竟被当成真唯一键：%v", got)
		}
	}
	// 非唯一索引不得被当成唯一键（dim_org 的表级 UNIQUE 应被解析到它自己名下）。
	if len(keys["dim_org"]) != 1 {
		t.Errorf("dim_org 的唯一键条数 = %d，期望 1（表级 UNIQUE (scope)）", len(keys["dim_org"]))
	}
}

// TestParseUniqueKeys_NoFalseUniqueIndex：普通 `CREATE INDEX` 不得被误当唯一键。
func TestParseUniqueKeys_NoFalseUniqueIndex(t *testing.T) {
	keys := gate.ParseUniqueKeys(syntheticBucketDDL)
	for _, k := range keys["bucket_pnl_month"] {
		if strings.Join(k, ",") == "month" {
			t.Fatalf("普通 CREATE INDEX (month) 被误当成唯一键 ⇒ 会制造假绿：%v", keys["bucket_pnl_month"])
		}
	}
}

// ─────────────────── ③ 与物理 DDL 同源：比对 ───────────────────

func TestCheckBucketGrainMatchesDDL(t *testing.T) {
	docs := []gate.BucketDoc{
		{ID: "pnl_month", Grain: []string{"month", "channel_code", "shop_id", "brand"}},
	}

	// ① 真仓库形态：表级 UNIQUE（无 tenant 前缀）+ 租户唯一索引（有 tenant 前缀）
	//    两者剥离后都等于 grain ⇒ 零违规。
	bad := gate.CheckBucketGrainMatchesDDL(docs, syntheticBucketDDL+syntheticTenantDDL)
	if len(bad) != 0 {
		t.Fatalf("真仓库形态应零违规，实得：%v", bad)
	}

	// ② 顺序不敏感：grain 写成另一顺序，仍应通过（唯一键是集合语义）。
	reordered := []gate.BucketDoc{
		{ID: "pnl_month", Grain: []string{"brand", "shop_id", "channel_code", "month"}},
	}
	if bad := gate.CheckBucketGrainMatchesDDL(reordered, syntheticBucketDDL+syntheticTenantDDL); len(bad) != 0 {
		t.Fatalf("grain 顺序不同不应报错（唯一键是集合语义），实得：%v", bad)
	}

	// ③ YAML 漂移：给 grain 加一列 spu，而迁移未动 ⇒ 必须报「分叉」。
	driftedYAML := []gate.BucketDoc{
		{ID: "pnl_month", Grain: []string{"month", "channel_code", "shop_id", "brand", "spu"}},
	}
	bad = gate.CheckBucketGrainMatchesDDL(driftedYAML, syntheticBucketDDL+syntheticTenantDDL)
	if len(bad) == 0 {
		t.Fatal("YAML 侧 grain 加列后**没有**报错 ⇒ 漏检")
	}
	if !strings.Contains(strings.Join(bad, " "), "分叉") {
		t.Errorf("报错信息未指出「分叉」：%v", bad)
	}

	// ④ DDL 漂移：迁移把唯一键的 brand 去掉，而 YAML 未动 ⇒ 必须报错。
	ddlDrifted := strings.Replace(syntheticBucketDDL,
		"UNIQUE (month, channel_code, shop_id, brand)",
		"UNIQUE (month, channel_code, shop_id)", 1)
	if ddlDrifted == syntheticBucketDDL {
		t.Fatal("夹具无效：DDL 替换未命中")
	}
	bad = gate.CheckBucketGrainMatchesDDL(docs, ddlDrifted)
	if len(bad) == 0 {
		t.Fatal("迁移侧唯一键改列后**没有**报错 ⇒ 漏检")
	}

	// ⑤ 表上没有任何唯一键 ⇒ 必须报错（「一行一个 grain」失去保障）。
	noKey := strings.Replace(syntheticBucketDDL,
		"CONSTRAINT uq_bucket_pnl_month_spark_cicada_grain\n        UNIQUE (month, channel_code, shop_id, brand)", "", 1)
	if noKey == syntheticBucketDDL {
		t.Fatal("夹具无效：约束删除未命中")
	}
	bad = gate.CheckBucketGrainMatchesDDL(docs, noKey)
	if len(bad) == 0 {
		t.Fatal("表上无唯一键时**没有**报错 ⇒ 漏检")
	}
	if !strings.Contains(strings.Join(bad, " "), "没有任何唯一键") {
		t.Errorf("报错信息未指出缺唯一键：%v", bad)
	}

	// ⑥ 声明不合法时跳过本桶（由 ① 报出，不重复）。
	bad = gate.CheckBucketGrainMatchesDDL(
		[]gate.BucketDoc{{ID: "pnl_month", Grain: []string{"月份"}}},
		syntheticBucketDDL+syntheticTenantDDL)
	if len(bad) != 0 {
		t.Fatalf("声明不合法时本函数应跳过（避免重复报错），实得：%v", bad)
	}
}

// TestCheckBucketGrainMatchesDDL_FixtureIsNotVacuous 是**夹具自证**：
// 合成 DDL 必须真的被解析出唯一键，否则上面那些「零违规」可能只是因为
// **两侧都没东西可比**（假绿）。
func TestCheckBucketGrainMatchesDDL_FixtureIsNotVacuous(t *testing.T) {
	keys := gate.ParseUniqueKeys(syntheticBucketDDL + syntheticTenantDDL)
	if len(keys["bucket_pnl_month"]) == 0 {
		t.Fatal("夹具无效：合成 DDL 没解析出任何唯一键")
	}
	// 且必须解析到**租户前缀**那条（证明 CREATE UNIQUE INDEX 分支真的工作）。
	found := false
	for _, k := range keys["bucket_pnl_month"] {
		if len(k) > 0 && k[0] == "tenant_id" {
			found = true
		}
	}
	if !found {
		t.Fatal("夹具无效：没解析到带 tenant_id 前缀的租户唯一索引")
	}
}
