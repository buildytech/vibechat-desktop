// Package providergw is the host-side gateway between an isolated coding agent (Pi) and a model provider of its
// contour (Yandex AI Studio, GigaChat). The agent's models.json points at the gateway on 127.0.0.1; the gateway adds
// the real provider credential, which it reads from the server's own environment at request time. The credential is
// never written to a file, never put in an argv and never given to the isolated process: inside the jail there is
// only a per-run gateway token, which is worth nothing outside this process.
//
// A Provider speaks OpenAI Chat Completions to the agent. Yandex AI Studio is OpenAI-compatible, so its provider is a
// pass-through that only authenticates; GigaChat is not (functions instead of tools, a 30-minute OAuth token), so its
// provider translates both ways.
package providergw

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// Provider is one model provider behind the gateway.
type Provider interface {
	// Name is the provider id of the contour config ("yandex", "gigachat").
	Name() string
	// Hosts are the only hostnames this provider's requests may reach (the egress policy of the contour).
	Hosts() []string
	// Ready is nil when the provider has what it needs to answer (its credential in the environment).
	Ready() error
	// Chat forwards one OpenAI chat-completions request and writes the OpenAI-format answer (plain or SSE). It
	// returns an error only when nothing was written yet.
	Chat(ctx context.Context, body []byte, w http.ResponseWriter) error
}

const maxBody = 32 << 20

// Gateway is the loopback listener the agent talks to.
type Gateway struct {
	p   Provider
	log func(string)

	mu     sync.Mutex
	ln     net.Listener
	srv    *http.Server
	tokens map[string]string // token -> short id for logs
}

// New builds a gateway for p; log gets one line per request (never a header, a body or a credential).
func New(p Provider, log func(string)) *Gateway {
	if log == nil {
		log = func(string) {}
	}
	return &Gateway{p: p, log: log, tokens: map[string]string{}}
}

// Provider is the provider behind the gateway.
func (g *Gateway) Provider() Provider { return g.p }

// Start listens on a free 127.0.0.1 port and returns the base URL the agent uses ("http://127.0.0.1:PORT/v1").
func (g *Gateway) Start() (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.ln != nil {
		return "", errors.New("gateway already started")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	g.ln = ln
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", g.chat)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 30 * time.Second, IdleTimeout: time.Minute}
	g.srv = srv
	go func() { _ = srv.Serve(ln) }()
	return "http://" + ln.Addr().String() + "/v1", nil
}

// Close stops the listener.
func (g *Gateway) Close() {
	g.mu.Lock()
	srv := g.srv
	g.srv, g.ln = nil, nil
	g.mu.Unlock()
	if srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		_ = srv.Close()
	}
}

// Mint makes a gateway token for one agent run and its revoke func. The token is what the agent sends as its "API
// key"; it only opens this gateway, only while the run lasts.
func (g *Gateway) Mint() (token string, revoke func()) {
	var b [24]byte
	_, _ = rand.Read(b[:])
	token = "vcgw_" + hex.EncodeToString(b[:])
	g.mu.Lock()
	g.tokens[token] = token[:9]
	g.mu.Unlock()
	return token, func() {
		g.mu.Lock()
		delete(g.tokens, token)
		g.mu.Unlock()
	}
}

func (g *Gateway) authorize(r *http.Request) (string, bool) {
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if got == "" || got == r.Header.Get("Authorization") {
		return "", false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for tok, id := range g.tokens {
		if subtle.ConstantTimeCompare([]byte(tok), []byte(got)) == 1 {
			return id, true
		}
	}
	return "", false
}

func (g *Gateway) chat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		WriteError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	id, ok := g.authorize(r)
	if !ok {
		WriteError(w, http.StatusUnauthorized, "invalid gateway token")
		return
	}
	g.serveChat(w, r, id)
}

// ServeUnix serves the gateway on a Unix socket at path, for one agent run: no token is involved, the socket is the
// credential. Only the run's own jail gets it (the folder of the socket is private to the server user and is bound
// into that jail alone, whose network namespace has nothing else to talk to). It returns the func that stops the
// listener and removes the socket.
func (g *Gateway) ServeUnix(path, id string) (stop func(), err error) {
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	_ = os.Chmod(path, 0o600)
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			WriteError(w, http.StatusMethodNotAllowed, "POST only")
			return
		}
		g.serveChat(w, r, id)
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 30 * time.Second, IdleTimeout: time.Minute}
	go func() { _ = srv.Serve(ln) }()
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		_ = srv.Close()
		_ = os.Remove(path)
	}, nil
}

// serveChat forwards one authorized chat-completions request to the provider.
func (g *Gateway) serveChat(w http.ResponseWriter, r *http.Request, id string) {
	start := time.Now()
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil || len(body) > maxBody {
		WriteError(w, http.StatusRequestEntityTooLarge, "request too large")
		return
	}
	if err := g.p.Ready(); err != nil {
		WriteError(w, http.StatusServiceUnavailable, err.Error())
		g.log(fmt.Sprintf("gateway %s: %s not ready: %v", id, g.p.Name(), err))
		return
	}
	sw := &statusWriter{ResponseWriter: w}
	if err := g.p.Chat(r.Context(), body, sw); err != nil {
		if !sw.wrote {
			WriteError(w, http.StatusBadGateway, g.p.Name()+": "+err.Error())
		}
		g.log(fmt.Sprintf("gateway %s: %s failed after %s: %v", id, g.p.Name(), time.Since(start).Round(time.Millisecond), err))
		return
	}
	g.log(fmt.Sprintf("gateway %s: %s %d in %s", id, g.p.Name(), sw.status, time.Since(start).Round(time.Millisecond)))
}

type statusWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (s *statusWriter) WriteHeader(code int) {
	s.status, s.wrote = code, true
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	if !s.wrote {
		s.WriteHeader(http.StatusOK)
	}
	return s.ResponseWriter.Write(b)
}

func (s *statusWriter) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// WriteError answers an OpenAI-style error object.
func WriteError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(code)
	b, _ := jsonMarshal(map[string]any{"error": map[string]any{"message": msg, "type": "gateway_error", "code": code}})
	_, _ = w.Write(b)
}
