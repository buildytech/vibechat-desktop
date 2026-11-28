// Package auth is the remote host's access control (vibechat-server; the in-process Wails host has none).
//
// The host token is 32 random bytes the server prints once (`vibechat-server token new`); the server keeps only its
// sha256 (file 0600). The desktop app sends it as `Authorization: Bearer` from Go, never from the WebView, and trades
// it for a one-time ticket (60 s, POST /runtime/v1/auth/ticket); the window opens /auth/session?ticket=… and gets an
// HttpOnly SameSite=Strict session cookie. Every other request needs the Bearer token or that cookie, and the Host
// header must be a loopback literal (the server listens on 127.0.0.1 behind the SSH tunnel; no DNS rebinding).
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/buildytech/vibechat-desktop/internal/privfile"
)

const (
	// SessionCookie, SessionPath, TicketParam and TicketTTL are the contract's HOST_SESSION_COOKIE,
	// HOST_SESSION_PATH, HOST_TICKET_PARAM and HOST_TICKET_TTL_SECONDS (studio packages/agent-contract).
	SessionCookie = "vibechat_session"
	SessionPath   = "/auth/session"
	TicketParam   = "ticket"
	TicketTTL     = 60 * time.Second
	// SessionTTL is how long a window session lives without use; each authenticated request extends it.
	SessionTTL = 12 * time.Hour
	tokenBytes = 32
)

// ErrUnauthenticated: no valid Bearer token or session cookie.
var ErrUnauthenticated = errors.New("not signed in: open this server from the VibeChat app")

// Principal is who a request acts for.
type Principal struct {
	Subject string // "owner" for the host token (one user per server)
	Method  string // "bearer" | "session"
}

// Authenticator decides who a request is. The host token is the only one today; an OIDC one (Authfly) can follow.
type Authenticator interface {
	Authenticate(r *http.Request) (Principal, error)
}

// NewToken returns a fresh host token (base64url of 32 random bytes). Show it once; store only HashToken.
func NewToken() (string, error) {
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// HashToken is the hex sha256 the server keeps.
func HashToken(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

// WriteTokenHash stores HashToken(tok) at path (dir 0700, file 0600, atomic).
func WriteTokenHash(path, tok string) error {
	return privfile.Write(path, []byte(HashToken(tok)+"\n"))
}

// TokenAuth checks the host token and the window sessions it bought. Tickets and sessions live in memory: after a
// restart the app buys a new ticket.
type TokenAuth struct {
	hash []byte // sha256 of the token
	now  func() time.Time

	mu       sync.Mutex
	tickets  map[string]time.Time
	sessions map[string]time.Time
}

// LoadTokenAuth reads the token hash written by WriteTokenHash. The file must not be readable by others.
func LoadTokenAuth(path string) (*TokenAuth, error) {
	raw, err := privfile.Read(path)
	if err != nil {
		return nil, fmt.Errorf("host token: %w (create one with `vibechat-server token new`)", err)
	}
	hash, err := hex.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil || len(hash) != sha256.Size {
		return nil, fmt.Errorf("host token: %s is not a sha256 hash", path)
	}
	return &TokenAuth{hash: hash, now: time.Now, tickets: map[string]time.Time{}, sessions: map[string]time.Time{}}, nil
}

// NewTokenAuth is TokenAuth for a token held in memory (tests).
func NewTokenAuth(tok string) *TokenAuth {
	sum := sha256.Sum256([]byte(tok))
	return &TokenAuth{hash: sum[:], now: time.Now, tickets: map[string]time.Time{}, sessions: map[string]time.Time{}}
}

func (a *TokenAuth) bearer(r *http.Request) bool {
	h := r.Header.Get("Authorization")
	tok, ok := strings.CutPrefix(h, "Bearer ")
	if !ok || tok == "" {
		return false
	}
	sum := sha256.Sum256([]byte(strings.TrimSpace(tok)))
	return subtle.ConstantTimeCompare(sum[:], a.hash) == 1
}

func randomID() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// Authenticate accepts the Bearer host token or a live session cookie (and extends that session).
func (a *TokenAuth) Authenticate(r *http.Request) (Principal, error) {
	if a.bearer(r) {
		return Principal{Subject: "owner", Method: "bearer"}, nil
	}
	c, err := r.Cookie(SessionCookie)
	if err != nil || c.Value == "" {
		return Principal{}, ErrUnauthenticated
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	exp, ok := a.sessions[c.Value]
	now := a.now()
	if !ok || now.After(exp) {
		delete(a.sessions, c.Value)
		return Principal{}, ErrUnauthenticated
	}
	a.sessions[c.Value] = now.Add(SessionTTL)
	return Principal{Subject: "owner", Method: "session"}, nil
}

// IssueTicket mints a one-time ticket; only the Bearer token buys one (a session cookie cannot mint more sessions).
func (a *TokenAuth) IssueTicket(r *http.Request) (string, time.Time, error) {
	if !a.bearer(r) {
		return "", time.Time{}, ErrUnauthenticated
	}
	t := randomID()
	now := a.now()
	exp := now.Add(TicketTTL)
	a.mu.Lock()
	for k, e := range a.tickets {
		if now.After(e) {
			delete(a.tickets, k)
		}
	}
	a.tickets[t] = exp
	a.mu.Unlock()
	return t, exp, nil
}

// redeem consumes a ticket and opens a session.
func (a *TokenAuth) redeem(ticket string) (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	exp, ok := a.tickets[ticket]
	delete(a.tickets, ticket)
	now := a.now()
	if !ok || now.After(exp) {
		return "", false
	}
	for k, e := range a.sessions {
		if now.After(e) {
			delete(a.sessions, k)
		}
	}
	s := randomID()
	a.sessions[s] = now.Add(SessionTTL)
	return s, true
}

// SessionHandler is GET /auth/session?ticket=…[&next=/path]: a valid ticket sets the session cookie and moves on to
// next (a same-origin path, SafeNext) or "/".
func (a *TokenAuth) SessionHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "GET only")
			return
		}
		s, ok := a.redeem(r.URL.Query().Get(TicketParam))
		if !ok {
			writeError(w, http.StatusUnauthorized, "this sign-in link has expired: reconnect from the VibeChat app")
			return
		}
		http.SetCookie(w, &http.Cookie{Name: SessionCookie, Value: s, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
		WriteSignedInPage(w, r.URL.Query().Get(NextParam))
	})
}

