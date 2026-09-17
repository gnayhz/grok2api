package provider

// Body interpretation for account blocks and DPoP challenges belongs to the
// Provider dialect boundary ; the port keeps only the stable text-signal
// fact rules these parsers delegate to.
import (
	"encoding/json"
	"strings"

	portprovider "github.com/chenyme/grok2api/backend/internal/port/provider"
)

// IsDefinitiveAccountBlockBody accepts only explicit error code or message signals.
func IsDefinitiveAccountBlockBody(body []byte) bool {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return portprovider.IsDefinitiveAccountBlockText(string(body))
	}
	values := []string{
		jsonStringField(payload, "code"),
		jsonStringField(payload, "message"),
		jsonStringField(payload, "error"),
	}
	if nested, ok := payload["error"].(map[string]any); ok {
		values = append(values,
			jsonStringField(nested, "code"),
			jsonStringField(nested, "message"),
			jsonStringField(nested, "error"),
		)
	}
	return portprovider.IsDefinitiveAccountBlockText(strings.Join(values, " "))
}

// IsDPoPProofRequiredBody reports the Console protocol-level DPoP challenge.
// It must not be attributed to an account credential or physical egress node.
func IsDPoPProofRequiredBody(body []byte) bool {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return portprovider.IsDPoPProofRequiredText(string(body))
	}
	values := []string{
		jsonStringField(payload, "code"),
		jsonStringField(payload, "message"),
		jsonStringField(payload, "error"),
	}
	if nested, ok := payload["error"].(map[string]any); ok {
		values = append(values,
			jsonStringField(nested, "code"),
			jsonStringField(nested, "message"),
			jsonStringField(nested, "error"),
		)
	}
	return portprovider.IsDPoPProofRequiredText(strings.Join(values, " "))
}

func jsonStringField(value map[string]any, key string) string {
	result, _ := value[key].(string)
	return result
}
