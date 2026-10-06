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

	// ★ 必须断言「YAML **键**存在」，不能只找子串。
	//
	// 最初的写法是 strings.Contains(yml, "SPARK_DB_DSN")，
	// 而 SPARK_TEST_DB_DSN 本身就**包含** SPARK_DB_DSN 这个子串 ⇒
	// 即使 SPARK_DB_DSN 键被整行删掉，断言仍然通过。
	// 这个弱断言在负向测试里当场被抓住（注入违规后闸门没响）。
	//
	// 教训：文本闸门的断言必须贴着「真正产生效果的语法形态」写，
	// 子串包含在命名有前缀关系时几乎必然给出假阴性。
	for _, name := range []string{"SPARK_DB_DSN", "SPARK_TEST_DB_DSN"} {
		if !hasYAMLKey(yml, name) {
			t.Errorf("CI workflow 未以 YAML 键注入 %s —— 若消费端需要它，真库步骤会以"+
				"「未配置连接信息」或「静默跳过」失败（见本文件顶部事故记录）。\n"+
				"注意：仅在其变量值/注释里出现该名字**不算**注入。", name)
		}
	}

	// 真库作业必须声明「不许跳过」，否则测试 t.Skip 会把失败掩盖成通过。
	if !hasYAMLKey(yml, "SPARK_REQUIRE_DB") {
		t.Error("CI workflow 未以 YAML 键注入 SPARK_REQUIRE_DB —— 真库测试缺 DSN 时会 skip，" +
			"CI 看起来绿了但数据面从未被验证")
	}
}

// hasYAMLKey 判断文档里是否存在形如 `KEY:` 的 YAML 映射键
// （允许前导空白与引号），从而把「真的注入了这个变量」与
// 「这个名字恰好作为子串出现在别处」区分开。
func hasYAMLKey(doc, key string) bool {
	for _, line := range strings.Split(doc, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue // 注释不算注入
		}
		for _, form := range []string{key + ":", "'" + key + "':", `"` + key + `":`} {
			if strings.HasPrefix(trimmed, form) {
				return true
			}
		}
	}
	return false
}