// SignedInHTML moves the window on to "/" by itself. Not a 303: a redirect keeps the navigation's initiator (the
// desktop app's own origin when it connects from Settings, or a link elsewhere), which is cross-site for
// 127.0.0.1, so the SameSite=Strict cookie just set would not be sent to "/". A meta refresh is a new navigation
// started by this origin.
const SignedInHTML = `<!doctype html><html><head><meta charset="utf-8"><meta http-equiv="refresh" content="0;url=/"><title>VibeChat</title></head><body><a href="/">Continue</a></body></html>`

// NextParam names the path the sign-in page moves on to (the desktop app's File → Settings opens /settings).
const NextParam = "next"

// SafeNext is next when it is a path of this origin ("/settings", "/p/abc?x=1"), else "/": never a scheme, a host,
// "//" or "/\\" (protocol-relative), control characters or more than 512 bytes.
func SafeNext(next string) string {
	if next == "" || len(next) > 512 || next[0] != '/' || strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/\\") {
		return "/"
	}
	for _, c := range next {
		if c < 0x20 || c == 0x7f || c == '\\' {
			return "/"
		}
	}
	u, err := url.Parse(next)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil {
		return "/"
	}
	return next
}

// SignedInPage is SignedInHTML moving on to SafeNext(next) instead of "/".
func SignedInPage(next string) string {
	n := SafeNext(next)
	if n == "/" {
		return SignedInHTML
	}
	e := html.EscapeString(n)
	return `<!doctype html><html><head><meta charset="utf-8"><meta http-equiv="refresh" content="0;url=` + e + `"><title>VibeChat</title></head><body><a href="` + e + `">Continue</a></body></html>`
}

// WriteSignedInPage answers SignedInPage(next) (200, no-store).
func WriteSignedInPage(w http.ResponseWriter, next string) {
	page := SignedInPage(next)
	w.Header().Set("cache-control", "no-store")
	w.Header().Set("content-type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(page))
}

// LoopbackHost reports whether a Host header names this machine by a literal that DNS cannot repoint: 127.0.0.1,
// [::1] or localhost, on any port (the tunnel's local port differs from the server's).
func LoopbackHost(hostport string) bool {
	h := hostport
	if hh, _, err := net.SplitHostPort(hostport); err == nil {
		h = hh
	}
	h = strings.Trim(h, "[]")
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// Middleware guards next: a loopback Host always, then a principal unless public(r). JSON errors under apiPrefix,
// plain text elsewhere. The ResponseWriter is passed on untouched, so SSE keeps http.Flusher.
func Middleware(a Authenticator, apiPrefix string, public func(*http.Request) bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		api := r.URL.Path == apiPrefix || strings.HasPrefix(r.URL.Path, apiPrefix+"/")
		if !LoopbackHost(r.Host) {
			deny(w, api, http.StatusMisdirectedRequest, "this server answers only on 127.0.0.1 (through the VibeChat app's tunnel)")
			return
		}
		if public == nil || !public(r) {
			if _, err := a.Authenticate(r); err != nil {
				deny(w, api, http.StatusUnauthorized, err.Error())
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func deny(w http.ResponseWriter, api bool, code int, msg string) {
	if api {
		writeError(w, code, msg)
		return
	}
	w.Header().Set("content-type", "text/plain; charset=utf-8")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(msg + "\n"))
}

func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
