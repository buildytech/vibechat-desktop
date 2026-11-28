package cursor

import (
	"encoding/json"
	"fmt"
	"github.com/buildytech/vibechat-desktop/server/internal/codingagent"
	"path"
	"path/filepath"
	"strings"
	"sync"
)

// Event is a neutral coding-agent event; the Translator emits the closed set of internal/codingagent.
type Event = codingagent.Event

const (
	summaryMax = 240
	detailMax  = 8000
)

// frame is one RunStreamMessage of the bridge's Send / ObserveRun stream.
type frame struct {
	SDKMessage *struct {
		Type    string          `json:"type"`
		Message json.RawMessage `json:"message"`
	} `json:"sdkMessage"`
	InteractionUpdate *struct {
		Type   string          `json:"type"`
		Update json.RawMessage `json:"update"`
	} `json:"interactionUpdate"`
	Result *struct {
		RunID     string `json:"runId"`
		Status    string `json:"status"`
		ErrorCode string `json:"errorCode"`
		Result    struct {
			Result       string `json:"result"`
			ErrorMessage string `json:"errorMessage"`
		} `json:"result"`
	} `json:"result"`
	Done *struct {
		RunID string `json:"runId"`
	} `json:"done"`
}

// Usage is the token usage of a run.
type Usage = codingagent.Usage

// Translator turns bridge frames into contract events for one run and keeps the run's totals.
type Translator struct {
	SessionID string
	RunID     string
	// Cwd makes file paths in tool summaries relative.
	Cwd string

	Text      strings.Builder
	ToolCalls int
	Files     map[string]bool
	Usage     *Usage
	// Status is the final RunLifecycleStatus ("" until the result frame).
	Status     string
	ResultText string
	Error      string
	BridgeRun  string

	mu sync.Mutex
}

// Bridge is the bridge's id of the run ("" until its first status frame); safe to call while frames arrive.
func (t *Translator) Bridge() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.BridgeRun
}

func (t *Translator) ref() codingagent.Ref {
	return codingagent.Ref{SessionID: t.SessionID, RunID: t.RunID}
}

