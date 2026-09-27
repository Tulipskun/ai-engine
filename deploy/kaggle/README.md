# Kaggle deployment

The daemon runs on Kaggle CPU: the notebook clones this repository, runs the
test suite, builds `cmd/ai-engine`, publishes a Cloudflare quick tunnel and
keeps the service alive until the 12-hour kernel cap.

`aixodia.ipynb` is the notebook itself, `kernel-metadata.json` is what
`kaggle kernels push` needs. Push from this directory:

```bash
export KAGGLE_API_TOKEN=...        # token with kernels write permission
kaggle kernels push -p .
```

The phone owns the runtime address: it saves the tunnel URL printed as
`TUNNEL_URL=https://...trycloudflare.com` in settings, and the daemon announces
the same URL plus its build label into the D1 `nodes` row, so a D1 query says
which build is actually serving.

Two things to know before pushing a new version:

- Kaggle allows five concurrent CPU sessions per kernel. Push a new version
  while five are running and it fails with `Maximum batch CPU session count of
  5 reached`; an old version has to finish or be cancelled first.
- The phone keeps one address. Until it is pointed at the new tunnel, a stale
  version can keep serving and every daemon writes to the same `nodes` row, so
  read `nodes.version` to tell them apart.

JEV's decision guard needs no credential: `jev-1.13-free` on
`https://opencode.ai/zen/v1/systemone` answers with the OpenCode client's
headers alone. `AI_JEV_ENABLED=0` turns the guard off.
