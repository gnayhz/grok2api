package account

import (
	"context"
	"errors"
	"fmt"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/port/provider"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

const (
	devicePollTimeout = 20 * time.Second
	devicePollLease   = time.Minute
)

// StartDeviceLogin owns the short-lived authorization session, never the
// user's permanent credential until the provider returns a successful grant.
func (s *Service) StartDeviceLogin(ctx context.Context) (DeviceStartResult, error) {
	adapter, ok := s.providers.DeviceOAuth(accountdomain.ProviderBuild)
	if !ok {
		return DeviceStartResult{}, fmt.Errorf("CLI Provider 未注册")
	}
	authorization, err := adapter.StartDeviceAuthorization(ctx)
	if err != nil {
		return DeviceStartResult{}, err
	}
	sessionID, err := s.tokens.NewOpaqueToken(18)
	if err != nil {
		return DeviceStartResult{}, err
	}
	now := s.now().UTC()
	session := accountdomain.DeviceSession{ID: sessionID, DeviceCode: authorization.DeviceCode, UserCode: authorization.UserCode, VerificationURI: authorization.VerificationURI, VerificationURIComplete: authorization.VerificationURIComplete, Interval: authorization.Interval, NextPollAt: now.Add(authorization.Interval), ExpiresAt: now.Add(authorization.ExpiresIn)}
	if err := s.deviceSessions.Create(ctx, session); err != nil {
		return DeviceStartResult{}, err
	}
	return DeviceStartResult{SessionID: sessionID, UserCode: session.UserCode, VerificationURI: session.VerificationURI, VerificationURIComplete: session.VerificationURIComplete, Interval: session.Interval, ExpiresAt: session.ExpiresAt}, nil
}

// PollDeviceLogin atomically claims one due poll and finishes that claim on
// every exit. Known issued credentials are saved within a separate deadline;
// cancellation does not detach the actual owner from its provider or storage.
func (s *Service) PollDeviceLogin(ctx context.Context, sessionID string) (view View, retErr error) {
	adapter, ok := s.providers.DeviceOAuth(accountdomain.ProviderBuild)
	if !ok {
		return View{}, fmt.Errorf("CLI Provider 未注册")
	}
	pollCtx, cancelPoll := context.WithTimeout(ctx, devicePollTimeout)
	defer cancelPoll()
	token, err := s.tokens.NewOpaqueToken(18)
	if err != nil {
		return View{}, err
	}
	now := s.now().UTC()
	session, err := s.deviceSessions.ClaimPoll(pollCtx, sessionID, token, now, now.Add(devicePollLease))
	if errors.Is(err, repository.ErrNotFound) {
		return View{}, ErrDeviceDenied
	}
	if errors.Is(err, accountdomain.ErrDevicePollTooSoon) {
		return View{}, ErrDeviceSlowDown
	}
	if err != nil {
		return View{}, fmt.Errorf("领取设备授权轮询: %w", err)
	}
	receipt := accountdomain.DevicePollReceipt{SessionID: session.ID, Token: token}
	kind := accountdomain.DevicePollFailed
	finished := false
	finish := func() error {
		finished = true
		finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), credentialStateWriteTimeout)
		defer cancel()
		applied, err := s.deviceSessions.FinishPoll(finishCtx, receipt, accountdomain.DevicePollCompletion{Kind: kind, CompletedAt: s.now().UTC()})
		if err != nil {
			return fmt.Errorf("记录设备授权轮询结果: %w", err)
		}
		if !applied {
			return fmt.Errorf("%w: 设备授权轮询状态已变化，请重新检查授权状态", ErrConflict)
		}
		return nil
	}
	defer func() {
		if !finished {
			if err := finish(); err != nil {
				s.logger.Warn("device_poll_completion_failed", "error", err)
				retErr = err
			}
		}
	}()
	seed, err := adapter.PollDeviceAuthorization(pollCtx, session.DeviceCode)
	switch {
	case errors.Is(err, provider.ErrAuthorizationPending):
		kind = accountdomain.DevicePollPending
		return View{}, ErrDevicePending
	case errors.Is(err, provider.ErrSlowDown):
		kind = accountdomain.DevicePollSlowDown
		return View{}, ErrDeviceSlowDown
	case errors.Is(err, provider.ErrAuthorizationDenied):
		kind = accountdomain.DevicePollDenied
		return View{}, ErrDeviceDenied
	case err != nil:
		return View{}, err
	}
	kind = accountdomain.DevicePollAuthorized
	if err := finish(); err != nil {
		return View{}, err
	}
	persistCtx, cancelPersist := context.WithTimeout(context.WithoutCancel(ctx), credentialStateWriteTimeout)
	defer cancelPersist()
	installed, err := s.persistSeed(persistCtx, seed, nil, nil)
	if err != nil {
		return View{}, err
	}
	if installed.Skipped != "" {
		return View{}, fmt.Errorf("%w: 账号已删除，无法重新导入", ErrConflict)
	}
	s.reconcileProviderLinksBestEffort(persistCtx, installed.ID)
	return s.Get(persistCtx, installed.ID)
}
