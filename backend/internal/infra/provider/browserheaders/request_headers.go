package browserheaders

import "net/http"

// ApplyBrowserRequestHeaders 写入 Console 与 Grok Session 共用的浏览器请求头集合。
// userAgent 为空时不写 User-Agent；cookie、origin、referer 为空时对应头不写入，
// 调用方按方言补充差异头。Chromium Client Hints 与 User-Agent 保持一致。
func ApplyBrowserRequestHeaders(header http.Header, userAgent, cookie, origin, referer string) {
	header.Set("Accept", "*/*")
	header.Set("Accept-Encoding", "gzip, deflate, br, zstd")
	header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	header.Set("Cache-Control", "no-cache")
	if cookie != "" {
		header.Set("Cookie", cookie)
	}
	if origin != "" {
		header.Set("Origin", origin)
	}
	if referer != "" {
		header.Set("Referer", referer)
	}
	header.Set("Pragma", "no-cache")
	header.Set("Priority", "u=1, i")
	header.Set("Sec-Fetch-Dest", "empty")
	header.Set("Sec-Fetch-Mode", "cors")
	header.Set("Sec-Fetch-Site", "same-origin")
	if userAgent != "" {
		header.Set("User-Agent", userAgent)
	}
	ApplyChromiumClientHints(header, userAgent)
}