// TestHasYAMLKey_RejectsSubstringOnly 是 hasYAMLKey 的**负向自测**。
//
// 为什么闸门本身也要有测试：
//   文本闸门最大的失效模式不是「漏写」，而是**写了却永远为真**。
//   这正是本文件开头记录的那次事故的翻版 —— 只是发生在闸门层。
//   若 hasYAMLKey 退化成 strings.Contains，下面每条 case 都会误判，
//   而 CI 依然全绿。**只有负向用例能让闸门「证明自己会响」。**
//
// 核心 case：SPARK_TEST_DB_DSN 含 SPARK_DB_DSN 子串。
//   只写测试期名、不写主名的配置，必须被判为「未注入主名」。
func TestHasYAMLKey_RejectsSubstringOnly(t *testing.T) {
	cases := []struct {
		name string
		doc  string
		key  string
		want bool
	}{
		{
			name: "只有前缀包含关系时不算命中（事故复现）",
			doc:  "      SPARK_TEST_DB_DSN: postgres://x/y\n",
			key:  "SPARK_DB_DSN",
			want: false,
		},
		{
			name: "真正的 YAML 键命中",
			doc:  "      SPARK_DB_DSN: postgres://x/y\n",
			key:  "SPARK_DB_DSN",
			want: true,
		},
		{
			name: "注释里的名字不算注入",
			doc:  "      # SPARK_DB_DSN: postgres://x/y\n",
			key:  "SPARK_DB_DSN",
			want: false,
		},
		{
			name: "值里出现该名字不算注入",
			doc:  "      SOMETHING_ELSE: SPARK_DB_DSN\n",
			key:  "SPARK_DB_DSN",
			want: false,
		},
		{
			name: "带引号的键也算",
			doc:  "      \"SPARK_DB_DSN\": postgres://x/y\n",
			key:  "SPARK_DB_DSN",
			want: true,
		},
		{
			name: "零缩进的键也算",
			doc:  "SPARK_DB_DSN: postgres://x/y\n",
			key:  "SPARK_DB_DSN",
			want: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasYAMLKey(tc.doc, tc.key); got != tc.want {
				t.Errorf("hasYAMLKey(%q, %q) = %v, 期望 %v —— "+
					"闸门在此处误判会让整个 CI 作业静默失效", tc.doc, tc.key, got, tc.want)
			}
		})
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

// TestConfig_LaunchCheckGuardsWindowsToolchain 断言 launch-check 只在
// **确实用托管工具链**时才注入 Windows 专属的 GOROOT/GOPATH/GOCACHE/GOTMPDIR。
//
// ★ 真实事故（CI 第三次红）：
//   脚本原先无条件写
//     export GOROOT="${GOROOT:-C:/Users/.../go1.27.1}"
//   本机跑没问题（托管的 go.exe 就在那儿）。但 CI 是 Linux：
//     - GOEXE 已按 PATH 回退成 /usr/bin/go（第 57 行那层回退是对的）
//     - GOROOT 却仍被强制指到一个 **Windows 路径**
//   Go 于是找不到自己的标准库，`go version` 失败 ⇒ 步骤 0 判「go 不可用」
//   ⇒ 脚本当场 exit 1。CI 摘要里只见「launch-check 在步骤 0 中途退出」，
//   真正的跨平台路径污染被完全掩盖。
//
//   修复：把四行 export 包进 `if [ "$GOEXE" = "$MANAGED_GO" ]; then ... fi`。
//
// 这条闸门守的就是「那个 if 还在，且 MANAGED_GO 与 GOEXE 的默认值同源」——
// 后者尤其重要：两处硬编码路径一旦漂移，守卫就恒为假，GOROOT 永不注入，
// 本机反而会坏。**守卫本身也需要被守卫。**
func TestConfig_LaunchCheckGuardsWindowsToolchain(t *testing.T) {
	sh := repoFile(t, "tools/verify/launch-check.sh")

	// 1) 必须先把托管路径抽成变量，作为守卫的判据。
	if !strings.Contains(sh, "MANAGED_GO=") {
		t.Fatal("launch-check 未定义 MANAGED_GO —— 没有判据变量，就无法把 Windows " +
			"专属环境变量的注入限制在托管工具链上（详见本函数注释里的事故记录）")
	}

	// 2) 四个 export 必须都在守卫体内。
	//
	//    用「守卫到 fi 之间的片段」做区间断言，而不是全文找 export ——
	//    否则把 export 挪到 if 外面（也就是把 bug 改回去）闸门依然会通过。
	guardStart := strings.Index(sh, `if [ "$GOEXE" = "$MANAGED_GO" ]; then`)
	if guardStart < 0 {
		t.Fatal("launch-check 缺少 `if [ \"$GOEXE\" = \"$MANAGED_GO\" ]; then` 守卫 —— " +
			"Win/Linux 双平台上必然有一边坏掉")
	}
	guardEnd := strings.Index(sh[guardStart:], "\nfi")
	if guardEnd < 0 {
		t.Fatal("launch-check 的托管工具链守卫没有闭合的 fi —— 脚本会被截断执行")
	}
	guardBody := sh[guardStart : guardStart+guardEnd]

	for _, v := range []string{"GOROOT", "GOPATH", "GOCACHE", "GOTMPDIR"} {
		if !strings.Contains(guardBody, "export "+v+"=") {
			t.Errorf("launch-check 的托管守卫体内缺少 export %s —— "+
				"要么它被挪到了守卫之外（Windows 路径会污染 Linux CI），"+
				"要么被误删（本机托管工具链会找不到标准库）", v)
		}
	}

	// 3) 守卫判据必须与 GOEXE 默认值指向同一个 bin 路径（防漂移）。
	//
	//    两条路径分别在脚本里硬编码了两次，任何一次被改都会让守卫失效。
	//    这里断言两处字符串一致，把这种漂移钉死在测试里。
	goexeLine := ""
	for _, line := range strings.Split(sh, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "GOEXE=") {
			goexeLine = line
			break
		}
	}
	if goexeLine == "" {
		t.Fatal("launch-check 未定义 GOEXE")
	}
	// 从 `GOEXE="${GOEXE:-<path>}"` 里取出 <path>。
	const prefix = `${GOEXE:-`
	i := strings.Index(goexeLine, prefix)
	if i < 0 {
		t.Fatalf("GOEXE 的默认值写法变了（期望形如 ${GOEXE:-<path>}）：%s\n"+
			"本闸门依赖这个形态来提取路径，请同步更新测试。", goexeLine)
	}
	rest := goexeLine[i+len(prefix):]
	j := strings.IndexAny(rest, "}\"")
	if j < 0 {
		t.Fatalf("无法从 GOEXE 默认值里解析出路径：%s", goexeLine)
	}
	goexeDefault := rest[:j]

	managedLine := ""
	for _, line := range strings.Split(sh, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "MANAGED_GO=") {
			managedLine = line
			break
		}
	}
	managed := strings.Trim(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(managedLine), "MANAGED_GO=")), `"`)

	if managed != goexeDefault {
		t.Errorf("MANAGED_GO 与 GOEXE 默认值不一致，守卫将恒为假：\n"+
			"  MANAGED_GO = %q\n  GOEXE 默认 = %q\n"+
			"两处路径已漂移 —— 后果是 GOROOT/GOPATH/GOCACHE/GOTMPDIR 永不注入，"+
			"本机托管工具链会因找不到标准库而失败。", managed, goexeDefault)
	}
}

