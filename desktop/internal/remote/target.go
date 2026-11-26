package remote

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/buildytech/vibechat-desktop/internal/privfile"
)

// Target is the saved server (<data>/remote.json, 0600): no secrets, those are in the keychain.
type Target struct {
	Address string `json:"address"`
	User    string `json:"user"`
	// HostKey is the pinned SSH host key (authorized_keys format), "" until the first connect.
	HostKey string `json:"hostKey,omitempty"`
	// Active: the app connects at start (the user did not choose Disconnect).
	Active bool `json:"active"`
	// ProxyPort is the window's local port, kept so the Studio's local storage (per origin) survives restarts.
	ProxyPort int `json:"proxyPort,omitempty"`
}

func loadTarget(path string) (*Target, error) {
	raw, err := privfile.Read(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var t Target
	if err := json.Unmarshal(raw, &t); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if t.Address == "" || t.User == "" {
		return nil, nil
	}
	return &t, nil
}

func saveTarget(path string, t *Target) error {
	raw, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	return privfile.Write(path, raw)
}

func removeTarget(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

var (
	userRe = regexp.MustCompile(`^[a-z_][a-z0-9_.-]{0,63}$`)
	hostRe = regexp.MustCompile(`^[A-Za-z0-9.-]{1,253}$`)
)

// sshAddr checks "host", "host:port" or "[v6]:port" and answers host:port (default 22).
func sshAddr(address string) (string, error) {
	a := strings.TrimSpace(address)
	if a == "" || len(a) > 255 {
		return "", errors.New("enter the server address (host or host:port)")
	}
	host, port := a, "22"
	if h, p, err := net.SplitHostPort(a); err == nil {
		host, port = h, p
	} else if strings.Count(a, ":") > 1 {
		host = strings.Trim(a, "[]") // a bare IPv6 address
	}
	if ip := net.ParseIP(host); ip == nil && !hostRe.MatchString(host) {
		return "", fmt.Errorf("%q is not a host name or IP address", host)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return "", fmt.Errorf("%q is not a port", port)
	}
	return net.JoinHostPort(host, port), nil
}

func checkUser(u string) error {
	if !userRe.MatchString(u) {
		return fmt.Errorf("%q is not a Unix user name", u)
	}
	return nil
}
