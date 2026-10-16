package clip

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestFaviconICODoesNotReachTheSuffixHandler guards a route that only exists
// because browsers ask for it implicitly.
//
// Without an explicit /favicon.ico route the request falls through to the
// suffix handler, where a name containing a dot is an invalid suffix — so every
// visit to every clip logged a console error and returned 400 for a request the
// user never made. Found by the real-browser verification, not by any unit test.
func TestFaviconICODoesNotReachTheSuffixHandler(t *testing.T) {
	r := testServer(t)
	w := do(t, r, "GET", "/favicon.ico", nil, nil)
	if w.Code != http.StatusNoContent {
		t.Fatalf("GET /favicon.ico = %d, want 204 (it must not be parsed as a suffix)", w.Code)
	}
}

// TestClientReadsTheFieldNamesTheServerWrites is a cross-language contract test.
//
// The JS in page.html is not type-checked against the Go structs, so a rename on
// either side is silent. The original instance of this bug: the server emitted
// `owner_token` (snake_case, per the project's response convention) while the
// page read `ownerToken`.
//
// So this asserts both halves: the JSON the server actually emits uses these
// names, and the page actually reads those names.
func TestClientReadsTheFieldNamesTheServerWrites(t *testing.T) {
	d := clipData{
		Suffix:      "ops1",
		Exists:      true,
		Rev:         3,
		Body:        "text",
		ExpiresAt:   1_700_000_000_000,
		CreatedAt:   1,
		UpdatedAt:   2,
		Assets:      []assetData{{Position: 0, Mime: "image/png", ByteSize: 4, URL: "u"}},
		URL:         "https://clip.example/ops1",
		RawURL:      "https://clip.example/ops1/raw",
		HasPassword: true,
		Locked:      false,
		ExpiresText: "明天",
	}
	encoded, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}

	// The wire names the client is allowed to depend on.
	for _, key := range []string{
		`"has_password"`, `"locked"`, `"expires_at"`, `"expires_text"`, `"exists"`, `"rev"`, `"body"`, `"assets"`, `"suffix"`,
	} {
		if !bytes.Contains(encoded, []byte(key)) {
			t.Errorf("the server does not emit %s; the client reads it", key)
		}
	}
	// And the camelCase spellings must NOT appear in a response, or a client
	// written against the request convention would half-work.
	for _, wrong := range []string{`"hasPassword"`, `"expiresAt"`, `"rawUrl"`} {
		if bytes.Contains(encoded, []byte(wrong)) {
			t.Errorf("response contains %s; responses are snake_case", wrong)
		}
	}
	// The old credential must be gone from the wire entirely: leaving the field
	// in place while the server no longer accepts it is exactly the kind of
	// half-migration that makes a client believe it holds a credential it does not.
	if bytes.Contains(encoded, []byte("owner_token")) {
		t.Error("responses still carry owner_token; the token model was removed")
	}

	page, err := pageFS.ReadFile("page.html")
	if err != nil {
		t.Fatal(err)
	}
	js := string(page)
	for _, key := range []string{"has_password", "expires_at", "expires_text", "exists", "rev", "assets"} {
		// The page reads either a fetch result (`data.x` / `r.json.data.x`) or a
		// poll result (`d.x`).
		if !strings.Contains(js, "data."+key) && !strings.Contains(js, "d."+key) {
			t.Errorf("page.html never reads %q, but the server emits it — a rename broke the client", key)
		}
	}
	// The camelCase reading that caused the original bug must stay gone.
	if strings.Contains(js, "data.ownerToken") {
		t.Error("page.html reads data.ownerToken, but the server emits owner_token")
	}
	// The password header is a contract too: the page must send the header the
	// server reads, or every read of a protected clip would silently 401.
	if !strings.Contains(js, `"X-Clip-Password"`) {
		t.Error("page.html never sends X-Clip-Password, so a protected clip could never be read")
	}
}