// TestConfig_GearTableMatchesCollectCode 断言**两处**降速档位定义不漂移。
//
// 档位表在仓库里存在两次：
//   1. `sql/migrations/0005_collect_staging.sql` 的 collect_rate_gear 种子（DB 真值）
//   2. `backend/internal/collect/collect.go` 的 GearTable（代码默认值）
//
// 为什么必须钉在一起：采集循环读代码里的档位算间隔，而 DB 里的档位
// 是给 IT 调优与审计看的。两处一旦漂移，会出现
// 「日志说退到 4 秒，实际 sleep 8 秒」这类**只有对数才能发现**的偏差，
// 且会让人对限流分析得出错误结论。
//
// 真库集成测试也有一条等价断言（TestIntegration_GearTableMatchesHardcoded），
// 但那条需要 DSN。本闸门是它的**无库版本**，保证在没有 Postgres 的机器上
// （包括大部分本地开发）也能拦住漂移。
func TestConfig_GearTableMatchesCollectCode(t *testing.T) {
	sh := repoFile(t, "sql/migrations/0005_collect_staging.sql")
	goSrc := repoFile(t, "backend/internal/collect/collect.go")

	// 1) 从迁移里抓 collect_rate_gear 的 VALUES 元组： (idx, delay_ms, label, recover)
	block := gearValuesBlock(t, sh)
	migGears := parseGearTuples(t, block)
	if len(migGears) == 0 {
		t.Fatal("迁移 0005 未解析出任何档位元组 —— 解析失败会让本闸门恒真")
	}

	// 2) 从 Go 源码抓 GearTable 字面量： {0, 500 * time.Millisecond, "...", 5}
	codeGears := parseGoGearTable(t, goSrc)
	if len(codeGears) == 0 {
		t.Fatal("未从 collect.go 解析出 GearTable —— 解析失败会让本闸门恒真")
	}

	// 3) 逐档比对
	if len(migGears) != len(codeGears) {
		t.Fatalf("档位数量不一致：迁移 %d 档 vs 代码 %d 档\n迁移：%v\n代码：%v",
			len(migGears), len(codeGears), migGears, codeGears)
	}
	for i := range migGears {
		if migGears[i] != codeGears[i] {
			t.Errorf("档位 %d 漂移：\n  迁移(DB) = %+v\n  代码      = %+v\n"+
				"两处必须逐字段一致，否则「日志说的档位」与「实际 sleep 的间隔」会对不上。",
				i, migGears[i], codeGears[i])
		}
	}

	// 4) 端点必须覆盖用户要求的 0.5s → 120s
	first, last := migGears[0], migGears[len(migGears)-1]
	if first.delayMs != 500 {
		t.Errorf("首档应为 500ms（0.5 秒），实际 %d ms", first.delayMs)
	}
	if last.delayMs != 120000 {
		t.Errorf("末档应为 120000ms（120 秒封顶），实际 %d ms", last.delayMs)
	}
}

// gearEntry 一个档位的可比较形态。
type gearEntry struct {
	index        int
	delayMs      int
	label        string
	recoverAfter int
}

