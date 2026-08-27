package audit

import "testing"

// TestClassifyOutputSpeedTerminalBurst 锁定 2026-08-27 线上续聊链 7 连发
// 的确切形态：首字节时间==总时长（整包末尾一次到达）、零思考 token、输出
// 达降智最小口径。此前该形态 genMS=0 在速度列与一切速率档位里都隐形。
func TestClassifyOutputSpeedTerminalBurst(t *testing.T) {
	class, tps, genMS := ClassifyOutputSpeed(339, 0, 20362, 20362, DefaultDegradeSoftTPS, DefaultDegradeHardTPS, DefaultDegradeMinGenMS, false)
	if class != DegradeClassTerminalBurst || tps != 0 || genMS != 0 {
		t.Fatalf("terminal burst = %q tps=%v genMS=%d", class, tps, genMS)
	}
	class, _, _ = ClassifyOutputSpeed(339, 0, 21000, 20362, DefaultDegradeSoftTPS, DefaultDegradeHardTPS, DefaultDegradeMinGenMS, false)
	if class != DegradeClassTerminalBurst {
		t.Fatalf("dirty first>dur = %q", class)
	}
	class, _, _ = ClassifyOutputSpeed(8, 0, 1000, 1000, DefaultDegradeSoftTPS, DefaultDegradeHardTPS, DefaultDegradeMinGenMS, false)
	if class != "" {
		t.Fatalf("tiny output = %q", class)
	}
	class, _, _ = ClassifyOutputSpeed(339, 0, 0, 0, DefaultDegradeSoftTPS, DefaultDegradeHardTPS, DefaultDegradeMinGenMS, false)
	if class != "" {
		t.Fatalf("missing duration = %q", class)
	}
}

// TestClassifyOutputSpeedRateTiersUnchanged 锁定既有速率档位不受
// terminal_burst 分支影响：正常窗口仍按 soft/hard 阈值分级。
func TestClassifyOutputSpeedRateTiersUnchanged(t *testing.T) {
	cases := []struct {
		name       string
		out        int64
		reason     int64
		first      int64
		dur        int64
		failClosed bool
		want       string
	}{
		{"healthy", 80, 10, 250, 1250, false, ""},
		{"soft", 600, 0, 250, 1250, false, DegradeClassSoft},
		{"hard", 1300, 0, 250, 1250, false, DegradeClassHard},
		{"buffered", 600, 0, 1100, 1200, true, DegradeClassBurst},
	}
	for _, tc := range cases {
		class, _, genMS := ClassifyOutputSpeed(tc.out, tc.reason, tc.first, tc.dur, DefaultDegradeSoftTPS, DefaultDegradeHardTPS, DefaultDegradeMinGenMS, tc.failClosed)
		if class != tc.want {
			t.Fatalf("%s: class = %q want %q (genMS=%d)", tc.name, class, tc.want, genMS)
		}
	}
}
