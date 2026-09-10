package inference

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/chenyme/grok2api/backend/internal/application/gateway"
	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
	"github.com/gin-gonic/gin"
)

func TestHistoryBarrierCommitBeforeSuccess(t *testing.T) {
	tests := []struct {
		name            string
		protocol        streamProtocol
		delta, terminal string
	}{
		{"responses", streamProtocolResponses, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n", "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"hello\"}]}]}}\n\n"},
		{"chat", streamProtocolChat, "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"},\"finish_reason\":null}]}\n\n", "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"},
		{"messages", streamProtocolAnthropic, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n", "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"},
	}
	for _, tt := range tests {
		for _, fail := range []bool{false, true} {
			t.Run(tt.name+map[bool]string{false: "/success", true: "/failure"}[fail], func(t *testing.T) {
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				called := false
				_, err := copyStreamWithCompletion(c.Writer, iotest.OneByteReader(strings.NewReader(tt.delta+tt.terminal)), tt.protocol, nil, "grok-4.6", func(_ responseMetadata) error {
					called = true
					if !strings.Contains(rec.Body.String(), "hello") {
						t.Error("delta was not forwarded before commit")
					}
					if completionSuccessFrame(rec.Body.Bytes()) {
						t.Error("success exposed before commit")
					}
					if fail {
						return errors.New("disk unavailable")
					}
					return nil
				})
				if !called {
					t.Fatalf("commit was not called: %v body=%s", err, rec.Body.String())
				}
				if fail {
					if !errors.Is(err, inferencedomain.ErrCompletionCommit) {
						t.Fatalf("error=%v", err)
					}
					if strings.Contains(rec.Body.String(), "[DONE]") || strings.Contains(rec.Body.String(), "message_stop") || strings.Contains(rec.Body.String(), "response.completed") {
						t.Fatalf("success leaked: %s", rec.Body.String())
					}
				} else if err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}
func TestHistoryJSONBarrier(t *testing.T) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	_, err := copyJSONWithCompletion(c.Writer, strings.NewReader(`{"id":"r","output":[]}`), streamProtocolResponses, func(_ responseMetadata) error {
		if rec.Body.Len() != 0 {
			t.Fatal("JSON delivered before commit")
		}
		return errors.New("disk unavailable")
	})
	if !errors.Is(err, inferencedomain.ErrCompletionCommit) || rec.Body.Len() != 0 {
		t.Fatalf("err=%v body=%s", err, rec.Body.String())
	}
}

func TestHistoryBarrierReadFailureNeverCommits(t *testing.T) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	called := false
	stream := io.MultiReader(strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"answer\"}}]}\n\ndata: [DONE]\n\n"), historyReadFailure{})
	_, err := copyStreamWithCompletion(c.Writer, stream, streamProtocolChat, nil, "", func(responseMetadata) error { called = true; return nil })
	if err == nil || called || strings.Contains(rec.Body.String(), "[DONE]") {
		t.Fatalf("error=%v called=%v body=%s", err, called, rec.Body.String())
	}
}

type historyReadFailure struct{}

func (historyReadFailure) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestHistoryJSONReadFailureReturnsGatewayError(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	called := false
	result := &gateway.Result{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(io.MultiReader(strings.NewReader(`{"id":"partial"`), historyReadFailure{})), CommitCompletion: func(gateway.Completion) error { called = true; return nil }, Finalize: func(gateway.Usage, string, string) {}}
	(&Handler{}).writeProtocolResult(c, result, false, false, streamProtocolResponses, "")
	if called || recorder.Code != http.StatusBadGateway || !strings.Contains(recorder.Body.String(), "error") {
		t.Fatalf("called=%v status=%d body=%s", called, recorder.Code, recorder.Body.String())
	}
}

type completionPartialWriter struct {
	gin.ResponseWriter
	limit    int
	writeErr error
	flushErr error
}

func (w *completionPartialWriter) Write(p []byte) (int, error) {
	if w.limit < len(p) {
		n, _ := w.ResponseWriter.Write(p[:w.limit])
		return n, w.writeErr
	}
	return w.ResponseWriter.Write(p)
}
func (w *completionPartialWriter) FlushError() error { return w.flushErr }

func TestCompletionJSONRetainsUsageOnCommitOrWriteFailure(t *testing.T) {
	for _, stage := range []string{"commit", "short_write", "write_error"} {
		t.Run(stage, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			writer := &completionPartialWriter{ResponseWriter: c.Writer, limit: 13}
			if stage == "write_error" {
				writer.writeErr = io.ErrClosedPipe
			}
			called := 0
			meta, err := copyJSONWithCompletion(writer, strings.NewReader(`{"id":"r_usage","status":"completed","usage":{"input_tokens":20,"output_tokens":5,"total_tokens":25},"output":[{"type":"message"}]}`), streamProtocolResponses, func(responseMetadata) error {
				called++
				if recorder.Body.Len() != 0 {
					t.Error("body exposed before necessary commit")
				}
				if stage == "commit" {
					return inferencedomain.ErrResponseOwnershipCommit
				}
				return nil
			})
			if err == nil || called != 1 || meta.ResponseID != "r_usage" || meta.Usage.InputTokens != 20 || meta.Usage.OutputTokens != 5 {
				t.Fatalf("stage=%s meta=%+v err=%v calls=%d", stage, meta, err, called)
			}
			if stage == "commit" {
				if meta.DeliveredBytes != 0 || recorder.Body.Len() != 0 || !errors.Is(err, inferencedomain.ErrResponseOwnershipCommit) {
					t.Fatalf("commit failure leaked output: %+v %v", meta, err)
				}
			} else if meta.DeliveredBytes != 13 || recorder.Body.Len() != 13 || !errors.Is(err, errClientStreamWrite) {
				t.Fatalf("partial write accounting: %+v %v body=%s", meta, err, recorder.Body.String())
			}
		})
	}
}