// gearValuesBlock 取出 `INSERT INTO collect_rate_gear ... VALUES` 的元组区。
func gearValuesBlock(t *testing.T, sql string) string {
	t.Helper()
	i := strings.Index(sql, "INSERT INTO collect_rate_gear")
	if i < 0 {
		t.Fatal("迁移 0005 未找到 INSERT INTO collect_rate_gear")
	}
	rest := sql[i:]
	j := strings.Index(rest, "VALUES")
	if j < 0 {
		t.Fatal("collect_rate_gear 的 INSERT 未找到 VALUES")
	}
	rest = rest[j+len("VALUES"):]
	if k := strings.Index(rest, "ON CONFLICT"); k >= 0 {
		rest = rest[:k]
	}
	return rest
}

// parseGearTuples 解析形如 `(0, 500, '0.5 秒 · 正常', 5),` 的元组。
func parseGearTuples(t *testing.T, block string) []gearEntry {
	t.Helper()
	var out []gearEntry
	for _, raw := range strings.Split(block, "),") {
		s := strings.TrimSpace(raw)
		s = strings.TrimPrefix(s, "(")
		s = strings.TrimSuffix(s, ")")
		if !strings.Contains(s, ",") {
			continue
		}
		parts := splitSimpleCommas(s)
		if len(parts) < 4 {
			continue
		}
		idx := atoiSafe(parts[0])
		delay := atoiSafe(parts[1])
		if idx < 0 || delay <= 0 {
			continue // 非档位行
		}
		label := strings.Trim(strings.TrimSpace(parts[2]), "'")
		rec := atoiSafe(parts[3])
		out = append(out, gearEntry{idx, delay, label, rec})
	}
	return out
}

