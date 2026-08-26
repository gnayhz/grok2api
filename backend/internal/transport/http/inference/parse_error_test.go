package inference

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// round 64 回归：非法 JSON 必须得到明确的解析错误消息，而不是误导性的
// 「缺少有效 model」——三者曾在同一条件里合并，客户端会去检查 model
// 字段而真正的问题是 JSON 语法。
func TestMalformedJSONReturnsParseErrorNotMissingModel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	NewHandler(nil, nil, 1<<20).Register(router.Group("/v1"))

	cases := []struct {
		name string
		path string
	}{
		{name: "chat completions", path: "/v1/chat/completions"},
		{name: "responses", path: "/v1/responses"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, test.path, strings.NewReader("{invalid json"))
			request.Header.Set("Content-Type", "application/json")
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", recorder.Code)
			}
			body := recorder.Body.String()
			if strings.Contains(body, "缺少有效 model") {
				t.Fatalf("malformed JSON must not be reported as missing model: %s", body)
			}
			if !strings.Contains(body, "不是有效的 JSON") {
				t.Fatalf("missing parse-error message: %s", body)
			}
		})
	}

	t.Run("messages", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{invalid json"))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("anthropic-version", "2023-06-01")
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", recorder.Code)
		}
		body := recorder.Body.String()
		if strings.Contains(body, "are required") {
			t.Fatalf("malformed JSON must not be reported as missing fields: %s", body)
		}
		if !strings.Contains(body, "not valid JSON") {
			t.Fatalf("missing parse-error message: %s", body)
		}
	})
}
