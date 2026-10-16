-- ============================================================================
--  clip queries
--
--  归属：没有用户表，也没有「创建者」。`password_hash IS NULL` = 公开 clip，
--  任何人都能写；非 NULL = 只有知道密码的人能写。密码**不是**在 SQL 里比较的
--  （argon2 需要明文 + salt，只能在 Go 里算），但**判定仍然发生在写语句里**：
--
--      调用方在 Go 里校验密码通过后，把「刚刚校验过的那个哈希」作为
--      @proof_hash 传进来，SQL 用
--          AND (password_hash IS NULL OR password_hash = @proof_hash)
--      作为写条件。
--
--  为什么不是 read-then-write：中间那一瞬间密码可能被改过。把校验过的哈希
--  当条件带进语句，改过就是 0 行 —— 调用方得到 404，什么也没写。这与 0001 里
--  「所有权判定写在 WHERE 里、一次语句完成」的初衷是同一条规则，只是判定值
--  在 Go 里算出来而已。
--
--  公开 clip（password_hash IS NULL）走 `IS NULL` 那一支，所以不带任何凭据的
--  写入是允许的 —— 这就是「无密码谁都能改」的实现，不是漏判。
-- ============================================================================

-- name: GetClip :one
SELECT suffix, rev, body, body_bytes, password_hash, expires_at, created_at, updated_at
FROM clips
WHERE suffix = $1;

-- name: GetLiveClip :one
-- A clip whose TTL has passed is gone as far as any reader is concerned, even
-- before the janitor deletes the row. Filtering here (rather than trusting the
-- janitor to have run) means an expired clip is never served.
SELECT suffix, rev, body, body_bytes, password_hash, expires_at, created_at, updated_at
FROM clips
WHERE suffix = $1
  AND (expires_at = 0 OR expires_at > @now_ms);

-- name: ClaimSuffix :one
-- Create a brand-new clip, OR take over one whose TTL has already passed but
-- which the janitor has not deleted yet.
--
-- Without the takeover branch an expired row would occupy its suffix forever
-- from the writer's point of view: INSERT ... ON CONFLICT DO NOTHING returns no
-- rows and the caller is told "taken" by a clip nobody can read. The WHERE on
-- the DO UPDATE arm is what makes this atomic — a live row conflicts, matches
-- nothing, and returns no rows.
--
-- 新 clip 的 password_hash 由调用方给（NULL = 公开）。在**创建时**就能带上密码，
-- 是为了避免「先建公开的、再设密码」中间那道缝：那道缝里内容对所有人可读，
-- 而用户以为自己已经设成私有的了。接管一个过期行时同样用新值（EXCLUDED），
-- 旧主人的密码不能顺延到新主人的 clip 上。
INSERT INTO clips (suffix, rev, body, body_bytes, password_hash, expires_at, created_at, updated_at)
VALUES (@suffix, 1, @body, @body_bytes, @password_hash, @expires_at, @now_ms, @now_ms)
ON CONFLICT (suffix) DO UPDATE SET
  rev           = 1,
  body          = EXCLUDED.body,
  body_bytes    = EXCLUDED.body_bytes,
  password_hash = EXCLUDED.password_hash,
  expires_at    = EXCLUDED.expires_at,
  created_at    = EXCLUDED.created_at,
  updated_at    = EXCLUDED.updated_at
WHERE clips.expires_at <> 0 AND clips.expires_at <= @now_ms
RETURNING rev, expires_at, created_at, updated_at;

-- name: UpdateClip :one
-- No rows means: no such suffix, a password that does not match (or was not
-- supplied), or expired — all three are answered with the same 404 so the
-- endpoint is not an existence oracle for guessed suffixes.
--
-- `expires_at` < 0 means "keep the current expiry": expressed here rather than
-- in Go so the decision needs no read-then-write round trip, and so an update
-- that does not mention the TTL cannot silently extend a clip's life.
UPDATE clips
SET body       = @body,
    body_bytes = @body_bytes,
    expires_at = CASE WHEN @expires_at::BIGINT < 0 THEN clips.expires_at ELSE @expires_at::BIGINT END,
    rev        = rev + 1,
    updated_at = @now_ms
WHERE suffix = @suffix
  AND (password_hash IS NULL OR password_hash = @proof_hash)
  AND (expires_at = 0 OR expires_at > @now_ms)
RETURNING rev, expires_at, created_at, updated_at;

-- name: DeleteClip :execrows
DELETE FROM clips
WHERE suffix = @suffix
  AND (password_hash IS NULL OR password_hash = @proof_hash);

-- name: TouchClipRev :execrows
-- Bump rev without touching content. Used after an asset-only change, so a
-- waiting reader is woken and re-fetches.
UPDATE clips
SET rev = rev + 1, updated_at = @now_ms
WHERE suffix = @suffix AND (password_hash IS NULL OR password_hash = @proof_hash);

-- name: SetClipPassword :execrows
-- Set a password on a public clip (this is the "claim": whoever sets one first
-- owns the clip), or replace the password on a protected one.
--
-- `@proof_hash` is the hash the caller already verified in Go, or '' when the
-- clip is expected to be public. An empty string can never equal a real PHC
-- string, so a caller who supplies no password cannot touch a protected clip.
UPDATE clips
SET password_hash = @password_hash,
    updated_at    = @now_ms
WHERE suffix = @suffix
  AND (password_hash IS NULL OR password_hash = @proof_hash);

-- name: ClearClipPassword :execrows
-- Remove the password, making the clip public again. Requires proving the
-- current password (or that there is none).
UPDATE clips
SET password_hash = NULL,
    updated_at    = @now_ms
WHERE suffix = @suffix
  AND (password_hash IS NULL OR password_hash = @proof_hash);

-- name: DeleteExpiredClips :execrows
DELETE FROM clips
WHERE expires_at <> 0 AND expires_at <= @now_ms;

-- name: StatsTotals :one
SELECT
  (SELECT COUNT(*) FROM clips)::BIGINT                            AS clip_count,
  (SELECT COALESCE(SUM(body_bytes), 0) FROM clips)::BIGINT         AS body_bytes,
  (SELECT COALESCE(SUM(byte_size), 0) FROM clip_assets)::BIGINT    AS asset_bytes;

-- name: InsertAsset :exec
INSERT INTO clip_assets (suffix, position, mime, byte_size, content, created_at)
VALUES (@suffix, @position, @mime, @byte_size, @content, @now_ms);

-- name: DeleteAssets :exec
DELETE FROM clip_assets WHERE suffix = @suffix;

-- name: ListAssetMeta :many
SELECT position, mime, byte_size
FROM clip_assets
WHERE suffix = $1
ORDER BY position;

-- name: GetAsset :one
SELECT mime, content, byte_size
FROM clip_assets
WHERE suffix = @suffix AND position = @position;
