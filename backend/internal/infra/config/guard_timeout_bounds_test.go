package config

import (
	"strings"
	"testing"
	"time"
)

// TestValidateRequestRetryEvidenceTimeout 边界锁定：与 config.example.yaml
// 文档声明的范围（0=默认 3.5s；非零须在 1s-5m）及验证逻辑逐条对齐。
// 零延迟拦截落地后证据截止仅是防死锁兜底，下限从 3s 放宽到 1s。
func TestValidateRequestRetryEvidenceTimeout(t *testing.T) {
	t.Parallel()
	base := func(d time.Duration) RequestRetryConfig {
		return RequestRetryConfig{Enabled: true, EvidenceTimeout: Duration(d)}
	}
	for _, invalid := range []time.Duration{999 * time.Millisecond, 5*time.Minute + time.Second} {
		if err := validateRequestRetry(base(invalid)); err == nil || !strings.Contains(err.Error(), "evidenceTimeout") {
			t.Fatalf("evidence timeout %v should be rejected, got %v", invalid, err)
		}
	}
	for _, valid := range []time.Duration{0, time.Second, 3500 * time.Millisecond, 5 * time.Minute} {
		if err := validateRequestRetry(base(valid)); err != nil {
			t.Fatalf("evidence timeout %v should be accepted, got %v", valid, err)
		}
	}
}

// TestValidateRequestRetryCreatedTimeout 边界锁定：文档声明（0=默认 5s；
// 非零须在 1s-2m）与 config.go L754 的验证对齐。首事件截止是降智流式
// 尝试的第一道预算，边界漂移会直接改变拦截时序。
func TestValidateRequestRetryCreatedTimeout(t *testing.T) {
	t.Parallel()
	base := func(d time.Duration) RequestRetryConfig {
		return RequestRetryConfig{Enabled: true, CreatedTimeout: Duration(d)}
	}
	for _, invalid := range []time.Duration{999 * time.Millisecond, 2*time.Minute + time.Second} {
		if err := validateRequestRetry(base(invalid)); err == nil || !strings.Contains(err.Error(), "createdTimeout") {
			t.Fatalf("created timeout %v should be rejected, got %v", invalid, err)
		}
	}
	for _, valid := range []time.Duration{0, time.Second, 5 * time.Second, 2 * time.Minute} {
		if err := validateRequestRetry(base(valid)); err != nil {
			t.Fatalf("created timeout %v should be accepted, got %v", valid, err)
		}
	}
}
