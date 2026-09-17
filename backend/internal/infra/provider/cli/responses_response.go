package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/chenyme/grok2api/backend/internal/pkg/jsonvalue"
	"github.com/chenyme/grok2api/backend/internal/pkg/responseflow"
)

const (
	maxCompatibleSSEEventBytes        = 8 << 20
	maxBufferedFunctionArgumentsBytes = 1 << 20
	maxTotalBufferedFunctionArgsBytes = 4 << 20
)

// normalizeResponseJSON 将上游普通函数别名恢复为下游 namespace 和 Tool Search 输出项。
func (c *responsesToolCompatibility) normalizeResponseJSON(body []byte) ([]byte, error) {
	if c == nil {
		return body, nil
	}
	var response map[string]any
	if err := jsonvalue.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("解析 Grok Build Responses 响应: %w", err)
	}
	if err := c.rewriteResponseValue(response); err != nil {
		return nil, err
	}
	c.restoreVisibleTools(response)
	converted, err := json.Marshal(response)
	if err != nil {
		return nil, fmt.Errorf("编码兼容 Responses 响应: %w", err)
	}
	return converted, nil
}

func (c *responsesToolCompatibility) writeResponseEvent(writer io.Writer, event compatibleSSEEvent) error {
	if isPrivateBuildControlEvent(event) {
		return nil
	}
	if c == nil {
		return event.writeTo(writer)
	}
	if !event.HasData() {
		return event.writeTo(writer)
	}
	outputs, rewriteErr := c.rewriteStreamData(event.Event, event.Data())
	if rewriteErr != nil {
		return rewriteErr
	}
	for index, output := range outputs {
		outputData := output.Data
		if output.Payload != nil {
			c.resequenceStreamPayload(output.Payload)
			encoded, encodeErr := json.Marshal(output.Payload)
			if encodeErr != nil {
				return fmt.Errorf("编码兼容 Responses SSE: %w", encodeErr)
			}
			outputData = encoded
		}
		current := event
		if output.Event != "" {
			current.Event = output.Event
		}
		if index > 0 {
			current.ID = ""
			current.Retry = ""
			current.Comments = nil
			current.Other = nil
		}
		current.SetData(outputData)
		if err := current.writeTo(writer); err != nil {
			return err
		}
	}
	return nil
}

func isPrivateBuildControlEvent(event compatibleSSEEvent) bool {
	if strings.TrimSpace(event.Event) == "response.doom_loop_check" {
		return true
	}
	if !event.HasData() {
		return false
	}
	var payload struct {
		Type string `json:"type"`
	}
	return json.Unmarshal(event.Data(), &payload) == nil && payload.Type == "response.doom_loop_check"
}

type responsesStreamOutput struct {
	Event   string
	Data    []byte
	Payload map[string]any
}

type responsesStreamCall struct {
	identity     responsesToolIdentity
	schema       any
	arguments    strings.Builder
	passthrough  bool
	lastDelta    map[string]any
	addedPayload map[string]any
}

