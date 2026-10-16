package clip

import (
	"context"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// testDSN returns the DSN for the test database, or skips.
//
// Two deliberate guards:
//
//  1. Without TEST_PG_DSN the Postgres tests skip, so `go test ./...` works on a
//     machine with no database and the gate stays green for contributors.
//  2. The database name must contain "test". These tests TRUNCATE tables, and a
//     stray environment variable must not be able to point them at the real
//     database. A missing database is a visible failure; a wrongly-targeted one
//     is data loss.
func testDSN(t *testing.T) string {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("TEST_PG_DSN"))
	if dsn == "" {
		t.Skip("TEST_PG_DSN not set; skipping Postgres-backed tests")
	}
	name := ""
	if u, err := url.Parse(dsn); err == nil {
		name = strings.TrimPrefix(u.Path, "/")
	}
	if !strings.Contains(name, "test") {
		t.Fatalf("refusing to run: TEST_PG_DSN names database %q, which does not contain \"test\"", name)
	}
	return dsn
}

// testStore opens the store against the test database with the tables emptied,
// so each test starts from a known state.
func testStore(t *testing.T, mutate ...func(*Config)) *Store {
	t.Helper()
	dsn := testDSN(t)

	cfg := &Config{
		PublicBaseURL: "http://test.local",
		DatabaseURL:   dsn,
		DBMaxConns:    4,
		MaxBodyBytes:  256 << 10,
		MaxAssetBytes: 5 << 20,
		MaxAssets:     4,
		MaxTotalClips: 20_000,
		MaxTotalBytes: 1 << 30,
		DefaultTTL:    24 * time.Hour,
		WritePerHour:  30,
		WritesEnabled: true,
	}
	for _, m := range mutate {
		m(cfg)
	}

	ctx := context.Background()
	st, err := Open(ctx, cfg)
	if err != nil {
		t.Fatalf("open test store: %v", err)
	}
	t.Cleanup(st.Close)

	if _, err := st.Pool().Exec(ctx, `TRUNCATE clips CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return st
}

func mustCreate(t *testing.T, st *Store, suffix, body, token string, expiresAt int64) {
	t.Helper()
	res, err := st.Create(context.Background(), suffix, body, hashOwnerToken(token), expiresAt)
	if err != nil {
		t.Fatalf("Create(%q): %v", suffix, err)
	}
	if res.Rev != 1 {
		t.Fatalf("first revision = %d, want 1", res.Rev)
	}
}

func TestCreateAndGetRoundTrip(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	mustCreate(t, st, "ops1", "hello\nworld\n", "tok-1", 0)

	got, err := st.Get(ctx, "ops1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Body != "hello\nworld\n" {
		t.Errorf("body = %q", got.Body)
	}
	if got.ExpiresAt != 0 {
		t.Errorf("expires_at = %d, want 0 (never)", got.ExpiresAt)
	}
	if got.Rev != 1 {
		t.Errorf("rev = %d, want 1", got.Rev)
	}
}

// TestUpdateRequiresTheOwnerToken is the security boundary of the whole module:
// a guessed suffix is readable but not writable. It also asserts the negative
// case directly (the body is unchanged), because "the call returned an error" is
// not the same claim as "nothing was written".
func TestUpdateRequiresTheOwnerToken(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	mustCreate(t, st, "ops1", "the original", "owner-token", 0)

	if _, err := st.Update(ctx, "ops1", hashOwnerToken("wrong-token"), "defaced", -1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update with a wrong token = %v, want ErrNotFound", err)
	}
	got, err := st.Get(ctx, "ops1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Body != "the original" {
		t.Fatalf("body was changed by an unauthorised update: %q", got.Body)
	}
	if got.Rev != 1 {
		t.Fatalf("rev advanced on a refused update: %d", got.Rev)
	}

	if _, err := st.Update(ctx, "ops1", hashOwnerToken("owner-token"), "the fix", -1); err != nil {
		t.Fatalf("update as the owner: %v", err)
	}
	got, _ = st.Get(ctx, "ops1")
	if got.Body != "the fix" || got.Rev != 2 {
		t.Fatalf("after the owner's update: body=%q rev=%d, want %q/2", got.Body, got.Rev, "the fix")
	}
}

// TestCreateOnALiveSuffixIsRefused proves the create path cannot be used as a
// back door to overwrite somebody else's clip.
func TestCreateOnALiveSuffixIsRefused(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	mustCreate(t, st, "ops1", "the original", "owner-token", 0)

	if _, err := st.Create(ctx, "ops1", "hijacked", hashOwnerToken("other"), 0); !errors.Is(err, ErrTaken) {
		t.Fatalf("Create on a live suffix = %v, want ErrTaken", err)
	}
	got, _ := st.Get(ctx, "ops1")
	if got.Body != "the original" {
		t.Fatalf("body was overwritten: %q", got.Body)
	}
	// The original token still works, i.e. the failed takeover did not rotate it.
	if _, err := st.Update(ctx, "ops1", hashOwnerToken("owner-token"), "still mine", -1); err != nil {
		t.Fatalf("the original owner lost write access: %v", err)
	}
}

// TestExpiredSuffixBecomesReusable is why ClaimSuffix has a takeover branch: an
// expired row still occupies its suffix until the janitor runs. Without the
// takeover, a suffix that expired a second ago would be permanently unusable —
// "taken" by a clip nobody can read.
func TestExpiredSuffixBecomesReusable(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	past := time.Now().Add(-time.Hour).UnixMilli()
	mustCreate(t, st, "ops1", "old content", "old-token", past)

	if _, err := st.Get(ctx, "ops1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an expired clip is still readable: %v", err)
	}
	// The old owner cannot update it either: expiry is expiry.
	if _, err := st.Update(ctx, "ops1", hashOwnerToken("old-token"), "zombie", -1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an expired clip accepted an update: %v", err)
	}

	if _, err := st.Create(ctx, "ops1", "new content", hashOwnerToken("new-token"), 0); err != nil {
		t.Fatalf("reusing an expired suffix: %v", err)
	}
	got, err := st.Get(ctx, "ops1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Body != "new content" || got.Rev != 1 {
		t.Fatalf("after takeover: body=%q rev=%d, want %q/1", got.Body, got.Rev, "new content")
	}
	// The previous owner must be locked out.
	if _, err := st.Update(ctx, "ops1", hashOwnerToken("old-token"), "zombie", -1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the old owner still has write access after a takeover: %v", err)
	}
}

// TestTakeoverClearsTheOldOwnersImages is the guard for the ON DELETE CASCADE
// that does NOT fire: a takeover is an UPDATE, so without an explicit delete the
// new owner would inherit the previous owner's images.
func TestTakeoverClearsTheOldOwnersImages(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	past := time.Now().Add(-time.Hour).UnixMilli()
	mustCreate(t, st, "ops1", "old", "old-token", past)

	if _, err := st.SetAssets(ctx, "ops1", hashOwnerToken("old-token"),
		[]Asset{{Position: 0, Mime: "image/png", Bytes: pngBytes()}}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetAssets on an expired clip = %v, want ErrNotFound", err)
	}

	// Write the asset while the clip is still live, then let it expire.
	if _, err := st.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	mustCreate(t, st, "ops2", "live", "tok-live", 0)
	if _, err := st.SetAssets(ctx, "ops2", hashOwnerToken("tok-live"),
		[]Asset{{Position: 0, Mime: "image/png", Bytes: pngBytes()}}); err != nil {
		t.Fatal(err)
	}
	// Expire it by hand, then take the suffix over.
	if _, err := st.Pool().Exec(ctx, `UPDATE clips SET expires_at = $1 WHERE suffix = 'ops2'`, past); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Create(ctx, "ops2", "fresh", hashOwnerToken("tok-new"), 0); err != nil {
		t.Fatalf("takeover: %v", err)
	}
	got, err := st.Get(ctx, "ops2")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Assets) != 0 {
		t.Fatalf("the new owner inherited %d image(s) from the old one", len(got.Assets))
	}
}

func TestQuotaStopsWrites(t *testing.T) {
	st := testStore(t, func(c *Config) { c.MaxTotalClips = 1 })
	ctx := context.Background()
	mustCreate(t, st, "one", "x", "t1", 0)

	if _, err := st.Create(ctx, "two", "y", hashOwnerToken("t2"), 0); !errors.Is(err, ErrQuota) {
		t.Fatalf("Create past the clip quota = %v, want ErrQuota", err)
	}
	// The failed attempt must not have written anything.
	if _, err := st.Get(ctx, "two"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a refused create still wrote a row: %v", err)
	}
}

func TestQuotaStopsWritesByBytes(t *testing.T) {
	st := testStore(t, func(c *Config) { c.MaxTotalBytes = 10 })
	ctx := context.Background()
	mustCreate(t, st, "one", "0123456789", "t1", 0)

	if _, err := st.Create(ctx, "two", "x", hashOwnerToken("t2"), 0); !errors.Is(err, ErrQuota) {
		t.Fatalf("Create past the byte quota = %v, want ErrQuota", err)
	}
}

func TestSweepDeletesOnlyExpired(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	past := time.Now().Add(-time.Hour).UnixMilli()
	future := time.Now().Add(time.Hour).UnixMilli()
	mustCreate(t, st, "gone", "a", "t1", past)
	mustCreate(t, st, "stays", "b", "t2", future)
	mustCreate(t, st, "forever", "c", "t3", 0)

	n, err := st.Sweep(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("Sweep deleted %d rows, want 1", n)
	}
	for _, suffix := range []string{"stays", "forever"} {
		if _, err := st.Get(ctx, suffix); err != nil {
			t.Errorf("Sweep removed a live clip %q: %v", suffix, err)
		}
	}
}

func TestSetAssetsReplacesWholesaleAndBumpsRev(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	mustCreate(t, st, "ops1", "text", "tok", 0)

	res, err := st.SetAssets(ctx, "ops1", hashOwnerToken("tok"), []Asset{
		{Position: 0, Mime: "image/png", Bytes: pngBytes()},
		{Position: 1, Mime: "image/jpeg", Bytes: jpegBytes()},
	})
	if err != nil {
		t.Fatalf("SetAssets: %v", err)
	}
	if res.Rev != 2 {
		t.Fatalf("rev after adding assets = %d, want 2 (readers must be woken)", res.Rev)
	}

	mime, data, err := st.Asset(ctx, "ops1", 0)
	if err != nil {
		t.Fatalf("Asset(0): %v", err)
	}
	if mime != "image/png" || len(data) == 0 {
		t.Fatalf("Asset(0) = %q/%d bytes", mime, len(data))
	}

	// Replacing with one image must leave exactly one, not three or two.
	res, err = st.SetAssets(ctx, "ops1", hashOwnerToken("tok"), []Asset{
		{Position: 0, Mime: "image/webp", Bytes: pngBytes()},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Rev != 3 {
		t.Fatalf("rev after replacing assets = %d, want 3", res.Rev)
	}
	got, _ := st.Get(ctx, "ops1")
	if len(got.Assets) != 1 || got.Assets[0].Mime != "image/webp" {
		t.Fatalf("assets after replacement = %+v, want one webp", got.Assets)
	}
	if _, _, err := st.Asset(ctx, "ops1", 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("position 1 survived the replacement: %v", err)
	}
}

func TestSetAssetsRequiresTheOwnerToken(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	mustCreate(t, st, "ops1", "text", "tok", 0)

	if _, err := st.SetAssets(ctx, "ops1", hashOwnerToken("nope"),
		[]Asset{{Position: 0, Mime: "image/png", Bytes: pngBytes()}}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetAssets with a wrong token = %v, want ErrNotFound", err)
	}
	got, _ := st.Get(ctx, "ops1")
	if len(got.Assets) != 0 {
		t.Fatalf("an unauthorised SetAssets wrote %d asset(s)", len(got.Assets))
	}
	if got.Rev != 1 {
		t.Fatalf("rev moved on a refused SetAssets: %d", got.Rev)
	}
}

// TestRevChangesEvenWhenTheBodyDoesNot: a reader waits on the revision, so a
// write of identical bytes must still advance it, or the other machine would
// never learn that anything happened.
func TestRevChangesEvenWhenTheBodyDoesNot(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	mustCreate(t, st, "ops1", "same", "tok", 0)
	before, _ := st.Get(ctx, "ops1")

	if _, err := st.Update(ctx, "ops1", hashOwnerToken("tok"), "same", -1); err != nil {
		t.Fatal(err)
	}
	after, _ := st.Get(ctx, "ops1")
	if after.Rev <= before.Rev {
		t.Fatalf("rev did not advance on an identical write: %d -> %d", before.Rev, after.Rev)
	}
}

func TestUpdateKeepsExpiryWhenNotMentioned(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	deadline := time.Now().Add(48 * time.Hour).UnixMilli()
	mustCreate(t, st, "ops1", "x", "tok", deadline)

	if _, err := st.Update(ctx, "ops1", hashOwnerToken("tok"), "y", -1); err != nil {
		t.Fatal(err)
	}
	got, _ := st.Get(ctx, "ops1")
	if got.ExpiresAt != deadline {
		t.Fatalf("expiry moved on an update that did not mention it: %d -> %d", deadline, got.ExpiresAt)
	}
	// And an explicit 0 means forever.
	if _, err := st.Update(ctx, "ops1", hashOwnerToken("tok"), "z", 0); err != nil {
		t.Fatal(err)
	}
	got, _ = st.Get(ctx, "ops1")
	if got.ExpiresAt != 0 {
		t.Fatalf("explicit 0 did not mean never: %d", got.ExpiresAt)
	}
}

func TestDeleteRequiresTheOwnerToken(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	mustCreate(t, st, "ops1", "text", "tok", 0)

	if ok, err := st.Delete(ctx, "ops1", hashOwnerToken("nope")); err != nil || ok {
		t.Fatalf("Delete with a wrong token = %v/%v, want false/nil", ok, err)
	}
	if _, err := st.Get(ctx, "ops1"); err != nil {
		t.Fatalf("a refused delete removed the clip: %v", err)
	}
	if ok, err := st.Delete(ctx, "ops1", hashOwnerToken("tok")); err != nil || !ok {
		t.Fatalf("Delete as the owner = %v/%v, want true/nil", ok, err)
	}
	if _, err := st.Get(ctx, "ops1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the clip survived its owner's delete: %v", err)
	}
}

// TestMigrationsAreRecorded proves the migration runner is idempotent and that
// the applied version is written down (so a future destructive migration is
// never replayed).
func TestMigrationsAreRecorded(t *testing.T) {
	st := testStore(t)
	var version string
	if err := st.Pool().QueryRow(context.Background(),
		`SELECT version FROM schema_migrations ORDER BY version LIMIT 1`).Scan(&version); err != nil {
		t.Fatalf("read schema_migrations: %v", err)
	}
	if !strings.HasSuffix(version, ".sql") {
		t.Fatalf("migration version %q does not look like a file name", version)
	}
	// Re-running must be a no-op rather than an error.
	if err := migrate(context.Background(), st.Pool()); err != nil {
		t.Fatalf("re-running migrations: %v", err)
	}
}

// pngBytes and jpegBytes are minimal but genuinely valid headers, because the API
// layer sniffs content types and would reject filler.
func pngBytes() []byte {
	return append([]byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}, make([]byte, 32)...)
}

func jpegBytes() []byte {
	return append([]byte{0xff, 0xd8, 0xff, 0xe0}, make([]byte, 32)...)
}

var _ = pgxpool.Config{}
