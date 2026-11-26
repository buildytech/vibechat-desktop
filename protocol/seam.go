// Package protocol is what the desktop app and vibechat-server share: the host seam contract of the pinned studio
// commit (host-seam.json, generated in buildytech/studio with `bun run gen:host-seam`) and the token / ticket /
// session auth (protocol/auth). Neither side's code lives here.
package protocol

import (
	"encoding/json"
	"fmt"
	"strings"

	vibechat "github.com/buildytech/vibechat-desktop"
)

// Pinned is the host seam of the pinned studio commit (third_party/studio).
func Pinned() (Seam, error) { return LoadSeam(vibechat.HostSeam()) }

// Seam is host-seam.json: the seam version, the URL prefix, the capability fields, every route the Studio calls and
// the remote-host part (vibechat-server): protocol versions, the Hello fields, the session constants and the routes
// the desktop app itself calls (Remote).
type Seam struct {
	Seam              string      `json:"seam"`
	Prefix            string      `json:"prefix"`
	Capabilities      []string    `json:"capabilities"`
	Routes            []SeamRoute `json:"routes"`
	Protocol          int         `json:"protocol"`
	MinClientProtocol int         `json:"minClientProtocol"`
	Hello             []string    `json:"hello"`
	Auth              SeamAuth    `json:"auth"`
	Remote            []SeamRoute `json:"remote"`
	// Desktop are the routes the desktop app itself answers (desktop/internal/remote), never a server: its connection to
	// vibechat-server. DesktopConnection are the fields of their answer, TunnelUser the default SSH user.
	Desktop           []SeamRoute `json:"desktop"`
	DesktopConnection []string    `json:"desktopConnection"`
	TunnelUser        string      `json:"tunnelUser"`
	// DesktopProvision / DesktopProvisionInput are the fields of the setup wizard's answer and input,
	// ProvisionSteps its steps in order (desktop/internal/provision.Steps).
	DesktopProvision      []string `json:"desktopProvision"`
	DesktopProvisionInput []string `json:"desktopProvisionInput"`
	ProvisionSteps        []string `json:"provisionSteps"`
}

// SeamAuth are the session constants of the remote host (HOST_SESSION_PATH and friends).
type SeamAuth struct {
	SessionPath      string `json:"sessionPath"`
	Cookie           string `json:"cookie"`
	TicketParam      string `json:"ticketParam"`
	TicketTTLSeconds int    `json:"ticketTtlSeconds"`
}

// All is Routes then Remote.
func (s Seam) All() []SeamRoute {
	return append(append([]SeamRoute{}, s.Routes...), s.Remote...)
}

// SeamRoute is one method + Express-style path (":id" segments are parameters).
type SeamRoute struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	SSE    bool   `json:"sse,omitempty"`
}

// Key is "METHOD /path".
func (r SeamRoute) Key() string { return r.Method + " " + r.Path }

// LoadSeam parses host-seam.json.
func LoadSeam(raw []byte) (Seam, error) {
	var s Seam
	if err := json.Unmarshal(raw, &s); err != nil {
		return s, fmt.Errorf("host-seam.json: %w", err)
	}
	if s.Seam == "" || !strings.HasPrefix(s.Prefix, "/") || len(s.Routes) == 0 {
		return s, fmt.Errorf("host-seam.json: incomplete (seam %q, prefix %q, %d routes)", s.Seam, s.Prefix, len(s.Routes))
	}
	return s, nil
}
