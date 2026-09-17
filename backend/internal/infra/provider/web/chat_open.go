package web

// Web 方言请求打开与响应资源处理(conversation/stream lifetime
// 组件面):上游会话打开、响应资源转发与打开错误分类。
// 请求解码在 chat_request_codec.go;响应解析在 chat_response_codec.go。

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	historydomain "github.com/chenyme/grok2api/backend/internal/domain/history"
	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/conversation"
	"github.com/chenyme/grok2api/backend/internal/pkg/streampipe"
	provider "github.com/chenyme/grok2api/backend/internal/port/provider"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

func (a *Adapter) openChat(ctx context.Context, credential account.Credential, previousResponseID string, spec ModelSpec, input normalizedChatInput, options gatewayOpenOptions) (*http.Response, *infraegress.Lease, *inferencedomain.WebResponseState, string, error) {
	return a.openGatewayChat(ctx, credential, previousResponseID, spec, input, options)
}

func (a *Adapter) handleResponseResource(ctx context.Context, request provider.ResponseResourceRequest) (*provider.Response, error) {
	id := strings.TrimPrefix(request.Path, "/responses/")
	if before, _, ok := strings.Cut(id, "?"); ok {
		id = before
	}
	id, _ = url.PathUnescape(id)
	resources := a.states
	if request.Method == http.MethodDelete {
		if err := resources.DeleteWeb(ctx, id); err != nil {
			if !errors.Is(err, historydomain.ErrResponseNotFound) {
				return nil, err
			}
			return jsonProviderResponse(http.StatusNotFound, map[string]any{"error": map[string]any{"message": "Response 不存在", "type": "invalid_request_error"}}), nil
		}
		return jsonProviderResponse(http.StatusOK, map[string]any{"id": id, "object": "response.deleted", "deleted": true}), nil
	}
	state, err := resources.LookupWeb(ctx, id, time.Now().UTC())
	if err != nil {
		if !errors.Is(err, historydomain.ErrResponseNotFound) {
			return nil, err
		}
		return jsonProviderResponse(http.StatusNotFound, map[string]any{"error": map[string]any{"message": "Response 不存在或已过期", "type": "invalid_request_error"}}), nil
	}
	return &provider.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: jsonHeaders(), Body: io.NopCloser(strings.NewReader(state.ResponseJSON))}, nil
}

