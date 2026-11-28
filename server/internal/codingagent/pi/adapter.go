// Package pi is the Pi coding-agent runtime (ADR 0002) of the RU contour: `pi --mode rpc` (the pinned
// @earendil-works/pi-coding-agent) driven over JSONL on stdin and stdout and mapped into the closed event set of
// internal/codingagent. Pi runs only inside internal/isolate's bubblewrap jail, one process per run, and it never holds a
// provider key: its models.json (generated here per run) points at the host-side provider gateway
// (internal/providergw) on loopback, which adds the key from the server's environment. Everything is fail closed: no
// bwrap, no Node >= the lock's minimum, no pinned Pi, no provider key or no models means the agent is off, never another
// runtime or an unjailed Pi.
package pi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/buildytech/vibechat-desktop/server/internal/codingagent"
	"github.com/buildytech/vibechat-desktop/server/internal/isolate"
	"github.com/buildytech/vibechat-desktop/server/internal/providergw"
)

// Options builds an Adapter. The contour package fills it from the contour config.
type Options struct {
	// Label is the runtime's name in the Studio ("Pi · Yandex AI Studio").
	Label string
	// Provider is the Studio's provider id of the menu entries ("yandex", "gigachat").
	Provider string
	DataDir  string
	// Install is the located Node and Pi; LocateErr is why there is none.
	Install   Install
	LocateErr error
	// Gateway is the provider gateway of this server (started with the first run, closed with the adapter).
	Gateway *providergw.Gateway
	Models  []ModelEntry
	// Compat is added to the generated models.json compat block (per provider quirks).
	Compat map[string]any
	Log    func(string)

	// Test seams: the jail probe, the process grace period and a tap on every record Pi writes.
	Jail func() error
	// ToolJail probes the nested jail of the agent's shell (default isolate.NestedJailAvailable).
	ToolJail func() error
	Grace    time.Duration
	Tap      func(raw []byte)
}

// Adapter is the Pi runtime behind codingagent.Adapter.
type Adapter struct {
	o Options
}

var _ codingagent.Adapter = (*Adapter)(nil)

// New builds the adapter. Nothing is started and nothing is checked yet: Ready answers that.
func New(o Options) *Adapter {
	if o.Log == nil {
		o.Log = func(string) {}
	}
	if o.ToolJail == nil {
		o.ToolJail = isolate.NestedJailAvailable
	}
	if o.Jail == nil {
		o.Jail = isolate.JailAvailable
	}
	if o.Grace == 0 {
		o.Grace = 5 * time.Second
	}
	if o.Label == "" {
		o.Label = "Pi"
	}
	return &Adapter{o: o}
}

// Install is the located Node and Pi (zero when LocateErr says why not).
func (a *Adapter) Install() Install { return a.o.Install }

func (a *Adapter) Runtime() string { return codingagent.RuntimePi }

func (a *Adapter) Capabilities() codingagent.Capabilities {
	return codingagent.Capabilities{
		Runtime: codingagent.RuntimePi, Label: a.o.Label, ModelSource: codingagent.ModelSourceOwnKeys,
		Streaming: true, Tools: true, ShowThinking: true, ShowToolTrace: true,
	}
}

// Ready is nil only when a run can start safely now.
func (a *Adapter) Ready() error {
	if err := a.o.Jail(); err != nil {
		return fmt.Errorf("the Pi agent runs only in the bubblewrap jail and it is not available: %v", err)
	}
	if err := a.o.ToolJail(); err != nil {
		return fmt.Errorf("the Pi agent's shell needs a nested jail without network: %v", err)
	}
	if a.o.LocateErr != nil {
		return a.o.LocateErr
	}
	if a.o.Install.Node == "" || a.o.Install.Entry == "" {
		return errors.New("Pi is not installed")
	}
	if err := ValidateModels(a.o.Models); err != nil {
		return fmt.Errorf("contour config: %v", err)
	}
	if a.o.Gateway == nil {
		return errors.New("no model provider is configured for this server (contour config)")
	}
	return a.o.Gateway.Provider().Ready()
}

func (a *Adapter) Unsandboxed() bool { return false }

// Credential is nil: the provider keys live in the server's environment, not in a slot the Studio can fill.
func (a *Adapter) Credential() *codingagent.CredentialSpec { return nil }

