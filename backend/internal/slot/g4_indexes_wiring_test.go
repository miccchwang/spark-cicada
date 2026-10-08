package slot_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miccchwang/spark-cicada/backend/internal/gate"
	"github.com/miccchwang/spark-cicada/backend/internal/slot"
)

// 本文件是 G4 第十一侧（桶索引声明）的**链路级**闸门：
// 从真 `buckets/*.yaml` + 真 `sql/migrations/0003_precompute.sql` 进，
// 以「声明与 DDL 逐条一致 / 不一致」出 —— 而不是直接调判定函数看返回值。
//
// ★ 为什么必须读**真迁移文件**：`indexes` 的全部意义在于「对应一张物理表上真的
//   建了这些索引」。只测判定函数会退化成「函数返回空切片」，摘掉一条索引照样绿。

const bucketIndexMigration = "sql/migrations/0003_precompute.sql"

// readMigration 读真迁移文件（CRLF 归一为 LF，便于做字符串手术）。
func readMigration(t *testing.T, rel string) string {
	t.Helper()
	root := repoRoot(t)
	raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("读迁移 %s 失败（本用例必须读真 DDL）：%v", rel, err)
	}
	return strings.ReplaceAll(string(raw), "\r\n", "\n")
}

// bucketIndexDocs 从**真实加载**的桶注册表构造索引声明文档。
func bucketIndexDocs(br *slot.BucketRegistry) []gate.BucketDoc {
	var docs []gate.BucketDoc
	for _, b := range br.Buckets() {
		docs = append(docs, gate.BucketDoc{ID: b.ID, Indexes: b.Indexes})
	}
	return docs
}

// TestG4Indexes_RealRepoYamlMatchesMigrationDDL 断言真仓库两侧逐条一致。
func TestG4Indexes_RealRepoYamlMatchesMigrationDDL(t *testing.T) {
	_, br := loadAll(t)
	docs := bucketIndexDocs(br)
	if len(docs) == 0 {
		t.Fatal("夹具无效：没从真仓库读到任何桶定义")
	}

	ddl := readMigration(t, bucketIndexMigration)
	parsed := gate.ParseCreateIndexes(ddl)

	// ★ 夹具自证：真 DDL 里确实解析到了桶表索引、YAML 里也确实声明了索引。
	// 否则「零违规」可能是因为**两侧都没东西可比**（假绿）。
	totalDDL, totalYAML := 0, 0
	for _, d := range docs {
		totalDDL += len(parsed[gate.BucketTableName(d.ID)])
		totalYAML += len(d.Indexes)
	}
	if totalDDL == 0 || totalYAML == 0 {
		t.Fatalf("夹具无效：DDL 解析到 %d 条索引、YAML 声明 %d 条 —— 两侧至少各要 >0，"+
			"否则「零违规」是因为压根没比对", totalDDL, totalYAML)
	}

	if bad := gate.CheckBucketIndexesMatchDDL(docs, ddl); len(bad) != 0 {
		t.Fatalf("真仓库的桶索引声明与迁移 DDL 不一致（G4 第十一侧）：\n  - %s",
			strings.Join(bad, "\n  - "))
	}
}

// TestG4Indexes_MigrationDriftIsCaught：把真 DDL 里的一条 CREATE INDEX **注释掉**，
// 断言闸门立刻报出「声明了但 DDL 没有」（该过滤维度退化成全表扫描）。
func TestG4Indexes_MigrationDriftIsCaught(t *testing.T) {
	_, br := loadAll(t)
	docs := bucketIndexDocs(br)
	ddl := readMigration(t, bucketIndexMigration)

	const target = "CREATE INDEX IF NOT EXISTS idx_bucket_pnl_month_brand"
	if !strings.Contains(ddl, target) {
		t.Fatalf("夹具无效：真 DDL 里找不到 %q（迁移改了，请同步本用例）", target)
	}
	drifted := strings.Replace(ddl, target, "-- "+target, 1)

	bad := gate.CheckBucketIndexesMatchDDL(docs, drifted)
	if len(bad) == 0 {
		t.Fatal("把一条 CREATE INDEX 注释掉后，闸门**没有**报出「声明了但 DDL 没有」⇒ 漏检")
	}
	if !strings.Contains(strings.Join(bad, " "), "没有对应的 CREATE INDEX") {
		t.Errorf("报错信息未指出缺索引：%v", bad)
	}
}