type toolCall struct {
	Type   string         `json:"type"`
	Name   string         `json:"name"`
	Args   map[string]any `json:"args"`
	Result *struct {
		Status string          `json:"status"`
		Value  json.RawMessage `json:"value"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	} `json:"result"`
}

// Frame translates one raw frame; it returns the events to publish (possibly none).
func (t *Translator) Frame(raw json.RawMessage) []Event {
	t.mu.Lock()
	defer t.mu.Unlock()
	var f frame
	if json.Unmarshal(raw, &f) != nil {
		return nil
	}
	switch {
	case f.InteractionUpdate != nil:
		return t.update(f.InteractionUpdate.Type, f.InteractionUpdate.Update)
	case f.SDKMessage != nil && f.SDKMessage.Type == "status":
		var m struct {
			RunID string `json:"run_id"`
		}
		_ = json.Unmarshal(f.SDKMessage.Message, &m)
		if m.RunID != "" {
			t.BridgeRun = m.RunID
		}
	case f.SDKMessage != nil && f.SDKMessage.Type == "usage":
		var m struct {
			Usage Usage `json:"usage"`
		}
		if json.Unmarshal(f.SDKMessage.Message, &m) == nil {
			u := m.Usage
			t.Usage = &u
			return []Event{codingagent.UsageEvent{Ref: t.ref(), Usage: u}}
		}
	case f.Result != nil:
		t.Status = f.Result.Status
		t.ResultText = f.Result.Result.Result
		if f.Result.RunID != "" {
			t.BridgeRun = f.Result.RunID
		}
		if t.Status != "RUN_LIFECYCLE_STATUS_FINISHED" {
			t.Error = firstNonEmpty(f.Result.Result.ErrorMessage, f.Result.ErrorCode, strings.TrimPrefix(t.Status, "RUN_LIFECYCLE_STATUS_"))
		}
	}
	return nil
}

func (t *Translator) update(typ string, raw json.RawMessage) []Event {
	var u struct {
		Text               string    `json:"text"`
		ThinkingDurationMs *int64    `json:"thinkingDurationMs"`
		CallID             string    `json:"callId"`
		ToolCall           *toolCall `json:"toolCall"`
		Event              *struct {
			Data   string `json:"data"`
			Stdout string `json:"stdout"`
			Stderr string `json:"stderr"`
		} `json:"event"`
	}
	if json.Unmarshal(raw, &u) != nil {
		return nil
	}
	switch typ {
	case "text-delta":
		if u.Text == "" {
			return nil
		}
		t.Text.WriteString(u.Text)
		return []Event{codingagent.MessageDelta{Ref: t.ref(), Text: u.Text}}
	case "thinking-delta":
		if u.Text == "" {
			return nil
		}
		return []Event{codingagent.ThinkingDelta{Ref: t.ref(), Text: u.Text}}
	case "thinking-completed":
		dur := int64(-1)
		if u.ThinkingDurationMs != nil && *u.ThinkingDurationMs >= 0 {
			dur = *u.ThinkingDurationMs
		}
		return []Event{codingagent.ThinkingCompleted{Ref: t.ref(), DurationMs: dur}}
	case "tool-call-started", "tool-call-completed", "tool-call-error":
		if u.ToolCall == nil {
			return nil
		}
		v := t.view(u.ToolCall, typ != "tool-call-started")
		started := typ == "tool-call-started"
		if started {
			t.ToolCalls++
		}
		var e Event
		if started {
			e = codingagent.ToolStarted{Ref: t.ref(), CallID: u.CallID, Tool: v.tool, Kind: v.kind, Summary: v.summary}
		} else {
			e = codingagent.ToolFinished{Ref: t.ref(), CallID: u.CallID, Tool: v.tool, Kind: v.kind, Summary: v.summary, Detail: v.detail}
		}
		out := []Event{e}
		if !started && v.file != "" && v.wrote {
			t.markFile(v.file)
		}
		return out
	case "shell-output-delta":
		text := u.Text
		if u.Event != nil {
			text = firstNonEmpty(u.Event.Data, u.Event.Stdout+u.Event.Stderr)
		}
		if text == "" {
			return nil
		}
		return []Event{codingagent.ToolDelta{Ref: t.ref(), CallID: u.CallID, Tool: "shell", Text: clip(text, detailMax)}}
	}
	return nil
}

func (t *Translator) markFile(p string) {
	if t.Files == nil {
		t.Files = map[string]bool{}
	}
	t.Files[p] = true
}

type view struct {
	tool, kind, summary, detail, file string
	wrote                             bool
}

func (t *Translator) rel(p string) string {
	// Slash-rooted paths (the SDK sandbox, POSIX hosts) are relativized the same way on every OS; filepath on Windows
	// does not treat "/W/ws" as absolute.
	if strings.HasPrefix(p, "/") && strings.HasPrefix(t.Cwd, "/") {
		cp, cc := path.Clean(p), path.Clean(t.Cwd)
		if cp == cc {
			return "."
		}
		if r := strings.TrimPrefix(cp, strings.TrimSuffix(cc, "/")+"/"); r != cp {
			return r
		}
		return p
	}
	if t.Cwd != "" && filepath.IsAbs(p) {
		if r, err := filepath.Rel(t.Cwd, p); err == nil && !strings.HasPrefix(r, "..") {
			return filepath.ToSlash(r)
		}
	}
	return p
}

func str(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

func (t *Translator) view(tc *toolCall, finished bool) view {
	typ := strings.ToLower(firstNonEmpty(tc.Type, tc.Name, "tool"))
	v := view{tool: typ, kind: "tool"}
	args := tc.Args
	if args == nil {
		args = map[string]any{}
	}
	switch typ {
	case "shell", "bash":
		v.tool, v.kind = "shell", "shell"
		v.summary = oneLine(str(args, "command"))
	case "read", "edit", "write", "delete", "ls":
		v.kind = "file"
		v.file = t.rel(str(args, "path", "file_path", "target_file"))
		v.summary = typ + " " + v.file
		v.wrote = typ == "edit" || typ == "write" || typ == "delete"
	case "grep", "glob", "semsearch", "codebase_search":
		v.kind = "search"
		v.summary = oneLine(firstNonEmpty(str(args, "pattern", "glob_pattern", "query"), typ))
	case "task":
		v.kind = "agent"
		v.summary = oneLine(firstNonEmpty(str(args, "description", "prompt"), "subagent"))
	case "updatetodos":
		v.tool, v.kind, v.summary = "updateTodos", "plan", "todos"
	default:
		v.summary = typ
	}
	if v.summary == "" {
		v.summary = typ
	}
	v.summary = clip(v.summary, summaryMax)
	if finished && tc.Result != nil {
		if tc.Result.Error != nil && tc.Result.Error.Message != "" {
			v.detail = "error: " + tc.Result.Error.Message
			v.wrote = false
		} else if tc.Result.Status == "error" {
			v.detail = "error"
			v.wrote = false
		} else {
			var val map[string]any
			_ = json.Unmarshal(tc.Result.Value, &val)
			switch v.kind {
			case "shell":
				d := str(val, "stdout") + str(val, "stderr")
				if code, ok := val["exitCode"].(float64); ok && code != 0 {
					d += fmt.Sprintf("\nexit %d", int(code))
				}
				v.detail = d
			case "file":
				v.detail = str(val, "diffString")
			}
		}
	}
	v.detail = clip(strings.TrimRight(v.detail, "\n"), detailMax)
	return v
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func clip(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max - 3
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut] + "…"
}

func firstNonEmpty(xs ...string) string {
	for _, x := range xs {
		if x != "" {
			return x
		}
	}
	return ""
}
