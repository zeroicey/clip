-- ============================================================================
--  clip queries
--
--  Ownership is enforced IN the WHERE clause, never by a read-then-write in Go.
--  A conditional UPDATE that matches nothing is indistinguishable from "no such
--  suffix" and returns no rows, which is exactly the answer a non-owner should
--  get — and it cannot race, because the check and the write are one statement.
--  (This is the pattern teleport's store already uses; see the pitfall note
--  about "code wrote it but the effect never happened".)
-- ============================================================================

-- name: GetClip :one
SELECT suffix, rev, body, body_bytes, owner_hash, expires_at, created_at, updated_at
FROM clips
WHERE suffix = $1;

-- name: GetLiveClip :one
-- A clip whose TTL has passed is gone as far as any reader is concerned, even
-- before the janitor deletes the row. Filtering here (rather than trusting the
-- janitor to have run) means an expired clip is never served.
SELECT suffix, rev, body, body_bytes, owner_hash, expires_at, created_at, updated_at
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
INSERT INTO clips (suffix, rev, body, body_bytes, owner_hash, expires_at, created_at, updated_at)
VALUES (@suffix, 1, @body, @body_bytes, @owner_hash, @expires_at, @now_ms, @now_ms)
ON CONFLICT (suffix) DO UPDATE SET
  rev         = 1,
  body        = EXCLUDED.body,
  body_bytes  = EXCLUDED.body_bytes,
  owner_hash  = EXCLUDED.owner_hash,
  expires_at  = EXCLUDED.expires_at,
  created_at  = EXCLUDED.created_at,
  updated_at  = EXCLUDED.updated_at
WHERE clips.expires_at <> 0 AND clips.expires_at <= @now_ms
RETURNING rev, expires_at, created_at, updated_at;

-- name: UpdateClip :one
-- Owner-checked, live-only update. No rows means: no such suffix, or not the
-- owner, or expired — all three are answered with the same 404 so the endpoint
-- is not an existence oracle.
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
  AND owner_hash = @owner_hash
  AND (expires_at = 0 OR expires_at > @now_ms)
RETURNING rev, expires_at, created_at, updated_at;

-- name: DeleteClip :execrows
DELETE FROM clips
WHERE suffix = @suffix
  AND owner_hash = @owner_hash;

-- name: TouchClipRev :execrows
-- Bump rev without touching content. Used after an asset-only change, so a
-- waiting reader is woken and re-fetches.
UPDATE clips
SET rev = rev + 1, updated_at = @now_ms
WHERE suffix = @suffix AND owner_hash = @owner_hash;

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
