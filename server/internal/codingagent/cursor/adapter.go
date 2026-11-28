package cursor

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/buildytech/vibechat-desktop/server/internal/codingagent"
	"github.com/buildytech/vibechat-desktop/server/internal/sandbox"
	"github.com/buildytech/vibechat-desktop/server/internal/secrets"
)

// Adapter is the Cursor SDK runtime behind the neutral codingagent.Adapter. It adds no behavior: every method is the
// Runner's own call, with Cursor's messages kept here instead of in the host.
type Adapter struct {
	r    *Runner
	keys codingagent.KeyStore
}

var _ codingagent.Adapter = (*Adapter)(nil)

// NewAdapter wraps a Runner; keys is the store the Runner reads the Cursor key from.
func NewAdapter(r *Runner, keys codingagent.KeyStore) *Adapter { return &Adapter{r: r, keys: keys} }

// Runner is the wrapped runner (doctor and the bridge smoke test use it directly).
func (a *Adapter) Runner() *Runner { return a.r }

func (a *Adapter) Runtime() string { return codingagent.RuntimeCursorSDK }

func (a *Adapter) Capabilities() codingagent.Capabilities {
	return codingagent.Capabilities{
		Runtime: codingagent.RuntimeCursorSDK, Label: "Cursor", ModelSource: codingagent.ModelSourceRuntime,
		Streaming: true, Tools: true, ShowThinking: true, ShowToolTrace: true,
	}
}

func (a *Adapter) Ready() error      { return a.r.SandboxError() }
func (a *Adapter) Unsandboxed() bool { return a.r.Unsandboxed() }
func (a *Adapter) HasCredential() bool {
	return a.r.HasKey()
}

func (a *Adapter) Credential() *codingagent.CredentialSpec {
	if a.keys == nil {
		return nil
	}
	return CredentialSpec(a.keys)
}

// CredentialSpec is the Cursor key slot over a key store.
func CredentialSpec(keys codingagent.KeyStore) *codingagent.CredentialSpec {
	return &codingagent.CredentialSpec{
		Provider: "cursor", Label: "Cursor", Store: keys,
		Missing: "Paste a Cursor API key: " + secrets.WhereToGet,
		Validate: func(key string) string {
			if err := secrets.CheckFormat(key); err != nil {
				return "Not saved: " + err.Error() + "."
			}
			if f := secrets.Foreign(key); f != "" {
				return "Not saved: this is a " + f + " key, not a key of type Cursor; " + secrets.WhereToGet + "."
			}
			return ""
		},
		NoCredential: ErrNoKey,
		AuthHint:     secrets.AuthHint,
		Describe:     secrets.Describe,
		Mask:         secrets.Mask,
	}
}

func account(a Account) codingagent.Account {
	return codingagent.Account{KeyName: a.APIKeyName, Email: a.UserEmail, FirstName: a.UserFirstName, LastName: a.UserLastName}
}

func (a *Adapter) Account(ctx context.Context) (codingagent.Account, error) {
	acc, err := a.r.Me(ctx)
	return account(acc), err
}

func (a *Adapter) CheckCredential(ctx context.Context, key string) (codingagent.Account, error) {
	acc, err := a.r.CheckKey(ctx, key)
	return account(acc), err
}

func (a *Adapter) Unauthenticated(err error) bool { return IsUnauthenticated(err) }

func (a *Adapter) Models(ctx context.Context) ([]codingagent.Model, error) { return a.r.Models(ctx) }

func (a *Adapter) DefaultModel() codingagent.Model {
	return codingagent.Model{ID: AutoID, Provider: "cursor", Label: "Auto"}
}

func (a *Adapter) CheckModel(modelID string) string { return CheckModelID(modelID) }

// CheckModelID refuses a model id that is not a Cursor one ("" = fine).
func CheckModelID(modelID string) string {
	if !strings.HasPrefix(modelID, "cursor/") || len(modelID) > 128 {
		return fmt.Sprintf("Model %q is not a Cursor model", modelID)
	}
	return ""
}

func (a *Adapter) DropModel(modelID string, err error) string {
	if a.r.MarkUnavailable(modelID, err) {
		return DroppedNote
	}
	return ""
}

// DroppedNote is what the user is told after a model was dropped from the key's menu.
const DroppedNote = "\n\nThis model is not available for your Cursor key and was removed from the model list; pick another one."

// CheckWorkspace refuses a workspace whose .cursor/hooks.json could change the agent's sandbox or hooks.
func (a *Adapter) CheckWorkspace(cwd string) error { return sandbox.CheckWorkspace(cwd) }

func (a *Adapter) Open(ctx context.Context, req codingagent.OpenRequest) (codingagent.Session, error) {
	model, err := a.r.Select(ctx, req.ModelID, req.ModelParams)
	if err != nil {
		return nil, &codingagent.OpenError{Phase: codingagent.PhaseModel, Err: err}
	}
	id, err := a.r.EnsureAgent(ctx, req.Resume, req.Cwd, model)
	if err != nil {
		return nil, &codingagent.OpenError{Phase: codingagent.PhaseSession, Err: err}
	}
	return &session{a: a, id: id, model: model, cwd: req.Cwd}, nil
}

func (a *Adapter) Close() { a.r.Close() }

type session struct {
	a     *Adapter
	id    string
	model Selection
	cwd   string
}

func (s *session) Handle() string { return s.id }

func (s *session) Start(ctx context.Context, sessionID, runID string, p codingagent.Prompt, emit func(codingagent.Event)) codingagent.Run {
	r := &run{s: s, tr: &Translator{SessionID: sessionID, RunID: runID, Cwd: s.cwd}, done: make(chan struct{})}
	go func() {
		defer close(r.done)
		r.err = s.a.r.Send(ctx, s.id, p.Text, p.Images, s.model, r.tr, emit)
	}()
	return r
}

type run struct {
	s    *session
	tr   *Translator
	done chan struct{}
	err  error
}

func (r *run) Cancel(ctx context.Context) error {
	if id := r.tr.Bridge(); id != "" {
		return r.s.a.r.Cancel(ctx, r.s.id, id)
	}
	return nil
}

func (r *run) Wait() (codingagent.Result, error) {
	<-r.done
	t := r.tr
	t.mu.Lock()
	defer t.mu.Unlock()
	files := make([]string, 0, len(t.Files))
	for p := range t.Files {
		files = append(files, p)
	}
	sort.Strings(files)
	res := codingagent.Result{Text: t.Text.String(), Summary: t.ResultText, Error: t.Error, ToolCalls: t.ToolCalls, Files: files}
	if t.Usage != nil {
		u := *t.Usage
		res.Usage = &u
	}
	return res, r.err
}
