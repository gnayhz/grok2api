package rsc

import (
	"math"
	"testing"
)

// FuzzParseRisk asserts parse robustness on arbitrary homepage bodies: no
// panic, the verdict is always one of the four known values, and persisted
// numeric fields are always finite — a NaN/Inf risk score would poison GORM
// writes and json.Marshal downstream (json: unsupported value: NaN).
func FuzzParseRisk(f *testing.F) {
	f.Add("<html>ok</html>")
	f.Add("self.__next_f.push([1,\"{}\"])</script>")
	f.Add("botFlagSource\":2, botFlagDetails\":\"policy=deny,risk=0.9,event=x")
	f.Add("botFlagDetails\":\"risk=NaN\"")
	f.Add("botFlagDetails\":\"risk=Inf\"")
	f.Add("botFlagDetails\":\"risk=-Inf\"")
	f.Add("botFlagSource\":-1")
	f.Add("botFlagSource\":9999999999999999999")
	f.Add(string([]byte{0xff, 0xfe, 0x00}))
	f.Fuzz(func(t *testing.T, body string) {
		result := ParseRisk(body)
		switch result.Verdict {
		case VerdictClean, VerdictDenied, VerdictFlagged, VerdictError:
		default:
			t.Fatalf("verdict = %q", result.Verdict)
		}
		if math.IsNaN(result.RiskScore) || math.IsInf(result.RiskScore, 0) {
			t.Fatalf("non-finite risk score %v from body %q", result.RiskScore, body)
		}
	})
}
