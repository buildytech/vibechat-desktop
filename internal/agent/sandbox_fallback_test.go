package agent

import (
	"errors"
	"runtime"
	"strings"
	"testing"

	"github.com/buildytech/vibechat-desktop/internal/bridge"
)

func sandboxFlag(opts map[string]any) bool {
	return opts["local"].(map[string]any)["sandboxOptions"].(map[string]any)["enabled"].(bool)
}

// The owner's Windows run: the bridge refuses a sandboxed agent; the runner retries once without the sandbox
// (explicit enabled=false) and keeps that for the process.
func TestSandboxRefusedRetriesOnceWithout(t *testing.T) {
	var logged []string
	r := &Runner{o: RunnerOptions{Log: func(s string) { logged = append(logged, s) }}}
	refusal := &bridge.Error{Code: "invalid_argument", Message: "Local SDK sandboxing was requested, but sandboxing is not supported in this environment. Disable local.sandboxOptions.enabled or remove ~/.cursor/sandbox.json to run without sandboxing."}
	var seen []bool
	err := r.withSandboxFallback("/ws", Selection{ID: "default"}, func(opts map[string]any) error {
		seen = append(seen, sandboxFlag(opts))
		if sandboxFlag(opts) {
			return refusal
		}
		return nil
	})
	if err != nil || len(seen) != 2 || !seen[0] || seen[1] {
		t.Fatalf("err %v, calls %v", err, seen)
	}
	if !strings.Contains(strings.Join(logged, "\n"), "without it") {
		t.Errorf("not logged: %q", logged)
	}
	seen = nil
	_ = r.withSandboxFallback("/ws", Selection{ID: "default"}, func(opts map[string]any) error {
		seen = append(seen, sandboxFlag(opts))
		return nil
	})
	if len(seen) != 1 || seen[0] {
		t.Fatalf("the next agent must start without the sandbox at once: %v", seen)
	}
}

func TestOtherErrorsAreNotRetriedWithoutSandbox(t *testing.T) {
	r := &Runner{}
	calls := 0
	err := r.withSandboxFallback("/ws", Selection{ID: "default"}, func(map[string]any) error {
		calls++
		return errors.New("bridge: unauthenticated: Invalid Service Token")
	})
	if err == nil || calls != 1 || !r.sandboxed() {
		t.Fatalf("err %v, calls %d, sandboxed %v", err, calls, r.sandboxed())
	}
}

func TestWindowsStartsWithoutSandbox(t *testing.T) {
	r := NewRunner(RunnerOptions{Located: errors.New("no bridge in this test")})
	if r.Unsandboxed() != (runtime.GOOS == "windows") {
		t.Fatalf("Unsandboxed %v on %s", r.Unsandboxed(), runtime.GOOS)
	}
	if want := runtime.GOOS != "windows"; r.sandboxed() != want {
		t.Fatalf("sandboxed %v on %s", r.sandboxed(), runtime.GOOS)
	}
}
