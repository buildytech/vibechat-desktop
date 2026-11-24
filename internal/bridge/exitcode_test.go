package bridge

import (
	"strings"
	"testing"
)

// The owner's Windows report: "exit code 3221225501" must say what it is and what to do.
func TestDescribeCodeNTSTATUS(t *testing.T) {
	got := describeCode(3221225501)
	for _, want := range []string{"3221225501", "0xC000001D", "STATUS_ILLEGAL_INSTRUCTION", "baseline"} {
		if !strings.Contains(got, want) {
			t.Errorf("describeCode(0xC000001D) = %q, missing %q", got, want)
		}
	}
	if got := describeCode(3); got != "3" {
		t.Errorf("plain exit code: %q", got)
	}
	if got := describeCode(int(int32(-1073741510))); !strings.Contains(got, "0xC000013A") {
		t.Errorf("unknown NTSTATUS: %q", got)
	}
	if got := DescribeExit(nil); got != "unknown" {
		t.Errorf("nil state: %q", got)
	}
}
