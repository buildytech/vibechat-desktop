// Package server holds what vibechat-server embeds: the built Studio (frontend/dist, scripts/build-frontend.sh from
// the pinned studio submodule) and the project templates (third_party/templates, see templates.go). Nothing here is
// edited by hand.
package server

import (
	"embed"
	"io/fs"
)

//go:embed all:frontend/dist
var frontend embed.FS

// Frontend is the built Studio (empty until scripts/build-frontend.sh ran).
func Frontend() fs.FS {
	sub, err := fs.Sub(frontend, "frontend/dist")
	if err != nil {
		panic(err)
	}
	return sub
}
