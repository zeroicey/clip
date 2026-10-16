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

func TestCreateReturnsTheOwnerTokenExactlyOnce(t *testing.T) {
	r := testServer(t)

	w := do(t, r, "PUT", "/api/clips/ops1", map[string]any{"body": "hello"}, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d, want 201 (%s)", w.Code, w.Body.String())
	}
	okFlag, data, _ := envelope(t, w)
	if !okFlag {
		t.Fatal("create did not report ok")
	}
	token, _ := data["owner_token"].(string)
	if len(token) < 40 {
		t.Fatalf("owner_token = %q, want a 256-bit token", token)
	}
	if data["exists"] != true {
		t.Errorf("create response says exists=%v, want true", data["exists"])
	}

	// Read back: the token must NOT be in a normal read, or the credential
	// would be handed to anyone who can reach the URL.
	w = do(t, r, "GET", "/api/clips/ops1", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("get = %d", w.Code)
	}
	_, data, _ = envelope(t, w)
	if _, present := data["owner_token"]; present {
		t.Fatal("a read response disclosed the owner token")
	}
	if data["body"] != "hello" {
		t.Errorf("body = %v", data["body"])
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

// TestUpdateWithoutATokenIsRefusedAndDoesNotLeak distinguishes the two failure
// modes on purpose: without any token the answer is 409 "taken" (which the
// person needs, to learn the suffix is in use), and with a *wrong* token it is
// 409 as well — but never a silent overwrite.
func TestUpdateWithoutATokenIsRefusedAndDoesNotLeak(t *testing.T) {
	r := testServer(t)
	w := do(t, r, "PUT", "/api/clips/ops1", map[string]any{"body": "first"}, nil)
	_, data, _ := envelope(t, w)
	token := data["owner_token"].(string)

	w = do(t, r, "PUT", "/api/clips/ops1", map[string]any{"body": "overwrite attempt"}, nil)
	if w.Code != http.StatusConflict {
		t.Fatalf("tokenless overwrite = %d, want 409 (%s)", w.Code, w.Body.String())
	}
	_, _, errBody := envelope(t, w)
	if errBody["code"] != "suffix_taken" {
		t.Errorf("error code = %v, want suffix_taken", errBody["code"])
	}

	w = do(t, r, "PUT", "/api/clips/ops1", map[string]any{"body": "wrong token attempt"},
		map[string]string{"X-Relay-Clip-Token": "not-the-token"})
	if w.Code == http.StatusOK || w.Code == http.StatusCreated {
		t.Fatalf("a wrong token was accepted: %d %s", w.Code, w.Body.String())
	}

	// The owner's token still works, and the content is the original.
	w = do(t, r, "PUT", "/api/clips/ops1", map[string]any{"body": "the real update"},
		map[string]string{"X-Relay-Clip-Token": token})
	if w.Code != http.StatusOK {
		t.Fatalf("owner update = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	_, data, _ = envelope(t, w)
	if data["body"] != "the real update" {
		t.Fatalf("body = %v", data["body"])
	}
	if data["rev"].(float64) != 2 {
		t.Errorf("rev = %v, want 2", data["rev"])
	}
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
	w := do(t, r, "PUT", "/api/clips/ops1", map[string]any{"body": "x"}, nil)
	_, data, _ := envelope(t, w)
	token := data["owner_token"].(string)

	if w := do(t, r, "DELETE", "/api/clips/ops1", nil, nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("delete without a token = %d, want 401", w.Code)
	}
	if w := do(t, r, "DELETE", "/api/clips/ops1", nil,
		map[string]string{"X-Relay-Clip-Token": "wrong"}); w.Code != http.StatusNotFound {
		t.Fatalf("delete with a wrong token = %d, want 404", w.Code)
	}
	if w := do(t, r, "DELETE", "/api/clips/ops1", nil,
		map[string]string{"X-Relay-Clip-Token": token}); w.Code != http.StatusNoContent {
		t.Fatalf("delete as the owner = %d, want 204 (%s)", w.Code, w.Body.String())
	}
	_, data, _ = envelope(t, do(t, r, "GET", "/api/clips/ops1", nil, nil))
	if data["exists"] != false {
		t.Fatal("the clip is still there after a successful delete")
	}
}

func TestAssetUploadAndServing(t *testing.T) {
	r := testServer(t)
	w := do(t, r, "PUT", "/api/clips/ops1", map[string]any{"body": "with a picture"}, nil)
	_, data, _ := envelope(t, w)
	token := data["owner_token"].(string)

	w = upload(t, r, "ops1", token, map[string][]byte{"shot.png": pngBytes()})
	if w.Code != http.StatusOK {
		t.Fatalf("upload = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	_, data, _ = envelope(t, w)
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
	w := do(t, r, "PUT", "/api/clips/ops1", map[string]any{"body": "x"}, nil)
	_, data, _ := envelope(t, w)
	token := data["owner_token"].(string)

	for name, payload := range map[string][]byte{
		"evil.png":  []byte("<html><script>alert(1)</script></html>"),
		"evil.svg":  []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`),
		"empty.png": {},
	} {
		w := upload(t, r, "ops1", token, map[string][]byte{name: payload})
		if w.Code != http.StatusBadRequest && w.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("uploading %s = %d, want 400 (%s)", name, w.Code, w.Body.String())
		}
	}
	// Nothing was written by any of the refused uploads.
	_, data, _ = envelope(t, do(t, r, "GET", "/api/clips/ops1", nil, nil))
	if assets, ok := data["assets"].([]any); ok && len(assets) != 0 {
		t.Fatalf("a refused upload still stored %d asset(s)", len(assets))
	}
}

func TestUploadRequiresTheOwnerToken(t *testing.T) {
	r := testServer(t)
	do(t, r, "PUT", "/api/clips/ops1", map[string]any{"body": "x"}, nil)

	w := upload(t, r, "ops1", "wrong-token", map[string][]byte{"shot.png": pngBytes()})
	if w.Code != http.StatusNotFound {
		t.Fatalf("upload with a wrong token = %d, want 404", w.Code)
	}
	w = upload(t, r, "ops1", "", map[string][]byte{"shot.png": pngBytes()})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("upload with no token = %d, want 401", w.Code)
	}
}

func upload(t *testing.T, r *gin.Engine, suffix, token string, files map[string][]byte) *httptest.ResponseRecorder {
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
	if token != "" {
		req.Header.Set("X-Relay-Clip-Token", token)
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
	w := do(t, r, "PUT", "/api/clips/ops1", map[string]any{"body": "first"}, nil)
	_, data, _ := envelope(t, w)
	token := data["owner_token"].(string)

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
	do(t, r, "PUT", "/api/clips/ops1", map[string]any{"body": "second"},
		map[string]string{"X-Relay-Clip-Token": token})

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
