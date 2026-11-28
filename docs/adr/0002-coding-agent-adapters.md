# ADR 0002: Coding-agent adapters, contour-based runtime (EU/US = Cursor SDK, RU = Pi)

Status: accepted. Step 1 (the adapter, no behavior change) is in this commit; steps 2 and 3 are open (see Plan).

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

- **Step 1 (done, this commit):** the seam, `cursor.Adapter`, goldens, `Session.Runtime`, this ADR.
- **Step 2:** `codingagent/pi` adapter. Pi runs as `pi --mode rpc` inside `internal/isolate` (bwrap, no network except the host-side provider gateway, no secrets in env or argv, Node ≥ 22.19). Maps Pi RPC events to the closed event set. Golden run for acceptance: create a file, edit, run shell, cancel, resume, big output, refuse to leave the worktree, `isolationCheck` (no secrets), egress to one host only. Server config: `runtime` per contour, `models.json` per contour, per-contour egress ACL (Incus) allowing only the selected provider's hosts.
- **Step 3:** Settings section "Coding agent" (runtime of this server, provider, key, check), Studio badges from `Capabilities`, `modelSource` added to the wire additively if the Studio needs it.
- Not planned: removing the Cursor bridge, `bridge.lock.json`, the `cursor-sdk/cursor` slot or `/me/cursor`.

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

**Host-side provider gateway (for step 2, recommended for both).** A small Go listener in `vibechat-server` (loopback only) that Pi's `baseUrl` points to: it injects the real credential, so the isolated Pi process never holds a provider secret and its only egress is the loopback port; for GigaChat it also refreshes the 30-minute token and translates `tools` ↔ `functions`, `tool_calls` ↔ `function_call` (object ↔ JSON string arguments, role `tool` ↔ `function`) in both the plain and the SSE path. The community proxy `gpt2giga` (MIT, Python) already maps OpenAI tools/streaming to GigaChat and bundles the Russian root CA; it is a reference and a fallback, but a Python process is an extra runtime on the server, so the Go gateway is preferred. A Pi extension with `registerProvider` is the alternative if the gateway is rejected, but the token then lives inside the sandbox.

**What the owner provides (keys never go through chat or this repo):**
- Yandex Cloud: a cloud and folder with billing, the folder id, a service account with role `ai.languageModels.user`, an API key with scope `yc.ai.foundationModels.execute`; the list of models to offer (ids as `gpt://<folder>/<model>/<version>`). Optional: confirm the data-logging terms.
- GigaChat (second): a project in GigaChat API with an authorization key and the scope of the plan (`GIGACHAT_API_PERS` for an individual, `GIGACHAT_API_B2B` or `GIGACHAT_API_CORP` for a legal entity, with the contract); the models to offer (GigaChat-2-Pro/Max, GigaChat-3-Ultra).
- A RU server (contour) with Node ≥ 22.19, outbound HTTPS only to the selected provider's hosts, and the Pi release to pin.
- A decision on one default model for the golden run, and on who signs off the data-processing terms for RU.

## Consequences

- One more concept (`Session.Runtime`), one more registry; the wire and the Studio are unchanged.
- A host built for the RU contour does not contain or contact the Cursor SDK; EU/US builds keep it.
- Pi's security rests on our isolation (no sandbox of its own): the risk of step 2 and the reason for the gateway and the per-contour egress ACL.

## Sources (checked 2026-11-28)

- GigaChat: <https://developers.sber.ru/docs/ru/gigachat/guides/compatible-openai>, <https://developers.sber.ru/docs/ru/gigachat/guides/functions/overview>, <https://developers.sber.ru/docs/ru/gigachat/guides/functions/generating-arguments-for-custom-functions>, <https://developers.sber.ru/docs/ru/gigachat/guides/response-token-streaming.md>, <https://developers.sber.ru/docs/ru/gigachat/api/reference/rest/post-chat.md>, proxy: <https://github.com/ai-forever/gpt2giga>
- Yandex AI Studio: <https://yandex.cloud/ru/docs/ai-studio/> (the docs pages answer a captcha to scripts; facts taken from the official examples <https://github.com/yandex-ai-studio/yandex-ai-studio-api-examples>, the official LiteLLM and tracing tutorials <https://yandex.cloud/ru/docs/tutorials/security/traces-for-litellm-proxy>, <https://yandex.cloud/ru/docs/monium/traces/llm/auto_instrumentation>, and <https://yandex.cloud/ru/blog/ai-studio-llm-agentic-api-context>)
- Pi `models.json`: <https://github.com/earendil-works/pi/blob/main/packages/coding-agent/docs/models.md>
