package clip

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// The share feature has one property that everything else hangs off: the entrance
// at /s/<token> is READ-ONLY and its secret is INDEPENDENT of the clip's master
// password. Both are asserted here against a real database, because both are
// enforced in SQL (the share resolves through its own row; the write statements
// are not reachable from /s at all) and a fake store would let either pass while
// the real one was broken.
//
// Design: .ai/decisions/2026-10-16-clip-share-links.md.

// createShare POSTs a share and returns its token and the decoded payload.
func createShare(t *testing.T, r *gin.Engine, suffix, masterPw string, body map[string]any) (string, map[string]any) {
	t.Helper()
	hdrs := map[string]string{}
	if masterPw != "" {
		hdrs["X-Clip-Password"] = masterPw
	}
	w := do(t, r, "POST", "/api/clips/"+suffix+"/shares", body, hdrs)
	if w.Code != http.StatusCreated {
		t.Fatalf("create share = %d, want 201 (%s)", w.Code, w.Body.String())
	}
	_, data, _ := envelope(t, w)
	token, _ := data["token"].(string)
	if token == "" {
		t.Fatalf("create share returned no token: %s", w.Body.String())
	}
	return token, data
}

// ---------------------------------------------------------------------------
// the store
// ---------------------------------------------------------------------------

func TestShareRoundTripAndRevoke(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	mustCreate(t, st, "ops1", "hello", nil, 0)

	token, err := newShareToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateShare(ctx, "ops1", "", token, nil, nil, 0); err != nil {
		t.Fatalf("CreateShare: %v", err)
	}

	sh, err := st.GetShare(ctx, token)
	if err != nil {
		t.Fatalf("GetShare: %v", err)
	}
	if sh.Suffix != "ops1" || sh.NeedsSecret() {
		t.Fatalf("share = %+v, want a link-only share on ops1", sh)
	}

	// Wrong suffix + right token revokes nothing: a token only ever belongs to the
	// clip it was minted for.
	if ok, err := st.DeleteShare(ctx, "other", "", token); err != nil || ok {
		t.Fatalf("DeleteShare through another suffix = (%v, %v), want (false, nil)", ok, err)
	}
	if ok, err := st.DeleteShare(ctx, "ops1", "", token); err != nil || !ok {
		t.Fatalf("DeleteShare = (%v, %v), want (true, nil)", ok, err)
	}
	if _, err := st.GetShare(ctx, token); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a revoked share is still readable: %v", err)
	}
}

// TestCreateShareWithAStaleProofInsertsNothing is the share half of
// TestReKeyInvalidatesAnInFlightProof: the master password check travels into the
// INSERT, so a password rotated between "verify" and "insert" must leave no row.
func TestCreateShareWithAStaleProofInsertsNothing(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	oldHash := hashOf(t, "old-secret")
	mustCreate(t, st, "ops1", "x", ptr(oldHash), 0)

	// Rotate the password, but keep using the hash that was verified a moment ago.
	newHash := hashOf(t, "new-secret")
	if err := st.SetPassword(ctx, "ops1", oldHash, newHash); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	token, _ := newShareToken()
	if err := st.CreateShare(ctx, "ops1", oldHash, token, nil, nil, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("CreateShare with a stale proof = %v, want ErrNotFound", err)
	}
	if _, err := st.GetShare(ctx, token); !errors.Is(err, ErrNotFound) {
		t.Fatal("a stale proof still minted a share")
	}
	// The current password does work, so the assertion above is not passing because
	// sharing is broken.
	if err := st.CreateShare(ctx, "ops1", newHash, token, nil, nil, 0); err != nil {
		t.Fatalf("CreateShare with the current proof: %v", err)
	}
}

