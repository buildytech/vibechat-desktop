# ADR 0002: Coding-agent adapters, contour-based runtime (EU/US = Cursor SDK, RU = Pi)

Status: accepted. Step 1 (the adapter seam, no behavior change) and step 2 (the Pi adapter, provider gateway, contour config; tested without provider keys) are implemented; step 3 and the live run are open (see Plan and "Step 2: what is verified").

Scope: VibeChat only (this repo's server and the Studio it serves). BuildY chat is a separate system of model providers (xAI-compatible: Grok API, OpenRouter) and is not touched here.

## Context

The agent that writes code for a chat was the Cursor SDK, reached through the standalone `cursor-sdk-bridge` process. The Cursor SDK serves Cursor-hosted models only. In Russia that is not an option, and a server must be able to pick the coding runtime by jurisdiction (contour), not by a build.

The seam to the Studio (`/runtime/v1`, Agent Contract v1) was already vendor-neutral: `message.delta`, `thinking.*`, `tool.*`, `files.changed`, `session.status`, `usage`. The leaks were in the Go server: `internal/agent` types (`Account`, `Selection`, `Translator`) in `host.Agent`, the `cursor/` model-id prefix check, `runtime: "cursor-sdk"` literals, the `cursor-sdk/cursor` credential slot and its Cursor-specific messages, `.cursor/hooks.json` as a workspace check, and the bridge cancel flow.

## Decision

**Contour-based runtime: EU/US = Cursor SDK, RU = Pi (GigaChat / Yandex AI Studio).**

1. Cursor SDK **stays**. Pi runs **alongside** it, not instead. The Cursor bridge is not planned for removal.
2. The runtime (`cursor-sdk` | `pi`) is selected by the **server's contour config** (`hostwire.Config.Runtime`; "" = `cursor-sdk`). New chats get that runtime and remember it (`store.Session.Runtime`; empty = `cursor-sdk`, so chats made before this ADR are Cursor chats).
3. The host talks to a neutral `internal/codingagent` seam. Both runtimes are first class: they implement the same `Adapter`, emit the same closed event set, and report `Capabilities`.
4. **Fail closed, never a silent fallback.** A configured runtime that is not registered is an error at start. A chat whose runtime has no adapter on this server stops with "The agent runtime is not part of this build". A RU server never reaches a Cursor host, and an EU/US server never sends RU data to GigaChat or Yandex.
5. First contour is **RU with Pi**. EU/US keep Cursor unchanged. A non-Cursor EU provider (Mistral/OpenRouter-in-region via Pi) and China are postponed.
6. OpenRouter is only a model provider that Pi may use per contour. The OpenRouter Agent SDK is not adopted (it is a model-tool loop without file/shell tools or a sandbox).

## Design (step 1, implemented)

```
server/internal/codingagent/            neutral seam
  codingagent.go   Adapter, Session, Run, Capabilities (modelSource: runtime | own-keys), CredentialSpec,
                   OpenRequest, Result, Registry, RuntimeOf, ErrNoCredential, ErrUnknownModel, OpenError
  events.go        closed event set; each event has Wire() = the Agent Contract v1 JSON, unchanged
server/internal/codingagent/cursor/     the Cursor SDK runtime (moved from internal/agent, same code)
  adapter.go       cursor.Adapter over Runner; Cursor's messages, key checks and model rules live here
```

- `Adapter`: `Runtime`, `Capabilities`, `Ready` (fail-closed reason), `Unsandboxed`, `Credential` (slot + messages), `HasCredential`, `Account`, `CheckCredential`, `Unauthenticated`, `Models`, `DefaultModel`, `CheckModel`, `DropModel`, `CheckWorkspace`, `Open`, `Close`.
- `Session.Start(...)` returns a `Run` (`Cancel`, `Wait`); events are a sealed interface (`MessageDelta`, `ThinkingDelta`, `ThinkingCompleted`, `ToolStarted`, `ToolDelta`, `ToolFinished`, `FilesChanged`, `SessionStatus`, `UsageEvent`). A runtime cannot invent a wire event; additive changes to Agent Contract v1 stay the only way to grow it.
- `Capabilities.ModelSource` is `runtime` (the runtime's account lists and bills the models: Cursor) or `own-keys` (the owner's provider keys through a models config: Pi). It is for Settings and egress rules and is **not on the wire** in this version.
- Host: `Options.Agents` (a `Registry`) replaces `Options.Agent` and `Options.Keys`. Routes resolve the adapter from the chat's runtime. Credential routes list and edit one slot per registered runtime; with only Cursor registered they answer exactly as before.
- Leak points moved behind the adapter: model-id prefix check (`CheckModel`), credential messages/validation/auth hints (`CredentialSpec`), the `.cursor/hooks.json` workspace check (`CheckWorkspace`), the bridge cancel (`Run.Cancel`), the "model dropped from the menu" note (`DropModel`), the key-rejected detection (`Unauthenticated`), account lookup. Still Cursor-specific on purpose: the route `GET /me/cursor` (part of the pinned seam), the `cursor-sdk` runtime id, the `cursor/…` Studio model ids.
- No behavior change is pinned by goldens written **before** the move and unchanged after it: `host/testdata/golden/agent-routes.json` (capabilities, sessions, prompt outcomes, the events on the session stream, transcript, export, credentials, every fail-closed answer) and `codingagent/cursor/testdata/golden/{sandbox-probe,synthetic}.json` (bridge frames → events and run totals). `UPDATE_GOLDEN=1` refreshes them on purpose.
- Neutrality is tested with a second fake runtime ("pi") next to the Cursor fake: the contour picks the default, each chat keeps its own runtime, each runtime has its own credential slot, a Cursor chat on a server without Cursor stops instead of falling back.

## Plan

- **Step 1 (done):** the seam, `cursor.Adapter`, goldens, `Session.Runtime`, this ADR.
- **Step 2 (done, without provider keys):** `codingagent/pi`, `providergw` (Yandex pass-through, GigaChat translator), `contour` config, `fetch-pi`, `egress-domains`, doctor checks. See the next section. Open for the live run: real Yandex/GigaChat calls, the model choice, the egress ACL applied on the RU server.
- **Step 3:** Settings section "Coding agent" (runtime of this server, provider, key, check), Studio badges from `Capabilities`, `modelSource` added to the wire additively if the Studio needs it.
- Not planned: removing the Cursor bridge, `bridge.lock.json`, the `cursor-sdk/cursor` slot or `/me/cursor`.

## Step 2: the Pi adapter (implemented)

```
server/internal/codingagent/pi/         the Pi runtime
  pi.lock.json     pinned @earendil-works/pi-coding-agent 1.7.0 (npm tarball, sha512 integrity, Node >= 22.19.0)
  install.go       Extract (verify + safe unpack + marker), Locate (Node version, pinned package, marker)
  models.go        ModelEntry, ValidateModels, ModelsJSON (generated per run)
  rpc.go           JSONL framing (LF only), command writer
  translate.go     Pi records -> the closed event set
  adapter.go run.go  Adapter, one jailed `pi --mode rpc` process per run
server/internal/providergw/             host-side provider gateway (loopback)
  gateway.go       listener, per-run tokens, request log without headers/bodies
  upstream.go      host-allowlist HTTP client (egress), optional CA bundle
  yandex.go        Yandex AI Studio pass-through (Api-Key, OpenAI-Project, x-data-logging-enabled: false)
  gigachat.go      GigaChat translator + OAuth token refresh
  models.go        model id -> provider model name, unknown models refused
  stub/            OpenAI-compatible and GigaChat-format stub providers for tests
server/internal/contour/                contour.json -> runtime, provider, models; Build; EgressHosts
server/cmd/fetch-pi/                    download (or -tarball), verify, install; -verify
```

**Run model.** Open (per prompt) checks the model and the resume handle and starts nothing. Start launches one `node <pi>/dist/bundle/cli.js --mode rpc` per run inside `isolate.Command` with `RequireJail`: bubblewrap, clean environment, the server's data folder replaced by an empty tmpfs, the chat's workspace writable, the chat's Pi session folder writable (`<data>/agents/pi/sessions/<handle>`, so resume is Pi's own `--session-id`), the per-run agent folder writable and deleted after the run, Node's folder and the Pi package read-only. Flags: `--provider vibechat --model <id> --thinking off --no-extensions --no-skills --no-prompt-templates --no-themes --no-mcp --no-approve --offline`: nothing project-local is loaded (an extension could register another provider), Pi talks to the one generated provider. Cancel sends `abort`; the context is the hard stop (abort, then SIGTERM/SIGKILL of the process group after a grace period). A dialog request from Pi is answered `cancelled`. A record over 16 MB, a crash before `agent_settled`, or a refused prompt is a run error with Pi's stderr (the gateway token redacted). The session handle is a random 128-bit hex string; a chat keeps its runtime for good, and Pi refuses a handle that is not its own.

