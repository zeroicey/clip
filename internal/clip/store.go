package clip

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zeroicey/clip/internal/clip/clipdb"
)

// migrationFS embeds the SQL migrations, so the deployed binary needs no
// separate migration directory and cannot run a migration that does not match
// the code it is running.
//
//go:embed migrations/*.sql
var migrationFS embed.FS

// Sentinel errors. Callers map them onto HTTP status codes; keeping the mapping
// in one place (the API layer) is what stops two handlers from disagreeing about
// what "not found" means.
var (
	// ErrNotFound covers "no such suffix", "not the owner" and "expired" alike.
	// They are deliberately indistinguishable: a public, guessable namespace
	// must not answer "this exists but is not yours".
	ErrNotFound = errors.New("clip not found")
	// ErrTaken means the suffix is live and owned by someone else.
	ErrTaken = errors.New("suffix already in use")
	// ErrQuota means the service's global ceiling is reached.
	ErrQuota = errors.New("clip storage quota reached")
)

// Asset is one image attached to a clip.
type Asset struct {
	Position int16
	Mime     string
	Bytes    []byte
}

// BodyBytes is the stored size of the asset.
func (a Asset) BodyBytes() int64 { return int64(len(a.Bytes)) }

// AssetMeta is an asset without its content, for rendering the page.
type AssetMeta struct {
	Position int16
	Mime     string
	ByteSize int64
}

// Clip is a clip as the API exposes it — never including the owner hash.
type Clip struct {
	Suffix    string
	Rev       int64
	Body      string
	ExpiresAt int64 // epoch ms; 0 = never
	CreatedAt int64
	UpdatedAt int64
	Assets    []AssetMeta
}

// Stats are the totals the quota is enforced against.
type Stats struct {
	Clips      int64
	BodyBytes  int64
	AssetBytes int64
	// TotalBytes is the sum the quota actually compares against.
	TotalBytes int64
}

// Store owns the connection pool and every SQL statement.
type Store struct {
	pool *pgxpool.Pool
	q    *clipdb.Queries
	cfg  *Config
}

// quotaLockKey is the advisory-lock key that serialises quota checks.
//
// The check is "read the totals, compare, then insert", which two concurrent
// requests can both pass. A transaction-level advisory lock makes the pair
// atomic; it is per-database, so it cannot collide with the other applications
// sharing this Postgres instance. The value is arbitrary but must stay stable.
const quotaLockKey int64 = 0x5E1AC11F00000001

// Open connects to Postgres and applies pending migrations.
func Open(ctx context.Context, cfg *Config) (*Store, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	poolCfg.MaxConns = cfg.DBMaxConns
	// Recycle idle connections: this service is often idle for hours, and a
	// connection held open across a database restart is a connection that fails
	// on its next use. Short idle timeouts turn that into a silent reconnect.
	poolCfg.MaxConnIdleTime = 5 * time.Minute
	poolCfg.MaxConnLifetime = time.Hour

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	if err := migrate(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	return &Store{pool: pool, q: clipdb.New(pool), cfg: cfg}, nil
}

// Pool exposes the pool for tests and for the health check.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// Close releases the pool.
func (s *Store) Close() { s.pool.Close() }

// migrate applies every embedded migration not yet recorded, in filename order,
// each inside its own transaction.
//
// Migrations are the schema's single source of truth: sqlc reads this same
// directory, so a query cannot be generated against a schema the server will
// not have. Filenames must sort in application order (0001_, 0002_, ...).
func migrate(ctx context.Context, pool *pgxpool.Pool) error {
	if _, err := pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    TEXT   PRIMARY KEY,
			applied_at BIGINT NOT NULL
		)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	names, err := migrationNames()
	if err != nil {
		return err
	}

	for _, name := range names {
		var exists bool
		if err := pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`, name,
		).Scan(&exists); err != nil {
			return fmt.Errorf("read schema_migrations: %w", err)
		}
		if exists {
			continue
		}

		body, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}

		tx, err := pool.Begin(ctx)
		if err != nil {
			return fmt.Errorf("begin migration %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx, string(body)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("apply migration %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO schema_migrations (version, applied_at) VALUES ($1, $2)`,
			name, time.Now().UnixMilli(),
		); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("record migration %s: %w", name, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit migration %s: %w", name, err)
		}
	}
	return nil
}

