package media

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestEncodeVideoInputEnforcesPersistedLimit(t *testing.T) {
	// image_url and combined image_urls both store the same value, so the URL is counted twice.
	base := `{"image_url":"","image_urls":[""]}`
	overhead := len(base)
	urlLen := (MaxInputJSONBytes - overhead) / 2
	atLimit := strings.Repeat("A", urlLen)
	encoded, err := (VideoInput{ImageURL: atLimit}).Encode()
	if err != nil {
		t.Fatalf("encode at limit: %v", err)
	}
	if len(encoded) > MaxInputJSONBytes {
		t.Fatalf("encoded len=%d exceeds limit", len(encoded))
	}
	if _, err := (VideoInput{ImageURL: atLimit + "AA"}).Encode(); !errors.Is(err, ErrVideoInputTooLarge) {
		t.Fatalf("oversized input error = %v", err)
	}
}

func TestEncodeDecodeVideoInputPreservesImageAndReferences(t *testing.T) {
	encoded, err := (VideoInput{ImageURL: "https://example.com/first.png", ReferenceURLs: []string{"https://example.com/ref.png"}}).Encode()
	if err != nil {
		t.Fatal(err)
	}
	input := DecodeVideoInput(encoded)
	imageURL, refs := input.ImageURL, input.ReferenceURLs
	if imageURL != "https://example.com/first.png" || len(refs) != 1 || refs[0] != "https://example.com/ref.png" {
		t.Fatalf("decoded split = %q %#v from %s", imageURL, refs, encoded)
	}

	encoded, err = (VideoInput{ReferenceURLs: []string{"https://example.com/ref-only.png"}}).Encode()
	if err != nil {
		t.Fatal(err)
	}
	input = DecodeVideoInput(encoded)
	imageURL, refs = input.ImageURL, input.ReferenceURLs
	if imageURL != "" || len(refs) != 1 || refs[0] != "https://example.com/ref-only.png" {
		t.Fatalf("single reference decoded = %q %#v from %s", imageURL, refs, encoded)
	}

	input = DecodeVideoInput(`{"image_urls":["https://legacy/one.png"]}`)
	imageURL, refs = input.ImageURL, input.ReferenceURLs
	if imageURL != "https://legacy/one.png" || len(refs) != 0 {
		t.Fatalf("legacy single = %q %#v", imageURL, refs)
	}
	input = DecodeVideoInput(`{"image_urls":["https://legacy/a.png","https://legacy/b.png"]}`)
	imageURL, refs = input.ImageURL, input.ReferenceURLs
	if imageURL != "" || len(refs) != 2 || refs[0] != "https://legacy/a.png" || refs[1] != "https://legacy/b.png" {
		t.Fatalf("legacy multi = %q %#v", imageURL, refs)
	}
}

func TestEncodeDecodeVideoInputPreservesOperationAndReferenceAudio(t *testing.T) {
	encoded, err := (VideoInput{Operation: VideoOperationGenerate, ReferenceURLs: []string{"https://example.com/ref.png"}, ReferenceAudios: []string{"eve", " ara "}}).Encode()
	if err != nil {
		t.Fatal(err)
	}
	input := DecodeVideoInput(encoded)
	imageURL, refs, audios, videoURL := input.ImageURL, input.ReferenceURLs, input.ReferenceAudios, input.VideoURL
	if imageURL != "" || videoURL != "" || len(refs) != 1 || refs[0] != "https://example.com/ref.png" || len(audios) != 2 || audios[0] != "eve" || audios[1] != "ara" {
		t.Fatalf("decoded reference input = image %q refs %#v audios %#v video %q from %s", imageURL, refs, audios, videoURL, encoded)
	}
	if operation := DecodeVideoInput(encoded).Operation; operation != VideoOperationGenerate {
		t.Fatalf("generation operation = %q", operation)
	}

	encoded, err = (VideoInput{Operation: VideoOperationExtend, VideoURL: InputReference("source-video")}).Encode()
	if err != nil {
		t.Fatal(err)
	}
	if operation := DecodeVideoInput(encoded).Operation; operation != VideoOperationExtend {
		t.Fatalf("extension operation = %q from %s", operation, encoded)
	}
	videoURL = DecodeVideoInput(encoded).VideoURL
	if videoURL != InputReference("source-video") {
		t.Fatalf("extension video = %q", videoURL)
	}

}