// TestShareCascadeDeletesWithTheClip pins the ON DELETE CASCADE: a deleted clip
// must not leave a share pointing at a suffix that no longer exists.
func TestShareCascadeDeletesWithTheClip(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	mustCreate(t, st, "ops1", "x", nil, 0)
	token, _ := newShareToken()
	if err := st.CreateShare(ctx, "ops1", "", token, nil, nil, 0); err != nil {
		t.Fatal(err)
	}
	if ok, err := st.Delete(ctx, "ops1", ""); err != nil || !ok {
		t.Fatalf("delete clip = (%v, %v)", ok, err)
	}
	if _, err := st.GetShare(ctx, token); !errors.Is(err, ErrNotFound) {
		t.Fatal("the share survived its clip")
	}
}

// TestTakeoverDropsTheOldOwnersShares guards the trap that a takeover is an
// UPDATE: the ON DELETE CASCADE never fires, so unless shares are cleared
// explicitly the previous holder's read link keeps working against the NEW
// holder's content. That is the worst version of the dangling-share bug: not a
// dead link, but a live one into somebody else's text.
func TestTakeoverDropsTheOldOwnersShares(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	past := time.Now().Add(-time.Hour).UnixMilli()
	// One hash value, used both as the stored credential and as the proof: hashOf
	// salts every call, so calling it twice would produce a proof that does not
	// match (and the test would fail for the wrong reason).
	oldHash := hashOf(t, "old-secret")
	mustCreate(t, st, "ops1", "old secret", ptr(oldHash), 0)

	token, _ := newShareToken()
	if err := st.CreateShare(ctx, "ops1", oldHash, token, nil, nil, 0); err != nil {
		t.Fatal(err)
	}

	// Let the clip expire, then let a new holder take the suffix over.
	if _, err := st.Pool().Exec(ctx, `UPDATE clips SET expires_at = $1 WHERE suffix = 'ops1'`, past); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Create(ctx, "ops1", "new owner's text", 0, nil); err != nil {
		t.Fatalf("takeover: %v", err)
	}

	if _, err := st.GetShare(ctx, token); !errors.Is(err, ErrNotFound) {
		t.Fatal("the old holder's share survived the takeover and now points at the new holder's content")
	}
}

// TestSweepDeletesExpiredSharesIndependentlyOfTheClip is the guard for the two
// TTLs being separate: a 30-minute pickup-code link on a week-long clip must not
// sit in the table (nor stay guessable) for the rest of the week.
func TestSweepDeletesExpiredSharesIndependentlyOfTheClip(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	mustCreate(t, st, "ops1", "x", nil, 0)

	past := time.Now().Add(-time.Minute).UnixMilli()
	future := time.Now().Add(time.Hour).UnixMilli()
	shortToken, _ := newShareToken()
	longToken, _ := newShareToken()
	if err := st.CreateShare(ctx, "ops1", "", shortToken, nil, nil, past); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateShare(ctx, "ops1", "", longToken, nil, nil, future); err != nil {
		t.Fatal(err)
	}

	// An expired share is unreadable before the janitor runs...
	if _, err := st.GetShare(ctx, shortToken); !errors.Is(err, ErrNotFound) {
		t.Fatal("an expired share is still readable")
	}
	res, err := st.Sweep(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Shares != 1 {
		t.Fatalf("Sweep deleted %d shares, want 1", res.Shares)
	}
	if res.Clips != 0 {
		t.Fatalf("Sweep deleted %d clips, want 0 (the clip is not expired)", res.Clips)
	}
	if _, err := st.GetShare(ctx, longToken); err != nil {
		t.Fatalf("Sweep removed a live share: %v", err)
	}
}

func TestShareTokenValidation(t *testing.T) {
	token, err := newShareToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateShareToken(token); err != nil {
		t.Fatalf("a freshly minted token was rejected: %v", err)
	}
	// Short, empty, and out-of-alphabet tokens are all unusable. The upper bound
	// matters most: it stops a pathological input from making the database compare
	// megabyte strings.
	for _, bad := range []string{"", "short", strings.Repeat("a", ShareTokenMaxLen+1), "has/slash", "has space", "有中文"} {
		if err := ValidateShareToken(bad); err == nil {
			t.Errorf("ValidateShareToken(%q) = nil, want an error", bad)
		}
	}
}

// TestShareCodeIsUniformlySixDigits is a cheap sanity check on the generator: the
// format has to be stable, because the code is meant to be read aloud.
func TestShareCodeIsUniformlySixDigits(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		code, err := newShareCode()
		if err != nil {
			t.Fatal(err)
		}
		if len(code) != ShareCodeDigits {
			t.Fatalf("code %q is not %d digits", code, ShareCodeDigits)
		}
		for _, c := range code {
			if c < '0' || c > '9' {
				t.Fatalf("code %q is not all digits", code)
			}
		}
		seen[code] = true
	}
	if len(seen) < 20 {
		t.Fatalf("50 codes produced only %d distinct values; the generator looks degenerate", len(seen))
	}
}

