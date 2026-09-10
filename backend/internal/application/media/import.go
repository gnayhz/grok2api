package media

import (
	"context"
	"fmt"

	mediadomain "github.com/chenyme/grok2api/backend/internal/domain/media"
)

// ImageInputImporter owns the URL-to-private-input use case. HTTP decoding,
// outbound transport and asset registration retain their respective owners.
type ImageInputImporter struct {
	media  *Service
	source mediadomain.InputImageSource
}

func NewImageInputImporter(media *Service, source mediadomain.InputImageSource) *ImageInputImporter {
	return &ImageInputImporter{media: media, source: source}
}

func (s *ImageInputImporter) Import(ctx context.Context, rawURL string) (mediadomain.Asset, error) {
	target, err := mediadomain.ParseInputImageURL(rawURL)
	if err != nil {
		return mediadomain.Asset{}, err
	}
	if s == nil || s.media == nil || s.source == nil {
		return mediadomain.Asset{}, fmt.Errorf("%w: %w", mediadomain.ErrInputImageFetch, mediadomain.ErrInputImageSourceUnavailable)
	}
	data, err := s.source.FetchImage(ctx, target, mediadomain.MaxInputAssetBytes)
	if err != nil {
		return mediadomain.Asset{}, fmt.Errorf("%w: %w", mediadomain.ErrInputImageFetch, err)
	}
	return s.media.SaveInputImage(ctx, data)
}
