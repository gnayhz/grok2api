package updatecheck

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	portupdatecheck "github.com/chenyme/grok2api/backend/internal/port/updatecheck"
)

type Status string

const (
	StatusUnchecked       Status = "unchecked"
	StatusUpToDate        Status = "up_to_date"
	StatusUpdateAvailable Status = "update_available"
	StatusCheckFailed     Status = "check_failed"
)

type Snapshot struct {
	CurrentVersion  string     `json:"currentVersion"`
	LatestVersion   string     `json:"latestVersion"`
	UpdateAvailable bool       `json:"updateAvailable"`
	Status          Status     `json:"status"`
	CheckedAt       *time.Time `json:"checkedAt"`
	ReleaseURL      string     `json:"releaseUrl"`
	ReleaseNotes    string     `json:"releaseNotes"`
	Error           string     `json:"error"`
}

// Release 与 ReleaseSource 是内圈端口类型；应用层在此复用，
// 使 infra 实现与 application 消费共享同一合同而不互相 import。
type (
	Release       = portupdatecheck.Release
	ReleaseSource = portupdatecheck.ReleaseSource
)

type Service struct {
	current string
	source  ReleaseSource
	now     func() time.Time

	mu       sync.RWMutex
	snapshot Snapshot
	checks   singleflight.Group
}

func NewService(currentVersion string, source ReleaseSource) *Service {
	currentVersion = strings.TrimSpace(currentVersion)
	if currentVersion == "" {
		currentVersion = "dev"
	}
	return &Service{
		current: currentVersion,
		source:  source,
		now:     time.Now,
		snapshot: Snapshot{
			CurrentVersion: currentVersion,
			Status:         StatusUnchecked,
		},
	}
}

func (s *Service) Snapshot() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneSnapshot(s.snapshot)
}

func (s *Service) Check(ctx context.Context) Snapshot {
	result, err, _ := s.checks.Do("latest", func() (any, error) {
		if s.source == nil {
			return Release{}, errors.New("更新检查来源未配置")
		}
		return s.source.LatestRelease(ctx)
	})
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.snapshot.Status = StatusCheckFailed
		s.snapshot.Error = err.Error()
		return cloneSnapshot(s.snapshot)
	}
	release := result.(Release)
	checkedAt := s.now().UTC()
	current, currentOK := parseSemanticVersion(s.current)
	latest, latestOK := parseSemanticVersion(release.Tag)
	if !currentOK || !latestOK {
		s.snapshot.LatestVersion = release.Tag
		s.snapshot.ReleaseURL = release.URL
		s.snapshot.ReleaseNotes = release.Notes
		s.snapshot.Status = StatusCheckFailed
		s.snapshot.Error = "当前版本或最新版本不是有效的语义化版本，无法比较"
		return cloneSnapshot(s.snapshot)
	}
	available := compareSemanticVersion(latest, current) > 0
	s.snapshot = Snapshot{
		CurrentVersion: s.current, LatestVersion: release.Tag,
		UpdateAvailable: available, CheckedAt: &checkedAt,
		ReleaseURL: release.URL, ReleaseNotes: release.Notes,
		Status: StatusUpToDate,
	}
	if available {
		s.snapshot.Status = StatusUpdateAvailable
	}
	return cloneSnapshot(s.snapshot)
}

type semanticVersion struct {
	major, minor, patch uint64
	prerelease          string
}

func parseSemanticVersion(value string) (semanticVersion, bool) {
	value = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(value), "v"))
	if before, _, ok := strings.Cut(value, "+"); ok {
		value = before
	}
	prerelease := ""
	if before, after, ok := strings.Cut(value, "-"); ok {
		value, prerelease = before, after
	}
	parts := strings.Split(value, ".")
	if len(parts) != 3 {
		return semanticVersion{}, false
	}
	numbers := make([]uint64, 3)
	for index, part := range parts {
		if part == "" || (len(part) > 1 && part[0] == '0') {
			return semanticVersion{}, false
		}
		value, err := strconv.ParseUint(part, 10, 64)
		if err != nil {
			return semanticVersion{}, false
		}
		numbers[index] = value
	}
	return semanticVersion{major: numbers[0], minor: numbers[1], patch: numbers[2], prerelease: prerelease}, true
}

func compareSemanticVersion(left, right semanticVersion) int {
	for _, pair := range [][2]uint64{{left.major, right.major}, {left.minor, right.minor}, {left.patch, right.patch}} {
		if pair[0] < pair[1] {
			return -1
		}
		if pair[0] > pair[1] {
			return 1
		}
	}
	if left.prerelease == right.prerelease {
		return 0
	}
	leftHotfix, leftHotfixNumber := projectHotfix(left.prerelease)
	rightHotfix, rightHotfixNumber := projectHotfix(right.prerelease)
	if leftHotfix && rightHotfix {
		if leftHotfixNumber < rightHotfixNumber {
			return -1
		}
		if leftHotfixNumber > rightHotfixNumber {
			return 1
		}
		return 0
	}
	if leftHotfix {
		return 1
	}
	if rightHotfix {
		return -1
	}
	if left.prerelease == "" {
		return 1
	}
	if right.prerelease == "" {
		return -1
	}
	return strings.Compare(left.prerelease, right.prerelease)
}

func projectHotfix(value string) (bool, uint64) {
	const prefix = "hotfix."
	if !strings.HasPrefix(value, prefix) {
		return false, 0
	}
	part := strings.TrimPrefix(value, prefix)
	if part == "" || (len(part) > 1 && part[0] == '0') {
		return false, 0
	}
	number, err := strconv.ParseUint(part, 10, 64)
	if err != nil {
		return false, 0
	}
	return true, number
}

func cloneSnapshot(value Snapshot) Snapshot {
	if value.CheckedAt != nil {
		checkedAt := *value.CheckedAt
		value.CheckedAt = &checkedAt
	}
	return value
}