func (c *responsesToolCompatibility) rewriteStreamData(event string, data []byte) ([]responsesStreamOutput, error) {
	if bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
		return []responsesStreamOutput{{Data: data}}, nil
	}
	if !isCompatibleResponsesEvent(event) {
		return nil, nil
	}
	var payload map[string]any
	if err := jsonvalue.Unmarshal(data, &payload); err != nil {
		if event == "" {
			return nil, nil
		}
		return []responsesStreamOutput{{Data: data}}, nil
	}
	kind := stringField(payload, "type")
	if kind == "" {
		kind = event
	}
	if !isCompatibleResponsesEvent(kind) {
		return nil, nil
	}
	if kind == "response.output_item.added" {
		if item, ok := payload["item"].(map[string]any); ok {
			state := c.rememberStreamCall(item)
			if state != nil && state.identity.Kind == responsesApplyPatchTool {
				state.addedPayload = cloneJSONObject(payload)
				return nil, nil
			}
		}
	}
	if kind == "response.function_call_arguments.delta" {
		identity, state, found := c.streamIdentity(payload)
		if found && identity.Kind == responsesFunctionTool && state.schema != nil {
			delta := stringField(payload, "delta")
			callLimitExceeded := state.arguments.Len()+len(delta) > maxBufferedFunctionArgumentsBytes
			totalLimitExceeded := c.streamArgumentBytes+len(delta) > maxTotalBufferedFunctionArgsBytes
			if !state.passthrough && (callLimitExceeded || totalLimitExceeded) {
				buffered := state.arguments.String()
				c.releaseBufferedFunctionArguments(state)
				state.passthrough = true
				outputs := make([]responsesStreamOutput, 0, 2)
				if buffered != "" {
					flushed := cloneJSONObject(payload)
					flushed["delta"] = buffered
					outputs = append(outputs, responsesStreamOutput{Event: "response.function_call_arguments.delta", Payload: flushed})
				}
				outputs = append(outputs, responsesStreamOutput{Event: "response.function_call_arguments.delta", Payload: payload})
				return outputs, nil
			}
			if !state.passthrough {
				state.arguments.WriteString(delta)
				c.streamArgumentBytes += len(delta)
				state.lastDelta = cloneJSONObject(payload)
				return nil, nil
			}
		}
		if found && (identity.Kind == responsesToolSearch || identity.Kind == responsesCustomTool || identity.Kind == responsesApplyPatchTool) {
			state.arguments.WriteString(stringField(payload, "delta"))
			if identity.Kind == responsesCustomTool {
				state.lastDelta = cloneJSONObject(payload)
			}
			return nil, nil
		}
	}
	if kind == "response.function_call_arguments.done" {
		identity, state, found := c.streamIdentity(payload)
		if found && identity.Kind == responsesFunctionTool && state.schema != nil {
			arguments := stringField(payload, "arguments")
			if arguments == "" {
				arguments = state.arguments.String()
			}
			normalized, _ := normalizeFunctionArguments(arguments, state.schema)
			if state.passthrough {
				done := cloneJSONObject(payload)
				done["arguments"] = normalized
				return []responsesStreamOutput{{Event: "response.function_call_arguments.done", Payload: done}}, nil
			}
			outputs := make([]responsesStreamOutput, 0, 2)
			if state.lastDelta != nil {
				delta := cloneJSONObject(state.lastDelta)
				delta["delta"] = normalized
				outputs = append(outputs, responsesStreamOutput{Event: "response.function_call_arguments.delta", Payload: delta})
			}
			done := cloneJSONObject(payload)
			done["arguments"] = normalized
			outputs = append(outputs, responsesStreamOutput{Event: "response.function_call_arguments.done", Payload: done})
			c.releaseBufferedFunctionArguments(state)
			return outputs, nil
		}
		if found && (identity.Kind == responsesToolSearch || identity.Kind == responsesApplyPatchTool) {
			// Tool Search 的 arguments 是结构化对象；等 output_item.done 带齐参数后再对下游可见。
			return nil, nil
		}
		if found && identity.Kind == responsesCustomTool {
			arguments := stringField(payload, "arguments")
			if arguments == "" {
				arguments = state.arguments.String()
			}
			input := decodeCustomToolInput(arguments)
			outputs := make([]responsesStreamOutput, 0, 2)
			if state.lastDelta != nil {
				delta := customToolStreamPayload(state.lastDelta, "response.custom_tool_call_input.delta", "delta", input)
				outputs = append(outputs, responsesStreamOutput{Event: "response.custom_tool_call_input.delta", Payload: delta})
			}
			done := customToolStreamPayload(payload, "response.custom_tool_call_input.done", "input", input)
			outputs = append(outputs, responsesStreamOutput{Event: "response.custom_tool_call_input.done", Payload: done})
			return outputs, nil
		}
	}
	if kind == "response.output_item.done" {
		if item, ok := payload["item"].(map[string]any); ok {
			identity, exists := c.aliases[stringField(item, "name")]
			if exists && identity.Kind == responsesApplyPatchTool {
				return c.rewriteApplyPatchDoneEvent(payload, item)
			}
		}
	}
	if err := c.rewriteResponseValue(payload); err != nil {
		return nil, err
	}
	if response, ok := payload["response"].(map[string]any); ok {
		c.restoreVisibleTools(response)
	}
	return []responsesStreamOutput{{Payload: payload}}, nil
}

func (c *responsesToolCompatibility) releaseBufferedFunctionArguments(state *responsesStreamCall) {
	if c == nil || state == nil {
		return
	}
	c.streamArgumentBytes -= state.arguments.Len()
	if c.streamArgumentBytes < 0 {
		c.streamArgumentBytes = 0
	}
	state.arguments.Reset()
	state.lastDelta = nil
}

func (c *responsesToolCompatibility) resequenceStreamPayload(payload map[string]any) {
	if c == nil || payload == nil {
		return
	}
	rawSequence, exists := payload["sequence_number"]
	if !exists {
		return
	}
	if !c.streamSequenceSet {
		sequence, ok := exactJSONInt64(rawSequence)
		if !ok {
			return
		}
		c.streamSequenceNext = sequence
		c.streamSequenceSet = true
	}
	payload["sequence_number"] = c.streamSequenceNext
	c.streamSequenceNext++
}

