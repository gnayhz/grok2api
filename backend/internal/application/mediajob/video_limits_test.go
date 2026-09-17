package mediajob

import "testing"

func TestVideoPhysicalCallLimitIsHardCeiling(t *testing.T) {
	if VideoPhysicalCallLimit != 8192 {
		t.Fatalf("physical call limit = %d", VideoPhysicalCallLimit)
	}
}
