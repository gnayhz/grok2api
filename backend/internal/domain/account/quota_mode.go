package account

const (
	QuotaModeWebAuto   = "auto"
	QuotaModeWebFast   = "fast"
	QuotaModeWebExpert = "expert"
	QuotaModeWebHeavy  = "heavy"

	QuotaModeConsole      = "console"
	QuotaModeConsoleImage = "console_image"
	QuotaModeConsoleVideo = "console_video"
)

// IsWebChatQuotaMode reports whether mode is a Grok Web chat product window.
func IsWebChatQuotaMode(mode string) bool {
	switch mode {
	case QuotaModeWebAuto, QuotaModeWebFast, QuotaModeWebExpert, QuotaModeWebHeavy:
		return true
	default:
		return false
	}
}

// IsConsoleUsageQuotaMode reports whether mode is a Console usage window that
// can exhaust a matching route.
func IsConsoleUsageQuotaMode(mode string) bool {
	switch mode {
	case QuotaModeConsole, QuotaModeConsoleImage, QuotaModeConsoleVideo:
		return true
	default:
		return false
	}
}

// QuotaWindowControlsRouting reports whether a persisted window should gate
// account selection for this provider. Console keeps non-usage windows
// (billing snapshots) from blocking chat/image/video routes.
func QuotaWindowControlsRouting(provider Provider, mode string) bool {
	return provider != ProviderConsole || IsConsoleUsageQuotaMode(mode)
}
