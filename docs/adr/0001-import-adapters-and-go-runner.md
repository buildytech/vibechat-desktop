# ADR 0001: Import adapters, the Go templ runner, core + data layers

Status: accepted (0.2.8). Related: buildytech/templates ADR 0004 (the templ template as a checked layer over Empty).

## Context

0.2.7 imported a ZIP into an empty project for svelte and react only, with one function that knew both. BuildY
Chat now also exports a Go templ site (fastygo/starter: Go + PocketBase + templ), both exports carry a
`manifest.json` of the pack format `buildy.pack.v1` whose `framework` names the export, and the next framework
(php-flight: Flight or Slim 4) is planned. A Go project also needs a runtime on the server: the toolchain, module
downloads, a build, and a live preview.

## Decision

### 1. Import is shared checks + one adapter per framework

`server/internal/host/import.go` is the one place of what every framework shares: emptiness (only `.git`), the
limits (32 MiB compressed, 128 MiB uncompressed, 5000 files, 8 MiB per file), path safety (absolute paths, `..`,
`node_modules`, `.git`, `.env*` except `.env.example`), no symbolic links, no executable binaries (ELF, Mach-O,
PE), the staging write (executable bits kept) and the flow (`ImportInto`: unzip → select adapter → template →
validate → write; the handler then commits, installs and verifies).

`import_adapter.go` is the registry:

```go
type importAdapter interface {
    ID() string          // the pack's framework and the template the project moves to
    Runner() string      // the templates.json runner its projects need
    Detect(in) bool      // ZIPs without a buildy.pack.v1 manifest
    Validate(ctx, in, tpl) error
    Fill(staging, in, tpl) error  // the core files the ZIP lacks, from the template
    Verify(ctx, dir, tpl, log) (string, error)
    DataPaths() []string // the data layer
}
```

Detection: a `manifest.json` with `format: buildy.pack.v1` names the adapter by `framework` (unknown → 400 naming
the frameworks this build imports); without one, the adapters detect in order (svelte/react: the framework of a root
`ui8kit.config.json`, as 0.2.7 and older Chat packs; templ: a root `go.mod` without `ui8kit.config.json`).

Adapters in 0.2.8:

- **svelte, react** (`import_vite.go`): the 0.2.7 checks and overlay, unchanged (regression tests: the 0.2.7
  quiet-rooms pack and the current Chat Svelte pack with its manifest). A manifest whose framework disagrees with
  `ui8kit.config.json` is refused.
- **templ** (`import_templ.go`): `go.mod`, `go.sum`, `cmd/site/main.go` required; every `go.mod` in the ZIP
  parsed (`golang.org/x/mod/modfile`): `go` 1.22 … the pinned toolchain (1.27.4), `toolchain` not above it, `tool`
  only `github.com/a-h/templ/cmd/templ`, `replace` only to a folder inside the project that has the replaced
  module's `go.mod`, `require` only modules of the template's `go.sum` files (plus the ZIP's own local modules);
  every `go.sum` line well-formed and of an allowed module; every `.go` file parses, no `import "C"`, no
  `//go:cgo_*`, `//go:generate` only `go run ./gen` (Chat's page generator) or `go tool templ generate`; no C, C++,
  assembly, `.syso` or object files, no `vendor/`, `go.work`, `bin/`, `pb_data/`, `.vibechat/`. Fill adds the
  starter's tooling (`AGENTS.md`, `Makefile`, `.gitignore`, `LICENSE`, `PRIVACY.md`, `.cursor/`, `scripts/`,
  `skills/`), `.vibechat/` in `.gitignore`, and the layer's executables 0755. Verify: `go build` in the tools jail
  under the server's build lock.
- **php-flight** (plan, `phpFlightPlan`): Detect `composer.json` + `public/index.php`; Validate `composer.json`
  against the template's `composer.lock` packages, no Composer scripts, no `ext-ffi`, no phar or binaries; Fill the
  template's tooling; Verify `php -l` and `composer install --no-scripts` in the jail; runner `php-flight`
  (`php -S 127.0.0.1:{port} -t public`). Adding it is the adapter, the template and the runner; nothing in the shared
  code changes.

### 2. The go-templ runner (variant A: inside vibechat-owner)

- Toolchain: `internal/gopin` pins Go 1.27.4 linux-amd64 by URL, size and SHA-256. The setup wizard downloads it
  on the host, checks the SHA-256 and unpacks it into `/opt/vibechat/go` in the container before the package
  installs; the package's postinstall runs `vibechat-server ensure-go` (a no-op when current; else download, size
  + SHA-256 check, unpack beside, swap), so a server update moves to the next pin. `/opt/vibechat/go/bin` is on the
  unit's PATH; GOROOT is outside the data folder, so the jail sees it read-only.