// ---------------------------------------------------------------------------
// the read surface
// ---------------------------------------------------------------------------

// TestEveryShareReadPathIsBehindTheShareSecret is the guard that makes a share
// secret mean something. A share's content is reachable four ways, and any one of
// them left open makes the secret decoration — /raw in particular is the route a
// script curls, so it is the one most likely to be forgotten.
//
// It ALSO pins the independence rule: the clip's master password must not open a
// share whose own secret is different, and the share secret must open a clip the
// caller could not otherwise read.
func TestEveryShareReadPathIsBehindTheShareSecret(t *testing.T) {
	r := testServer(t)
	const secret = "SHARED-CONTENT-xyz789"
	const masterPw = "master-password"
	const sharePw = "share-password"

	if w := do(t, r, "PUT", "/api/clips/ops1",
		map[string]any{"body": secret, "password": masterPw}, nil); w.Code != http.StatusCreated {
		t.Fatalf("create = %d (%s)", w.Code, w.Body.String())
	}
	if w := upload(t, r, "ops1", masterPw, map[string][]byte{"shot.png": pngBytes()}); w.Code != http.StatusOK {
		t.Fatalf("upload = %d (%s)", w.Code, w.Body.String())
	}
	token, _ := createShare(t, r, "ops1", masterPw, map[string]any{"password": sharePw})

	// Without the share secret, or with the MASTER password, every path refuses
	// and leaks nothing. The master password is the interesting case: it is the
	// strongest credential the owner has, and it still must not open the share,
	// because the share's whole point is that its visitor does not hold it.
	for _, path := range []string{"/api/shares/" + token, "/s/" + token + "/raw", "/s/" + token + "/raw/0"} {
		for _, hdr := range []string{"", masterPw, "wrong"} {
			hdrs := map[string]string{}
			if hdr != "" {
				hdrs["X-Clip-Password"] = hdr
			}
			w := do(t, r, "GET", path, nil, hdrs)
			if w.Code == http.StatusOK {
				t.Errorf("GET %s with secret %q = 200; this read path is not protected", path, hdr)
			}
			if strings.Contains(w.Body.String(), secret) {
				t.Errorf("GET %s with secret %q leaked the body", path, hdr)
			}
		}
		// The share secret opens it, so the assertions above are not passing merely
		// because the path is broken.
		w := do(t, r, "GET", path, nil, map[string]string{"X-Clip-Password": sharePw})
		if w.Code != http.StatusOK {
			t.Errorf("GET %s with the share secret = %d, want 200 (%s)", path, w.Code, w.Body.String())
		}
	}

	// The page: 200 (the link is live) but the body is NOT in the HTML.
	page := do(t, r, "GET", "/s/"+token, nil, nil)
	if page.Code != http.StatusOK {
		t.Fatalf("share page = %d, want 200", page.Code)
	}
	html := page.Body.String()
	if strings.Contains(html, secret) {
		t.Error("the locked share page carried the clip body in its HTML")
	}
	if !strings.Contains(html, `data-state="locked"`) {
		t.Error("the locked share page does not render the locked state")
	}

	// And the master password DOES still open the clip's own entrance, so this test
	// is not accidentally describing a clip that lost its password.
	if w := do(t, r, "GET", "/api/clips/ops1", nil, map[string]string{"X-Clip-Password": masterPw}); w.Code != http.StatusOK {
		t.Fatalf("the master password no longer opens the clip: %d", w.Code)
	}
}

