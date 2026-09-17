package account

import (
	"strings"
	"sync"
	"sync/atomic"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/pkg/tokenhash"
	"github.com/chenyme/grok2api/backend/internal/port/provider"
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
	return tokenhash.HashToken(teamID)
}

func shortTeamFingerprint(value string) string {
	if len(value) <= 12 {
		return value
	}
	return value[:12]
}

// teamRateLimitTracker 拥有 Team 限流观察的全部可变状态:互斥、活跃标记、
// 下次过期、模型窗口表与身份映射。Service 只经组件方法访问。
type teamRateLimitTracker struct {
	mu         sync.Mutex
	active     atomic.Bool
	nextExpiry atomic.Int64
	limits     map[string]TeamModelRateLimit
	teams      map[teamRateLimitIdentity]teamRateLimitObservation
}

func newTeamRateLimitTracker() *teamRateLimitTracker { return &teamRateLimitTracker{} }

// ActiveTeamModelRateLimit checks the current material and its current team.
// This process-local send throttle is not a replacement for shared quota state.
func (t *teamRateLimitTracker) lookupActiveTeamModelRateLimit(credential accountdomain.Credential, upstreamModel string, now time.Time) (TeamModelRateLimit, bool) {
	if !t.active.Load() {
		return TeamModelRateLimit{}, false
	}
	identity := teamLimitIdentity(credential)
	credentialFingerprint := identity.LocalTeam
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.active.Load() {
		return TeamModelRateLimit{}, false
	}
	nextExpiry := t.nextExpiry.Load()
	if nextExpiry <= 0 || now.UnixNano() >= nextExpiry {
		t.pruneTeamModelRateLimitsLocked(now)
		if len(t.limits) == 0 {
			return TeamModelRateLimit{}, false
		}
	}
	// Check the TeamID observed in an upstream response first, then current
	// credential metadata. The fallback prevents a historical observation from
	// permanently masking a later server-side team reassignment.
	observation := t.teams[identity]
	observedFingerprint := observation.Fingerprint
	if observedFingerprint != "" && !now.Before(observation.ExpiresAt) {
		delete(t.teams, identity)
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
		value, ok := t.limits[key]
		if !ok {
			continue
		}
		if !now.Before(value.Until) {
			delete(t.limits, key)
			t.refreshTeamModelRateLimitStateLocked()
			continue
		}
		return value, true
	}
	return TeamModelRateLimit{}, false
}

func (t *teamRateLimitTracker) pruneTeamModelRateLimitsLocked(now time.Time) {
	for key, value := range t.limits {
		if !now.Before(value.Until) {
			delete(t.limits, key)
		}
	}
	for identity, observation := range t.teams {
		if !now.Before(observation.ExpiresAt) {
			delete(t.teams, identity)
		}
	}
	t.refreshTeamModelRateLimitStateLocked()
}

func (t *teamRateLimitTracker) refreshTeamModelRateLimitStateLocked() {
	if len(t.limits) == 0 {
		clear(t.teams)
		t.nextExpiry.Store(0)
		t.active.Store(false)
		return
	}
	var nextExpiry time.Time
	for _, value := range t.limits {
		if nextExpiry.IsZero() || value.Until.Before(nextExpiry) {
			nextExpiry = value.Until
		}
	}
	for _, observation := range t.teams {
		if nextExpiry.IsZero() || observation.ExpiresAt.Before(nextExpiry) {
			nextExpiry = observation.ExpiresAt
		}
	}
	t.nextExpiry.Store(nextExpiry.UnixNano())
	t.active.Store(true)
}

// ObserveTeamModelRateLimit accepts Provider facts for one attempted material.
// RPS/RPM defaults, monotonic expiry and identity binding are owned here.
func (t *teamRateLimitTracker) recordTeamModelRateLimitObservation(credential accountdomain.Credential, upstreamModel string, metadata provider.RateLimitMetadata, now time.Time) (TeamModelRateLimit, bool) {
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
	t.mu.Lock()
	t.active.Store(true)
	if t.limits == nil {
		t.limits = make(map[string]TeamModelRateLimit)
	}
	if t.teams == nil {
		t.teams = make(map[teamRateLimitIdentity]teamRateLimitObservation)
	}

	for existingKey, value := range t.limits {
		if !now.Before(value.Until) {
			delete(t.limits, existingKey)
		}
	}
	for identity, observation := range t.teams {
		if !now.Before(observation.ExpiresAt) {
			delete(t.teams, identity)
		}
	}
	if current, ok := t.limits[key]; ok && !current.Until.Before(until) {
		value = current
	} else {
		t.limits[key] = value
	}
	if teamFingerprint != identity.LocalTeam {
		expiresAt := value.Until
		// Observing the same team on another model must not shorten the
		// identity mapping while its earlier model window is still active.
		if previous := t.teams[identity]; previous.Fingerprint == teamFingerprint && previous.ExpiresAt.After(expiresAt) {
			expiresAt = previous.ExpiresAt
		}
		t.teams[identity] = teamRateLimitObservation{Fingerprint: teamFingerprint, ExpiresAt: expiresAt}
	} else {
		delete(t.teams, identity)
	}
	t.refreshTeamModelRateLimitStateLocked()
	t.mu.Unlock()
	return value, true
}

// Service 导出面保持不变:经组合根注入的 Execution 能力继续使用这两个方法,
// 状态与锁只属于 teamRateLimitTracker。
func (s *Service) ActiveTeamModelRateLimit(credential accountdomain.Credential, upstreamModel string, now time.Time) (TeamModelRateLimit, bool) {
	return s.rateLimiter.lookupActiveTeamModelRateLimit(credential, upstreamModel, now)
}

func (s *Service) ObserveTeamModelRateLimit(credential accountdomain.Credential, upstreamModel string, metadata provider.RateLimitMetadata, now time.Time) (TeamModelRateLimit, bool) {
	return s.rateLimiter.recordTeamModelRateLimitObservation(credential, upstreamModel, metadata, now)
}
