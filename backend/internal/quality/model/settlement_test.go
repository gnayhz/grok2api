package model

import "testing"

func TestCaseStatusForVerdict(t *testing.T) {
	cases := []struct {
		verdict Verdict
		want    CaseStatus
	}{
		{VerdictAccountGuilty, CaseAccountGuilty},
		{VerdictExitGuilty, CaseExitGuilty},
		{VerdictInsufficient, CaseDismissed},
	}
	for _, tc := range cases {
		got, err := CaseStatusForVerdict(tc.verdict)
		if err != nil || got != tc.want {
			t.Fatalf("CaseStatusForVerdict(%q) = %q, %v; want %q", tc.verdict, got, err, tc.want)
		}
	}
	if _, err := CaseStatusForVerdict(Verdict("rogue")); err == nil {
		t.Fatal("unknown verdict accepted")
	}
}

func TestReviewRelease(t *testing.T) {
	if s, id := ReviewReleaseAccount(7, 3); s != AccountSentenced || id != 7 {
		t.Fatalf("account holder: %q %d", s, id)
	}
	if s, id := ReviewReleaseAccount(0, 3); s != AccountRemanded || id != 3 {
		t.Fatalf("account no holder: %q %d", s, id)
	}
	if s, id := ReviewReleaseExit(9, 4); s != ExitBanned || id != 9 {
		t.Fatalf("exit holder: %q %d", s, id)
	}
	if s, id := ReviewReleaseExit(0, 4); s != ExitRemanded || id != 4 {
		t.Fatalf("exit no holder: %q %d", s, id)
	}
}