// TestSharePageNeverRevealsTheOwnerSAddress pins the reason the token exists as a
// separate address: /s/<token> must be usable without disclosing (or helping
// enumerate) the suffix the owner actually types.
func TestSharePageNeverRevealsTheOwnerSAddress(t *testing.T) {
	r := testServer(t)
	if w := do(t, r, "PUT", "/api/clips/secretops", map[string]any{"body": "text"}, nil); w.Code != http.StatusCreated {
		t.Fatalf("create = %d", w.Code)
	}
	token, data := createShare(t, r, "secretops", "", map[string]any{"password": "share-pw"})

	// The list response must not carry the suffix either — it is the same
	// temptation ("just include it so the page can build URLs").
	if _, present := data["suffix"]; present {
		t.Error("the share payload exposes the clip suffix")
	}
	for _, path := range []string{"/s/" + token, "/api/shares/" + token} {
		hdrs := map[string]string{"X-Clip-Password": "share-pw"}
		w := do(t, r, "GET", path, nil, hdrs)
		if strings.Contains(w.Body.String(), "secretops") {
			t.Errorf("GET %s leaked the owner's suffix: %s", path, w.Body.String())
		}
	}
}

// TestShareContentFollowsTheOwnersEdit is the "share does not copy" rule: the
// share points at the same row, so an edit by the owner shows up through the same
// token with no second copy and no invalidation.
func TestShareContentFollowsTheOwnersEdit(t *testing.T) {
	r := testServer(t)
	if w := do(t, r, "PUT", "/api/clips/ops1", map[string]any{"body": "first"}, nil); w.Code != http.StatusCreated {
		t.Fatalf("create = %d", w.Code)
	}
	token, _ := createShare(t, r, "ops1", "", map[string]any{})

	if w := do(t, r, "PUT", "/api/clips/ops1", map[string]any{"body": "second"}, nil); w.Code != http.StatusOK {
		t.Fatalf("update = %d (%s)", w.Code, w.Body.String())
	}
	got := do(t, r, "GET", "/s/"+token+"/raw", nil, nil)
	if got.Code != http.StatusOK || got.Body.String() != "second" {
		t.Fatalf("share raw = %d %q, want 200 \"second\"", got.Code, got.Body.String())
	}
}

// TestLinkOnlyShareNeedsNoSecretAndOutlivesTheMasterPassword pins a choice that
// looks surprising and is deliberate: a link-only share on a password-protected
// clip is readable by anyone holding the link. The owner chose that shape when
// they created the share — "只看链接即可看" is one of the three documented modes —
// and the share secret is independent of the master password by design.
func TestLinkOnlyShareNeedsNoSecretAndOutlivesTheMasterPassword(t *testing.T) {
	r := testServer(t)
	if w := do(t, r, "PUT", "/api/clips/ops1",
		map[string]any{"body": "public-ish", "password": "master"}, nil); w.Code != http.StatusCreated {
		t.Fatalf("create = %d", w.Code)
	}
	token, data := createShare(t, r, "ops1", "master", map[string]any{})
	if data["has_password"] != false || data["has_code"] != false {
		t.Fatalf("link-only share reported a secret: %v", data)
	}
	w := do(t, r, "GET", "/s/"+token+"/raw", nil, nil)
	if w.Code != http.StatusOK || w.Body.String() != "public-ish" {
		t.Fatalf("GET link-only share raw = %d %q, want 200", w.Code, w.Body.String())
	}
}

