package hostwire

import (
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type brokenConsole struct{}

func (brokenConsole) Write([]byte) (int, error) { return 0, errors.New("the handle is invalid") }

// A GUI build has no valid stderr; the log file must still get every line (it stayed empty with io.MultiWriter).
func TestLogWriterFileFirst(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vibechat.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var nilFile *os.File // what os.Stderr is when the process has no console handle
	for _, console := range []interface{ Write([]byte) (int, error) }{brokenConsole{}, nilFile} {
		l := log.New(&logWriter{file: f, console: console}, "", 0)
		l.Print("bridge: cursor-sdk-bridge exited before it was ready (exit code 3221225501 = 0xC000001D)")
	}
	b, _ := os.ReadFile(path) // no Close: the line is synced already
	if n := strings.Count(string(b), "0xC000001D"); n != 2 {
		t.Fatalf("log file has %d lines, want 2: %q", n, b)
	}
}
