-- ============================================================================
--  Relay / clip — the network clipboard
--
--  One row per addressable suffix (`https://clip.hcyj.xyz/<suffix>`).
--
--  Time convention: every *_at/_expires_at column is UNIX epoch MILLISECONDS
--  (BIGINT), matching the rest of Relay. `expires_at = 0` means "never expires"
--  — the same convention as teleport's share links, deliberately: two modules
--  that disagreed about what 0 means would eventually be confused for one
--  another.
--
--  Ownership: there is no user table. A clip is writable by whoever holds the
--  owner token minted when it was created, and the server only ever stores
--  sha256(token). Reading is always anonymous — that is the entire point of the
--  module (move text between two machines without an account on either).
-- ============================================================================

CREATE TABLE IF NOT EXISTS clips (
  suffix      TEXT   PRIMARY KEY,
  -- rev increases on every successful write. Clients long-poll on it, so it must
  -- change on *every* mutation — including one that writes identical bytes, or a
  -- waiting reader would never learn that anything happened.
  rev         BIGINT NOT NULL DEFAULT 1,
  body        TEXT   NOT NULL DEFAULT '',
  body_bytes  BIGINT NOT NULL DEFAULT 0,
  -- sha256 of the owner token. BYTEA rather than TEXT so a comparison cannot be
  -- confused with a display string.
  owner_hash  BYTEA  NOT NULL,
  expires_at  BIGINT NOT NULL DEFAULT 0,
  created_at  BIGINT NOT NULL,
  updated_at  BIGINT NOT NULL
);

-- The janitor's sweep. Partial: the overwhelming majority of rows are expected
-- to expire, so indexing only the expiring ones keeps the index small and means
-- it stays warm for the one query that uses it.
CREATE INDEX IF NOT EXISTS clips_expires_at_idx
  ON clips (expires_at)
  WHERE expires_at <> 0;

-- Images attached to a clip, addressed by position (0..3). Content lives here
-- rather than in a filesystem or an object store so that a clip is one
-- transactionally-consistent thing: there is no window where a clip row points
-- at an image that has not been written yet, and no second store to back up.
-- Postgres TOASTs these values out of line, so they do not bloat scans of the
-- metadata columns.
CREATE TABLE IF NOT EXISTS clip_assets (
  suffix     TEXT     NOT NULL REFERENCES clips (suffix) ON DELETE CASCADE,
  position   SMALLINT NOT NULL CHECK (position >= 0),
  mime       TEXT     NOT NULL,
  byte_size  BIGINT   NOT NULL CHECK (byte_size > 0),
  content    BYTEA    NOT NULL,
  created_at BIGINT   NOT NULL,
  PRIMARY KEY (suffix, position)
);
