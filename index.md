# Repository Index

Read this file first. It maps the whole repository in one call so workers
skip 5-10 discovery calls: there is no list or search tool, so this file and
the shell are the only map. Read files with `read` one at a time, and reach
for `bash` (`ls`, `rg`, `sed`, `cat`, `go test`) for everything else.
Before any code change read this file plus relevant requirements; after any
code change update this file when structure or key files changed and update
requirements plus `requirements/changes.md` when behavior or spec changed.

## Top-level tree

```text
.
├── bin/               empty placeholder (reserved install target)
├── cmd/ai-engine/
│   ├── main.go       entry point: state root, assemble, open WS + tunnel, run
│   ├── io.go         input/output boundary: WS events ↔ sdk.Input/Output, D1 mirrors
│   ├── agent.go      agent construction, system prompts, AGENTS.md, workspace root
│   ├── mobile.go     gateway wiring: D1 client, tunnel, hydrate/push, stores
│   └── admin_store.go  phone write surface for providers, keys and agent settings
├── sdk/               provider-neutral Agent runtime, sessions, orchestration
│   ├── providers/     wire adapters: openai (canonical), anthropic, gemini, opencode
│   └── providers/internal/  shared provider HTTP (JSON + SSE, default User-Agent)
├── tools/             worker tools: read, bash (the whole execution surface)
├── runtime/           config load/save, session manager, provider wiring
│   └── d1store/       Cloudflare D1: authoritative state + hydrate/push sync
├── transport/mobile/  AIxodia WebSocket gateway over a Cloudflare quick tunnel
├── requirements/      source of truth for product behavior (read before code)
├── skills/            contributor procedures (spec management, checklists)
├── AGENTS.md          contributor entry: spec-first rule + module discipline
├── README.md          daemon operation + config layout + AIxodia gateway help
└── index.md           this file (hand-maintained, concise)
```

## Module responsibility → key files

| Responsibility | Key files |
|---|---|
| Turn loop model → tool → model | `sdk/agent.go`, `sdk/loop.go` |
| Loop-control caps (REQ-045) | `sdk/loop_control.go`, `sdk/loop_control_test.go` |
| Planner (Main Agent, senior: read-only context tools) | `sdk/plan_tool.go`, `sdk/plan_tool_test.go` |
| Worker delegation contracts, progress/final reports | `sdk/subagent.go`, `sdk/subagent_test.go`, `sdk/subagent_trace_sink.go` |
| Context budget, newest-group truncation | `sdk/context_window.go` |
| Session persistence (one db per session) | `sdk/session_db.go`, `sdk/session_settings.go` |
| Provider routing, catalogue, retry | `sdk/router_client.go`, `sdk/routing.go`, `sdk/providers/` |
| Worker tool surface (read, bash) | `tools/registry.go` |
| Read tool and workspace path discipline | `tools/files.go`, `tools/files_test.go` |
| Shell tool | `tools/command.go` |
| Runtime config load/save, session manager | `runtime/system_config.go`, `runtime/provider_config.go`, `runtime/session_manager.go`, `runtime/config_test.go` |
| Stateless runtime state ↔ Cloudflare D1 | `runtime/d1store/client.go`, `runtime/d1store/sync.go` |
| Mobile gateway (AIxodia, the only transport) + auth gate | `transport/mobile/gateway.go`, `transport/mobile/auth.go`, `transport/mobile/auth_test.go`, `transport/mobile/tunnel.go`, `transport/mobile/history.go` (ประวัติแชทจาก D1 ผ่าน tunnel), `transport/mobile/admin.go` (provider/key pool + agent settings ที่มือถือจัดการ), `cmd/ai-engine/mobile.go` |
| Inbound/outbound boundary between WS and canonical turns | `cmd/ai-engine/io.go` (`mobileIO`: WS → `sdk.Input`, `sdk.Output` → frames, D1 mirror ทั้งสองทาง) |
| Agent construction, system prompts, workspace root | `cmd/ai-engine/agent.go` |
| Phone-owned provider keys + per-agent routes (file ↔ D1) | `cmd/ai-engine/admin_store.go`, `runtime/provider_manager.go` (Reload/Rt/RefreshProvider) |

## Key files (what each owns)

- `sdk/agent.go` — provider-neutral control loop; owns request composition,
  tool-result continuation, and per-session planning switch. No transport code.
  Enforces REQ-045 loop-control caps per attempt (fail-fast, never retried).
- `sdk/loop_control.go` — hard loop caps (max tools/turn, max consecutive
  read/edit, max bash output) with the fatal-error classifier. No transport.
- `requirements/loop-control.md` — ordered checklist, per-delegation tool
  budgets, batch-read and stop rules (REQ-045 prompt discipline).
- `requirements/lessons.md` — append-only failure lessons (REQ-045).
- `sdk/subagent.go` — async delegation (`delegate_task` starts and returns a job
  id at once, `delegate_message` sends more work into the same worker session,
  `delegate_stop` blocks, `delegate_status` reads one job, `delegate_result`
  returns the handoff report and accepts a verified step); progress report every
  X tool calls plus a complete handoff report (tool names, args, results, error
  flags). A follow-up routes to a retry when the named job still holds the
  current plan step, and to follow-on work otherwise.
