package account

import (
	"context"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"github.com/chenyme/grok2api/backend/internal/testsupport"
)

func TestQuotaResetPreservesIndependentAccountRestrictions(t *testing.T) {
	for _, all := range []bool{false, true} {
		for _, quality := range []bool{false, true} {
			t.Run(map[bool]string{false: "selected", true: "all"}[all]+map[bool]string{false: "/health", true: "/quality"}[quality], func(t *testing.T) {
				ctx := context.Background()
				now := time.Now().UTC()
				s, v, _ := newCredentialRefreshTestService(t, now)
				health := accountdomain.HealthEvent{Kind: accountdomain.HealthFailure, Status: 503, CooldownBase: time.Hour, CooldownMax: time.Hour}
				if _, err := s.accounts.ApplyHealth(ctx, v.ID, v.Provider, health); err != nil {
					t.Fatal(err)
				}
				if quality {
					if _, err := s.accounts.ApplyHealth(ctx, v.ID, v.Provider, accountdomain.HealthEvent{Kind: accountdomain.HealthQualityIdle, RetryAfter: time.Hour}); err != nil {
						t.Fatal(err)
					}
				}
				before, err := s.accounts.Get(ctx, v.ID)
				if err != nil {
					t.Fatal(err)
				}
				probe := now.Add(time.Minute)
				if err := testsupport.Recovery(ctx, s.accounts, accountdomain.QuotaRecovery{AccountID: v.ID, Kind: accountdomain.QuotaRecoveryKindFree, Status: accountdomain.QuotaRecoveryStatusExhausted, ExhaustedAt: &now, NextProbeAt: &probe, UpdatedAt: now}); err != nil {
					t.Fatal(err)
				}
				if all {
					if _, err := s.ResetAllBuildQuotaState(ctx); err != nil {
						t.Fatal(err)
					}
				} else {
					if _, err := s.BatchResetQuotaState(ctx, []uint64{v.ID}); err != nil {
						t.Fatal(err)
					}
				}
				after, err := s.accounts.Get(ctx, v.ID)
				if err != nil {
					t.Fatal(err)
				}
				if after.HealthRevision != before.HealthRevision || after.FailureCount != before.FailureCount || after.LastError != before.LastError || after.CooldownUntil == nil || !after.CooldownUntil.Equal(*before.CooldownUntil) {
					t.Errorf("quota reset erased independent restriction: before revision=%d count=%d reason=%s after revision=%d count=%d reason=%s cooldown=%v", before.HealthRevision, before.FailureCount, before.LastError, after.HealthRevision, after.FailureCount, after.LastError, after.CooldownUntil)
				}
				if _, err := s.accounts.GetQuotaRecovery(ctx, v.ID); err != repository.ErrNotFound {
					t.Fatalf("quota state did not reset: %v", err)
				}
			})
		}
	}
}
