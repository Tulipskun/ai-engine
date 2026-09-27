# การตัดสินใจด้านสถาปัตยกรรม

DEC-001 — Requirement ต้องอยู่ใน repository ของโปรเจค ไม่ใช่ใน AI memory

เหตุผล: specification ต้องพร้อมใช้งานสำหรับ agent, contributor, branch และ clone ของโปรเจคในอนาคตทุกตัว

DEC-002 — Project requirements แยกเป็นเป้าหมายผลิตภัณฑ์, ข้อกำหนดด้านการทำงาน, ข้อจำกัด, การตัดสินใจ และประวัติการเปลี่ยนแปลง

เหตุผล: แยกเจตนาที่คงที่ออกจาก implementation constraints และประวัติ เพื่อให้ agent สามารถวิเคราะห์ผลกระทบได้โดยไม่ต้องเขียน specification ทั้งหมดใหม่

DEC-003 — หาก user request ขัดแย้งกับ requirement เดิม ต้องจัดการเป็น requirement change ก่อน implementation

เหตุผล: การ implement พฤติกรรมที่ขัดแย้งโดยไม่บันทึกจะทำให้ specification ของโปรเจคกับโค้ดเกิด drift โดยไม่มีเอกสารกำกับ

DEC-004 — Software project ใหม่ที่ AI สร้างต้องมี `requirements/` อยู่ใน repository ของโปรเจคก่อนเริ่ม implementation ในส่วนสำคัญ

เหตุผล: repository ของโปรเจคต้องมี specification ของตัวเองตั้งแต่เริ่ม implementation

DEC-005 — Main Agent เป็น senior ที่อ่านโค้ด/บริบทเองผ่าน read-only tools และสั่ง worker (junior) ด้วย delegation contract ที่มี evidence กำกับ; `ai update` เป็น blue-green flow เดียวที่ binary ตัวเดียวเป็นเจ้าของ end-to-end โดยไม่มี supervisor ภายนอก (CHANGE-054, CHANGE-055)
- D-011 (2026-09-24) — Single-surface daemon: mobile-over-tunnel only. CLI,
  Discord and self-update are removed (CHANGE-059) because AIxodia is the only
  client, the daemon is stateless (REQ-046) so it needs no handoff machinery,
  and every removed surface was untested dead weight. The cost is loss of a
  fallback operator channel: a phone becomes the only way to talk to the
  harness, so `GET /api/node` + the tunnel URL must stay reliable, and config
  changes go through D1 rather than a local command.

- D-012 (2026-09-26) — Mobile scope and display integrity: a chat may pin an
  explicit provider/model route, while a blank session route follows the global
  agent defaults; unpinning is an explicit `clear_model` operation that also
  resets the live cached session, not a silent empty PATCH. Provider keys are
  write-only and can only be added, replaced wholesale, or removed by position
  after confirmation; existing key values can never be edited in place because
  they are never returned. Live display may show estimates marked `≈`, stored
  answers must carry provider-reported input/output/cache and daemon-measured
  duration, tool rows must carry name/status/args and duration, and raw
  reasoning is only a transient thinking indicator: it is never stored or
  transcribed.