// TestCreateFailureIsVisibleToTheClient guards the other half of that contract:
// if a create succeeds but the response carries no token, the page must say so
// rather than reporting a successful save that left the browser unable to write.
// TestPasswordFailuresAreVisibleToTheClient guards the page half of the password
// contract: every way a password request can fail must produce a message, because
// the failure mode that matters here is a silent one — the user believes they
// protected something and nothing was sent.
func TestPasswordFailuresAreVisibleToTheClient(t *testing.T) {
	page, err := pageFS.ReadFile("page.html")
	if err != nil {
		t.Fatal(err)
	}
	js := string(page)
	// The endpoints the page must call for the feature to exist at all.
	for _, call := range []string{"/unlock", "/password"} {
		if !strings.Contains(js, call) {
			t.Errorf("page.html never calls %s — the password UI is not wired up", call)
		}
	}
	// A wrong password and a throttled attempt are different situations for the
	// person reading the screen, so they must not collapse into one message.
	if !strings.Contains(js, "r.status === 429") {
		t.Error("page.html has no message for a throttled password attempt")
	}
	// The claim this whole feature rests on has to be stated where the user acts,
	// not only in the README: a clip with no password is editable by anyone.
	if !strings.Contains(js, "都能改") && !strings.Contains(js, "password") {
		t.Error("page.html never explains the no-password case")
	}
}

// TestUserAgentCannotChooseTheAssetContentType asserts the sniffing rule end to
// end at the HTTP layer: the stored and served type is the sniffed one, and a
// declaration cannot override it.
func TestAssetTypeIsSniffedNotDeclared(t *testing.T) {
	r := testServer(t)
	w := do(t, r, "PUT", "/api/clips/ops1", map[string]any{"body": "x"}, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d (%s)", w.Code, w.Body.String())
	}

	// A GIF payload declared as image/jpeg: must be stored and served as image/gif.
	gif := append([]byte("GIF89a"), make([]byte, 32)...)
	w = upload(t, r, "ops1", "", map[string][]byte{"mystery.jpg": gif})
	if w.Code != http.StatusOK {
		t.Fatalf("upload = %d (%s)", w.Code, w.Body.String())
	}
	_, data, _ := envelope(t, w)
	assets, _ := data["assets"].([]any)
	if len(assets) != 1 {
		t.Fatalf("assets = %v", data["assets"])
	}
	first := assets[0].(map[string]any)
	if first["mime"] != "image/gif" {
		t.Errorf("stored mime = %v, want image/gif (sniffed, not the declared image/jpeg)", first["mime"])
	}

	got := do(t, r, "GET", "/ops1/raw/0", nil, nil)
	if ct := got.Header().Get("Content-Type"); ct != "image/gif" {
		t.Errorf("served content type = %q, want image/gif", ct)
	}
}

// TestUnknownAPIPathAnswersInTheEnvelope pins the shape of a mistyped endpoint.
//
// The page routes deliberately answer unknown paths as text/plain ("404 not
// found"), because a human typed the URL. The API must do the opposite: every
// /api/* response, including a 404, has to be the documented envelope, or a
// client's error handling has to special-case typos.
//
// This was found by checking the *live* deployment rather than the test server —
// the local run had never asserted it.
func TestUnknownAPIPathAnswersInTheEnvelope(t *testing.T) {
	r := testServer(t)
	for _, path := range []string{"/api/nope", "/api/clips", "/api/clips/ops1/extra"} {
		w := do(t, r, "GET", path, nil, nil)
		if w.Code != http.StatusNotFound && w.Code != http.StatusMethodNotAllowed {
			t.Errorf("GET %s = %d, want 404/405", path, w.Code)
		}
		if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("GET %s answered %q; an unmatched /api path must stay JSON", path, ct)
		}
		okFlag, _, errBody := envelope(t, w)
		if okFlag {
			t.Errorf("GET %s reported ok=true", path)
		}
		if errBody["code"] == nil {
			t.Errorf("GET %s has no error code", path)
		}
	}
}

