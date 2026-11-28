# BuildY VibeChat

The desktop app is **a window to your own server**: the [BuildY Studio](https://github.com/buildytech/studio), the
Cursor agent, projects and previews run in `vibechat-server` on a **Debian 13 "trixie" x86_64** server (one Incus
container per user); the app ([Wails 3](https://wails.io), v3.1.0) connects to it over SSH built into the app.
Nothing runs on the PC: no local agent, no local projects, no templates (since 0.2.0).

**Требуется сервер на Debian 13 (trixie), x86_64.** Приложение — только окно к серверу: агент, проекты и превью
работают на сервере, приложение подключается к нему по встроенному SSH. Ubuntu и другие системы пока не поддерживаются.

One repository, one Go module, three parts:

- `protocol/` — what both sides share: the host seam contract of the pinned studio commit (`protocol.Pinned`, from
  `third_party/studio/packages/agent-contract/host-seam.json`, embedded by the root package) and the token / ticket /
  session auth (`protocol/auth`).
- `server/` — `vibechat-server` (`server/cmd/vibechat-server`) and everything that runs the agent: `server/internal/`
  host (the seam), agent, bridge, sandbox, preview, store, gitx, templates, secrets, hostwire, procgroup;
  `server/cmd/fetch-bridge` and `server/bridge.lock.json` (the agent runtime); the built Studio
  (`server/frontend/dist`) and the templates submodule (`server/third_party/templates`).
- `desktop/` — the app (`desktop/cmd/vibechat`): the start screen (`desktop/shell`, built from the studio submodule),
  the SSH connection (`desktop/internal/remote`) and the setup wizard (`desktop/internal/provision`). Go's internal
  rule keeps `server/internal` out of reach of `desktop/`, and `scripts/check-desktop.sh` checks the packages the app
  links and the built binary (CI on all three systems).
- `internal/` — four small shared packages: `appid` (identity), `applog` (the log file), `privfile` (0600 files),
  `webapp` (serving a built site).

Two public git submodules, pinned by commit: `third_party/studio` =
[buildytech/studio](https://github.com/buildytech/studio) (the Studio, the start screen, Agent Contract v1 and the
host seam, [ADR 0002](https://github.com/buildytech/studio/blob/main/docs/adr/0002-host-seam-contract.md)) and
`server/third_party/templates` = [buildytech/templates](https://github.com/buildytech/templates) (`templates.json`:
`svelte`, `react`, `templ`, `empty`; the server builds its template list from the manifest, so a template with a known runner
needs only a pin bump; `templ` is a layer over Empty: fastygo/starter at a pinned commit, every file checked by SHA-256). Nothing in a submodule is edited here; a change reaches this repository by moving the pin.

## Скачать / Download

The app (portable zips of the latest release; installers `.deb`, NSIS `.exe`, `.dmg` are on the
[releases page](https://github.com/buildytech/vibechat-desktop/releases/latest)):

- **Linux x64**: [vibechat-desktop-linux-x64.zip](https://github.com/buildytech/vibechat-desktop/releases/latest/download/vibechat-desktop-linux-x64.zip)
  — GTK 4 + WebKitGTK 6 (`sudo apt install libgtk-4-1 libwebkitgtk-6.0-4`; Ubuntu 24.04 / Debian 13 or newer).
  Run `./vibechat-desktop-linux-x64/vibechat`.
- **Windows x64**: [vibechat-desktop-windows-x64.zip](https://github.com/buildytech/vibechat-desktop/releases/latest/download/vibechat-desktop-windows-x64.zip)
  — Windows 10/11 64-bit with Microsoft Edge WebView2 (preinstalled on Windows 11). Run `vibechat.exe`; the
  unsigned build may trigger SmartScreen: «More info» → «Run anyway».
- **macOS Apple Silicon (M1+)**: [vibechat-desktop-macos-arm64.zip](https://github.com/buildytech/vibechat-desktop/releases/latest/download/vibechat-desktop-macos-arm64.zip)
  — macOS 12+. The app is not notarized: after unzipping run
  `xattr -dr com.apple.quarantine "BuildY VibeChat.app"` (or right-click → Open).

And a server: a Debian 13 x86_64 VPS (≥ 2 GiB RAM, ≥ 18 GiB free disk for the wizard), see below.

### First start / Первый запуск

Without a saved server the window shows the start screen with two choices:

- **Connect a server / Подключить сервер** — a server with `vibechat-server` already installed: address, SSH user
  (`vibechat-tunnel`), the host token, and the lines for the server that add the app's SSH key (port forwarding
  only). The first connect pins the server's host key.
- **Set up a new server / Настроить новый сервер** — the wizard (below): a fresh VPS's address and the root password
  once; the app installs everything and connects.

With a saved server the app reconnects at start ("Connecting…") and opens the server's Studio directly; when that
fails, the start screen shows why. Disconnect (Settings → Server in the server's Studio) goes back to the start
screen. The Cursor key is entered in the server's Studio Settings and stays on the server. Projects made on the PC
by versions before 0.2.0 stay in `~/VibeChat`; the app no longer opens them.

The app log is `%APPDATA%\tw.buildy.vibechat\logs\vibechat.log` on Windows,
`~/Library/Application Support/tw.buildy.vibechat/logs/vibechat.log` on macOS, `~/.config/tw.buildy.vibechat/logs/vibechat.log`
on Linux. `vibechat --version` prints the version. Releases are built by CI from a `v*` tag.

## How it works

- The desktop window loads the start screen (`desktop/shell`, studio `build:shell`), embedded in the binary. The app
  answers only its own routes, `/runtime/v1/desktop/connection` and `/runtime/v1/desktop/provision`; every other seam
  path is 404. Connected, the window moves to `http://127.0.0.1:<L>`, a local reverse proxy through the SSH tunnel to
  the server's Studio.
- On the server, the Studio calls the host with `fetch("/runtime/v1/...")` on the server's origin
  (`server/internal/host`). The route list is the pinned `packages/agent-contract/host-seam.json`; a route the build
  does not serve answers 501 with `{ error }`.
- The Cursor key is a 0600 file on the server (`secrets.File`), never on the PC and never in the webview.

### The agent on the server: cursor-sdk-bridge in the SDK sandbox (fail closed)

- One [cursor-sdk-bridge](https://github.com/cursor/cursor-sdk-bridge) process per server (`server/internal/bridge`), found in
  `VIBECHAT_BRIDGE_DIR` or `<exe dir>/bridge` (`/usr/lib/vibechat-server/bridge` from the `.deb`),
  with the platform helper `@cursor/sdk-<os>-<arch>` (`cursorsandbox`, `rg`) in `bin/node_modules/`. It listens on
  127.0.0.1 with a bearer token; the key reaches it only as `CURSOR_API_KEY` in its environment (never argv, a URL or a
  file). The catalog calls (`Me`, `ListModels`) need the key in their loopback request body; agent runs use the env.
- A message is stored and run once per `clientMsgId`; a repeat answers 409.
- **The SDK sandbox is mandatory.** At start the host runs the helper's preflight; no bridge, no helper or a failed
  preflight turns `agent_sandbox` off and every prompt answers 503 (Landlock + bubblewrap on the Debian 13 server).
  The agent never runs on Windows or macOS any more: the old Windows-without-sandbox mode went away with the local
  agent in 0.2.0.
- The bridge runs with a private `HOME` (`<data>/agent-home`) that holds the user-level `.cursor/sandbox.json`
  (`readBoundary: workspace`, `networkAccess: false`, plus the tool runtimes and `/proc` Bun needs) and
  `.cursor/hooks.json`: fail-closed `preToolUse` / `beforeReadFile` hooks that run the server binary (`vibechat-server --hook`) and
  deny file tools outside the workspace, MCP, network commands, and writes to agent config. A project that carries
  its own `.cursor/{hooks,sandbox,cli,permissions,mcp}.json` or `.claude/settings*.json` is refused before a run.
- The agent's network is fully closed. Dependencies are installed by the host (`bun install`) outside the sandbox:
  when a project is created, when a chat's worktree is made, and after a run that changed `package.json`/`bun.lock`.
- Models: `auto`, Composer 2.5 (and fast), `grok-4.7` (context 256k/500k, effort, fast), Claude Opus 5. Runs stream
  Agent Contract v1 events over SSE (`/agent/sessions/:sid/events`); the transcript is `<data>/state/transcripts`.

### Projects, chats and worktrees

- On the server, projects and state live under `/var/lib/vibechat` (the `vibechat` user). Each project is a git repository (`main`) scaffolded from a template embedded from the
  templates submodule.
- Every chat works in its own git worktree, `<data>/worktrees/<project>/<chat>` on branch `vibe/<chat>`, outside the
  project folder. Before a run the worktree takes in `main`; after it the host commits the run and merges it into
  `main` (the project folder, where the preview runs). A conflict leaves the work on the branch and says so. Deleting
  a chat removes its worktree and branch.

### Live preview

- Play starts the project's dev server (`bun x vite --host 127.0.0.1 --port <port> --strictPort`; a Go templ project:
  `vibechat-server go-dev --port <port>`, templ generate + go build, rebuild on change, ready on `/healthz`) in its own process
  group, after the host-side `bun install` (`go mod download`). Stop, deleting the project and stopping the server stop the whole group.
- The Studio iframe opens the preview gateway, `http://<projectId>.preview.localhost:18121/` (`server/internal/preview`,
  forwarded by the app through the tunnel; on macOS a local port per project), never the dev server directly. The host mints a one-time grant (`?__buildy_grant=…`, 60 s); the proxy also lets through
  requests with its session cookie or that the browser marks as coming from the preview itself (Sec-Fetch-Site
  same-origin / Origin / Referer), checks the Host header, and answers everything else 403.
- Vite HMR runs over the gateway port (`HMR_CLIENT_PORT`), WebSocket included.

## Build

Requirements: Go 1.26+, Bun 1.4 (the Studio and start screen builds), and for the desktop app on Linux
`pkg-config libgtk-4-dev libwebkitgtk-6.0-dev`.

```bash
git clone --recurse-submodules https://github.com/buildytech/vibechat-desktop
cd vibechat-desktop
bash scripts/build-shell.sh                          # studio build:shell → desktop/shell/dist (the start screen)
go build -o bin/vibechat ./desktop/cmd/vibechat      # the desktop app
bash scripts/check-desktop.sh bin/vibechat           # it links no server or local-agent code
bash scripts/build-frontend.sh                       # studio build:desktop → server/frontend/dist (the Studio)
go build -o bin/vibechat-server ./server/cmd/vibechat-server
bash scripts/package-server.sh                       # the server .deb (Linux; fetches the agent runtime)
```

The old headless Wails mode (`-tags server`, no auth) is gone: tests and checks run against `vibechat-server`
(`server/cmd/vibechat-server` tests, `scripts/server-smoke.sh`, the Debian 13 jobs in CI).

## Server / Сервер

`server/cmd/vibechat-server` is the host on the user's own server: plain `net/http`, no Wails. The desktop app
connects to it over SSH built into the app (the start screen's Connect a server); a fresh Debian 13 VPS is set up by
the app's wizard (Set up a new server, below).

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
  preview port, capabilities) is the only public route. The desktop app answers its own routes only; the ticket
  route is the server's.
- The Cursor key: `/var/lib/vibechat/secrets/cursor-key` (0600; a looser file is refused), set in Settings or with
  `vibechat-server set-key` (stdin).
- `vibechat-server doctor`: system (Debian 13, else a warning), bridge + `cursorsandbox --preflight-only`, Landlock,
  user namespaces, token, key, bun, git, go (the pinned toolchain in `/opt/vibechat/go`).
- Import ZIP (`POST /projects/:id/import.zip`, an empty project): shared checks plus one adapter per framework
  (svelte, react, templ; `manifest.json` `buildy.pack.v1` names it, else `ui8kit.config.json` / `go.mod`):
  [ADR 0001](docs/adr/0001-import-adapters-and-go-runner.md).
- Package: `bash scripts/package-server.sh` → `dist/vibechat-server-linux-amd64.deb` (`/usr/lib/vibechat-server`,
  system user `vibechat`, `vibechat-server.service`, `vibechat-update.timer`). The timer runs
  `vibechat-server update` daily: the newest GitHub release, if newer, checked against its `SHA256SUMS`, then
  `dpkg -i` (integrity, not origin: release signing comes later).

```bash
sudo dpkg -i vibechat-server-linux-amd64.deb
sudo -u vibechat vibechat-server token new     # copy the token (shown once)
sudo -u vibechat vibechat-server set-key       # or later in Settings
sudo systemctl start vibechat-server
# then in the app: Connect a server (the lines below), or by hand in a browser:
ssh -N -L 18120:127.0.0.1:18120 -L 18121:127.0.0.1:18121 you@server
```

### The app's connection / Подключение приложения

The start screen's Connect a server, and Settings → Server in the server's Studio (`desktop/internal/remote`, the Studio's `GET/PUT/DELETE /runtime/v1/desktop/connection`,
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
  start (the start screen shows "Connecting…", `connecting` in the answer); on failure the start screen says why.
- The Studio, the projects, the agent and the Cursor key are the server's: a key entered in Settings is kept on the
  server (`secrets.File`, 0600 there), never on the PC. Settings shows the server version and a
  warning when it is older than the app (it updates itself daily; `sudo vibechat-server update` to update now).
  "Disconnect" goes back to the start screen; "Forget server" also drops the pin and the token.
- `*.localhost`: previews are `http://<projectId>.preview.localhost:18121/`; `vibechat --probe-localhost` checks that
  the platform's webview resolves and frames such a name (CI on Linux/WebKitGTK, Windows/WebView2 and
  macOS/WKWebView, also from the `.app`). WebView2 and WebKitGTK: yes. macOS: no — its system resolver answers "no
  such host" for `*.localhost` (not App Transport Security: `localhost` and `127.0.0.1` load). So on macOS every
  preview gets its own `http://127.0.0.1:<P>` from the app (`desktop/internal/remote/previewports.go`): the window proxy
  rewrites the preview URLs in the seam's project/ensure/status/grant answers, the per-project proxy talks to the
  gateway as `<id>.preview.localhost:18121` (Origin / Referer mapped too), and the local port 18121 maps Vite's HMR
  socket (`ws://127.0.0.1:18121`, Origin `http://127.0.0.1:<P>`) to its project. `VIBECHAT_PREVIEW_PORTS=1|0`
  forces it on or off.
- Server help (on the start screen and in Settings → Server, a lazy card, EN — RU): signing in as root with your own SSH key once root password
  login is off (key paths on Windows / macOS / Linux, `ssh-copy-id` or the PowerShell line, the provider's web console
  as the way back; the app's tunnel key cannot sign in) and backups of `/var/lib/vibechat` to an S3-compatible bucket
  with restic (a 0600 env file, `restic init`, a daily systemd service + timer with `forget --prune`, `restic
  snapshots`, restore with the server stopped). Commands: `third_party/studio/apps/studio/src/server-help.ts`.
- Tests: `desktop/internal/remote` (an in-process SSH server with permitopen: transport, ticket flow, SSE, preview forward,
  reconnect, TOFU and strict pinning, keychain mock, problems) and the e2e `scripts/ci-remote-e2e.sh` (CI, job
  `server-debian`): a real sshd and the `.deb` in the Debian 13 VM, the tunnel user from the same lines Settings shows,
  `go test -tags e2e -run TestE2E ./desktop/internal/remote` from the runner.

The server's agent needs the runtime next to the binary: `go run ./server/cmd/fetch-bridge -out bin/bridge` downloads
the cursor-sdk-bridge build and the SDK sandbox helper pinned in `server/bridge.lock.json` and verifies their SHA-256 / sha512.
On x64 the upstream bridge needs AVX2 (Bun's default runtime: `0xC000001D` /
SIGILL on older CPUs), so fetch-bridge takes the JavaScript bundle out of the pinned binary and compiles it again on
the pinned Bun **baseline** runtime of the same version (`baselineRuntime` in the lock; SSE4.2, no AVX). `go run
./server/cmd/fetch-bridge -verify bin/bridge` checks a bridge folder (manifest `runtime: bun-*-baseline`, machine code equal
to the pinned baseline Bun); `scripts/bridge-qemu-smoke.sh <bridge dir>` runs it under QEMU `-cpu Nehalem-v2` until
ready (CI does both). An early bridge exit is logged with its decoded exit code (e.g. `0xC000001D
STATUS_ILLEGAL_INSTRUCTION`) and the last output lines.
Without it (or without a working sandbox) the server runs but the agent stays off.

### Setup wizard / Мастер настройки

The start screen's Set up a new server (`desktop/internal/provision`, `desktop/internal/remote/provision.go`, the Studio's
`GET/POST /runtime/v1/desktop/provision`, answered by the app itself). You type the VPS address and the root password
once; the password lives only in memory for this run (zeroed afterwards), never in a file, the log or the webview.
Over SSH (host key pinned on first use, strictly afterwards) the app runs ten steps; each one is check → apply →
check, so a second run skips what is done (`/var/lib/vibechat-provision/state.json`):

1. connect — root over SSH (password), pin the host key;
2. preflight — Debian 13 x86_64, root, systemd, ≥ 2 GiB RAM, ≥ 18 GiB free on `/var/lib` (the container disk lives in an 18 GiB loop-file btrfs pool `vibechat`, where the 15 GiB root quota is enforced; the dir pool of `incus admin init --minimal` could not enforce it); anything else stops with "not supported";
3. incus — `apt-get install incus`, `incus admin init --minimal` (no network listener: the app never gets the Incus API);
4. container — bridge `vibechatbr0` (NAT, IPv4), profile `vibechat` (`limits.cpu=2`, `limits.memory=4GiB`, root disk
   15GiB on the btrfs pool `vibechat`, `security.nesting=true`, proxy devices `127.0.0.1:18120/18121` on the host → the container),
   unprivileged `images:debian/13` container `vibechat-owner`;
5. server — the release `.deb` and `SHA256SUMS` downloaded on the host, `sha256sum -c --strict`, pushed and installed in
   the container; Bun (pinned baseline build, sha256 from `server/bridge.lock.json`) for the user `vibechat`; Go
   (`internal/gopin`, sha256-pinned) into `/opt/vibechat/go` before the package (whose postinstall runs `vibechat-server ensure-go`);
6. egress — the allowlist (below);
7. token — `vibechat-server token new` in the container; the token goes from stdout straight into the app's keychain
   (`SERVER_HOST_TOKEN`), never printed or logged; a rerun checks the stored token with `Authorization: Bearer` sent on
   stdin and makes a new one only if it no longer works;
8. tunnel — host user `vibechat-tunnel` (no login shell) with only the app key:
   `restrict,port-forwarding,permitopen="127.0.0.1:18120",permitopen="127.0.0.1:18121"`;
9. verify — `vibechat-server doctor`, a new SSH connection as `vibechat-tunnel` with the app key and the pin, hello +
   ticket; snapshot `provisioned`;
10. lockdown — first `systemd-run --on-active=5min` arms a rollback (it removes the drop-in and reloads ssh), then
    `/etc/ssh/sshd_config.d/00-vibechat.conf` (`PasswordAuthentication no`, `KbdInteractiveAuthentication no`,
    `PermitRootLogin prohibit-password`), `sshd -t`, `sshd -T` check, reload. The app then signs in with its key and
    checks that the root password is refused; only then is the rollback timer stopped. Without that confirmation the
    password login comes back by itself after 5 minutes. An optional root public key from the form goes into
    `/root/.ssh/authorized_keys` before the lockdown (otherwise: the provider's web console).

After step 10 the app connects to the server (remote mode). A run after the lockdown can no longer sign in with the
password (`auth`): by design.

**Egress allowlist.** An Incus network ACL `vibechat-egress` on `vibechatbr0`: egress default reject, only TCP 443 to
the IPv4 addresses (/32) of these names, plus DNS to the bridge's dnsmasq:
`api.cursor.com` (the Cursor SDK's API: account, model list, agent create; not on Cursor's network page), `api2.cursor.sh`, `api3.cursor.sh`, `api4.cursor.sh`, `api5.cursor.sh`, `repo42.cursor.sh`, `agent.api5.cursor.sh`,
`agentn.api5.cursor.sh`, `agentn.us.api5.cursor.sh`, `agentn.global.api5.cursor.sh`, `authenticate.cursor.sh`,
`authentication.cursor.sh`, `prod.authentication.cursor.sh` (Cursor's list, cursor.com/docs/enterprise/network-configuration),
`registry.npmjs.org`, `bun.sh`, and GitHub for Bun / server updates (`github.com`, `api.github.com`,
`codeload.github.com`, `objects.githubusercontent.com`, `release-assets.githubusercontent.com`), and Go
(`proxy.golang.org`, `storage.googleapis.com` where the proxy redirects module ZIPs, `sum.golang.org`, `dl.google.com`). The host resolves the
names (`/usr/local/sbin/vibechat-egress`, `vibechat-egress.timer` every 10 min), keeps the union with the previous run
(CDN rotation; an empty resolution keeps the old ACL) and pins the answers in the container's `/etc/hosts`.
DNS caveats: the rule is by IP, so another site on the same CDN address is reachable too; a name whose addresses change
between refreshes fails until the next one; wildcard subdomains are not covered; DNS queries themselves go out through
the host's dnsmasq (a possible channel for small leaks); IPv6 is off on the bridge. The ACL is applied after the
server step: `apt` in the container needs `deb.debian.org`, which is blocked afterwards (server updates come from
GitHub releases).

Tests: `desktop/internal/provision` (every script through `bash -n`, a fake host: each step, idempotency, continuing after a
failure, refusal of other systems, the token only to the keychain, lockdown waits for the confirmation, the egress
script with stub `incus`/`getent`) and the e2e `scripts/ci-provision-e2e.sh` (CI job `provision-debian`): a Debian 13
Incus VM with sshd and a random root password, the wizard via `go test -tags e2e -run TestProvisionE2E
./desktop/internal/remote`: run 1 without the confirmation → `not_confirmed`, password refused, back after the rollback; run 2
skips done steps, locks down, connects and signs in; run 3 → `auth`; then limits, unprivileged id map, ACL (allowed:
npm, Cursor, Bun, GitHub; blocked: example.com, 1.1.1.1, deb.debian.org, plain http), `sshd -T`, the tunnel user.

## Installers

`bash scripts/package.sh` (after `scripts/build-shell.sh`) builds the unsigned installer of the current OS into
`dist/` and leaves the bare executable in `bin/`; the app carries no agent runtime:

| OS | Installer |
|---|---|
| Linux | `vibechat_<version>_amd64.deb` (`/usr/bin/vibechat`) |
| Windows | `vibechat-amd64-installer.exe` (NSIS; removes the `bridge\` folder of an older install) |
| macOS | `BuildY-VibeChat.dmg` (ad-hoc signed `.app`; volume «BuildY VibeChat») |

It also writes the portable zip of that OS: `vibechat-desktop-linux-x64.zip`, `vibechat-desktop-windows-x64.zip`,
`vibechat-desktop-macos-arm64.zip` (Apple Silicon only). `bash scripts/package-server.sh` (Linux, after
`scripts/build-frontend.sh`) writes `vibechat-server-linux-amd64.deb` with the agent runtime. CI uploads
`vibechat-installer-<os>`, `vibechat-<os>` (the bare executable) and `vibechat-server`; a pushed `v*` tag also
publishes a GitHub Release with the zips, the installers, the server `.deb` and `SHA256SUMS` under these stable
names. The release body is `docs/releases/<tag>.md`; the release job fails if that file is missing.

## Tests and the gate

`bash scripts/gate.sh` before every push to `main` (no `--force`, no `--no-verify`): gofmt, `go vet`, all unit tests,
the host seam contract tests (server and desktop sides) against the pinned `host-seam.json`, the server build, the
desktop build when the GUI libraries are there, and `scripts/check-desktop.sh`. CI (`.github/workflows/ci.yml`): job
`build` (the desktop app on Linux, macOS and Windows: start screen, tests, build, no-local-agent check on the binary
and the shipped zip, installers), job `server` (Studio, server tests, templates install e2e with Bun 1.3.3 and the
current one, the Go templ e2e with the pinned Go, the agent runtime on the Bun baseline build and under QEMU without AVX2, the server `.deb`), jobs
`server-debian` and `provision-debian` (Debian 13 Incus VMs: install, connection e2e, the setup wizard) and
`release` (tags only).

## License

Apache-2.0, see `LICENSE`.
