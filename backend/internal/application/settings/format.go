package settings

import (
	settingsdomain "github.com/chenyme/grok2api/backend/internal/domain/settings"
	"strings"
	"time"
)

const (
	recommendedBuildClientVersion = "1.0.4"
	recommendedBuildUserAgent     = "grok-shell/" + recommendedBuildClientVersion + " (linux; x86_64)"
	statsigModeManual             = "manual"
)

func normalizeBuildFallbackBaseURL(value string) string {
	return settingsdomain.NormalizeBuildFallbackBaseURL(value)
}

func formatDuration(value time.Duration) string {
	text := value.String()
	if strings.HasSuffix(text, "m0s") {
		text = strings.TrimSuffix(text, "0s")
	}
	if strings.HasSuffix(text, "h0m") {
		text = strings.TrimSuffix(text, "0m")
	}
	return text
}

func optionalDuration(value *time.Duration) time.Duration {
	if value == nil {
		return 0
	}
	return *value
}

func durationPointer(value time.Duration) *time.Duration { return &value }
