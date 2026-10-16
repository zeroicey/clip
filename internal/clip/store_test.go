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
		// Mirrors config.Load's defaults. Leaving a duration field at its zero value
		// here is not harmless: a code share's TTL is used as an absolute deadline, so
		// a zero would mint shares that are born expired and every share test would
		// fail with a 404 that looks like a routing bug.
		ShareCodeTTL: 30 * time.Minute,
		// Non-zero on purpose: a zero limit disables the limiter entirely
		// (ratelimit treats <= 0 as "unlimited"), so a test config that left it out
		// would silently stop exercising password throttling.
		UnlockPerHour: 20,
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

// mustCreate inserts a clip directly. `password` is the stored PHC hash, or nil
// for a public clip — the same shape the store takes, so a test cannot
// accidentally create a "protected" clip whose password is a plaintext string.
func mustCreate(t *testing.T, st *Store, suffix, body string, password *string, expiresAt int64) {
	t.Helper()
	res, err := st.Create(context.Background(), suffix, body, expiresAt, password)
	if err != nil {
		t.Fatalf("Create(%q): %v", suffix, err)
	}
	if res.Rev != 1 {
		t.Fatalf("first revision = %d, want 1", res.Rev)
	}
}

// hashOf is the stored hash for a test password. Tests pass the hash around
// because that — not the plaintext — is what the write queries take as proof.
func hashOf(t *testing.T, plain string) string {
	t.Helper()
	h, err := hashPassword(plain)
	if err != nil {
		t.Fatalf("hashPassword: %v", err)
	}
	return h
}

func TestCreateAndGetRoundTrip(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	mustCreate(t, st, "ops1", "hello\nworld\n", nil, 0)

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
	// A clip created without a password is public. That is the documented model
	// (anyone may read and write it), so it is asserted rather than assumed.
	if got.HasPassword() {
		t.Error("a clip created with no password reports one")
	}
	if got.PasswordHash != nil {
		t.Errorf("password_hash = %q, want nil for a public clip", *got.PasswordHash)
	}
}

// TestPublicClipIsWritableWithNoCredential is the new model in one test: with no
// password there is nothing to present, and the write is allowed.
//
// It exists because the opposite was true until 2026-10-16 (a minted owner token
// was required) and a regression to that behaviour would look like a bug report
// about "can't save" rather than a security change.
func TestPublicClipIsWritableWithNoCredential(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	mustCreate(t, st, "ops1", "the original", nil, 0)

	if _, err := st.Update(ctx, "ops1", "", "anyone may do this", -1); err != nil {
		t.Fatalf("a public clip refused a credential-free update: %v", err)
	}
	got, err := st.Get(ctx, "ops1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Body != "anyone may do this" || got.Rev != 2 {
		t.Fatalf("after the update: body=%q rev=%d", got.Body, got.Rev)
	}
}

