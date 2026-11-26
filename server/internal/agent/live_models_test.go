//go:build live

package agent

// Live: the menu from the key's model list and a short run on menu entries, exactly the app's path (Runner.Models →
// Select → EnsureAgent → Send). Costs a few tiny model calls.
// CURSOR_API_KEY=… VIBECHAT_BRIDGE_DIR=… VIBECHAT_HOOK_EXE=… [VIBECHAT_LIVE_MENU="cursor/composer-2.5-fast,…"]
//   go test -tags live -run TestLiveMenuRuns ./internal/agent

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/buildytech/vibechat-desktop/server/internal/bridge"
)

type envKey string

func (k envKey) Key() (string, string) { return string(k), "env" }

func TestLiveMenuRuns(t *testing.T) {
	key, hook := os.Getenv("CURSOR_API_KEY"), os.Getenv("VIBECHAT_HOOK_EXE")
	if key == "" || hook == "" || os.Getenv("VIBECHAT_BRIDGE_DIR") == "" {
		t.Skip("needs CURSOR_API_KEY, VIBECHAT_HOOK_EXE, VIBECHAT_BRIDGE_DIR")
	}
	inst, err := bridge.Locate("/nonexistent/x", os.Getenv)
	r := NewRunner(RunnerOptions{Install: inst, Located: err, Keys: envKey(key), DataDir: t.TempDir(), HookExe: hook,
		Log: func(s string) { t.Log(bridge.Redact(s, key)) }})
	defer r.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	menu, err := r.Models(ctx)
	if err != nil {
		t.Fatalf("Models: %s", bridge.Redact(err.Error(), key))
	}
	t.Logf("menu: %d entries", len(menu))
	ids := strings.Split(os.Getenv("VIBECHAT_LIVE_MENU"), ",")
	if ids[0] == "" {
		ids = []string{"cursor/composer-2.5-fast", "cursor/claude-opus-5", AutoID}
	}
	for _, id := range ids {
		sel, err := r.Select(ctx, id, nil)
		if err != nil {
			t.Errorf("%s: Select: %v", id, err)
			continue
		}
		ws := t.TempDir()
		agentID, err := r.EnsureAgent(ctx, "", ws, sel)
		if err != nil {
			t.Errorf("%s %+v: create: %s", id, sel, bridge.Redact(err.Error(), key))
			continue
		}
		tr := &Translator{SessionID: "live", RunID: "live-" + id, Cwd: ws}
		err = r.Send(ctx, agentID, "Reply with the single word OK.", nil, sel, tr, func(Event) {})
		answer := strings.TrimSpace(tr.Text.String() + tr.ResultText)
		t.Logf("%s %+v: status %s, answer %.80q, err %v, tr.Error %q", id, sel, tr.Status, answer, err, tr.Error)
		if err != nil || tr.Error != "" || !strings.Contains(strings.ToUpper(answer), "OK") {
			failure := err
			if failure == nil && tr.Error != "" {
				failure = errors.New(tr.Error)
			}
			if r.MarkUnavailable(id, failure) {
				t.Logf("%s: not available for this key, removed from the menu", id)
				menu2, _ := r.Models(ctx)
				for _, m := range menu2 {
					if m.ID == id {
						t.Errorf("%s still in the menu", id)
					}
				}
				continue
			}
			t.Errorf("%s: no OK answer", id)
		}
	}
}