func exactJSONInt64(value any) (int64, bool) {
	number, ok := value.(float64)
	if raw, isNumber := value.(json.Number); isNumber {
		var err error
		number, err = raw.Float64()
		ok = err == nil
	}
	if !ok || number < 0 || number > float64(maxExactJSONInteger) || number != float64(int64(number)) {
		return 0, false
	}
	return int64(number), true
}

func (c *responsesToolCompatibility) rememberStreamCall(item map[string]any) *responsesStreamCall {
	if stringField(item, "type") != "function_call" {
		return nil
	}
	identity, exists := c.aliases[stringField(item, "name")]
	if !exists {
		return nil
	}
	state := &responsesStreamCall{identity: identity, schema: c.functionSchemas[stringField(item, "name")]}
	for _, key := range []string{stringField(item, "id"), stringField(item, "call_id")} {
		if key != "" {
			c.streamCalls[key] = state
		}
	}
	return state
}

func (c *responsesToolCompatibility) streamIdentity(payload map[string]any) (responsesToolIdentity, *responsesStreamCall, bool) {
	for _, key := range []string{stringField(payload, "item_id"), stringField(payload, "call_id")} {
		if state, exists := c.streamCalls[key]; exists {
			return state.identity, state, true
		}
	}
	identity, exists := c.aliases[stringField(payload, "name")]
	if !exists {
		return responsesToolIdentity{}, nil, false
	}
	state := &responsesStreamCall{identity: identity, schema: c.functionSchemas[stringField(payload, "name")]}
	for _, key := range []string{stringField(payload, "item_id"), stringField(payload, "call_id")} {
		if key != "" {
			c.streamCalls[key] = state
		}
	}
	return identity, state, true
}

func (c *responsesToolCompatibility) rewriteResponseValue(value any) error {
	switch typed := value.(type) {
	case []any:
		for _, item := range typed {
			if err := c.rewriteResponseValue(item); err != nil {
				return err
			}
		}
	case map[string]any:
		for _, item := range typed {
			if err := c.rewriteResponseValue(item); err != nil {
				return err
			}
		}
		switch stringField(typed, "type") {
		case "function_call":
			if err := c.rewriteFunctionCall(typed); err != nil {
				return err
			}
		case "shell_call":
			if c.legacyLocalShell {
				rewriteLegacyLocalShellCall(typed)
			}
		}
	}
	return nil
}

func (c *responsesToolCompatibility) rewriteFunctionCall(call map[string]any) error {
	alias := stringField(call, "name")
	identity, exists := c.aliases[alias]
	if !exists {
		return nil
	}
	switch identity.Kind {
	case responsesFunctionTool:
		if schema := c.functionSchemas[alias]; schema != nil {
			if arguments, ok := call["arguments"].(string); ok {
				normalized, changed := normalizeFunctionArguments(arguments, schema)
				if changed {
					call["arguments"] = normalized
				}
			}
		}
		call["name"] = identity.Name
		if identity.Namespace != "" {
			call["namespace"] = identity.Namespace
		} else {
			delete(call, "namespace")
		}
	case responsesCustomTool:
		call["type"] = "custom_tool_call"
		call["name"] = identity.Name
		if identity.Namespace != "" {
			call["namespace"] = identity.Namespace
		} else {
			delete(call, "namespace")
		}
		call["input"] = decodeCustomToolInput(call["arguments"])
		delete(call, "arguments")
	case responsesToolSearch:
		call["type"] = "tool_search_call"
		call["execution"] = "client"
		call["arguments"] = decodeToolSearchArguments(call["arguments"])
		delete(call, "name")
		delete(call, "namespace")
	case responsesApplyPatchTool:
		operation, err := decodeApplyPatchArguments(call["arguments"], "response.output[].arguments")
		if err != nil {
			return fmt.Errorf("恢复 apply_patch_call: %w", err)
		}
		call["type"] = "apply_patch_call"
		call["operation"] = operation
		delete(call, "name")
		delete(call, "namespace")
		delete(call, "arguments")
	}
	return nil
}

func rewriteLegacyLocalShellCall(call map[string]any) {
	call["type"] = "local_shell_call"
	call["action"] = rewriteLegacyShellAction(call["action"])
	delete(call, "max_output_length")
}

