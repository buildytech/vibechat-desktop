package pi

import (
	"encoding/json"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/buildytech/vibechat-desktop/server/internal/codingagent"
)

// Event is a neutral coding-agent event; the Translator emits only the closed set of internal/codingagent.
type Event = codingagent.Event

const (
	summaryMax = 240
	detailMax  = 8000
)

// Translator turns the records of Pi's RPC stdout (the JSON event stream) into the closed set of contract events for
// one run, and keeps the run's totals. Mapping (docs/adr/0002):
//
//	message_update/text_delta                       -> message.delta
//	message_update/thinking_delta, thinking_end     -> thinking.delta, thinking.completed
//	tool_execution_start / _update / _end           -> tool.started / tool.delta / tool.finished
//	message_end (assistant, with usage)             -> usage (cumulative over the run's turns)
//	agent_settled                                   -> ends the run (no event; the host sends session.status)
//
// Everything else (queue, compaction, retry, extension and session-info records, the provider-level start/done of a
// message, the tool-call argument stream) has no event on the contract and is dropped; a retry that fails for good and
// an abort become the run's Error.
type Translator struct {
	SessionID string
	RunID     string
	// Cwd makes file paths in tool summaries relative.
	Cwd string
	// Now is the clock of thinking durations (nil = time.Now).
	Now func() time.Time

	mu        sync.Mutex
	text      strings.Builder
	toolCalls int
	files     map[string]bool
	usage     *Usage
	summary   string
	errText   string
	settled   bool
	aborted   bool
	think     map[int]time.Time
	tools     map[string]*toolState
}

// Usage is the token usage of a run.
type Usage = codingagent.Usage

type toolState struct {
	view    view
	partial string
}

func (t *Translator) ref() codingagent.Ref {
	return codingagent.Ref{SessionID: t.SessionID, RunID: t.RunID}
}

func (t *Translator) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now()
}

// Settled reports that Pi said agent_settled: nothing more will happen for this prompt.
func (t *Translator) Settled() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.settled
}

// Result is what the run left (valid once Settled, or when the process ended).
func (t *Translator) Result() codingagent.Result {
	t.mu.Lock()
	defer t.mu.Unlock()
	files := make([]string, 0, len(t.files))
	for p := range t.files {
		files = append(files, p)
	}
	sort.Strings(files)
	res := codingagent.Result{Text: t.text.String(), Summary: t.summary, Error: t.errText, ToolCalls: t.toolCalls, Files: files}
	if t.usage != nil {
		u := *t.usage
		res.Usage = &u
	}
	return res
}

type content struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type message struct {
	Role         string          `json:"role"`
	Content      json.RawMessage `json:"content"`
	StopReason   string          `json:"stopReason"`
	ErrorMessage string          `json:"errorMessage"`
	Usage        *struct {
		Input       int64 `json:"input"`
		Output      int64 `json:"output"`
		CacheRead   int64 `json:"cacheRead"`
		CacheWrite  int64 `json:"cacheWrite"`
		TotalTokens int64 `json:"totalTokens"`
	} `json:"usage"`
}

// text is the text of a message's content: a string or a list of blocks.
func (m message) text() string {
	var s string
	if json.Unmarshal(m.Content, &s) == nil {
		return s
	}
	return blocksText(m.Content)
}

func blocksText(raw json.RawMessage) string {
	var cs []content
	if json.Unmarshal(raw, &cs) != nil {
		return ""
	}
	var b strings.Builder
	for _, c := range cs {
		if c.Type == "text" {
			b.WriteString(c.Text)
		}
	}
	return b.String()
}

