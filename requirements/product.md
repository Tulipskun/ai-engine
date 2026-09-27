# Product

`ai` is the AI harness behind the AIxodia Android client. It is a single daemon
with one gateway: a WebSocket published through a Cloudflare quick tunnel
(REQ-047). There is no CLI and no Discord bot any more (CHANGE-059).

## What it does

- Receives canonical `sdk.Input` from the phone and answers with canonical
  `sdk.Output`, including main/sub agent attribution.
- Runs the Main Agent (planning + delegation) and worker agents with the tool
  registry, which since CHANGE-087 is exactly `read` and `bash`.
- Keeps no state of its own: provider keys, system prompt and session history
  live in Cloudflare D1 and are materialized locally on demand (CON-012).
- Keeps going when the app is closed: a turn already accepted finishes and is
  persisted, and the phone catches up from D1 when it reconnects.

## Who talks to it

Exactly one client: AIxodia. It authenticates with the D1 access token in the
WebSocket handshake header; the daemon verifies that token against the Worker
and holds it in memory only.

## Non-goals

- Local operator interaction (no interactive CLI, no slash commands).
- In-place self-update or release management (build and deploy with the normal
  toolchain).
- Public listeners: the tunnel is the only ingress.