func (c *responsesToolCompatibility) rewriteApplyPatchDoneEvent(payload, item map[string]any) ([]responsesStreamOutput, error) {
	done := cloneJSONObject(payload)
	if err := c.rewriteResponseValue(done); err != nil {
		return nil, err
	}
	doneItem, _ := done["item"].(map[string]any)
	var state *responsesStreamCall
	for _, key := range []string{stringField(item, "id"), stringField(item, "call_id")} {
		if candidate, exists := c.streamCalls[key]; exists {
			state = candidate
			break
		}
	}
	added := map[string]any{"type": "response.output_item.added"}
	if state != nil && state.addedPayload != nil {
		added = cloneJSONObject(state.addedPayload)
		added["type"] = "response.output_item.added"
	}
	for _, key := range []string{"output_index", "sequence_number"} {
		if value, exists := done[key]; exists && added[key] == nil {
			added[key] = cloneJSONValue(value)
		}
	}
	addedItem := cloneJSONObject(doneItem)
	addedItem["status"] = "in_progress"
	added["item"] = addedItem
	return []responsesStreamOutput{
		{Event: "response.output_item.added", Payload: added},
		{Event: "response.output_item.done", Payload: done},
	}, nil
}

func customToolStreamPayload(source map[string]any, kind, valueKey, value string) map[string]any {
	result := map[string]any{"type": kind, valueKey: value}
	for _, key := range []string{"item_id", "output_index", "sequence_number"} {
		if item, exists := source[key]; exists {
			result[key] = item
		}
	}
	return result
}

func isCompatibleResponsesEvent(kind string) bool {
	return kind == "" || kind == "error" || strings.HasPrefix(kind, "response.")
}

func decodeToolSearchArguments(value any) any {
	text, ok := value.(string)
	if !ok {
		return value
	}
	if strings.TrimSpace(text) == "" {
		return map[string]any{}
	}
	var decoded any
	if jsonvalue.Unmarshal([]byte(text), &decoded) == nil {
		return decoded
	}
	return map[string]any{"input": text}
}

func (c *responsesToolCompatibility) restoreVisibleTools(response map[string]any) {
	if _, exists := response["tools"]; !exists {
		return
	}
	response["tools"] = cloneJSONArray(c.visibleTools)
}

type compatibleSSEEvent struct {
	Event            string
	ID               string
	Retry            string
	Comments         []string
	Other            []string
	data             []string
	canonicalData    []byte
	hasCanonicalData bool
}

func (e compatibleSSEEvent) Data() []byte {
	if e.hasCanonicalData {
		return e.canonicalData
	}
	return []byte(strings.Join(e.data, "\n"))
}

func (e compatibleSSEEvent) HasData() bool { return e.hasCanonicalData || len(e.data) > 0 }

func (e *compatibleSSEEvent) SetData(data []byte) {
	e.canonicalData = data
	e.hasCanonicalData = true
	e.data = nil
}

func (e compatibleSSEEvent) writeTo(writer io.Writer) error {
	for _, comment := range e.Comments {
		if _, err := fmt.Fprintln(writer, comment); err != nil {
			return err
		}
	}
	if e.Event != "" {
		if _, err := fmt.Fprintf(writer, "event: %s\n", e.Event); err != nil {
			return err
		}
	}
	if e.ID != "" {
		if _, err := fmt.Fprintf(writer, "id: %s\n", e.ID); err != nil {
			return err
		}
	}
	if e.Retry != "" {
		if _, err := fmt.Fprintf(writer, "retry: %s\n", e.Retry); err != nil {
			return err
		}
	}
	for _, field := range e.Other {
		if _, err := fmt.Fprintln(writer, field); err != nil {
			return err
		}
	}
	dataLines := e.data
	if e.hasCanonicalData {
		dataLines = strings.Split(string(e.canonicalData), "\n")
	}
	for _, line := range dataLines {
		if _, err := fmt.Fprintf(writer, "data: %s\n", line); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintln(writer)
	return err
}

func consumeCompatibleSSE(source io.Reader, handle func(compatibleSSEEvent) error) error {
	return responseflow.Consume(source, func(frame *responseflow.Event) error {
		event := compatibleSSEEvent{canonicalData: frame.Data, hasCanonicalData: frame.HasData}
		frame.Fields(func(name, value []byte) {
			switch string(name) {
			case "event":
				event.Event = string(value)
			case "id":
				event.ID = string(value)
			case "retry":
				event.Retry = string(value)
			case "data":
			case "":
				event.Comments = append(event.Comments, ":"+string(value))
			default:
				field := string(name)
				if value != nil {
					field += ": " + string(value)
				}
				event.Other = append(event.Other, field)
			}
		})
		return handle(event)
	})
}
