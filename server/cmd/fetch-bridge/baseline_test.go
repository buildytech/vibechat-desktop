package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"os"
	"testing"
)

// standalone builds a minimal Bun standalone layout: runtime bytes, payload, offsets, trailer, total size.
func standalone(name string, js []byte, extra int) []byte {
	var payload bytes.Buffer
	payload.WriteString(name + "\x00")
	jsOff := payload.Len()
	payload.Write(js)
	payload.WriteByte(0)
	modOff := payload.Len()
	rec := make([]byte, moduleSize)
	binary.LittleEndian.PutUint32(rec[0:], 0)
	binary.LittleEndian.PutUint32(rec[4:], uint32(len(name)))
	binary.LittleEndian.PutUint32(rec[8:], uint32(jsOff))
	binary.LittleEndian.PutUint32(rec[12:], uint32(len(js)))
	if extra > 0 { // a bytecode blob
		binary.LittleEndian.PutUint32(rec[24:], uint32(jsOff))
		binary.LittleEndian.PutUint32(rec[28:], uint32(extra))
	}
	rec[48], rec[49], rec[50] = 1, 1, 1
	payload.Write(rec)
	off := make([]byte, offsetsSize)
	binary.LittleEndian.PutUint64(off[0:], uint64(payload.Len()))
	binary.LittleEndian.PutUint32(off[8:], uint32(modOff))
	binary.LittleEndian.PutUint32(off[12:], moduleSize)
	binary.LittleEndian.PutUint32(off[28:], 0x0f)
	out := append([]byte("ELF-runtime-bytes"), payload.Bytes()...)
	out = append(out, off...)
	out = append(out, bunTrailer...)
	return binary.LittleEndian.AppendUint64(out, uint64(len(out)+8))
}

func TestExtractBundle(t *testing.T) {
	js := []byte("// @bun\nconsole.log('ready')\n")
	name, got, err := extractBundle(standalone("/$bunfs/root/cursor-sdk-bridge", js, 0))
	if err != nil || name != "/$bunfs/root/cursor-sdk-bridge" || !bytes.Equal(got, js) {
		t.Fatalf("extract: %q %q %v", name, got, err)
	}
	if _, _, err := extractBundle(standalone("B:/~BUN/root/cursor-sdk-bridge.exe", js, 4)); err == nil {
		t.Fatal("bytecode module accepted: it cannot be rebuilt from the JS alone")
	}
	if _, _, err := extractBundle([]byte("plain binary")); err == nil {
		t.Fatal("non-Bun binary accepted")
	}
	if _, _, err := extractBundle(standalone("/$bunfs/root/x", []byte("not bun output"), 0)); err == nil {
		t.Fatal("non-bundled module accepted")
	}
}

func TestBunTarget(t *testing.T) {
	for in, want := range map[string]string{"win32-x64": "windows-x64", "linux-x64": "linux-x64", "darwin-x64": "darwin-x64"} {
		if got := bunTarget(in); got != want {
			t.Errorf("bunTarget(%s) = %s, want %s", in, got, want)
		}
	}
}

// Every shipped x64 platform is rebuilt on a pinned baseline runtime; arm64 keeps the upstream build.
func TestLockPinsBaselineRuntime(t *testing.T) {
	raw, err := os.ReadFile("../../bridge.lock.json")
	if err != nil {
		t.Fatal(err)
	}
	var l lock
	if err := json.Unmarshal(raw, &l); err != nil {
		t.Fatal(err)
	}
	if l.Baseline.Bun == "" {
		t.Fatal("baselineRuntime.bun is empty")
	}
	for _, p := range []string{"linux-x64", "win32-x64", "darwin-x64"} {
		if !needsBaseline(l, p) || len(l.Baseline.SHA256[p]) != 64 {
			t.Errorf("%s: no pinned baseline runtime", p)
		}
	}
	for _, p := range []string{"darwin-arm64", "linux-arm64"} {
		if needsBaseline(l, p) {
			t.Errorf("%s must keep the upstream runtime", p)
		}
	}
}
