// static.go —— 前端静态托管（把 web/dist 交付到浏览器）。
//
// 动机：deploy/Dockerfile.sparkd 已把前端产物 COPY 到 /opt/spark/web 并设
// SPARK_WEB_DIR，但 sparkd 此前没有静态文件服务 —— 前端产物是**死文件**，
// 单进程交付不了整个平台（「可上线」的硬缺口）。
//
// 纪律：
//   * 未配置 / 目录不存在 ⇒ 显式降级（返回 503 + 说明），**不静默 404**。
//     静默 404 会让人以为「页面写错了」，而真相是「没挂载产物」。
//   * 不做目录列举（避免产物文件名被枚举）。
//   * 不注入任何凭据；身份由网关/请求头进入 /api/*。
//   * SPA fallback：非 /api 且非静态文件命中 ⇒ 回 index.html（前端路由用）。
package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// staticHandler 返回托管 dir 的处理器。
//
// dir 为空或不存在时返回一个**显式报错**的处理器，而不是 nil ——
// 让「没配静态目录」这件事在浏览器上可见（而不是 404 迷雾）。
func staticHandler(dir string) http.Handler {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return notMountedHandler("未配置前端产物目录（SPARK_WEB_DIR 为空）")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return notMountedHandler("前端产物目录解析失败: " + err.Error())
	}
	idx := filepath.Join(abs, "index.html")
	if st, err := os.Stat(idx); err != nil || st.IsDir() {
		return notMountedHandler(fmt.Sprintf("前端产物未挂载：%s 下找不到 index.html（请先构建 web/ 或检查镜像 COPY）", abs))
	}
	log.Printf("static hosting: %s", abs)

	fs := http.FileServer(http.Dir(abs))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// 清理路径，拒绝穿越（含编码后的 %2e%2e 已由 net/http 解码）。
		upath := path.Clean("/" + r.URL.Path)
		if strings.Contains(upath, "..") {
			http.Error(w, "bad path", http.StatusBadRequest)
			return
		}

		// 目录列举一律拒绝（FileServer 遇目录会列举）。
		if upath == "/" {
			serveIndex(w, r, abs)
			return
		}

		target := filepath.Join(abs, filepath.FromSlash(strings.TrimPrefix(upath, "/")))
		if st, err := os.Stat(target); err == nil && !st.IsDir() {
			// 缓存策略：带内容哈希的产物长缓存，其余短缓存。
			if strings.HasPrefix(upath, "/assets/") || strings.Contains(filepath.Base(upath), ".") {
				w.Header().Set("Cache-Control", "public, max-age=3600")
			}
			fs.ServeHTTP(w, r)
			return
		}

		// SPA fallback：未命中静态文件 ⇒ 交给前端路由。
		serveIndex(w, r, abs)
	})
}

// serveIndex 直出 index.html（禁用缓存，避免发布后仍加载旧壳）。
func serveIndex(w http.ResponseWriter, r *http.Request, abs string) {
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeFile(w, r, filepath.Join(abs, "index.html"))
}

// notMountedHandler 显式说明静态目录未挂载（降级可观测）。
func notMountedHandler(reason string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprintf(w, "spark-cicada: 前端不可用\n\n%s\n\n"+
			"提示：构建前端后设置 SPARK_WEB_DIR 指向其 dist 目录。\n"+
			"      API 不受影响：/api/*、/healthz 仍可用。\n", reason)
	})
}

// isAPIPath 判断是否属于 API/健康检查前缀（不交给静态托管）。
func isAPIPath(p string) bool {
	return strings.HasPrefix(p, "/api/") || p == "/healthz"
}
