package codingagent

// Event is one Agent Contract v1 AgentEvent (studio packages/agent-contract). The set is closed: only the types in
// this file implement it, so a runtime cannot invent a wire event. Wire is the JSON object sent over SSE; its keys are
// the contract's and must not change.
type Event interface {
	Wire() map[string]any
	closed()
}

// Ref stamps an event with its chat and run.
type Ref struct {
	SessionID string
	RunID     string
}

func (r Ref) wire(typ string, kv ...any) map[string]any {
	e := map[string]any{"type": typ, "sessionId": r.SessionID}
	if r.RunID != "" {
		e["runId"] = r.RunID
	}
	for i := 0; i+1 < len(kv); i += 2 {
		e[kv[i].(string)] = kv[i+1]
	}
	return e
}

// MessageDelta is a piece of the assistant's answer.
type MessageDelta struct {
	Ref
	Text string
}

// ThinkingDelta is a piece of the model's reasoning; ThinkingCompleted closes it (DurationMs < 0 = unknown).
type ThinkingDelta struct {
	Ref
	Text string
}
type ThinkingCompleted struct {
	Ref
	DurationMs int64
}

// ToolStarted, ToolDelta and ToolFinished trace one tool call.
type ToolStarted struct {
	Ref
	CallID, Tool, Kind, Summary string
}
type ToolDelta struct {
	Ref
	CallID, Tool, Text string
}
type ToolFinished struct {
	Ref
	CallID, Tool, Kind, Summary, Detail string
}

// FilesChanged lists workspace paths the run changed.
type FilesChanged struct {
	Ref
	Paths []string
}

// SessionStatus is idle, running or error.
type SessionStatus struct {
	Ref
	Status string
}

// Usage is the token usage of a run. UsageEvent carries it on the wire.
type Usage struct {
	InputTokens      int64 `json:"inputTokens"`
	OutputTokens     int64 `json:"outputTokens"`
	CacheReadTokens  int64 `json:"cacheReadTokens"`
	CacheWriteTokens int64 `json:"cacheWriteTokens"`
	TotalTokens      int64 `json:"totalTokens"`
	ReasoningTokens  int64 `json:"reasoningTokens"`
}
type UsageEvent struct {
	Ref
	Usage Usage
}

func (e MessageDelta) Wire() map[string]any {
	return e.wire("message.delta", "role", "assistant", "text", e.Text)
}
func (e ThinkingDelta) Wire() map[string]any { return e.wire("thinking.delta", "text", e.Text) }
func (e ThinkingCompleted) Wire() map[string]any {
	m := e.wire("thinking.completed")
	if e.DurationMs >= 0 {
		m["durationMs"] = e.DurationMs
	}
	return m
}
func (e ToolStarted) Wire() map[string]any {
	m := e.wire("tool.started", "tool", e.Tool, "summary", e.Summary, "kind", e.Kind)
	if e.CallID != "" {
		m["callId"] = e.CallID
	}
	return m
}
func (e ToolDelta) Wire() map[string]any {
	m := e.wire("tool.delta", "tool", e.Tool, "text", e.Text)
	if e.CallID != "" {
		m["callId"] = e.CallID
	}
	return m
}
func (e ToolFinished) Wire() map[string]any {
	m := e.wire("tool.finished", "tool", e.Tool, "summary", e.Summary, "kind", e.Kind)
	if e.Detail != "" {
		m["detail"] = e.Detail
	}
	if e.CallID != "" {
		m["callId"] = e.CallID
	}
	return m
}
func (e FilesChanged) Wire() map[string]any {
	paths := make([]any, len(e.Paths))
	for i, p := range e.Paths {
		paths[i] = p
	}
	return e.wire("files.changed", "paths", paths)
}
func (e SessionStatus) Wire() map[string]any { return e.wire("session.status", "status", e.Status) }
func (e UsageEvent) Wire() map[string]any {
	u := e.Usage
	return e.wire("usage", "tokensIn", u.InputTokens, "tokensOut", u.OutputTokens, "cacheReadTokens", u.CacheReadTokens,
		"cacheWriteTokens", u.CacheWriteTokens, "reasoningTokens", u.ReasoningTokens, "totalTokens", u.TotalTokens)
}

func (MessageDelta) closed()      {}
func (ThinkingDelta) closed()     {}
func (ThinkingCompleted) closed() {}
func (ToolStarted) closed()       {}
func (ToolDelta) closed()         {}
func (ToolFinished) closed()      {}
func (FilesChanged) closed()      {}
func (SessionStatus) closed()     {}
func (UsageEvent) closed()        {}