**Fail closed.** `Ready()` is nil only with: bubblewrap working (`isolate.JailAvailable`), Node >= 22.19.0 (a Bun shim answering `node --version` fails), Pi at exactly the pinned version with the marker fetch-pi writes after the integrity check, at least one valid model, a provider, and the provider's key in the server environment. Anything else leaves the agent off with the reason ("The agent is off on this machine: ..."); there is no fallback to Cursor, to an unjailed Pi or to another provider. `isolate.Command` refuses `RequireJail` programs without bwrap even when the process-wide Require is off.

**Secrets.** Provider keys exist only in the server's environment (`VIBECHAT_YANDEX_API_KEY`, `VIBECHAT_GIGACHAT_AUTH_KEY`; the unit's `EnvironmentFile`, 0600) and are read per request by the gateway. `contour.json` is parsed strictly (unknown fields are errors, so a pasted key is rejected and not echoed). Pi's `models.json` has `baseUrl` = the loopback gateway and `apiKey: "$VIBECHAT_GATEWAY_KEY"`; the variable holds a random per-run token that opens only the gateway and only while the run lasts. Neither argv nor any file nor any log line holds a provider key or the token. Pi never sees the provider's model name (the gateway swaps it; a model outside the config is refused with 400).

**Event mapping** (golden-tested against a capture of the real Pi, `testdata/real-run.jsonl`, plus synthetic and aborted streams): `message_update/text_delta` -> `message.delta`; `thinking_delta`/`thinking_end` -> `thinking.delta`/`thinking.completed` (duration measured); `tool_execution_start/update/end` -> `tool.started/delta/finished` (`bash` is `shell`, `read/write/edit/ls` are `file`, `grep/find` are `search`; shell output is delta-ed from the partial results; a failed tool has `error: ...` as detail); assistant `message_end` usage -> cumulative `usage`; `agent_settled` ends the run; an aborted run is `Result.Error = "cancelled"`; a provider error or a failed retry is the error text. `files.changed` and `session.status` stay the host's (it lands the worktree and publishes them, as for Cursor). Everything else Pi emits (queue, compaction, retry start, extension and session-info records, the tool-call argument stream) has no event on the contract and is dropped. Only writes inside the workspace count as changed files.