// TestPickupCodeIsReturnedOnceAndThenActsAsTheSecret covers the third mode. The
// code is a secret like any other, so it is stored hashed and can never be read
// back: the creation response is the only time it exists outside the visitor's
// head, which is why the UI has to say so.
func TestPickupCodeIsReturnedOnceAndThenActsAsTheSecret(t *testing.T) {
	r := testServer(t)
	if w := do(t, r, "PUT", "/api/clips/ops1", map[string]any{"body": "by-code"}, nil); w.Code != http.StatusCreated {
		t.Fatalf("create = %d", w.Code)
	}
	token, data := createShare(t, r, "ops1", "", map[string]any{"code": true})
	code, _ := data["code"].(string)
	if len(code) != ShareCodeDigits {
		t.Fatalf("creation returned code %q, want %d digits", code, ShareCodeDigits)
	}
	if data["has_code"] != true || data["has_password"] != false {
		t.Fatalf("code share flags = %v", data)
	}

	// Reading it back through the list must NOT return the code — only the fact
	// that one exists.
	w := do(t, r, "GET", "/api/clips/ops1/shares", nil, nil)
	if strings.Contains(w.Body.String(), code) {
		t.Fatalf("the share list re-exposed the pickup code: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"has_code":true`) {
		t.Fatalf("the share list does not report has_code: %s", w.Body.String())
	}

	// Wrong code refused, right code opens.
	if w := do(t, r, "GET", "/s/"+token+"/raw", nil, map[string]string{"X-Clip-Password": "000000"}); w.Code == http.StatusOK && code != "000000" {
		t.Fatal("a wrong pickup code was accepted")
	}
	if w := do(t, r, "GET", "/s/"+token+"/raw", nil, map[string]string{"X-Clip-Password": code}); w.Code != http.StatusOK {
		t.Fatalf("the pickup code does not open its share: %d (%s)", w.Code, w.Body.String())
	}
}

// TestShareCreationRequiresTheMasterPassword: minting a read link to a protected
// clip is exactly as sensitive as handing out the master password, so it needs it.
func TestShareCreationRequiresTheMasterPassword(t *testing.T) {
	r := testServer(t)
	if w := do(t, r, "PUT", "/api/clips/ops1",
		map[string]any{"body": "x", "password": "master"}, nil); w.Code != http.StatusCreated {
		t.Fatalf("create = %d", w.Code)
	}
	if w := do(t, r, "POST", "/api/clips/ops1/shares", map[string]any{}, nil); w.Code != http.StatusUnauthorized {
		t.Errorf("creating a share with no master password = %d, want 401", w.Code)
	}
	if w := do(t, r, "POST", "/api/clips/ops1/shares", map[string]any{},
		map[string]string{"X-Clip-Password": "wrong"}); w.Code != http.StatusUnauthorized {
		t.Errorf("creating a share with a wrong master password = %d, want 401", w.Code)
	}
	// Listing too.
	if w := do(t, r, "GET", "/api/clips/ops1/shares", nil, nil); w.Code != http.StatusUnauthorized {
		t.Errorf("listing shares with no master password = %d, want 401", w.Code)
	}
	// And on a public clip, no credential at all is needed — that is the model.
	if w := do(t, r, "PUT", "/api/clips/ops2", map[string]any{"body": "y"}, nil); w.Code != http.StatusCreated {
		t.Fatalf("create public = %d", w.Code)
	}
	if w := do(t, r, "POST", "/api/clips/ops2/shares", map[string]any{}, nil); w.Code != http.StatusCreated {
		t.Errorf("creating a share on a public clip = %d, want 201 (%s)", w.Code, w.Body.String())
	}
}

// TestRevokeIsImmediateAndCannotCrossClips: the owner's way to take a link back,
// and the guard that one clip's authority cannot revoke another's share.
func TestRevokeIsImmediateAndCannotCrossClips(t *testing.T) {
	r := testServer(t)
	for _, s := range []string{"ops1", "ops2"} {
		if w := do(t, r, "PUT", "/api/clips/"+s, map[string]any{"body": s}, nil); w.Code != http.StatusCreated {
			t.Fatalf("create %s = %d", s, w.Code)
		}
	}
	token, _ := createShare(t, r, "ops1", "", map[string]any{})

	// ops2's authority must not revoke ops1's share.
	if w := do(t, r, "DELETE", "/api/clips/ops2/shares/"+token, nil, nil); w.Code != http.StatusNotFound {
		t.Errorf("revoking through the wrong clip = %d, want 404", w.Code)
	}
	if w := do(t, r, "GET", "/s/"+token+"/raw", nil, nil); w.Code != http.StatusOK {
		t.Fatal("the share was revoked through the wrong clip's authority")
	}

	if w := do(t, r, "DELETE", "/api/clips/ops1/shares/"+token, nil, nil); w.Code != http.StatusNoContent {
		t.Fatalf("revoke = %d (%s)", w.Code, w.Body.String())
	}
	// Immediately dead, on every path.
	for _, path := range []string{"/s/" + token, "/s/" + token + "/raw", "/api/shares/" + token} {
		if w := do(t, r, "GET", path, nil, nil); w.Code != http.StatusNotFound {
			t.Errorf("GET %s after revocation = %d, want 404", path, w.Code)
		}
	}
	if w := do(t, r, "DELETE", "/api/clips/ops1/shares/"+token, nil, nil); w.Code != http.StatusNotFound {
		t.Errorf("revoking twice = %d, want 404", w.Code)
	}
}

// TestUnknownShareIs404AndNeverLeaksTheSuffix: a token that does not resolve is
// the same answer whether it is malformed, unknown or expired — and that answer
// must not describe the clip namespace.
func TestUnknownShareIs404AndNeverLeaksTheSuffix(t *testing.T) {
	r := testServer(t)
	if w := do(t, r, "PUT", "/api/clips/ops1", map[string]any{"body": "x"}, nil); w.Code != http.StatusCreated {
		t.Fatalf("create = %d", w.Code)
	}
	unknown, _ := newShareToken()
	expired, _ := newShareToken()
	// An expired share (created with a past deadline) is in the same bucket.
	if w := do(t, r, "POST", "/api/clips/ops1/shares", map[string]any{"expiresInHours": 0.000001}, nil); w.Code != http.StatusCreated {
		t.Fatalf("create expiring share = %d", w.Code)
	}
	_ = expired

	for _, path := range []string{
		"/s/" + unknown, "/s/" + unknown + "/raw", "/s/" + unknown + "/raw/0",
		"/api/shares/" + unknown, "/s/not-a-token", "/s/", "/s",
	} {
		w := do(t, r, "GET", path, nil, nil)
		if w.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, w.Code)
		}
		if strings.Contains(w.Body.String(), "ops1") {
			t.Errorf("GET %s leaked a suffix: %s", path, w.Body.String())
		}
	}
	// The JSON shape stays the envelope even for a dead link, so a client never
	// has to parse text/plain to find out why its request failed.
	w := do(t, r, "GET", "/api/shares/"+unknown, nil, nil)
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("a dead share answered %q; /api must stay JSON", ct)
	}
}

