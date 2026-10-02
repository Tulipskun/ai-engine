# Kaggle deployment

The daemon runs on Kaggle CPU. The notebook clones this repository, installs the
Go toolchain, runs the test suite, builds `cmd/ai-engine`, publishes a
Cloudflare quick tunnel and keeps serving until its time is up.

- `aixodia.ipynb` — the notebook itself
- `kernel-metadata.json` — what `kaggle kernels push` needs, including the kernel id
- `.github/workflows/kaggle-deploy.yml` — starts the runs (in the repository root)

```bash
export KAGGLE_API_TOKEN=...        # token with kernels write permission
kaggle kernels push -p deploy/kaggle -t 43200
```

`-t 43200` is twelve hours, the CPU cap. Kaggle takes the lower of that and its
own maximum, so asking for the cap asks for as long as it allows.

## Staying up across the cap

**A Kaggle kernel cannot start another Kaggle kernel.** There is no API call for
it from inside a running one, and the token that would allow it must not exist
there (CON-012: the daemon has no credential of its own). So the successor is
started from outside, and the two halves are:

| who | what |
|---|---|
| `.github/workflows/kaggle-deploy.yml` | every 11 hours, and on any push that touches the daemon, it runs `kaggle kernels push -t 43200`, which uploads the notebook **and starts a run** |
| the notebook | stays serving until it sees the successor announce itself in D1, then stops — so the tunnel is never dropped |

The announcement is the state key `handover/ready`, which the notebook writes
once its tunnel URL is known. A kernel only counts an announcement written
*after* it started, otherwise a fresh instance would read the previous one's
record on its first poll and stand itself down immediately. Anything older than
its own start time is ignored, and the poll runs once a minute rather than once
per loop.

The tunnel URL still goes to the phone: save the `TUNNEL_URL=` line from the
kernel log, and the daemon announces the same URL plus its build label into the
D1 `nodes` row, so a D1 query says which build is actually serving.

## Time is counted from the kernel, not from the daemon

Cloning, installing Go and running the tests are spent before the service
starts. The first code cell records `KERNEL_T0` and the cap is measured from
there, so the run asks for the twelve hours it actually has. Measuring uptime
from the daemon would add the build time on top and get the kernel killed
mid-answer.

Three values can be set as environment variables on the kernel, so the shape
does not need a code change to adjust:

| variable | default | meaning |
|---|---|---|
| `AIX_KERNEL_BUDGET_S` | `43200` | total wall time allowed for the run |
| `AIX_HANDOVER_LEAD_S` | `1800` | how long before the cap the handover window opens |
| `AIX_HANDOVER_POLL_S` | `60` | how often the successor is looked for |
| `AIX_HARD_STOP_MARGIN_S` | `120` | stop this far before the cap, whatever else is true |

## Updating

Push a commit that touches the daemon. The workflow's push trigger deploys it,
the notebook clones `main` at run time so it is always the newest code, and the
build is stamped into the binary with `-ldflags -X main.version=<sha>` and
announced in `nodes.version`. A new instance standing down the one it replaces
is what a deploy looks like from the outside: same address in the phone, new
build answering.

To replace a running kernel deliberately, run the workflow with `force=true`,
which starts a run even though one is live.

## Things that bite

- Kaggle allows five concurrent CPU sessions per kernel. The workflow checks
  whether a run is live first and skips when one is, so it will not stack runs
  on its own; `force=true` bypasses that and can hit the limit.
- Handover needs `CF_TOKEN` as a Kaggle secret so the notebook can read and write
  the announcement. Without it the notebook says `handover_disabled` and simply
  stops at its cap, which leaves a gap rather than a handover.
- The notebook still carries the D1 mirror cells (0–6) from the mirror watchdog.
  They find no `SCHEDULE` in their own output and skip, so they are inert, but
  they are not what this notebook is for.
