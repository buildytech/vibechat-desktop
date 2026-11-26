package remote

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// previewPorts is the fallback for webviews that do not resolve http://<id>.preview.localhost (WKWebView on macOS,
// see --probe-localhost): every preview gets its own local origin http://127.0.0.1:<P>, a reverse proxy that talks to
// the server's gateway with Host <id>.preview.localhost:<gw> (and Origin / Referer mapped the same way, so the
// gateway's same-origin rule holds). The window proxy rewrites the preview URLs in the seam's JSON answers to those
// origins; the local gateway port itself becomes an HTTP proxy that maps a request from http://127.0.0.1:<P> (Vite's
// HMR WebSocket uses location.hostname with the gateway port) to its project.
type previewPorts struct {
	s    *session
	gw   int
	logf func(string)

	mu     sync.Mutex
	byID   map[string]string // id → http://127.0.0.1:<P>
	byPort map[string]string // "<P>" → id
	tr     *http.Transport
}

func newPreviewPorts(s *session, gw int) *previewPorts {
	pp := &previewPorts{s: s, gw: gw, logf: s.logf, byID: map[string]string{}, byPort: map[string]string{}}
	gwAddr := net.JoinHostPort("127.0.0.1", strconv.Itoa(gw))
	pp.tr = &http.Transport{
		DialContext:     func(ctx context.Context, _, _ string) (net.Conn, error) { return s.dial(ctx, gwAddr) },
		MaxIdleConns:    16,
		IdleConnTimeout: 90 * time.Second,
	}
	return pp
}

func (pp *previewPorts) gatewayOrigin(id string) string {
	return "http://" + id + ".preview.localhost:" + strconv.Itoa(pp.gw)
}

// origin answers the local origin of project id, starting its listener the first time.
func (pp *previewPorts) origin(id string) (string, error) {
	pp.mu.Lock()
	defer pp.mu.Unlock()
	if o, ok := pp.byID[id]; ok {
		return o, nil
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	local := "http://127.0.0.1:" + port
	srv := &http.Server{ReadHeaderTimeout: 30 * time.Second, Handler: pp.proxy(id, local)}
	pp.s.mu.Lock()
	pp.s.servers = append(pp.s.servers, srv)
	pp.s.lns = append(pp.s.lns, ln)
	pp.s.mu.Unlock()
	go func() { _ = srv.Serve(ln) }()
	pp.byID[id], pp.byPort[port] = local, id
	pp.logf(fmt.Sprintf("remote: preview %s on %s (port per project)", id, local))
	return local, nil
}

// proxy forwards to the gateway as <id>.preview.localhost:<gw>; local is the origin the webview sees ("" = the
// local gateway port, where the origin comes from the request).
func (pp *previewPorts) proxy(id, local string) http.Handler {
	gwOrigin := pp.gatewayOrigin(id)
	target := &url.URL{Scheme: "http", Host: "127.0.0.1:" + strconv.Itoa(pp.gw)}
	return &httputil.ReverseProxy{
		Transport:     pp.tr,
		FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			from := local
			if from == "" {
				from = pr.In.Header.Get("Origin")
			}
			pr.SetURL(target)
			pr.Out.Host = strings.TrimPrefix(gwOrigin, "http://")
			for _, h := range []string{"Origin", "Referer"} {
				if v := pr.Out.Header.Get(h); v != "" && from != "" && strings.HasPrefix(v, from) {
					pr.Out.Header.Set(h, gwOrigin+strings.TrimPrefix(v, from))
				}
			}
		},
		ModifyResponse: func(res *http.Response) error {
			if loc := res.Header.Get("Location"); local != "" && strings.HasPrefix(loc, gwOrigin) {
				res.Header.Set("Location", local+strings.TrimPrefix(loc, gwOrigin))
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			http.Error(w, "preview unreachable over SSH: "+err.Error(), http.StatusBadGateway)
		},
	}
}

// gatewayHandler serves the local gateway port: <id>.preview.localhost names go through unchanged, a request from a
// per-project origin (HMR) goes to that project.
func (pp *previewPorts) gatewayHandler() http.Handler {
	pass := &httputil.ReverseProxy{
		Transport:     pp.tr,
		FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(&url.URL{Scheme: "http", Host: "127.0.0.1:" + strconv.Itoa(pp.gw)})
			pr.Out.Host = pr.In.Host
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(strings.ToLower(hostOnly(r.Host)), ".preview.localhost") {
			pass.ServeHTTP(w, r)
			return
		}
		if o, err := url.Parse(r.Header.Get("Origin")); err == nil && o.Hostname() == "127.0.0.1" {
			pp.mu.Lock()
			id, ok := pp.byPort[o.Port()]
			pp.mu.Unlock()
			if ok {
				pp.proxy(id, "").ServeHTTP(w, r)
				return
			}
		}
		http.Error(w, "no preview here", http.StatusNotFound)
	})
}

func hostOnly(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return hostport
}

// rewritable: the seam answers that carry preview URLs (project list and card, clone, ensure, status, grant).
var rewritable = regexp.MustCompile(`^/projects(/[^/]+/(clone|ensure|status|preview/grant))?$`)

// rewriteJSON replaces the gateway preview origins in a seam JSON answer with the per-project local origins.
func (pp *previewPorts) rewriteJSON(prefix string, res *http.Response) error {
	rel, ok := strings.CutPrefix(res.Request.URL.Path, prefix)
	if !ok || !rewritable.MatchString(rel) || !strings.HasPrefix(res.Header.Get("content-type"), "application/json") {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, 16<<20))
	_ = res.Body.Close()
	if err != nil {
		return err
	}
	re := regexp.MustCompile(`http://([0-9a-f]{1,63})\.preview\.localhost:` + strconv.Itoa(pp.gw))
	var ferr error
	out := re.ReplaceAllFunc(body, func(m []byte) []byte {
		id := string(re.FindSubmatch(m)[1])
		o, err := pp.origin(id)
		if err != nil {
			ferr = err
			return m
		}
		return []byte(o)
	})
	if ferr != nil {
		pp.logf("remote: preview port: " + ferr.Error())
	}
	res.Body = io.NopCloser(bytes.NewReader(out))
	res.ContentLength = int64(len(out))
	res.Header.Set("content-length", strconv.Itoa(len(out)))
	return nil
}
