package gateway

import "strings"

func sse(frames ...string) string {
	var b strings.Builder
	for _, frame := range frames {
		b.WriteString(frame)
		if !strings.HasSuffix(frame, "\n") {
			b.WriteByte('\n')
		}
		if !strings.HasSuffix(frame, "\n\n") {
			b.WriteByte('\n')
		}
	}
	return b.String()
}