// TestHeadMirrorsGetOnEveryRoute guards a defect the deploy check itself found.
//
// Gin matches methods exactly: a route registered with GET does NOT answer HEAD.
// A HEAD request therefore matched no route and fell through to the NoRoute
// dispatcher for public paths, which reads the first path segment as a clip
// suffix. That made `curl -sI https://clip.hcyj.xyz/api/health` — the
// "complete public path" step in .ai/runbooks/deploy-clip.md, and one of the
// three checks a fresh session is told to run — answer 404, and answered
// /robots.txt, /favicon.ico and /favicon.svg with 400 "invalid suffix".
//
// The assertion runs against a real net/http server instead of an
// httptest.ResponseRecorder on purpose: RFC 9110 §9.3.2's "same headers as GET,
// but no body" is implemented by net/http discarding the body for HEAD, and a
// ResponseRecorder does not model that — it would record the body and hide the
// very property under test.
func TestHeadMirrorsGetOnEveryRoute(t *testing.T) {
	r := testServer(t)

	// A clip with content, so the page and the raw view are success paths rather
	// than expiry/not-found paths.
	w := do(t, r, "PUT", "/api/clips/ops1", map[string]any{"body": "hello"}, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("setup: create = %d (%s)", w.Code, w.Body.String())
	}

	srv := httptest.NewServer(r)
	defer srv.Close()

	for _, path := range []string{
		"/", "/robots.txt", "/favicon.ico", "/favicon.svg",
		"/ops1", "/ops1/raw", "/brandnew", "/api/health", "/api/nope",
	} {
		get := fetch(t, srv, "GET", path)
		getBody := readBody(t, get)

		head := fetch(t, srv, "HEAD", path)
		headBody := readBody(t, head)

		if head.StatusCode != get.StatusCode {
			t.Errorf("HEAD %s = %d, GET = %d (Gin does not route HEAD to a GET route)",
				path, head.StatusCode, get.StatusCode)
		}
		if got, want := head.Header.Get("Content-Type"), get.Header.Get("Content-Type"); got != want {
			t.Errorf("HEAD %s content-type = %q, GET = %q", path, got, want)
		}
		if len(headBody) != 0 {
			t.Errorf("HEAD %s returned a %d-byte body; HEAD must carry no body", path, len(headBody))
		}
		// Anti-vacuity: on every path but the icon, GET must actually send a body,
		// otherwise "HEAD has no body" would hold for the wrong reason.
		if path != "/favicon.ico" && len(getBody) == 0 {
			t.Errorf("GET %s sent no body, so the HEAD body assertion proves nothing", path)
		}
		// net/http decides Content-Length itself (a body that fits its buffer is
		// measured, a larger one is streamed), so the contract is not a fixed
		// number: HEAD must advertise exactly the header GET advertised, and never
		// a stale or truncated one.
		// The raw header is compared, not resp.ContentLength: for a 204 the client
		// reports 0 for GET and -1 for HEAD although the server sent neither a
		// Content-Length nor a body in both cases — a client-side detail, not a
		// difference in what this service emits.
		if hcl, gcl := head.Header.Get("Content-Length"), get.Header.Get("Content-Length"); hcl != gcl {
			t.Errorf("HEAD %s Content-Length header = %q, GET = %q", path, hcl, gcl)
		}
	}
}

// TestPublicPathsOnlyAnswerReadMethods pins the other half of the same root
// cause: NoRoute runs for every method, so a write-shaped request to a page path
// used to be answered with the page and a 200 — a client that POSTs and gets a
// document has no way to tell it did nothing. Writes live under /api, where the
// owner token is checked in SQL, so the page namespace names the methods it has.
func TestPublicPathsOnlyAnswerReadMethods(t *testing.T) {
	r := testServer(t)
	for _, tc := range []struct{ method, path string }{
		{"POST", "/"}, {"POST", "/ops1"}, {"PUT", "/ops1"},
		{"DELETE", "/ops1"}, {"PATCH", "/brandnew"}, {"OPTIONS", "/ops1"},
	} {
		w := do(t, r, tc.method, tc.path, nil, nil)
		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s = %d, want 405", tc.method, tc.path, w.Code)
		}
		if allow := w.Header().Get("Allow"); allow != "GET, HEAD" {
			t.Errorf("%s %s Allow = %q, want \"GET, HEAD\"", tc.method, tc.path, allow)
		}
	}

	// The /api namespace keeps its own rule: an unmatched path is a 404 in the
	// envelope whatever the method, because no route exists for it at all.
	w := do(t, r, "POST", "/api/nope", nil, nil)
	if w.Code != http.StatusNotFound {
		t.Errorf("POST /api/nope = %d, want 404 (the envelope rule is method-agnostic)", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("POST /api/nope answered %q, want JSON", ct)
	}
}

