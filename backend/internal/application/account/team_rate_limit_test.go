package account

import (
	"sync"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

func TestTeamObservationRetainsOtherModelWindow(t *testing.T) {
	now := time.Now().UTC()
	credential := account.Credential{ID: 17, Provider: account.ProviderBuild, CredentialGeneration: 2}
	s := &Service{}
	s.ObserveTeamModelRateLimit(credential, "long-model", provider.RateLimitMetadata{TeamID: "observed", RetryAfter: time.Minute}, now)
	s.ObserveTeamModelRateLimit(credential, "short-model", provider.RateLimitMetadata{TeamID: "observed", RetryAfter: time.Second}, now)
	if _, active := s.ActiveTeamModelRateLimit(credential, "long-model", now.Add(2*time.Second)); !active {
		t.Fatal("short observation for another model forgot the active team window")
	}
	if _, active := s.ActiveTeamModelRateLimit(credential, "short-model", now.Add(2*time.Second)); active {
		t.Fatal("expired model inherited another model's longer window")
	}
	if _, active := s.ActiveTeamModelRateLimit(credential, "long-model", now.Add(time.Minute)); active {
		t.Fatal("expired longer model remained active")
	}
}

func TestTeamRateLimitIdentityIsolation(t *testing.T) {
	now := time.Now().UTC()
	original := account.Credential{ID: 71, Provider: account.ProviderBuild, CredentialGeneration: 4, TeamID: "local-original"}
	for _, scenario := range []string{"new_material", "new_local_team", "other_provider", "other_model", "same_team_rotation", "late_old_observation"} {
		t.Run(scenario, func(t *testing.T) {
			s := &Service{}
			s.ObserveTeamModelRateLimit(original, "model-a", provider.RateLimitMetadata{TeamID: "observed-original", RetryAfter: time.Minute}, now)
			current, model, expected := original, "model-a", false
			checkAt := now
			switch scenario {
			case "new_material":
				current.CredentialGeneration++
			case "new_local_team":
				current.TeamID = "replacement-team"
			case "other_provider":
				current.Provider = account.ProviderConsole
			case "other_model":
				model = "model-b"
			case "same_team_rotation":
				current.CredentialGeneration++
				current.TeamID = "observed-original"
				expected = true
			case "late_old_observation":
				current.CredentialGeneration++
				s.ObserveTeamModelRateLimit(current, "model-a", provider.RateLimitMetadata{TeamID: "observed-new", RetryAfter: time.Second}, now)
				s.ObserveTeamModelRateLimit(original, "model-a", provider.RateLimitMetadata{TeamID: "observed-original", RetryAfter: time.Minute}, now.Add(time.Millisecond))
				if limit, ok := s.ActiveTeamModelRateLimit(current, model, now.Add(time.Millisecond)); !ok || limit.TeamFingerprint != shortTeamFingerprint(rateLimitTeamFingerprint("observed-new")) {
					t.Fatal("late old observation replaced current identity")
				}
				checkAt = now.Add(2 * time.Second)
			}
			if _, limited := s.ActiveTeamModelRateLimit(current, model, checkAt); limited != expected {
				t.Fatalf("current identity limited=%t expected=%t", limited, expected)
			}
		})
	}
}

func TestTeamRateLimitConcurrentObservationsPreserveLongestWindow(t *testing.T) {
	now := time.Now().UTC()
	credential := account.Credential{ID: 17, Provider: account.ProviderBuild, CredentialGeneration: 2}
	s := &Service{}
	var workers sync.WaitGroup
	for i := 1; i <= 32; i++ {
		workers.Go(func() {
			s.ObserveTeamModelRateLimit(credential, "model", provider.RateLimitMetadata{TeamID: "observed", RetryAfter: time.Duration(i) * time.Second}, now)
			_, _ = s.ActiveTeamModelRateLimit(credential, "model", now)
		})
	}
	workers.Wait()
	// A later, shorter response cannot make an otherwise unchanged identity
	// forget the already observed longer team restriction.
	s.ObserveTeamModelRateLimit(credential, "model", provider.RateLimitMetadata{TeamID: "observed", RetryAfter: time.Second}, now)
	limit, ok := s.ActiveTeamModelRateLimit(credential, "model", now.Add(2*time.Second))
	if !ok || !limit.Until.Equal(now.Add(32*time.Second)) {
		t.Fatalf("longer window lost: limit=%+v active=%t", limit, ok)
	}
	if _, ok := s.ActiveTeamModelRateLimit(credential, "model", now.Add(32*time.Second)); ok {
		t.Fatal("expired limit remained active")
	}
}

func TestTeamRateLimitDefaultsAndMissingIdentity(t *testing.T) {
	now := time.Now().UTC()
	for _, scope := range []string{provider.RateLimitScopeRPS, provider.RateLimitScopeRPM} {
		s := &Service{}
		credential := account.Credential{ID: 5, Provider: account.ProviderBuild, TeamID: "known-team"}
		limit, ok := s.ObserveTeamModelRateLimit(credential, "model", provider.RateLimitMetadata{Scope: scope}, now)
		duration := time.Minute
		if scope == provider.RateLimitScopeRPS {
			duration = 2 * time.Second
		}
		if !ok || !limit.Until.Equal(now.Add(duration)) {
			t.Fatalf("default=%+v accepted=%t", limit, ok)
		}
		if _, active := s.ActiveTeamModelRateLimit(credential, "model", now); !active {
			t.Fatal("credential team fallback unavailable")
		}
	}
	s := &Service{}
	if _, ok := s.ObserveTeamModelRateLimit(account.Credential{ID: 5}, "model", provider.RateLimitMetadata{}, now); ok {
		t.Fatal("missing identity created team throttle")
	}
}

func TestActiveTeamModelRateLimitFallsBackToCurrentCredentialTeam(t *testing.T) {
	now := time.Now().UTC()
	const model = "grok-team-fallback"
	const observedTeam = "00000000-0000-0000-0000-0000000000e5"
	const currentTeam = "00000000-0000-0000-0000-0000000000f6"
	credential := account.Credential{ID: 42, Provider: account.ProviderBuild, TeamID: currentTeam}
	currentFingerprint := rateLimitTeamFingerprint(currentTeam)
	service := &Service{
		rateLimits: map[string]TeamModelRateLimit{
			teamModelRateLimitKey(account.ProviderBuild, currentFingerprint, model): {
				TeamFingerprint: shortTeamFingerprint(currentFingerprint), Until: now.Add(time.Minute),
			},
		},
		rateLimitTeams: map[teamRateLimitIdentity]teamRateLimitObservation{
			teamLimitIdentity(credential): {Fingerprint: rateLimitTeamFingerprint(observedTeam), ExpiresAt: now.Add(time.Minute)},
		},
	}
	service.rateLimitActive.Store(true)

	limited, ok := service.ActiveTeamModelRateLimit(credential, model, now)
	if !ok || limited.TeamFingerprint != shortTeamFingerprint(currentFingerprint) {
		t.Fatalf("limit = %#v, ok=%v", limited, ok)
	}
}

func TestActiveTeamModelRateLimitDropsExpiredObservedTeam(t *testing.T) {
	now := time.Now().UTC()
	const model = "grok-team-observation-expiry"
	const observedTeam = "00000000-0000-0000-0000-0000000000A1"
	const currentTeam = "00000000-0000-0000-0000-0000000000b2"
	credential := account.Credential{ID: 43, Provider: account.ProviderBuild, TeamID: currentTeam}
	currentFingerprint := rateLimitTeamFingerprint(currentTeam)
	service := &Service{
		rateLimits: map[string]TeamModelRateLimit{
			teamModelRateLimitKey(account.ProviderBuild, rateLimitTeamFingerprint(observedTeam), model): {
				TeamFingerprint: shortTeamFingerprint(rateLimitTeamFingerprint(observedTeam)), Until: now.Add(time.Minute),
			},
			teamModelRateLimitKey(account.ProviderBuild, currentFingerprint, model): {
				TeamFingerprint: shortTeamFingerprint(currentFingerprint), Until: now.Add(time.Minute),
			},
		},
		rateLimitTeams: map[teamRateLimitIdentity]teamRateLimitObservation{
			teamLimitIdentity(credential): {Fingerprint: rateLimitTeamFingerprint(observedTeam), ExpiresAt: now.Add(-time.Second)},
		},
	}
	service.rateLimitActive.Store(true)

	limited, ok := service.ActiveTeamModelRateLimit(credential, model, now)
	if !ok || limited.TeamFingerprint != shortTeamFingerprint(currentFingerprint) {
		t.Fatalf("limit = %#v, ok=%v", limited, ok)
	}
	if _, exists := service.rateLimitTeams[teamLimitIdentity(credential)]; exists {
		t.Fatal("expired observed Team mapping was retained")
	}
}

func TestActiveTeamModelRateLimitPrunesExpiredUnrelatedLimit(t *testing.T) {
	now := time.Now().UTC()
	service := &Service{
		rateLimits: map[string]TeamModelRateLimit{
			teamModelRateLimitKey(account.ProviderBuild, rateLimitTeamFingerprint("00000000-0000-0000-0000-0000000000a1"), "old-model"): {
				Until: now.Add(-time.Second),
			},
		},
		rateLimitTeams: map[teamRateLimitIdentity]teamRateLimitObservation{
			teamLimitIdentity(account.Credential{ID: 99}): {Fingerprint: rateLimitTeamFingerprint("00000000-0000-0000-0000-0000000000a1"), ExpiresAt: now.Add(-time.Second)},
		},
	}
	service.rateLimitActive.Store(true)
	service.rateLimitNextExpiry.Store(now.Add(-time.Second).UnixNano())

	credential := account.Credential{ID: 100, Provider: account.ProviderBuild, TeamID: "00000000-0000-0000-0000-0000000000b2"}
	if limited, ok := service.ActiveTeamModelRateLimit(credential, "new-model", now); ok {
		t.Fatalf("expired unrelated limit remained active: %#v", limited)
	}
	if service.rateLimitActive.Load() || service.rateLimitNextExpiry.Load() != 0 || len(service.rateLimits) != 0 || len(service.rateLimitTeams) != 0 {
		t.Fatalf("expired state was not fully pruned: active=%v next=%d limits=%d teams=%d", service.rateLimitActive.Load(), service.rateLimitNextExpiry.Load(), len(service.rateLimits), len(service.rateLimitTeams))
	}
}

func TestObservedTeamLimitDoesNotBindReplacementCredential(t *testing.T) {
	now := time.Now().UTC()
	credential := account.Credential{ID: 42, Provider: account.ProviderBuild, CredentialGeneration: 7, TeamID: "old-metadata-team"}
	service := &Service{}
	service.ObserveTeamModelRateLimit(credential, "grok-4.5", provider.RateLimitMetadata{TeamID: "observed-team", RetryAfter: time.Minute}, now)
	if _, limited := service.ActiveTeamModelRateLimit(credential, "grok-4.5", now); !limited {
		t.Fatal("original observed identity was not limited")
	}
	credential.CredentialGeneration++
	credential.TeamID = "replacement-team"
	if _, limited := service.ActiveTeamModelRateLimit(credential, "grok-4.5", now); limited {
		t.Fatal("old observed team bound a replacement credential")
	}
}
