package audit

import "testing"

// TestClassifyTerminalBurst 锁定线上续聊链连续多发的确切形态：
// 首字节时间==总时长（整包末尾一次到达）、零思考 token、输出达降智最小
// 口径。此前该形态 genMS=0 在速度列与一切速率档位里都隐形。
func TestClassifyTerminalBurst(t *testing.T) {
	if !ClassifyTerminalBurst(339, 0, 20362, 20362) {
		t.Fatal("first==dur terminal burst must classify")
	}
	if !ClassifyTerminalBurst(339, 0, 21000, 20362) {
		t.Fatal("dirty first>dur terminal burst must classify")
	}
	if ClassifyTerminalBurst(8, 0, 1000, 1000) {
		t.Fatal("tiny output below sighting threshold must not classify")
	}
	if ClassifyTerminalBurst(339, 0, 0, 0) {
		t.Fatal("missing duration must not classify")
	}
	// 正常窗口（首字节远早于总时长）不是 terminal burst——速度列照常展示。
	if ClassifyTerminalBurst(80, 10, 250, 1250) {
		t.Fatal("normal generation window must not classify")
	}
}
