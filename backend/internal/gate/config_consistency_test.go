// config_consistency_test.go —— 「同一份配置，多处消费」的一致性闸门。
//
// 动机（★ 真实事故）：
//   CI 真库作业曾把迁移步骤跑死在「未配置连接信息」上。根因不是 SQL 写错，
//   而是**变量名分叉**：
//     - spark-migrate / sparkd 只读 SPARK_DB_DSN
//     - 真库集成测试只读 SPARK_TEST_DB_DSN
//     - CI workflow 只设了 SPARK_TEST_DB_DSN
//   于是迁移拿不到 DSN，其后所有真库闸门被连环 skip —— 而**两侧单看都自洽**，
//   静态读任何单个文件都发现不了。
//
//   这类 bug 的共性是：契约分散在不同文件里，靠人眼对齐。
//   本文件的职责就是把它们钉在一起 —— 谁改了消费端，就逼他同步改供给端。
//
// 纪律：本文件不连库、不跑进程，纯静态文本断言（能在无库机器上跑）。
package gate_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repoFile 读取仓库根下的相对路径文件（测试工作目录 = backend/internal/gate）。
func repoFile(t *testing.T, rel string) string {
	t.Helper()
	p := filepath.Join("..", "..", "..", filepath.FromSlash(rel))
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", rel, err)
	}
	return string(b)
}

// TestConfig_CIWorkflowSetsBothDSNNames 断言 CI 的两个真库作业
// **同时**注入 SPARK_DB_DSN 与 SPARK_TEST_DB_DSN。
//
// 背景：运行期组件（sparkd / spark-migrate）读 SPARK_DB_DSN；
// 真库集成测试读 SPARK_TEST_DB_DSN。CI 若只设其一，
// 另一半就会以「未配置连接信息」或「静默跳过」的形式失败，
// 且报错位置离真实原因很远。
//
// 这条断言很朴素（就是找两个字符串），但它拦住的是一次真实的连环失败。
func TestConfig_CIWorkflowSetsBothDSNNames(t *testing.T) {
	yml := repoFile(t, ".github/workflows/ci.yml")

	for _, name := range []string{"SPARK_DB_DSN", "SPARK_TEST_DB_DSN"} {
		if !strings.Contains(yml, name) {
			t.Errorf("CI workflow 未注入 %s —— 若消费端需要它，真库步骤会以"+
				"「未配置连接信息」或「静默跳过」失败（见本文件顶部事故记录）", name)
		}
	}

	// 真库作业必须声明「不许跳过」，否则测试 t.Skip 会把失败掩盖成通过。
	if !strings.Contains(yml, "SPARK_REQUIRE_DB") {
		t.Error("CI workflow 未设 SPARK_REQUIRE_DB —— 真库测试缺 DSN 时会 skip，" +
			"CI 看起来绿了但数据面从未被验证")
	}
}

// TestConfig_DSNResolverAcceptsTestName 断言 DSN 解析器认得测试期变量名。
//
// 这是上面那条 CI 断言的「消费端镜像」：CI 设了 SPARK_TEST_DB_DSN，
// 解析器就必须认它。两边任缺其一，整条链就断。
func TestConfig_DSNResolverAcceptsTestName(t *testing.T) {
	src := repoFile(t, "backend/internal/db/db.go")

	if !strings.Contains(src, `"SPARK_TEST_DB_DSN"`) {
		t.Error("db.DSNFromEnv 未识别 SPARK_TEST_DB_DSN —— " +
			"CI 只设这个名字时，迁移/sparkd 会拿不到连接串")
	}
	if !strings.Contains(src, `"SPARK_DB_DSN"`) {
		t.Error("db.DSNFromEnv 未识别 SPARK_DB_DSN —— 运行期主名必须保留")
	}
}

// TestConfig_LaunchCheckIsolatesSmokeFromRealDSN 断言 launch-check 的
// **无库降级冒烟段**显式抹掉 DSN，而不是依赖「上游恰好没设」。
//
// 为什么必须显式：
//   该段的断言之一是 `healthz db=false`。若上游作业级 env 注入了 DSN
//   （CI 现在正是如此），sparkd 会真的去连库，而 launch-check 作业
//   并不保证先跑过迁移 ⇒ 连到空 schema 库，db 状态与预期不符，
//   失败信息指向 healthz，真实原因却是「环境串味」。
//
//   显式的 env -u 让这条断言在任何上游环境下都成立 —— 这才是**可复现**。
func TestConfig_LaunchCheckIsolatesSmokeFromRealDSN(t *testing.T) {
	sh := repoFile(t, "tools/verify/launch-check.sh")

	if !strings.Contains(sh, "env -u SPARK_DB_DSN") {
		t.Error("launch-check 的无库冒烟段未显式清除 SPARK_DB_DSN —— " +
			"上游若注入 DSN，`healthz db=false` 断言会因环境串味而失败")
	}
	if !strings.Contains(sh, "env -u SPARK_DB_DSN -u SPARK_TEST_DB_DSN") {
		t.Error("launch-check 只清了一个 DSN 变量名 —— 两个都要清，否则仍会串味")
	}
}

// TestConfig_DocsMentionBothDSNNames 断言文档写清了两个名字。
//
// 运维照着文档配环境；文档只写一个名字，就等于把这次事故再埋一次。
func TestConfig_DocsMentionBothDSNNames(t *testing.T) {
	readme := repoFile(t, "backend/README.md")
	if !strings.Contains(readme, "SPARK_TEST_DB_DSN") {
		t.Error("backend/README.md 未提到 SPARK_TEST_DB_DSN —— " +
			"运维会漏配真库测试用的连接串")
	}
}
