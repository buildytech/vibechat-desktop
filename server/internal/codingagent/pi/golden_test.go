package pi

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The golden files pin what Pi's RPC records become on the seam (Agent Contract v1 events) and what the host reads from
// a finished run. real-run.jsonl is a capture of the pinned Pi 1.7.0 in the jail against the stub provider (a shell
// call with streamed output, a file write, an edit, a failing read, a final answer); synthetic.jsonl and aborted.jsonl
// are hand-made for what a stub cannot provoke (thinking, retries, provider errors, records with no event, an abort,
// CRLF, U+2028 inside text). Refresh on purpose: UPDATE_GOLDEN=1 go test ./internal/codingagent/pi -run Golden

func translateFile(t *testing.T, name, cwd string) map[string]any {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	clock := time.Unix(1_700_000_000, 0)
	tr := &Translator{SessionID: "s1", RunID: "r1", Cwd: cwd, Now: func() time.Time { clock = clock.Add(250 * time.Millisecond); return clock }}
	var events []any
	if err := readRecords(f, func(rec Record) bool {
		for _, e := range tr.Record(rec) {
			events = append(events, e.Wire())
		}
		return true
	}); err != nil {
		t.Fatal(err)
	}
	res := tr.Result()
	var usage any
	if res.Usage != nil {
		b, _ := json.Marshal(res.Usage)
		_ = json.Unmarshal(b, &usage)
	}
	return map[string]any{
		"events": events,
		"run": map[string]any{"settled": tr.Settled(), "error": res.Error, "text": res.Text, "summary": res.Summary,
			"toolCalls": res.ToolCalls, "files": res.Files, "usage": usage},
	}
}

func goldenCheck(t *testing.T, name string, got map[string]any) {
	t.Helper()
	raw, _ := json.MarshalIndent(got, "", "  ")
	raw = append(raw, '\n')
	file := filepath.Join("testdata", "golden", name)
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(file, raw, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("%v (UPDATE_GOLDEN=1 writes it)", err)
	}
	if string(want) != string(raw) {
		_ = os.WriteFile(file+".got", raw, 0o644)
		t.Fatalf("%s differs (the answer is in %s.got)", file, file)
	}
}

func TestGoldenRealRun(t *testing.T) {
	goldenCheck(t, "real-run.json", translateFile(t, "real-run.jsonl", "/W/ws"))
}

func TestGoldenSyntheticRun(t *testing.T) {
	goldenCheck(t, "synthetic.json", translateFile(t, "synthetic.jsonl", "/W/ws"))
}

func TestGoldenAbortedRun(t *testing.T) {
	goldenCheck(t, "aborted.json", translateFile(t, "aborted.jsonl", "/W/ws"))
}

// Every event the translator makes is one of the contract's nine types, and the real run's semantics hold regardless of
// the golden text: the shell output arrives as deltas, a failed tool has an error detail, only successful writes count
// as changed files.
func TestRealRunSemantics(t *testing.T) {
	got := translateFile(t, "real-run.jsonl", "/W/ws")
	allowed := map[string]bool{"message.delta": true, "thinking.delta": true, "thinking.completed": true, "tool.started": true,
		"tool.delta": true, "tool.finished": true, "files.changed": true, "session.status": true, "usage": true}
	var deltas string
	finished := map[string]map[string]any{}
	for _, e := range got["events"].([]any) {
		m := e.(map[string]any)
		if !allowed[m["type"].(string)] {
			t.Fatalf("event outside the contract: %v", m)
		}
		if m["type"] == "tool.delta" {
			deltas += m["text"].(string)
		}
		if m["type"] == "tool.finished" {
			finished[m["callId"].(string)] = m
		}
	}
	if deltas != "one\ntwo\n" {
		t.Fatalf("shell deltas %q", deltas)
	}
	if d, _ := finished["call_4_0"]["detail"].(string); d == "" || d[:6] != "error:" {
		t.Fatalf("failed read: %v", finished["call_4_0"])
	}
	run := got["run"].(map[string]any)
	if files := run["files"].([]string); len(files) != 1 || files[0] != "notes/a.txt" {
		t.Fatalf("files %v", files)
	}
}
