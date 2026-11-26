//go:build !server

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	goruntime "runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"

	vibechat "github.com/buildytech/vibechat-desktop"
	"github.com/buildytech/vibechat-desktop/internal/appid"
	"github.com/buildytech/vibechat-desktop/internal/webapp"
)

// probeResult is what `vibechat --probe-localhost` learns about this platform's webview (WebView2, WKWebView,
// WebKitGTK): can a page on http://127.0.0.1:<port> frame and fetch http://<name>.preview.localhost:<port> (the
// server's preview gateway through the app's tunnel), and do the preview cookies come back.
type probeResult struct {
	OS            string `json:"os"`
	FrameLoaded   bool   `json:"frameLoaded"`   // the iframe document reached the fake gateway with Host *.preview.localhost
	FramePing     bool   `json:"framePing"`     // the framed page's same-origin fetch came back
	FrameReferer  string `json:"frameReferer"`  // what the gateway's same-origin rule sees
	FetchSiteSeen string `json:"fetchSiteSeen"` // Sec-Fetch-Site of that same-origin fetch
	CookieNone    bool   `json:"cookieNone"`    // SameSite=None; Secure; Partitioned cookie returned on http (iframe)
	CookieLax     bool   `json:"cookieLax"`     // SameSite=Lax cookie returned (iframe: expected false, cross-site)
	CORSFetch     bool   `json:"corsFetch"`     // the top page's cross-origin fetch to the name worked
	FetchError    string `json:"fetchError,omitempty"`
	// Why a failure: GoResolve is the system resolver's answer for the name (DNS), CORSLocalhost / CORSIP the same
	// fetch to http://localhost:<port> and http://127.0.0.1:<port> (macOS App Transport Security exempts both).
	GoResolve     string `json:"goResolve"`
	CORSLocalhost bool   `json:"corsLocalhost"`
	CORSIP        bool   `json:"corsIp"`
	UserAgent     string `json:"userAgent,omitempty"`
	TimedOut      bool   `json:"timedOut,omitempty"`
}

// probeLocalhost opens a window on a local page that frames and fetches a fake preview gateway at
// probe.preview.localhost; prints one JSON line; exit 0 when the frame and its same-origin fetch worked.
func probeLocalhost() int {
	var mu sync.Mutex
	res := probeResult{OS: appid.Name}
	done := make(chan struct{})
	var once sync.Once
	finish := func() { once.Do(func() { close(done) }) }

	gw4, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Println("probe-localhost: listen:", err)
		return 1
	}
	port := gw4.Addr().(*net.TCPAddr).Port
	gwHost := "probe.preview.localhost:" + strconv.Itoa(port)
	gw := http.NewServeMux()
	gw.HandleFunc("/frame", func(w http.ResponseWriter, r *http.Request) {
		if r.Host != gwHost {
			http.Error(w, "host "+r.Host, http.StatusForbidden)
			return
		}
		mu.Lock()
		res.FrameLoaded, res.FrameReferer = true, r.Header.Get("Referer")
		mu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: "p_none", Value: "1", Path: "/", Secure: true, SameSite: http.SameSiteNoneMode, Partitioned: true})
		http.SetCookie(w, &http.Cookie{Name: "p_lax", Value: "1", Path: "/", SameSite: http.SameSiteLaxMode})
		w.Header().Set("content-type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, `<!doctype html><title>frame</title><script>fetch("/ping").catch(()=>{})</script>`)
	})
	gw.HandleFunc("/ping", func(w http.ResponseWriter, r *http.Request) {
		_, none := r.Cookie("p_none")
		_, lax := r.Cookie("p_lax")
		mu.Lock()
		res.FramePing, res.FetchSiteSeen = r.Host == gwHost, r.Header.Get("Sec-Fetch-Site")
		res.CookieNone, res.CookieLax = none == nil, lax == nil
		mu.Unlock()
		_, _ = io.WriteString(w, "pong")
	})
	gw.HandleFunc("/cors", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("access-control-allow-origin", "*")
		_, _ = io.WriteString(w, "ok")
	})
	go func() { _ = http.Serve(gw4, gw) }()
	if gw6, err := net.Listen("tcp", "[::1]:"+strconv.Itoa(port)); err == nil {
		go func() { _ = http.Serve(gw6, gw) }() // *.localhost may resolve to ::1 first
	}

	if addrs, err := net.LookupHost("probe.preview.localhost"); err != nil {
		res.GoResolve = "error: " + err.Error()
	} else {
		res.GoResolve = strings.Join(addrs, ",")
	}

	page, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Println("probe-localhost: listen:", err)
		return 1
	}
	pm := http.NewServeMux()
	pm.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "text/html; charset=utf-8")
		_, _ = fmt.Fprintf(w, `<!doctype html><title>probe</title><iframe src="http://%[1]s/frame"></iframe>
<script>
(async () => {
  const r = { ua: navigator.userAgent }
  try { r.cors = (await fetch("http://%[1]s/cors")).ok } catch (e) { r.cors = false; r.err = String(e) }
  try { r.corsLocalhost = (await fetch("http://localhost:%[2]d/cors")).ok } catch (e) { r.corsLocalhost = false }
  try { r.corsIp = (await fetch("http://127.0.0.1:%[2]d/cors")).ok } catch (e) { r.corsIp = false }
  setTimeout(() => fetch("/result", { method: "POST", body: JSON.stringify(r) }), 3000)
})()
</script>`, gwHost, port)
	})
	pm.HandleFunc("/result", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			UA            string `json:"ua"`
			CORS          bool   `json:"cors"`
			CORSLocalhost bool   `json:"corsLocalhost"`
			CORSIP        bool   `json:"corsIp"`
			Err           string `json:"err"`
		}
		_ = json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body)
		mu.Lock()
		res.UserAgent, res.CORSFetch, res.FetchError = body.UA, body.CORS, body.Err
		res.CORSLocalhost, res.CORSIP = body.CORSLocalhost, body.CORSIP
		mu.Unlock()
		finish()
	})
	go func() { _ = http.Serve(page, pm) }()

	app := application.New(application.Options{
		Name:   appid.Name + " probe",
		Assets: application.AssetOptions{Handler: webapp.Handler(vibechat.Frontend())},
	})
	app.Window.NewWithOptions(application.WebviewWindowOptions{
		Title: "probe", Width: 480, Height: 320, URL: "http://" + page.Addr().String() + "/",
	})
	// Print and exit from here: on macOS app.Quit ends the process inside app.Run (the result was lost in CI).
	report := func() int {
		mu.Lock()
		defer mu.Unlock()
		res.OS = goos()
		out, _ := json.Marshal(res)
		fmt.Println("probe-localhost:", string(out))
		_ = os.Stdout.Sync()
		if res.FrameLoaded && res.FramePing {
			return 0
		}
		return 1
	}
	go func() {
		select {
		case <-done:
		case <-time.After(60 * time.Second):
			mu.Lock()
			res.TimedOut = true
			mu.Unlock()
		}
		os.Exit(report())
	}()
	if err := app.Run(); err != nil {
		fmt.Println("probe-localhost: app:", err)
	}
	return report()
}

func goos() string { return goruntime.GOOS + "/" + goruntime.GOARCH + " " + os.Getenv("RUNNER_OS") }
