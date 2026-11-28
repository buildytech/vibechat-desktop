package cursor

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// The golden files pin what the bridge's run stream becomes on the seam (Agent Contract v1 events) and what the host
// reads from a finished run. They guard the coding-agent adapter extraction (ADR 0002): the Cursor runtime may move,
// these events may not change. Refresh on purpose: UPDATE_GOLDEN=1 go test ./serve./internal/codingagent/cursor -run Golden

func translateFile(t *testing.T, name, cwd string) map[string]any {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tr := &Translator{SessionID: "s1", RunID: "r1", Cwd: cwd}
	var events []any
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		for _, e := range runFrame(tr, []byte(sc.Text())) {
			events = append(events, e)
		}
	}
	files := make([]string, 0, len(tr.Files))
	for p := range tr.Files {
		files = append(files, p)
	}
	sort.Strings(files)
	return map[string]any{
		"events": events,
		"run":    runSummary(tr, files),
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

func TestGoldenSandboxProbeRun(t *testing.T) {
	goldenCheck(t, "sandbox-probe.json", translateFile(t, "run-sandbox-probe.jsonl", "/W/ws"))
}

func TestGoldenSyntheticFrames(t *testing.T) {
	goldenCheck(t, "synthetic.json", translateFile(t, "synthetic-frames.jsonl", "/W/ws"))
}

// runFrame translates one frame into events as plain maps (what the host publishes).
func runFrame(tr *Translator, raw []byte) []map[string]any {
	var out []map[string]any
	for _, e := range tr.Frame(raw) {
		out = append(out, e.Wire())
	}
	return out
}

// runSummary is what the host reads from a finished run.
func runSummary(tr *Translator, files []string) map[string]any {
	usage := any(nil)
	if tr.Usage != nil {
		b, _ := json.Marshal(tr.Usage)
		_ = json.Unmarshal(b, &usage)
	}
	return map[string]any{"status": tr.Status, "error": tr.Error, "text": tr.Text.String(), "resultText": tr.ResultText,
		"toolCalls": tr.ToolCalls, "files": files, "usage": usage, "bridgeRun": tr.BridgeRun}
}
