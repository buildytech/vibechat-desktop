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

### The agent: cursor-sdk-bridge in the SDK sandbox (fail closed)

- One [cursor-sdk-bridge](https://github.com/cursor/cursor-sdk-bridge) process per app (`internal/bridge`), found in
  `VIBECHAT_BRIDGE_DIR`, `<exe dir>/bridge`, `<exe dir>/../lib/vibechat/bridge` or `<app>/Contents/Resources/bridge`,
  with the platform helper `@cursor/sdk-<os>-<arch>` (`cursorsandbox`, `rg`) in `bin/node_modules/`. It listens on
  127.0.0.1 with a bearer token; the key reaches it only as `CURSOR_API_KEY` in its environment (never argv, a URL or a
  file). The catalog calls (`Me`, `ListModels`) need the key in their loopback request body; agent runs use the env.
- **The SDK sandbox is mandatory.** At start the host runs the helper's preflight; no bridge, no helper or a failed
  preflight turns `agent_sandbox` off and every prompt answers 503. Nothing runs unsandboxed.
- The bridge runs with a private `HOME` (`<data>/agent-home`) that holds the user-level `.cursor/sandbox.json`
  (`readBoundary: workspace`, `networkAccess: false`, plus the tool runtimes and `/proc` Bun needs) and
  `.cursor/hooks.json`: fail-closed `preToolUse` / `beforeReadFile` hooks that run this binary (`vibechat --hook`) and
  deny file tools outside the workspace, MCP, network commands, and writes to agent config. A project that carries
  its own `.cursor/{hooks,sandbox,cli,permissions,mcp}.json` or `.claude/settings*.json` is refused before a run.
- The agent's network is fully closed. Dependencies are installed by the host (`bun install`) outside the sandbox:
  when a project is created, when a chat's worktree is made, and after a run that changed `package.json`/`bun.lock`.
- Models: `auto`, Composer 2.5 (and fast), `grok-4.7` (context 256k/500k, effort, fast), Claude Opus 5. Runs stream
  Agent Contract v1 events over SSE (`/agent/sessions/:sid/events`); the transcript is `<data>/state/transcripts`.

### Projects, chats and worktrees

- Projects live in `~/VibeChat/<name>` (`VIBECHAT_PROJECTS_DIR`), state in `<UserConfigDir>/tw.buildy.vibechat`
  (`VIBECHAT_DATA_DIR`). Each project is a git repository (`main`) scaffolded from a template embedded from the
  vibecode submodule.
- Every chat works in its own git worktree, `<data>/worktrees/<project>/<chat>` on branch `vibe/<chat>`, outside the
  project folder. Before a run the worktree takes in `main`; after it the host commits the run and merges it into
  `main` (the project folder, where the preview runs). A conflict leaves the work on the branch and says so. Deleting
  a chat removes its worktree and branch.

### Live preview

- Play starts the project's dev server (`bun x vite --host 127.0.0.1 --port <port> --strictPort`) in its own process
  group (on Windows also a Job Object with kill-on-close, so the tree dies with the app), after the host-side
  `bun install`. Stop, deleting the project and quitting the app stop the whole group.
- The Studio iframe opens a separate loopback origin, `http://127.0.0.1:<proxy port>` (`internal/preview`), never
  the dev server directly. The host mints a one-time grant (`?__buildy_grant=…`, 60 s); the proxy also lets through
  requests with its session cookie or that the browser marks as coming from the preview itself (Sec-Fetch-Site
  same-origin / Origin / Referer), checks the Host header, and answers everything else 403.
- Vite HMR runs over the proxy port (`HMR_CLIENT_PORT`), WebSocket included.

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

The agent needs the runtime next to the binary: `go run ./cmd/fetch-bridge -out bin/bridge` downloads the
cursor-sdk-bridge build and the SDK sandbox helper pinned in `bridge.lock.json` and verifies their SHA-256 / sha512.
Without it (or without a working sandbox) the app runs but the agent stays off.

## Installers

`bash scripts/package.sh` (after `scripts/build-frontend.sh`) builds the unsigned installer of the current OS into
`dist/`, with the agent runtime inside, and leaves the bare executable with `bin/bridge/` next to it:

| OS | Installer | Runtime location |
|---|---|---|
| Linux | `vibechat_<version>_amd64.deb` | `/usr/lib/vibechat/bridge` |
| Windows | `vibechat-amd64-installer.exe` (NSIS) | `<install dir>\bridge` |
| macOS | `BuildY VibeChat.dmg` (ad-hoc signed `.app`) | `Contents/Resources/bridge` |

CI uploads `vibechat-installer-<os>` (the installer) and `vibechat-<os>` (the bare executable + `bridge/`).

## Tests and the gate

`bash scripts/gate.sh` before every push to `main` (no `--force`, no `--no-verify`): gofmt, `go vet`, unit tests, the
host seam contract test against the pinned `host-seam.json`, and the headless build. CI (`.github/workflows/ci.yml`)
builds the Studio, runs the tests and builds the desktop app and unsigned installers on Linux, macOS and Windows.

## License

Apache-2.0, see `LICENSE`.