**Gateway.** One loopback listener per server, `POST /v1/chat/completions`, bearer = a live run token. Yandex is a pass-through (body and SSE stream unchanged) with `Authorization: Api-Key`, `OpenAI-Project: <folder>` and `x-data-logging-enabled: false` unless `keepLogging`. GigaChat: `tools` -> `functions`, `tool_choice` -> `function_call`, assistant `tool_calls` -> one `function_call` message per call, role `tool` -> role `function` (non-JSON results wrapped as `{"result": ...}`), arguments object <-> JSON string, the whole-call stream chunk -> OpenAI tool-call deltas with `finish_reason: tool_calls` and usage, OpenAI-only fields dropped, OAuth token fetched with the authorization key, kept for its 30 minutes, renewed a minute early and once on a 401. `functions_state_id` is carried inside the tool-call id and sent back. The upstream client dials only the provider's hosts (and no redirect to another host); GigaChat can add the Russian root CA bundle (`caFile`); a missing bundle is an error, never a fallback to system roots.

**Contour config** (`<data>/contour.json`, or `VIBECHAT_CONTOUR_FILE`; no file = the Cursor contour, as before). `hostwire.Config.Contour`; the Pi adapter is registered next to the Cursor adapter, the contour's runtime is the default for new chats. Example (RU, Yandex; one provider only):