func TestPersistedVideoInputReferencesUseWorkerInterpretation(t *testing.T) {
	imageID := "input_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	videoID := "input_BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"
	imageRef, videoRef := InputReference(imageID), InputReference(videoID)
	cases := []struct {
		name, raw string
		want      VideoInput
		local     []InputAssetReference
		invalid   bool
	}{
		{name: "legacy_one", raw: `{"image_urls":["  ` + imageRef + `  "]}`, want: VideoInput{Operation: VideoOperationGenerate, ImageURL: imageRef}, local: []InputAssetReference{{ID: imageID, Kind: "image"}}},
		{name: "legacy_many", raw: `{"image_urls":["https://remote.invalid/a","` + imageRef + `"]}`, want: VideoInput{Operation: VideoOperationGenerate, ReferenceURLs: []string{"https://remote.invalid/a", imageRef}}, local: []InputAssetReference{{ID: imageID, Kind: "image"}}},
		{name: "split_overrides_legacy", raw: `{"image_url":"https://remote.invalid/a","image_urls":["` + imageRef + `"]}`, want: VideoInput{Operation: VideoOperationGenerate, ImageURL: "https://remote.invalid/a"}},
		{name: "audio_is_voice_id", raw: `{"reference_audios":["` + imageRef + `"],"image_urls":["` + imageRef + `"]}`, want: VideoInput{Operation: VideoOperationGenerate, ReferenceAudios: []string{imageRef}}},
		{name: "escaped_local_id", raw: `{"image_url":"grok2api-input:input_\u0041AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}`, want: VideoInput{Operation: VideoOperationGenerate, ImageURL: imageRef}, local: []InputAssetReference{{ID: imageID, Kind: "image"}}},
		{name: "escaped_prefix", raw: `{"image_url":"\u0067rok2api-input:` + imageID + `"}`, want: VideoInput{Operation: VideoOperationGenerate, ImageURL: imageRef}, local: []InputAssetReference{{ID: imageID, Kind: "image"}}},
		{name: "duplicate_and_order", raw: `{"operation":" edit ","image_url":"` + imageRef + `","reference_urls":["` + imageRef + `"],"video_url":"` + videoRef + `"}`, want: VideoInput{Operation: VideoOperationEdit, ImageURL: imageRef, ReferenceURLs: []string{imageRef}, VideoURL: videoRef}, local: []InputAssetReference{{ID: imageID, Kind: "image"}, {ID: videoID, Kind: "video"}}},
		{name: "unknown_operation_keeps_split_precedence", raw: `{"operation":"future","image_urls":["` + imageRef + `"]}`, want: VideoInput{Operation: VideoOperationGenerate}},
		{name: "ignored_fields", raw: `{"unknown":"` + imageRef + `"}`, want: VideoInput{Operation: VideoOperationGenerate}},
		{name: "empty_object", raw: `{}`, want: VideoInput{Operation: VideoOperationGenerate}},
		{name: "invalid_json", raw: `{"image_url":`, want: VideoInput{Operation: VideoOperationGenerate}},
		{name: "invalid_local_namespace", raw: `{"image_url":"grok2api-input:bad"}`, want: VideoInput{Operation: VideoOperationGenerate, ImageURL: "grok2api-input:bad"}, invalid: true},
		{name: "same_id_two_kinds", raw: `{"image_url":"` + imageRef + `","video_url":"` + imageRef + `"}`, want: VideoInput{Operation: VideoOperationGenerate, ImageURL: imageRef, VideoURL: imageRef}, invalid: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := DecodeVideoInput(tc.raw)
			if got.Operation != tc.want.Operation || got.ImageURL != tc.want.ImageURL || got.VideoURL != tc.want.VideoURL || !slices.Equal(got.ReferenceURLs, tc.want.ReferenceURLs) || !slices.Equal(got.ReferenceAudios, tc.want.ReferenceAudios) {
				t.Fatalf("decoded=%+v want=%+v", got, tc.want)
			}
			local, err := LocalInputAssets(tc.raw)
			if tc.invalid {
				if !errors.Is(err, ErrVideoInputUnavailable) {
					t.Fatalf("invalid local references=%+v err=%v", local, err)
				}
				return
			}
			if err != nil || !slices.Equal(local, tc.local) {
				t.Fatalf("local=%+v want=%+v err=%v", local, tc.local, err)
			}
		})
	}
}

func TestVideoInputEncodingRetainsExistingWireBytes(t *testing.T) {
	got, err := (VideoInput{Operation: VideoOperationExtend, ImageURL: " https://first ", ReferenceURLs: []string{"", " https://ref "}, ReferenceAudios: []string{" eve ", ""}, VideoURL: " https://video "}).Encode()
	want := `{"image_url":"https://first","image_urls":["https://first","https://ref"],"operation":"extend","reference_audios":["eve"],"reference_urls":["https://ref"],"video_url":"https://video"}`
	if err != nil || got != want {
		t.Fatalf("encoded=%s err=%v", got, err)
	}
}
