# Repository Index

Read this file first. It maps the whole repository in one call so workers
skip 5-10 discovery calls: there is no list or search tool, so this file and
the shell are the only map. Read files with `read` one at a time, and reach
for `bash` (`ls`, `rg`, `sed`, `cat`, `go test`) for everything else.
Before any code change read this file; after any
code change update this file when structure or key files changed.

## Top-level tree

Each package below exists because of one question it answers. Read it before
touching code; the tree is the map a worker has instead of a search tool.

```text
.
├── provider/        what a provider call is made of, and where it goes
│   ├── types.go       Request/Response/Turn/ToolCall/Usage/Model/SessionConfig
│   ├── router.go      provider catalogue + model routing
│   ├── client.go      RouterClient, session contract it needs
│   ├── keys.go        key pool and rotation
│   ├── capabilities.go what each model family accepts
│   ├── validate.go    bounds a provider will accept
│   ├── registry/      binds config/provider.json to the adapters (needs them)
│   └── {openai,anthropic,gemini,opencode}/  the wire adapters
├── session/        which session a message belongs to, what it remembers,
│                 and how a turn runs on it (loop, planner, delegation)
├── tools/          the worker's whole world: read + bash, workspace-confined
├── io/             canonical Input/Output, Display routing, trace
│   └── gateway/      WebSocket server, auth gate, tunnel, REST for the phone
├── db/             Cloudflare D1, fresh layer: authoritative state, hydrate/push
├── config/         reserved for runtime config loaders (empty for now)
├── main.go         composition root: CF_TOKEN verify, assemble, run, tunnel
├── harness.go      turn harness: input source → session → displays
├── mobile.go       mobile runtime: gateway wiring, D1 hydrate/push, model store
├── io.go           mobileIO: WS frames ↔ canonical turns, D1 turn mirror
├── admin_store.go  provider keys + agent settings the phone writes
├── agent.go        agent construction, system prompts, workspace root
├── AGENTS.md       spec-first rule + module discipline
├── README.md       how to run the daemon
└── index.md        this file
```

Dependency direction is one way: `main` assembles, then `session`/`io`/`tools`/`db`,
then `provider`. Nothing below imports anything above it, which is why `provider`
declares the `Session` interface it consumes rather than importing `session`.
Internal imports never use the hosting path: the module is `ai-engine`, so code
says `ai-engine/session`, never `github.com/...`; only outside libraries keep
their full paths.

## Module responsibility → key files

| Responsibility | Key files |
|---|---|
| Turn loop model → tool → model | `session/agent.go`, `harness.go` |
| Loop-control caps (REQ-045) | `session/loop_control.go`, `session/loop_control_test.go` |
| Planner (Main Agent, senior: read-only context tools) | `session/plan_tool.go`, `session/plan_tool_test.go` |
| Worker delegation contracts, progress/final reports | `session/subagent.go`, `session/subagent_test.go` |
| Context budget, newest-group truncation | `session/context_window.go` |
| Session persistence (one db per session) | `session/db.go`, `session/settings.go` |
| Provider routing, catalogue, retry | `provider/router.go`, `provider/client.go`, `provider/*/` |
| Worker tool surface (read, bash) | `tools/registry.go` |
| Read tool and workspace path discipline | `tools/files.go`, `tools/files_test.go` |
| Shell tool | `tools/command.go` |
| Runtime config load/save, session manager | `session/system_config.go`, `provider/registry/config.go`, `session/manager.go`, `provider/registry/config_test.go` |
| Stateless runtime state ↔ Cloudflare D1 | `db/client.go` (`providers`/`tunnel`/`nodes`/`sessions`/`turns`/`state` tables, hydrate/push), `db/sync.go` |
| Mobile gateway (AIxodia, the only transport) + auth gate | `io/gateway/gateway.go`, `io/gateway/auth.go`, `io/gateway/auth_test.go`, `io/gateway/tunnel.go`, `io/gateway/history.go` (ประวัติแชทจาก D1 ผ่าน tunnel), `io/gateway/admin.go` (REST surface), `io/gateway/mobile.go` (runtime wiring + Attach), `io/gateway/display.go` (D1 mirror display), `io/gateway/admin_store.go` (provider keys + agent settings ที่มือถือจัดการ), `io/gateway/agent.go` (agent construction, prompts, workspace) |
| Composition root: CF_TOKEN verify → assemble → harness loop → tunnel | `main.go` เท่านั้น (boot: verify token, providers จากตาราง D1, assemble ผ่าน `gateway.Attach`, รัน `session.HarnessLoop`) |

## Key files (what each owns)

- `session/agent.go` — provider-neutral control loop; owns request composition,
  tool-result continuation, and per-session planning switch. No transport code.
  Enforces REQ-045 loop-control caps per attempt (fail-fast, never retried).
