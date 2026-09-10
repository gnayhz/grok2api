package gateway

import (
	"context"
	"errors"
	"net/http"
	"time"

	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
)

// runPhysicalAttempt is shared by production and controlled measurements. Its
// caller owns account selection and policy; this boundary owns adapter dispatch,
// the response body and physical identity, including synthetic adapter responses.
func (s *Service) runPhysicalAttempt(ctx context.Context, request provider.ResponseResourceRequest, resources *attemptResources) (*provider.Response, error) {
	adapter, ok := s.providers.Responses(request.Credential.Provider)
	if !ok {
		return nil, errors.New("provider adapter unavailable")
	}
	started := time.Now().UTC()
	ctx = attemptmeta.WithAccount(ctx, request.Credential.ID, string(request.Credential.Provider), request.Model)
	response, err := adapter.ForwardResponse(ctx, request)
	// Both typed preparation errors and marked responses describe local input
	// rejection. Keep them out of retries, physical identities and account health.
	var validation *inferencedomain.RequestValidationError
	if errors.As(err, &validation) {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		response = &provider.Response{RequestValidation: validation, StatusCode: http.StatusBadRequest, Body: http.NoBody}
		err = nil
	}
	if response != nil {
		response.Body = resources.own(response.Body)
		if response.Attempt.ID == "" && response.RequestValidation == nil {
			response.Attempt = attemptmeta.FromContext(attemptmeta.Begin(ctx, attemptmeta.Path{}))
			response.Attempt.StartedAt = started
		}
	}
	if (response == nil || response.Body == nil) && err == nil {
		err = errors.New("provider returned no response body")
	}
	return response, err
}
