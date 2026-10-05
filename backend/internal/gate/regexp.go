package gate

import "regexp"

// credRe 匹配 URL 内的凭据形态 `scheme://user[:pass]@host`（G11）。
//
// 刻意保持保守：要求「scheme://」前缀 + 「@」，避免把普通邮箱误判。
// 例如命中：https://user:pw@example.com/api
// 不命中：mailto:a@b.com（无 //）、user@example.com（无 scheme://）
var credRe = regexp.MustCompile(`(?i)[a-z][a-z0-9+.\-]*://[^/@\s:]+:[^/@\s]*@[^/\s]+`)

// urlCredRe 供测试导出（保持 credRe 不导出，避免外部依赖内部实现）。
func URLHasCredentials(raw string) bool {
	return credRe.MatchString(raw)
}
