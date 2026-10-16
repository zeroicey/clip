package clip

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func init() { gin.SetMode(gin.TestMode) }

// testServer builds the whole HTTP surface over a real (test) database.
//
// Nothing here stubs the store: the interesting guarantees of this service are
// enforced in SQL (ownership in the WHERE clause, expiry in the read filter,
// quota under an advisory lock), and a fake store would let all of them pass
// while the real ones were broken.
func testServer(t *testing.T, mutate ...func(*Config)) *gin.Engine {
	t.Helper()
	st := testStore(t, mutate...)
	cfg := st.cfg
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	_, router := NewServer(cfg, st, "test", log)
	return router
}

func do(t *testing.T, r *gin.Engine, method, path string, body any, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	switch v := body.(type) {
	case nil:
	case string:
		reader = strings.NewReader(v)
	default:
		buf, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}
		reader = bytes.NewReader(buf)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != nil {
		if _, ok := headers["Content-Type"]; !ok {
			req.Header.Set("Content-Type", "application/json")
		}
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// envelope decodes the response envelope and fails the test on a malformed body,
// so every test also asserts the envelope shape.
func envelope(t *testing.T, w *httptest.ResponseRecorder) (bool, map[string]any, map[string]any) {
	t.Helper()
	var parsed struct {
		OK        bool           `json:"ok"`
		Data      map[string]any `json:"data"`
		Error     map[string]any `json:"error"`
		RequestID string         `json:"requestId"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &parsed); err != nil {
		t.Fatalf("response is not the JSON envelope: %v\nbody: %s", err, w.Body.String())
	}
	if parsed.RequestID == "" {
		t.Errorf("response envelope has no requestId: %s", w.Body.String())
	}
	return parsed.OK, parsed.Data, parsed.Error
}

// TestCreateWithPasswordIsProtectedFromTheFirstByte pins the reason a password
// may be supplied at creation: creating first and protecting second would leave
// a window in which the content is readable by anyone, while the user believes
// they made it private.
func TestCreateWithPasswordIsProtectedFromTheFirstByte(t *testing.T) {
	r := testServer(t)

	w := do(t, r, "PUT", "/api/clips/ops1",
		map[string]any{"body": "secret plans", "password": "s3cret"}, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d, want 201 (%s)", w.Code, w.Body.String())
	}
	bodyText := w.Body.String()
	_, data, _ := envelope(t, w)
	if data["has_password"] != true {
		t.Fatalf("has_password = %v, want true", data["has_password"])
	}
	// The credential must not be echoed back in any form — not the plaintext, and
	// not the stored hash (which is the thing an offline attack would work on).
	if _, present := data["password"]; present {
		t.Error("the create response echoed the password")
	}
	if strings.Contains(bodyText, "argon2id") {
		t.Error("a response carried the stored password hash")
	}

	// Without the password: refused, and the refusal does not contain the body.
	w = do(t, r, "GET", "/api/clips/ops1", nil, nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("read without a password = %d, want 401 (%s)", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "secret plans") {
		t.Fatal("a refused read leaked the body")
	}
	_, _, errBody := envelope(t, w)
	if errBody["code"] != "password_required" {
		t.Errorf("error code = %v, want password_required", errBody["code"])
	}

	// With it: the content, and still no hash on the wire.
	w = do(t, r, "GET", "/api/clips/ops1", nil, map[string]string{"X-Clip-Password": "s3cret"})
	if w.Code != http.StatusOK {
		t.Fatalf("read with the password = %d (%s)", w.Code, w.Body.String())
	}
	_, data, _ = envelope(t, w)
	if data["body"] != "secret plans" {
		t.Fatalf("body = %v", data["body"])
	}
	if strings.Contains(w.Body.String(), "argon2id") {
		t.Error("an authorised read carried the stored password hash")
	}
}

// TestPublicClipIsOverwrittenByAnyoneAtTheAPI is the inverse of what this
// endpoint used to assert, and it is asserted explicitly because the change was
// deliberate rather than incidental: with no password there is nothing to
// present, so anyone who knows the suffix may write. A regression to the old
// owner-token behaviour would otherwise surface as "saving is broken".
func TestPublicClipIsOverwrittenByAnyoneAtTheAPI(t *testing.T) {
	r := testServer(t)
	if w := do(t, r, "PUT", "/api/clips/ops1", map[string]any{"body": "first"}, nil); w.Code != http.StatusCreated {
		t.Fatalf("create = %d (%s)", w.Code, w.Body.String())
	}

	w := do(t, r, "PUT", "/api/clips/ops1", map[string]any{"body": "second"}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("a credential-free overwrite of a public clip = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	_, data, _ := envelope(t, w)
	if data["body"] != "second" || data["rev"].(float64) != 2 {
		t.Fatalf("after the overwrite: body=%v rev=%v", data["body"], data["rev"])
	}
}

// TestProtectedClipRefusesWritesWithoutThePassword is the boundary that remains
// once a password is set — and it asserts the body did NOT change, because
// "the call failed" and "nothing was written" are different claims.
func TestProtectedClipRefusesWritesWithoutThePassword(t *testing.T) {
	r := testServer(t)
	if w := do(t, r, "PUT", "/api/clips/ops1",
		map[string]any{"body": "original", "password": "s3cret"}, nil); w.Code != http.StatusCreated {
		t.Fatalf("create = %d (%s)", w.Code, w.Body.String())
	}

	for _, hdrs := range []map[string]string{nil, {"X-Clip-Password": "wrong"}} {
		w := do(t, r, "PUT", "/api/clips/ops1", map[string]any{"body": "defaced"}, hdrs)
		if w.Code == http.StatusOK || w.Code == http.StatusCreated {
			t.Fatalf("a write without the password was accepted: %d %s", w.Code, w.Body.String())
		}
	}

	// Nothing was written.
	w := do(t, r, "GET", "/api/clips/ops1", nil, map[string]string{"X-Clip-Password": "s3cret"})
	_, data, _ := envelope(t, w)
	if data["body"] != "original" {
		t.Fatalf("a refused write changed the body: %v", data["body"])
	}

	// With the password it goes through — otherwise the assertions above would
	// pass for the wrong reason.
	w = do(t, r, "PUT", "/api/clips/ops1", map[string]any{"body": "the real update"},
		map[string]string{"X-Clip-Password": "s3cret"})
	if w.Code != http.StatusOK {
		t.Fatalf("write with the password = %d (%s)", w.Code, w.Body.String())
	}
	_, data, _ = envelope(t, w)
	if data["body"] != "the real update" || data["rev"].(float64) != 2 {
		t.Fatalf("after the authorised write: body=%v rev=%v", data["body"], data["rev"])
	}
}

func TestGetUnknownSuffixIsAnEmptyEditorNotAnError(t *testing.T) {
	r := testServer(t)
	w := do(t, r, "GET", "/api/clips/never-existed", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("get unknown = %d, want 200 (the first visit creates the clip)", w.Code)
	}
	okFlag, data, _ := envelope(t, w)
	if !okFlag || data["exists"] != false {
		t.Fatalf("unknown suffix response = ok:%v exists:%v", okFlag, data["exists"])
	}
	if data["body"] != "" {
		t.Errorf("unknown suffix has a body: %v", data["body"])
	}
}

// TestUpdateWithoutATokenIsRefusedAndDoesNotLeak
//
// (The name is kept for continuity with the incidents it documents: an update
// with NO credential used to be refused, because a guessed suffix was readable
// but not writable. Under the model adopted on 2026-10-16 that is no longer
// true for a public clip — see TestPublicClipIsOverwrittenByAnyoneAtTheAPI — and
// the remaining boundary is the password, covered by
// TestProtectedClipRefusesWritesWithoutThePassword.)
func TestUpdateWithoutATokenIsRefusedAndDoesNotLeak(t *testing.T) {
	t.Skip("superseded by TestPublicClipIsOverwrittenByAnyoneAtTheAPI and TestProtectedClipRefusesWritesWithoutThePassword")
}

func TestWriteValidation(t *testing.T) {
	r := testServer(t)

	cases := []struct {
		name   string
		path   string
		body   any
		header map[string]string
		want   int
	}{
		{"uppercase suffix", "/api/clips/Ops1", map[string]any{"body": "x"}, nil, http.StatusBadRequest},
		{"dotted suffix", "/api/clips/a.b", map[string]any{"body": "x"}, nil, http.StatusBadRequest},
		{"reserved suffix", "/api/clips/api", map[string]any{"body": "x"}, nil, http.StatusBadRequest},
		{"reserved suffix takes precedence over api route", "/api/clips/raw", map[string]any{"body": "x"}, nil, http.StatusBadRequest},
		{"unknown field", "/api/clips/ops1", map[string]any{"body": "x", "expires_in_hours": 3}, nil, http.StatusBadRequest},
		{"negative ttl", "/api/clips/ops1", map[string]any{"body": "x", "expiresInHours": -2}, nil, http.StatusBadRequest},
		{"malformed json", "/api/clips/ops1", `{"body":`, nil, http.StatusBadRequest},
		{"text/plain body", "/api/clips/ops1", "body=hi",
			map[string]string{"Content-Type": "text/plain"}, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := do(t, r, "PUT", tc.path, tc.body, tc.header)
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d (%s)", w.Code, tc.want, w.Body.String())
			}
			okFlag, _, _ := envelope(t, w)
			if okFlag {
				t.Fatal("an invalid write reported ok=true")
			}
		})
	}
}

func TestBodyOverTheLimitIsRejected(t *testing.T) {
	r := testServer(t, func(c *Config) { c.MaxBodyBytes = 64 })
	big := strings.Repeat("x", 200)
	w := do(t, r, "PUT", "/api/clips/ops1", map[string]any{"body": big}, nil)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body = %d, want 413 (%s)", w.Code, w.Body.String())
	}
}

func TestWritesCanBeTurnedOff(t *testing.T) {
	r := testServer(t, func(c *Config) { c.WritesEnabled = false })
	w := do(t, r, "PUT", "/api/clips/ops1", map[string]any{"body": "x"}, nil)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("write while disabled = %d, want 503", w.Code)
	}
	_, _, errBody := envelope(t, w)
	if errBody["code"] != "writes_disabled" {
		t.Errorf("code = %v, want writes_disabled", errBody["code"])
	}
	// Reads keep working: turning writes off must not take the content away
	// from whoever is in the middle of a transfer.
	if w := do(t, r, "GET", "/api/clips/ops1", nil, nil); w.Code != http.StatusOK {
		t.Fatalf("read while writes are off = %d, want 200", w.Code)
	}
}

func TestRateLimitAppliesToWritesOnly(t *testing.T) {
	r := testServer(t, func(c *Config) { c.WritePerHour = 1 })

	if w := do(t, r, "PUT", "/api/clips/one", map[string]any{"body": "a"}, nil); w.Code != http.StatusCreated {
		t.Fatalf("first write = %d", w.Code)
	}
	w := do(t, r, "PUT", "/api/clips/two", map[string]any{"body": "b"}, nil)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("second write = %d, want 429", w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Error("a 429 did not carry Retry-After")
	}
	// Reads are not throttled by the write budget.
	if w := do(t, r, "GET", "/api/clips/one", nil, nil); w.Code != http.StatusOK {
		t.Fatalf("read after the write budget was spent = %d, want 200", w.Code)
	}
}

func TestDelete(t *testing.T) {
	r := testServer(t)

	// A public clip: anyone who can reach it may delete it. Same rule as writing
	// to it — with no password there is no credential to withhold.
	if w := do(t, r, "PUT", "/api/clips/ops1", map[string]any{"body": "x"}, nil); w.Code != http.StatusCreated {
		t.Fatalf("create = %d (%s)", w.Code, w.Body.String())
	}
	if w := do(t, r, "DELETE", "/api/clips/ops1", nil, nil); w.Code != http.StatusNoContent {
		t.Fatalf("delete of a public clip = %d, want 204 (%s)", w.Code, w.Body.String())
	}
	_, data, _ := envelope(t, do(t, r, "GET", "/api/clips/ops1", nil, nil))
	if data["exists"] != false {
		t.Fatal("the clip is still there after a successful delete")
	}

	// A protected one: the password is required, and a wrong one is a 404 — not
	// a 403, which would confirm the clip exists to someone who cannot read it.
	if w := do(t, r, "PUT", "/api/clips/ops2",
		map[string]any{"body": "private", "password": "s3cret"}, nil); w.Code != http.StatusCreated {
		t.Fatalf("create protected = %d (%s)", w.Code, w.Body.String())
	}
	for _, hdrs := range []map[string]string{nil, {"X-Clip-Password": "wrong"}} {
		if w := do(t, r, "DELETE", "/api/clips/ops2", nil, hdrs); w.Code != http.StatusUnauthorized && w.Code != http.StatusNotFound {
			t.Fatalf("delete of a protected clip without the password = %d, want 401/404", w.Code)
		}
	}
	if w := do(t, r, "DELETE", "/api/clips/ops2", nil,
		map[string]string{"X-Clip-Password": "s3cret"}); w.Code != http.StatusNoContent {
		t.Fatalf("delete with the password = %d, want 204 (%s)", w.Code, w.Body.String())
	}
}

func TestAssetUploadAndServing(t *testing.T) {
	r := testServer(t)
	if w := do(t, r, "PUT", "/api/clips/ops1", map[string]any{"body": "with a picture"}, nil); w.Code != http.StatusCreated {
		t.Fatalf("create = %d (%s)", w.Code, w.Body.String())
	}

	w := upload(t, r, "ops1", "", map[string][]byte{"shot.png": pngBytes()})
	if w.Code != http.StatusOK {
		t.Fatalf("upload = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	_, data, _ := envelope(t, w)
	assets, _ := data["assets"].([]any)
	if len(assets) != 1 {
		t.Fatalf("assets = %v, want one", data["assets"])
	}

	img := do(t, r, "GET", "/ops1/raw/0", nil, nil)
	if img.Code != http.StatusOK {
		t.Fatalf("raw asset = %d", img.Code)
	}
	if ct := img.Header().Get("Content-Type"); ct != "image/png" {
		t.Errorf("asset content type = %q, want image/png (sniffed, not declared)", ct)
	}
	if img.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("asset response is missing nosniff")
	}
	if !bytes.Equal(img.Body.Bytes(), pngBytes()) {
		t.Error("asset bytes did not round-trip")
	}

	// A position that does not exist is a plain 404, not an empty image.
	if w := do(t, r, "GET", "/ops1/raw/3", nil, nil); w.Code != http.StatusNotFound {
		t.Errorf("missing asset = %d, want 404", w.Code)
	}
}

// TestUploadRejectsNonImages is the stored-XSS guard: HTML with an image name
// must be refused, and the refusal must be based on the bytes rather than on
// anything the client said.
func TestUploadRejectsNonImages(t *testing.T) {
	r := testServer(t)
	if w := do(t, r, "PUT", "/api/clips/ops1", map[string]any{"body": "x"}, nil); w.Code != http.StatusCreated {
		t.Fatalf("create = %d (%s)", w.Code, w.Body.String())
	}

	for name, payload := range map[string][]byte{
		"evil.png":  []byte("<html><script>alert(1)</script></html>"),
		"evil.svg":  []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`),
		"empty.png": {},
	} {
		w := upload(t, r, "ops1", "", map[string][]byte{name: payload})
		if w.Code != http.StatusBadRequest && w.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("uploading %s = %d, want 400 (%s)", name, w.Code, w.Body.String())
		}
	}
	// Nothing was written by any of the refused uploads.
	_, data, _ := envelope(t, do(t, r, "GET", "/api/clips/ops1", nil, nil))
	if assets, ok := data["assets"].([]any); ok && len(assets) != 0 {
		t.Fatalf("a refused upload still stored %d asset(s)", len(assets))
	}
}

// TestUploadRequiresThePassword: the image route is behind the same password as
// the text. A public clip accepts a credential-free upload (there is nothing to
// present), a protected one does not — and the wrong-password case must not
// store anything.
func TestUploadRequiresThePassword(t *testing.T) {
	r := testServer(t)
	if w := do(t, r, "PUT", "/api/clips/ops1",
		map[string]any{"body": "x", "password": "s3cret"}, nil); w.Code != http.StatusCreated {
		t.Fatalf("create = %d (%s)", w.Code, w.Body.String())
	}

	for _, pw := range []string{"wrong", ""} {
		w := upload(t, r, "ops1", pw, map[string][]byte{"shot.png": pngBytes()})
		if w.Code == http.StatusOK {
			t.Fatalf("upload with password %q = %d, want a refusal", pw, w.Code)
		}
	}
	// And nothing was stored by either attempt.
	w := do(t, r, "GET", "/api/clips/ops1", nil, map[string]string{"X-Clip-Password": "s3cret"})
	_, data, _ := envelope(t, w)
	if assets, ok := data["assets"].([]any); ok && len(assets) != 0 {
		t.Fatalf("a refused upload stored %d asset(s)", len(assets))
	}

	// With the password it lands.
	if w := upload(t, r, "ops1", "s3cret", map[string][]byte{"shot.png": pngBytes()}); w.Code != http.StatusOK {
		t.Fatalf("upload with the password = %d, want 200 (%s)", w.Code, w.Body.String())
	}
}

func upload(t *testing.T, r *gin.Engine, suffix, password string, files map[string][]byte) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for name, data := range files {
		fw, err := mw.CreateFormFile("images", name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/api/clips/"+suffix+"/assets", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if password != "" {
		req.Header.Set("X-Clip-Password", password)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestRawTextEndpoint(t *testing.T) {
	r := testServer(t)
	body := "line one\nline two\n$ echo hi\n"
	do(t, r, "PUT", "/api/clips/ops1", map[string]any{"body": body}, nil)

	w := do(t, r, "GET", "/ops1/raw", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("raw = %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("raw content type = %q", ct)
	}
	if w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("raw response is missing nosniff")
	}
	// Byte-for-byte: a transfer buffer that mangles whitespace or adds markup is
	// worse than useless on the receiving machine.
	if w.Body.String() != body {
		t.Fatalf("raw body = %q, want %q", w.Body.String(), body)
	}

	if w := do(t, r, "GET", "/nope/raw", nil, nil); w.Code != http.StatusNotFound {
		t.Errorf("raw for an unknown suffix = %d, want 404", w.Code)
	}
	if w := do(t, r, "GET", "/ops1/nonsense", nil, nil); w.Code != http.StatusNotFound {
		t.Errorf("unknown sub-path = %d, want 404", w.Code)
	}
	if w := do(t, r, "GET", "/a/b/c/d", nil, nil); w.Code != http.StatusNotFound {
		t.Errorf("deep unknown path = %d, want 404", w.Code)
	}
}

// TestPageEscapesTheBody is the XSS guard for the rendered page. The body is
// written by an unauthenticated stranger, so it must be inert in the document.
func TestPageEscapesTheBody(t *testing.T) {
	r := testServer(t)
	payload := `<script>alert(1)</script><img src=x onerror=alert(2)>`
	do(t, r, "PUT", "/api/clips/ops1", map[string]any{"body": payload}, nil)

	w := do(t, r, "GET", "/ops1", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("page = %d", w.Code)
	}
	html := w.Body.String()
	if strings.Contains(html, "<script>alert(1)</script>") {
		t.Fatal("the clip body was rendered as live markup")
	}
	if strings.Contains(html, "onerror=alert(2)>") {
		t.Fatal("the clip body's event handler survived rendering")
	}
	if !strings.Contains(html, "&lt;script&gt;") {
		t.Error("the body was not escaped at all — is it even rendered?")
	}

	csp := w.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "script-src 'nonce-") {
		t.Errorf("page CSP does not use a nonce: %q", csp)
	}
	if strings.Contains(csp, "unsafe-inline") {
		t.Errorf("page CSP allows inline script wholesale: %q", csp)
	}
	// Every nonce in the document must be the one the policy authorises.
	nonce := extractNonce(t, csp)
	if got := strings.Count(html, `nonce="`+nonce+`"`); got < 2 {
		t.Errorf("found %d tags carrying the authorised nonce, want at least 2 (style + script)", got)
	}
	if w.Header().Get("X-Robots-Tag") == "" {
		t.Error("the page is missing X-Robots-Tag: a crawled suffix is a published clip")
	}
	if w.Header().Get("Cache-Control") == "" {
		t.Error("the page is missing Cache-Control: a cached page shows stale content")
	}
}

// TestPublicPagesAreNot404 is the guard for a Gin trap worth its own test:
// engine.NoRoute runs the handler with the response status ALREADY set to 404.
// A handler that renders a real page without resetting the status produces a
// page that looks perfect in a browser and reports 404 to every script, monitor
// and deploy check. The body would pass any assertion that only greps for
// content, so this test asserts the status explicitly.
func TestPublicPagesAreNot404(t *testing.T) {
	r := testServer(t)
	for _, path := range []string{"/", "/somebrandnewsuffix"} {
		w := do(t, r, "GET", path, nil, nil)
		if w.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200 (NoRoute sets 404 before the handler runs)", path, w.Code)
		}
		if strings.HasPrefix(w.Body.String(), "404 page not found") {
			t.Errorf("GET %s returned Gin's default 404 body", path)
		}
	}
}

func TestPageStatesAndLanding(t *testing.T) {
	r := testServer(t)

	// A new suffix is an editor, not a 404.
	w := do(t, r, "GET", "/brandnew", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("new suffix page = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), `data-state="new"`) {
		t.Error("a new suffix was not rendered in the create state")
	}

	// An invalid suffix explains itself rather than 404ing.
	w = do(t, r, "GET", "/Bad_Suffix", nil, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid suffix page = %d, want 400", w.Code)
	}

	// The landing page explains the service for someone handed only the host.
	w = do(t, r, "GET", "/", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("landing = %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "网络剪贴板") {
		t.Error("the landing page does not describe the service")
	}
	if w.Header().Get("Content-Security-Policy") == "" {
		t.Error("the landing page has no CSP")
	}
}

func TestRobotsAndFavicon(t *testing.T) {
	r := testServer(t)
	w := do(t, r, "GET", "/robots.txt", nil, nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Disallow: /") {
		t.Fatalf("robots.txt = %d %q", w.Code, w.Body.String())
	}
	if w := do(t, r, "GET", "/favicon.svg", nil, nil); w.Code != http.StatusOK {
		t.Errorf("favicon = %d", w.Code)
	}
}

func TestHealth(t *testing.T) {
	r := testServer(t)
	do(t, r, "PUT", "/api/clips/ops1", map[string]any{"body": "some bytes"}, nil)

	w := do(t, r, "GET", "/api/health", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("health = %d", w.Code)
	}
	okFlag, data, _ := envelope(t, w)
	if !okFlag {
		t.Fatal("health reported not-ok")
	}
	if data["service"] != "clip" {
		t.Errorf("service = %v", data["service"])
	}
	if data["clips"].(float64) != 1 {
		t.Errorf("clips = %v, want 1", data["clips"])
	}
}

// TestLongPollReturnsImmediatelyWhenBehind proves the long poll does not hold a
// request that already has something to report — otherwise every stale reader
// would wait the full window for no reason.
func TestLongPollReturnsImmediatelyWhenBehind(t *testing.T) {
	r := testServer(t)
	do(t, r, "PUT", "/api/clips/ops1", map[string]any{"body": "first"}, nil)

	start := time.Now()
	w := do(t, r, "GET", "/api/clips/ops1?rev=0&wait=25", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("long poll = %d", w.Code)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("a behind-caller was held for %s", elapsed)
	}
	_, data, _ := envelope(t, w)
	if data["rev"].(float64) != 1 {
		t.Errorf("rev = %v, want 1", data["rev"])
	}
}

func TestLongPollWakesOnWrite(t *testing.T) {
	r := testServer(t)
	if w := do(t, r, "PUT", "/api/clips/ops1", map[string]any{"body": "first"}, nil); w.Code != http.StatusCreated {
		t.Fatalf("create = %d (%s)", w.Code, w.Body.String())
	}

	done := make(chan int64, 1)
	go func() {
		// Already up to date, so this request registers with the hub and waits.
		res := do(t, r, "GET", "/api/clips/ops1?rev=1&wait=25", nil, nil)
		_, d, _ := envelope(t, res)
		rev, _ := d["rev"].(float64)
		done <- int64(rev)
	}()

	// Give the waiter time to register. This is not a substitute for a
	// synchronisation primitive — the assertion below holds either way, because
	// a missed wake-up would still be answered with the current revision on the
	// poll's next iteration — but it makes the wake-up path the likely one.
	time.Sleep(150 * time.Millisecond)
	do(t, r, "PUT", "/api/clips/ops1", map[string]any{"body": "second"}, nil)

	select {
	case rev := <-done:
		if rev != 2 {
			t.Fatalf("woken reader saw rev %d, want 2", rev)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the woken reader never returned")
	}
}

func extractNonce(t *testing.T, csp string) string {
	t.Helper()
	const marker = "script-src 'nonce-"
	i := strings.Index(csp, marker)
	if i < 0 {
		t.Fatalf("no nonce in CSP: %q", csp)
	}
	rest := csp[i+len(marker):]
	j := strings.Index(rest, "'")
	if j < 0 {
		t.Fatalf("malformed nonce in CSP: %q", csp)
	}
	return rest[:j]
}

// TestEveryGetRouteAlsoAnswersHead is the cheap, database-free half of the HEAD
// guard, and the half that runs in the default gate (scripts/check.sh skips every
// test that needs TEST_PG_DSN, and the end-to-end version of this check is one of
// them).
//
// It reads the router's own route table instead of issuing requests, so it needs
// no store: NewServer does not dereference one while building the router. Gin
// matches methods exactly, so a GET route without a HEAD twin is invisible to
// HEAD and the request falls through to NoRoute — which is how
// `curl -sI https://clip.hcyj.xyz/api/health` came to answer 404. The dynamic
// public paths (`/<suffix>`) have no route entry at all and are covered by
// TestHeadMirrorsGetOnEveryRoute instead.
func TestEveryGetRouteAlsoAnswersHead(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &Config{PublicBaseURL: "http://test.local", Environment: "test", WritePerHour: 30}
	_, router := NewServer(cfg, nil, "test", slog.New(slog.NewTextHandler(io.Discard, nil)))

	head := map[string]bool{}
	var gets []string
	for _, route := range router.Routes() {
		if route.Method == http.MethodHead {
			head[route.Path] = true
		}
		if route.Method == http.MethodGet {
			gets = append(gets, route.Path)
		}
	}
	if len(gets) == 0 {
		t.Fatal("the router registered no GET routes; this guard would pass vacuously")
	}
	for _, path := range gets {
		if !head[path] {
			t.Errorf("GET %s has no HEAD twin, so a HEAD probe falls into NoRoute", path)
		}
	}
}