func (a *Adapter) streamOpenAIResponse(ctx context.Context, source io.ReadCloser, lease *infraegress.Lease, credential account.Credential, responseID, model, operation, prompt string, previous *inferencedomain.WebResponseState, tools toolConfiguration, parallelTools bool, options conversation.ResponseOptions, pending *responseStateCommit, physicalID string) io.ReadCloser {
	return streampipe.Transform(source, func(source io.Reader, writer io.Writer) error {
		defer lease.Release()
		resources := newWebResponseResources(ctx)
		defer resources.Close()
		parsed := &parsedChat{
			resources:  resources,
			ResponseID: responseID, InputTokens: estimateTokens(prompt), Tools: tools.ResponseTools,
			ToolChoice: tools.ResponseChoice, ParallelTools: parallelTools,
			DisableInlineCitations: !options.InlineCitationsEnabled(),
		}
		if previous != nil {
			parsed.ConversationID = previous.ConversationID
		}
		var clientText strings.Builder
		archivedImages := make(map[string]struct{})
		var sieve *toolStreamSieve
		if len(tools.Functions) > 0 && tools.Choice != "none" {
			sieve = newToolStreamSieve(tools.available)
		}
		messagesStream := newWebMessagesStream(writer, responseID, model, parsed.InputTokens, options)
		visiblePhase := webVisibleStreamPhase{}
		annotationCursor := 0
		hostedSearchEmitted := make(map[string]struct{})
		var responsesStream *webResponsesStream
		if operation == conversation.OperationResponses {
			responsesStream = newWebResponsesStream(writer, responseID)
		}
		var chatStop *webStopFilter
		if operation == conversation.OperationChat {
			chatStop = newWebStopFilter(options.StopSequences)
		}
		writeDelta := func(kind, delta string) error {
			if chatStop != nil {
				if chatStop.matched != "" {
					return nil
				}
				if kind == "text" {
					delta, _ = chatStop.Push(delta)
				}
			}
			if !visiblePhase.Allow(kind, delta) {
				return nil
			}
			if responsesStream != nil {
				return responsesStream.Delta(kind, delta)
			}
			return writeWebStreamDelta(writer, messagesStream, operation, responseID, model, kind, delta)
		}
		writeToolCalls := func(calls []parsedToolCall) error {
			if responsesStream != nil {
				return responsesStream.ToolCalls(calls)
			}
			return writeWebStreamToolCalls(writer, messagesStream, operation, responseID, model, calls)
		}
		flushAnnotations := func() error {
			if annotationCursor >= len(parsed.Annotations) {
				return nil
			}
			newOnes := parsed.Annotations[annotationCursor:]
			annotationCursor = len(parsed.Annotations)
			if responsesStream != nil {
				return responsesStream.Annotations(newOnes, annotationCursor-len(newOnes))
			}
			return writeStreamAnnotations(writer, operation, responseID, model, newOnes, annotationCursor-len(newOnes))
		}
		flushHostedSearch := func() error {
			if operation != conversation.OperationResponses {
				return nil
			}
			for _, call := range parsed.HostedSearchCalls {
				// Wait until the tool_result arrives (completed / has sources).
				if call.Status != "completed" && len(call.Sources) == 0 {
					continue
				}
				if _, emitted := hostedSearchEmitted[call.ID]; emitted {
					continue
				}
				if err := responsesStream.HostedSearch(call); err != nil {
					return err
				}
				hostedSearchEmitted[call.ID] = struct{}{}
			}
			return nil
		}
		flushSideChannel := func() error {
			if err := flushHostedSearch(); err != nil {
				return err
			}
			return flushAnnotations()
		}
		if operation != conversation.OperationMessages {
			writeStreamStart(writer, operation, responseID, model, parsed.InputTokens, historydomain.ResponseStorageRequested(options.Store))
		}
		err := consumeUpstreamInto(source, parsed, func(kind, delta string) error {
			if len(parsed.ToolCalls) > 0 && kind != "reasoning" && kind != "image" {
				// 工具调用被识别后, 剩余纯文本增量是工具语法的原文(不下发, 这是
				// 过滤器的本意); 但 image 增量仍是有效生成内容——同一请求的非流式
				// 路径(archiveChatImages)能返回它们, 流式丢图会让客户端拿到缺失图片
				// 的不完整回答且 URL 不归档。图片落到下方正常分支处理。
				return flushSideChannel()
			}
			if kind == "image" {
				rawURL := delta
				item, imageErr := a.imageDataItem(ctx, credential, imagineImageValue{URL: delta}, "url")
				if imageErr != nil {
					return imageErr
				}
				delta = liteImageMarkdown(item)
				if parsed.Text.Len() > 0 {
					delta = "\n\n" + delta
				}
				parsed.appendText(delta)
				archivedImages[rawURL] = struct{}{}
				kind = "text"
			}
			if kind == "text" && sieve != nil {
				result := sieve.Feed(delta)
				if result.Err != nil {
					return result.Err
				}
				if result.SafeText != "" {
					clientText.WriteString(result.SafeText)
					if err := writeDelta(kind, result.SafeText); err != nil {
						return err
					}
				}
				if result.Complete {
					if len(result.Calls) == 0 {
						clientText.WriteString(result.Raw)
						if err := writeDelta(kind, result.Raw); err != nil {
							return err
						}
						return flushSideChannel()
					}
					parsed.ToolCalls = result.Calls
					return writeToolCalls(result.Calls)
				}
				return flushSideChannel()
			}
			if kind == "text" {
				if delta != "" {
					clientText.WriteString(delta)
				}
			}
			if delta != "" {
				if err := writeDelta(kind, delta); err != nil {
					return err
				}
			}
			return flushSideChannel()
		})
		observeWebGeneration(ctx, physicalID, parsed)
		if err != nil {
			lease.Observe(0, err)
			return err
		}
		if sieve != nil && len(parsed.ToolCalls) == 0 {
			result := sieve.Flush()
			if result.Err != nil {
				return result.Err
			}
			if result.SafeText != "" {
				clientText.WriteString(result.SafeText)
				if err := writeDelta("text", result.SafeText); err != nil {
					return err
				}
			}
			if len(result.Calls) > 0 {
				parsed.ToolCalls = result.Calls
				if err := writeToolCalls(result.Calls); err != nil {
					return err
				}
			}
		}
		// 图片补归档不再被工具调用门控:parsed.Images 中的 URL 未必都有对应的
		// image 增量(如工具调用前已被解析), 与非流式路径 archiveChatImages 行为
		// 对齐——流式不应因工具调用而少内容。
		for _, rawURL := range parsed.Images {
			if _, exists := archivedImages[rawURL]; exists {
				continue
			}
			item, imageErr := a.imageDataItem(ctx, credential, imagineImageValue{URL: rawURL}, "url")
			if imageErr != nil {
				return imageErr
			}
			delta := liteImageMarkdown(item)
			if clientText.Len() > 0 {
				delta = "\n\n" + delta
			}
			clientText.WriteString(delta)
			if err := writeDelta("text", delta); err != nil {
				return err
			}
		}
		if chatStop != nil {
			if pending := chatStop.Flush(); pending != "" {
				if err := writeWebStreamDelta(writer, messagesStream, operation, responseID, model, "text", pending); err != nil {
					return err
				}
			}
		}
		parsed.resetText(clientText.String())
		if err := checkToolChoice(parsed, tools); err != nil {
			return err
		}
		if err := checkChatOutput(parsed); err != nil {
			return err
		}
		if operation == conversation.OperationResponses {
			finalizeXAIAnnotations(parsed)
			if finishErr := responsesStream.Finish(parsed); finishErr != nil {
				return finishErr
			}
		}
		lease.Observe(http.StatusOK, nil)
		payload := buildOpenAIResult(operation, responseID, model, *parsed, false, options)
		data, _ := json.Marshal(payload)
		if err := pending.prepare(credential.ID, responseID, *parsed, data); err != nil {
			return err
		}
		if operation == conversation.OperationMessages {
			if finishErr := messagesStream.Finish(*parsed, payload); finishErr != nil {
				return finishErr
			}
		} else {
			return writeStreamDone(writer, operation, responseID, model, *parsed, payload)
		}
		return nil
	})
}
