package slot_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/miccchwang/spark-cicada/backend/internal/gate"
	"github.com/miccchwang/spark-cicada/backend/internal/slot"
)

// 本文件是 G4 第十二侧（桶粒度声明 grain）的**链路级**闸门：
// 从真 `buckets/*.yaml` + 真 `sql/migrations/0003_precompute.sql` + `0011_rls.sql` 进，
// 以「声明与物理唯一键一致 / 不一致」出 —— 而不是直接调判定函数看返回值。
//
// ★ 为什么必须读**真迁移文件**：`grain` 的全部意义在于「物理表上真的按这些列去重」。
// 只测判定函数会退化成「函数返回空切片」，改一条唯一键照样绿。
//
// ★ 为什么 0011 也要读：租户迁移把桶的**全局**唯一键改为「租户内唯一」——
//   `uq_bucket_pnl_month_tenant_grain ON bucket_pnl_month (tenant_id, month, …)`。
//   它是桶表**当前的**有效唯一键；漏读它就等于只看被 DROP 掉的旧键。

const bucketGrainMigrations = "sql/migrations/0003_precompute.sql"
const bucketGrainTenantMigration = "sql/migrations/0011_rls.sql"

// realBucketDDL 读出真迁移里与桶唯一键相关的全部 DDL（0003 + 0011）。
func realBucketDDL(t *testing.T) string {
	t.Helper()
	return readMigration(t, bucketGrainMigrations) + "\n" + readMigration(t, bucketGrainTenantMigration)
}

// bucketGrainDocs 从**真实加载**的桶注册表构造粒度声明文档。
func bucketGrainDocs(br *slot.BucketRegistry) []gate.BucketDoc {
	var docs []gate.BucketDoc
	for _, b := range br.Buckets() {
		docs = append(docs, gate.BucketDoc{ID: b.ID, Grain: b.Grain})
	}
	return docs
}

// TestG4Grain_RealRepoYamlMatchesPhysicalUniqueKey 断言真仓库两侧一致。
func TestG4Grain_RealRepoYamlMatchesPhysicalUniqueKey(t *testing.T) {
	_, br := loadAll(t)
	docs := bucketGrainDocs(br)
	if len(docs) == 0 {
		t.Fatal("夹具无效：没从真仓库读到任何桶定义")
	}

	ddl := realBucketDDL(t)

	// ★ 夹具自证：真 DDL 里确实解析到了桶表的唯一键、YAML 里也确实声明了 grain。
	// 否则「零违规」可能是因为**两侧都没东西可比**（假绿）。
	parsed := gate.ParseUniqueKeys(ddl)
	totalDDL, totalYAML := 0, 0
	for _, d := range docs {
		totalDDL += len(parsed[gate.BucketTableName(d.ID)])
		totalYAML += len(d.Grain)
	}
	if totalDDL == 0 || totalYAML == 0 {
		t.Fatalf("夹具无效：DDL 解析到 %d 条唯一键、YAML 声明 %d 个 grain 列 —— "+
			"两侧至少各要 >0，否则「零违规」是因为压根没比对", totalDDL, totalYAML)
	}
	// 且必须解析到带 tenant_id 前缀的租户唯一索引（证明 0011 真被读到）。
	foundTenant := false
	for _, d := range docs {
		for _, k := range parsed[gate.BucketTableName(d.ID)] {
			if len(k) > 0 && k[0] == "tenant_id" {
				foundTenant = true
			}
		}
	}
	if !foundTenant {
		t.Fatal("夹具无效：没从真迁移里解析到带 tenant_id 前缀的租户唯一索引（0011 未被读到？）")
	}

	if bad := gate.CheckBucketGrainMatchesDDL(docs, ddl); len(bad) != 0 {
		t.Fatalf("真仓库的桶粒度声明与物理唯一键不一致（G4 第十二侧）：\n  - %s",
			strings.Join(bad, "\n  - "))
	}
}

// TestG4Grain_MigrationDriftIsCaught：把真 DDL 里唯一键的一列删掉，
// 断言闸门立刻报出「声明与物理去重键分叉」。
func TestG4Grain_MigrationDriftIsCaught(t *testing.T) {
	_, br := loadAll(t)
	docs := bucketGrainDocs(br)
	ddl := realBucketDDL(t)

	const target = "UNIQUE (month, channel_code, shop_id, brand)"
	if !strings.Contains(ddl, target) {
		t.Fatalf("夹具无效：真 DDL 里找不到 %q（迁移改了，请同步本用例）", target)
	}
	drifted := strings.Replace(ddl, target, "UNIQUE (month, channel_code, shop_id)", 1)

	bad := gate.CheckBucketGrainMatchesDDL(docs, drifted)
	if len(bad) == 0 {
		t.Fatal("把唯一键的一列删掉后，闸门**没有**报错 ⇒ 漏检")
	}
	if !strings.Contains(strings.Join(bad, " "), "分叉") {
		t.Errorf("报错信息未指出「分叉」：%v", bad)
	}
}

// TestG4Grain_YamlDriftIsCaught：给 YAML 侧的 grain 加一列，断言闸门报错。
func TestG4Grain_YamlDriftIsCaught(t *testing.T) {
	_, br := loadAll(t)
	docs := bucketGrainDocs(br)
	ddl := realBucketDDL(t)

	drifted := make([]gate.BucketDoc, len(docs))
	copy(drifted, docs)
	added := false
	for i := range drifted {
		if len(drifted[i].Grain) > 0 {
			drifted[i].Grain = append(append([]string{}, drifted[i].Grain...), "spu")
			added = true
			break
		}
	}
	if !added {
		t.Fatal("夹具无效：真仓库里没有任何桶声明了 grain")
	}

	bad := gate.CheckBucketGrainMatchesDDL(drifted, ddl)
	if len(bad) == 0 {
		t.Fatal("给 grain 加一列后闸门**没有**报错 ⇒ 漏检")
	}
}

