package httpserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 路由级 404/405 契约：此前落到 gin 默认裸文本「404 page not found」且
// 错误方法也返回 404——API 消费方无法区分两类错误，管理面错误无
// requestId 可关联日志。修复后：管理面走 response.Error 信封（含
// requestId），/v1 走 OpenAI 兼容信封，SPA 回退行为不变。
func TestRouteErrorsUseConsistentEnvelopes(t *testing.T) {
	deps := testDependencies()
	router := New(deps) // 无 FrontendStaticPath → 纯后端部署分支

	t.Run("admin 404 uses envelope with requestId", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/admin/v1/nonexistent", nil))
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("status = %d", recorder.Code)
		}
		var payload struct {
			Error struct {
				Code      string `json:"code"`
				RequestID string `json:"requestId"`
			} `json:"error"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
			t.Fatalf("body must be the admin error envelope, got %q: %v", recorder.Body.String(), err)
		}
		if payload.Error.Code != "notFound" || payload.Error.RequestID == "" {
			t.Fatalf("code=%q requestId=%q", payload.Error.Code, payload.Error.RequestID)
		}
	})

	t.Run("v1 404 uses OpenAI envelope", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/nonexistent", nil))
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("status = %d", recorder.Code)
		}
		body := recorder.Body.String()
		if !strings.Contains(body, "invalid_request_error") || !strings.Contains(body, "not_found") {
			t.Fatalf("body must be the OpenAI envelope, got %q", body)
		}
	})

	t.Run("wrong method is 405 not 404", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodDelete, "/healthz", nil))
		if recorder.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want 405", recorder.Code)
		}
		recorder2 := httptest.NewRecorder()
		router.ServeHTTP(recorder2, httptest.NewRequest(http.MethodGet, "/v1/chat/completions", nil))
		if recorder2.Code != http.StatusMethodNotAllowed {
			t.Fatalf("v1 status = %d, want 405", recorder2.Code)
		}
		if body := recorder2.Body.String(); !strings.Contains(body, "method_not_allowed") {
			t.Fatalf("v1 405 body = %q", body)
		}
	})

	t.Run("non-backend path stays plain 404 without dist", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/some/spa/path", nil))
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("status = %d", recorder.Code)
		}
		if strings.Contains(recorder.Body.String(), "requestId") {
			t.Fatalf("non-backend path should not carry the admin envelope: %q", recorder.Body.String())
		}
	})
}

// SPA 回退语义在信封化后必须原样保留：dist 存在时后端路径仍走信封、
// SPA 深链仍返回 index.html、扩展名路径 404。
func TestSPAFallbackUnchangedWithRouteErrors(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "index.html"), []byte("<html>app</html>"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "assets", "app.js"), []byte("console.log(1)"), 0o600); err != nil {
		t.Fatal(err)
	}
	deps := testDependencies()
	deps.FrontendStaticPath = root
	router := New(deps)

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/dashboard", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "<html>") {
		t.Fatalf("SPA deep link must still serve index.html: code=%d body=%.80s", recorder.Code, recorder.Body.String())
	}

	adminRecorder := httptest.NewRecorder()
	router.ServeHTTP(adminRecorder, httptest.NewRequest(http.MethodGet, "/api/admin/v1/missing", nil))
	if adminRecorder.Code != http.StatusNotFound || !strings.Contains(adminRecorder.Body.String(), "notFound") {
		t.Fatalf("backend path behind dist must use envelope: code=%d body=%q", adminRecorder.Code, adminRecorder.Body.String())
	}

	assetRecorder := httptest.NewRecorder()
	router.ServeHTTP(assetRecorder, httptest.NewRequest(http.MethodGet, "/assets/missing.js", nil))
	if assetRecorder.Code != http.StatusNotFound {
		t.Fatalf("missing asset status = %d", assetRecorder.Code)
	}
}
