//go:build live

package bridge

// Live auth check against a real bridge and the Cursor API (Me, ListModels, one tiny Composer 2.5 Fast run):
// CURSOR_API_KEY=… VIBECHAT_BRIDGE_DIR=… VIBECHAT_HOOK_EXE=… go test -tags live -run TestLiveAuth ./internal/bridge
// The key comes only from the environment; it is never logged (Redact) nor put in argv or files.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/buildytech/vibechat-desktop/server/internal/sandbox"
)

func TestLiveAuth(t *testing.T) {
	key := os.Getenv("CURSOR_API_KEY")
	hookExe := os.Getenv("VIBECHAT_HOOK_EXE")
	if key == "" || hookExe == "" || os.Getenv("VIBECHAT_BRIDGE_DIR") == "" {
		t.Skip("live auth needs CURSOR_API_KEY, VIBECHAT_HOOK_EXE, VIBECHAT_BRIDGE_DIR")
	}
	inst, err := Locate("/nonexistent/x", os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("bridge %s runtime %s", inst.Manifest.BridgeVersion, inst.Manifest.Runtime)
	base := t.TempDir()
	home := filepath.Join(base, "agent-home")
	ws := filepath.Join(base, "ws")
	_ = os.MkdirAll(ws, 0o755)
	if err := sandbox.WriteHome(home, hookExe, nil); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	p, err := Start(ctx, StartOptions{Install: inst, Key: key, Home: home, StateRoot: filepath.Join(base, "state"), Log: func(l string) { t.Log("bridge:", l) }})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	var me map[string]any
	if err := p.Unary(ctx, "SdkCursorService", "Me", map[string]any{"options": map[string]any{"apiKey": key}}, &me); err != nil {
		t.Fatalf("Me: %s", Redact(err.Error(), key))
	}
	var rawModels json.RawMessage
	if err := p.Unary(ctx, "SdkCursorService", "ListModels", map[string]any{"options": map[string]any{"apiKey": key}}, &rawModels); err == nil && os.Getenv("VIBECHAT_LIVE_MODELS_OUT") != "" {
		_ = os.WriteFile(os.Getenv("VIBECHAT_LIVE_MODELS_OUT"), []byte(Redact(string(rawModels), key)), 0o600)
	}
	var models struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	if err := p.Unary(ctx, "SdkCursorService", "ListModels", map[string]any{"options": map[string]any{"apiKey": key}}, &models); err != nil {
		t.Fatalf("ListModels: %s", Redact(err.Error(), key))
	}
	var ids []string
	for _, m := range models.Items {
		ids = append(ids, m.ID)
	}
	t.Logf("models: %v", ids)
	// The owner's pick: Composer 2.5 Fast (agent.ToSelection("cursor/composer-2.5-fast")).
	model := map[string]any{"id": "composer-2.5", "params": []map[string]string{{"id": "fast", "value": "true"}}}
	if id := os.Getenv("VIBECHAT_LIVE_MODEL"); id != "" {
		model = map[string]any{"id": id}
	}
	var created struct {
		AgentID string `json:"agentId"`
	}
	err = p.Unary(ctx, "SdkAgentService", "CreateAgent", map[string]any{"options": map[string]any{
		"model": model,
		"local": map[string]any{"cwd": []string{ws}, "settingSources": []string{"SETTING_SOURCE_PROJECT", "SETTING_SOURCE_USER"}, "sandboxOptions": map[string]any{"enabled": true}},
	}}, &created)
	if err != nil {
		t.Fatalf("CreateAgent: %s", Redact(err.Error(), key))
	}
	var text strings.Builder
	err = p.Stream(ctx, "SdkAgentService", "Send", map[string]any{"agentId": created.AgentID, "message": map[string]any{"text": "Reply with the single word OK."}, "options": map[string]any{"model": model}}, func(m json.RawMessage) error {
		text.WriteString(Redact(string(m), key))
		return nil
	})
	if err != nil {
		t.Fatalf("Send: %s", Redact(err.Error(), key))
	}
	if s := text.String(); strings.Contains(s, "unauthenticated") || !strings.Contains(s, "OK") {
		t.Fatalf("run did not answer OK: %.600s", s)
	}
}