// HasCredential: there is no per-user credential; a missing provider key is reported by Ready.
func (a *Adapter) HasCredential() bool { return true }

func (a *Adapter) Account(context.Context) (codingagent.Account, error) {
	return codingagent.Account{}, nil
}

func (a *Adapter) CheckCredential(context.Context, string) (codingagent.Account, error) {
	return codingagent.Account{}, errors.New("the Pi runtime keeps its provider keys in the server environment, not in Settings")
}

func (a *Adapter) Unauthenticated(error) bool { return false }

func (a *Adapter) Models(context.Context) ([]codingagent.Model, error) {
	return Menu(a.o.Provider, a.o.Models), nil
}

func (a *Adapter) DefaultModel() codingagent.Model {
	if m := Menu(a.o.Provider, a.o.Models); len(m) > 0 {
		return m[0]
	}
	return codingagent.Model{ID: a.o.Provider + "/none", Provider: a.o.Provider, Label: "No model configured"}
}

func (a *Adapter) entry(modelID string) (ModelEntry, bool) {
	id, ok := strings.CutPrefix(modelID, a.o.Provider+"/")
	if !ok {
		return ModelEntry{}, false
	}
	for _, m := range a.o.Models {
		if m.ID == id {
			return m, true
		}
	}
	return ModelEntry{}, false
}

func (a *Adapter) CheckModel(modelID string) string {
	if _, ok := a.entry(modelID); !ok {
		return fmt.Sprintf("Model %q is not in this server's model list", modelID)
	}
	return ""
}

// DropModel: the menu is the owner's config, nothing is dropped at run time.
func (a *Adapter) DropModel(string, error) string { return "" }

// CheckWorkspace: the workspace is the jail's working folder; project-local Pi files (.pi, extensions, skills) are not
// loaded (see args), so there is nothing to refuse but a folder that is not one.
func (a *Adapter) CheckWorkspace(cwd string) error {
	if st, err := os.Stat(cwd); err != nil || !st.IsDir() {
		return fmt.Errorf("the workspace %s is not a folder", cwd)
	}
	return nil
}

// EgressHosts are the only hostnames the contour's agent traffic may reach: the chosen provider's.
func (a *Adapter) EgressHosts() []string {
	if a.o.Gateway == nil {
		return nil
	}
	return a.o.Gateway.Provider().Hosts()
}

var handlePattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

func (a *Adapter) Open(_ context.Context, req codingagent.OpenRequest) (codingagent.Session, error) {
	m, ok := a.entry(req.ModelID)
	if !ok {
		return nil, &codingagent.OpenError{Phase: codingagent.PhaseModel, Err: fmt.Errorf("%w: %s", codingagent.ErrUnknownModel, req.ModelID)}
	}
	if err := a.Ready(); err != nil {
		return nil, &codingagent.OpenError{Phase: codingagent.PhaseSession, Err: err}
	}
	handle := req.Resume
	switch {
	case handle == "":
		var b [16]byte
		if _, err := rand.Read(b[:]); err != nil {
			return nil, &codingagent.OpenError{Phase: codingagent.PhaseSession, Err: err}
		}
		handle = hex.EncodeToString(b[:])
	case !handlePattern.MatchString(handle):
		// Not one of ours (a Cursor agent id): a Pi session cannot be resumed from it.
		return nil, &codingagent.OpenError{Phase: codingagent.PhaseSession, Err: fmt.Errorf("this chat belongs to another runtime (session %q)", clipID(handle))}
	}
	cwd, err := filepath.Abs(req.Cwd)
	if err != nil {
		return nil, &codingagent.OpenError{Phase: codingagent.PhaseSession, Err: err}
	}
	return &session{a: a, handle: handle, model: m, cwd: cwd}, nil
}

func clipID(s string) string {
	if len(s) > 12 {
		return s[:12] + "…"
	}
	return s
}

func (a *Adapter) Close() {
	if a.o.Gateway != nil {
		a.o.Gateway.Close()
	}
}

type session struct {
	a      *Adapter
	handle string
	model  ModelEntry
	cwd    string
}

func (s *session) Handle() string { return s.handle }