// splitSimpleCommas 按逗号切分，但跳过单引号字面量内部的逗号。
//
// 为什么需要：档位 label 是中文文本，内容可能含逗号；
// 直接 strings.Split 会把一个元组切碎，解析出错误字段。
func splitSimpleCommas(s string) []string {
	var out []string
	var cur strings.Builder
	inQuote := false
	for _, r := range s {
		switch {
		case r == '\'':
			inQuote = !inQuote
			cur.WriteRune(r)
		case r == ',' && !inQuote:
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// parseGoGearTable 解析 collect.go 里的 GearTable 字面量。
//
// 形如： {0, 500 * time.Millisecond, "0.5 秒 · 正常", 5},
func parseGoGearTable(t *testing.T, src string) []gearEntry {
	t.Helper()
	i := strings.Index(src, "var GearTable = []Gear{")
	if i < 0 {
		t.Fatal("collect.go 未找到 GearTable 定义")
	}
	rest := src[i:]
	if k := strings.Index(rest, "}\n"); k >= 0 {
		rest = rest[:k]
	}
	var out []gearEntry
	for _, line := range strings.Split(rest, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		line = strings.TrimPrefix(line, "{")
		line = strings.TrimSuffix(line, "},")
		line = strings.TrimSuffix(line, "}")
		parts := splitSimpleCommas(line)
		if len(parts) < 4 {
			continue
		}
		idx := atoiSafe(parts[0])
		// 第二字段形如 `500 * time.Millisecond` 或 `1 * time.Second`
		delay := parseGoDurationMs(parts[1])
		if idx < 0 || delay <= 0 {
			continue
		}
		label := strings.Trim(strings.TrimSpace(parts[2]), `"`)
		rec := atoiSafe(parts[3])
		out = append(out, gearEntry{idx, delay, label, rec})
	}
	return out
}

// parseGoDurationMs 把 `500 * time.Millisecond` / `2 * time.Second` 转成毫秒。
func parseGoDurationMs(expr string) int {
	expr = strings.TrimSpace(expr)
	n := atoiSafe(strings.SplitN(expr, "*", 2)[0])
	if n <= 0 {
		return -1
	}
	switch {
	case strings.Contains(expr, "Millisecond"):
		return n
	case strings.Contains(expr, "Second"):
		return n * 1000
	}
	return -1
}

// atoiSafe 宽松取整数前缀；失败返回 -1。
func atoiSafe(s string) int {
	s = strings.TrimSpace(s)
	neg := false
	if strings.HasPrefix(s, "-") {
		neg, s = true, s[1:]
	}
	n := 0
	got := false
	for _, r := range s {
		if r < '0' || r > '9' {
			break
		}
		n = n*10 + int(r-'0')
		got = true
	}
	if !got {
		return -1
	}
	if neg {
		return -n
	}
	return n
}

// TestConfig_CIAndLaunchCheckRunCollectStoreSuite 断言 M-COLLECT 的真库集成套件
// **真的会被 CI 和 launch-check 执行**。
//
// 为什么单列一条闸门：
//   写好了测试却没人跑，与「没写测试」在保护力上是**等价的** ——
//   甚至更糟：它让人误以为已被覆盖。
//   本仓库已有一次同型事故：真库套件因环境变量名分叉而被连环 skip，
//   写好的断言从未真正执行，CI 却全绿（见本文件顶部事故记录）。
//
//   因此凡是新增「真库套件」的地方，都要同时在两处注册：
//     1. `.github/workflows/ci.yml` 的 go 作业（跑 go test 时要带上它）
//     2. `tools/verify/launch-check.sh` 的真库段（本地/部署机可复现）
//   只注册一处，就会出现「CI 绿但本地红」或反之。
func TestConfig_CIAndLaunchCheckRunCollectStoreSuite(t *testing.T) {
	const suite = "./internal/collectstore/"

	yml := repoFile(t, ".github/workflows/ci.yml")
	if !strings.Contains(yml, suite) {
		t.Errorf("CI workflow 未运行 %s 的真库集成套件 —— "+
			"采集临时仓库（页级原子提交 / 断点续传 / 幂等 / 守卫过闸）将在 CI 上**从未被验证**。\n"+
			"这一层专门覆盖「只有真库才暴露」的约束（UTF8 编码 / NULL DEFAULT / 类型严格性）。", suite)
	}
	if !strings.Contains(yml, "-run Integration") {
		t.Error("CI 未以 `-run Integration` 过滤真库套件 —— 会把无库的纯逻辑用例一并跑，掩盖真实覆盖")
	}

	sh := repoFile(t, "tools/verify/launch-check.sh")
	if !strings.Contains(sh, suite) {
		t.Errorf("launch-check 的真库段未运行 %s —— "+
			"部署机上就复现不出 CI 的结论，「退出码可信」这条前提被破坏。", suite)
	}
}

// TestParseGearHelpers 自测上面的解析器。
//
// 「解析器写错时会给出错误答案而不报错」—— 这比崩溃危险得多：
// 一个恒返回空的解析器会让闸门**永远通过**。故必须单独自测。
func TestParseGearHelpers(t *testing.T) {
	t.Run("parseGoDurationMs", func(t *testing.T) {
		cases := map[string]int{
			"500 * time.Millisecond": 500,
			"120 * time.Second":      120000,
			"1 * time.Second":        1000,
			"garbage":                -1,
			"0 * time.Second":        -1,
		}
		for in, want := range cases {
			if got := parseGoDurationMs(in); got != want {
				t.Errorf("parseGoDurationMs(%q) = %d，期望 %d", in, got, want)
			}
		}
	})

	t.Run("splitSimpleCommas 不切引号内逗号", func(t *testing.T) {
		got := splitSimpleCommas("0, 500, 'a, b', 5")
		if len(got) != 4 {
			t.Fatalf("应切成 4 段（引号内的逗号不算），实际 %d 段：%v", len(got), got)
		}
		if strings.TrimSpace(got[2]) != "'a, b'" {
			t.Errorf("第 3 段应为 'a, b'，实际 %q", got[2])
		}
	})

	t.Run("atoiSafe", func(t *testing.T) {
		for in, want := range map[string]int{"12": 12, " 7": 7, "-3": -3, "x": -1, "": -1} {
			if got := atoiSafe(in); got != want {
				t.Errorf("atoiSafe(%q) = %d，期望 %d", in, got, want)
			}
		}
	})

	t.Run("parseGoGearTable 能真的解析出档位", func(t *testing.T) {
		src := `var GearTable = []Gear{
	{0, 500 * time.Millisecond, "正常", 5},
	{1, 1 * time.Second, "慢", 5},
}
`
		got := parseGoGearTable(t, src)
		if len(got) != 2 {
			t.Fatalf("应解析出 2 档，实际 %d：%v", len(got), got)
		}
		if got[1].delayMs != 1000 {
			t.Errorf("第 2 档应为 1000ms，实际 %d", got[1].delayMs)
		}
	})
}