func fetch(t *testing.T, srv *httptest.Server, method, path string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

func readBody(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", resp.Request.URL.Path, err)
	}
	return body
}

// TestEveryReadPathIsBehindThePassword is the guard that makes the password mean
// something. The service exposes the same content four ways, and the decision
// record says so explicitly: any one of them left open and the password is
// decoration. /raw in particular is the route the README promises to curl, so it
// is the one most likely to be forgotten.
func TestEveryReadPathIsBehindThePassword(t *testing.T) {
	r := testServer(t)
	const secret = "PRIVATE-CONTENT-abc123"

	if w := do(t, r, "PUT", "/api/clips/ops1",
		map[string]any{"body": secret, "password": "s3cret"}, nil); w.Code != http.StatusCreated {
		t.Fatalf("create = %d (%s)", w.Code, w.Body.String())
	}
	// An image too, so /raw/N is a real success path rather than a 404.
	if w := upload(t, r, "ops1", "s3cret", map[string][]byte{"shot.png": pngBytes()}); w.Code != http.StatusOK {
		t.Fatalf("upload = %d (%s)", w.Code, w.Body.String())
	}

	// (The page is checked separately below: it is the one path that answers 200
	// while still withholding the content.)
	for _, path := range []string{"/api/clips/ops1", "/ops1/raw", "/ops1/raw/0"} {
		for _, pw := range []string{"", "wrong"} {
			hdrs := map[string]string{}
			if pw != "" {
				hdrs["X-Clip-Password"] = pw
			}
			w := do(t, r, "GET", path, nil, hdrs)
			if w.Code == http.StatusOK {
				t.Errorf("GET %s with password %q = 200; this read path is not protected", path, pw)
			}
			if strings.Contains(w.Body.String(), secret) {
				t.Errorf("GET %s with password %q leaked the body", path, pw)
			}
		}
		// And with the password it works, so the assertions above are not
		// passing merely because the path is broken.
		w := do(t, r, "GET", path, nil, map[string]string{"X-Clip-Password": "s3cret"})
		if w.Code != http.StatusOK {
			t.Errorf("GET %s with the password = %d, want 200 (%s)", path, w.Code, w.Body.String())
		}
	}

	// The page: 200 (the address is public), but the body is NOT in the HTML and
	// the state says so. Rendering the text and hiding it with CSS would be a
	// lock in name only.
	page := do(t, r, "GET", "/ops1", nil, nil)
	if page.Code != http.StatusOK {
		t.Fatalf("page = %d, want 200 (a locked clip still has a public address)", page.Code)
	}
	html := page.Body.String()
	if strings.Contains(html, secret) {
		t.Error("the locked page carried the clip body in its HTML")
	}
	if !strings.Contains(html, `data-state="locked"`) {
		t.Error("the locked page does not render the locked state")
	}
	// The lock prompt must be there for the client to use, and it must not be a
	// form that would put the password in the URL.
	if !strings.Contains(html, `id="pw"`) {
		t.Error("the locked page has no password input")
	}
	if strings.Contains(html, "?password=") {
		t.Error("the page builds a URL with the password in it")
	}
}

