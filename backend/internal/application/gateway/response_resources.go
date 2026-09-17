package gateway

import (
	"context"
	"errors"
	"fmt"
	"github.com/chenyme/grok2api/backend/internal/application/selector"
	"net/http"
	"net/url"
	"sync"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
	portphysical "github.com/chenyme/grok2api/backend/internal/port/physical"
	"github.com/chenyme/grok2api/backend/internal/port/provider"
)

type responseHistory interface {
	Lookup(context.Context, string, uint64, time.Time) (inferencedomain.ResponseOwnership, error)
	Record(context.Context, inferencedomain.ResponseOwnership, time.Time) error
	Forget(context.Context, string, uint64) error
}

func (s *Service) GetResponse(ctx context.Context, input ResourceInput) (*Result, error) {
	return s.forwardOwnedResponse(ctx, input, http.MethodGet)
}

func (s *Service) DeleteResponse(ctx context.Context, input ResourceInput) (*Result, error) {
	return s.forwardOwnedResponse(ctx, input, http.MethodDelete)
}

func (s *Service) forwardOwnedResponse(ctx context.Context, input ResourceInput, method string) (*Result, error) {
	ownership, err := s.responses.Lookup(ctx, input.ResponseID, input.ClientKey.ID, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	if !s.providers.SupportsStoredResponses(ownership.Provider) {
		if err := s.responses.Forget(ctx, input.ResponseID, input.ClientKey.ID); err != nil {
			return nil, err
		}
		return nil, ErrResponseNotFound
	}
	accountScope := input.ClientKey.AccountScope()
	if !accountScope.AllowsProvider(ownership.Provider) {
		return nil, &selector.SelectionUnavailableError{Reason: selector.SelectionNoAccounts, Scope: accountScope}
	}
	if _, ok := s.providers.Responses(ownership.Provider); !ok {
		return nil, ErrResponseAccountUnavailable
	}
	operation := "response_get"
	if method == http.MethodDelete {
		operation = "response_delete"
	}
	physicalCallCtx := s.startPhysicalTrace(ctx, string(ownership.Provider), operation)
	// Resource access is pinned to its owner. One explicit authentication recovery
	// is permitted, and all real sends (including connection retries) share two slots.
	requestBudget := inferencedomain.NewAttemptBudget(2)
	defer requestBudget.Close()
	physicalCallCtx = portphysical.WithPhysicalCallBudget(physicalCallCtx, requestBudget)
	lease, err := s.selector.AcquirePinnedForKey(ctx, ownership.Provider, ownership.AccountID, 0, "", "", false, accountScope)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrResponseAccountUnavailable, err)
	}
	physicalCallCtx, cancel := context.WithCancel(physicalCallCtx)
	var once sync.Once
	release := func() { once.Do(func() { cancel(); lease.Release() }) }
	stop := context.AfterFunc(physicalCallCtx, release)
	var resources *selector.AttemptResources
	finish := func() {
		if resources != nil {
			resources.Close()
		}
		stop()
		release()
	}
	handedOff := false
	defer func() {
		if !handedOff {
			finish()
		}
	}()
	credential, err := s.accounts.EnsureCredential(ctx, lease.Credential, false)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrResponseAccountUnavailable, err)
	}
	path := "/responses/" + url.PathEscape(input.ResponseID)
	if input.RawQuery != "" {
		path += "?" + input.RawQuery
	}
	forward := func(callCtx context.Context, credential accountdomain.Credential) (*provider.Response, error) {
		if resources != nil {
			resources.Close()
		}
		var attemptCtx context.Context
		attemptCtx, resources = selector.NewAttemptResources(callCtx)
		return s.runPhysicalAttempt(attemptCtx, provider.ResponseResourceRequest{Credential: credential, Method: method, Path: path}, resources)
	}
	response, err := forward(physicalCallCtx, credential)
	if err != nil {
		if isSSOCredentialRejected(err, credential) {
			s.markSSOCredentialRejected(ctx, credential, fmt.Sprintf("%s SSO credential rejected", credential.Provider))
		}
		return nil, err
	}
	if response.StatusCode == http.StatusUnauthorized {
		resources.Close()
		if credential.AuthType == accountdomain.AuthTypeSSO {
			s.markSSOCredentialRejected(ctx, credential, fmt.Sprintf("%s SSO credential rejected", credential.Provider))
			return nil, ErrResponseAccountUnavailable
		}
		if s.markPermanentlyUnrefreshableCredentialRejected(ctx, credential) {
			return nil, fmt.Errorf("%w: %w", ErrResponseAccountUnavailable, accountapp.ErrCredentialRefreshPermanent)
		}
		permit, err := requestBudget.Reserve(ctx)
		if err != nil {
			return nil, err
		}
		defer permit.Release()
		refreshed, refreshErr := s.accounts.EnsureCredential(ctx, credential, true)
		if refreshErr != nil {
			if errors.Is(refreshErr, accountapp.ErrCredentialRefreshPermanent) {
				s.markCredentialRejectedAfterPermanentRefresh(ctx, credential)
			}
			return nil, refreshErr
		}
		credential = refreshed
		response, err = forward(portphysical.WithPhysicalCallPermit(physicalCallCtx, permit), credential)
		if err != nil {
			return nil, err
		}
	}
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		s.selector.MarkSuccessWithRecovery(ctx, credential, nil)
		if method == http.MethodDelete {
			if err := s.completeResponseRemoval(ctx, input); err != nil {
				return nil, err
			}
		}
	} else if response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusGone {
		// Absence is an upstream fact; acknowledge it only after the necessary
		// local removal succeeds. Keep the controlled public error envelope.
		if err := s.completeResponseRemoval(ctx, input); err != nil {
			return nil, err
		}
		return nil, ErrResponseNotFound
	} else if response.StatusCode >= 400 {
		body, _ := readRetryableBody(response.Body)
		return nil, newHTTPUpstreamFailure(response.StatusCode, body, credential.ID, credential.Name)
	}
	handedOff = true
	return &Result{StatusCode: response.StatusCode, Status: response.Status, Header: response.Header,
		Body:     &finalizingBody{ReadCloser: response.Body, finalize: finish},
		Finalize: func(Usage, string, string) { finish() }}, nil
}

func (s *Service) completeResponseRemoval(ctx context.Context, input ResourceInput) error {
	// Once upstream deletion/absence is known, finish the local commit within
	// the existing completion budget even if the request is canceled meanwhile.
	commitCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finalizationOwnershipBudget)
	defer cancel()
	return s.responses.Forget(commitCtx, input.ResponseID, input.ClientKey.ID)
}
