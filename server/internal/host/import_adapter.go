package host

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"net/http"
	"sort"
	"strings"

	"github.com/buildytech/vibechat-desktop/server/internal/templates"
)

// Import adapters: one per framework, in a registry, behind the shared checks of import.go. An import is
// core(framework) + data: the core is what every project of the framework shares with its template (tooling, build
// setup, the runtime code), the data layer is the site itself (page, fixtures, CSS, fonts, images). An adapter
// claims a ZIP, checks it, fills the core from the template where the ZIP has none and verifies the result. Adding a
// framework is one adapter + one template + one runner (see phpFlightPlan and docs/adr/0001-import-adapters-and-go-runner.md).

type importAdapter interface {
	// ID is the framework (the buildy.pack.v1 manifest's `framework`) and the template the project moves to.
	ID() string
	// Runner is the template runner the adapter's projects need (templates.json `runner`).
	Runner() string
	// Detect claims a ZIP that has no buildy.pack.v1 manifest (older packs, hand-made projects).
	Detect(in *importInput) bool
	// Validate refuses what the framework must not import (after the shared checks). It may read the template.
	Validate(ctx context.Context, in *importInput, tpl templates.Profile) error
	// Fill adds the core files the ZIP does not carry (tooling, settings) from the template into staging.
	Fill(staging string, in *importInput, tpl templates.Profile) error
	// Verify checks the written project (in the tools jail) after its dependencies are installed.
	Verify(ctx context.Context, dir string, tpl templates.Profile, log func(string)) (string, error)
	// DataPaths are the path prefixes (or exact paths) of the data layer.
	DataPaths() []string
}

// importAdapters is the registry, in detection order.
var importAdapters = []importAdapter{viteAdapter{"svelte"}, viteAdapter{"react"}, templAdapter{}}

// phpFlightPlan: the next adapter (not part of this build). framework "php-flight" (Flight or Slim 4 on PHP 8.x):
// Detect composer.json + public/index.php; Validate composer.json against an allowlist of packages from the
// template's composer.lock, no scripts (composer runs with --no-scripts), no ext-ffi, no phar or binaries, no
// .htaccess outside public/; Fill the template's tooling; Verify `php -l` over the tree and
// `composer install --no-scripts`; Runner "php-flight" (php -S 127.0.0.1:{port} -t public, readiness GET /).
const phpFlightPlan = "php-flight"

// importInput is the unpacked ZIP and its BuildY pack manifest.
type importInput struct {
	files []importFile
	by    map[string][]byte
	pack  importPack
}

// importPack is manifest.json of a BuildY Chat pack (format buildy.pack.v1); present is false for other ZIPs.
type importPack struct {
	present       bool
	Format        string `json:"format"`
	SchemaVersion int    `json:"schemaVersion"`
	Framework     string `json:"framework"`
	Preset        string `json:"preset"`
}

const packFormat = "buildy.pack.v1"

func newImportInput(files []importFile) *importInput {
	in := &importInput{files: files, by: fileMap(files)}
	if raw, ok := in.by["manifest.json"]; ok {
		var m importPack
		if json.Unmarshal(raw, &m) == nil && m.Format == packFormat {
			m.present = true
			in.pack = m
		}
	}
	return in
}

// selectImportAdapter: a buildy.pack.v1 manifest names the framework; without one, the adapters detect in order;
// a ZIP no adapter claims gets the 0.2.7 message.
func selectImportAdapter(in *importInput) (importAdapter, error) {
	if looksLikeChatExport(in.files) {
		return nil, importFail(http.StatusBadRequest, "This is a chat export (buildy.chat.v1 or buildy:v2), not a project ZIP")
	}
	if err := sharedImportChecks(in); err != nil {
		return nil, err
	}
	if in.pack.present {
		if in.pack.SchemaVersion != 1 {
			return nil, importFail(http.StatusBadRequest, "manifest.json schemaVersion must be 1 (buildy.pack.v1)")
		}
		fw := strings.TrimSpace(in.pack.Framework)
		for _, a := range importAdapters {
			if a.ID() == fw {
				return a, nil
			}
		}
		if fw == "" {
			return nil, importFail(http.StatusBadRequest, "manifest.json (buildy.pack.v1) names no framework")
		}
		return nil, importFail(http.StatusBadRequest, "This pack is for "+fw+", which VibeChat cannot import (it imports "+strings.Join(importAdapterIDs(), ", ")+")")
	}
	for _, a := range importAdapters {
		if a.Detect(in) {
			return a, nil
		}
	}
	if _, ok := in.by["ui8kit.config.json"]; ok {
		// A UI8Kit config with another framework: the Vite check answers why (framework, JSON).
		_, err := validateVite(in.by)
		return nil, err
	}
	return nil, importFail(http.StatusBadRequest, "ui8kit.config.json is required (or go.mod of a Go templ project)")
}

// importLayers counts the ZIP's files per layer: core (the same bytes as the template's file at that path), data
// (the adapter's data paths) and code (the rest: the page's own code). Reported to the client; the future deploy
// ships only data + code to a runtime that holds the core.
func importLayers(in *importInput, ad importAdapter, tpl templates.Profile) map[string]int {
	out := map[string]int{"core": 0, "data": 0, "code": 0}
	for _, f := range in.files {
		out[importLayerOf(f, ad.DataPaths(), tpl.Files)]++
	}
	return out
}

func importLayerOf(f importFile, data []string, core fs.FS) string {
	for _, d := range data {
		if f.path == d || (strings.HasSuffix(d, "/") && strings.HasPrefix(f.path, d)) {
			return "data"
		}
	}
	if core != nil {
		if raw, err := fs.ReadFile(core, f.path); err == nil && bytes.Equal(raw, f.data) {
			return "core"
		}
	}
	return "code"
}

// importAdapterIDs lists the registry (the error messages and tests).
func importAdapterIDs() []string {
	ids := make([]string, 0, len(importAdapters))
	for _, a := range importAdapters {
		ids = append(ids, a.ID())
	}
	sort.Strings(ids)
	return ids
}
