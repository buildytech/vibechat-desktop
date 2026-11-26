// Package applog is the log file of the desktop app and vibechat-server: <data>/logs/<name>, synced line by line.
package applog

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
)

// Setup sends the log to <data>/logs/<name> as well (the GUI build has no console; the bridge output goes there
// with the key redacted); the file rolls over at 5 MB. It answers the file, or "" when it cannot write there.
func Setup(data, name string) string {
	dir := filepath.Join(data, "logs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ""
	}
	file := filepath.Join(dir, name)
	if st, err := os.Stat(file); err == nil && st.Size() > 5<<20 {
		_ = os.Rename(file, file+".1")
	}
	f, err := os.OpenFile(file, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return ""
	}
	log.SetOutput(&logWriter{file: f, console: os.Stderr})
	return file
}

// logWriter writes every log line to the file first and syncs it, then to the console. A windowsgui app has no
// valid stderr: io.MultiWriter(os.Stderr, f) stopped at that error and never wrote the file (an empty vibechat.log).
type logWriter struct {
	mu      sync.Mutex
	file    *os.File
	console io.Writer
}

func (w *logWriter) Write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.file.Write(b)
	_ = w.file.Sync()
	if w.console != nil {
		_, _ = w.console.Write(b) // ignored: no console in the GUI build
	}
	return n, err
}
