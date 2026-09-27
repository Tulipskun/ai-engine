# ข้อจำกัด

CON-001 — Runtime configuration ต้องเก็บใน `config/*.json`; ห้ามเพิ่ม configuration แบบ `.env`

CON-002 — Session database ต้องแยกจากกัน: หนึ่ง session ต้อง map ไปยังหนึ่ง database ภายใต้ `data/sessions/`

CON-003 — ห้ามเพิ่มการเก็บ raw provider request/response ที่ทำให้ session data โตโดยไม่จำเป็น ต้อง persist เฉพาะ structured state ที่จำเป็นสำหรับ continuation, replay และ accounting

CON-004 — Agent ต้องไม่ผูกกับ provider รายใดรายหนึ่งและต้องไม่ผูกกับ transport

CON-005 — Provider adapter ห้ามแก้ไข shared adapter configuration เมื่อใช้ settings เฉพาะของ session

CON-006 — **ยกเลิก (CHANGE-087)**: browser automation ถูกลบออกทั้งหมด จึงไม่มีข้อกำหนดเรื่อง Go CDP implementation หรือการห้าม Playwright/Node.js browser worker แล้ว

CON-007 — Requirement files ใน repository เป็นแหล่งอ้างอิงหลักของโปรเจคนั้น Chat memory เป็นเพียง context และใช้แทน project specification ไม่ได้

CON-008 — ห้ามลดหรือเอา requirement ที่มีอยู่ออกอย่างเงียบ ๆ เพื่อให้ implementation ใหม่ทำได้ง่ายขึ้น หากมีการเปลี่ยนโดยตั้งใจต้องบันทึกเป็น specification change

CON-009 — หลีกเลี่ยงการ refactor ที่ไม่เกี่ยวข้องขณะ implement requirement change

CON-010 — ห้ามนำเส้นทาง model-call แบบ streaming กลับมาใช้; `Generate` เป็นเส้นทาง model call เพียงเส้นทางเดียว

CON-011 — **ยกเลิก (CHANGE-087)**: attachment file store ถูกลบทั้งแพ็กเกจ (`runtime/filestore`, `config/attachment.json`, `config:attachment`) CON-002/CON-003 ยังคงบังคับกับ session data และ CON-001 ยังคงบังคับว่า path/limits กำหนดใน `config/*.json` เท่านั้น

CON-012 — Cloudflare D1 เป็น authoritative copy ของ runtime state ได้ แต่ local materialization ต้องคงรูปแบบเดิม: config อ่าน/เขียนเป็น `config/*.json` และหนึ่ง session ยัง map ไปหนึ่ง SQLite file ใต้ `data/sessions/` (คง CON-001, CON-002) — daemon ต้องสามารถรันได้จาก local copy ที่ถูกลบทิ้งทั้งหมด และ D1 ต้องเป็นที่ที่ sync ไป/กลับ ไม่ใช่ที่ที่ core อ่านข้ามไปเรียกเอง; ห้ามสร้าง credential ใหม่เพื่อการ sync (ไม่มี node token, ไม่มี encryption key, ไม่มี per-device token) — Cloudflare API token ที่มือถือส่งมาใน header คือ credential เดียวของระบบ และต้องไม่ถูกเขียนลง config/disk/log; daemon ห้ามตั้งหรือสร้าง secret ของตัวเอง (รวมถึง Worker secret) และไม่ต้องมี Worker เพื่อถึง D1; ห้ามเกินขนาด value ของ D1 (ข้าม + log แทนที่จะ truncate state)

CON-013 — ห้ามเพิ่ม transport/คำสั่งที่ถูกถอดออกตาม CHANGE-059 กลับมาโดยไม่มี specification change ใหม่: gateway ของ daemon ต้องเป็น mobile-over-tunnel ทางเดียว (ห้าม bind ที่พอร์ตสาธารณะเพื่อเลี่ยง tunnel, ห้ามมี Discord bot/CLI ในโปรเจกต์, ห้ามมี self-update ที่เขียน state/pid/handoff) และ secret ของระบบยังมีได้ตัวเดียวคือ D1 access token ตาม CON-012
