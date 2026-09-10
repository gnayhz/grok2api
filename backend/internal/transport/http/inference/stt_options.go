package inference

import (
	"bytes"
	"encoding/json"
	"math"
	"math/big"
	"mime/multipart"
	"strconv"
	"strings"

	"github.com/chenyme/grok2api/backend/internal/application/gateway"
	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
)

// Preserve number text until the transport can verify an exact conversion.
// Objects and arrays remain invalid even if they happen to contain scalar text.
type sttScalar struct {
	text    string
	invalid bool
	number  bool
}

func (v *sttScalar) UnmarshalJSON(data []byte) error {
	*v = sttScalar{}
	data = bytes.TrimSpace(data)
	if bytes.Equal(data, []byte("null")) {
		return nil
	}
	if len(data) > 0 && data[0] == '"' {
		return json.Unmarshal(data, &v.text)
	}
	if len(data) == 0 || data[0] == '{' || data[0] == '[' {
		v.invalid = true
		return nil
	}
	v.text = string(data)
	v.number = data[0] == '-' || data[0] >= '0' && data[0] <= '9'
	return nil
}

type sttOptions struct {
	SampleRate   sttScalar `json:"sample_rate"`
	Format       sttScalar `json:"format"`
	Multichannel sttScalar `json:"multichannel"`
	Channels     sttScalar `json:"channels"`
	Diarize      sttScalar `json:"diarize"`
	FillerWords  sttScalar `json:"filler_words"`
	VADThreshold sttScalar `json:"vad_threshold"`
}

func sttOptionError(param, message string) error {
	return &inferencedomain.RequestValidationError{Code: "invalid_parameter", Param: param, Message: param + " " + message}
}

// JSON and multipart share these conversions. This owns wire syntax only;
// supported languages, rates and channel capabilities remain Provider policy.
func (o sttOptions) apply(input *gateway.STTInput) error {
	if o.SampleRate.invalid {
		return sttOptionError("sample_rate", "必须是整数")
	}
	if value := strings.TrimSpace(o.SampleRate.text); value != "" {
		parsed, ok := exactSTTInteger(value)
		if !ok {
			return sttOptionError("sample_rate", "必须是可表示的整数")
		}
		input.SampleRate = strconv.FormatInt(parsed, 10)
	}
	if o.Channels.invalid {
		return sttOptionError("channels", "必须是正整数")
	}
	if value := strings.TrimSpace(o.Channels.text); value != "" {
		parsed, ok := exactSTTInteger(value)
		if !ok || parsed <= 0 || strconv.IntSize == 32 && parsed > math.MaxInt32 {
			return sttOptionError("channels", "必须是可表示的正整数")
		}
		input.Channels = int(parsed)
	}
	for _, option := range []struct {
		name   string
		value  sttScalar
		target *bool
	}{
		{"format", o.Format, &input.Format},
		{"multichannel", o.Multichannel, &input.Multichannel},
		{"diarize", o.Diarize, &input.Diarize},
		{"filler_words", o.FillerWords, &input.FillerWords},
	} {
		if option.value.invalid {
			return sttOptionError(option.name, "必须是布尔值")
		}
		switch strings.ToLower(strings.TrimSpace(option.value.text)) {
		case "", "0", "false", "no", "off":
			*option.target = false
		case "1", "true", "yes", "on":
			*option.target = true
		default:
			if parsed, ok := exactSTTInteger(strings.TrimSpace(option.value.text)); option.value.number && ok && (parsed == 0 || parsed == 1) {
				*option.target = parsed == 1
			} else {
				return sttOptionError(option.name, "必须是明确的布尔值")
			}
		}
	}
	if o.VADThreshold.invalid {
		return sttOptionError("vad_threshold", "必须是有限数值")
	}
	if value := strings.TrimSpace(o.VADThreshold.text); value != "" {
		parsed, err := strconv.ParseFloat(value, 64)
		if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
			return sttOptionError("vad_threshold", "必须是有限数值")
		}
		input.VADThreshold = &parsed
	}
	return nil
}

func exactSTTInteger(value string) (int64, bool) {
	if parsed, err := strconv.ParseInt(value, 10, 64); err == nil {
		return parsed, true
	}
	// Accept integral decimal/exponent notation without a float64 round trip.
	// Bound syntax and exponent before big.Rat so hostile exponents cannot force
	// arbitrary work; any nonzero value outside this range cannot fit int64.
	if len(value) > 64 || !json.Valid([]byte(value)) {
		return 0, false
	}
	if index := strings.IndexAny(value, "eE"); index >= 0 {
		exponent, err := strconv.Atoi(value[index+1:])
		if err != nil || exponent < -64 || exponent > 64 {
			return 0, false
		}
	}
	parsed, ok := new(big.Rat).SetString(value)
	if !ok || !parsed.IsInt() || !parsed.Num().IsInt64() {
		return 0, false
	}
	return parsed.Num().Int64(), true
}

func multipartSTTOptions(form *multipart.Form) (sttOptions, error) {
	var options sttOptions
	if form == nil {
		return options, nil
	}
	for _, field := range []struct {
		name   string
		target *sttScalar
	}{
		{"sample_rate", &options.SampleRate}, {"format", &options.Format},
		{"multichannel", &options.Multichannel}, {"channels", &options.Channels},
		{"diarize", &options.Diarize}, {"filler_words", &options.FillerWords},
		{"vad_threshold", &options.VADThreshold},
	} {
		values := form.Value[field.name]
		if len(values) > 1 {
			return options, sttOptionError(field.name, "不能重复")
		}
		if len(values) == 1 {
			field.target.text = values[0]
		}
	}
	alias := form.Value["sample_rate_hertz"]
	if len(alias) > 1 {
		return options, sttOptionError("sample_rate_hertz", "不能重复")
	}
	if len(alias) == 1 && strings.TrimSpace(alias[0]) != "" {
		if strings.TrimSpace(options.SampleRate.text) == "" {
			options.SampleRate.text = alias[0]
		} else {
			primary, primaryOK := exactSTTInteger(strings.TrimSpace(options.SampleRate.text))
			alternate, alternateOK := exactSTTInteger(strings.TrimSpace(alias[0]))
			if !primaryOK || !alternateOK || primary != alternate {
				return options, sttOptionError("sample_rate_hertz", "与 sample_rate 冲突")
			}
		}
	}
	return options, nil
}