// TestG4Grain_NoUniqueKeyIsCaught：把真 DDL 里桶表的两条唯一键**都去掉**
// （表级 UNIQUE 约束删除 + 租户唯一索引降级为普通索引），
// 断言闸门报出「没有任何唯一键」（一行一个 grain 失去保障）。
func TestG4Grain_NoUniqueKeyIsCaught(t *testing.T) {
	_, br := loadAll(t)
	docs := bucketGrainDocs(br)
	ddl := realBucketDDL(t)

	// ① 删掉 0003 的表级 UNIQUE 约束。
	stripped := strings.Replace(ddl,
		"UNIQUE (month, channel_code, shop_id, brand)", "", 1)
	// ② 把 0011 的租户**唯一**索引降级为普通索引（去掉 UNIQUE 关键字）。
	const tenantUniqueIdx = "CREATE UNIQUE INDEX IF NOT EXISTS uq_bucket_pnl_month_tenant_grain"
	if !strings.Contains(stripped, tenantUniqueIdx) {
		t.Fatalf("夹具无效：真 DDL 里找不到 %q", tenantUniqueIdx)
	}
	stripped = strings.Replace(stripped, tenantUniqueIdx,
		"CREATE INDEX IF NOT EXISTS uq_bucket_pnl_month_tenant_grain", 1)
	if stripped == ddl {
		t.Fatal("夹具无效：DDL 替换未命中")
	}

	bad := gate.CheckBucketGrainMatchesDDL(docs, stripped)
	if len(bad) == 0 {
		t.Fatal("表上没有任何唯一键时闸门**没有**报错 ⇒ 漏检")
	}
	if !strings.Contains(strings.Join(bad, " "), "没有任何唯一键") {
		t.Errorf("报错信息未指出缺唯一键：%v", bad)
	}
}

// TestG4Grain_LoadRejectsMalformedGrain 是**行为级**断言：
// 从 `LoadBucketRegistry` 入口进，一个声明了非法 grain 的桶必须**加载失败**。
//
// ★ 为什么必须有这一条（而不是只有静态扫描）：若 `slot/bucket.go` 没把
// `b.Grain` 映射进 `gate.BucketDoc`（或 validate 没调用判定函数），
// 静态扫描能看出「调用点没了」，但**映射丢失**看不出 —— 而映射一丢，
// `docs[].Grain` 恒为 nil，声明层闸门就**恒放行**（假绿）。本用例走真实加载
// 入口，两种破坏都会让它红。
func TestG4Grain_LoadRejectsMalformedGrain(t *testing.T) {
	root := repoRoot(t)
	reg, err := slot.LoadRegistry(filepath.Join(root, "slots"), filepath.Join(root, "algorithms"))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct{ name, grain, want string }{
		{"非法列名", "grain: [\"month day\"]\n", "非法列名"},
		{"重复列", "grain: [month, month]\n", "重复列"},
		{"空 grain", "grain: []\n", "未声明 grain"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := writeTempBuckets(t, reg, map[string]string{
				"b.yaml": "id: b_bad\n" + c.grain +
					"produced_by: [algo.gp]\nalgo_versions: {algo.gp: 1}\nrefresh: monthly\n",
			})
			if err == nil {
				t.Fatalf("★ 非法 grain 声明（%s）竟加载成功 ⇒ 声明层闸门没有真实生产调用点"+
					"（或 Grain 没被映射进 BucketDoc）", c.name)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("报错未含 %q（可能被别的闸门先拦下 ⇒ 本用例失去鉴别力）：%v",
					c.want, err)
			}
		})
	}
}

// TestG4Grain_HasProductionCallSite 钉住「闸门不是装饰」：
// `gate.CheckBucketGrainDeclared` 必须在 `bucket.go: validate` 里被**真实调用**。
//
// ★ needle 运行时拼接（防本文件自指恒真 —— 见 g4_version_wiring_test.go 的说明）。
func TestG4Grain_HasProductionCallSite(t *testing.T) {
	body := readMethodBody(t, "bucket.go",
		"func (r *BucketRegistry) validate(reg *Registry, registeredRules map[string]bool) error {")
	if len(body) < 600 {
		t.Fatalf("validate 方法体只读到 %d 字节（去注释后）—— 夹具没读到真源码", len(body))
	}
	needle := "gate." + "CheckBucketGrainDeclared"
	if !strings.Contains(body, needle) {
		t.Errorf("★ gate.CheckBucketGrainDeclared 在 bucket.validate 里**没有真实生产调用点**" +
			" ⇒ 粒度声明层闸门是装饰（有定义、没消费）")
	}
}

// TestG4Grain_ScannerCanDetectRemoval 是**夹具自证**：
// 扫描器对绝不存在的名字必须报「找不到」，对已确认存在的名字必须能扫到。
func TestG4Grain_ScannerCanDetectRemoval(t *testing.T) {
	body := readMethodBody(t, "bucket.go",
		"func (r *BucketRegistry) validate(reg *Registry, registeredRules map[string]bool) error {")
	if strings.Contains(body, "gate.CheckThisDoesNotExistAnywhere") {
		t.Fatal("夹具无效：不存在的名字竟被扫到")
	}
	if !strings.Contains(body, "gate."+"CheckBucketProducersRegistered") {
		t.Fatal("夹具无效：连已确认存在的 CheckBucketProducersRegistered 都扫不到 ⇒ 盯错文件")
	}
}
