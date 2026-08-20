package gateway

import (
	"context"
	"io"
	"sync"
	"testing"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
)

// recordingAttributor records OnDegraded calls for assertions.
type recordingAttributor struct {
	mu       sync.Mutex
	degraded []uint64
}

func (r *recordingAttributor) OnDegraded(_ context.Context, credential accountdomain.Credential) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.degraded = append(r.degraded, credential.ID)
}

func (r *recordingAttributor) calls() []uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]uint64(nil), r.degraded...)
}

// TestEmptyStreamTriggersAttribution：空流同样走 RSC 归因——上游既有逻辑只
// 打 24h 冷却不归因，无辜（IP 嫌疑）账号只能干等；归因后 clean 结论可自动
// 解除冷却。B 形态→同号重试→空流 的序列也不得丢失归因。
func TestEmptyStreamTriggersAttribution(t *testing.T) {
	t.Parallel()
	fixture := newSameAccountFixture(t)
	// 账号 0：先 B 形态（触发同号重试），重试恰为空流。
	fixture.scriptAccount(0, bFormStream())
	fixture.scriptAccount(0, "")
	fixture.scriptAccount(1, aFormStream())

	attributor := &recordingAttributor{}
	service := fixture.service(t, baseSameAccountRuntime())
	service.UpdateAccountRisk(attributor)

	result, err := service.CreateChatCompletion(context.Background(), fixture.request())
	if err != nil {
		t.Fatalf("request should deliver from account 1, err=%v", err)
	}
	body, _ := io.ReadAll(result.Body)
	_ = result.Body.Close()
	if !contains(body, "good answer") {
		t.Fatal("delivered body should come from the second account")
	}

	calls := attributor.calls()
	if len(calls) == 0 || calls[0] != fixture.credentials[0].ID {
		t.Fatalf("empty-stream account %d must be attributed for RSC, calls=%v", fixture.credentials[0].ID, calls)
	}
}

func contains(haystack []byte, needle string) bool {
	return len(needle) == 0 || (len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0)
}

func indexOf(haystack []byte, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		match := true
		for j := 0; j < len(needle); j++ {
			if haystack[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}