// migrationNames lists the embedded migrations in application order.
func migrationNames() ([]string, error) {
	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		return nil, fmt.Errorf("read migrations dir: %w", err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || len(e.Name()) < 5 || e.Name()[len(e.Name())-4:] != ".sql" {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names, nil
}

// nowMS is the clock. A field rather than a bare time.Now so tests can pin it
// without sleeping — a sleep-based expiry test both slows the suite down and
// stops testing the thing it claims to (see the pitfalls about sleeps removing
// the condition under test).
func (s *Store) nowMS() int64 { return time.Now().UnixMilli() }

// Get returns a live clip with its asset metadata.
func (s *Store) Get(ctx context.Context, suffix string) (*Clip, error) {
	row, err := s.q.GetLiveClip(ctx, clipdb.GetLiveClipParams{
		Suffix: suffix,
		NowMs:  s.nowMS(),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get clip: %w", err)
	}
	return s.withAssets(ctx, row)
}

func (s *Store) withAssets(ctx context.Context, row clipdb.Clip) (*Clip, error) {
	metas, err := s.q.ListAssetMeta(ctx, row.Suffix)
	if err != nil {
		return nil, fmt.Errorf("list assets: %w", err)
	}
	clip := &Clip{
		Suffix:    row.Suffix,
		Rev:       row.Rev,
		Body:      row.Body,
		ExpiresAt: row.ExpiresAt,
		CreatedAt: row.CreatedAt,
		UpdatedAt: row.UpdatedAt,
	}
	for _, m := range metas {
		clip.Assets = append(clip.Assets, AssetMeta{Position: m.Position, Mime: m.Mime, ByteSize: m.ByteSize})
	}
	return clip, nil
}

// CreateResult reports what a write did.
type CreateResult struct {
	Rev       int64
	ExpiresAt int64
	CreatedAt int64
	UpdatedAt int64
}

// Create claims a suffix and writes the first body.
//
// The quota check and the claim run in one transaction guarded by an advisory
// lock, so two simultaneous requests cannot both observe "under quota" and both
// insert. Without that, the ceiling is a suggestion: the exact overshoot is
// bounded by concurrency, which is precisely the case an abusive client
// controls.
func (s *Store) Create(ctx context.Context, suffix string, body string, ownerHash []byte, expiresAt int64) (*CreateResult, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin create: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, quotaLockKey); err != nil {
		return nil, fmt.Errorf("lock quota: %w", err)
	}
	q := s.q.WithTx(tx)

	addBytes := int64(len(body))
	if err := checkQuota(ctx, q, s.cfg, addBytes); err != nil {
		return nil, err
	}

	now := s.nowMS()
	row, err := q.ClaimSuffix(ctx, clipdb.ClaimSuffixParams{
		Suffix:    suffix,
		Body:      body,
		BodyBytes: int64(len(body)),
		OwnerHash: ownerHash,
		ExpiresAt: expiresAt,
		NowMs:     now,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// A live row already holds this suffix. ClaimSuffix's DO UPDATE arm
			// only fires for an expired row, so "no rows" means "taken".
			return nil, ErrTaken
		}
		return nil, fmt.Errorf("claim suffix: %w", err)
	}

	// A takeover leaves the previous owner's images behind, because it is an
	// UPDATE rather than a DELETE and the ON DELETE CASCADE never fires. Clear
	// them here; on a fresh insert this deletes nothing.
	if err := q.DeleteAssets(ctx, suffix); err != nil {
		return nil, fmt.Errorf("clear assets on claim: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit create: %w", err)
	}
	return &CreateResult{Rev: row.Rev, ExpiresAt: row.ExpiresAt, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}, nil
}

// Update replaces the body of a clip the caller owns.
//
// expiresAt < 0 means "leave the current expiry alone" — expressed in SQL so the
// decision does not need a read-then-write round trip.
func (s *Store) Update(ctx context.Context, suffix string, ownerHash []byte, body string, expiresAt int64) (*CreateResult, error) {
	row, err := s.q.UpdateClip(ctx, clipdb.UpdateClipParams{
		Suffix:    suffix,
		Body:      body,
		BodyBytes: int64(len(body)),
		OwnerHash: ownerHash,
		ExpiresAt: expiresAt,
		NowMs:     s.nowMS(),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("update clip: %w", err)
	}
	return &CreateResult{Rev: row.Rev, ExpiresAt: row.ExpiresAt, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}, nil
}

// Delete removes a clip the caller owns. It reports whether anything was
// deleted; a false is answered as 404 by the API, not as 403.
func (s *Store) Delete(ctx context.Context, suffix string, ownerHash []byte) (bool, error) {
	n, err := s.q.DeleteClip(ctx, clipdb.DeleteClipParams{Suffix: suffix, OwnerHash: ownerHash})
	if err != nil {
		return false, fmt.Errorf("delete clip: %w", err)
	}
	return n > 0, nil
}

// SetAssets replaces the whole image set of a clip the caller owns, and bumps
// the revision so readers waiting on the old revision wake up.
//
// Replacement is wholesale rather than per-image: positions are the address, so
// a partial update would need a second addressing scheme (ids) that a client
// would then have to track. Four images is well inside the size where replacing
// everything is cheapest.
func (s *Store) SetAssets(ctx context.Context, suffix string, ownerHash []byte, assets []Asset) (*CreateResult, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin set assets: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)

	now := s.nowMS()
	// TouchClipRev is the ownership gate AND the wake-up signal, in one
	// statement: if it matches no row, the caller is not the owner (or the clip
	// is gone) and nothing is written.
	n, err := q.TouchClipRev(ctx, clipdb.TouchClipRevParams{
		Suffix:    suffix,
		OwnerHash: ownerHash,
		NowMs:     now,
	})
	if err != nil {
		return nil, fmt.Errorf("touch clip: %w", err)
	}
	if n == 0 {
		return nil, ErrNotFound
	}

	if err := q.DeleteAssets(ctx, suffix); err != nil {
		return nil, fmt.Errorf("clear assets: %w", err)
	}
	for _, a := range assets {
		if err := q.InsertAsset(ctx, clipdb.InsertAssetParams{
			Suffix:   suffix,
			Position: a.Position,
			Mime:     a.Mime,
			ByteSize: a.BodyBytes(),
			Content:  a.Bytes,
			NowMs:    now,
		}); err != nil {
			return nil, fmt.Errorf("insert asset %d: %w", a.Position, err)
		}
	}

	// Re-read so the caller learns the revision it just created.
	row, err := q.GetLiveClip(ctx, clipdb.GetLiveClipParams{Suffix: suffix, NowMs: now})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("re-read clip: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit set assets: %w", err)
	}
	return &CreateResult{Rev: row.Rev, ExpiresAt: row.ExpiresAt, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}, nil
}

// Asset returns one image's bytes and media type.
func (s *Store) Asset(ctx context.Context, suffix string, position int16) (string, []byte, error) {
	row, err := s.q.GetAsset(ctx, clipdb.GetAssetParams{Suffix: suffix, Position: position})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil, ErrNotFound
		}
		return "", nil, fmt.Errorf("get asset: %w", err)
	}
	return row.Mime, row.Content, nil
}

