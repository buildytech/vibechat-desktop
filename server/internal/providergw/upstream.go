package providergw

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }

// NewClient is the HTTP client of a provider: it connects only to the given hostnames (nothing else can be reached
// through the gateway, whatever a request or a redirect says), and trusts the extra CA bundle when caFile is set (the
// Russian Trusted Root CA for GigaChat).
func NewClient(hosts []string, caFile string) (*http.Client, error) {
	allowed := map[string]bool{}
	for _, h := range hosts {
		allowed[strings.ToLower(h)] = true
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("CA bundle: %w", err)
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("CA bundle: no certificate found in " + caFile)
		}
	}
	d := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
	tr := &http.Transport{
		TLSClientConfig:       &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		DialContext:           hostDialer(d, allowed),
		ForceAttemptHTTP2:     true,
		ResponseHeaderTimeout: 3 * time.Minute,
		IdleConnTimeout:       time.Minute,
	}
	return &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, nil
}

func hostDialer(d *net.Dialer, allowed map[string]bool) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(addr)
		if err != nil || !allowed[strings.ToLower(host)] {
			return nil, fmt.Errorf("egress policy: %q is not a host of this provider", addr)
		}
		return d.DialContext(ctx, network, addr)
	}
}

// copyStream copies an upstream body to w, flushing after every read (SSE).
func copyStream(w http.ResponseWriter, r io.Reader) error {
	fl, _ := w.(http.Flusher)
	buf := make([]byte, 8<<10)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return werr
			}
			if fl != nil {
				fl.Flush()
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}
