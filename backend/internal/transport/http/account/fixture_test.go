package account

import (
	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	accountsyncapp "github.com/chenyme/grok2api/backend/internal/application/accountsync"
)

func newTestHandler(service *accountapp.Service, syncer accountsyncapp.Synchronizer) *Handler {
	return NewHandler(Dependencies{Administration: service, Credentials: service, Maintenance: service, Onboarding: accountsyncapp.NewOnboarding(service, service, syncer)})
}
