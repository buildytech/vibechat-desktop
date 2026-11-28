// Package codingagent is the neutral seam between the host and a coding-agent runtime (ADR 0002).
//
// A runtime is whatever does the coding work for one chat: the Cursor SDK (package cursor, behind a bridge process) in
// the EU and US contours, Pi (RPC, inside internal/isolate) in the RU contour. Both are first class: the host knows
// only Adapter, Session and Run, the closed set of events below, and Capabilities. Which runtime serves a server is its
// contour config; a chat remembers its runtime (store.Session.Runtime, empty = cursor-sdk). Nothing here names a vendor
// except the runtime ids.
package codingagent

import (
	"context"
	"errors"
	"fmt"
	"sort"
)

// Runtime ids (also the "runtime" of the seam: sessions, credentials slots, agent capabilities).
const (
	RuntimeCursorSDK = "cursor-sdk"
	RuntimePi        = "pi"
)

// RuntimeOf is the runtime of a stored chat: chats made before runtimes existed have none and are Cursor chats.
func RuntimeOf(stored string) string {
	if stored == "" {
		return RuntimeCursorSDK
	}
	return stored
}

// Where the models of a runtime come from.
const (
	// ModelSourceRuntime: the runtime's own account lists and bills them (Cursor: the key's model menu).
	ModelSourceRuntime = "runtime"
	// ModelSourceOwnKeys: the owner's provider keys (Pi: GigaChat, Yandex AI Studio, ...) through a models config.
	ModelSourceOwnKeys = "own-keys"
)

// Capabilities describe a runtime to the Studio and the host. ModelSource is for the host (Settings, egress rules); it
// is not on the seam's wire in this version.
type Capabilities struct {
	Runtime       string
	Label         string
	ModelSource   string
	Streaming     bool
	Tools         bool
	PTY           bool
	Snapshots     bool
	CostUsage     bool
	EvidencePack  bool
	Subagents     bool
	ShowThinking  bool
	ShowToolTrace bool
}

// Model is one entry of a runtime's model menu (the id is the Studio's, "<provider>/<model>").
type Model struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	Provider string `json:"provider"`
}

// ModelParam is one chosen model parameter (reasoning effort, fast mode, ...).
type ModelParam struct {
	ID    string `json:"id"`
	Value string `json:"value"`
}

// Image is a prompt attachment (base64 data).
type Image struct {
	MediaType string `json:"mediaType"`
	Data      string `json:"data"`
}

// Account is who a credential belongs to, when the runtime can say.
type Account struct {
	KeyName   string
	Email     string
	FirstName string
	LastName  string
}

// Sentinel errors every adapter maps its failures to.
var (
	// ErrNoCredential: the runtime needs a credential and none is configured.
	ErrNoCredential = errors.New("no credential configured")
	// ErrUnknownModel: the model is not in the menu (or was dropped from it).
	ErrUnknownModel = errors.New("model not available")
)

// Phase tells where Open failed.
type Phase string

const (
	PhaseModel   Phase = "model"   // choosing the model
	PhaseSession Phase = "session" // creating or resuming the runtime's session
)

// OpenError is a failed Open.
type OpenError struct {
	Phase Phase
	Err   error
}

func (e *OpenError) Error() string { return e.Err.Error() }
func (e *OpenError) Unwrap() error { return e.Err }

// KeyStore is where a runtime's credential lives (secrets.File).
type KeyStore interface {
	Key() (key string, source string)
	Store(string) error
	Forget() error
}

// CredentialSpec is the one credential slot of a runtime: what the settings routes need to take, check and describe it.
// Messages are the user-facing sentences the Studio shows.
type CredentialSpec struct {
	Provider string // slot id under /me/credentials/<runtime>/<provider>
	Label    string // "Cursor"
	Store    KeyStore
	// Missing is what to say when nothing was pasted; Validate returns "" or why a pasted secret is refused.
	Missing  string
	Validate func(secret string) string
	// NoCredential is the error of a run or a check without a credential.
	NoCredential error
	// AuthHint says what to do about a credential the runtime rejected; Describe is a log-safe description of it.
	AuthHint func(secret, source string) string
	Describe func(secret, source string) string
	// Mask is the Studio's masked form of the secret.
	Mask func(secret string) string
}

// Prompt is one user turn.
type Prompt struct {
	Text   string
	Images []Image
}

// OpenRequest opens (or resumes) the runtime's session of one chat.
type OpenRequest struct {
	// Resume is the handle the runtime gave this chat before ("" for a new chat).
	Resume string
	// Cwd is the chat's workspace (its worktree or the project folder).
	Cwd string
	// ModelID and ModelParams are what the Studio picked.
	ModelID     string
	ModelParams []ModelParam
}

