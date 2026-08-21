package middleware

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestWriteOpenAIErrorTypeAlignment 锁定 round 24 修复：中间件的 OpenAI
// 错误信封 type 与 inference handler 同口径——401=authentication_error、
// 429=rate_limit_error、5xx=server_error、其余 invalid_request_error。
// 此前硬编码 invalid_request_error（活体矩阵发现 401 漂移）。
func TestWriteOpenAIErrorTypeAlignment(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cases := []struct {
		status int
		want   string
	}{
		{http.StatusUnauthorized, "authentication_error"},
		{http.StatusTooManyRequests, "rate_limit_error"},
		{http.StatusServiceUnavailable, "server_error"},
		{http.StatusBadRequest, "invalid_request_error"},
	}
	for _, tc := range cases {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		writeOpenAIError(ctx, tc.status, "some_code", "msg")
		var payload struct {
			Error struct {
				Type string `json:"type"`
			} `json:"error"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
			t.Fatalf("status %d: %v", tc.status, err)
		}
		if payload.Error.Type != tc.want {
			t.Fatalf("status %d type = %q, want %q", tc.status, payload.Error.Type, tc.want)
		}
		if recorder.Code != tc.status {
			t.Fatalf("status %d: body status %d", tc.status, recorder.Code)
		}
	}
}
