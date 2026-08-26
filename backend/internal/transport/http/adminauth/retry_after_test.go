package adminauth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	adminapp "github.com/chenyme/grok2api/backend/internal/application/adminauth"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/gin-gonic/gin"
)

// 登录限流 429 必须带 Retry-After（round 55 修复：此前限流器算出的
// 固定窗口剩余时间在 service 层被丢弃，锁定客户端无从得知需等待多久）。
func TestLoginRateLimitedResponseIncludesRetryAfter(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	service := adminapp.NewService(
		relational.NewAdminRepository(database),
		relational.NewAdminSessionRepository(database),
		security.NewTokenService("12345678901234567890123456789012"),
		15*time.Minute,
		30*24*time.Hour,
	)
	if err := service.Bootstrap(ctx, "admin", "password123"); err != nil {
		t.Fatal(err)
	}
	service.SetLoginRateLimiter(memory.NewRateLimiter())

	router := gin.New()
	router.POST("/auth/login", func(c *gin.Context) {
		var request struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		_ = c.ShouldBindJSON(&request)
		_, _, err := service.Login(c.Request.Context(), request.Username, request.Password, "127.0.0.1")
		if err != nil {
			var limited *adminapp.LoginRateLimitedError
			if errors.As(err, &limited) && limited.RetryAfter > 0 {
				seconds := max(int64(1), int64((limited.RetryAfter+time.Second-1)/time.Second))
				c.Header("Retry-After", strconv.FormatInt(seconds, 10))
			}
			if errors.Is(err, adminapp.ErrLoginRateLimited) {
				c.JSON(http.StatusTooManyRequests, gin.H{"error": gin.H{"code": "loginRateLimited"}})
				return
			}
			c.JSON(http.StatusUnauthorized, gin.H{"error": gin.H{"code": "invalidCredentials"}})
			return
		}
		c.JSON(http.StatusOK, gin.H{})
	})

	// user limit = 12: 前 12 次凭据错误, 第 13 次必须 429 + Retry-After。
	for i := 0; i < 12; i++ {
		request := httptest.NewRequest(http.MethodPost, "/auth/login", strings.NewReader(`{"username":"admin","password":"wrong"}`))
		request.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d status = %d, want 401", i+1, recorder.Code)
		}
		if retry := recorder.Header().Get("Retry-After"); retry != "" {
			t.Fatalf("attempt %d Retry-After = %q, want empty before limiting", i+1, retry)
		}
	}
	request := httptest.NewRequest(http.MethodPost, "/auth/login", strings.NewReader(`{"username":"admin","password":"wrong"}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("attempt 13 status = %d, want 429", recorder.Code)
	}
	retry := recorder.Header().Get("Retry-After")
	if retry == "" {
		t.Fatal("attempt 13 Retry-After missing")
	}
	if parsed, err := strconv.Atoi(retry); err != nil || parsed < 1 || parsed > 60 {
		t.Fatalf("Retry-After = %q (err=%v), want 1..60 seconds", retry, err)
	}
}