// Record translates one stdout record; it returns the events to publish (possibly none).
func (t *Translator) Record(rec Record) []Event {
	t.mu.Lock()
	defer t.mu.Unlock()
	switch rec.Type {
	case "message_update":
		var u struct {
			E struct {
				Type         string `json:"type"`
				ContentIndex int    `json:"contentIndex"`
				Delta        string `json:"delta"`
			} `json:"assistantMessageEvent"`
		}
		if json.Unmarshal(rec.Raw, &u) != nil {
			return nil
		}
		switch u.E.Type {
		case "text_delta":
			if u.E.Delta == "" {
				return nil
			}
			t.text.WriteString(u.E.Delta)
			return []Event{codingagent.MessageDelta{Ref: t.ref(), Text: u.E.Delta}}
		case "thinking_start":
			if t.think == nil {
				t.think = map[int]time.Time{}
			}
			t.think[u.E.ContentIndex] = t.now()
		case "thinking_delta":
			if u.E.Delta == "" {
				return nil
			}
			if t.think == nil {
				t.think = map[int]time.Time{}
			}
			if _, ok := t.think[u.E.ContentIndex]; !ok {
				t.think[u.E.ContentIndex] = t.now()
			}
			return []Event{codingagent.ThinkingDelta{Ref: t.ref(), Text: u.E.Delta}}
		case "thinking_end":
			dur := int64(-1)
			if start, ok := t.think[u.E.ContentIndex]; ok {
				dur = t.now().Sub(start).Milliseconds()
				if dur < 0 {
					dur = 0
				}
				delete(t.think, u.E.ContentIndex)
			}
			return []Event{codingagent.ThinkingCompleted{Ref: t.ref(), DurationMs: dur}}
		}
	case "message_end":
		var m struct {
			Message message `json:"message"`
		}
		if json.Unmarshal(rec.Raw, &m) != nil || m.Message.Role != "assistant" {
			return nil
		}
		msg := m.Message
		if s := msg.text(); s != "" {
			t.summary = s
		}
		switch msg.StopReason {
		case "error":
			t.errText = firstNonEmpty(msg.ErrorMessage, "error")
		case "aborted":
			t.errText = "cancelled"
		default:
			t.errText = ""
		}
		if msg.Usage == nil {
			return nil
		}
		if t.usage == nil {
			t.usage = &Usage{}
		}
		u := msg.Usage
		t.usage.InputTokens += u.Input
		t.usage.OutputTokens += u.Output
		t.usage.CacheReadTokens += u.CacheRead
		t.usage.CacheWriteTokens += u.CacheWrite
		total := u.TotalTokens
		if total == 0 {
			total = u.Input + u.Output + u.CacheRead + u.CacheWrite
		}
		t.usage.TotalTokens += total
		return []Event{codingagent.UsageEvent{Ref: t.ref(), Usage: *t.usage}}
	case "tool_execution_start":
		var e struct {
			ID   string         `json:"toolCallId"`
			Name string         `json:"toolName"`
			Args map[string]any `json:"args"`
		}
		if json.Unmarshal(rec.Raw, &e) != nil {
			return nil
		}
		v := t.view(e.Name, e.Args)
		if t.tools == nil {
			t.tools = map[string]*toolState{}
		}
		t.tools[e.ID] = &toolState{view: v}
		t.toolCalls++
		return []Event{codingagent.ToolStarted{Ref: t.ref(), CallID: e.ID, Tool: v.tool, Kind: v.kind, Summary: v.summary}}
	case "tool_execution_update":
		var e struct {
			ID     string `json:"toolCallId"`
			Name   string `json:"toolName"`
			Result struct {
				Content json.RawMessage `json:"content"`
			} `json:"partialResult"`
		}
		if json.Unmarshal(rec.Raw, &e) != nil {
			return nil
		}
		st := t.tools[e.ID]
		if st == nil || st.view.kind != "shell" {
			return nil
		}
		full := blocksText(e.Result.Content)
		// The partial result of a shell call is its output so far: the new part is the delta.
		delta := full
		if strings.HasPrefix(full, st.partial) {
			delta = full[len(st.partial):]
		}
		st.partial = full
		if delta == "" {
			return nil
		}
		return []Event{codingagent.ToolDelta{Ref: t.ref(), CallID: e.ID, Tool: st.view.tool, Text: clip(delta, detailMax)}}
	case "tool_execution_end":
		var e struct {
			ID     string `json:"toolCallId"`
			Name   string `json:"toolName"`
			IsErr  bool   `json:"isError"`
			Result struct {
				Content json.RawMessage `json:"content"`
				Details struct {
					Diff string `json:"diff"`
				} `json:"details"`
			} `json:"result"`
		}
		if json.Unmarshal(rec.Raw, &e) != nil {
			return nil
		}
		var v view
		if st := t.tools[e.ID]; st != nil {
			v = st.view
			delete(t.tools, e.ID)
		} else {
			v = t.view(e.Name, nil)
		}
		out := blocksText(e.Result.Content)
		switch {
		case e.IsErr:
			v.detail = "error: " + out
		case v.kind == "shell":
			v.detail = out
		case v.kind == "file" && e.Result.Details.Diff != "":
			v.detail = e.Result.Details.Diff
		}
		if !e.IsErr && v.wrote && v.file != "" {
			if t.files == nil {
				t.files = map[string]bool{}
			}
			t.files[v.file] = true
		}
		v.detail = clip(strings.TrimRight(v.detail, "\n"), detailMax)
		return []Event{codingagent.ToolFinished{Ref: t.ref(), CallID: e.ID, Tool: v.tool, Kind: v.kind, Summary: v.summary, Detail: v.detail}}
	case "auto_retry_end":
		var e struct {
			Success    bool   `json:"success"`
			FinalError string `json:"finalError"`
		}
		if json.Unmarshal(rec.Raw, &e) == nil && !e.Success && e.FinalError != "" {
			t.errText = e.FinalError
		}
	case "agent_settled":
		var e struct {
			Aborted bool `json:"aborted"`
		}
		_ = json.Unmarshal(rec.Raw, &e)
		t.settled, t.aborted = true, e.Aborted
		if e.Aborted && t.errText == "" {
			t.errText = "cancelled"
		}
	}
	return nil
}

type view struct {
	tool, kind, summary, detail, file string
	wrote                             bool
}

func (t *Translator) rel(p string) string {
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

// view names a Pi tool the way the Studio (and the Cursor adapter) names it: bash is "shell", the file tools are
// "file", grep/find "search"; anything else keeps its own name.
func (t *Translator) view(name string, args map[string]any) view {
	typ := strings.ToLower(firstNonEmpty(name, "tool"))
	v := view{tool: typ, kind: "tool"}
	if args == nil {
		args = map[string]any{}
	}
	switch typ {
	case "bash":
		v.tool, v.kind = "shell", "shell"
		v.summary = oneLine(str(args, "command"))
	case "read", "edit", "write", "ls":
		v.kind = "file"
		v.file = t.rel(str(args, "path", "file_path"))
		v.summary = strings.TrimSpace(typ + " " + v.file)
		// Only a path inside the workspace is a changed file of the run (the jail keeps the rest read-only anyway).
		v.wrote = (typ == "edit" || typ == "write") && v.file != "" && v.file != "." && !path.IsAbs(v.file) && !filepath.IsAbs(v.file) &&
			v.file != ".." && !strings.HasPrefix(v.file, "../")
	case "grep", "find":
		v.kind = "search"
		v.summary = oneLine(firstNonEmpty(str(args, "pattern", "query"), typ))
	default:
		v.summary = typ
	}
	if strings.TrimSpace(v.summary) == "" {
		v.summary = typ
	}
	v.summary = clip(v.summary, summaryMax)
	return v
}

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
