# Failure Lessons (REQ-045)

Append-only log of loop failures and the discipline that prevents them.
Each entry: what looped, why, and the budget/batching rule that stops it
next time. Workers append here instead of retrying a blown budget.

## Format

```text
LESSON-XXX

Date: YYYY-MM-DD
Symptom: <tool counts, visible loop>
Cause: <why it looped>
Fix: <budget/batching/cap rule applied>
```

---

LESSON-001

Date: 2026-09-17
Symptom: Slide-window panel work burned 47 worker tool calls — repeated
`sed` edit loops plus drifting test expectations re-run after every edit.
Cause: No per-delegation tool budget, edits applied one `sed` invocation
at a time instead of batched reads + single deliberate edits, and
validation re-ran overlapping checks after the outcome was already proven.
Fix: REQ-045 dual control — max 30 tool calls per turn and max 8
consecutive read/edit probes without progress fail the turn fast;
planner declares a tool budget per delegation (single-digit reads/edits/
bash), worker batches reads via `read_files`, validates once with the
minimal sufficient check, and writes a lesson instead of looping.

LESSON-002

Date: 2026-09-17
Symptom: `ai update` รายงาน "blue-green update complete" แต่ bot ดับ — ai.pid ชี้ standby ที่ไม่เคยไป live (staged binary ค้างใน probation, ไม่มี live intake, ไม่มี gateway ready) ต้อง kill + `ai start` เอง
Cause: health gates ค้น log marker แบบไม่ผูก pid — marker "live intake enabled" ของ handover ก่อนหน้ายังค้างใน log tail ทำให้ waitForGreenLive ผ่านทันทีจากหลักฐานเก่า (ของ blue ที่กำลังจะถูกหยุด) แล้ว updater ล้าง phase ก่อน green จะเห็น cutover
Fix: CHANGE-057 — ทุก log-marker gate ต้อง correlate `marker + pid=<greenPID>` (logTailContainsPidMarker); เทส stale-marker โดยเฉพาะ; กฎ: หลักฐาน readiness ใด ๆ ที่ reuse ข้าม run ต้องผูก identity ของ run นั้นเสมอ