func TestCompletionRejectsInvalidOrUnsuccessfulJSONBeforeCommit(t *testing.T) {
	for _, payload := range []string{`{"id":"unfinished"`, `null`, `[]`, `{"id":"r","status":"in_progress"}`, `{"id":"r","status":"failed"}`, `{"error":{"message":"failed"}}`, `{"type":"error","message":"failed"}`} {
		t.Run(payload, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			called := false
			_, err := copyJSONWithCompletion(c.Writer, strings.NewReader(payload), streamProtocolResponses, func(responseMetadata) error { called = true; return nil })
			if err == nil || called || recorder.Body.Len() != 0 {
				t.Fatalf("payload=%s err=%v committed=%v body=%s", payload, err, called, recorder.Body.String())
			}
		})
	}
}

func TestCompletionDoesNotTreatNestedErrorsAsProtocolFailure(t *testing.T) {
	response := `{"id":"resp_nested","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}],"metadata":{"error":"user field"}}`
	for _, streaming := range []bool{false, true} {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		called := false
		commit := func(responseMetadata) error { called = true; return nil }
		var err error
		if streaming {
			_, err = copyStreamWithCompletion(c.Writer, strings.NewReader("data: {\"type\":\"response.completed\",\"response\":"+response+"}\n\n"), streamProtocolResponses, nil, "", commit)
		} else {
			_, err = copyJSONWithCompletion(c.Writer, strings.NewReader(response), streamProtocolResponses, commit)
		}
		if err != nil || !called {
			t.Fatalf("nested field changed completion: stream=%t err=%v called=%v", streaming, err, called)
		}
	}
}

