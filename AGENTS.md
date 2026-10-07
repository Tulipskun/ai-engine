# AGENTS

## Verify changes
- `go build ./...`, `go vet ./...`, `gofmt -l .`, `go test ./...` — the whole toolchain. No CI, no lint config, no Makefile.
- `go test ./io/gateway` is the useful one: it drives the gateway end to end against a fake OpenAI upstream (no network, no D1) and covers model routing, the tool loop, SSE streaming, session persistence, and history merging. Run it after touching `io/gateway`, `provider/`, `session/`, or `tools/`.
- `db` and `config` cannot be unit-tested offline: `db.Verify` must hit api.cloudflare.com first or every call returns "Verify must succeed first".

## Architecture (not obvious from filenames)
- Module `ai-engine`, Go 1.22.2, **stdlib only** (no go.sum, no third-party deps).
- `db/` is not a local database — it is a thin HTTP client for the Cloudflare **D1 REST API**. No SQL files, no migrations; schema lives only in D1 and in the column names quoted in code.
  - The query body must be a **JSON object**: `{"sql":"...","params":[...]}`. An array of statements is rejected with error 7400 ("Expected object, received array") — this silently broke the original code.
- Startup (`main.go`): `CF_TOKEN` → `db.Verify` → `config.LoadConfig()` (error logged, process continues) → serve `127.0.0.1:8787` → config reloads every 60s → `cloudflared` quick tunnel → `db.SaveTunnel(url)`.
  - `db.Verify` sets package globals (`token`, `AccountID`, `DBID`); every `db.*` call fails until it runs.
  - D1 target: first account the token can see; database named `aixodia`, else the first one. No env override.
  - `cloudflared` must be on PATH; the public URL is scraped from its stderr (45s timeout).

## HTTP API (`io/gateway`)
- `GET /healthz`, `GET /v1/providers`, `GET /v1/models[?provider=Name]` (fetches upstream `/models`, 60s cache).
- Sessions: `POST /v1/sessions`, `GET /v1/sessions`, `GET|DELETE /v1/sessions/{id}`, `GET /v1/sessions/{id}/turns`. Go 1.22 `ServeMux` patterns with `{id}`.
- Chat: `POST /v1/chat/completions`, OpenAI-compatible, `"stream":true` for SSE.
- **Model routing**: `model` must be `provider/model` (e.g. `NousResearch/inclusionai/ling-3.1-flash`), or give the `provider` field. A bare model name errors with 400 — there is no model→provider index anywhere.
- **Sessions own history**: when `session_id` is set the engine loads turns from D1 and merges the incoming `messages` by common prefix, so clients may send either the full history or only the new turn. Reply, tokens, and duration are appended as turns; the session's `provider`, `model`, and first `title` are written on success.
- Defaults: all builtin tools are offered unless the body has `"tools": []`; context is trimmed to `session.DefaultMaxTokens` (100k estimated tokens); at most 4 tool rounds; upstream calls get 3 attempts with 750ms/1.5s backoff on retryable errors, keys rotated round-robin per provider; `sub_provider`/`sub_model` is a one-shot fallback while nothing has been streamed yet.
- Upstream failures return 502 `{"error":{"message","type":"upstream_error"}}`; mid-stream failures emit a `data: {"error":...}` event then `[DONE]`.

## Schema in use
- `providers(index, provider, endpoint, keys, adapter, free)` — no `model` column, so the client always names the model.
  - `config.Provider` fields differ from columns; mapping is `config.providerFromRow` (`index`→`ID`, `provider`→`Name`, `endpoint`→`APIURL`). `keys` is a JSON array as text.
- `sessions(...)` and `turns(id, session_id, seq, role, text, model, input_tokens, output_tokens, duration_ms, ...)` — other clients already write here (existing rows with Thai titles), so keep column names and `UNIQUE(session_id, seq)` in mind.
  - Only `user`/`system`/`assistant` text is persisted. Tool calls are executed inside one request and **not** stored, so tool detail does not survive across turns (the turns table has no tool columns).
- `tunnel(url)`: `db.SaveTunnel` creates the table, deletes all rows, inserts one.

## db package rules
- Helpers `Query`, `Select`, `Insert`, `Update`, `Delete`, `Where`; identifiers must match `^[A-Za-z_][A-Za-z0-9_]*$`, values always bound as `?`.
- `Update` needs a non-empty `Where`; `Delete` with no `Where` wipes the table. Writes need raw `db.Query` for `ORDER BY` (`session` does this for turns).
- Rows come back as `[]map[string]any`; D1 numbers arrive as `float64`.

## Adapters (`provider/`)
- Shared types and the `Adapter` interface live in package `provider` (`provider/types.go`). They are **not** under `provider/internal/`: Go's `internal` rule would block `session`, `tools`, and `io/gateway` from importing them, and that directory was removed.
- Only the **openai** adapter exists (streaming, tools, usage). Registry maps `opencode` → the same adapter (OpenCode Zen speaks the OpenAI protocol). `provider/anthropic` and `provider/gemini` are still empty placeholders — a D1 row using those adapter names fails with `unknown adapter`.
- Outbound HTTP must send `User-Agent: ai-engine/1.0` (`provider.UserAgent`). Cloudflare returns a 403 HTML challenge for Go's default `Go-http-client/1.1` — this was a real outage on NousResearch.

## Live provider state (verified 2026-10-07, all from this host)
- **NousResearch** works: `inclusionai/ling-3.1-flash` returns real completions, but with frequent transient `429 temporarily at capacity upstream` — retry, do not treat as fatal.
- **Opencode**: free models return `403 FreeTierError "can only be used from within OpenCode"` (client-gated); paid models return insufficient funds.
- **AgentRouter** answers `/models` with WAF HTML; **tokenharbor** balance is zero; **cavoti** has no eligible supplier. Errors surface as 502 with the upstream message — that is expected, not a bug in the engine.
- Only `GET {endpoint}/models` (OpenAI style) is implemented for model discovery.

## Layout
- Provider-specific logic goes in `provider/<name>/`, gateway logic in `io/gateway/`, model-calling helpers in `tools/`. Empty `.gitkeep` dirs (`provider/anthropic`, `provider/gemini`, `provider/opencode`, `io/`) are still reserved boundaries.
- `session/` is real Go now (D1 store + context trimming) — the old pseudocode sketches are gone.

## Runtime requirements
- Needs `CF_TOKEN`, `cloudflared`, and outbound HTTPS to `api.cloudflare.com` plus the provider endpoints. No offline mode.
