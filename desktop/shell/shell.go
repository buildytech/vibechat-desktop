// Package shell embeds the desktop app's start screen: buildytech/studio `build:shell` (apps/studio/dist-shell),
// copied into dist by scripts/build-shell.sh from the pinned studio submodule. It is not the Studio: connect a server
// or set up a new one (the wizard); the Studio itself always comes from the server.
package shell

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// Frontend is the built start screen (empty until scripts/build-shell.sh ran).
func Frontend() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err)
	}
	return sub
}
