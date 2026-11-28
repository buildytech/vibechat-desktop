package codingagent

import (
	"encoding/json"
	"testing"
)

type stub struct {
	Adapter
	rt string
}

func (s stub) Runtime() string { return s.rt }
func (s stub) Close()          {}

func TestRuntimeOf(t *testing.T) {
	if RuntimeOf("") != RuntimeCursorSDK || RuntimeOf(RuntimePi) != RuntimePi {
		t.Fatal("empty stored runtime must be cursor-sdk, others unchanged")
	}
}

func TestRegistry(t *testing.T) {
	r, err := NewRegistry("", stub{rt: RuntimePi}, stub{rt: RuntimeCursorSDK})
	if err != nil || r.Default() != RuntimeCursorSDK {
		t.Fatalf("default %q err %v: cursor-sdk is the default when registered", r.Default(), err)
	}
	if got := r.Runtimes(); len(got) != 2 || got[0] != RuntimeCursorSDK || got[1] != RuntimePi {
		t.Fatalf("runtimes %v", got)
	}
	r, err = NewRegistry(RuntimePi, stub{rt: RuntimeCursorSDK}, stub{rt: RuntimePi})
	if err != nil || r.Default() != RuntimePi || r.Of(RuntimePi) == nil || r.Of("x") != nil {
		t.Fatal("contour config picks the default; both runtimes stay first class")
	}
	if _, err := NewRegistry("pi", stub{rt: RuntimeCursorSDK}); err == nil {
		t.Fatal("a configured runtime this build does not have must be an error, not a fallback")
	}
	if _, err := NewRegistry("", stub{rt: "a"}, stub{rt: "a"}); err == nil {
		t.Fatal("duplicate runtime")
	}
	var nilReg *Registry
	if nilReg.Default() != RuntimeCursorSDK || nilReg.Of(RuntimeCursorSDK) != nil || len(nilReg.Runtimes()) != 0 {
		t.Fatal("a nil registry has no runtime and the legacy default")
	}
}

// The wire shape of every event is the Agent Contract v1 and must not change.
func TestEventWire(t *testing.T) {
	ref := Ref{SessionID: "s", RunID: "r"}
	cases := []struct {
		e    Event
		want string
	}{
		{MessageDelta{Ref: ref, Text: "hi"}, `{"role":"assistant","runId":"r","sessionId":"s","text":"hi","type":"message.delta"}`},
		{MessageDelta{Ref{SessionID: "s"}, "hi"}, `{"role":"assistant","sessionId":"s","text":"hi","type":"message.delta"}`},
		{ThinkingDelta{ref, "t"}, `{"runId":"r","sessionId":"s","text":"t","type":"thinking.delta"}`},
		{ThinkingCompleted{ref, -1}, `{"runId":"r","sessionId":"s","type":"thinking.completed"}`},
		{ThinkingCompleted{ref, 7}, `{"durationMs":7,"runId":"r","sessionId":"s","type":"thinking.completed"}`},
		{ToolStarted{ref, "c", "shell", "shell", "ls"}, `{"callId":"c","kind":"shell","runId":"r","sessionId":"s","summary":"ls","tool":"shell","type":"tool.started"}`},
		{ToolStarted{ref, "", "shell", "shell", "ls"}, `{"kind":"shell","runId":"r","sessionId":"s","summary":"ls","tool":"shell","type":"tool.started"}`},
		{ToolDelta{ref, "c", "shell", "o"}, `{"callId":"c","runId":"r","sessionId":"s","text":"o","tool":"shell","type":"tool.delta"}`},
		{ToolFinished{ref, "c", "shell", "shell", "ls", "d"}, `{"callId":"c","detail":"d","kind":"shell","runId":"r","sessionId":"s","summary":"ls","tool":"shell","type":"tool.finished"}`},
		{ToolFinished{ref, "c", "shell", "shell", "ls", ""}, `{"callId":"c","kind":"shell","runId":"r","sessionId":"s","summary":"ls","tool":"shell","type":"tool.finished"}`},
		{FilesChanged{ref, []string{"a", "b"}}, `{"paths":["a","b"],"runId":"r","sessionId":"s","type":"files.changed"}`},
		{SessionStatus{ref, "idle"}, `{"runId":"r","sessionId":"s","status":"idle","type":"session.status"}`},
		{UsageEvent{ref, Usage{1, 2, 3, 4, 5, 6}}, `{"cacheReadTokens":3,"cacheWriteTokens":4,"reasoningTokens":6,"runId":"r","sessionId":"s","tokensIn":1,"tokensOut":2,"totalTokens":5,"type":"usage"}`},
	}
	for _, c := range cases {
		raw, _ := json.Marshal(c.e.Wire())
		if string(raw) != c.want {
			t.Errorf("%T:\n got %s\nwant %s", c.e, raw, c.want)
		}
	}
}
