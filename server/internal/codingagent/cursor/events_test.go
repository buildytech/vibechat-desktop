package cursor

import (
	"bufio"
	"os"
	"strings"
	"testing"
)

// The fixture is a real sandboxed grok-4.7 run through the bridge (paths normalised to /W, token deltas dropped):
// create hello.txt, Read / cat a file outside the workspace, write outside, a network call.
func TestTranslateSandboxProbeRun(t *testing.T) {
	f, err := os.Open("testdata/run-sandbox-probe.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tr := &Translator{SessionID: "s1", RunID: "r1", Cwd: "/W/ws"}
	counts := map[string]int{}
	var tools []map[string]any
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		for _, ev := range tr.Frame([]byte(sc.Text())) {
			e := ev.Wire()
			counts[e["type"].(string)]++
			if e["sessionId"] != "s1" || e["runId"] != "r1" {
				t.Fatalf("unstamped event %v", e)
			}
			if strings.HasPrefix(e["type"].(string), "tool.") {
				tools = append(tools, e)
			}
		}
	}
	for _, typ := range []string{"message.delta", "thinking.delta", "thinking.completed", "tool.started", "tool.finished", "usage"} {
		if counts[typ] == 0 {
			t.Errorf("no %s events (%v)", typ, counts)
		}
	}
	if tr.Status != "RUN_LIFECYCLE_STATUS_FINISHED" || tr.Error != "" || tr.ToolCalls != 6 {
		t.Fatalf("status %s err %q tools %d", tr.Status, tr.Error, tr.ToolCalls)
	}
	if !tr.Files["hello.txt"] || len(tr.Files) != 1 {
		t.Fatalf("files %v", tr.Files)
	}
	if !strings.Contains(tr.Text.String(), "hello.txt") || tr.Usage == nil || tr.Usage.InputTokens == 0 {
		t.Fatal("text or usage missing")
	}
	var sawDenied bool
	for _, e := range tools {
		if d, _ := e["detail"].(string); strings.Contains(d, "Permission denied") {
			sawDenied = true
		}
		if e["kind"] == "file" && strings.Contains(e["summary"].(string), "/W/ws") {
			t.Errorf("summary not relative: %v", e["summary"])
		}
	}
	if !sawDenied {
		t.Error("sandbox denial not visible in tool details")
	}
}

func TestClipKeepsUTF8(t *testing.T) {
	s := strings.Repeat("я", 200)
	c := clip(s, 101)
	if !strings.HasSuffix(c, "…") || !strings.HasPrefix(c, "я") {
		t.Fatal(c)
	}
	for _, r := range c {
		if r == '\uFFFD' {
			t.Fatal("broken rune")
		}
	}
}