// TestShareEntranceHasNoWriteShape asserts the read-only guarantee at the routing
// layer rather than trusting a hidden button. /s/... only exists for GET and HEAD,
// so every write-shaped request is refused before any handler can be reached.
func TestShareEntranceHasNoWriteShape(t *testing.T) {
	r := testServer(t)
	if w := do(t, r, "PUT", "/api/clips/ops1", map[string]any{"body": "x"}, nil); w.Code != http.StatusCreated {
		t.Fatalf("create = %d", w.Code)
	}
	token, _ := createShare(t, r, "ops1", "", map[string]any{})

	for _, method := range []string{"POST", "PUT", "PATCH", "DELETE"} {
		for _, path := range []string{"/s/" + token, "/s/" + token + "/raw", "/s/" + token + "/raw/0"} {
			w := do(t, r, method, path, map[string]any{"body": "defaced"}, nil)
			if w.Code != http.StatusMethodNotAllowed {
				t.Errorf("%s %s = %d, want 405", method, path, w.Code)
			}
			if allow := w.Header().Get("Allow"); allow != "GET, HEAD" {
				t.Errorf("%s %s Allow = %q, want \"GET, HEAD\"", method, path, allow)
			}
		}
	}
	// And the content is untouched by the attempts above.
	if w := do(t, r, "GET", "/s/"+token+"/raw", nil, nil); w.Body.String() != "x" {
		t.Fatalf("a write-shaped request reached the share's content: %q", w.Body.String())
	}
	// There is no write endpoint under /api/shares at all.
	if w := do(t, r, "POST", "/api/shares/"+token, map[string]any{"body": "x"}, nil); w.Code != http.StatusNotFound {
		t.Errorf("POST /api/shares/<token> = %d, want 404 (no write shape exists)", w.Code)
	}
}

