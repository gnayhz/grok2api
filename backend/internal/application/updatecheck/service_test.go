package updatecheck

import (
	"context"
	"errors"
	"testing"
	"time"
)

type releaseSourceFunc func(context.Context) (Release, error)

func (f releaseSourceFunc) LatestRelease(ctx context.Context) (Release, error) { return f(ctx) }

func TestCheckFindsLatestRelease(t *testing.T) {
	service := NewService("v3.0.0", releaseSourceFunc(func(context.Context) (Release, error) {
		return Release{Tag: "v3.0.1", URL: "https://github.com/chenyme/grok2api/releases/tag/v3.0.1", Notes: "Release notes"}, nil
	}))
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }
	snapshot := service.Check(context.Background())
	if snapshot.Status != StatusUpdateAvailable || !snapshot.UpdateAvailable || snapshot.LatestVersion != "v3.0.1" || snapshot.CheckedAt == nil || !snapshot.CheckedAt.Equal(now) {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	if snapshot.ReleaseURL != "https://github.com/chenyme/grok2api/releases/tag/v3.0.1" || snapshot.ReleaseNotes != "Release notes" {
		t.Fatalf("release = %#v", snapshot)
	}
}

func TestCheckFailureKeepsLastSuccessfulRelease(t *testing.T) {
	fail := false
	service := NewService("v3.0.0", releaseSourceFunc(func(context.Context) (Release, error) {
		if fail {
			return Release{}, errors.New("network down")
		}
		return Release{Tag: "v3.0.0", Notes: "Stable"}, nil
	}))
	first := service.Check(context.Background())
	fail = true
	second := service.Check(context.Background())
	if first.Status != StatusUpToDate || second.Status != StatusCheckFailed || second.LatestVersion != "v3.0.0" || second.CheckedAt == nil || second.Error == "" {
		t.Fatalf("first=%#v second=%#v", first, second)
	}
}

func TestSemanticVersionComparison(t *testing.T) {
	stable, ok := parseSemanticVersion("v3.0.1")
	if !ok {
		t.Fatal("stable version was rejected")
	}
	older, _ := parseSemanticVersion("3.0.0")
	prerelease, _ := parseSemanticVersion("v3.0.1-rc.1")
	if compareSemanticVersion(stable, older) <= 0 || compareSemanticVersion(prerelease, stable) >= 0 {
		t.Fatal("semantic version ordering is invalid")
	}
	if _, ok := parseSemanticVersion("dev"); ok {
		t.Fatal("development version was accepted as semver")
	}
	base, _ := parseSemanticVersion("v3.0.8")
	hotfix1, _ := parseSemanticVersion("v3.0.8-hotfix.1")
	hotfix2, _ := parseSemanticVersion("v3.0.8-hotfix.2")
	sameBasePrerelease, _ := parseSemanticVersion("v3.0.8-rc.1")
	next, _ := parseSemanticVersion("v3.0.9")
	if compareSemanticVersion(hotfix1, base) <= 0 || compareSemanticVersion(hotfix2, hotfix1) <= 0 || compareSemanticVersion(next, hotfix2) <= 0 {
		t.Fatal("project hotfix ordering is invalid")
	}
	if compareSemanticVersion(hotfix1, sameBasePrerelease) <= 0 {
		t.Fatal("project hotfix should follow ordinary prereleases")
	}
}

func TestCheckFindsHotfixAfterStableRelease(t *testing.T) {
	service := NewService("v3.0.8", releaseSourceFunc(func(context.Context) (Release, error) {
		return Release{Tag: "v3.0.8-hotfix.1"}, nil
	}))
	snapshot := service.Check(context.Background())
	if snapshot.Status != StatusUpdateAvailable || !snapshot.UpdateAvailable {
		t.Fatalf("snapshot = %#v", snapshot)
	}
}
