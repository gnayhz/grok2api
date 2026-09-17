package selector

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/chenyme/grok2api/backend/internal/quality/model"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

const maxConcurrentQualityMeasurements = 2

// Every measurement has an outer deadline of at most 90s (including resource
// acquisition). Its slots expire after 2m even if the process disappears, before
// the investigation task's 3m lease. Production long-request leases stay intact.
const qualityMeasurementTimeout = 90 * time.Second
const qualityMeasurementLease = qualityMeasurementTimeout + 30*time.Second

func (s *Selector) acquireBoundedProbeSlot(ctx context.Context, key string, limit int) (func(), bool, error) {
	limiter, ok := s.concurrency.(repository.BoundedConcurrencyLimiter)
	if !ok {
		return nil, false, errors.New("bounded probe capacity unavailable")
	}
	return limiter.AcquireBounded(ctx, key, limit, qualityMeasurementLease)
}

// Probe capacity uses the configured production limiter (Redis in a cluster).
// These slots bound investigation traffic independently of the worker count.
func (s *Selector) acquireProbeBudget(ctx context.Context) (func(), error) {
	return s.acquireProbeSlot(ctx, "quality:measurements", maxConcurrentQualityMeasurements)
}

func (s *Selector) acquireProbeSlot(ctx context.Context, key string, limit int) (func(), error) {
	if s == nil || s.concurrency == nil {
		return func() {}, nil
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		release, acquired, err := s.acquireBoundedProbeSlot(ctx, key, limit)
		if err != nil {
			return nil, err
		}
		if acquired {
			return release, nil
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func (s *Selector) acquireProbeResources(ctx context.Context, accountID uint64) (func(), error) {
	// Group membership is a periodically refreshed projection. Its hash can
	// change while a measurement is running, so every probe also holds the
	// stable account slot, even when a group slot is available.
	releaseAccount, err := s.acquireProbeSlot(ctx, "quality:identity/account/"+strconv.FormatUint(accountID, 10), 1)
	if err != nil {
		return nil, err
	}
	releaseIdentity := releaseAccount
	if identity, ok := model.ProbeIdentityFromContext(ctx); ok && identity.Grouped {
		releaseGroup, err := s.acquireProbeSlot(ctx, "quality:identity/group/"+strconv.FormatUint(identity.ID, 10), 1)
		if err != nil {
			releaseAccount()
			return nil, err
		}
		releaseIdentity = func() { releaseGroup(); releaseAccount() }
	}
	releaseBudget, err := s.acquireProbeBudget(ctx)
	if err != nil {
		releaseIdentity()
		return nil, err
	}
	return func() { releaseBudget(); releaseIdentity() }, nil
}
