package inference

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	historydomain "github.com/chenyme/grok2api/backend/internal/domain/history"
	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
	"github.com/chenyme/grok2api/backend/internal/pkg/responsebuffer"
	"github.com/gin-gonic/gin"
)

// writeStreamChunk detects errors that Gin's void Flush method cannot return.
// Count bytes accepted by Write, but mark first-token delivery only after a
// successful flush. Real network writers expose FlushError via the controller.
func writeStreamChunk(writer gin.ResponseWriter, chunk []byte) (n int, err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("%w: %w", errClientStreamWrite, err)
		}
	}()
	if err = setResponseWriteDeadline(writer); err != nil {
		return 0, err
	}
	n, err = writer.Write(chunk)
	if err != nil {
		return n, err
	}
	if n != len(chunk) {
		return n, io.ErrShortWrite
	}
	return n, flushStreamResponse(writer)
}

func flushStreamResponse(writer gin.ResponseWriter) error {
	writer.WriteHeaderNow()
	if flusher, ok := writer.(interface{ FlushError() error }); ok {
		return flusher.FlushError()
	}
	var target http.ResponseWriter = writer
	if wrapper, ok := writer.(interface{ Unwrap() http.ResponseWriter }); ok {
		target = wrapper.Unwrap()
	}
	err := http.NewResponseController(target).Flush()
	if errors.Is(err, http.ErrNotSupported) {
		err = nil // preserve Gin's behavior for non-network test/custom writers
	}
	return err
}

// internalSSEMarkerFilter 在转发前剥除转换器写入流的内部 SSE 注释
// （inferencedomain.ThinkingEvidenceComment——客户端未请求 thinking 的
// Messages 请求的守卫思考证据）。跨 chunk 边界的标记以 pending 前缀
// 保留，流结束时（final）冲刷剩余字节。
type internalSSEMarkerFilter struct {
	enabled bool
	pending []byte
}

func (f *internalSSEMarkerFilter) Filter(chunk []byte, final bool) []byte {
	if !f.enabled {
		return chunk
	}
	f.pending = append(f.pending, chunk...)
	result := make([]byte, 0, len(f.pending))
	for {
		index, markerLength := nextInternalSSEMarker(f.pending)
		if index >= 0 {
			result = append(result, f.pending[:index]...)
			f.pending = f.pending[index+markerLength:]
			continue
		}
		if final {
			result = append(result, f.pending...)
			f.pending = nil
			return result
		}
		// 保留可能是标记前缀的尾部字节，等待下一 chunk 判定。
		marker := []byte(inferencedomain.ThinkingEvidenceComment + "\n\n")
		keep := 0
		limit := min(len(f.pending), len(marker)-1)
		for size := limit; size > keep; size-- {
			if bytes.Equal(f.pending[len(f.pending)-size:], marker[:size]) {
				keep = size
				break
			}
		}
		result = append(result, f.pending[:len(f.pending)-keep]...)
		f.pending = f.pending[len(f.pending)-keep:]
		return result
	}
}

// nextInternalSSEMarker 返回最早出现的内部注释及其长度（未找到时 index<0）。
func nextInternalSSEMarker(value []byte) (int, int) {
	index := bytes.Index(value, []byte(inferencedomain.ThinkingEvidenceComment+"\n\n"))
	if index < 0 {
		return -1, 0
	}
	return index, len(inferencedomain.ThinkingEvidenceComment) + 2
}

