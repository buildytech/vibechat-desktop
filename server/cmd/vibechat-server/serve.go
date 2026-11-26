package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	vibechat "github.com/buildytech/vibechat-desktop"
	"github.com/buildytech/vibechat-desktop/internal/appid"
	"github.com/buildytech/vibechat-desktop/internal/privfile"
	"github.com/buildytech/vibechat-desktop/internal/webapp"
	"github.com/buildytech/vibechat-desktop/protocol/auth"
	assets "github.com/buildytech/vibechat-desktop/server"
	"github.com/buildytech/vibechat-desktop/server/internal/host"
	"github.com/buildytech/vibechat-desktop/server/internal/hostwire"
	"github.com/buildytech/vibechat-desktop/server/internal/preview"
)

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// loopbackAddr checks that addr is a loopback IP literal with a port: the server has no TLS and trusts the tunnel.
func loopbackAddr(addr string) (int, error) {
	h, ps, err := net.SplitHostPort(addr)
	if err != nil {
		return 0, err
	}
	ip := net.ParseIP(h)
	if ip == nil || !ip.IsLoopback() {
		return 0, fmt.Errorf("%s is not a loopback address: the server listens on 127.0.0.1 only (the desktop app reaches it through SSH)", addr)
	}
	port, err := strconv.Atoi(ps)
	if err != nil || port <= 0 || port > 65535 {
		return 0, fmt.Errorf("%s: bad port", addr)
	}
	return port, nil
}

// instanceID is the install's stable random id (hello.instanceId), made on first start.
func instanceID(p paths) (string, error) {
	if raw, err := privfile.Read(p.instanceID()); err == nil {
		if id := strings.TrimSpace(string(raw)); len(id) >= 16 {
			return id, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	id := hex.EncodeToString(b)
	return id, privfile.Write(p.instanceID(), []byte(id+"\n"))
}

// checkSeamAuth: the session constants of the pinned contract are the ones internal/auth speaks.
func checkSeamAuth(s host.Seam) error {
	a := s.Auth
	if a.SessionPath != auth.SessionPath || a.Cookie != auth.SessionCookie || a.TicketParam != auth.TicketParam ||
		time.Duration(a.TicketTTLSeconds)*time.Second != auth.TicketTTL {
		return fmt.Errorf("host-seam.json auth %+v does not match internal/auth", a)
	}
	return nil
}

// apiHandler is the server's front door: the session exchange, the seam and the Studio bundle, all behind the host
// token (hello and the session exchange are public), on loopback Host names only.
func apiHandler(s host.Seam, seam http.Handler, ta *auth.TokenAuth, studio http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.Handle(s.Auth.SessionPath, ta.SessionHandler())
	mux.Handle("/", host.Middleware(s.Prefix, seam)(studio))
	hello := s.Prefix + "/host/hello"
	public := func(r *http.Request) bool {
		return (r.URL.Path == hello && r.Method == http.MethodGet) || r.URL.Path == s.Auth.SessionPath
	}
	return auth.Middleware(ta, s.Prefix, public, mux)
}

func serve(p paths, args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	listen := fs.String("listen", envOr("VIBECHAT_LISTEN", defaultListen), "API + Studio address (loopback only)")
	previewListen := fs.String("preview-listen", envOr("VIBECHAT_PREVIEW_LISTEN", defaultPreviewListen), "preview gateway address (loopback only)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	fail := func(err error) int {
		log.Printf("vibechat-server: %v", err)
		return 1
	}
	if _, err := loopbackAddr(*listen); err != nil {
		return fail(err)
	}
	previewPort, err := loopbackAddr(*previewListen)
	if err != nil {
		return fail(err)
	}
	seam, err := host.LoadSeam(vibechat.HostSeam())
	if err != nil {
		return fail(err)
	}
	if err := checkSeamAuth(seam); err != nil {
		return fail(err)
	}
	ta, err := auth.LoadTokenAuth(p.tokenHash())
	if err != nil {
		return fail(err)
	}
	id, err := instanceID(p)
	if err != nil {
		return fail(fmt.Errorf("instance id: %w", err))
	}
	keys := p.keys()
	if err := keys.Check(); err != nil {
		log.Printf("cursor key not used: %v", err)
	}
	logf := func(s string) { log.Print(s) }
	gw := preview.NewGateway(logf, previewPort)
	h, closeHost, err := hostwire.New(seam, hostwire.Config{
		DataDir: p.data, ProjectsDir: p.projects, Keys: keys, Preview: gw,
		InstanceID: id, PreviewPort: previewPort, Tickets: ta,
	})
	if err != nil {
		return fail(err)
	}
	defer closeHost()
	if h.Capabilities().AgentUnsandboxed {
		log.Print("WARNING: the agent runs without the SDK sandbox on this machine; run `vibechat-server doctor`")
	}

	apiLn, err := net.Listen("tcp", *listen)
	if err != nil {
		return fail(err)
	}
	gwLn, err := net.Listen("tcp", *previewListen)
	if err != nil {
		apiLn.Close()
		return fail(err)
	}
	// No WriteTimeout: the event stream (SSE) and a long agent run stay open; idle keep-alives close.
	api := &http.Server{Handler: apiHandler(seam, h, ta, webapp.Handler(assets.Frontend())), ReadHeaderTimeout: 30 * time.Second, IdleTimeout: 2 * time.Minute}
	pv := &http.Server{Handler: gw.Gateway(), ReadHeaderTimeout: 30 * time.Second, IdleTimeout: 2 * time.Minute}
	errc := make(chan error, 2)
	go func() { errc <- api.Serve(apiLn) }()
	go func() { errc <- pv.Serve(gwLn) }()
	log.Printf("vibechat-server %s: api %s, preview gateway %s, data %s, instance %s", appid.Version, apiLn.Addr(), gwLn.Addr(), p.data, id)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	code := 0
	select {
	case s := <-sig:
		log.Printf("vibechat-server: %v, stopping", s)
	case err := <-errc:
		log.Printf("vibechat-server: %v", err)
		code = 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = api.Shutdown(ctx)
	_ = pv.Shutdown(ctx)
	closeHost()
	return code
}
