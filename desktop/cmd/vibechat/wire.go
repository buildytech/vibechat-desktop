package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/buildytech/vibechat-desktop/desktop/internal/remote"
	"github.com/buildytech/vibechat-desktop/internal/appid"
	"github.com/buildytech/vibechat-desktop/protocol"
)

// dataDir is <UserConfigDir>/tw.buildy.vibechat (remote.json, logs); VIBECHAT_DATA_DIR overrides it (tests,
// portable installs). The same folder as earlier versions, so a saved server and its pin survive the update.
func dataDir() string {
	if d := os.Getenv("VIBECHAT_DATA_DIR"); d != "" {
		return d
	}
	base, err := os.UserConfigDir()
	if err != nil || base == "" {
		base = os.TempDir()
	}
	return filepath.Join(base, appid.ID)
}

// seamNotFound answers every seam path the app does not own with 404 { error }: the app has no local host, and the
// start screen must never get the SPA page for an API call.
func seamNotFound(prefix string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == prefix || strings.HasPrefix(r.URL.Path, prefix+"/") {
			w.Header().Set("content-type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "not connected to a server"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// newRemote is the connection to the owner's vibechat-server (SSH key and host token in the OS keychain, the saved
// server in <data>/remote.json). The app speaks the pinned seam's protocol and accepts servers down to its
// minClientProtocol (one compatibility window for both sides).
func newRemote(seam protocol.Seam, data string, navigate func(string)) *remote.Manager {
	m, err := remote.New(remote.Options{
		DataDir: data, Creds: remote.Keyring{}, AppVersion: appid.Version,
		AppProtocol: seam.Protocol, MinServerProtocol: seam.MinClientProtocol,
		Prefix: seam.Prefix, SessionPath: seam.Auth.SessionPath, TicketParam: seam.Auth.TicketParam,
		Navigate: navigate, Log: func(s string) { log.Print(s) }, PreviewPortPerProject: previewPortPerProject(),
	})
	if err != nil {
		log.Fatal(err)
	}
	return m
}

// previewPortPerProject: macOS does not resolve *.localhost (CI probe: the system resolver answers "no such host",
// WKWebView "Load failed"; WebView2 and WebKitGTK resolve it), so there every preview gets its own 127.0.0.1 port.
// VIBECHAT_PREVIEW_PORTS=1 / 0 forces it on / off.
func previewPortPerProject() bool {
	switch os.Getenv("VIBECHAT_PREVIEW_PORTS") {
	case "1":
		return true
	case "0":
		return false
	}
	return runtime.GOOS == "darwin"
}
