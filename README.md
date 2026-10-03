# Tulipskun/ai-engine

The AI harness behind **AIxodia** (the Android client). `ai` is a single
daemon whose only gateway is a WebSocket published through a Cloudflare quick
tunnel; runtime configuration and session state live in Cloudflare D1 (REQ-047).

## Run

```bash
go build -o ai ./cmd/ai-engine
./ai          # or: ./ai daemon
```

There is no CLI, no Discord bot and no self-update any more (CHANGE-059): the
phone is the only client, the daemon owns no state, and there is nothing to
hand over on restart.

## Tools

The worker's tool surface is two tools (`CHANGE-087`, `CHANGE-098`):

- `read` — one UTF-8 text file inside the workspace, 4 MiB cap, `safePath` /
  `withinRoot` enforced
- `bash` — the only execution tool inside the workspace: create, edit, search,
  list, build and test all go through the shell

The Main Agent (planner) gets `read`, `plan`, and the sub-agent orchestration
tools; it cannot execute. There is no browser, no attachment store, no OS input,
no screen control, no background job manager and no `web_fetch` tool — `bash`
(and `curl`) covers those jobs.

## Gateway and state

- **One ingress**: `transport/mobile` listens on localhost and publishes itself
  with `cloudflared tunnel --url …`. The daemon writes the random URL into the
  D1 `nodes` row as its own heartbeat — and answers `GET /api/node` with the same
  values — so a client can find it through either route.
- **Two-step access** (REQ-046): the unguessable tunnel hostname, then
  `Authorization: Bearer <Cloudflare API token>` on the WebSocket handshake,
  verified against Cloudflare (`GET /user/tokens/verify`) before the socket is
  upgraded. Missing header → 401; five wrong tokens → 429 and a progressive
  lockout (30 → 60 → 120 → 240 → 300s); Cloudflare unreachable → 503, fail closed.
- **Stateless** (CON-012): `runtime/d1store` pulls `config:*` and
  `sessions/<id>` from D1 into the state root after the first verified
  connection and pushes changes back. Local files stay a cache, so wiping
  `~/.local/share/ai` is recoverable.
- **No credential of its own**: the D1 token a phone presents is the only
  secret, and it lives in memory until the process restarts.

## config/entry.json

```json
{
  "mobile": {
    "enabled": true,
    "cloudflare_api": "",
    "d1_database": "",
    "listen": "127.0.0.1:18789",
    "tunnel": true,
    "cloudflared": "cloudflared",
    "sync_config": true,
    "sync_sessions": true
  }
}
```

Provider keys, the system prompt and session history are **not** configured here
— they live in D1 as `state` rows (`config:provider`, `config:system`,
`sessions/<id>`) and are pulled in once the
first verified phone connects. There is no `worker_base` any more: the daemon
calls the Cloudflare API directly (`https://api.cloudflare.com/client/v4`) and
discovers the account and the database from the phone's token, so the empty
`cloudflare_api`/`d1_database` above exist only to override that discovery. The
token is never written to a file.

## CI

`.github/workflows/go.yml` builds, tests and vets on every push;
`release.yml` produces the Linux arm64 binary as an artifact (no release tags).