// Stats returns the totals used by the quota check and by /api/health.
func (s *Store) Stats(ctx context.Context) (Stats, error) {
	row, err := s.q.StatsTotals(ctx)
	if err != nil {
		return Stats{}, fmt.Errorf("stats: %w", err)
	}
	return Stats{
		Clips:      row.ClipCount,
		BodyBytes:  row.BodyBytes,
		AssetBytes: row.AssetBytes,
		TotalBytes: row.BodyBytes + row.AssetBytes,
	}, nil
}

// Sweep deletes clips whose TTL has passed and reports how many rows went.
//
// It is a backstop, not the mechanism: every read filters on expiry, so a clip
// is unreachable the moment it expires whether or not this has run. Deletion is
// about reclaiming space and keeping the suffix addressable again.
func (s *Store) Sweep(ctx context.Context) (int64, error) {
	n, err := s.q.DeleteExpiredClips(ctx, s.nowMS())
	if err != nil {
		return 0, fmt.Errorf("sweep expired clips: %w", err)
	}
	return n, nil
}

// checkQuota fails when one more write would push the service past its ceiling.
//
// It runs inside the caller's transaction, after the advisory lock, so the
// totals it reads cannot change underneath it.
func checkQuota(ctx context.Context, q *clipdb.Queries, cfg *Config, addBytes int64) error {
	totals, err := q.StatsTotals(ctx)
	if err != nil {
		return fmt.Errorf("quota stats: %w", err)
	}
	if totals.ClipCount >= cfg.MaxTotalClips {
		return fmt.Errorf("%w: clip count %d >= %d", ErrQuota, totals.ClipCount, cfg.MaxTotalClips)
	}
	if totals.BodyBytes+totals.AssetBytes+addBytes > cfg.MaxTotalBytes {
		return fmt.Errorf("%w: bytes %d + %d > %d",
			ErrQuota, totals.BodyBytes+totals.AssetBytes, addBytes, cfg.MaxTotalBytes)
	}
	return nil
}