```json
{"contour": "ru", "runtime": "pi", "provider": "yandex",
 "pi": {"node": "/opt/node/bin/node", "dir": "/opt/vibechat/pi"},
 "yandex": {"folderId": "b1g...", "models": [
   {"id": "yandexgpt", "label": "YandexGPT", "upstream": "gpt://{folder}/yandexgpt/rc", "contextWindow": 32000, "maxTokens": 8192}]}}
```

**Egress.** `vibechat-server egress-domains` prints the hosts the contour's agent traffic may reach: Yandex `ai.api.cloud.yandex.net`; GigaChat `api.giga.chat`, `ngw.devices.sberbank.ru`; nothing else, and only the chosen provider's. The server owner applies them to the container's network policy (the existing Incus ACL machinery of the desktop provisioning is not changed by this step). `vibechat-server doctor` checks the contour, the jail, Node, the pinned Pi, the provider key and prints the same list.

**Install.** `go run ./cmd/fetch-pi -out <dir>` (or `-tarball f.tgz` offline) downloads the pinned npm tarball, checks its sha512 against `pi.lock.json`, unpacks it (files and folders only, nothing outside `package/`) and writes `.vibechat-pi.json`; `-verify <dir>` runs the server's own check. Node is not installed by this step.

### Step 2: what is verified (no provider keys, a box with bwrap and Node 22.23.5)

- Real Pi 1.7.0 in the real jail against the stub provider through the real gateway (`VIBECHAT_PI_DIR`, optional `VIBECHAT_PI_NODE`; they skip without it): write a file with streamed tool calls, resume keeping the conversation, the agent's shell sees neither the provider key nor the data folder, cancel of a 60-second shell call, a provider 403 becoming the run error, the same through the GigaChat translator and OAuth; and through the host routes (project, session, prompt, SSE events, landing of the change, no Pi key slot in Settings).
- Always running: a fake Pi process (the test binary) in the real jail observed from inside (argv flags, environment without the key, `models.json` without any secret, hidden data folder, read-only Pi folder, token revoked and run folder removed afterwards, abort honored or killed, dialogs cancelled, 2.5 MB records with U+2028, over-limit records, crash with redacted stderr), fail-closed matrix, install and Node-version checks, unsafe tarballs, framing, model validation, contour validation and egress lists, golden event mapping, gateway token, Yandex pass-through and header injection, GigaChat request/response/stream translation, token refresh and the 401 retry, the host-allowlist client, `isolate` `ReadOnly` and `RequireJail`.

### Gaps (cannot be known without real keys; all fail visibly, none silently)

1. **GigaChat tool round-trip.** The documented format is implemented and stub-tested, but `functions_state_id` handling, schema limits of GigaChat-2 (union types), and the behavior of tool loops with real answers are unverified. Needs the golden live run.
2. **Yandex specifics.** Whether the endpoint accepts `stream_options.include_usage` and `max_tokens` as Pi sends them, the exact model URIs and their tool-call quality over long loops, and the logging terms of `x-data-logging-enabled`. `compat` can be tuned per provider (the generator takes extra compat fields) once seen live.
3. **Gateway token in the agent shell.** Pi's bash tool inherits Pi's environment, so the per-run gateway token is readable by the agent. It is loopback-only and dies with the run, but while the run lasts the agent can call the gateway (and so spend the provider quota) outside Pi, and the jail shares the host network (Pi needs TCP to the loopback gateway), so other loopback services are reachable (the API needs the host token). Next hardening: a unix socket or a network namespace with only the gateway forwarded.
4. **Network egress of the jail** is limited by the server's container policy, not by the jail: `egress-domains` is the list to apply; applying it on the RU server and testing it is open.
5. **Git in a worktree chat.** The worktree's `.git` file points into the project folder, which is hidden in the jail, so `git` inside Pi's shell may not work there; the host lands and commits the worktree itself. To be checked live.
6. **Windows and macOS** have no jail: Pi is Linux-only by design (Ready fails closed elsewhere).

## Step 3: providers as data, live findings (2026-11-28)

