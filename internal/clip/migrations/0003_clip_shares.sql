-- ============================================================================
--  0003 — 只读分享链接（/s/<token>）
--
--  裁决：.ai/decisions/2026-10-16-clip-share-links.md（✅ ACCEPTED）
--
--  一条 share 一行；删行 = 撤销。分享**不复制内容**，只指向同一份 clip，所以
--  主人改了正文，分享方刷新即见新内容 —— 没有第二份副本要同步，也没有「分享出去
--  的那一版」这种会悄悄过期的概念。
--
--    password_hash IS NULL AND code_hash IS NULL  → 拿到链接就能看（只读）
--    password_hash IS NOT NULL                    → 主人自设的分享密码
--    code_hash     IS NOT NULL                    → 服务端生成的 6 位取件码（短 TTL）
--
--  两者二选一：一条分享不会同时有密码和取件码，否则「用户输入的是哪一个」这个
--  问题就没有答案，校验分支也会开始互相掩盖。
--
--  分享口令**独立于主密码**（分歧二 → a）：知道分享口令就能看，不需要主密码。
--  这正是这个功能存在的理由 —— 「不要把主密码给他」。所以读 /s/<token> 时**不**
--  套用 clip 自己的密码闸门。
--
--  口令同样只存 argon2id 的 PHC 字符串（与 clips.password_hash 同一套）。
--  明文永不入库、永不回显、永不进日志；取件码也只回显一次（创建响应），
--  之后只剩哈希。
--
--  TIME 约定不变：expires_at 是 epoch 毫秒，0 = 永不过期。
--  on delete cascade：clip 过期被 sweep 掉或被人删掉时，分享一起消失，
--  不会留下指向不存在内容的悬空链接。
-- ============================================================================

CREATE TABLE IF NOT EXISTS clip_shares (
  -- 分享链接里的那一段。服务端生成的高熵 base64url（16 字节 → 22 字符），
  -- **不是**用户选的后缀：分享链接要能随手发出去，而用户自己的名字留在主入口。
  -- 因此它不走 ValidateSuffix（那是给用户输入用的），只走自己的格式校验。
  token         TEXT   PRIMARY KEY,
  suffix        TEXT   NOT NULL REFERENCES clips (suffix) ON DELETE CASCADE,
  password_hash TEXT,
  code_hash     TEXT,
  expires_at    BIGINT NOT NULL,
  created_at    BIGINT NOT NULL
);

-- 列出某个 clip 的分享（主人页面的「分享」区域）和撤销都要走这个索引。
-- 它同时也是 cascade 删除的支撑：删 clip 时 Postgres 要按 suffix 找到所有分享行，
-- 没有索引就是一次全表扫描。
CREATE INDEX IF NOT EXISTS clip_shares_suffix_idx ON clip_shares (suffix);

-- 清扫过期分享。与 clips_expires_at_idx 同样做成 partial：大多数行预期都会过期，
-- 只索引会过期的那些，索引小、且对唯一使用它的那条语句保持热。
CREATE INDEX IF NOT EXISTS clip_shares_expires_at_idx
  ON clip_shares (expires_at)
  WHERE expires_at <> 0;
