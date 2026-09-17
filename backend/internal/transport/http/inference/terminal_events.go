package inference

// 终止事件的唯一事实源。热路径(responseInspector.observeTerminal)、大帧
// 路径(observeHugeSSEPayload)与 completion barrier 的成功帧判定都从这里
// 取事件名;在调用侧另列一份平行表曾让 image_edit.* 终止帧在大帧路径上
// 被漏判(整图 base64 必然超过 maxParsedSSEJSONBytes,从而被误报为
// upstream_stream_incomplete)。新增终止事件只改本文件。
//
// completion barrier 的 event: 行判定还接受 response.done(兼容层合成),
// 与 JSON 根层 type 表分开维护为 completionSuccessEventKind。
func streamTerminalOutcome(protocol streamProtocol, typ string) (terminal, success bool) {
	switch protocol {
	case streamProtocolResponses:
		switch typ {
		case "response.completed":
			return true, true
		case "response.failed", "response.incomplete", "response.error", "error":
			return true, false
		}
	case streamProtocolChat:
		if typ == "error" {
			return true, false
		}
	case streamProtocolAnthropic:
		switch typ {
		case "message_stop":
			return true, true
		case "error":
			return true, false
		}
	case streamProtocolImage:
		switch typ {
		case "image_generation.completed", "image_edit.completed":
			return true, true
		case "image_generation.failed", "image_edit.failed", "error":
			return true, false
		}
	}
	return false, false
}

// completionSuccessEventKind reports whether an SSE `event:` line kind marks a
// successfully completed generation for the completion barrier. It unions the
// per-protocol success events with the compat layer's synthesized
// response.done.
func completionSuccessEventKind(kind string) bool {
	switch kind {
	case "response.completed", "response.done", "message_stop", "image_generation.completed", "image_edit.completed":
		return true
	}
	return false
}

// completionSuccessEventType is the JSON root-level `type` twin of
// completionSuccessEventKind.
func completionSuccessEventType(typ string) bool {
	return completionSuccessEventKind(typ)
}