// Result is what a finished run left: the host stores the text and shows the tool count, usage and the error.
type Result struct {
	// Text is the assistant's streamed text; Summary is the runtime's final text (used when nothing streamed).
	Text      string
	Summary   string
	Error     string // the run ended badly (not a transport failure)
	ToolCalls int
	Usage     *Usage
	Files     []string // workspace-relative paths the agent wrote or deleted, as the runtime saw them
}

// Adapter is one coding-agent runtime.
type Adapter interface {
	Runtime() string
	Capabilities() Capabilities
	// Ready is nil when the runtime may run agents; an error fails the agent closed with that reason.
	Ready() error
	// Unsandboxed: agents run without the runtime's sandbox (Windows); the Studio shows a warning.
	Unsandboxed() bool
	// Credential is the runtime's credential slot, nil when it keeps none.
	Credential() *CredentialSpec
	HasCredential() bool
	Account(ctx context.Context) (Account, error)
	// CheckCredential asks the runtime whether secret works before it is stored.
	CheckCredential(ctx context.Context, secret string) (Account, error)
	// Unauthenticated reports an error that means the runtime rejected the credential.
	Unauthenticated(err error) bool
	// Models is the model menu (nothing hard-coded: the runtime's own list or the owner's config).
	Models(ctx context.Context) ([]Model, error)
	// DefaultModel is the menu entry offered when the list cannot be had.
	DefaultModel() Model
	// CheckModel refuses an id this runtime cannot serve ("" = fine); it runs before any run starts.
	CheckModel(modelID string) string
	// DropModel removes a model that failed with an access or plan error and returns what to tell the user ("" = nothing).
	DropModel(modelID string, err error) string
	// CheckWorkspace refuses a workspace the runtime must not run in (an error = HTTP 409 with its text).
	CheckWorkspace(cwd string) error
	Open(ctx context.Context, req OpenRequest) (Session, error)
	Close()
}

// Session is the runtime's side of one chat.
type Session interface {
	// Handle is what the host stores to resume the chat (store.Session.AgentID).
	Handle() string
	// Start begins one run; events go to emit in order and stop before Wait returns. runID and sessionID stamp them.
	Start(ctx context.Context, sessionID, runID string, p Prompt, emit func(Event)) Run
}

// Run is one running prompt.
type Run interface {
	// Cancel asks the runtime to stop the run (best effort; the context given to Start is the hard stop). It is safe
	// before the runtime has a run to stop.
	Cancel(ctx context.Context) error
	// Wait returns when the run is over; err is a transport or startup failure, Result.Error a run that ended badly.
	Wait() (Result, error)
}

// Registry holds the adapters of one server, by runtime.
type Registry struct {
	byRuntime map[string]Adapter
	def       string
}

// NewRegistry registers adapters; def is the runtime of new chats ("" = the first adapter's, cursor-sdk preferred).
func NewRegistry(def string, adapters ...Adapter) (*Registry, error) {
	r := &Registry{byRuntime: map[string]Adapter{}}
	for _, a := range adapters {
		if a == nil {
			continue
		}
		if _, dup := r.byRuntime[a.Runtime()]; dup {
			return nil, fmt.Errorf("runtime %q registered twice", a.Runtime())
		}
		r.byRuntime[a.Runtime()] = a
	}
	switch {
	case def != "":
		if _, ok := r.byRuntime[def]; !ok {
			return nil, fmt.Errorf("default runtime %q is not registered", def)
		}
		r.def = def
	case r.byRuntime[RuntimeCursorSDK] != nil:
		r.def = RuntimeCursorSDK
	default:
		if ids := r.Runtimes(); len(ids) > 0 {
			r.def = ids[0]
		}
	}
	return r, nil
}

// Of is the adapter of a runtime (nil when this server does not have it).
func (r *Registry) Of(runtime string) Adapter {
	if r == nil {
		return nil
	}
	return r.byRuntime[runtime]
}

// Default is the runtime new chats get.
func (r *Registry) Default() string {
	if r == nil {
		return RuntimeCursorSDK
	}
	return r.def
}

// Runtimes lists the registered runtime ids, sorted.
func (r *Registry) Runtimes() []string {
	if r == nil {
		return nil
	}
	ids := make([]string, 0, len(r.byRuntime))
	for id := range r.byRuntime {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Close closes every adapter.
func (r *Registry) Close() {
	if r == nil {
		return
	}
	for _, a := range r.byRuntime {
		a.Close()
	}
}

// What the seam answers when this server has no runtime at all: the shape of the first (Cursor) answer, kept so a Studio
// that predates runtimes sees no change.
var (
	LegacyCapabilities = Capabilities{Runtime: RuntimeCursorSDK, Label: "Cursor", ModelSource: ModelSourceRuntime,
		Streaming: true, Tools: true, ShowThinking: true, ShowToolTrace: true}
	LegacyDefaultModel = Model{ID: "cursor/auto", Provider: "cursor", Label: "Auto"}
)
