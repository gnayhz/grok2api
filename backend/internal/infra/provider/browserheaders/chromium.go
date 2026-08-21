package browserheaders

import (
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

var (
	chromiumVersionPattern = regexp.MustCompile(`(?i)\b(?:chrome|chromium|crios)/(\d{2,3})(?:\.\d+)*`)
	edgeVersionPattern     = regexp.MustCompile(`(?i)\b(?:edg|edga|edgios)/(\d{2,3})(?:\.\d+)*`)
)

// ApplyChromiumClientHints 根据真实 User-Agent 补齐一致的 Chromium Client Hints。
// 非 Chromium UA 不生成提示头，避免互相矛盾的浏览器指纹。
func ApplyChromiumClientHints(header http.Header, userAgent string) {
	lower := strings.ToLower(userAgent)
	brand := "Google Chrome"
	match := chromiumVersionPattern.FindStringSubmatch(userAgent)
	if edge := edgeVersionPattern.FindStringSubmatch(userAgent); len(edge) == 2 {
		brand, match = "Microsoft Edge", edge
	} else if strings.Contains(lower, "chromium/") {
		brand = "Chromium"
	}
	if len(match) != 2 {
		return
	}
	version := match[1]
	header.Set("Sec-Ch-Ua", fmt.Sprintf(`"%s";v="%s", "Chromium";v="%s", "Not(A:Brand";v="24"`, brand, version, version))

	platform := ""
	switch {
	case strings.Contains(lower, "windows"):
		platform = "Windows"
	// iPhone/iPad 的 UA 都含 "like Mac OS X"——必须先于 macOS 判定，
	// 否则移动设备报桌面平台，与 Sec-Ch-Ua-Mobile=?1 构成可检测的
	// 指纹矛盾（round 35 矩阵测试发现）。
	case strings.Contains(lower, "iphone") || strings.Contains(lower, "ipad"):
		platform = "iOS"
	case strings.Contains(lower, "mac os x") || strings.Contains(lower, "macintosh"):
		platform = "macOS"
	case strings.Contains(lower, "android"):
		platform = "Android"
	case strings.Contains(lower, "linux"):
		platform = "Linux"
	}
	header.Set("Sec-Ch-Ua-Mobile", "?0")
	if strings.Contains(lower, "mobile") || platform == "Android" || platform == "iOS" {
		header.Set("Sec-Ch-Ua-Mobile", "?1")
	}
	header.Set("Sec-Ch-Ua-Model", "")
	if platform != "" {
		header.Set("Sec-Ch-Ua-Platform", strconv.Quote(platform))
	}

	arch := ""
	switch {
	case strings.Contains(lower, "aarch64") || strings.Contains(lower, "arm64") || strings.Contains(lower, " arm"):
		arch = "arm"
	case strings.Contains(lower, "x86_64") || strings.Contains(lower, "x64") || strings.Contains(lower, "win64") || strings.Contains(lower, "intel"):
		arch = "x86"
	}
	if arch != "" {
		header.Set("Sec-Ch-Ua-Arch", arch)
		header.Set("Sec-Ch-Ua-Bitness", "64")
	}
}
