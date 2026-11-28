package pi

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func records(t *testing.T, in string) []string {
	t.Helper()
	var out []string
	if err := readRecords(strings.NewReader(in), func(r Record) bool { out = append(out, r.Type+":"+string(r.Raw)); return true }); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestFramingSplitsOnLFOnly(t *testing.T) {
	got := records(t, "{\"type\":\"a\",\"x\":\"p\u2028q\u2029r\"}\r\n\n  \n{\"type\":\"b\"}\n{\"nope\":1}\nnot json\n{\"type\":\"c\"}")
	if len(got) != 3 || !strings.HasPrefix(got[0], "a:") || !strings.Contains(got[0], "p\u2028q\u2029r") || got[1] != `b:{"type":"b"}` || got[2] != `c:{"type":"c"}` {
		t.Fatalf("%q", got)
	}
}

func TestFramingHandlesLongRecordsAndStops(t *testing.T) {
	big := strings.Repeat("y", 1<<20)
	in := `{"type":"big","v":"` + big + `"}` + "\n" + `{"type":"after"}` + "\n"
	got := records(t, in)
	if len(got) != 2 || len(got[0]) < 1<<20 {
		t.Fatalf("%d records", len(got))
	}
	n := 0
	_ = readRecords(strings.NewReader(in), func(Record) bool { n++; return false })
	if n != 1 {
		t.Fatalf("did not stop: %d", n)
	}
	tooBig := `{"type":"x","v":"` + strings.Repeat("z", maxRecord+10) + `"}` + "\n"
	if err := readRecords(strings.NewReader(tooBig), func(Record) bool { return true }); err == nil {
		t.Fatal("a record over the limit must be an error, not unbounded memory")
	}
}

func TestCommandsAreOneLinePerObject(t *testing.T) {
	var buf bytes.Buffer
	w := &cmdWriter{w: &buf}
	if err := w.send(map[string]any{"type": "prompt", "message": "line1\nline2 \u2028 end"}); err != nil {
		t.Fatal(err)
	}
	if bytes.Count(buf.Bytes(), []byte("\n")) != 1 || !bytes.HasSuffix(buf.Bytes(), []byte("\n")) {
		t.Fatalf("%q", buf.String())
	}
	var back map[string]any
	if err := json.Unmarshal(buf.Bytes(), &back); err != nil || back["message"] != "line1\nline2 \u2028 end" {
		t.Fatalf("%v %v", err, back)
	}
}
