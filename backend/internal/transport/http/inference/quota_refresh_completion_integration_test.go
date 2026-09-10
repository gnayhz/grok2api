package inference

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/console"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestQuotaSnapshotDoesNotOverwriteGeneration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	started, release := make(chan struct{}), make(chan struct{})
	var calls, queries, generated atomic.Int32
	upstream := voiceCompletionUpstream(t, &calls, func(w fhttp.ResponseWriter, r *fhttp.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/usage") {
			remaining := 20 - int(generated.Load())
			if queries.Add(1) == 1 {
				close(started)
				select {
				case <-release:
				case <-ctx.Done():
					return
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"quotas": []map[string]any{
				{"kind": "chat", "limit": 20, "remaining": 20},
				{"kind": "image", "limit": 20, "remaining": remaining, "used": 20 - remaining},
				{"kind": "video", "limit": 20, "remaining": 20},
			}})
			return
		}
		generated.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{"b64_json": completionImagePNG}}})
	})
	defer upstream.Close()
	fx := newVoiceCompletionFixture(t, upstream.URL, "grok-imagine-image")
	if err := saveQuotaWindowsFixture(fx.accounts, ctx, fx.account.ID, "", time.Now().UTC(), []account.QuotaWindow{{Mode: console.QuotaModeImage, Remaining: 20, Total: 20}}); err != nil {
		t.Fatal(err)
	}
	refreshed := make(chan error, 1)
	go func() {
		_, err := fx.accountService.RefreshQuotaMode(ctx, fx.account.ID, console.QuotaModeImage)
		refreshed <- err
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("quota query did not start")
	}
	payload, _ := json.Marshal(map[string]any{"model": fx.publicModel, "prompt": "synthetic", "response_format": "b64_json"})
	request := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(string(payload))).WithContext(ctx)
	request.Header.Set("Authorization", "Bearer "+fx.created.Secret)
	request.Header.Set("Content-Type", "application/json")
	out := httptest.NewRecorder()
	fx.router.ServeHTTP(out, request)
	if out.Code != http.StatusOK {
		t.Fatalf("generation: %d %s", out.Code, out.Body.String())
	}
	assertRemaining := func(want int) {
		t.Helper()
		windows, err := fx.accounts.GetQuotaWindows(ctx, []uint64{fx.account.ID})
		if err != nil {
			t.Fatal(err)
		}
		for _, window := range windows[fx.account.ID] {
			if window.Mode == console.QuotaModeImage {
				if window.Remaining != want {
					t.Errorf("quota remaining=%d want=%d", window.Remaining, want)
				}
				return
			}
		}
		t.Fatal("image window missing")
	}
	assertRemaining(19)
	close(release)
	select {
	case err := <-refreshed:
		if !errors.Is(err, repository.ErrConflict) {
			t.Errorf("in-flight pre-generation query should conflict: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("quota query did not finish")
	}
	assertRemaining(19)
	if _, err := fx.accountService.RefreshQuotaMode(ctx, fx.account.ID, console.QuotaModeImage); err != nil {
		t.Fatal(err)
	}
	assertRemaining(19)
	record := waitVoiceAudit(t, fx.audits)
	if generated.Load() != 1 || queries.Load() != 2 || record.GenerationOutcome != "completed" || record.MediaOutputImages != 1 {
		t.Fatalf("generation=%d queries=%d audit=%+v", generated.Load(), queries.Load(), record)
	}
}