// TestShareGuessingIsThrottledPerTokenAndPerClient mirrors the clip-side guard:
// only failures are charged, and the same limiter keys the share token as well as
// the source address. The per-token bucket is what makes a 6-digit code safe.
func TestShareGuessingIsThrottledPerTokenAndPerClient(t *testing.T) {
	r := testServer(t, func(c *Config) { c.UnlockPerHour = 2 })
	if w := do(t, r, "PUT", "/api/clips/ops1", map[string]any{"body": "x"}, nil); w.Code != http.StatusCreated {
		t.Fatalf("create = %d", w.Code)
	}
	token, _ := createShare(t, r, "ops1", "", map[string]any{"password": "s3cret"})

	// Honest reads never consume the budget, so they are never throttled.
	for i := 0; i < 8; i++ {
		w := do(t, r, "GET", "/s/"+token+"/raw", nil, map[string]string{"X-Clip-Password": "s3cret"})
		if w.Code != http.StatusOK {
			t.Fatalf("honest read %d = %d; a correct share secret must not consume the budget", i, w.Code)
		}
	}

	// Failures are charged until the bucket is empty.
	throttled := false
	for i := 0; i < 8; i++ {
		w := do(t, r, "GET", "/s/"+token+"/raw", nil, map[string]string{"X-Clip-Password": "guess"})
		if w.Code == http.StatusTooManyRequests {
			throttled = true
			if w.Header().Get("Retry-After") == "" {
				t.Error("a 429 came back without Retry-After")
			}
			break
		}
	}
	if !throttled {
		t.Fatal("guessing a share secret was never throttled")
	}
	// A fresh peer is still blocked by the per-TOKEN bucket — that is the half
	// that stops an attacker with many addresses from walking the budget.
	if w := doFrom(t, r, "203.0.113.9", "GET", "/s/"+token+"/raw", "s3cret"); w.Code != http.StatusTooManyRequests {
		t.Errorf("a fresh peer on a hammered share = %d, want 429 (the per-token bucket is missing)", w.Code)
	}
}

