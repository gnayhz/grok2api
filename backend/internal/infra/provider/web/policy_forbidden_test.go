package web

import (
	"net/http"
	"testing"
)

// TestWebMediaUpstreamErrorPolicyForbidden 锁定 round 56：403+JSON body 是
// 源站策略级拒绝（IsPolicyForbidden=true）；403+html/empty（浏览器会话层）
// 与非 403 均不是。视频循环据此短路换号轮询。
func TestWebMediaUpstreamErrorPolicyForbidden(t *testing.T) {
	json403 := newWebMediaUpstreamError(http.StatusForbidden, []byte(`{"error":{"code":7,"message":"This page is out of date"}}`), false)
	if !json403.IsPolicyForbidden() {
		t.Fatal("403+json 应为策略级拒绝")
	}
	html403 := newWebMediaUpstreamError(http.StatusForbidden, []byte("<html>challenge</html>"), false)
	if html403.IsPolicyForbidden() {
		t.Fatal("403+html 是会话层，不是策略级")
	}
	empty403 := newWebMediaUpstreamError(http.StatusForbidden, nil, false)
	if empty403.IsPolicyForbidden() {
		t.Fatal("403+empty 是会话层，不是策略级")
	}
	json429 := newWebMediaUpstreamError(http.StatusTooManyRequests, []byte("{}"), false)
	if json429.IsPolicyForbidden() {
		t.Fatal("429 不是 403")
	}
}