- `sdk/plan_tool.go` — planner-only tool surface (`plan` + the five orchestration
  tools + `read`); the checklist is injected into the planner system prompt every
  iteration. `orchestrationTools` is the single list behind both Definitions and
  Execute, so an advertised tool can never be unroutable.
- `sdk/context_window.go` — token budget (default 58000); keeps newest
  complete user→response→tools groups unsplit, drops oldest first.
- `sdk/session_db.go` — one SQLite db per session under `data/sessions/`;
  persists settings, turns, and plan state across restarts, adding new
  generation columns to a database written before they existed.
- `tools/registry.go` — worker `Registry`: registers the whole tool surface
  (`read`, `bash`), resolves per-session workspace root, dispatches `Execute`
  calls. CHANGE-087 retired every other tool.
- `tools/files.go` — `read` plus the `safePath`/`withinRoot` discipline every
  tool relies on (4 MiB per file, traversal and symlink escapes rejected).
- `runtime/system_config.go`, `runtime/provider_config.go` — the single
  reader/writer pair for `config/system.json` and `config/provider.json`; the
  phone's admin store writes through these rather than re-implementing the
  mkdir/chmod/write rules.

## Requirements and docs pointers

- `requirements/README.md` — how to read the spec directory.
- `requirements/product.md` — product purpose and goals.
- `requirements/functional.md` — stable REQ-xxx behavior (check before code).
- `requirements/loop-control.md` — loop checklist + tool budgets (REQ-045).
- `requirements/lessons.md` — failure lessons log (REQ-045).
- `requirements/constraints.md` — non-negotiable limits (CON-xxx).
- `requirements/decisions.md` — accepted architecture decisions.
- `requirements/changes.md` — append-only CHANGE-xxx history.
- `AGENTS.md` — spec-first workflow and module-discipline rule.

Generation settings (REQ-049) live in `sdk/types.go` (the knobs and their single
merge point), `sdk/validate.go` (the bounds), `sdk/capabilities.go` (what each
model accepts), and are read from `config/system.json` and set by the phone
through `transport/mobile/generation.go`.

## Start-here routes

- Fix a turn-loop bug: `sdk/agent.go` + `sdk/loop.go` + `sdk/context_window.go`.
- Change planner/worker behavior: `sdk/plan_tool.go` + `sdk/subagent.go`.
- Add or change a worker tool: `tools/registry.go` + owning `tools/*.go`
  file; keep the change in the owning module (no cross-module refactors).
- Provider / catalogue / retry issue: `sdk/router_client.go` +
  `sdk/routing.go` + `sdk/providers/<adapter>/`.
- Daemon start / gateway / tunnel / D1 hydration: `cmd/ai-engine/main.go`
  (entry point), `cmd/ai-engine/mobile.go`, `transport/mobile/`, `runtime/d1store/`.
- Inbound WS event or outbound frame / D1 mirroring: `cmd/ai-engine/io.go`.
- Session persist / workspace / settings: `sdk/session_db.go` +
  `sdk/session_settings.go` + `runtime/session_manager.go`.
- Config or state that must come from D1 instead of disk: `runtime/d1store/`
  keys `config:*` and `sessions/<id>` (CON-012).
- Mobile app (AIxodia) gateway: `transport/mobile/` plus `cmd/ai-engine/mobile.go`;
  core orchestration stays untouched. There is no other transport
  (CHANGE-059).
- Stateless runtime state / D1 sync: `runtime/d1store/` only (CON-012 keeps
  `config/*.json` and one SQLite file per session as the local materialization).
- New spec conflict: update `requirements/` + append `requirements/changes.md`
  before implementing (see `AGENTS.md` and `skills/`).

## Tests

Every package with behaviour worth pinning has offline tests; they need no
provider key, no network and no Cloudflare token:

- `sdk/loop_control_test.go` — REQ-045 caps and that budget failures are fatal.
- `sdk/subagent_test.go` — REQ-019/REQ-020 reservations: one running job per
  parent, retry after failure reuses the worker session, follow-on waits for
  acceptance, replaced plans invalidate old jobs, jobs are parent-scoped.
- `sdk/plan_tool_test.go` — planner surface: no execution tools, and every
  advertised orchestration tool is routable.
- `tools/files_test.go` — `safePath` traversal/symlink escape, and that the
  worker surface is exactly read+bash.
- `runtime/config_test.go` — config reader/writer round trips, bounds, adapter
  inference.
- `transport/mobile/auth_test.go` — REQ-046(2) handshake gate: what counts as a
  failed attempt, the lockout schedule, fail-closed on verifier outage, and that
  a rejection never echoes the token.

Run them with `go test ./... -timeout 2m`; add `-race` before pushing a change
that touches the delegation or session paths.
