// Package vibechat holds the files the desktop host embeds from its pinned dependencies: buildytech/studio (git
// submodule third_party/studio: the built Studio and the host seam contract) and buildytech/templates
// (third_party/templates, see templates.go). Nothing here is edited by hand:
// frontend/dist is produced by scripts/build-frontend.sh from the submodule; host-seam.json is generated in studio
// (`bun run gen:host-seam`).
package vibechat

import (
	"embed"
	"io/fs"
)

//go:embed all:frontend/dist
var frontend embed.FS

//go:embed third_party/studio/packages/agent-contract/host-seam.json
var hostSeam []byte

// Frontend is the built Studio (empty until scripts/build-frontend.sh ran).
func Frontend() fs.FS {
	sub, err := fs.Sub(frontend, "frontend/dist")
	if err != nil {
		panic(err)
	}
	return sub
}

// HostSeam is packages/agent-contract/host-seam.json of the pinned studio commit.
func HostSeam() []byte { return hostSeam }