func TestCompletionHugeNativeIdentityUsesResourceEvents(t *testing.T) {
	item := `{"id":"item_cipher","type":"reasoning","encrypted_content":"` + strings.Repeat("a", maxParsedSSEJSONBytes+1024) + `"}`
	state := &responsesCompatState{}
	rewriteResponsesDataLine([]byte("data: {\"type\":\"response.output_item.done\",\"item\":"+item+"}\n"), state)
	if state.nativeResponseID != "" {
		t.Fatal("item identity became response ownership")
	}
	// Reordered top-level fields also work when the resource ID follows the
	// large opaque content. The canonical ID cannot come from an output item.
	rewriteResponsesDataLine([]byte("data: {\"response\":{\"output\":["+item+"],\"id\":\"native_huge\"},\"type\":\"response.completed\"}\n"), state)
	if state.nativeResponseID != "native_huge" {
		t.Fatalf("missing large-frame native identity: %+v", state)
	}
}

func TestCompletionStreamDetectsFlushFailureBeforeCommit(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	writer := &completionPartialWriter{ResponseWriter: c.Writer, limit: 1 << 20, flushErr: io.ErrClosedPipe}
	called, firstToken := false, false
	meta, err := copyStreamWithCompletion(writer, strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"answer\"}}]}\n\ndata: [DONE]\n\n"), streamProtocolChat, func() { firstToken = true }, "", func(responseMetadata) error { called = true; return nil })
	if !errors.Is(err, errClientStreamWrite) || called || firstToken || meta.DeliveredBytes == 0 || strings.Contains(recorder.Body.String(), "[DONE]") {
		t.Fatalf("flush err=%v committed=%v first=%v meta=%+v body=%s", err, called, firstToken, meta, recorder.Body.String())
	}
}

func TestCompletionAbortBytesAreIncludedAndSuccessStaysHeld(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	meta, err := copyStreamWithCompletion(c.Writer, strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"answer\"}}]}\n\ndata: [DONE]\n\n"), streamProtocolChat, nil, "", func(responseMetadata) error { return inferencedomain.ErrResponseOwnershipCommit })
	if !errors.Is(err, inferencedomain.ErrResponseOwnershipCommit) || strings.Contains(recorder.Body.String(), "[DONE]") || !strings.Contains(recorder.Body.String(), "response_ownership_commit_failed") || meta.DeliveredBytes != int64(recorder.Body.Len()) || meta.DeliveredEvents != 2 {
		t.Fatalf("abort err=%v meta=%+v body=%s", err, meta, recorder.Body.String())
	}
}

func TestCompletionContradictoryTerminalNeverDisappears(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	called := false
	payload := "data: {\"choices\":[{\"delta\":{\"content\":\"answer\"}}]}\n\ndata: [DONE]\n\ndata: {\"error\":{\"code\":\"failed\",\"message\":\"upstream failure\"}}\n\n"
	_, err := copyStreamWithCompletion(c.Writer, strings.NewReader(payload), streamProtocolChat, nil, "", func(responseMetadata) error { called = true; return nil })
	if err == nil || called || strings.Contains(recorder.Body.String(), "[DONE]") || !strings.Contains(recorder.Body.String(), "error") {
		t.Fatalf("err=%v called=%v body=%s", err, called, recorder.Body.String())
	}
}

func TestCompletionNativeIdentityIsNotCompatibilityPlaceholder(t *testing.T) {
	for _, mode := range []string{"absent", "changed", "same"} {
		t.Run(mode, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			payload := ""
			if mode != "absent" {
				payload += "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_first\"}}\n\n"
			}
			id := ""
			if mode == "same" {
				id = `"id":"resp_first",`
			}
			if mode == "changed" {
				id = `"id":"resp_other",`
			}
			payload += "data: {\"type\":\"response.completed\",\"response\":{" + id + "\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"answer\"}]}]}}\n\n"
			meta, err := copyStreamWithCompletion(c.Writer, strings.NewReader(payload), streamProtocolResponses, nil, "grok", func(meta responseMetadata) error {
				if mode != "same" && meta.NativeResponseID != "" {
					t.Fatalf("compatibility fabricated native identity: %+v", meta)
				}
				if mode == "same" && meta.NativeResponseID != "resp_first" {
					t.Fatalf("native identity lost: %+v", meta)
				}
				return nil
			})
			if err != nil {
				t.Fatalf("mode=%s meta=%+v err=%v", mode, meta, err)
			}
		})
	}
}