**Providers are data.** Besides the `yandex` and `gigachat` presets, `contour.json` takes `"provider": "openai"` with an
`openai` block: `id` (menu prefix), `baseUrl`, `keyEnv` (the environment variable that holds the key, `VIBECHAT_<NAME>_KEY`,
or `noKey`), `auth` (`header`, `scheme`: Bearer by default, `Api-Key`, `none` = the bare key), fixed `headers` (no
secrets; `Authorization`, `Cookie`, `Host` ... are refused), `quirks` (`uniqueToolCallIds`), `compat` (single keys of Pi's
models.json compat block, allow-listed and type-checked), `caFile` and `models`. A new OpenAI-compatible provider (OpenRouter,
gpt-oss-120b on a Russian cloud, a vLLM gateway) is one such block, no code; egress is the block's host only. Yandex is now a
preset of the same `providergw.OpenAICompat` (Api-Key, `OpenAI-Project`, logging off, `uniqueToolCallIds`).

**Yandex quirk, handled and tested:** Yandex answers every tool call with the function name as its id, so two parallel calls of one
tool collide. The gateway rewrites ids on the way out to `vc<8 hex>_<index>_<original>` (plain answer and SSE stream; the same index keeps
its id inside a stream) and puts the original back on the next request (assistant `tool_calls` and `role: tool` messages). It is
stateless (the original is inside the id), so a resumed chat works; requests without tool history stay byte for byte.
Tests: gateway (plain and stream, restore, untouched ids, numbers beyond 2^53 preserved) and a real Pi with parallel same-name calls.
Other observed Yandex facts: content starts with a space; tool calls stream whole in one chunk; usage chunk has empty `choices`;
model names in answers omit the folder. Live (Alice AI LLM Flash, a few calls, before the owner moved the pilot to OpenRouter): a
Python edit/write with apostrophes in the arguments made the model emit `\'` (invalid JSON) and Yandex ended the stream with
`Stream error`; with double quotes the three-step task (write, edit, bash) passed. Yandex is no longer called by the pilot.

**Live pilot provider: OpenRouter** (`https://openrouter.ai/api/v1`, `openai/gpt-oss-120b`, key `VIBECHAT_OPENROUTER_KEY`): real Pi
through the gateway, three steps in one resumed chat (write a file, edit it, run it with bash): all tool calls were made with unique
ids, the file ends as asked and `python3 hello.py` prints the new text; Pi's thinking deltas map to `thinking.*` events.
`TestLiveCodingTask` (`VIBECHAT_LIVE=1`, off by default) repeats it against any provider.

## RU contour spike: GigaChat vs Yandex AI Studio (from official docs, no API calls, no keys used)

What Pi needs from a provider (Pi docs, `models.json`): `baseUrl`, `"api": "openai-completions"` (or `openai-responses`), `apiKey` as a literal, `$ENV` or `!command` (run at request time, not cached), headers with the same interpolation. Coding needs OpenAI-style `tools` / `tool_calls` and SSE streaming.

| | **Yandex AI Studio** | **GigaChat (Sber)** |
|---|---|---|
| Endpoint | `https://ai.api.cloud.yandex.net/v1` (Chat Completions and Responses) | `https://api.giga.chat/v1/chat/completions` |
| Auth | static API key of a service account, `Authorization: Api-Key <key>`; folder in `OpenAI-Project` | OAuth: `POST https://ngw.devices.sberbank.ru:9443/api/v2/oauth` with the authorization key and `scope=GIGACHAT_API_PERS` (or B2B/CORP) gives a **30-minute** bearer token |
| Tool calling | OpenAI `tools` / `tool_choice` (`auto`, `none`), parallel `tool_calls`, result as role `tool` with `tool_call_id` | **Not OpenAI-shaped:** request field `functions` (not `tools`), reply `message.function_call` with `finish_reason: "function_call"`, `arguments` is a JSON **object** (not a string), result as role `function`; in a stream the name and arguments arrive in **one** chunk. Parallel calls only on `GigaChat-3-Ultra` via `model_options.parallel_tool_calls` |
| Streaming | `stream: true`, SSE (Chat Completions and Responses) | `stream: true`, SSE, ends with `data: [DONE]` |
| Pi plug-in | `models.json`: `baseUrl`, `api: openai-completions`, `apiKey: "$YC_API_KEY"`, header `OpenAI-Project: $YC_FOLDER_ID`; model ids `gpt://<folder>/<model>/<version>` | Official docs call it "partially compatible"; Pi's `openai-completions` sends `tools`, so a translating gateway is required (see below) |
| Data | the provider documents `x-data-logging-enabled: false` to opt out of logging (confirm in the contract) | the Russian Trusted Root CA must be trusted for `ngw.devices.sberbank.ru:9443` and `api.giga.chat` |
| Risk for coding | model quality for long tool loops is unproven; run the golden set | same, plus translation edge cases (union schemas on GigaChat-2) |

