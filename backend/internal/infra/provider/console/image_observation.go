package console

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/chenyme/grok2api/backend/internal/pkg/jsonpeek"
	"github.com/chenyme/grok2api/backend/internal/pkg/responsebuffer"
	"github.com/chenyme/grok2api/backend/internal/port/provider"
)

const consoleImageJSONLimit = 128 << 20

// inspectConsoleImages interprets the upstream image response before optional
// resource processing. Base64 is validated without allocating decoded images.
func inspectConsoleImages(ctx context.Context, data []byte, format string, requested int, observe func(provider.ImageGenerationObservation)) (int, error) {
	if !json.Valid(data) {
		return 0, errors.New("Console 图片响应不是有效 JSON")
	}
	var items []byte
	dataFields := 0
	jsonpeek.ObjectFields(data, func(key, value []byte) bool {
		if bytes.Equal(key, []byte("data")) {
			dataFields++
			items = bytes.TrimSpace(value)
		}
		return true
	})
	if dataFields != 1 || len(items) == 0 || items[0] != '[' {
		return 0, errors.New("Console 图片响应缺少有效 data")
	}
	count, total := 0, 0
	var invalid error
	jsonpeek.ArrayValues(items, func(item []byte) bool {
		total++
		if total > 10 {
			invalid = errors.New("Console 图片响应超过 10 张")
			return false
		}
		field := "url"
		if format == "b64_json" {
			field = "b64_json"
		}
		var raw []byte
		fields := 0
		jsonpeek.ObjectFields(item, func(key, value []byte) bool {
			if bytes.Equal(key, []byte(field)) {
				fields++
				raw = bytes.TrimSpace(value)
			}
			return true
		})
		if fields != 1 || len(raw) < 3 || raw[0] != '"' {
			invalid = errors.New("Console 图片响应缺少图片内容")
			return true
		}
		var workspace *responsebuffer.Lease
		if bytes.IndexByte(raw, '\\') >= 0 {
			var err error
			workspace, err = responsebuffer.FromContext(ctx).Reserve(3 * len(raw))
			if err != nil {
				invalid = err
				return false
			}
			defer workspace.Release()
		}
		value := jsonpeek.UnquoteBytes(raw)
		if len(bytes.TrimSpace(value)) == 0 {
			invalid = errors.New("Console 图片响应包含空图片")
			return true
		}
		if format == "b64_json" {
			n, err := io.Copy(io.Discard, base64.NewDecoder(base64.StdEncoding, bytes.NewReader(value)))
			if err != nil || n == 0 {
				invalid = errors.New("Console 图片响应包含无效 base64")
				return true
			}
		}
		count++
		return true
	})
	provider.ObserveImageGeneration(observe, provider.ImageGenerationObservation{OutputImages: count, QuotaUnits: count, Completed: invalid == nil && count >= requested})
	if invalid != nil {
		return count, invalid
	}
	if count < requested {
		return count, fmt.Errorf("Console 图片仅完成 %d/%d 张", count, requested)
	}
	return count, nil
}
