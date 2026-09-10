package console

import "io"

func benchmarkVoiceBodyRead(body io.Reader, limit int64) ([]byte, error) {
	return readConsoleVoiceBody(body, limit)
}
