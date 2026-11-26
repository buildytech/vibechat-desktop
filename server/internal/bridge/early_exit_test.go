//go:build !windows

package bridge

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A bridge that dies before its ready line: the error names the exit code and the last output lines, without the key.
func TestStartExplainsAnEarlyExit(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\necho \"stdout: starting with $CURSOR_API_KEY\"\necho 'Error: Cannot find module @cursor/sdk-win32-x64' >&2\nexit 3\n"
	if err := os.WriteFile(filepath.Join(dir, "bridge.sh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	var logged []string
	_, err := Start(context.Background(), StartOptions{
		Install: Install{Dir: dir, Manifest: Manifest{Entrypoint: "bridge.sh"}}, Key: "sk-secret-123",
		Home: filepath.Join(dir, "home"), StateRoot: filepath.Join(dir, "state"), Env: []string{"PATH=/usr/bin:/bin"},
		Log: func(l string) { logged = append(logged, l) }, ReadyTimeout: 10 * time.Second,
	})
	if err == nil {
		t.Fatal("no error")
	}
	msg := err.Error()
	for _, want := range []string{"exited before it was ready", "exit code 3", "Cannot find module", "stdout: starting with [redacted]"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error lacks %q: %s", want, msg)
		}
	}
	if strings.Contains(msg, "sk-secret-123") || strings.Contains(strings.Join(logged, "\n"), "sk-secret-123") {
		t.Fatal("key leaked")
	}
	if all := strings.Join(logged, "\n"); !strings.Contains(all, "bridge: cursor-sdk-bridge exited before it was ready (exit code 3)") {
		t.Errorf("the early exit is not in the log: %s", all)
	}
}

// A bridge runtime that needs a missing CPU feature dies with SIGILL (0xC000001D on Windows) and prints nothing.
func TestStartExplainsIllegalInstruction(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "bridge.sh"), []byte("#!/bin/sh\nkill -ILL $$\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	var logged []string
	_, err := Start(context.Background(), StartOptions{
		Install: Install{Dir: dir, Manifest: Manifest{Entrypoint: "bridge.sh"}}, Key: "k",
		Home: filepath.Join(dir, "home"), StateRoot: filepath.Join(dir, "state"), Env: []string{"PATH=/usr/bin:/bin"},
		Log: func(l string) { logged = append(logged, l) }, ReadyTimeout: 10 * time.Second,
	})
	if err == nil || !strings.Contains(err.Error(), "illegal instruction") || !strings.Contains(err.Error(), "baseline") {
		t.Fatalf("error: %v", err)
	}
	if !strings.Contains(strings.Join(logged, "\n"), "baseline") {
		t.Errorf("not logged: %q", logged)
	}
}