- `session/loop_control.go` — hard loop caps (max tools/turn, max consecutive
  read/edit, max bash output) with the fatal-error classifier. No transport.
- `session/subagent.go` — async delegation (`delegate_task` starts and returns a job
  id at once, `delegate_message` sends more work into the same worker session,
  `delegate_stop` blocks, `delegate_status` reads one job, `delegate_result`
  returns the handoff report and accepts a verified step); progress report every
  X tool calls plus a complete handoff report (tool names, args, results, error
  flags). A follow-up routes to a retry when the named job still holds the
  current plan step, and to follow-on work otherwise.
- `session/plan_tool.go` — planner-only tool surface (`plan` + the five orchestration
  tools + `read`); the checklist is injected into the planner system prompt every
  iteration. `orchestrationTools` is the single list behind both Definitions and
  Execute, so an advertised tool can never be unroutable.
- `session/context_window.go` — token budget (default 58000); keeps newest
  complete user→response→tools groups unsplit, drops oldest first.
- `session/db.go` — one SQLite db per session under `data/sessions/`;
  persists settings, turns, and plan state across restarts, adding new
  generation columns to a database written before they existed.
- `tools/registry.go` — worker `Registry`: registers the whole tool surface
  (`read`, `bash`), resolves per-session workspace root, dispatches `Execute`
  calls. CHANGE-087 retired every other tool.
- `tools/files.go` — `read` plus the `safePath`/`withinRoot` discipline every
  tool relies on (4 MiB per file, traversal and symlink escapes rejected).
- `session/system_config.go`, `provider/registry/config.go` — the single
  reader/writer pair for `config/system.json` and `config/provider.json`; the
  phone's admin store writes through these rather than re-implementing the
  mkdir/chmod/write rules.

## Docs pointers

- `AGENTS.md` — spec-first workflow and module-discipline rule.

Generation settings (REQ-049) live in `provider/types.go` (the knobs and their single
merge point), `provider/validate.go` (the bounds), `provider/capabilities.go` (what each
model accepts), and are read from `config/system.json` and set by the phone
through `io/gateway/generation.go`.

## Start-here routes

- Fix a turn-loop bug: `session/agent.go` + `session/harness.go` + `session/context_window.go`.
- Change planner/worker behavior: `session/plan_tool.go` + `session/subagent.go`.
- Add or change a worker tool: `tools/registry.go` + owning `tools/*.go`
  file; keep the change in the owning module (no cross-module refactors).
- Provider / catalogue / retry issue: `provider/client.go` +
  `provider/router.go` + `provider/<adapter>/`.
- Daemon start / gateway / tunnel / D1 hydration: `main.go`
  (entry point: verify CF_TOKEN, providers จากตาราง D1, assemble
  ผ่าน `gateway.Attach`, รัน `session.HarnessLoop`), `io/gateway/`,
  `db/`.
- Inbound WS event or outbound frame / D1 mirroring: `io/gateway/display.go`.
- Session persist / workspace / settings: `session/db.go` +
  `session/settings.go` + `session/manager.go`.
- Config or state that must come from D1 instead of disk: `db/`
  keys `config:*` and `sessions/<id>` (CON-012).
- Mobile app (AIxodia) gateway: `io/gateway/` ทั้งหมด (wiring อยู่ใน
  `mobile.go` + `Attach`); `main.go` เป็นแค่ entry point.
  core orchestration stays untouched. There is no other
  transport (CHANGE-059).
- Stateless runtime state / D1 sync: `db/` only (local materialization
  stays `config/*.json` plus one SQLite file per session).

## Tests

Every package with behaviour worth pinning has offline tests; they need no
provider key, no network and no Cloudflare token:

- `session/loop_control_test.go` — REQ-045 caps and that budget failures are fatal.
- `session/subagent_test.go` — REQ-019/REQ-020 reservations: one running job per
  parent, retry after failure reuses the worker session, follow-on waits for
  acceptance, replaced plans invalidate old jobs, jobs are parent-scoped.
- `session/plan_tool_test.go` — planner surface: no execution tools, and every
  advertised orchestration tool is routable.
- `tools/files_test.go` — `safePath` traversal/symlink escape, and that the
  worker surface is exactly read+bash.
- `provider/registry/config_test.go` — config reader/writer round trips, bounds, adapter
  inference.
- `io/gateway/auth_test.go` — REQ-046(2) handshake gate: what counts as a
  failed attempt, the lockout schedule, fail-closed on verifier outage, and that
  a rejection never echoes the token.

Run them with `go test ./... -timeout 2m`; add `-race` before pushing a change
that touches the delegation or session paths.
