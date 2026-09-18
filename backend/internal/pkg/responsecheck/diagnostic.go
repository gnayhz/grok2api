package responsecheck

import (
	"encoding/json"
	"unicode/utf8"
)

// FailureDiagnostic is a bounded projection of a protocol failure envelope.
type FailureDiagnostic struct {
	Body          []byte
	BodyTruncated bool
}

// ProjectFailureDiagnostic retains only bounded error metadata, never output items.
// The audit owner must still redact credentials and private addresses before storage.
func ProjectFailureDiagnostic(data []byte, limit int) FailureDiagnostic {
	if limit <= 0 {
		return FailureDiagnostic{BodyTruncated: len(data) > 0}
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(data, &root) != nil {
		return FailureDiagnostic{}
	}
	projected := make(map[string]json.RawMessage)
	copySafeDiagnosticFields(projected, root, "type", "status", "code", "message", "param")
	if raw := projectSafeErrorValue(root["error"]); len(raw) > 0 {
		projected["error"] = raw
	}
	if responseRaw := root["response"]; len(responseRaw) > 0 {
		var response map[string]json.RawMessage
		if json.Unmarshal(responseRaw, &response) == nil {
			safeResponse := make(map[string]json.RawMessage)
			copySafeDiagnosticFields(safeResponse, response, "id", "status", "code", "message")
			if raw := projectSafeErrorValue(response["error"]); len(raw) > 0 {
				safeResponse["error"] = raw
			}
			if raw := projectSafeErrorValue(response["incomplete_details"]); len(raw) > 0 {
				safeResponse["incomplete_details"] = raw
			}
			if len(safeResponse) > 0 {
				if encoded, err := json.Marshal(safeResponse); err == nil {
					projected["response"] = encoded
				}
			}
		}
	}
	if len(projected) == 0 {
		return FailureDiagnostic{}
	}
	encoded, err := json.Marshal(projected)
	if err != nil {
		return FailureDiagnostic{}
	}
	diagnostic := FailureDiagnostic{Body: encoded}
	if len(diagnostic.Body) > limit {
		bounded := diagnostic.Body[:limit]
		for len(bounded) > 0 && !utf8.Valid(bounded) {
			bounded = bounded[:len(bounded)-1]
		}
		diagnostic.Body = append([]byte(nil), bounded...)
		diagnostic.BodyTruncated = true
	} else {
		diagnostic.Body = append([]byte(nil), diagnostic.Body...)
	}
	return diagnostic
}

func copySafeDiagnosticFields(destination, source map[string]json.RawMessage, fields ...string) {
	for _, field := range fields {
		if raw := projectSafeScalar(source[field]); len(raw) > 0 {
			destination[field] = raw
		}
	}
}

func projectSafeErrorValue(raw json.RawMessage) json.RawMessage {
	if scalar := projectSafeScalar(raw); len(scalar) > 0 {
		return scalar
	}
	var value map[string]json.RawMessage
	if json.Unmarshal(raw, &value) != nil {
		return nil
	}
	projected := make(map[string]json.RawMessage)
	copySafeDiagnosticFields(projected, value, "type", "status", "code", "message", "param", "reason")
	if len(projected) == 0 {
		return nil
	}
	encoded, err := json.Marshal(projected)
	if err != nil {
		return nil
	}
	return encoded
}

func projectSafeScalar(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return nil
	}
	switch value.(type) {
	case nil, string, bool, float64:
		return append(json.RawMessage(nil), raw...)
	default:
		return nil
	}
}
