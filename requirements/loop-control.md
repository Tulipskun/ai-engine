# Loop Control Checklist (REQ-045)

Dual control for worker loops: hard system caps fail a runaway turn
automatically (`sdk/loop_control.go`), and this checklist keeps planners
and workers disciplined so the caps are never hit in normal work.

## Planner self-read (REQ-016, CHANGE-054)

The Main Agent is the senior: it reads code and context itself instead of
delegating investigation blindly. Thinking stays in the main context
(serial on thinking); the worker executes bounded contracts.

- Read `index.md` first in one call, then read only the files it names as
  relevant — the same ordered checklist below, but executed by the planner,
  whose only tool is `read` since CHANGE-087. No write, exec, list or search
  tool, ever: `index.md` is the only map the planner has.
- Planner read budget per planning round: max ~10 reads total
  (`index.md` + the files it points at). If the context is still unclear after that,
  delegate a bounded read-only recon task (lookup only, no edits) instead
  of reading in a loop.
- Never delegate thinking work (design decisions, architecture choices,
  tradeoff calls, judging correctness, task structure). Lookup work
  (find files/symbols, grep call sites, read a file) may be delegated as
  read-only recon, serial on thinking, parallel on lookup.
- Every delegation is a contract: Objective, Non-goals, Authority (allowed
  paths, allowed commands, forbidden actions), Expected tests, Required
  evidence (summary, changed files with reasons, commands run, tests and
  results, known limitations), Acceptance criteria. Verify the returned
  work package against the evidence (spot-check by reading files if
  needed); accept only with verification evidence via
  `delegate_result`. Verify, don't trust.

## Ordered checklist per task

Every delegated task follows this order. No step is skipped, no step is
repeated once its outcome is proven.

1. Read `index.md` first in one call.
2. Read the files `index.md` points at, one call each — never re-read a file
   you already read, and never guess a path you have not seen in `index.md`.
3. Re-check `index.md` plus `requirements/` before and after code changes.
4. Implement the smallest change that satisfies the step.
5. Validate with the minimal sufficient check: one command that proves the
   outcome (or a single combined shell line for related checks).
6. Report file paths changed, the validation command and its outcome, and
   anything left unresolved.

## Tool budget declaration per delegation

Every `delegate_task` / `delegate_message` / `delegate_message`
task states its budget explicitly, for example:

```text
Tool budget: max 6 reads, max 2 edits, max 3 bash calls.
Your tools are read and bash only. If the budget is exceeded, stop as failed
and write a lesson to requirements/lessons.md instead of looping.
```

Guidance: single-digit totals per step. A step that needs more than ~10
tool calls is a step that must be split.

## Reads and the shell (CHANGE-087)

- `read` is the only read tool: one file per call, 4 MiB cap.
- `bash` replaces every tool that used to do listing, search, writing or
  editing: `ls`, `rg`, `sed`, `cat`, `tee`, `python3`, `go test`.
- Read `index.md` before touching anything else, and use `ls`/`rg` through
  `bash` only when `index.md` did not name the file you need — never to
  rediscover what a previous call already returned.

## Stop condition

Budget exceeded = fail, not retry:

1. Stop calling tools immediately.
2. Return the failure with the counts (reads/edits/bash used).
3. Append a lesson to `requirements/lessons.md` (what looped, why, what
   budget or batching would have prevented it).
4. Never retry a loop-control failure in the same shape — the harness
   treats budget errors as fatal and will not retry them automatically.

## System caps (reference)

Enforced in `sdk/loop_control.go`; quoted here so prompts and code agree:

- Max 30 executed tool calls per turn attempt — then auto-fail.
- Max 8 consecutive `read` calls without an intervening progress tool
  (`bash`) — then auto-fail as a stall. `read` is the only probe tool left
  since CHANGE-087.
- Max 1 MiB per `bash` tool result payload — then auto-fail.
- Budget failures are never retried at the turn level.

Staying under budget is part of verified success (planner prompt,
worker prompt).