// TestUnlockEndpoints covers the two ways a password is set and removed, and the
// unlock exchange the page uses.
func TestUnlockEndpoints(t *testing.T) {
	r := testServer(t)
	if w := do(t, r, "PUT", "/api/clips/ops1", map[string]any{"body": "notes"}, nil); w.Code != http.StatusCreated {
		t.Fatalf("create = %d (%s)", w.Code, w.Body.String())
	}

	// Unlocking a public clip is a no-op that still answers with the data.
	if w := do(t, r, "POST", "/api/clips/ops1/unlock", map[string]any{"password": ""}, nil); w.Code != http.StatusOK {
		t.Fatalf("unlock of a public clip = %d (%s)", w.Code, w.Body.String())
	}

	// Claim it. No credential is needed: that is the documented "first to set a
	// password owns it" rule.
	w := do(t, r, "PUT", "/api/clips/ops1/password", map[string]any{"password": "s3cret"}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("set password = %d (%s)", w.Code, w.Body.String())
	}
	_, data, _ := envelope(t, w)
	if data["has_password"] != true {
		t.Fatalf("has_password = %v, want true", data["has_password"])
	}

	// Now it needs the password: wrong, right, wrong-then-right via /unlock.
	if w := do(t, r, "POST", "/api/clips/ops1/unlock", map[string]any{"password": "nope"}, nil); w.Code != http.StatusUnauthorized {
		t.Errorf("unlock with a wrong password = %d, want 401", w.Code)
	}
	w = do(t, r, "POST", "/api/clips/ops1/unlock", map[string]any{"password": "s3cret"}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("unlock = %d (%s)", w.Code, w.Body.String())
	}
	_, data, _ = envelope(t, w)
	if data["body"] != "notes" {
		t.Errorf("unlock returned body %v", data["body"])
	}

	// Too short a password is refused with a message about the bound.
	if w := do(t, r, "PUT", "/api/clips/ops1/password", map[string]any{"password": "ab"},
		map[string]string{"X-Clip-Password": "s3cret"}); w.Code != http.StatusBadRequest {
		t.Errorf("a 2-character password = %d, want 400", w.Code)
	}

	// Rotating needs the current one.
	if w := do(t, r, "PUT", "/api/clips/ops1/password", map[string]any{"password": "newpass"}, nil); w.Code == http.StatusOK {
		t.Error("the password was rotated without presenting the old one")
	}
	if w := do(t, r, "PUT", "/api/clips/ops1/password", map[string]any{"password": "newpass"},
		map[string]string{"X-Clip-Password": "s3cret"}); w.Code != http.StatusOK {
		t.Fatalf("rotation with the old password = %d (%s)", w.Code, w.Body.String())
	}
	if w := do(t, r, "GET", "/api/clips/ops1", nil, map[string]string{"X-Clip-Password": "s3cret"}); w.Code == http.StatusOK {
		t.Error("the old password still works after a rotation")
	}

	// Removing it reopens the clip to everyone.
	if w := do(t, r, "DELETE", "/api/clips/ops1/password", nil, nil); w.Code == http.StatusNoContent || w.Code == http.StatusOK {
		t.Error("the password was cleared without presenting it")
	}
	if w := do(t, r, "DELETE", "/api/clips/ops1/password", nil,
		map[string]string{"X-Clip-Password": "newpass"}); w.Code != http.StatusOK {
		t.Fatalf("clear with the password = %d (%s)", w.Code, w.Body.String())
	}
	if w := do(t, r, "GET", "/api/clips/ops1", nil, nil); w.Code != http.StatusOK {
		t.Errorf("the clip is still protected after clearing: %d", w.Code)
	}
}

// TestGuessingIsThrottledButHonestReadsAreNot pins the deliberate asymmetry in
// clipAccess: only FAILURES are charged, so a reader that re-sends its password
// on every long poll never locks itself out, while a guesser runs out of budget.
func TestGuessingIsThrottledButHonestReadsAreNot(t *testing.T) {
	r := testServer(t, func(c *Config) { c.UnlockPerHour = 3 })
	if w := do(t, r, "PUT", "/api/clips/ops1",
		map[string]any{"body": "x", "password": "s3cret"}, nil); w.Code != http.StatusCreated {
		t.Fatalf("create = %d (%s)", w.Code, w.Body.String())
	}

	// Ten correct reads against a budget of three: if a success consumed budget,
	// the fourth would be a 429.
	for i := 0; i < 10; i++ {
		w := do(t, r, "GET", "/api/clips/ops1", nil, map[string]string{"X-Clip-Password": "s3cret"})
		if w.Code != http.StatusOK {
			t.Fatalf("honest read %d = %d; a correct password must not consume the budget", i, w.Code)
		}
	}

	// Failures are charged, and the budget is finite.
	throttled := false
	for i := 0; i < 10; i++ {
		w := do(t, r, "GET", "/api/clips/ops1", nil, map[string]string{"X-Clip-Password": "guess"})
		if w.Code == http.StatusTooManyRequests {
			throttled = true
			if w.Header().Get("Retry-After") == "" {
				t.Error("a 429 came back without Retry-After")
			}
			break
		}
	}
	if !throttled {
		t.Error("guessing was never throttled")
	}
}

