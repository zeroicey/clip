-- ============================================================================
--  clip_shares queries — 只读分享链接
--
--  写判定（创建/撤销分享）仍然**一次语句完成**，与 clips 的规则同一条：
--  调用方在 Go 里校验主密码通过后，把「刚刚校验过的那个哈希」当 @proof_hash
--  传进来，语句里用 EXISTS 把「这个 clip 还是那个主人」作为插入/删除条件。
--  中间密码被改过 → 0 行 → 404。绝不 read-then-write。
--
--  读侧（GetLiveShare）**不碰 clips.password_hash**：分享口令独立于主密码
--  （裁决分歧二 → a）。主密码的闸门在 API 层对 /s/... 根本不适用 —— 它只适用于
--  主人自己的 /<后缀> 与 /api/clips/<后缀>。
-- ============================================================================

-- name: GetLiveShare :one
-- 分享自己没到期就还活着。clip 那一侧的过期由 GetLiveClip 过滤，两者独立：
-- 一条分享的 TTL 通常短于（或等于）clip 的 TTL，但语义上它们各管各的。
SELECT token, suffix, password_hash, code_hash, expires_at, created_at
FROM clip_shares
WHERE token = $1
  AND (expires_at = 0 OR expires_at > @now_ms);

-- name: InsertShare :execrows
-- 插入一条分享，条件是这个 clip 仍然存在、没过期、并且调用方拿得出主密码
-- （公开 clip 走 password_hash IS NULL 那一支）。
--
-- 为什么是 INSERT ... SELECT ... WHERE EXISTS 而不是先查再插：
-- 中间那一瞬间主密码可能被改过。把校验过的哈希当条件带进同一条语句，改过就是
-- 0 行 —— 调用方得到 404，什么也没插入。
--
-- 外键（suffix → clips）是第二道闸：即使 EXISTS 通过，clip 恰好在同一瞬间被删掉
-- 也会插入失败，而不是留下一条指向不存在内容的分享。
INSERT INTO clip_shares (token, suffix, password_hash, code_hash, expires_at, created_at)
SELECT @token, @suffix, @password_hash, @code_hash, @expires_at, @now_ms
WHERE EXISTS (
  SELECT 1 FROM clips c
  WHERE c.suffix = @suffix
    AND (c.password_hash IS NULL OR c.password_hash = @proof_hash)
    AND (c.expires_at = 0 OR c.expires_at > @now_ms)
);

-- name: ListSharesForSuffix :many
-- 主人页面上的分享列表。读，所以不带 proof：主密码的校验由调用方（clipAccess）
-- 在这次查询之前完成，与 handleGetClip 的「先读再判」同形。列表里没有明文口令，
-- 只有哈希是否存在（has_password / has_code 由调用方转成布尔）。
SELECT token, suffix, password_hash, code_hash, expires_at, created_at
FROM clip_shares
WHERE suffix = $1
ORDER BY created_at DESC, token;

-- name: DeleteShare :execrows
-- 撤销。token 与 suffix 一起作为条件：一个 token 只属于一个后缀，所以拿别人的
-- token 来撤不了任何东西。proof 同样进语句（见本文件顶部）。
DELETE FROM clip_shares
WHERE clip_shares.token = @token
  AND clip_shares.suffix = @suffix
  AND EXISTS (
    SELECT 1 FROM clips c
    WHERE c.suffix = @suffix
      AND (c.password_hash IS NULL OR c.password_hash = @proof_hash)
  );

-- name: DeleteSharesForSuffix :exec
-- 清掉某个 clip 的全部分享。接管（ClaimSuffix 的 DO UPDATE 分支）要用：
-- 那是一次 UPDATE，ON DELETE CASCADE **不会**触发，所以旧主人留下的分享会
-- 继续指向这个后缀 —— 而内容已经换人了。这与 clip_assets 是同一个坑
-- （见 store.go 的 Create），必须一起清。
DELETE FROM clip_shares WHERE clip_shares.suffix = @suffix;

-- name: DeleteExpiredShares :execrows
DELETE FROM clip_shares
WHERE expires_at <> 0 AND expires_at <= @now_ms;
