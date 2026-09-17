package media

import "time"

// OutputPollDelays 统一媒体产物轮询退避表(上游与网关共用)。
// application/gateway 的 waitVideoOutputRetry (video.go) 必须同样使用本表，
// 不得在本表之外另写一份 {200ms,750ms}。
var OutputPollDelays = [2]time.Duration{200 * time.Millisecond, 750 * time.Millisecond}

// OutputPollDelay 返回第 attempt 次重试(0 基)应等待的退避时长，越界取边界值。
func OutputPollDelay(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	if attempt >= len(OutputPollDelays) {
		attempt = len(OutputPollDelays) - 1
	}
	return OutputPollDelays[attempt]
}
