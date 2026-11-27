package isolate

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/buildytech/vibechat-desktop/server/internal/procgroup"
)

// TestJailCannotCallLoopbackWithoutToken: the project's tools share the host network (so the preview proxy can
// reach Vite), but a service on 127.0.0.1 that requires a bearer token (the agent bridge) rejects a call from the
// jail that has no token. The token file in the host /tmp is not visible either (the jail has a private /tmp).
func TestJailCannotCallLoopbackWithoutToken(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("bubblewrap is Linux only")
	}
	if _, err := probe(); err != nil {
		t.Skipf("bwrap not usable here: %v", err)
	}
	token := "test-bridge-token-not-a-secret"
	tokenDir, err := os.MkdirTemp("", "cursor-sdk-bridge-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tokenDir) })
	tokenFile := filepath.Join(tokenDir, "auth-token")
	if err := os.WriteFile(tokenFile, []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	mux := http.NewServeMux()
	mux.HandleFunc("/sdk.v1.SdkBridgeControlService/Ping", func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("authorization"), "Bearer ")
		if got != token {
			http.Error(w, `{"code":"unauthenticated","message":"missing or invalid token"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	addr := ln.Addr().String()

	proj := t.TempDir()
	Configure(Config{Require: true})
	t.Cleanup(func() { Configure(Config{}) })
	script := fmt.Sprintf(`set -u
code=$(curl -s -o /tmp/body -w "%%{http_code}" -X POST --http1.1 -H "content-type: application/json" http://%s/sdk.v1.SdkBridgeControlService/Ping || echo curlfail)
echo "unauthorized:$code"
if cat %s 2>/dev/null; then echo "LEAK:token-file"; fi
echo done`, addr, tokenFile)
	argv, env, mode, err := Command([]string{"/bin/sh", "-c", script}, Spec{Dir: proj})
	if err != nil {
		t.Fatal(err)
	}
	if mode != ModeJail {
		t.Fatalf("mode %s", mode)
	}
	cmd := procgroup.Command(argv[0], argv[1:]...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	got := string(out)
	if strings.Contains(got, "LEAK") {
		t.Fatalf("token file visible in the jail: %s", got)
	}
	if !strings.Contains(got, "unauthorized:401") {
		t.Fatalf("expected 401 without a token, got: %s", got)
	}
	if !strings.Contains(got, "done") {
		t.Fatalf("script did not finish: %s", got)
	}
}
