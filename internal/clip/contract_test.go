package clip

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
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
// either side is silent: the request succeeds, the browser stores nothing, and
// the user quietly loses write access to the clip it just created. That is
// exactly what happened during development — the server emitted `owner_token`
// (snake_case, per the project's response convention) while the page read
// `ownerToken`.
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
		ExpiresText: "明天",
		OwnerToken:  "secret-token",
	}
	encoded, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}

	// The wire names the client is allowed to depend on.
	for _, key := range []string{
		`"owner_token"`, `"expires_at"`, `"expires_text"`, `"exists"`, `"rev"`, `"body"`, `"assets"`, `"suffix"`,
	} {
		if !bytes.Contains(encoded, []byte(key)) {
			t.Errorf("the server does not emit %s; the client reads it", key)
		}
	}
	// And the camelCase spellings must NOT appear in a response, or a client
	// written against the request convention would half-work.
	for _, wrong := range []string{`"ownerToken"`, `"expiresAt"`, `"rawUrl"`} {
		if bytes.Contains(encoded, []byte(wrong)) {
			t.Errorf("response contains %s; responses are snake_case", wrong)
		}
	}

	page, err := pageFS.ReadFile("page.html")
	if err != nil {
		t.Fatal(err)
	}
	js := string(page)
	for _, key := range []string{"owner_token", "expires_at", "expires_text", "exists", "rev", "assets"} {
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
}

// TestCreateFailureIsVisibleToTheClient guards the other half of that contract:
// if a create succeeds but the response carries no token, the page must say so
// rather than reporting a successful save that left the browser unable to write.
func TestCreateFailureIsVisibleToTheClient(t *testing.T) {
	page, err := pageFS.ReadFile("page.html")
	if err != nil {
		t.Fatal(err)
	}
	js := string(page)
	if !strings.Contains(js, "r.status === 201") {
		t.Fatal("page.html does not special-case a 201 create response")
	}
	if !strings.Contains(js, "没有拿到写权限") {
		t.Error("page.html has no failure message for a token-less 201")
	}
}

// TestUserAgentCannotChooseTheAssetContentType asserts the sniffing rule end to
// end at the HTTP layer: the stored and served type is the sniffed one, and a
// declaration cannot override it.
func TestAssetTypeIsSniffedNotDeclared(t *testing.T) {
	r := testServer(t)
	w := do(t, r, "PUT", "/api/clips/ops1", map[string]any{"body": "x"}, nil)
	_, data, _ := envelope(t, w)
	token := data["owner_token"].(string)

	// A GIF payload declared as image/jpeg: must be stored and served as image/gif.
	gif := append([]byte("GIF89a"), make([]byte, 32)...)
	w = upload(t, r, "ops1", token, map[string][]byte{"mystery.jpg": gif})
	if w.Code != http.StatusOK {
		t.Fatalf("upload = %d (%s)", w.Code, w.Body.String())
	}
	_, data, _ = envelope(t, w)
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
