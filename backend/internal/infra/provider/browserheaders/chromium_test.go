package browserheaders

import (
	"net/http"
	"strings"
	"testing"
)

// TestApplyChromiumClientHintsMatrix locks the client-hint generation matrix (round 35:
// the package had zero tests; only console covered the non-Chromium skip).
func TestApplyChromiumClientHintsMatrix(t *testing.T) {
	cases := []struct {
		name string
		ua   string
		want map[string]string
		skip map[string]bool
	}{
		{
			name: "chrome windows",
			ua:   "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36",
			want: map[string]string{
				"Sec-Ch-Ua":          `"Google Chrome";v="125"`,
				"Sec-Ch-Ua-Platform": `"Windows"`,
				"Sec-Ch-Ua-Arch":     "x86",
				"Sec-Ch-Ua-Mobile":   "?0",
			},
		},
		{
			name: "edge mac",
			ua:   "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36 Edg/124.0.2478.51",
			want: map[string]string{
				"Sec-Ch-Ua":          `"Microsoft Edge";v="124"`,
				"Sec-Ch-Ua-Platform": `"macOS"`,
			},
		},
		{
			name: "chromium linux",
			ua:   "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/123.0.0.0 Chromium/123.0.6312.86 Safari/537.36",
			want: map[string]string{
				"Sec-Ch-Ua": `"Chromium";v="123"`,
			},
		},
		{
			name: "android mobile",
			ua:   "Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Mobile Safari/537.36",
			want: map[string]string{
				"Sec-Ch-Ua-Mobile":   "?1",
				"Sec-Ch-Ua-Platform": `"Android"`,
			},
			// Android UA has no arch token: Sec-Ch-Ua-Arch stays unset by design (literal extraction only).
			skip: map[string]bool{"Sec-Ch-Ua-Arch": true},
		},
		{
			name: "iphone ios",
			ua:   "Mozilla/5.0 (iPhone; CPU iPhone OS 17_4 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) CriOS/126.0.6478.54 Mobile/15E148 Safari/604.1",
			want: map[string]string{
				"Sec-Ch-Ua":          `"Google Chrome";v="126"`,
				"Sec-Ch-Ua-Mobile":   "?1",
				"Sec-Ch-Ua-Platform": `"iOS"`,
			},
		},
		{
			name: "firefox skipped",
			ua:   "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:126.0) Gecko/20100101 Firefox/126.0",
			skip: map[string]bool{
				"Sec-Ch-Ua":          true,
				"Sec-Ch-Ua-Platform": true,
			},
		},
		{
			name: "safari skipped",
			ua:   "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.4 Safari/605.1.15",
			skip: map[string]bool{
				"Sec-Ch-Ua": true,
			},
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			header := http.Header{}
			ApplyChromiumClientHints(header, tc.ua)
			for key, substr := range tc.want {
				got := header.Get(key)
				if got == "" {
					t.Fatalf("%s missing", key)
				}
				if substr != "" && !strings.Contains(got, substr) {
					t.Fatalf("%s = %q want substr %q", key, got, substr)
				}
			}
			for key := range tc.skip {
				if header.Get(key) != "" {
					t.Fatalf("%s should be absent, got %q", key, header.Get(key))
				}
			}
		})
	}
}
