package media

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

var (
	ErrVideoInputTooLarge    = errors.New("视频参考图片编码后总输入超过 32 MiB")
	ErrVideoInputUnavailable = errors.New("视频临时输入不存在或已过期")
)

// VideoInput is the persisted input contract shared by admission, execution and
// release. Voice IDs are upstream references, never local image/video assets.
type VideoInput struct {
	Operation       VideoOperation
	ImageURL        string
	ReferenceURLs   []string
	ReferenceAudios []string
	VideoURL        string
}

func inputValues(values []string) []string {
	out := make([]string, 0, len(values))
	for _, raw := range values {
		if value := strings.TrimSpace(raw); value != "" {
			out = append(out, value)
		}
	}
	return out
}

func (input VideoInput) ImageReferences() []string {
	values := make([]string, 0, 1+len(input.ReferenceURLs))
	if value := strings.TrimSpace(input.ImageURL); value != "" {
		values = append(values, value)
	}
	return append(values, inputValues(input.ReferenceURLs)...)
}

func (input VideoInput) References() []string {
	values := input.ImageReferences()
	if value := strings.TrimSpace(input.VideoURL); value != "" {
		values = append(values, value)
	}
	return values
}

func (input VideoInput) Encode() (string, error) {
	payload := map[string]any{}
	if input.Operation != "" && input.Operation != VideoOperationGenerate {
		payload["operation"] = string(input.Operation)
	}
	if value := strings.TrimSpace(input.ImageURL); value != "" {
		payload["image_url"] = value
	}
	if refs := inputValues(input.ReferenceURLs); len(refs) > 0 {
		payload["reference_urls"] = refs
	}
	if audios := inputValues(input.ReferenceAudios); len(audios) > 0 {
		payload["reference_audios"] = audios
	}
	if value := strings.TrimSpace(input.VideoURL); value != "" {
		payload["video_url"] = value
	}
	// Preserve the combined field for older workers/tools and the exact encoded
	// size contract. New readers prefer the split fields.
	if combined := input.ImageReferences(); len(combined) > 0 {
		payload["image_urls"] = combined
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("编码视频输入: %w", err)
	}
	if len(data) > MaxInputJSONBytes {
		return "", ErrVideoInputTooLarge
	}
	return string(data), nil
}

// DecodeVideoInput retains the historical permissive JSON decoding, including
// split-field precedence and the single-image versus multi-reference mapping.
func DecodeVideoInput(value string) VideoInput {
	var wire struct {
		Operation       string   `json:"operation"`
		ImageURL        string   `json:"image_url"`
		ReferenceURLs   []string `json:"reference_urls"`
		ReferenceAudios []string `json:"reference_audios"`
		ImageURLs       []string `json:"image_urls"`
		VideoURL        string   `json:"video_url"`
	}
	_ = json.Unmarshal([]byte(value), &wire)
	input := VideoInput{Operation: VideoOperationGenerate, ImageURL: strings.TrimSpace(wire.ImageURL), ReferenceAudios: inputValues(wire.ReferenceAudios), VideoURL: strings.TrimSpace(wire.VideoURL)}
	switch VideoOperation(strings.TrimSpace(wire.Operation)) {
	case VideoOperationEdit:
		input.Operation = VideoOperationEdit
	case VideoOperationExtend:
		input.Operation = VideoOperationExtend
	}
	if input.ImageURL != "" || len(wire.ReferenceURLs) > 0 || len(input.ReferenceAudios) > 0 || input.VideoURL != "" || strings.TrimSpace(wire.Operation) != "" {
		input.ReferenceURLs = inputValues(wire.ReferenceURLs)
		return input
	}
	input.ReferenceAudios = nil
	legacy := inputValues(wire.ImageURLs)
	switch len(legacy) {
	case 0:
	case 1:
		input.ImageURL = legacy[0]
	default:
		input.ReferenceURLs = legacy
	}
	return input
}

type InputAssetReference struct {
	ID   string
	Kind string
}

// LocalInputAssets avoids materializing large data URLs when the raw JSON
// cannot contain a local reference. Any JSON escape requires the authoritative
// decoder, since it can hide characters in the prefix or identifier.
func LocalInputAssets(value string) ([]InputAssetReference, error) {
	if !strings.Contains(value, InputReferencePrefix) && !strings.ContainsRune(value, '\\') {
		return nil, nil
	}
	return DecodeVideoInput(value).LocalAssets()
}

// LocalAssets returns distinct references in a stable lock order. Invalid local
// identifiers and a single asset used as two kinds cannot be admitted.
func (input VideoInput) LocalAssets() ([]InputAssetReference, error) {
	refs := make(map[string]string)
	add := func(value, kind string) error {
		value = strings.TrimSpace(value)
		if !strings.HasPrefix(value, InputReferencePrefix) {
			return nil
		}
		id, ok := ParseInputReference(value)
		if !ok || refs[id] != "" && refs[id] != kind {
			return ErrVideoInputUnavailable
		}
		refs[id] = kind
		return nil
	}
	for _, value := range input.ImageReferences() {
		if err := add(value, "image"); err != nil {
			return nil, err
		}
	}
	if err := add(input.VideoURL, "video"); err != nil {
		return nil, err
	}
	out := make([]InputAssetReference, 0, len(refs))
	for id, kind := range refs {
		out = append(out, InputAssetReference{ID: id, Kind: kind})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}
