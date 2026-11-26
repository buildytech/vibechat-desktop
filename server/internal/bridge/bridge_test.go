package bridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestChildEnvCarriesTheKeyOnlyOnce(t *testing.T) {
	env := childEnv([]string{"PATH=/bin", "CURSOR_API_KEY=old", "HOME=/home/u", "USERPROFILE=x"}, "new", "/agent")
	joined := strings.Join(env, "\n")
	if strings.Count(joined, "CURSOR_API_KEY=") != 1 || !strings.Contains(joined, "CURSOR_API_KEY=new") {
		t.Fatalf("key env: %q", joined)
	}
	if !strings.Contains(joined, "HOME=/agent") || strings.Contains(joined, "/home/u") {
		t.Fatalf("home env: %q", joined)
	}
}

// An inherited CURSOR_BACKEND_URL (or any CURSOR_*) must not steer the bridge to another backend.
func TestChildEnvDropsInheritedCursorVars(t *testing.T) {
	base := []string{"PATH=/bin", "Cursor_Backend_Url=https://elsewhere", "CURSOR_DATA_DIR=/x", "HTTPS_PROXY=http://p:3128"}
	joined := strings.Join(childEnv(base, "k", "/agent"), "\n")
	if strings.Contains(joined, "elsewhere") || strings.Contains(joined, "CURSOR_DATA_DIR") || !strings.Contains(joined, "HTTPS_PROXY=") {
		t.Fatalf("env: %q", joined)
	}
	if got := strings.Join(droppedCursorVars(base), ","); got != "Cursor_Backend_Url,CURSOR_DATA_DIR" {
		t.Fatalf("dropped: %q", got)
	}
}

func TestRedact(t *testing.T) {
	if got := Redact("x key_123 y", "key_123"); got != "x [redacted] y" {
		t.Fatal(got)
	}
}

func TestLocateChecksManifestAndHelper(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "vibechat")
	bdir := filepath.Join(dir, "bridge")
	if err := os.MkdirAll(filepath.Join(bdir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	m := Manifest{Protocol: "sdk.v1", OS: sdkOS(), Arch: sdkArch(), Entrypoint: "bin/cursor-sdk-bridge"}
	raw, _ := json.Marshal(m)
	_ = os.WriteFile(filepath.Join(bdir, "manifest.json"), raw, 0o644)
	if _, err := Locate(exe, func(string) string { return "" }); err == nil {
		t.Fatal("located without an entrypoint")
	}
	_ = os.WriteFile(filepath.Join(bdir, "bin", "cursor-sdk-bridge"), []byte("x"), 0o755)
	inst, err := Locate(exe, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if inst.HasSandboxHelper() {
		t.Fatal("helper reported without files")
	}
	if !strings.Contains(inst.HelperDir(), filepath.Join("node_modules", "@cursor", "sdk-")) {
		t.Fatal(inst.HelperDir())
	}
}

func frame(flag byte, v any) []byte {
	raw, _ := json.Marshal(v)
	var b bytes.Buffer
	b.WriteByte(flag)
	_ = binary.Write(&b, binary.BigEndian, uint32(len(raw)))
	b.Write(raw)
	return b.Bytes()
}

func TestConnectStreamAndUnary(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("authorization") != "Bearer tok" {
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"code":"unauthenticated","message":"no"}`))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/Ping") {
			_, _ = w.Write([]byte(`{"message":"pong"}`))
			return
		}
		body, _ := io.ReadAll(r.Body)
		if len(body) < 5 || body[0] != 0 {
			t.Errorf("bad request envelope")
		}
		_, _ = w.Write(frame(0, map[string]int{"n": 1}))
		_, _ = w.Write(frame(0, map[string]int{"n": 2}))
		_, _ = w.Write(frame(2, map[string]any{"error": map[string]string{"code": "canceled", "message": "stop"}}))
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, Token: "tok"}
	var pong struct{ Message string }
	if err := c.Unary(context.Background(), "SdkBridgeControlService", "Ping", struct{}{}, &pong); err != nil || pong.Message != "pong" {
		t.Fatal(err, pong)
	}
	var got []string
	err := c.Stream(context.Background(), "SdkAgentService", "Send", map[string]string{}, func(m json.RawMessage) error {
		got = append(got, string(m))
		return nil
	})
	if len(got) != 2 {
		t.Fatalf("got %v", got)
	}
	if e, ok := err.(*Error); !ok || e.Code != "canceled" {
		t.Fatalf("end error %v", err)
	}
	bad := &Client{BaseURL: srv.URL, Token: "x"}
	if err := bad.Unary(context.Background(), "S", "Ping", struct{}{}, nil); err == nil || !strings.Contains(err.Error(), "unauthenticated") {
		t.Fatal(err)
	}
}