// copyStreamWithFallbackModel 把请求模型注入 compat 状态作为 model 兜底：
// 流在首个 response 事件前中止时，trailer 的 model 键仍能带上真实值。
func copyStreamWithFallbackModel(writer gin.ResponseWriter, source io.Reader, protocol streamProtocol, onFirstToken func(), fallbackModel string) (metadata responseMetadata, returnErr error) {
	budget := responseReaderBudget(source)
	retention := responsebuffer.NewState(budget, 16<<20)
	defer retention.Close()
	if err := retention.Grow(0, responseCopyBufferBytes); err != nil {
		return responseMetadata{}, err
	}
	inspector := &responseInspector{protocol: protocol, onFirstToken: onFirstToken}
	// 仅 Anthropic Messages 下行会携带内部思考证据注释（chat/responses
	// 的思考增量本身可见，无需注释通道）。
	markerFilter := internalSSEMarkerFilter{enabled: protocol == streamProtocolAnthropic}
	var compat responsesCompatState
	compat.model = strings.TrimSpace(fallbackModel)
	buffer := make([]byte, responseCopyBufferBytes)
	received := 0
	transferred := 0
	// 所有 return 出口都经过 inspector.Metadata()——defer 把累计写出字节与
	// inspector 已计的事件数统一回填，无需逐出口补写（轮26）。
	// 命名返回值 + defer：所有 return 出口的返回值在 defer 统一补写交付统计
	//（inspector.Metadata() 是值拷贝，改 inspector 字段无法影响已拷贝的返回值——
	// 轮26 首版 defer 写 inspector 字段导致流式统计恒 0，活体 919 行实证）。
	defer func() {
		metadata.DeliveredBytes = int64(transferred)
		metadata.DeliveredEvents = inspector.metadata.DeliveredEvents
		if protocol == streamProtocolResponses {
			metadata.NativeResponseID = compat.nativeResponseID
		}
	}()
	for {
		n, readErr := source.Read(buffer)
		if n > 0 {
			// Reserve pending-line growth and its transient rewrite/inspection
			// copies before either byte consumer appends this chunk.
			pendingBytes := len(inspector.pending) + len(compat.pending) + n
			needed := responseCopyBufferBytes + 4*pendingBytes + 32*min(pendingBytes, maxParsedSSEJSONBytes) + 1024*(len(compat.itemIDs)+len(compat.usedItemIDs))
			if err := retention.Grow(0, needed); err != nil {
				return inspector.Metadata(), err
			}
			if len(compat.itemIDs) > 4096 || len(compat.usedItemIDs) > 4096 {
				return inspector.Metadata(), responsebuffer.ErrLimit
			}
			if received+n > maxStreamResponseTransferBytes {
				inspector.Finish()
				return inspector.Metadata(), fmt.Errorf("%w: 流式响应超过 %d MiB", errResponseTransferLimit, maxStreamResponseTransferBytes>>20)
			}
			received += n
			chunk := buffer[:n]
			chunk = markerFilter.Filter(chunk, false)
			if protocol == streamProtocolResponses {
				chunk = rewriteResponsesStreamChunk(chunk, &compat)
			}
			// Inspect the actual downstream representation so compatibility
			// fields such as generated item IDs participate in timing and
			// output-observed classification.
			inspector.Inspect(chunk)
			if transferred+len(chunk) > maxStreamResponseTransferBytes {
				inspector.Finish()
				return inspector.Metadata(), fmt.Errorf("%w: 流式响应超过 %d MiB", errResponseTransferLimit, maxStreamResponseTransferBytes>>20)
			}
			if len(chunk) > 0 {
				written, err := writeStreamChunk(writer, chunk)
				transferred += written
				if err != nil {
					inspector.Finish()
					return inspector.Metadata(), err
				}
			}
			inspector.markFirstTokenForwarded()
		}
		if readErr != nil {
			if markerTail := markerFilter.Filter(nil, true); len(markerTail) > 0 {
				if transferred+len(markerTail) > maxStreamResponseTransferBytes {
					return inspector.Metadata(), fmt.Errorf("%w: 流式响应超过 %d MiB", errResponseTransferLimit, maxStreamResponseTransferBytes>>20)
				}
				written, err := writeStreamChunk(writer, markerTail)
				transferred += written
				if err != nil {
					return inspector.Metadata(), err
				}
			}
			if protocol == streamProtocolResponses {
				if tail := flushResponsesStreamTail(&compat); len(tail) > 0 {
					inspector.Inspect(tail)
					if transferred+len(tail) > maxStreamResponseTransferBytes {
						return inspector.Metadata(), fmt.Errorf("%w: 流式响应超过 %d MiB", errResponseTransferLimit, maxStreamResponseTransferBytes>>20)
					}
					written, err := writeStreamChunk(writer, tail)
					transferred += written
					if err != nil {
						return inspector.Metadata(), err
					}
				}
			}
			inspector.Finish()
			inspector.markFirstTokenForwarded()
			terminalErr := inspector.TerminalError()
			if terminalErr == nil || errors.Is(terminalErr, errUpstreamStreamFailed) {
				return inspector.Metadata(), terminalErr
			}
			if errors.Is(readErr, io.EOF) {
				writeStreamAbortTrailer(writer, protocol, terminalErr, inspector.Metadata(), &compat, transferred)
				return inspector.Metadata(), terminalErr
			}
			writeStreamAbortTrailer(writer, protocol, readErr, inspector.Metadata(), &compat, transferred)
			return inspector.Metadata(), fmt.Errorf("%w: %w", errUpstreamStreamRead, readErr)
		}
	}
}

func writeStreamAbortTrailer(writer gin.ResponseWriter, protocol streamProtocol, cause error, meta responseMetadata, compat *responsesCompatState, transferred int) {
	trailer := streamAbortTrailer(protocol, cause, meta, compat)
	if len(trailer) == 0 || transferred+len(trailer) > maxStreamResponseTransferBytes {
		return
	}
	_, _ = writeStreamChunk(writer, trailer)
}