// TestUnlockBudgetIsPerSuffixAndPerClient is the guard against the failure mode
// recorded in .ai/pitfalls/cases/2026-10-10-rate-limit-collapsed-to-one-bucket.md:
// a throttle whose key is computed wrongly merges the whole world into one
// bucket, and the failure direction is silent (a 429 looks like a reasonable
// answer).
//
// Two distinct peers are simulated by varying RemoteAddr — which is where a
// direct client's address actually appears. X-Forwarded-For is deliberately NOT
// used for that: clientip.ClientIP ignores it for an untrusted peer, so forging
// it cannot mint a bucket (asserted at the end).
func TestUnlockBudgetIsPerSuffixAndPerClient(t *testing.T) {
	r := testServer(t, func(c *Config) { c.UnlockPerHour = 2 })
	for _, s := range []string{"ops1", "ops2"} {
		if w := do(t, r, "PUT", "/api/clips/"+s,
			map[string]any{"body": "notes", "password": "s3cret"}, nil); w.Code != http.StatusCreated {
			t.Fatalf("create %s = %d (%s)", s, w.Code, w.Body.String())
		}
	}

	// Peer A hammers ops1 with wrong passwords until its budget is gone.
	for i := 0; i < 6; i++ {
		w := doFrom(t, r, "198.51.100.7", "GET", "/api/clips/ops1", "guess")
		if w.Code == http.StatusTooManyRequests {
			break
		}
	}
	// A is now blocked even with the right password, and it stays blocked on a
	// DIFFERENT suffix: the per-peer bucket is global for that peer. That is the
	// intended trade (one noisy client is throttled everywhere), so it is
	// asserted rather than left to surprise someone later.
	if w := doFrom(t, r, "198.51.100.7", "GET", "/api/clips/ops1", "s3cret"); w.Code != http.StatusTooManyRequests {
		t.Errorf("a throttled peer with the correct password = %d, want 429", w.Code)
	}

	// A FRESH peer is still blocked on ops1 by the per-SUFFIX bucket. This is the
	// half that stops an attacker from walking the budget by changing source
	// address: one suffix under attack stays closed to everyone.
	if w := doFrom(t, r, "203.0.113.9", "GET", "/api/clips/ops1", "s3cret"); w.Code != http.StatusTooManyRequests {
		t.Errorf("a fresh peer on a hammered suffix = %d, want 429 (the per-suffix bucket is missing)", w.Code)
	}
	// But that same fresh peer is fine on an untouched suffix, which is what shows
	// the two buckets are genuinely separate rather than one global counter.
	if w := doFrom(t, r, "203.0.113.9", "GET", "/api/clips/ops2", "s3cret"); w.Code != http.StatusOK {
		t.Errorf("a fresh peer on an untouched suffix = %d, want 200 (the buckets are not separate)", w.Code)
	}

	// Forging a forwarding header must not mint a fresh bucket for the throttled
	// peer: per the clientip contract an untrusted peer ignores the header, which
	// is what makes the value usable as a security boundary at all.
	for _, header := range []string{"X-Forwarded-For", "X-Real-Ip", "Cf-Connecting-Ip", "Forwarded"} {
		w := doFromHdr(t, r, "198.51.100.7", "GET", "/api/clips/ops2", "s3cret", header, "198.51.100.123")
		if w.Code != http.StatusTooManyRequests {
			t.Errorf("forging %s minted a fresh bucket: %d, want 429", header, w.Code)
		}
	}
}

// doFrom issues a request that appears to come from a given peer address, with
// the password in the header the service reads.
func doFrom(t *testing.T, r *gin.Engine, ip, method, path, password string) *httptest.ResponseRecorder {
	t.Helper()
	return doFromHdr(t, r, ip, method, path, password, "", "")
}

func doFromHdr(t *testing.T, r *gin.Engine, ip, method, path, password, header, value string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	// The port is part of RemoteAddr on purpose: clientip must strip it, and the
	// upstream really does send "host:port" (see the rate-limit pitfall).
	req.RemoteAddr = ip + ":40000"
	if password != "" {
		req.Header.Set("X-Clip-Password", password)
	}
	if header != "" {
		req.Header.Set(header, value)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}
