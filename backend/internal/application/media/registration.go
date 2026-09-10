package media

import (
	"context"
	"errors"

	mediadomain "github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// The final input admission checks the current shared total even if an earlier
// precheck passed. Callers compensate the already committed object on any error.
func (s *Service) registerAsset(ctx context.Context, asset mediadomain.Asset, capacityLimit int64) error {
	if asset.ExpiresAt == nil {
		return s.assets.CreateMediaAsset(ctx, asset)
	}
	err := s.assets.CreateMediaInputAsset(ctx, asset, capacityLimit)
	if errors.Is(err, repository.ErrLimitExceeded) {
		select {
		case s.cleanupSignal <- struct{}{}:
		default:
		}
		return ErrMediaCapacity
	}
	return err
}

// The input ceiling reserves cleanup headroom. Both image and video admission
// use the same snapshot for the precheck and final conditional registration.
func (s *Service) checkInputCapacity(ctx context.Context, sizeBytes int64, cfg Config) (int64, error) {
	total, err := s.assets.TotalMediaAssetBytes(ctx)
	if err != nil {
		return 0, err
	}
	capacityLimit := cleanupThresholdBytes(cfg)
	if capacityLimit <= 0 || capacityLimit > cfg.MaxTotalBytes {
		capacityLimit = cfg.MaxTotalBytes
	}
	if sizeBytes > capacityLimit || total > capacityLimit-sizeBytes {
		select {
		case s.cleanupSignal <- struct{}{}:
		default:
		}
		return 0, ErrMediaCapacity
	}
	return capacityLimit, nil
}