func streamAbortTrailer(protocol streamProtocol, cause error, meta responseMetadata, compat *responsesCompatState) []byte {
	code, message := "upstream_stream_interrupted", "上游流式响应中断"
	if sharedCode, sharedMessage, matched := streamFailureShape(cause); matched {
		code, message = sharedCode, sharedMessage
	}
	switch protocol {
	case streamProtocolChat:
		payload, err := json.Marshal(map[string]any{
			"type": "error",
			"error": map[string]any{
				"code":    code,
				"message": message,
				"type":    "server_error",
			},
		})
		if err != nil {
			return []byte("data: [DONE]\n\n")
		}
		if errors.Is(cause, inferencedomain.ErrCompletionCommit) || errors.Is(cause, historydomain.ErrHistoryCommit) {
			return []byte("data: " + string(payload) + "\n\n")
		}
		return []byte("data: " + string(payload) + "\n\ndata: [DONE]\n\n")
	case streamProtocolResponses:
		if compat == nil {
			compat = &responsesCompatState{}
		}
		compat.rememberFromMeta(meta)
		id := compat.ensureID()
		// Grok TUI 0.2.93 treats any response.incomplete as fatal
		// max_tokens_truncation (ignoring incomplete_details.reason), so a
		// synthesized incomplete trailer killed the turn instead of letting the
		// client retry. Stream aborts are transport failures: emit
		// response.failed, which TUI maps to a retryable error.
		model := strings.TrimSpace(meta.Model)
		if model == "" {
			model = strings.TrimSpace(compat.model)
		}
		response := map[string]any{
			"id":           id,
			"object":       "response",
			"created_at":   compat.createdAt,
			"completed_at": compat.createdAt,
			"status":       "failed",
			// serde 也要求 model 键必填：缺失即反序列化失败，宁可空串。
			"model":  model,
			"output": []any{},
			"error": map[string]any{
				// 严格客户端（TUI 0.2.93 serde）对非标准 code 枚举反序列化失败；
				// 细节码保留在 message 前缀里供人读与日志检索。
				"code":    "server_error",
				"message": code + ": " + message,
			},
		}
		event := map[string]any{
			"type":            "response.failed",
			"id":              id,
			"sequence_number": meta.SequenceNumber + 1,
			"response":        response,
		}
		sanitizeResponsesEvent(event, compat)
		payload, err := json.Marshal(event)
		if err != nil {
			return nil
		}
		return []byte("event: response.failed\ndata: " + string(payload) + "\n\n")
	case streamProtocolAnthropic:
		anthropicMessage := message
		if code == "upstream_output_loop" {
			anthropicMessage = code + ": " + message
		}
		payload, err := json.Marshal(map[string]any{
			"type":  "error",
			"error": map[string]any{"type": "api_error", "message": anthropicMessage},
		})
		if err != nil {
			return nil
		}
		return []byte("event: error\ndata: " + string(payload) + "\n\n")
	default:
		return nil
	}
}

func copyJSON(writer gin.ResponseWriter, source io.Reader, protocol streamProtocol) (metadata responseMetadata, returnErr error) {
	budget := responseReaderBudget(source)
	scratch, err := budget.Reserve(responseCopyBufferBytes)
	if err != nil {
		return responseMetadata{}, err
	}
	defer scratch.Release()
	buffer := make([]byte, responseCopyBufferBytes)
	metadataBody := responsebuffer.New(budget, maxJSONMetadataInspectionBytes)
	defer metadataBody.Close()
	metadataComplete := true
	transferred := 0
	// 错误出口也回填已交付字节:非流式传输中途失败(超限/写错误)时,
	// RecordDelivery 此前拿到 0, 审计里"200+错误码"的行无法反映实际已写体量。
	defer func() {
		if returnErr != nil {
			metadata = responseMetadata{DeliveredBytes: int64(transferred)}
		}
	}()
	for {
		n, readErr := source.Read(buffer)
		if n > 0 {
			if transferred+n > maxJSONResponseTransferBytes {
				return responseMetadata{}, fmt.Errorf("%w: 非流式响应超过 %d MiB", errResponseTransferLimit, maxJSONResponseTransferBytes>>20)
			}
			chunk := buffer[:n]
			if err := setResponseWriteDeadline(writer); err != nil {
				return responseMetadata{}, err
			}
			written, err := writer.Write(chunk)
			transferred += written
			if err != nil {
				return responseMetadata{}, fmt.Errorf("%w: %w", errClientStreamWrite, err)
			}
			if written != len(chunk) {
				return responseMetadata{}, fmt.Errorf("%w: %w", errClientStreamWrite, io.ErrShortWrite)
			}
			if metadataComplete {
				if metadataBody.Len()+len(chunk) <= maxJSONMetadataInspectionBytes {
					if _, err := metadataBody.Write(chunk); err != nil {
						return responseMetadata{}, err
					}
				} else {
					_ = metadataBody.Close()
					metadataComplete = false
				}
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				if metadataComplete {
					workspace, err := responsebuffer.JSONWorkspace(budget, metadataBody.Bytes())
					if err != nil {
						return responseMetadata{}, err
					}
					metadata := normalizeMetadataUsage(extractMetadata(metadataBody.Bytes()), protocol)
					workspace.Release()
					metadata.DeliveredBytes = int64(transferred)
					metadata.DeliveredEvents = 1 // 非流式：单 JSON 响应体
					return metadata, nil
				}
				return responseMetadata{}, nil
			}
			return responseMetadata{}, readErr
		}
	}
}

func responseReaderBudget(source io.Reader) *responsebuffer.Budget {
	if body, ok := source.(io.ReadCloser); ok {
		return responsebuffer.BudgetOf(body)
	}
	return responsebuffer.NewRequest()
}