- Egress: `proxy.golang.org`, `storage.googleapis.com` (the proxy answers module ZIP downloads with a redirect there;
  added in 0.2.9), `sum.golang.org`, `dl.google.com` on the allowlist (TCP 443).
- Environment of every Go run (`isolate.GoEnv`): one shared `GOPATH`/`GOMODCACHE`/`GOCACHE` under
  `~/.cache/vibechat-go` (bound read-write into the jail), `GOFLAGS=-mod=readonly`, `GOTOOLCHAIN=local`,
  `CGO_ENABLED=0`, `GOPROXY=https://proxy.golang.org`, `GOSUMDB=sum.golang.org`, `GOENV=off`, telemetry off.
- Runs: `go mod download` (install), `go build` (verify) and the dev loop run in the bubblewrap jail like Bun
  (`VIBECHAT_TOOLS_ISOLATION=require` on the server). One Go build at a time on the server: a `flock` in the Go
  cache (`server/internal/golock`), taken by the host's install and verify and by every dev loop; `go build -p
  min(2, CPUs)`; the container keeps the 0.2.6 limits (min(2, nproc) CPUs, 75% of memory up to 4 GiB).
- Preview: dev argv `{self} go-dev --port {port}` (`server/internal/godev`, in the jail, in the project folder):
  a self-reloading "Building…" page (503) at once, then `go generate ./...` when the project has `//go:generate`,
  `go tool templ generate` when a `.templ` is newer than its `_templ.go`, `go build -o .vibechat/bin/site`, and the
  site (`site dev --http 127.0.0.1:PORT [--preset from the Makefile]`, PocketBase data in `.vibechat/pb_data`);
  ready when `/healthz` answers 200; served through the shared preview proxy like Vite. Change detection polls
  sizes and mtimes (generated `_templ.go`/`_gen.go` and output ignored); a failed rebuild keeps the running site,
  a failed first build serves the log (500). No `air`.
- Doctor: a `go` line (installed, the pinned version).

### 3. Templates are layers over Empty

A template is Empty + layers. The templ template is the first layer template: `templates/templ/layer.json` in
buildytech/templates names fastygo/starter at the commit Chat's export pins (cf9d7db) and the SHA-256 of every
file; the server downloads that tree once from codeload.github.com, checks every file, and caches it
(`templates.EnsureLayer`). Go's `embed` cannot carry a tree with its own `go.mod`, which is one reason it is a
checked download and not a copy.

## Target (not implemented in 0.2.8)

- **Templates: core + data + design over Empty.** Each framework has ONE shared core (Chat's Svelte export and
  VibeChat's Svelte template are the same core; likewise react, templ = fastygo/starter, php); a starter differs
  by its data layer (page, fixtures, CSS, fonts) and design. The registry will declare templates as layers over
  Empty: `empty` → `core(framework)` → `data(+design)`.
- **Import = core(framework) + data.** Adapters already report each file's layer (`layers` in the import
  answer: core = the template's bytes at that path, data = the adapter's data paths, code = the page's own code).
  Next: verify the core against the template (or fill it from there) and accept only the data layer plus allowed
  page code.
- **Deploy.** On the same Incus host, one runtime container per framework (svelte, react, templ/go, php) holds the
  core; deploying a site ships only its data layer (page, fixtures, CSS, fonts) to that runtime.

## Consequences

- One adapter + one template + one runner per new framework; the shared checks stay in one file.
- A Go import can only use modules the starter was checked with; anything else needs a new template pin.
- Existing servers get Go from the package update only if their egress allows `dl.google.com`; re-running the
  setup wizard's egress and server steps adds the domains and installs Go from the host.
- Tailwind CSS is not rebuilt by the preview (the starter commits `app.css`); `make css` needs the Tailwind
  standalone binary, which the server does not install.