// TestUpdateRequiresThePassword is the security boundary of the password model:
// the proof travels INTO the UPDATE, so a wrong (or absent) one must match no
// row. It asserts the negative case directly — the body is unchanged — because
// "the call returned an error" is not the same claim as "nothing was written".
func TestUpdateRequiresThePassword(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	mustCreate(t, st, "ops1", "the original", ptr(hashOf(t, "s3cret")), 0)

	stored, err := st.Get(ctx, "ops1")
	if err != nil {
		t.Fatal(err)
	}
	if !stored.HasPassword() {
		t.Fatal("setup: the clip has no password")
	}

	// Neither a wrong password nor no password at all may write.
	for _, proof := range []string{"", hashOf(t, "wrong")} {
		if _, err := st.Update(ctx, "ops1", proof, "defaced", -1); !errors.Is(err, ErrNotFound) {
			t.Fatalf("update with proof %q = %v, want ErrNotFound", proof, err)
		}
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

	// The proof is the hash that was verified, not a fresh hash of the same
	// password: a second hash of "s3cret" is salted differently and must not
	// match — which is what makes the check ":the row still holds what I
	// verified", not ":someone knows this string".
	if _, err := st.Update(ctx, "ops1", hashOf(t, "s3cret"), "wrong mechanism", -1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a re-hash of the same password was accepted: %v", err)
	}

	if _, err := st.Update(ctx, "ops1", *stored.PasswordHash, "the fix", -1); err != nil {
		t.Fatalf("update with the stored hash: %v", err)
	}
	got, _ = st.Get(ctx, "ops1")
	if got.Body != "the fix" || got.Rev != 2 {
		t.Fatalf("after the authorised update: body=%q rev=%d, want %q/2", got.Body, got.Rev, "the fix")
	}
}

// TestReKeyInvalidatesAnInFlightProof is the reason the proof goes into the
// statement instead of being checked beforehand: between verifying a password
// and writing with it, the password can change. The old proof must then match
// nothing, so the write simply does not happen.
func TestReKeyInvalidatesAnInFlightProof(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	mustCreate(t, st, "ops1", "original", ptr(hashOf(t, "first")), 0)

	snapshot, err := st.Get(ctx, "ops1")
	if err != nil {
		t.Fatal(err)
	}
	verified := *snapshot.PasswordHash // what a caller would have verified

	// Someone re-keys the clip with the old password before our write lands.
	if err := st.SetPassword(ctx, "ops1", verified, hashOf(t, "second")); err != nil {
		t.Fatalf("re-key: %v", err)
	}

	if _, err := st.Update(ctx, "ops1", verified, "stale writer", -1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a proof verified before the re-key was accepted after it: %v", err)
	}
	got, _ := st.Get(ctx, "ops1")
	if got.Body != "original" {
		t.Fatalf("a stale proof wrote: %q", got.Body)
	}
}

// TestSetPasswordClaimsAPublicClip pins the "first one to set a password owns
// it" rule, including the part that is easy to forget: the claim needs no
// credential (there is none to give), and it locks out everyone else.
func TestSetPasswordClaimsAPublicClip(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	mustCreate(t, st, "ops1", "public notes", nil, 0)

	if err := st.SetPassword(ctx, "ops1", "", hashOf(t, "mine")); err != nil {
		t.Fatalf("claiming a public clip: %v", err)
	}
	got, err := st.Get(ctx, "ops1")
	if err != nil {
		t.Fatal(err)
	}
	if !got.HasPassword() {
		t.Fatal("the clip is still public after being claimed")
	}

	// A second claimer with no credential is refused: the suffix is taken now.
	if err := st.SetPassword(ctx, "ops1", "", hashOf(t, "yours")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a second claim on a protected clip = %v, want ErrNotFound", err)
	}
	// And a write without the password is refused too.
	if _, err := st.Update(ctx, "ops1", "", "hijack", -1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("write on a claimed clip = %v, want ErrNotFound", err)
	}
}

// TestClearPasswordReopensTheClip is the way back: proving the password makes
// the clip public again, and then anyone may write it.
func TestClearPasswordReopensTheClip(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	mustCreate(t, st, "ops1", "private", ptr(hashOf(t, "mine")), 0)

	if err := st.ClearPassword(ctx, "ops1", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("clearing without the password = %v, want ErrNotFound", err)
	}
	got, _ := st.Get(ctx, "ops1")
	if !got.HasPassword() {
		t.Fatal("an unauthorised clear removed the password")
	}

	if err := st.ClearPassword(ctx, "ops1", *got.PasswordHash); err != nil {
		t.Fatalf("clearing with the password: %v", err)
	}
	got, _ = st.Get(ctx, "ops1")
	if got.HasPassword() {
		t.Fatal("the password survived a clear")
	}
	if _, err := st.Update(ctx, "ops1", "", "anyone again", -1); err != nil {
		t.Fatalf("the reopened clip refused a credential-free write: %v", err)
	}
}

// ptr is a convenience for the optional password hash the store takes.
func ptr(s string) *string { return &s }

// TestCreateOnALiveSuffixIsRefused proves the create path cannot be used as a
// back door to overwrite a live clip — not even a public one, where the attempt
// comes from someone who is allowed to write it: claiming an existing suffix is
// an UPDATE, and Create is only for a suffix nobody is using.
func TestCreateOnALiveSuffixIsRefused(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	mustCreate(t, st, "ops1", "the original", nil, 0)

	if _, err := st.Create(ctx, "ops1", "hijacked", 0, nil); !errors.Is(err, ErrTaken) {
		t.Fatalf("Create on a live suffix = %v, want ErrTaken", err)
	}
	got, _ := st.Get(ctx, "ops1")
	if got.Body != "the original" {
		t.Fatalf("body was overwritten: %q", got.Body)
	}
	// The clip is still public, i.e. the refused create did not make it private.
	if got.HasPassword() {
		t.Fatal("a refused create changed the clip's protection")
	}
}

// TestExpiredSuffixBecomesReusable is why ClaimSuffix has a takeover branch: an
// expired row still occupies its suffix until the janitor runs. Without the
// takeover, a suffix that expired a second ago would be permanently unusable —
// "taken" by a clip nobody can read.
//
// It also pins that the takeover does NOT carry the old password over: a new
// occupant inherits an empty, public clip, or the previous holder's secret would
// outlive their clip.
func TestExpiredSuffixBecomesReusable(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	past := time.Now().Add(-time.Hour).UnixMilli()
	mustCreate(t, st, "ops1", "old content", ptr(hashOf(t, "old-secret")), past)

	if _, err := st.Get(ctx, "ops1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an expired clip is still readable: %v", err)
	}
	// The old password cannot resurrect it either: expiry is expiry.
	if _, err := st.Update(ctx, "ops1", hashOf(t, "old-secret"), "zombie", -1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an expired clip accepted an update: %v", err)
	}

	if _, err := st.Create(ctx, "ops1", "new content", 0, nil); err != nil {
		t.Fatalf("reusing an expired suffix: %v", err)
	}
	got, err := st.Get(ctx, "ops1")
	if err != nil {
		t.Fatal(err)
	}
	if got.HasPassword() {
		t.Error("the new occupant inherited the previous holder's password")
	}
	if _, err := st.Update(ctx, "ops1", "", "the new occupant writes", -1); err != nil {
		t.Fatalf("the reused suffix is not publicly writable: %v", err)
	}
	if got.Body != "new content" || got.Rev != 1 {
		t.Fatalf("after takeover: body=%q rev=%d, want %q/1", got.Body, got.Rev, "new content")
	}
	// There is deliberately no "the previous holder is locked out" assertion
	// here any more: a taken-over suffix is a NEW public clip, so the previous
	// holder has no standing to be locked out of — they are exactly as much a
	// stranger to it as anyone else. What matters was asserted above: the
	// password did not carry over.
}