// TestShareListIsOwnerOnlyAndHidesHashes pins the shape of the owner's panel data:
// booleans, never a hash.
func TestShareListIsOwnerOnlyAndHidesHashes(t *testing.T) {
	r := testServer(t)
	if w := do(t, r, "PUT", "/api/clips/ops1",
		map[string]any{"body": "x", "password": "master"}, nil); w.Code != http.StatusCreated {
		t.Fatalf("create = %d", w.Code)
	}
	createShare(t, r, "ops1", "master", map[string]any{"password": "share-pw"})
	createShare(t, r, "ops1", "master", map[string]any{"code": true})
	createShare(t, r, "ops1", "master", map[string]any{})

	w := do(t, r, "GET", "/api/clips/ops1/shares", nil, map[string]string{"X-Clip-Password": "master"})
	if w.Code != http.StatusOK {
		t.Fatalf("list = %d (%s)", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if strings.Contains(body, "argon2id") {
		t.Errorf("the share list exposed a stored hash: %s", body)
	}
	if strings.Contains(body, "share-pw") {
		t.Errorf("the share list exposed a plaintext secret: %s", body)
	}
	_, data, _ := envelope(t, w)
	shares, _ := data["shares"].([]any)
	if len(shares) != 3 {
		t.Fatalf("list returned %d shares, want 3", len(shares))
	}
	var sawPassword, sawCode, sawLink bool
	for _, raw := range shares {
		m := raw.(map[string]any)
		sawPassword = sawPassword || m["has_password"] == true
		sawCode = sawCode || m["has_code"] == true
		sawLink = sawLink || (m["has_password"] == false && m["has_code"] == false)
	}
	if !sawPassword || !sawCode || !sawLink {
		t.Errorf("the list did not report all three modes: pw=%v code=%v link=%v", sawPassword, sawCode, sawLink)
	}
}

// ---------------------------------------------------------------------------
// expiry and TTL policy
// ---------------------------------------------------------------------------

// TestPickupCodeTTLCannotBeExtended is the guard for the 10^6-space argument: a
// short window is not a default the caller can opt out of, because enumeration
// becomes feasible the moment the window is long.
func TestPickupCodeTTLCannotBeExtended(t *testing.T) {
	r := testServer(t)
	if w := do(t, r, "PUT", "/api/clips/ops1", map[string]any{"body": "x"}, nil); w.Code != http.StatusCreated {
		t.Fatalf("create = %d", w.Code)
	}
	_, data := createShare(t, r, "ops1", "", map[string]any{"code": true, "expiresInHours": 8760})
	expires, _ := data["expires_at"].(float64)
	if expires == 0 {
		t.Fatal("a code share was allowed to never expire")
	}
	limit := float64(time.Now().Add(31 * time.Minute).UnixMilli())
	if expires > limit {
		t.Fatalf("a code share expires at %.0f, beyond the 30-minute cap", expires)
	}
}

// TestShareNeverOutlivesItsClip: a link advertising a longer life than the content
// underneath it is a link that lies.
func TestShareNeverOutlivesItsClip(t *testing.T) {
	r := testServer(t)
	if w := do(t, r, "PUT", "/api/clips/ops1",
		map[string]any{"body": "x", "expiresInHours": 2}, nil); w.Code != http.StatusCreated {
		t.Fatalf("create = %d", w.Code)
	}
	_, data := createShare(t, r, "ops1", "", map[string]any{"expiresInHours": 24})
	expires, _ := data["expires_at"].(float64)
	if expires > float64(time.Now().Add(3*time.Hour).UnixMilli()) {
		t.Fatalf("a share outlived its clip: expires_at=%.0f", expires)
	}
}

// TestShareSecretModesAreMutuallyExclusive: one share carries one secret, or the
// question "which one did the visitor mean" has no answer.
func TestShareSecretModesAreMutuallyExclusive(t *testing.T) {
	r := testServer(t)
	if w := do(t, r, "PUT", "/api/clips/ops1", map[string]any{"body": "x"}, nil); w.Code != http.StatusCreated {
		t.Fatalf("create = %d", w.Code)
	}
	w := do(t, r, "POST", "/api/clips/ops1/shares",
		map[string]any{"password": "share-pw", "code": true}, nil)
	if w.Code != http.StatusBadRequest {
		t.Errorf("a share with both a password and a code = %d, want 400", w.Code)
	}
}
