package host

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/buildytech/vibechat-desktop/server/internal/bridge"
	"github.com/buildytech/vibechat-desktop/server/internal/codingagent"
	"github.com/buildytech/vibechat-desktop/server/internal/codingagent/cursor"
	"github.com/buildytech/vibechat-desktop/server/internal/sandbox"
	"github.com/buildytech/vibechat-desktop/server/internal/store"
	"github.com/buildytech/vibechat-desktop/server/internal/templates"
)

// fakeAgent stands in for the Cursor runtime (cursor.Adapter): a run writes a file named by the prompt into the session
// folder and streams a few contract events, so routes, worktrees and the merge back are tested without the bridge.
// It keeps Cursor's messages (the goldens pin them) by using the same spec and checks as the real adapter.
type fakeAgent struct {
	mu      sync.Mutex
	sandbox error
	key     bool
	keys    codingagent.KeyStore
	// unsandboxed: agents run without the SDK sandbox (the Windows host).
	unsandboxed bool
	cwds        []string
	cancelled   []string
}

var _ codingagent.Adapter = (*fakeAgent)(nil)

func (f *fakeAgent) Runtime() string                        { return codingagent.RuntimeCursorSDK }
func (f *fakeAgent) Capabilities() codingagent.Capabilities { return codingagent.LegacyCapabilities }
func (f *fakeAgent) Ready() error                           { return f.sandbox }
func (f *fakeAgent) Unsandboxed() bool                      { return f.unsandboxed }
func (f *fakeAgent) HasCredential() bool                    { return f.key }
func (f *fakeAgent) Credential() *codingagent.CredentialSpec {
	return cursor.CredentialSpec(f.keys)
}
func (f *fakeAgent) Account(context.Context) (codingagent.Account, error) {
	return codingagent.Account{KeyName: "test", Email: "a@b.c"}, nil
}
func (f *fakeAgent) CheckCredential(_ context.Context, key string) (codingagent.Account, error) {
	if strings.HasPrefix(key, "grok_bad") {
		return codingagent.Account{}, &bridge.Error{Code: "unauthenticated", Message: "Invalid Service Token"}
	}
	if strings.HasPrefix(key, "grok_offline") {
		return codingagent.Account{}, errors.New("dial tcp: network is unreachable")
	}
	return codingagent.Account{KeyName: "test", Email: "a@b.c"}, nil
}
func (f *fakeAgent) Unauthenticated(err error) bool { return cursor.IsUnauthenticated(err) }
func (f *fakeAgent) Models(context.Context) ([]codingagent.Model, error) {
	return []codingagent.Model{{ID: cursor.AutoID, Provider: "cursor", Label: "Auto"}, {ID: "cursor/composer-2.5-fast", Provider: "cursor", Label: "Composer 2.5 Fast"}}, nil
}
func (f *fakeAgent) DefaultModel() codingagent.Model { return codingagent.LegacyDefaultModel }
func (f *fakeAgent) CheckModel(id string) string     { return cursor.CheckModelID(id) }
func (f *fakeAgent) DropModel(id string, err error) string {
	if cursor.ModelAccessError(err) {
		return cursor.DroppedNote
	}
	return ""
}
func (f *fakeAgent) CheckWorkspace(cwd string) error { return sandbox.CheckWorkspace(cwd) }
func (f *fakeAgent) Close()                          {}
func (f *fakeAgent) Open(_ context.Context, req codingagent.OpenRequest) (codingagent.Session, error) {
	if req.ModelID == "cursor/gone" {
		return nil, &codingagent.OpenError{Phase: codingagent.PhaseModel, Err: cursor.ErrUnknownModel}
	}
	f.mu.Lock()
	f.cwds = append(f.cwds, req.Cwd)
	f.mu.Unlock()
	h := req.Resume
	if h == "" {
		h = "agent-" + filepath.Base(req.Cwd)
	}
	return &fakeSession{f: f, handle: h, cwd: req.Cwd}, nil
}

type fakeSession struct {
	f      *fakeAgent
	handle string
	cwd    string
}

func (s *fakeSession) Handle() string { return s.handle }
func (s *fakeSession) Start(_ context.Context, sessionID, runID string, p codingagent.Prompt, emit func(codingagent.Event)) codingagent.Run {
	r := &fakeRun{f: s.f, runID: runID}
	switch {
	case p.Text == "fail":
		r.err = errors.New("boom")
	default:
		if err := os.WriteFile(filepath.Join(s.cwd, p.Text+".txt"), []byte(p.Text), 0o644); err != nil {
			r.err = err
			break
		}
		r.res.Text = "wrote " + p.Text
		emit(codingagent.MessageDelta{Ref: codingagent.Ref{SessionID: sessionID, RunID: runID}, Text: "wrote " + p.Text})
	}
	return r
}

type fakeRun struct {
	f     *fakeAgent
	runID string
	res   codingagent.Result
	err   error
}

func (r *fakeRun) Wait() (codingagent.Result, error) { return r.res, r.err }
func (r *fakeRun) Cancel(context.Context) error {
	r.f.mu.Lock()
	r.f.cancelled = append(r.f.cancelled, r.runID)
	r.f.mu.Unlock()
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
	return newEnvAgents(t, s, worktrees, "", nil)
}

// newEnvAgents builds the host with the Cursor fake plus extra runtimes; def is the runtime of new chats ("" = cursor-sdk).
// withoutCursor leaves the Cursor fake out (a contour that has only another runtime).
func newEnvAgents(t *testing.T, s Seam, worktrees bool, def string, extra []codingagent.Adapter, withoutCursor ...bool) testEnv {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	keys := &memKeys{}
	fa := &fakeAgent{key: true, keys: keys}
	all := append([]codingagent.Adapter{fa}, extra...)
	if len(withoutCursor) > 0 && withoutCursor[0] {
		all = extra
	}
	reg, err := codingagent.NewRegistry(def, all...)
	if err != nil {
		t.Fatal(err)
	}
	o := Options{Seam: s, Version: "test", Store: st, Agents: reg, Templates: []templates.Profile{miniTemplate},
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