**Recommendation: default = Yandex AI Studio via Pi `openai-completions`.** It is the only one of the two that works with Pi's request format as is, with a static key (no refresh), the OpenAI tool format and parallel calls. GigaChat is the second provider, behind the same gateway.

**Host-side provider gateway (implemented in step 2, used for both).** A small Go listener in `vibechat-server` (loopback only) that Pi's `baseUrl` points to: it injects the real credential, so the isolated Pi process never holds a provider secret and its only egress is the loopback port; for GigaChat it also refreshes the 30-minute token and translates `tools` ↔ `functions`, `tool_calls` ↔ `function_call` (object ↔ JSON string arguments, role `tool` ↔ `function`) in both the plain and the SSE path. The community proxy `gpt2giga` (MIT, Python) already maps OpenAI tools/streaming to GigaChat and bundles the Russian root CA; it is a reference and a fallback, but a Python process is an extra runtime on the server, so the Go gateway is preferred. A Pi extension with `registerProvider` is the alternative if the gateway is rejected, but the token then lives inside the sandbox.

**What the owner provides (keys never go through chat or this repo):**
- Yandex Cloud: a cloud and folder with billing, the folder id, a service account with role `ai.languageModels.user`, an API key with scope `yc.ai.foundationModels.execute`; the list of models to offer (ids as `gpt://<folder>/<model>/<version>`). Optional: confirm the data-logging terms.
- GigaChat (second): a project in GigaChat API with an authorization key and the scope of the plan (`GIGACHAT_API_PERS` for an individual, `GIGACHAT_API_B2B` or `GIGACHAT_API_CORP` for a legal entity, with the contract); the models to offer (GigaChat-2-Pro/Max, GigaChat-3-Ultra).
- A RU server (contour) with Node ≥ 22.19, outbound HTTPS only to the selected provider's hosts, and the Pi release to pin.
- A decision on one default model for the golden run, and on who signs off the data-processing terms for RU.

## Consequences

- One more concept (`Session.Runtime`), one more registry; the wire and the Studio are unchanged.
- A host built for the RU contour does not contain or contact the Cursor SDK; EU/US builds keep it.
- Pi's security rests on our isolation (no sandbox of its own): the reason for the jail-only rule, the gateway and the per-contour egress ACL (see Gaps 3 and 4 for what remains).

## Sources (checked 2026-11-28)

- GigaChat: <https://developers.sber.ru/docs/ru/gigachat/guides/compatible-openai>, <https://developers.sber.ru/docs/ru/gigachat/guides/functions/overview>, <https://developers.sber.ru/docs/ru/gigachat/guides/functions/generating-arguments-for-custom-functions>, <https://developers.sber.ru/docs/ru/gigachat/guides/response-token-streaming.md>, <https://developers.sber.ru/docs/ru/gigachat/api/reference/rest/post-chat.md>, proxy: <https://github.com/ai-forever/gpt2giga>
- Yandex AI Studio: <https://yandex.cloud/ru/docs/ai-studio/> (the docs pages answer a captcha to scripts; facts taken from the official examples <https://github.com/yandex-ai-studio/yandex-ai-studio-api-examples>, the official LiteLLM and tracing tutorials <https://yandex.cloud/ru/docs/tutorials/security/traces-for-litellm-proxy>, <https://yandex.cloud/ru/docs/monium/traces/llm/auto_instrumentation>, and <https://yandex.cloud/ru/blog/ai-studio-llm-agentic-api-context>)
- Pi `models.json`: <https://github.com/earendil-works/pi/blob/main/packages/coding-agent/docs/models.md>
