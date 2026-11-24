package agent

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

// Event is one Agent Contract v1 AgentEvent (studio packages/agent-contract), as sent over SSE.
type Event map[string]any

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
type Usage struct {
	InputTokens      int64 `json:"inputTokens"`
	OutputTokens     int64 `json:"outputTokens"`
	CacheReadTokens  int64 `json:"cacheReadTokens"`
	CacheWriteTokens int64 `json:"cacheWriteTokens"`
	TotalTokens      int64 `json:"totalTokens"`
	ReasoningTokens  int64 `json:"reasoningTokens"`
}

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
}

func (t *Translator) ev(typ string, kv ...any) Event {
	e := Event{"type": typ, "sessionId": t.SessionID}
	if t.RunID != "" {
		e["runId"] = t.RunID
	}
	for i := 0; i+1 < len(kv); i += 2 {
		e[kv[i].(string)] = kv[i+1]
	}
	return e
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
			return []Event{t.ev("usage", "tokensIn", u.InputTokens, "tokensOut", u.OutputTokens, "cacheReadTokens", u.CacheReadTokens,
				"cacheWriteTokens", u.CacheWriteTokens, "reasoningTokens", u.ReasoningTokens, "totalTokens", u.TotalTokens)}
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
		return []Event{t.ev("message.delta", "role", "assistant", "text", u.Text)}
	case "thinking-delta":
		if u.Text == "" {
			return nil
		}
		return []Event{t.ev("thinking.delta", "text", u.Text)}
	case "thinking-completed":
		e := t.ev("thinking.completed")
		if u.ThinkingDurationMs != nil && *u.ThinkingDurationMs >= 0 {
			e["durationMs"] = *u.ThinkingDurationMs
		}
		return []Event{e}
	case "tool-call-started", "tool-call-completed", "tool-call-error":
		if u.ToolCall == nil {
			return nil
		}
		v := t.view(u.ToolCall, typ != "tool-call-started")
		evType := "tool.finished"
		if typ == "tool-call-started" {
			evType = "tool.started"
			t.ToolCalls++
		}
		e := t.ev(evType, "tool", v.tool, "summary", v.summary, "kind", v.kind)
		if v.detail != "" {
			e["detail"] = v.detail
		}
		if u.CallID != "" {
			e["callId"] = u.CallID
		}
		out := []Event{e}
		if evType == "tool.finished" && v.file != "" && v.wrote {
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
		e := t.ev("tool.delta", "tool", "shell", "text", clip(text, detailMax))
		if u.CallID != "" {
			e["callId"] = u.CallID
		}
		return []Event{e}
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