// TestTakeoverClearsTheOldOwnersImages is the guard for the ON DELETE CASCADE
// that does NOT fire: a takeover is an UPDATE, so without an explicit delete the
// new holder would inherit the previous one's images.
func TestTakeoverClearsTheOldOwnersImages(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	past := time.Now().Add(-time.Hour).UnixMilli()
	mustCreate(t, st, "ops1", "old", ptr(hashOf(t, "old-secret")), past)

	if _, err := st.SetAssets(ctx, "ops1", "",
		[]Asset{{Position: 0, Mime: "image/png", Bytes: pngBytes()}}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetAssets on an expired clip = %v, want ErrNotFound", err)
	}

	// Write the asset while the clip is still live, then let it expire.
	if _, err := st.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	mustCreate(t, st, "ops2", "live", nil, 0)
	if _, err := st.SetAssets(ctx, "ops2", "",
		[]Asset{{Position: 0, Mime: "image/png", Bytes: pngBytes()}}); err != nil {
		t.Fatal(err)
	}
	// Expire it by hand, then take the suffix over.
	if _, err := st.Pool().Exec(ctx, `UPDATE clips SET expires_at = $1 WHERE suffix = 'ops2'`, past); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Create(ctx, "ops2", "fresh", 0, nil); err != nil {
		t.Fatalf("takeover: %v", err)
	}
	got, err := st.Get(ctx, "ops2")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Assets) != 0 {
		t.Fatalf("the new holder inherited %d image(s) from the old one", len(got.Assets))
	}
}

