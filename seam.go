// Package vibechat embeds the host seam contract of the pinned studio commit (git submodule third_party/studio,
// packages/agent-contract/host-seam.json, generated there with `bun run gen:host-seam`; never edited here).
// protocol.Pinned parses it. The layout: protocol/ (shared), server/ (vibechat-server), desktop/ (the app).
package vibechat

import _ "embed"

//go:embed third_party/studio/packages/agent-contract/host-seam.json
var hostSeam []byte

// HostSeam is packages/agent-contract/host-seam.json of the pinned studio commit.
func HostSeam() []byte { return hostSeam }