// TestG4Indexes_YamlDriftIsCaught：把 YAML 侧的某条索引声明删掉，
// 断言闸门报出「DDL 有但 YAML 未声明」。
func TestG4Indexes_YamlDriftIsCaught(t *testing.T) {
	_, br := loadAll(t)
	docs := bucketIndexDocs(br)
	ddl := readMigration(t, bucketIndexMigration)

	drifted := make([]gate.BucketDoc, len(docs))
	copy(drifted, docs)
	cut := false
	for i := range drifted {
		if len(drifted[i].Indexes) > 0 {
			drifted[i].Indexes = drifted[i].Indexes[:len(drifted[i].Indexes)-1]
			cut = true
			break
		}
	}
	if !cut {
		t.Fatal("夹具无效：真仓库里没有任何桶声明了索引")
	}

	bad := gate.CheckBucketIndexesMatchDDL(drifted, ddl)
	if len(bad) == 0 {
		t.Fatal("砍掉一条 YAML 索引声明后闸门**没有**报出「DDL 有但未声明」⇒ 漏检")
	}
	if !strings.Contains(strings.Join(bad, " "), "未声明") {
		t.Errorf("报错信息未指出未声明：%v", bad)
	}
}

// TestG4Indexes_LoadRejectsMalformedIndex 是**行为级**断言：
// 从 `LoadBucketRegistry` 入口进，一个声明了非法索引的桶必须**加载失败**。
//
// ★ 为什么必须有这一条（而不是只有静态扫描）：
//
//	若 `slot/bucket.go` 没把 `b.Indexes` 映射进 `gate.BucketDoc`（或 validate
//	没调用判定函数），静态扫描能看出「调用点没了」，但**映射丢失**看不出 ——
//	而映射一丢，`docs[].Indexes` 恒为 nil，声明层闸门就**恒放行**（假绿）。
//	本用例走真实加载入口，两种破坏都会让它红。
func TestG4Indexes_LoadRejectsMalformedIndex(t *testing.T) {
	root := repoRoot(t)
	reg, err := slot.LoadRegistry(filepath.Join(root, "slots"), filepath.Join(root, "algorithms"))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct{ name, indexes, want string }{
		{"列名含空格", "  - [\"month day\"]\n", "非法列名"},
		{"重复声明同一索引", "  - [month]\n  - [month]\n", "重复声明"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := writeTempBuckets(t, reg, map[string]string{
				"b.yaml": "id: b_bad\ngrain: [month]\nproduced_by: [algo.gp]\n" +
					"algo_versions: {algo.gp: 1}\nrefresh: monthly\nindexes:\n" + c.indexes,
			})
			if err == nil {
				t.Fatalf("★ 非法索引声明（%s）竟加载成功 ⇒ 声明层闸门没有真实生产调用点"+
					"（或 Indexes 没被映射进 BucketDoc）", c.name)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("报错未含 %q（可能被别的闸门先拦下 ⇒ 本用例失去鉴别力）：%v",
					c.want, err)
			}
		})
	}
}

// TestG4Indexes_HasProductionCallSite 钉住「闸门不是装饰」：
// `gate.CheckBucketIndexesDeclared` 必须在 `bucket.go: validate` 里被**真实调用**。
//
// ★ needle 运行时拼接（防本文件自指恒真 —— 见 g4_version_wiring_test.go 的说明）。
func TestG4Indexes_HasProductionCallSite(t *testing.T) {
	body := readMethodBody(t, "bucket.go",
		"func (r *BucketRegistry) validate(reg *Registry, registeredRules map[string]bool) error {")
	if len(body) < 2000 {
		t.Fatalf("validate 方法体只读到 %d 字节 —— 夹具没读到真源码", len(body))
	}
	needle := "gate." + "CheckBucketIndexesDeclared"
	if !strings.Contains(body, needle) {
		t.Errorf("★ gate.CheckBucketIndexesDeclared 在 bucket.validate 里**没有真实生产调用点**" +
			" ⇒ 索引声明层闸门是装饰（有定义、没消费）")
	}
}

// TestG4Indexes_ScannerCanDetectRemoval 是**夹具自证**：
// 扫描器对绝不存在的名字必须报「找不到」，对已确认存在的名字必须能扫到。
func TestG4Indexes_ScannerCanDetectRemoval(t *testing.T) {
	body := readMethodBody(t, "bucket.go",
		"func (r *BucketRegistry) validate(reg *Registry, registeredRules map[string]bool) error {")
	if strings.Contains(body, "gate.CheckThisDoesNotExistAnywhere") {
		t.Fatal("夹具无效：不存在的名字竟被扫到")
	}
	if !strings.Contains(body, "gate."+"CheckBucketProducersRegistered") {
		t.Fatal("夹具无效：连已确认存在的 CheckBucketProducersRegistered 都扫不到 ⇒ 盯错文件")
	}
}