func TestQuotaStopsWrites(t *testing.T) {
	st := testStore(t, func(c *Config) { c.MaxTotalClips = 1 })
	ctx := context.Background()
	mustCreate(t, st, "one", "x", nil, 0)

	if _, err := st.Create(ctx, "two", "y", 0, nil); !errors.Is(err, ErrQuota) {
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
	mustCreate(t, st, "one", "0123456789", nil, 0)

	if _, err := st.Create(ctx, "two", "x", 0, nil); !errors.Is(err, ErrQuota) {
		t.Fatalf("Create past the byte quota = %v, want ErrQuota", err)
	}
}

func TestSweepDeletesOnlyExpired(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	past := time.Now().Add(-time.Hour).UnixMilli()
	future := time.Now().Add(time.Hour).UnixMilli()
	mustCreate(t, st, "gone", "a", nil, past)
	mustCreate(t, st, "stays", "b", nil, future)
	mustCreate(t, st, "forever", "c", nil, 0)

	res, err := st.Sweep(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Clips != 1 {
		t.Fatalf("Sweep deleted %d clips, want 1", res.Clips)
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
	mustCreate(t, st, "ops1", "text", nil, 0)

	res, err := st.SetAssets(ctx, "ops1", "", []Asset{
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
	res, err = st.SetAssets(ctx, "ops1", "", []Asset{
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

// TestSetAssetsRequiresThePassword: the image set is behind the same password as
// the text. Leaving it open would let anyone with the link replace a private
// clip's screenshots while being unable to read them.
func TestSetAssetsRequiresThePassword(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	mustCreate(t, st, "ops1", "text", ptr(hashOf(t, "s3cret")), 0)

	stored, err := st.Get(ctx, "ops1")
	if err != nil {
		t.Fatal(err)
	}
	for _, proof := range []string{"", hashOf(t, "nope")} {
		if _, err := st.SetAssets(ctx, "ops1", proof,
			[]Asset{{Position: 0, Mime: "image/png", Bytes: pngBytes()}}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("SetAssets with proof %q = %v, want ErrNotFound", proof, err)
		}
	}
	got, _ := st.Get(ctx, "ops1")
	if len(got.Assets) != 0 {
		t.Fatalf("an unauthorised SetAssets wrote %d asset(s)", len(got.Assets))
	}
	if got.Rev != 1 {
		t.Fatalf("rev moved on a refused SetAssets: %d", got.Rev)
	}
	// With the password, it works — otherwise the previous assertions would pass
	// for the wrong reason ("nothing ever works" is not "the check works").
	if _, err := st.SetAssets(ctx, "ops1", *stored.PasswordHash,
		[]Asset{{Position: 0, Mime: "image/png", Bytes: pngBytes()}}); err != nil {
		t.Fatalf("SetAssets with the password: %v", err)
	}
}

// TestRevChangesEvenWhenTheBodyDoesNot: a reader waits on the revision, so a
// write of identical bytes must still advance it, or the other machine would
// never learn that anything happened.
func TestRevChangesEvenWhenTheBodyDoesNot(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	mustCreate(t, st, "ops1", "same", nil, 0)
	before, _ := st.Get(ctx, "ops1")

	if _, err := st.Update(ctx, "ops1", "", "same", -1); err != nil {
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
	mustCreate(t, st, "ops1", "x", nil, deadline)

	if _, err := st.Update(ctx, "ops1", "", "y", -1); err != nil {
		t.Fatal(err)
	}
	got, _ := st.Get(ctx, "ops1")
	if got.ExpiresAt != deadline {
		t.Fatalf("expiry moved on an update that did not mention it: %d -> %d", deadline, got.ExpiresAt)
	}
	// And an explicit 0 means forever.
	if _, err := st.Update(ctx, "ops1", "", "z", 0); err != nil {
		t.Fatal(err)
	}
	got, _ = st.Get(ctx, "ops1")
	if got.ExpiresAt != 0 {
		t.Fatalf("explicit 0 did not mean never: %d", got.ExpiresAt)
	}
}

func TestDeleteRequiresThePassword(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	mustCreate(t, st, "ops1", "text", ptr(hashOf(t, "s3cret")), 0)

	stored, err := st.Get(ctx, "ops1")
	if err != nil {
		t.Fatal(err)
	}
	// No credential and a wrong one are both refused, and the clip survives both.
	for _, proof := range []string{"", hashOf(t, "nope")} {
		if ok, err := st.Delete(ctx, "ops1", proof); err != nil || ok {
			t.Fatalf("Delete with proof %q = %v/%v, want false/nil", proof, ok, err)
		}
	}
	if _, err := st.Get(ctx, "ops1"); err != nil {
		t.Fatalf("a refused delete removed the clip: %v", err)
	}

	if ok, err := st.Delete(ctx, "ops1", *stored.PasswordHash); err != nil || !ok {
		t.Fatalf("Delete with the password = %v/%v, want true/nil", ok, err)
	}
	if _, err := st.Get(ctx, "ops1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the clip survived a delete with the right password: %v", err)
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
