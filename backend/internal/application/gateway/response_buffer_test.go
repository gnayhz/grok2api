package gateway

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/pkg/responsebuffer"
)

func TestDeferredJSONBorrowsAdmissionBufferAcrossOwnership(t *testing.T) {
	pool := responsebuffer.NewPool(1 << 20)
	budget := pool.Request(1 << 20)
	raw := `{"output":[{"type":"reasoning","summary":[{"text":"plan"}]}]}`
	buf := responsebuffer.New(budget, 1<<20)
	_, _ = buf.Write([]byte(raw))
	body := buf.Body()
	original, release, _ := body.BorrowBytes()
	_, owner := newAttemptResources(context.Background())
	response := &provider.Response{Body: owner.own(body), ConvertJSON: func(data []byte) ([]byte, error) {
		if &data[0] != &original[0] {
			t.Fatal("conversion copied/re-read the admission buffer")
		}
		if budget.Snapshot().Used == 0 {
			t.Fatal("borrow released during conversion")
		}
		return []byte(`{"choices":[]}`), nil
	}}
	if err := applyDeferredStreamConversion(response); err != nil {
		t.Fatal(err)
	}
	owner.close()
	release()
	data, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil || string(data) != `{"choices":[]}` {
		t.Fatalf("converted=%q err=%v", data, err)
	}
	if got := pool.Snapshot().Used; got != 0 {
		t.Fatalf("remaining reservation=%d", got)
	}
}

func TestResourceExhaustionRejectsWithoutAccountRotation(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		pool := responsebuffer.NewPool(1)
		budget := pool.Request(1)
		adapter := &scriptedBuildAdapter{responses: map[uint64][]scriptedBuildResponse{}}
		service, credentials := newGuardLoopService(t, adapter, "budget-a", "budget-b")
		for _, credential := range credentials {
			adapter.responses[credential.ID] = []scriptedBuildResponse{{status: http.StatusOK, body: `{"output":[{"type":"reasoning","summary":[{"text":"plan"}]}]}`}}
		}
		result, err := service.CreateChatCompletion(responsebuffer.WithContext(context.Background(), budget), guardLoopInput("response-budget", streaming))
		if result != nil {
			_ = result.Body.Close()
			t.Fatal("exhausted request delivered")
		}
		var failure *UpstreamFailure
		if !errors.As(err, &failure) || failure.Code != "response_resource_exhausted" || failure.HTTPStatus != http.StatusServiceUnavailable {
			t.Fatalf("streaming=%v: %v", streaming, err)
		}
		if len(adapter.responses[credentials[1].ID]) != 1 {
			t.Fatal("local exhaustion generated another upstream attempt")
		}
		if pool.Snapshot().Used != 0 {
			t.Fatal("exhausted request retained a reservation")
		}
	}
}

func TestStreamPrefixReleasesReservationOnReadAndAbort(t *testing.T) {
	for _, payload := range []string{
		"data: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"plan\"}\n\nrest",
		"data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"reasoning\"}}\n\n",
	} {
		pool := responsebuffer.NewPool(1 << 20)
		ctx := responsebuffer.WithContext(context.Background(), pool.Request(1<<20))
		body, _, _, err := peekQualityStream(ctx, io.NopCloser(strings.NewReader(payload)), qualityProtocolResponses, QualityRetryRuntime{})
		if err != nil {
			t.Fatal(err)
		}
		out, err := io.ReadAll(body)
		_ = body.Close()
		if err != nil || !bytes.Equal(out, []byte(payload)) {
			t.Fatalf("replay changed bytes: %q err=%v", out, err)
		}
		if pool.Snapshot().Used != 0 {
			t.Fatal("prefix retained after close")
		}
	}
}

func TestProviderResourceFailureDoesNotRotateAccount(t *testing.T) {
	for _, cause := range []error{responsebuffer.ErrExhausted, responsebuffer.ErrLimit} {
		failure := newTransportUpstreamFailure(cause, 1, "account")
		if failure.Code != "response_resource_exhausted" && failure.Code != "response_too_large" {
			t.Fatalf("misclassified: %s", failure.Code)
		}
		for _, kind := range []accountdomain.Provider{accountdomain.ProviderBuild, accountdomain.ProviderWeb, accountdomain.ProviderConsole} {
			if isRetryableTransportFailure(kind, cause) {
				t.Fatalf("%s retries local resource failure", kind)
			}
		}
	}
}
