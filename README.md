# BuildY VibeChat desktop

The [BuildY VibeCode](https://github.com/buildytech/vibecode) Studio as a desktop app on [Wails 3](https://wails.io)
(v3.1.0), for one user with their own Cursor API key.

This repository is **only the Go backend and the build**. The Studio, Agent Contract v1, the host seam and the
templates live in `buildytech/vibecode` and come in as the git submodule `third_party/vibecode`, pinned by commit.
Nothing in the submodule is edited here; a Studio change reaches the desktop app by moving the pin
([vibecode ADR-0027](https://github.com/buildytech/vibecode/blob/main/.project/adr/0027-desktop-host-seam.md)).

## How it works

- The window loads the built Studio (`apps/web` `build:desktop`), embedded in the binary (`frontend/dist`, made by
  `scripts/build-frontend.sh`).
- The Studio calls the host with `fetch("/runtime/v1/...")`. The Wails asset-server middleware answers those paths in
  Go, in process, on the app's own origin (`internal/host`). The route list is the pinned
  `packages/agent-contract/host-seam.json`; a route this build does not serve yet answers 501 with `{ error }`.
- The Cursor key is kept in the OS keychain (`github.com/zalando/go-keyring`). `CURSOR_API_KEY` in the environment is
  the development fallback. The key never reaches the webview.

## Build

Requirements: Go 1.26+, Bun 1.4 (the Studio build), and on Linux `pkg-config libgtk-4-dev libwebkitgtk-6.0-dev`.

```bash
git clone --recurse-submodules https://github.com/buildytech/vibechat-desktop
cd vibechat-desktop
bash scripts/build-frontend.sh               # bun install + build:desktop in third_party/vibecode → frontend/dist
go build -o bin/vibechat ./cmd/vibechat      # the desktop app
bin/vibechat --set-key                       # paste the Cursor key on stdin, kept in the OS keychain
bin/vibechat
```

Headless mode for tests and the exam (no window; loopback only, port `VIBECHAT_PORT`, default 18120):

```bash
go build -tags server -o bin/vibechat-server ./cmd/vibechat
bin/vibechat-server
curl -s http://127.0.0.1:18120/runtime/v1/host/capabilities
```

The server build has no auth: never expose it beyond loopback.

## Tests and the gate

`bash scripts/gate.sh` before every push to `main` (no `--force`, no `--no-verify`): gofmt, `go vet`, unit tests, the
host seam contract test against the pinned `host-seam.json`, and the headless build. CI (`.github/workflows/ci.yml`)
builds the Studio, runs the tests and builds the desktop app and unsigned installers on Linux, macOS and Windows.

## License

Apache-2.0, see `LICENSE`.
