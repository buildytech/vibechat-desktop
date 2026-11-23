package host

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/buildytech/vibechat-desktop/internal/agent"
	"github.com/buildytech/vibechat-desktop/internal/store"
	"github.com/buildytech/vibechat-desktop/internal/templates"
)

// fakeAgent stands in for agent.Runner: Send writes a file named by the prompt into the session folder and streams a
// few contract events, so routes, worktrees and the merge back are tested without the bridge.
type fakeAgent struct {
	mu        sync.Mutex
	sandbox   error
	key       bool
	cwds      []string
	cancelled []string
}

func (f *fakeAgent) SandboxError() error { return f.sandbox }
func (f *fakeAgent) HasKey() bool        { return f.key }
func (f *fakeAgent) Me(context.Context) (agent.Account, error) {
	return agent.Account{APIKeyName: "test", UserEmail: "a@b.c"}, nil
}
func (f *fakeAgent) ModelIDs(context.Context) ([]string, error) {
	return []string{"grok-4.7", "composer-2.5"}, nil
}
func (f *fakeAgent) EnsureAgent(_ context.Context, id, cwd string, _ agent.Selection) (string, error) {
	f.mu.Lock()
	f.cwds = append(f.cwds, cwd)
	f.mu.Unlock()
	if id != "" {
		return id, nil
	}
	return "agent-" + filepath.Base(cwd), nil
}
func (f *fakeAgent) Send(_ context.Context, _ string, text string, _ []agent.Image, _ agent.Selection, tr *agent.Translator, emit func(agent.Event)) error {
	if text == "fail" {
		return errors.New("boom")
	}
	if err := os.WriteFile(filepath.Join(tr.Cwd, text+".txt"), []byte(text), 0o644); err != nil {
		return err
	}
	tr.Text.WriteString("wrote " + text)
	emit(agent.Event{"type": "message.delta", "sessionId": tr.SessionID, "role": "assistant", "text": "wrote " + text, "runId": tr.RunID})
	tr.Status = "RUN_LIFECYCLE_STATUS_FINISHED"
	return nil
}
func (f *fakeAgent) Cancel(_ context.Context, _, runID string) error {
	f.mu.Lock()
	f.cancelled = append(f.cancelled, runID)
	f.mu.Unlock()
	return nil
}

var miniTemplate = templates.Profile{
	ID: "mini", Name: "Mini", Stack: "Static", Description: "test", Enabled: true,
	Files: fstest.MapFS{
		"index.html":   {Data: []byte("<!doctype html><h1>hi</h1>")},
		"package.json": {Data: []byte(`{"name":"mini","scripts":{"build":"true"}}`)},
		"AGENTS.md":    {Data: []byte("# rules")},
	},
}

type testEnv struct {
	host  *Host
	agent *fakeAgent
	dir   string
}

func newEnv(t *testing.T, s Seam, worktrees bool) testEnv {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	fa := &fakeAgent{key: true}
	o := Options{Seam: s, Version: "test", Store: st, Agent: fa, Keys: &memKeys{}, Templates: []templates.Profile{miniTemplate},
		ProjectsDir: filepath.Join(dir, "projects")}
	if worktrees {
		o.WorktreesDir = filepath.Join(dir, "worktrees")
	}
	return testEnv{host: New(o), agent: fa, dir: dir}
}

func testHost(t *testing.T, s Seam) *Host { return newEnv(t, s, true).host }

type memKeys struct{ k string }

func (m *memKeys) Key() (string, string) {
	if m.k == "" {
		return "", ""
	}
	return m.k, "keychain"
}
func (m *memKeys) Store(k string) error { m.k = k; return nil }
func (m *memKeys) Forget() error        { m.k = ""; return nil }
