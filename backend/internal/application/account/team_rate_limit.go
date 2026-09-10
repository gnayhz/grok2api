package account

import (
	"strings"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

// TeamModelRateLimit is an observed upstream throttle, independent of account
// health, administrator intent and persistent quota windows. The fingerprint
// is shortened for diagnostics; the internal state uses the complete hash.
type TeamModelRateLimit struct {
	TeamFingerprint string
	Until           time.Time
}

type teamRateLimitObservation struct {
	Fingerprint string
	ExpiresAt   time.Time
}

// An upstream-observed team belongs to the material and local identity that
// sent that request. Replacing credentials or synchronizing a different team
// cannot inherit that mapping; late observations can only update their own key.
// The actual team/model limit stays shared, including across token rotation.
type teamRateLimitIdentity struct {
	Material  accountdomain.CredentialRef
	LocalTeam string
}

func teamLimitIdentity(value accountdomain.Credential) teamRateLimitIdentity {
	return teamRateLimitIdentity{Material: value.CredentialRef(), LocalTeam: rateLimitTeamFingerprint(value.TeamID)}
}

func teamModelRateLimitKey(providerValue accountdomain.Provider, teamFingerprint, upstreamModel string) string {
	return string(providerValue) + "\x00" + teamFingerprint + "\x00" + strings.TrimSpace(upstreamModel)
}

func rateLimitTeamFingerprint(teamID string) string {
	teamID = strings.ToLower(strings.TrimSpace(teamID))
	if teamID == "" {
		return ""
	}
	return security.HashToken(teamID)
}

func shortTeamFingerprint(value string) string {
	if len(value) <= 12 {
		return value
	}
	return value[:12]
}

// ActiveTeamModelRateLimit checks the current material and its current team.
// This process-local send throttle is not a replacement for shared quota state.
func (s *Service) ActiveTeamModelRateLimit(credential accountdomain.Credential, upstreamModel string, now time.Time) (TeamModelRateLimit, bool) {
	if !s.rateLimitActive.Load() {
		return TeamModelRateLimit{}, false
	}
	identity := teamLimitIdentity(credential)
	credentialFingerprint := identity.LocalTeam
	s.rateLimitMu.Lock()
	defer s.rateLimitMu.Unlock()
	if !s.rateLimitActive.Load() {
		return TeamModelRateLimit{}, false
	}
	nextExpiry := s.rateLimitNextExpiry.Load()
	if nextExpiry <= 0 || now.UnixNano() >= nextExpiry {
		s.pruneTeamModelRateLimitsLocked(now)
		if len(s.rateLimits) == 0 {
			return TeamModelRateLimit{}, false
		}
	}
	// Check the TeamID observed in an upstream response first, then current
	// credential metadata. The fallback prevents a historical observation from
	// permanently masking a later server-side team reassignment.
	observation := s.rateLimitTeams[identity]
	observedFingerprint := observation.Fingerprint
	if observedFingerprint != "" && !now.Before(observation.ExpiresAt) {
		delete(s.rateLimitTeams, identity)
		observedFingerprint = ""
	}
	teamFingerprints := [2]string{observedFingerprint, credentialFingerprint}
	fingerprintCount := 1
	if credentialFingerprint != observedFingerprint {
		fingerprintCount = 2
	}
	for index := 0; index < fingerprintCount; index++ {
		teamFingerprint := teamFingerprints[index]
		if teamFingerprint == "" {
			continue
		}
		key := teamModelRateLimitKey(credential.Provider, teamFingerprint, upstreamModel)
		value, ok := s.rateLimits[key]
		if !ok {
			continue
		}
		if !now.Before(value.Until) {
			delete(s.rateLimits, key)
			s.refreshTeamModelRateLimitStateLocked()
			continue
		}
		return value, true
	}
	return TeamModelRateLimit{}, false
}

func (s *Service) pruneTeamModelRateLimitsLocked(now time.Time) {
	for key, value := range s.rateLimits {
		if !now.Before(value.Until) {
			delete(s.rateLimits, key)
		}
	}
	for identity, observation := range s.rateLimitTeams {
		if !now.Before(observation.ExpiresAt) {
			delete(s.rateLimitTeams, identity)
		}
	}
	s.refreshTeamModelRateLimitStateLocked()
}

func (s *Service) refreshTeamModelRateLimitStateLocked() {
	if len(s.rateLimits) == 0 {
		clear(s.rateLimitTeams)
		s.rateLimitNextExpiry.Store(0)
		s.rateLimitActive.Store(false)
		return
	}
	var nextExpiry time.Time
	for _, value := range s.rateLimits {
		if nextExpiry.IsZero() || value.Until.Before(nextExpiry) {
			nextExpiry = value.Until
		}
	}
	for _, observation := range s.rateLimitTeams {
		if nextExpiry.IsZero() || observation.ExpiresAt.Before(nextExpiry) {
			nextExpiry = observation.ExpiresAt
		}
	}
	s.rateLimitNextExpiry.Store(nextExpiry.UnixNano())
	s.rateLimitActive.Store(true)
}

// ObserveTeamModelRateLimit accepts Provider facts for one attempted material.
// RPS/RPM defaults, monotonic expiry and identity binding are owned here.
func (s *Service) ObserveTeamModelRateLimit(credential accountdomain.Credential, upstreamModel string, metadata provider.RateLimitMetadata, now time.Time) (TeamModelRateLimit, bool) {
	teamID := strings.TrimSpace(metadata.TeamID)
	if teamID == "" {
		teamID = strings.TrimSpace(credential.TeamID)
	}
	if teamID == "" {
		return TeamModelRateLimit{}, false
	}
	retryAfter := metadata.RetryAfter
	if retryAfter <= 0 {
		// RPS limits recover within about one second; do not apply the generic 1m cooldown.
		if strings.EqualFold(metadata.Scope, provider.RateLimitScopeRPS) {
			retryAfter = 2 * time.Second
		} else {
			retryAfter = time.Minute
		}
	}
	identity := teamLimitIdentity(credential)
	teamFingerprint := rateLimitTeamFingerprint(teamID)
	value := TeamModelRateLimit{TeamFingerprint: shortTeamFingerprint(teamFingerprint), Until: now.Add(retryAfter)}
	key := teamModelRateLimitKey(credential.Provider, teamFingerprint, upstreamModel)
	until := now.Add(retryAfter)
	s.rateLimitMu.Lock()
	s.rateLimitActive.Store(true)
	if s.rateLimits == nil {
		s.rateLimits = make(map[string]TeamModelRateLimit)
	}
	if s.rateLimitTeams == nil {
		s.rateLimitTeams = make(map[teamRateLimitIdentity]teamRateLimitObservation)
	}

	for existingKey, value := range s.rateLimits {
		if !now.Before(value.Until) {
			delete(s.rateLimits, existingKey)
		}
	}
	for identity, observation := range s.rateLimitTeams {
		if !now.Before(observation.ExpiresAt) {
			delete(s.rateLimitTeams, identity)
		}
	}
	if current, ok := s.rateLimits[key]; ok && !current.Until.Before(until) {
		value = current
	} else {
		s.rateLimits[key] = value
	}
	if teamFingerprint != identity.LocalTeam {
		expiresAt := value.Until
		// Observing the same team on another model must not shorten the
		// identity mapping while its earlier model window is still active.
		if previous := s.rateLimitTeams[identity]; previous.Fingerprint == teamFingerprint && previous.ExpiresAt.After(expiresAt) {
			expiresAt = previous.ExpiresAt
		}
		s.rateLimitTeams[identity] = teamRateLimitObservation{Fingerprint: teamFingerprint, ExpiresAt: expiresAt}
	} else {
		delete(s.rateLimitTeams, identity)
	}
	s.refreshTeamModelRateLimitStateLocked()
	s.rateLimitMu.Unlock()
	return value, true
}
