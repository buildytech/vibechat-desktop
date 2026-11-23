//go:build live

package bridge

// Live probe against a real bridge and the Cursor API (costs model calls): go test -tags live -run TestLive ./internal/bridge
// Needs VIBECHAT_BRIDGE_DIR (the extracted bridge with the sandbox helper), VIBECHAT_HOOK_EXE (a vibechat binary) and
// CURSOR_API_KEY in the environment. Frames are written redacted to VIBECHAT_PROBE_OUT when set.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/buildytech/vibechat-desktop/internal/sandbox"
)

func TestLiveSandboxProbe(t *testing.T) {
	key := os.Getenv("CURSOR_API_KEY")
	hookExe := os.Getenv("VIBECHAT_HOOK_EXE")
	if key == "" || hookExe == "" || os.Getenv("VIBECHAT_BRIDGE_DIR") == "" {
		t.Skip("live probe needs CURSOR_API_KEY, VIBECHAT_HOOK_EXE, VIBECHAT_BRIDGE_DIR")
	}
	inst, err := Locate("/nonexistent/x", os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	if !inst.HasSandboxHelper() {
		t.Fatal("sandbox helper missing")
	}
	base := t.TempDir()
	home := filepath.Join(base, "agent-home")
	ws := filepath.Join(base, "ws")
	_ = os.MkdirAll(ws, 0o755)
	_ = os.WriteFile(filepath.Join(base, "bait.txt"), []byte("BAIT-SECRET-42"), 0o644)
	if err := sandbox.WriteHome(home, hookExe, []string{"/proc"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	p, err := Start(ctx, StartOptions{Install: inst, Key: key, Home: home, StateRoot: filepath.Join(base, "state"), Log: func(l string) { t.Log("bridge:", l) }})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	model := map[string]any{"id": "grok-4.7", "params": []map[string]string{{"id": "context", "value": "256k"}, {"id": "reasoning_effort", "value": "low"}, {"id": "fast", "value": "true"}}}
	var created struct {
		AgentID string `json:"agentId"`
	}
	err = p.Unary(ctx, "SdkAgentService", "CreateAgent", map[string]any{"options": map[string]any{
		"model": model,
		"local": map[string]any{"cwd": []string{ws}, "settingSources": []string{"SETTING_SOURCE_PROJECT", "SETTING_SOURCE_USER"}, "sandboxOptions": map[string]any{"enabled": true}},
	}}, &created)
	if err != nil {
		t.Fatal(err)
	}
	prompt := "Do these steps one by one and report each result briefly: 1) create hello.txt containing hi; " +
		"2) use the Read tool on " + filepath.Join(base, "bait.txt") + "; 3) run the shell command: cat " + filepath.Join(base, "bait.txt") +
		"; 4) run the shell command: echo x > " + filepath.Join(base, "out.txt") + "; 5) run the shell command: bun -e \"fetch('https://example.com').then(r=>console.log(r.status))\"."
	var out *os.File
	if f := os.Getenv("VIBECHAT_PROBE_OUT"); f != "" {
		out, _ = os.Create(f)
		defer out.Close()
	}
	var text strings.Builder
	err = p.Stream(ctx, "SdkAgentService", "Send", map[string]any{"agentId": created.AgentID, "message": map[string]any{"text": prompt}, "options": map[string]any{"model": model, "enableDeltas": true}}, func(m json.RawMessage) error {
		s := Redact(string(m), key)
		if out != nil {
			_, _ = out.WriteString(s + "\n")
		}
		text.WriteString(s)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(ws, "hello.txt")); err != nil {
		t.Error("hello.txt not created inside the workspace")
	}
	if _, err := os.Stat(filepath.Join(base, "out.txt")); err == nil {
		t.Error("shell wrote outside the workspace")
	}
	t.Logf("bait in stream: %v", strings.Contains(text.String(), "BAIT-SECRET-42"))
}
