package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setupWebDir 造一个最小前端产物目录。
func setupWebDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"),
		[]byte("<!doctype html><div id=app></div>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "assets", "app.js"),
		[]byte("console.log(1)"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestStatic_ServesIndexAtRoot(t *testing.T) {
	h := staticHandler(setupWebDir(t))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("GET / => %d, 期望 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "id=app") {
		t.Fatalf("body 未包含 index 内容: %q", rec.Body.String())
	}
}

func TestStatic_ServesAsset(t *testing.T) {
	h := staticHandler(setupWebDir(t))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/assets/app.js", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /assets/app.js => %d, 期望 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "console.log") {
		t.Fatalf("未返回资源内容: %q", rec.Body.String())
	}
}

// SPA fallback：未知路径应回 index.html，而不是 404（前端路由需要）。
func TestStatic_SPAFallback(t *testing.T) {
	h := staticHandler(setupWebDir(t))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/report/pnl/month", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("SPA fallback => %d, 期望 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "id=app") {
		t.Fatal("SPA fallback 未返回 index.html")
	}
}

// ★ 关键：未挂载产物必须**显式 503**，不能静默 404。
// 静默 404 会误导为「页面写错」，真相是「没挂载」。
func TestStatic_NotMountedFailsLoudly(t *testing.T) {
	cases := []struct {
		name string
		dir  string
	}{
		{"空目录配置", ""},
		{"目录不存在", filepath.Join(t.TempDir(), "nope")},
		{"目录存在但无 index.html", t.TempDir()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := staticHandler(tc.dir)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("未挂载产物 => %d, 期望 503（显式降级）", rec.Code)
			}
			if !strings.Contains(rec.Body.String(), "前端不可用") {
				t.Fatalf("503 正文应说明原因，实际: %q", rec.Body.String())
			}
		})
	}
}

// 目录列举必须被拒绝（避免产物文件名被枚举）。
func TestStatic_NoDirectoryListing(t *testing.T) {
	dir := setupWebDir(t)
	if err := os.WriteFile(filepath.Join(dir, "secret-note.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := staticHandler(dir)

	// /assets/ 是真实目录：应 fallback 到 index，而不是列举出 app.js。
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/assets/", nil))
	if strings.Contains(rec.Body.String(), "app.js") {
		t.Fatalf("目录列举未被阻止: %q", rec.Body.String())
	}
}

// 路径穿越必须被拒。
func TestStatic_RejectsPathTraversal(t *testing.T) {
	h := staticHandler(setupWebDir(t))
	for _, p := range []string{"/../etc/passwd", "/assets/../../x"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code == http.StatusOK {
			t.Fatalf("路径穿越 %q 未被拒绝（%d）", p, rec.Code)
		}
	}
}

// 非 GET/HEAD 方法应 405。
func TestStatic_RejectsNonReadMethods(t *testing.T) {
	h := staticHandler(setupWebDir(t))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST / => %d, 期望 405", rec.Code)
	}
}

// API 路径不交给静态托管。
func TestIsAPIPath(t *testing.T) {
	yes := []string{"/api/query", "/api/admin/slots", "/healthz"}
	no := []string{"/", "/report", "/assets/app.js"}
	for _, p := range yes {
		if !isAPIPath(p) {
			t.Errorf("isAPIPath(%q) 应为 true", p)
		}
	}
	for _, p := range no {
		if isAPIPath(p) {
			t.Errorf("isAPIPath(%q) 应为 false", p)
		}
	}
}

// index.html 禁用缓存，避免发布后仍加载旧壳。
func TestStatic_IndexNoCache(t *testing.T) {
	h := staticHandler(setupWebDir(t))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "no-cache") {
		t.Fatalf("index Cache-Control = %q, 期望含 no-cache", cc)
	}
}
