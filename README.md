# BuildY VibeChat desktop

The [BuildY Studio](https://github.com/buildytech/studio) as a desktop app on [Wails 3](https://wails.io)
(v3.1.0), for one user with their own Cursor API key.

This repository is **only the Go backend and the build**. Two public git submodules, pinned by commit:

- `third_party/studio` = [buildytech/studio](https://github.com/buildytech/studio): the Studio, Agent Contract v1 and
  the host seam (`packages/agent-contract/host-seam.json`, [ADR 0002](https://github.com/buildytech/studio/blob/main/docs/adr/0002-host-seam-contract.md));
- `third_party/templates` = [buildytech/templates](https://github.com/buildytech/templates): `vite-svelte-bun` and
  `vite-react-bun`.

Nothing in a submodule is edited here; a Studio or template change reaches the desktop app by moving the pin.

## Скачать / Download

Portable zips of the latest release (unzip and run; `bridge/` — the agent runtime — must stay next to the executable):

- **Linux x64**: [vibechat-desktop-linux-x64.zip](https://github.com/buildytech/vibechat-desktop/releases/latest/download/vibechat-desktop-linux-x64.zip)
  — GTK 4 + WebKitGTK 6 (`sudo apt install libgtk-4-1 libwebkitgtk-6.0-4`; Ubuntu 24.04 / Debian 13 or newer).
  Run `./vibechat-desktop-linux-x64/vibechat`.
- **Windows x64**: [vibechat-desktop-windows-x64.zip](https://github.com/buildytech/vibechat-desktop/releases/latest/download/vibechat-desktop-windows-x64.zip)
  — Windows 10/11 64-bit with Microsoft Edge WebView2 (preinstalled on Windows 11). Run `vibechat.exe`; the
  unsigned build may trigger SmartScreen: «More info» → «Run anyway».
- **macOS Apple Silicon (M1+)**: [vibechat-desktop-macos-arm64.zip](https://github.com/buildytech/vibechat-desktop/releases/latest/download/vibechat-desktop-macos-arm64.zip)
  — macOS 12+. The app is not notarized: after unzipping run
  `xattr -dr com.apple.quarantine "BuildY VibeChat.app"` (or right-click → Open).

Installers (`.deb`, NSIS `.exe`, `.dmg`) are on the [releases page](https://github.com/buildytech/vibechat-desktop/releases/latest).
Projects need [Bun](https://bun.sh) 1.3.3 or newer on PATH (`bun upgrade`); an older Bun gets a clear «update Bun»
message. A Cursor API key is needed for the agent: Settings in the app, or `vibechat --set-key`. The app log (bridge
output included, key redacted) is `%APPDATA%\tw.buildy.vibechat\logs\vibechat.log` on Windows,
`~/Library/Application Support/tw.buildy.vibechat/logs/vibechat.log` on macOS, `~/.config/tw.buildy.vibechat/logs/vibechat.log`
on Linux; `vibechat --bridge-smoke` checks that the agent runtime starts. `vibechat --check-key` checks the stored key with Cursor (Me +
agent create) and prints a non-secret diagnostic (length, first/last 4 characters, whitespace, source); on Windows
run `vibechat.exe --check-key > check.txt`. The key is an API key of type **Cursor** (`grok_…`) from
https://grok.com/dashboard/api-keys; "Grok API" keys (`xai-…`) do not work. Cursor answers an unknown `grok_…` key
with `unauthenticated: Invalid Service Token`, an unknown key of another shape with `Invalid User API Key`. `vibechat --version` prints the
version. Releases are built by CI from a `v*` tag.

## Windows: no sandbox / Windows: без песочницы

**v0.1.5 freezes the local version.** On Windows the agent runs **without a sandbox**: cursor-sdk-bridge 1.2.0 (and
@cursor/sdk 1.5.0) has no filesystem sandbox on win32, its Windows helper is a network proxy only. The app shows a
warning (EN + RU) in every project. What that means on Windows:

- the agent runs arbitrary shell commands with your user's rights, **with network access** (the hook's blocklist of
  `curl`/`wget`/`ssh`/… is easy to get around, e.g. PowerShell `iwr`, `bun -e fetch`, `git clone`, `npm i`);
- shell commands can **write and read outside the project** (the hooks check the file tools and the shell's working
  directory, not what a command touches);
- there is **no approval** step: without the sandbox the SDK approves every action automatically;
- the Cursor key reaches the bridge through its environment and is **likely visible to the agent's shell**.

Open only trusted projects on Windows. On **macOS** (Seatbelt) and **Linux** (Landlock + bubblewrap, seccomp) the
agent runs inside the SDK sandbox: writes only in the project, no network, fail closed if the sandbox is missing.

**Decision: next versions move the agent to the user's server** — one Incus container per user (Debian 13), the
desktop app becomes a thin shell connected over built-in SSH (pinned host key, host token), and the Cursor key is
stored on the server, never on the PC.

Кратко по-русски: в Windows агент работает без песочницы (shell-команды с доступом вашего пользователя к файлам и
сети, без подтверждений, ключ, вероятно, виден shell). Открывайте только доверенные проекты. Следующие версии
переносят агента на ваш сервер: контейнер Incus на пользователя (Debian 13), встроенный SSH, ключ хранится на сервере.

## How it works

- The window loads the built Studio (`apps/studio` `build:desktop`), embedded in the binary (`frontend/dist`, made by
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
- **Editing keys belong to the page.** The Edit menu role is used only on macOS (a WKWebView needs it for Cmd+V). On
  Windows and Linux it bound Ctrl+V/C/X/Z/A and Backspace as menu accelerators, so on Windows a Ctrl+V pasted twice
  (the role's own paste on top of WebView2's). A message is stored and run once per `clientMsgId`; a repeat answers 409.
- **The SDK sandbox is mandatory.** At start the host runs the helper's preflight; no bridge, no helper or a failed
  preflight turns `agent_sandbox` off and every prompt answers 503. **Exception: Windows.** cursor-sdk-bridge 1.2.0
  refuses a sandboxed local agent on win32 ("Local SDK sandboxing was requested, but sandboxing is not supported in
  this environment": its Windows helper is proxy-only, no filesystem isolation), so on Windows agents are created
  with `sandboxOptions.enabled=false` (logged at start); elsewhere that refusal is retried once without the sandbox
  and logged. The permission hooks below and the workspace checks still apply; see «Windows: no sandbox» above
  for what that leaves open.
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
  templates submodule.
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
bash scripts/build-frontend.sh               # bun install + build:desktop in third_party/studio → frontend/dist
go build -o bin/vibechat ./cmd/vibechat      # the desktop app
bin/vibechat --set-key                       # paste the Cursor key on stdin, kept in the OS keychain
bin/vibechat
```

Headless Wails mode for tests and the exam (no window, no auth; loopback only, port `VIBECHAT_PORT`, default 18120):

```bash
go build -tags server -o bin/vibechat-headless ./cmd/vibechat
bin/vibechat-headless
curl -s http://127.0.0.1:18120/runtime/v1/host/capabilities
```

The headless build has no auth: never expose it beyond loopback. It stays until the tests and the exam move to
`vibechat-server`.

## Server (preview) / Сервер (предварительно)

`cmd/vibechat-server` is the same host on the user's own server: plain `net/http`, no Wails. The desktop app
connects to it over SSH built into the app (Settings → Server); a setup wizard comes next.

**Required server OS: Debian 13 "trixie", x86_64** (one Incus container per user, the container also Debian 13).
Ubuntu and other systems are not supported for now; `vibechat-server doctor` warns on them. CI installs the `.deb`
in a Debian 13 Incus VM and in an Incus container inside it (`scripts/ci-server-debian.sh`).
Требуется Debian 13 (trixie); Ubuntu и другие системы пока не поддерживаются.

- `127.0.0.1:18120`: the host seam `/runtime/v1/*` and the Studio; `127.0.0.1:18121`: the preview gateway,
  `http://<projectId>.preview.localhost:18121/` (one port for every preview; grants and cookies as in the app).
  Loopback only: the app reaches both through SSH, and any other `Host` header gets 421.
- Auth: `vibechat-server token new` prints a host token once (32 random bytes) and keeps only its sha256
  (`/var/lib/vibechat/auth/token.sha256`, 0600). The app's Go side sends it as `Authorization: Bearer`, buys a
  one-time ticket (`POST /runtime/v1/auth/ticket`, 60 s) and opens `/auth/session?ticket=…` in the window, which sets
  an HttpOnly SameSite=Strict cookie and moves on to `/` with a meta refresh (a 303 would keep the app's own origin as
  the navigation's initiator, and the Strict cookie would not be sent). `GET /runtime/v1/host/hello` (seam, server version, instance id, protocol,
  preview port, capabilities) is the only public route. The desktop app's own host has no auth and answers the ticket
  route with 404.
- The Cursor key: `/var/lib/vibechat/secrets/cursor-key` (0600; a looser file is refused), set in Settings or with
  `vibechat-server set-key` (stdin).
- `vibechat-server doctor`: system (Debian 13, else a warning), bridge + `cursorsandbox --preflight-only`, Landlock,
  user namespaces, token, key, bun, git.
- Package: `bash scripts/package-server.sh` → `dist/vibechat-server-linux-amd64.deb` (`/usr/lib/vibechat-server`,
  system user `vibechat`, `vibechat-server.service`, `vibechat-update.timer`). The timer runs
  `vibechat-server update` daily: the newest GitHub release, if newer, checked against its `SHA256SUMS`, then
  `dpkg -i` (integrity, not origin: release signing comes later).

```bash
sudo dpkg -i vibechat-server-linux-amd64.deb
sudo -u vibechat vibechat-server token new     # copy the token (shown once)
sudo -u vibechat vibechat-server set-key       # or later in Settings
sudo systemctl start vibechat-server
# then in the app: Settings → Server (the lines below), or by hand in a browser:
ssh -N -L 18120:127.0.0.1:18120 -L 18121:127.0.0.1:18121 you@server
```

### The app's connection / Подключение приложения

Settings → Server in the desktop app (`internal/remote`, the Studio's `GET/PUT/DELETE /runtime/v1/desktop/connection`,
answered by the app itself, never by a server):

- SSH is built in (`golang.org/x/crypto/ssh`, no `ssh` binary). The app makes an ed25519 key once and keeps the
  private key in the OS keychain (macOS Keychain, Windows Credential Manager, Linux Secret Service; item
  `vibechat-desktop` / `SSH_KEY`); Settings shows its public line and the root commands for the server: a user
  `vibechat-tunnel` with no login shell whose key may only forward the two ports
  (`restrict,port-forwarding,permitopen="127.0.0.1:18120",permitopen="127.0.0.1:18121"`).
- The host token (`vibechat-server token new`) goes into the keychain too (`SERVER_HOST_TOKEN`), never into a file,
  the log or the webview. `<data>/remote.json` (0600) has only the address, the user, the pinned host key and the
  local port.
- Host key: the first connect pins the key the server presents (trust on first use, ed25519 preferred); afterwards
  only that key is accepted, a different one is `host_key_changed` (Settings: if you reinstalled the server, Forget
  server and connect again). A reconnect after a network drop uses the same pin.
- Connect: SSH → `GET /host/hello` through the tunnel → protocol check (the app speaks the pinned seam's `protocol`
  and accepts servers down to its `minClientProtocol`; a mismatch shows the update screen) → `POST /auth/ticket` with
  the Bearer token from Go → a local reverse proxy on `http://127.0.0.1:<L>` (L kept across restarts, so the
  Studio's local storage survives) and the preview gateway's port forwarded as the same port on `127.0.0.1` and
  `[::1]` → the window opens `http://127.0.0.1:<L>/auth/session?ticket=…`. An active server is connected again at
  start; on failure the app stays on this computer and Settings says why.
- In remote mode the Studio, the projects, the agent and the Cursor key are the server's: a key entered in Settings
  is sent to the server (`secrets.File`, 0600 there), not kept on the PC. Settings shows the server version and a
  warning when it is older than the app (it updates itself daily; `sudo vibechat-server update` to update now).
  "Use this computer" goes back to the local host; "Forget server" also drops the pin and the token.
- `*.localhost`: previews are `http://<projectId>.preview.localhost:18121/`; `vibechat --probe-localhost` checks that
  the platform's webview resolves and frames such a name (CI on Linux/WebKitGTK, Windows/WebView2 and
  macOS/WKWebView, also from the `.app`). WebView2 and WebKitGTK: yes. macOS: no — its system resolver answers "no
  such host" for `*.localhost` (not App Transport Security: `localhost` and `127.0.0.1` load). So on macOS every
  preview gets its own `http://127.0.0.1:<P>` from the app (`internal/remote/previewports.go`): the window proxy
  rewrites the preview URLs in the seam's project/ensure/status/grant answers, the per-project proxy talks to the
  gateway as `<id>.preview.localhost:18121` (Origin / Referer mapped too), and the local port 18121 maps Vite's HMR
  socket (`ws://127.0.0.1:18121`, Origin `http://127.0.0.1:<P>`) to its project. `VIBECHAT_PREVIEW_PORTS=1|0`
  forces it on or off.
- Tests: `internal/remote` (an in-process SSH server with permitopen: transport, ticket flow, SSE, preview forward,
  reconnect, TOFU and strict pinning, keychain mock, problems) and the e2e `scripts/ci-remote-e2e.sh` (CI, job
  `server-debian`): a real sshd and the `.deb` in the Debian 13 VM, the tunnel user from the same lines Settings shows,
  `go test -tags e2e -run TestE2E ./internal/remote` from the runner.

The agent needs the runtime next to the binary: `go run ./cmd/fetch-bridge -out bin/bridge` downloads the
cursor-sdk-bridge build and the SDK sandbox helper pinned in `bridge.lock.json` and verifies their SHA-256 / sha512.
On x64 (Linux, Windows, Intel macOS) the upstream bridge needs AVX2 (Bun's default runtime: `0xC000001D` /
SIGILL on older CPUs), so fetch-bridge takes the JavaScript bundle out of the pinned binary and compiles it again on
the pinned Bun **baseline** runtime of the same version (`baselineRuntime` in the lock; SSE4.2, no AVX). `go run
./cmd/fetch-bridge -verify bin/bridge` checks a bridge folder (manifest `runtime: bun-*-baseline`, machine code equal
to the pinned baseline Bun); `scripts/bridge-qemu-smoke.sh <bridge dir>` runs it under QEMU `-cpu Nehalem-v2` until
ready (CI does both). An early bridge exit is logged with its decoded exit code (e.g. `0xC000001D
STATUS_ILLEGAL_INSTRUCTION`) and the last output lines.
Without it (or without a working sandbox) the app runs but the agent stays off.

## Installers

`bash scripts/package.sh` (after `scripts/build-frontend.sh`) builds the unsigned installer of the current OS into
`dist/`, with the agent runtime inside, and leaves the bare executable with `bin/bridge/` next to it:

| OS | Installer | Runtime location |
|---|---|---|
| Linux | `vibechat_<version>_amd64.deb` | `/usr/lib/vibechat/bridge` |
| Windows | `vibechat-amd64-installer.exe` (NSIS) | `<install dir>\bridge` |
| macOS | `BuildY VibeChat.dmg` (ad-hoc signed `.app`) | `Contents/Resources/bridge` |

It also writes the portable zip of that OS: `vibechat-desktop-linux-x64.zip`, `vibechat-desktop-windows-x64.zip`,
`vibechat-desktop-macos-arm64.zip` (Apple Silicon only). CI uploads `vibechat-installer-<os>` (installer + zip) and
`vibechat-<os>` (the bare executable + `bridge/`); a pushed `v*` tag also publishes a GitHub Release with the three
zips and the installers under these stable names.

## Tests and the gate

`bash scripts/gate.sh` before every push to `main` (no `--force`, no `--no-verify`): gofmt, `go vet`, unit tests, the
host seam contract test against the pinned `host-seam.json`, and the headless build. CI (`.github/workflows/ci.yml`)
builds the Studio, runs the tests and builds the desktop app and unsigned installers on Linux, macOS and Windows.

## License

Apache-2.0, see `LICENSE`.
