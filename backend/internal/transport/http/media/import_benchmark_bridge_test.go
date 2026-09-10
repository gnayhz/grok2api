package media

import (
	"context"
	"net/url"

	mediaapp "github.com/chenyme/grok2api/backend/internal/application/media"
	mediadomain "github.com/chenyme/grok2api/backend/internal/domain/media"
)

type retrievedImageCostSource []byte

func (s retrievedImageCostSource) FetchImage(context.Context, *url.URL, int64) ([]byte, error) {
	return []byte(s), nil
}

func imageImportCostOperation(service *mediaapp.Service, rawURL string, data []byte) func(context.Context) (mediadomain.Asset, error) {
	importer := mediaapp.NewImageInputImporter(service, retrievedImageCostSource(data))
	return func(ctx context.Context) (mediadomain.Asset, error) { return importer.Import(ctx, rawURL) }
}
