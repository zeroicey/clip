-- ============================================================================
--  0002 — 可选访问密码取代 owner token
--
--  归属模型在 2026-10-16 改判（.ai/decisions/2026-10-16-clip-open-edit-and-ownership.md）：
--  没有用户表，也没有「创建者」这个概念。
--
--    password_hash IS NULL       → 公开 clip：任何人都能读、能写（含给它设密码）
--    password_hash IS NOT NULL   → 只有知道密码的人能读、能写、能换密码
--
--  「谁先设密码谁就是主人」，密码是**唯一**凭据，所以旧的 owner_hash 被删掉。
--
--  为什么是 TEXT 而不是 BYTEA（0001 里 owner_hash 用的是 BYTEA）：
--  这里存的是 argon2id 的 PHC 字符串（`$argon2id$v=19$m=…,t=…,p=…$salt$hash`），
--  它自带算法、参数与 salt，校验时要原样解析回参数。它不是一个用来比较的裸哈希，
--  而是一段**自描述的校验配方**；把它塞进 BYTEA 只会让「它是字符串」这件事变得含糊。
--  比较仍然在 Go 里做常量时间比较（argon2 自带），不做 SQL 等值比较 —— 见 store.go。
--
--  TIME 约定不变：expires_at 是 epoch 毫秒，0 = 永不过期。
-- ============================================================================

ALTER TABLE clips ADD COLUMN IF NOT EXISTS password_hash TEXT;

-- 存量行一律 NULL = 公开，这正是「无密码的 clip 本来就谁都能写」的意思，
-- 所以这次迁移对已有内容是语义中性的（写权限放宽，读权限不变）。
ALTER TABLE clips DROP COLUMN IF EXISTS owner_hash;
