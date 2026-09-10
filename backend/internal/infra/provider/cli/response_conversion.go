package cli

import (
	"bytes"
	"io"

	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/conversation"
	"github.com/chenyme/grok2api/backend/internal/pkg/jsonpeek"
	"github.com/chenyme/grok2api/backend/internal/pkg/responsebuffer"
	"github.com/chenyme/grok2api/backend/internal/pkg/streampipe"
)

// prepareBuildClientConversion never reads the upstream body. The raw protocol
// remains available to admission, usage and replay observers until delivery.
func prepareBuildClientConversion(response *provider.Response, request provider.ResponseResourceRequest, route buildPromptCacheRoute, compatibility *responsesToolCompatibility, options conversation.ResponseOptions) {
	clientConversation := request.Operation == conversation.OperationChat || request.Operation == conversation.OperationMessages
	var filter *buildXSearchResponseFilter
	if route.filterXSearch || len(route.injectedToolTypes) != 0 {
		filter = newBuildXSearchResponseFilter(route)
	}
	if request.Streaming {
		response.Header.Del("Content-Length")
		response.Header.Set("Content-Type", "text/event-stream")
		response.ConvertStream = func(source io.ReadCloser) io.ReadCloser {
			return streampipe.Transform(source, func(input io.Reader, writer io.Writer) error {
				state := responsebuffer.NewState(responsebuffer.BudgetOf(source), 16<<20)
				defer state.Close()
				var encoder *conversation.ResponseStreamEncoder
				if clientConversation {
					encoder = conversation.NewResponseStreamEncoderWithBudget(writer, request.Operation, options, responsebuffer.BudgetOf(source))
					defer encoder.Close()
				}
				err := consumeCompatibleSSE(input, func(event compatibleSSEEvent) error {
					if filter != nil || compatibility != nil {
						kind := jsonpeek.RootStringFieldScan(event.Data(), "type")
						retained := 0
						switch kind {
						case "response.output_item.added", "response.function_call_arguments.delta", "response.custom_tool_call_input.delta":
							retained = 512 + 4*len(event.Data())
						case "response.output_item.done":
							retained = 512 + 4*min(len(event.Data()), 64<<10)
						}
						if err := state.Grow(retained, 0); err != nil {
							return err
						}
					}
					if filter != nil && event.HasData() && !bytes.Equal(bytes.TrimSpace(event.Data()), []byte("[DONE]")) {
						filtered, keep, err := filter.filterEvent(event.Data())
						if err != nil || !keep {
							return err
						}
						event.SetData(filtered)
					}
					if encoder != nil {
						if event.HasData() {
							return encoder.Handle(event.Event, event.Data())
						}
						return nil
					}
					return compatibility.writeResponseEvent(writer, event)
				})
				if err == nil && encoder != nil {
					err = encoder.Finish()
				}
				return err
			})
		}
		return
	}
	if filter == nil && compatibility == nil && !clientConversation {
		return
	}
	response.Header.Del("Content-Length")
	response.Header.Set("Content-Type", "application/json")
	response.ConvertJSON = func(raw []byte) ([]byte, error) {
		var err error
		if filter != nil {
			raw, err = filter.filterJSON(raw)
			if err != nil {
				return nil, err
			}
		}
		if clientConversation {
			return conversation.ConvertResponseJSONWithOptions(raw, request.Operation, options)
		}
		return compatibility.normalizeResponseJSON(raw)
	}
}
