package clip

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/zeroicey/clip/internal/clientip"
	"github.com/zeroicey/clip/internal/ratelimit"
)

// Envelope, identical to the teleport service's:
//
//	{ "ok": true,  "data": {...}, "requestId": "..." }
//	{ "ok": false, "error": { "code": "...", "message": "..." }, "requestId": "..." }
//
// Relay holds more than one service; two of them answering with different shapes
// would be a trap for anything that talks to both. Requests are camelCase and
// responses are snake_case — that asymmetry is the project's existing rule, kept
// here on purpose so it cannot drift into two different rules.

// Server holds the dependencies shared by the handlers.
type Server struct {
	cfg     *Config
	store   *Store
	hub     *hub
	limiter *ratelimit.Limiter
	// unlockLimiter throttles password GUESSES, and only failures are counted
	// (see clipAccess). It is separate from limiter because the two have
	// opposite cost profiles: a write is cheap to reject, while a password check
	// is deliberately expensive (argon2), so an unthrottled guesser is also a CPU
	// and memory amplifier.
	unlockLimiter *ratelimit.Limiter
	log           *slog.Logger
	version       string
	started       time.Time
}

// NewServer builds the handler for the whole clip service.
func NewServer(cfg *Config, st *Store, version string, log *slog.Logger) (*Server, *gin.Engine) {
	s := &Server{
		cfg:     cfg,
		store:   st,
		hub:     newHub(1024),
		limiter: ratelimit.New(cfg.WritePerHour, time.Hour),
		// One limiter, two prefixed key namespaces (see clipAccess): the budget is
		// "per source address AND per suffix". Prefixing is not cosmetic — an IP
		// string and a suffix could otherwise collide into one bucket, which is
		// the failure mode recorded in
		// .ai/pitfalls/cases/2026-10-10-rate-limit-collapsed-to-one-bucket.md.
		unlockLimiter: ratelimit.New(cfg.UnlockPerHour, time.Hour),
		log:           log,
		version:       version,
		started:       time.Now(),
	}
	return s, s.router()
}

func (s *Server) router() *gin.Engine {
	// Gin's own log stream is noise here: in debug mode it dumps the whole route
	// table on every start, and in production the route table is not something an
	// operator reads from journald. The service logs through slog instead, in
	// the same shape as the teleport service, so two services on one host produce
	// comparable lines. Setting the mode from configuration (rather than relying
	// on GIN_MODE being exported) means a deployment cannot forget it.
	if s.cfg.Environment == "production" {
		gin.SetMode(gin.ReleaseMode)
		gin.DefaultWriter = io.Discard
	}

	// gin.New rather than gin.Default: the default logger writes to stdout in
	// Gin's own format, and this service logs through slog so the two services
	// produce comparable lines.
	r := gin.New()
	r.Use(gin.Recovery())
	r.Use(s.requestContext())
	r.Use(s.accessLog())

	// HEAD is registered next to every GET, because Gin matches methods exactly:
	// a route added with GET does NOT answer HEAD, so a HEAD request matched no
	// route at all and fell through to the NoRoute dispatcher for public paths,
	// which reads the first segment as a clip suffix. That turned the deploy check
	// `curl -sI https://clip.hcyj.xyz/api/health` into a 404 and answered
	// /robots.txt, /favicon.ico and /favicon.svg with 400 "invalid suffix" —
	// while `curl -I` is exactly how monitors, CDNs, load balancers and
	// `wget --spider` probe a service, so it reported itself broken to the clients
	// watching it. RFC 9110 §9.3.2 requires HEAD to return the same status and
	// header fields as GET, so no handler below needs a special case: net/http
	// discards the body for HEAD and frames the response exactly as it frames the
	// GET — a measured Content-Length when the body fits its buffer, a streamed
	// response when it does not.
	get := func(relativePath string, handlers ...gin.HandlerFunc) {
		r.GET(relativePath, handlers...)
		r.HEAD(relativePath, handlers...)
	}

	get("/", func(c *gin.Context) { renderLanding(c.Writer) })
	get("/robots.txt", func(c *gin.Context) {
		c.Header("Content-Type", "text/plain; charset=utf-8")
		c.String(http.StatusOK, robotsTXT)
	})
	get("/favicon.svg", func(c *gin.Context) {
		c.Header("Content-Type", "image/svg+xml")
		c.Header("Cache-Control", "public, max-age=86400")
		c.String(http.StatusOK, faviconSVG)
	})
	get("/api/health", s.handleHealth)
	// Browsers request /favicon.ico implicitly on every page. Without this route
	// it falls through to the clip-suffix handler, where a name containing a dot
	// is an invalid suffix — so the browser would get a 400 and log a console
	// error on every visit. A 204 is the accepted "there is no icon" answer and
	// keeps the console clean (the real icon is linked as favicon.svg).
	get("/favicon.ico", func(c *gin.Context) { c.Status(http.StatusNoContent) })

	// Registered explicitly rather than as a wildcard so Gin's radix tree never
	// has to choose between a static and a parameterised first segment. The
	// public surface (`/<suffix>`, `/<suffix>/raw[/n]`) is dispatched from
	// NoRoute below for the same reason — a `/:suffix` route plus static routes
	// like `/api/...` is exactly the shape that panics or misroutes.
	get("/api/clips/:suffix", s.handleGetClip)
	r.PUT("/api/clips/:suffix", s.handlePutClip)
	r.DELETE("/api/clips/:suffix", s.handleDeleteClip)
	r.POST("/api/clips/:suffix/assets", s.handlePutAssets)
	// The password surface. Separate endpoints rather than fields on the write
	// body: setting a password is a distinct authority decision from editing the
	// text, and mixing them would let an edit and a re-key happen in one
	// partially-applied request.
	r.POST("/api/clips/:suffix/unlock", s.handleUnlock)
	r.PUT("/api/clips/:suffix/password", s.handleSetPassword)
	r.DELETE("/api/clips/:suffix/password", s.handleClearPassword)

	// Share management. These live behind the clip's own password because they
	// grant access: creating a link that hands out read access is exactly as
	// sensitive as handing out the master password, and revoking is the owner's
	// way to take it back.
	get("/api/clips/:suffix/shares", s.handleListShares)
	r.POST("/api/clips/:suffix/shares", s.handleCreateShare)
	r.DELETE("/api/clips/:suffix/shares/:token", s.handleDeleteShare)

	// The visitor side of a share. It has its own JSON read (for the locked-shell
	// page, which cannot send the header on the initial navigation) and its own
	// unlock. Note there is no write endpoint anywhere under /api/shares: a share
	// is read-only by construction, not by hiding buttons.
	get("/api/shares/:token", s.handleGetShare)
	r.POST("/api/shares/:token/unlock", s.handleUnlockShare)

	r.NoRoute(s.handlePublicPath)
	return r
}

// requestContext assigns a request id and puts it in the response, so a log line
// and a report from a user can be matched up.
func (s *Server) requestContext() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetHeader("X-Request-Id")
		if id == "" || len(id) > 64 {
			buf := make([]byte, 9)
			_, _ = rand.Read(buf)
			id = base64.RawURLEncoding.EncodeToString(buf)
		}
		c.Set("requestId", id)
		c.Header("X-Request-Id", id)
		c.Next()
	}
}

// accessLog records method, path, status, size and duration — and nothing else.
//
// Deliberately no headers, no query string, no body: the write credential
// travels in X-Relay-Clip-Token, and a log that recorded every header would put
// every clip's owner token on disk. The reverse proxy in front of this service
// has the same constraint, which is why it is written down in the runbook too.
func (s *Server) accessLog() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		s.log.Info("request",
			"method", c.Request.Method,
			"path", c.Request.URL.Path,
			"status", c.Writer.Status(),
			"bytes", c.Writer.Size(),
			"ms", time.Since(start).Milliseconds(),
			"requestId", c.GetString("requestId"),
		)
	}
}

// ---------------------------------------------------------------------------
// envelope helpers
// ---------------------------------------------------------------------------

func ok(c *gin.Context, status int, data any) {
	c.JSON(status, gin.H{"ok": true, "data": data, "requestId": c.GetString("requestId")})
}

func fail(c *gin.Context, status int, code, message string) {
	c.AbortWithStatusJSON(status, gin.H{
		"ok":        false,
		"error":     gin.H{"code": code, "message": message},
		"requestId": c.GetString("requestId"),
	})
}

// failInvalidSuffix reports a suffix the service cannot address.
func failInvalidSuffix(c *gin.Context, err error) {
	fail(c, http.StatusBadRequest, "bad_request", err.Error())
}

// ---------------------------------------------------------------------------
// read
// ---------------------------------------------------------------------------

type assetData struct {
	Position int16  `json:"position"`
	Mime     string `json:"mime"`
	ByteSize int64  `json:"byte_size"`
	URL      string `json:"url"`
}

type clipData struct {
	Suffix    string      `json:"suffix"`
	Exists    bool        `json:"exists"`
	Rev       int64       `json:"rev"`
	Body      string      `json:"body"`
	ExpiresAt int64       `json:"expires_at"`
	CreatedAt int64       `json:"created_at"`
	UpdatedAt int64       `json:"updated_at"`
	Assets    []assetData `json:"assets"`
	URL       string      `json:"url"`
	RawURL    string      `json:"raw_url"`
	// HasPassword drives the page's "设置密码" vs "修改密码" choice, and tells a
	// client that a write will need the password.
	HasPassword bool `json:"has_password"`
	// Locked marks the answer a locked clip gives when no password was supplied:
	// the clip exists, its address is public, and its contents are not in this
	// response. The page renders a prompt instead of an editor.
	Locked      bool   `json:"locked"`
	ExpiresText string `json:"expires_text"`
}

func (s *Server) clipData(clip *Clip) clipData {
	d := clipData{
		Suffix:      clip.Suffix,
		Exists:      true,
		Rev:         clip.Rev,
		Body:        clip.Body,
		URL:         s.cfg.ClipURL(clip.Suffix),
		RawURL:      s.cfg.RawURL(clip.Suffix),
		ExpiresAt:   clip.ExpiresAt,
		CreatedAt:   clip.CreatedAt,
		UpdatedAt:   clip.UpdatedAt,
		ExpiresText: expiresText(clip.ExpiresAt),
		HasPassword: clip.HasPassword(),
		Assets:      make([]assetData, 0, len(clip.Assets)),
	}
	for _, a := range clip.Assets {
		d.Assets = append(d.Assets, assetData{
			Position: a.Position,
			Mime:     a.Mime,
			ByteSize: a.ByteSize,
			URL:      s.rawAssetURL(clip.Suffix, a.Position),
		})
	}
	return d
}

func (s *Server) emptyClipData(suffix string) clipData {
	return clipData{
		Suffix:    suffix,
		Exists:    false,
		Rev:       0,
		Body:      "",
		ExpiresAt: 0,
		Assets:    []assetData{},
		URL:       s.cfg.ClipURL(suffix),
		RawURL:    s.cfg.RawURL(suffix),
	}
}

// lockedClipData is what a locked clip answers when no (or a wrong) password was
// supplied: the fact that it is locked, and nothing else.
//
// No body, no asset list, no revision. The revision is withheld on purpose —
// it is a change counter, and publishing it would let a stranger watch how
// often a private clip is edited.
func (s *Server) lockedClipData(suffix string) clipData {
	return clipData{
		Suffix:      suffix,
		Exists:      true,
		Locked:      true,
		HasPassword: true,
		Assets:      []assetData{},
		URL:         s.cfg.ClipURL(suffix),
		RawURL:      s.cfg.RawURL(suffix),
	}
}

// ---------------------------------------------------------------------------
// access
// ---------------------------------------------------------------------------

// passwordFrom reads the password the caller supplied.
//
// A header, never a query parameter or a path segment: those reach access logs
// (including Caddy's, which records every header unless filtered), browser
// history and Referer headers. The reasoning that keeps the write token out of
// the URL applies to the password for the same reason.
func passwordFrom(c *gin.Context) string {
	return c.GetHeader("X-Clip-Password")
}

// clipAccess decides whether this request may see (and write) the clip, and
// returns the proof that write statements require.
//
// The model, per .ai/decisions/2026-10-16-clip-open-edit-and-ownership.md: a
// clip with no password is public — anyone may read and write it — and a clip
// with a password is readable and writable only by someone who knows it. There
// is no third credential; the owner token this service used to mint is gone.
//
// `proof` is the STORED HASH, not the password: it is what the write queries
// compare against (see queries/clips.sql). Returning it lets a caller verify
// once and then write, while a password change in between still invalidates the
// write.
//
// When access is denied this writes the response itself, so callers only have
// to check the boolean.
// passwordDecision is the single verification path, shared by the JSON API, the
// raw views and the unlock endpoint. It returns either the proof a write needs,
// or the refusal as (status, code, message) so each caller can shape it for its
// own audience — the /api namespace answers in the envelope, while a page path
// answers text/plain (/raw is read by curl).
//
// status == 0 means allowed.
func (s *Server) passwordDecision(c *gin.Context, clip *Clip, password string) (string, int, string, string) {
	if clip == nil || !clip.HasPassword() {
		return "", 0, "", ""
	}

	// Two namespaces, so the budget is per source address AND per suffix. Either
	// alone is insufficient: per-IP lets an attacker with many addresses hammer
	// one suffix, and per-suffix lets one address spray every suffix it can
	// guess.
	ipKey := "ip:" + clientip.ClientIP(c.Request)
	suffixKey := "suffix:" + clip.Suffix

	// Checked BEFORE verifying, because verification is the expensive part
	// (argon2 at 19 MiB). An unthrottled guesser is a memory/CPU amplifier, not
	// just a confidentiality problem.
	if s.unlockLimiter.Blocked(ipKey) || s.unlockLimiter.Blocked(suffixKey) {
		c.Header("Retry-After", strconv.Itoa(s.unlockLimiter.RetryAfter()))
		return "", http.StatusTooManyRequests, "rate_limited", "密码试得太频繁了，稍后再试。"
	}

	if password == "" {
		return "", http.StatusUnauthorized, "password_required", "这个剪贴板需要密码。"
	}
	if !verifyPassword(*clip.PasswordHash, password) {
		// Only failures consume budget. A correct password never exhausts
		// anything, so a reader may poll — the long-poll re-sends the password on
		// every request — as often as it likes while a guesser is throttled.
		s.unlockLimiter.Allow(ipKey)
		s.unlockLimiter.Allow(suffixKey)
		return "", http.StatusUnauthorized, "password_incorrect", "密码不对。"
	}
	return *clip.PasswordHash, 0, "", ""
}

// clipAccess is passwordDecision for the JSON API: refusals come back in the
// documented envelope.
func (s *Server) clipAccess(c *gin.Context, clip *Clip, password string) (string, bool) {
	proof, status, code, msg := s.passwordDecision(c, clip, password)
	if status != 0 {
		fail(c, status, code, msg)
		return "", false
	}
	return proof, true
}

// rawAccess is passwordDecision for the page/raw namespace, which answers in
// text/plain — the same shape as its 404s, so a script parsing these routes
// never has to switch parsers on the status code.
func (s *Server) rawAccess(c *gin.Context, clip *Clip, password string) (string, bool) {
	proof, status, code, msg := s.passwordDecision(c, clip, password)
	if status != 0 {
		c.Header("Content-Type", "text/plain; charset=utf-8")
		c.Header("Cache-Control", "no-store")
		if status == http.StatusUnauthorized {
			msg += "用 curl 的话：加 -H 'X-Clip-Password: <密码>'。"
		}
		_ = code // the text form carries no machine-readable code
		c.String(status, msg+"\n")
		return "", false
	}
	return proof, true
}

func (s *Server) rawAssetURL(suffix string, position int16) string {
	return fmt.Sprintf("%s/%s/raw/%d", s.cfg.PublicBaseURL, suffix, position)
}

// handleGetClip returns a clip, or a well-formed "nothing here yet" answer.
//
// A missing suffix is 200 with `exists:false` rather than 404. Two reasons: the
// page is expected to be a 200 editor the first time a suffix is opened, and a
// 404-versus-200 difference would let anyone probe which short suffixes are
// taken without reading them. The response body is identical in shape either
// way, so nothing is leaked by not distinguishing them.
//
// `?wait=N` turns the call into a long poll: if the caller already has the
// current revision, the request is held (up to N seconds, capped) until the clip
// changes.
func (s *Server) handleGetClip(c *gin.Context) {
	suffix := c.Param("suffix")
	if err := ValidateSuffix(suffix); err != nil {
		failInvalidSuffix(c, err)
		return
	}

	rev := int64(-1)
	if raw := c.Query("rev"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < 0 {
			fail(c, http.StatusBadRequest, "bad_request", "rev must be a non-negative integer")
			return
		}
		rev = n
	}

	wait := 0
	if raw := c.Query("wait"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			fail(c, http.StatusBadRequest, "bad_request", "wait must be a non-negative number of seconds")
			return
		}
		wait = n
	}

	clip, err := s.store.Get(c.Request.Context(), suffix)
	if err != nil && !errors.Is(err, ErrNotFound) {
		s.log.Error("get clip", "suffix", suffix, "error", err, "requestId", c.GetString("requestId"))
		fail(c, http.StatusInternalServerError, "internal_error", "could not read the clip")
		return
	}

	// A locked clip refuses here, before any waiting: an unauthorised caller
	// must not be able to hold a long-poll connection open (or to learn, from
	// the timing of an empty answer, that the clip changed).
	if _, allowed := s.clipAccess(c, clip, passwordFrom(c)); !allowed {
		return
	}

	// Long-poll only when the caller is already up to date. If they are behind,
	// answering immediately is both correct and faster than waiting.
	if wait > 0 && rev >= 0 && (clip == nil || clip.Rev == rev) {
		window := time.Duration(wait) * time.Second
		if window > longPollWindow {
			window = longPollWindow
		}
		if ch, release, okWait := s.hub.subscribe(suffix); okWait {
			timer := time.NewTimer(window)
			select {
			case <-ch:
			case <-timer.C:
			case <-c.Request.Context().Done():
			}
			timer.Stop()
			// Release on every exit path: a leaked waiter means the next publish
			// closes a channel nobody is reading and the hub grows forever.
			release()
			clip, err = s.store.Get(c.Request.Context(), suffix)
			if err != nil && !errors.Is(err, ErrNotFound) {
				s.log.Error("re-read clip after wait", "suffix", suffix, "error", err)
				fail(c, http.StatusInternalServerError, "internal_error", "could not read the clip")
				return
			}
			// Re-checked rather than trusted from before the wait: the password can
			// be changed (or set) while this request is parked, and the reader would
			// otherwise be handed the new content on the strength of an old check.
			if _, allowed := s.clipAccess(c, clip, passwordFrom(c)); !allowed {
				return
			}
		}
	}

	c.Header("Cache-Control", "no-store")
	if clip == nil {
		ok(c, http.StatusOK, s.emptyClipData(suffix))
		return
	}
	ok(c, http.StatusOK, s.clipData(clip))
}

// ---------------------------------------------------------------------------
// write
// ---------------------------------------------------------------------------

// putRequest is the JSON body for a write. Camel-case on the way in, per the
// project's convention.
type putRequest struct {
	Body string `json:"body"`
	// ExpiresInHours is a pointer so that "absent" and "0" mean different
	// things: absent = the default TTL, 0 = never expires. Only a human can
	// choose "forever", and only explicitly.
	ExpiresInHours *float64 `json:"expiresInHours"`
	// Password, when present, protects the clip AT CREATION. It is rejected on a
	// write to an existing clip, where changing the password goes through
	// PUT /api/clips/<suffix>/password instead — see the check in handlePutClip.
	Password *string `json:"password"`
}

func (s *Server) handlePutClip(c *gin.Context) {
	suffix := c.Param("suffix")
	if err := ValidateSuffix(suffix); err != nil {
		failInvalidSuffix(c, err)
		return
	}
	if !s.cfg.WritesEnabled {
		fail(c, http.StatusServiceUnavailable, "writes_disabled",
			"这个服务当前只读（管理员关闭了写入）。")
		return
	}
	if !s.limiter.Allow(clientip.ClientIP(c.Request)) {
		c.Header("Retry-After", strconv.Itoa(s.limiter.RetryAfter()))
		fail(c, http.StatusTooManyRequests, "rate_limited", "写得太频繁了，稍后再试。")
		return
	}

	// Bound the body before parsing: without this a single request could read
	// gigabytes into memory, and the size limit would only be discovered after
	// the allocation.
	body, err := readJSONBody(c, s.cfg.MaxBodyBytes)
	if err != nil {
		s.writeReadError(c, err)
		return
	}

	ctx := c.Request.Context()

	// A negative TTL would produce a clip that is born expired, which is never
	// what someone means; treating it as "never expires" (the <=0 branch) would
	// be the worst possible reading of it.
	if body.ExpiresInHours != nil && *body.ExpiresInHours < 0 {
		fail(c, http.StatusBadRequest, "bad_request", "expiresInHours 不能为负数（0 表示永不过期）。")
		return
	}

	// Read what is stored first: whether this write needs a password depends on
	// the clip's own state, which the caller cannot know without reading it.
	existing, err := s.store.Get(ctx, suffix)
	if err != nil && !errors.Is(err, ErrNotFound) {
		s.log.Error("get clip for write", "suffix", suffix, "error", err)
		fail(c, http.StatusInternalServerError, "internal_error", "could not read the clip")
		return
	}
	proof, allowed := s.clipAccess(c, existing, passwordFrom(c))
	if !allowed {
		return
	}

	if existing != nil {
		if body.Password != nil {
			// Changing the password is not folded into a body write on purpose:
			// one partially-applied request could otherwise both replace the text
			// and re-key the clip, and the caller could not tell which half
			// happened.
			fail(c, http.StatusBadRequest, "bad_request",
				"改密码请用 PUT /api/clips/<后缀>/password，不要放在正文里。")
			return
		}
		expiresAt := int64(-1) // keep whatever the clip already has
		if body.ExpiresInHours != nil {
			expiresAt = expiryFromHours(*body.ExpiresInHours)
		}
		if _, err := s.store.Update(ctx, suffix, proof, body.Body, expiresAt); err != nil {
			s.writeStoreError(c, err)
			return
		}
		s.hub.publish(suffix)
		clip, gerr := s.store.Get(ctx, suffix)
		if gerr != nil {
			fail(c, http.StatusInternalServerError, "internal_error", "saved, but could not read it back")
			return
		}
		ok(c, http.StatusOK, s.clipData(clip))
		return
	}

	// Create. A new clip is public unless this very request asks for a password,
	// in which case the hash is inserted with the row: creating the clip and then
	// protecting it in a second request would leave a window where the content is
	// readable by anyone while the user believes they made it private.
	expiresAt := expiresAtFromDefault(s.cfg)
	if body.ExpiresInHours != nil {
		expiresAt = expiryFromHours(*body.ExpiresInHours)
	}
	var newHash *string
	if body.Password != nil {
		if err := CheckPassword(*body.Password); err != nil {
			fail(c, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		h, herr := hashPassword(*body.Password)
		if herr != nil {
			s.log.Error("hash password", "error", herr)
			fail(c, http.StatusInternalServerError, "internal_error", "密码处理失败。")
			return
		}
		newHash = &h
	}
	if _, err := s.store.Create(ctx, suffix, body.Body, expiresAt, newHash); err != nil {
		if errors.Is(err, ErrTaken) {
			fail(c, http.StatusConflict, "suffix_taken",
				fmt.Sprintf("后缀 %q 已经被占用了，换一个。", suffix))
			return
		}
		s.writeStoreError(c, err)
		return
	}
	s.hub.publish(suffix)

	clip, err := s.store.Get(ctx, suffix)
	if err != nil {
		fail(c, http.StatusInternalServerError, "internal_error", "created, but could not read it back")
		return
	}
	ok(c, http.StatusCreated, s.clipData(clip))
}

func (s *Server) handleDeleteClip(c *gin.Context) {
	suffix := c.Param("suffix")
	if err := ValidateSuffix(suffix); err != nil {
		failInvalidSuffix(c, err)
		return
	}
	if !s.cfg.WritesEnabled {
		fail(c, http.StatusServiceUnavailable, "writes_disabled", "这个服务当前只读。")
		return
	}
	ctx := c.Request.Context()
	// Read before deleting: a protected clip must not be deletable by someone
	// who cannot read it. There is no separate delete credential — the password
	// is the only one, and it guards removal exactly as it guards a rewrite.
	existing, err := s.store.Get(ctx, suffix)
	if err != nil && !errors.Is(err, ErrNotFound) {
		s.log.Error("get clip for delete", "suffix", suffix, "error", err)
		fail(c, http.StatusInternalServerError, "internal_error", "could not read the clip")
		return
	}
	if existing == nil {
		fail(c, http.StatusNotFound, "not_found", "没有这份剪贴板。")
		return
	}
	proof, allowed := s.clipAccess(c, existing, passwordFrom(c))
	if !allowed {
		return
	}
	deleted, err := s.store.Delete(ctx, suffix, proof)
	if err != nil {
		s.writeStoreError(c, err)
		return
	}
	if !deleted {
		fail(c, http.StatusNotFound, "not_found", "没有这份剪贴板。")
		return
	}
	s.hub.publish(suffix)
	c.Status(http.StatusNoContent)
}

// allowedImageTypes is the complete set of media types an asset may have.
//
// Sampled from the bytes, never taken from the request: a client-declared type
// is attacker-chosen, and a `<script>`/HTML body declared as an image is the
// classic way to turn an upload endpoint into stored XSS. SVG is deliberately
// absent — it is a scriptable document, and this service has no way to render it
// safely.
var allowedImageTypes = map[string]string{
	"image/png":  ".png",
	"image/jpeg": ".jpg",
	"image/gif":  ".gif",
	"image/webp": ".webp",
}

func (s *Server) handlePutAssets(c *gin.Context) {
	suffix := c.Param("suffix")
	if err := ValidateSuffix(suffix); err != nil {
		failInvalidSuffix(c, err)
		return
	}
	if !s.cfg.WritesEnabled {
		fail(c, http.StatusServiceUnavailable, "writes_disabled", "这个服务当前只读。")
		return
	}
	if !s.limiter.Allow(clientip.ClientIP(c.Request)) {
		c.Header("Retry-After", strconv.Itoa(s.limiter.RetryAfter()))
		fail(c, http.StatusTooManyRequests, "rate_limited", "写得太频繁了，稍后再试。")
		return
	}

	// The asset route is behind the same password as the text: images are
	// content, and a password that guarded only the body would leave the
	// screenshots readable.
	assetClip, err := s.store.Get(c.Request.Context(), suffix)
	if err != nil && !errors.Is(err, ErrNotFound) {
		s.log.Error("get clip for assets", "suffix", suffix, "error", err)
		fail(c, http.StatusInternalServerError, "internal_error", "could not read the clip")
		return
	}
	if assetClip == nil {
		fail(c, http.StatusNotFound, "not_found", "没有这份剪贴板。")
		return
	}
	assetProof, assetAllowed := s.clipAccess(c, assetClip, passwordFrom(c))
	if !assetAllowed {
		return
	}

	// Cap the whole request before parsing: multipart parsing writes to memory
	// and then to temp files, so the limit has to be applied to the reader, not
	// to the files after they have been buffered.
	maxTotal := s.cfg.MaxAssetBytes*int64(s.cfg.MaxAssets) + 1<<20
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxTotal)

	form, err := c.MultipartForm()
	if err != nil {
		if strings.Contains(err.Error(), "request body too large") {
			fail(c, http.StatusRequestEntityTooLarge, "payload_too_large",
				fmt.Sprintf("图片总量超过上限（单张 %s，最多 %d 张）。",
					humanBytes(s.cfg.MaxAssetBytes), s.cfg.MaxAssets))
			return
		}
		fail(c, http.StatusBadRequest, "bad_request", "请求不是合法的 multipart/form-data："+err.Error())
		return
	}
	files := form.File["images"]
	if len(files) == 0 {
		fail(c, http.StatusBadRequest, "bad_request", "没有找到名为 images 的文件字段。")
		return
	}
	if len(files) > s.cfg.MaxAssets {
		fail(c, http.StatusRequestEntityTooLarge, "payload_too_large",
			fmt.Sprintf("最多 %d 张图片（收到 %d 张）。", s.cfg.MaxAssets, len(files)))
		return
	}

	assets := make([]Asset, 0, len(files))
	for i, fh := range files {
		mime, data, err := readImage(fh, s.cfg.MaxAssetBytes)
		if err != nil {
			var tooLarge *tooLargeError
			if errors.As(err, &tooLarge) {
				fail(c, http.StatusRequestEntityTooLarge, "payload_too_large", tooLarge.Error())
				return
			}
			var badType *unsupportedTypeError
			if errors.As(err, &badType) {
				fail(c, http.StatusBadRequest, "bad_request", badType.Error())
				return
			}
			s.log.Error("read upload", "error", err)
			fail(c, http.StatusBadRequest, "bad_request", "图片读取失败。")
			return
		}
		assets = append(assets, Asset{Position: int16(i), Mime: mime, Bytes: data})
	}

	if _, err := s.store.SetAssets(c.Request.Context(), suffix, assetProof, assets); err != nil {
		if errors.Is(err, ErrNotFound) {
			fail(c, http.StatusNotFound, "not_found", "没有这份剪贴板，或者密码不对。")
			return
		}
		if errors.Is(err, ErrQuota) {
			fail(c, http.StatusServiceUnavailable, "quota_exceeded",
				"这个服务的总容量已经用满了，暂时不能上传。")
			return
		}
		s.writeStoreError(c, err)
		return
	}
	s.hub.publish(suffix)

	clip, err := s.store.Get(c.Request.Context(), suffix)
	if err != nil {
		fail(c, http.StatusInternalServerError, "internal_error", "上传成功，但没能读回来。")
		return
	}
	ok(c, http.StatusOK, s.clipData(clip))
}

// ---------------------------------------------------------------------------
// password
// ---------------------------------------------------------------------------

// maxPasswordBytes bounds the password request bodies. Generous next to the 128
// character limit (which is counted in runes, so up to 512 bytes) and small
// enough that the parse cannot be used to allocate.
const maxPasswordBytes = 4 << 10

// passwordRequest is the body of the unlock and set-password endpoints.
// Camel-case on the way in, per the project's convention.
type passwordRequest struct {
	Password string `json:"password"`
}

// handleUnlock exchanges a password for the clip's contents.
//
// The page cannot obtain them any other way: a locked clip is rendered as a
// shell with no body in it, precisely so the HTML never carries protected text
// into a browser that has not proved it may see it.
//
// This is also the only place a password travels in a request body; everywhere
// else it is the X-Clip-Password header. Neither is a URL, so it never reaches
// an access log, browser history or a Referer.
func (s *Server) handleUnlock(c *gin.Context) {
	suffix := c.Param("suffix")
	if err := ValidateSuffix(suffix); err != nil {
		failInvalidSuffix(c, err)
		return
	}
	body := passwordRequest{}
	if err := readJSONInto(c, maxPasswordBytes, &body); err != nil {
		s.writeReadError(c, err)
		return
	}

	clip, err := s.store.Get(c.Request.Context(), suffix)
	if err != nil && !errors.Is(err, ErrNotFound) {
		s.log.Error("get clip for unlock", "suffix", suffix, "error", err)
		fail(c, http.StatusInternalServerError, "internal_error", "could not read the clip")
		return
	}
	if clip == nil {
		// 404, not 401: no password would help, because there is nothing there.
		// This does tell the caller the suffix is absent. That is a deliberate
		// trade — the page has to reveal "exists and is locked" in order to show
		// a prompt at all, so pretending otherwise here would only make the API
		// harder to use without making anything secret (see the decision record).
		fail(c, http.StatusNotFound, "not_found", "没有这份剪贴板。")
		return
	}
	if _, allowed := s.clipAccess(c, clip, body.Password); !allowed {
		return
	}
	c.Header("Cache-Control", "no-store")
	ok(c, http.StatusOK, s.clipData(clip))
}

// handleSetPassword sets a password on a public clip, or replaces the password
// on a protected one.
//
// Setting one on a public clip is the "claim": there is no account and no
// creator record, so the password IS the ownership claim. That means anyone who
// can reach a public suffix may claim it, including someone who did not write
// its contents — accepted explicitly in
// .ai/decisions/2026-10-16-clip-open-edit-and-ownership.md, and the reason the
// page tells a user to set a password on anything they care about.
func (s *Server) handleSetPassword(c *gin.Context) {
	suffix := c.Param("suffix")
	if err := ValidateSuffix(suffix); err != nil {
		failInvalidSuffix(c, err)
		return
	}
	if !s.cfg.WritesEnabled {
		fail(c, http.StatusServiceUnavailable, "writes_disabled", "这个服务当前只读。")
		return
	}
	if !s.limiter.Allow(clientip.ClientIP(c.Request)) {
		c.Header("Retry-After", strconv.Itoa(s.limiter.RetryAfter()))
		fail(c, http.StatusTooManyRequests, "rate_limited", "写得太频繁了，稍后再试。")
		return
	}
	body := passwordRequest{}
	if err := readJSONInto(c, maxPasswordBytes, &body); err != nil {
		s.writeReadError(c, err)
		return
	}
	if err := CheckPassword(body.Password); err != nil {
		fail(c, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	ctx := c.Request.Context()
	existing, err := s.store.Get(ctx, suffix)
	if err != nil && !errors.Is(err, ErrNotFound) {
		s.log.Error("get clip for set password", "suffix", suffix, "error", err)
		fail(c, http.StatusInternalServerError, "internal_error", "could not read the clip")
		return
	}
	if existing == nil {
		// A password belongs to a clip; there is nothing to protect yet.
		fail(c, http.StatusNotFound, "not_found", "没有这份剪贴板（先写下内容创建它，再设密码）。")
		return
	}
	// A public clip needs no credential (this is the claim); a protected one
	// requires the current password before it can be re-keyed.
	proof, allowed := s.clipAccess(c, existing, passwordFrom(c))
	if !allowed {
		return
	}

	hash, err := hashPassword(body.Password)
	if err != nil {
		s.log.Error("hash password", "error", err)
		fail(c, http.StatusInternalServerError, "internal_error", "密码处理失败。")
		return
	}
	if err := s.store.SetPassword(ctx, suffix, proof, hash); err != nil {
		if errors.Is(err, ErrNotFound) {
			fail(c, http.StatusNotFound, "not_found", "没有这份剪贴板，或者密码不对。")
			return
		}
		s.writeStoreError(c, err)
		return
	}

	clip, err := s.store.Get(ctx, suffix)
	if err != nil {
		fail(c, http.StatusInternalServerError, "internal_error", "saved, but could not read it back")
		return
	}
	s.hub.publish(suffix)
	c.Header("Cache-Control", "no-store")
	ok(c, http.StatusOK, s.clipData(clip))
}

// handleClearPassword removes the password, making the clip public again.
//
// The current password is required (it arrives in X-Clip-Password, like every
// other authenticated request). "Forgot the password" is not recoverable and
// this endpoint deliberately cannot be used to recover one: there is no
// administrator and no reset flow, so the only ways out of a forgotten password
// are to let the clip expire or to take a different suffix.
func (s *Server) handleClearPassword(c *gin.Context) {
	suffix := c.Param("suffix")
	if err := ValidateSuffix(suffix); err != nil {
		failInvalidSuffix(c, err)
		return
	}
	if !s.cfg.WritesEnabled {
		fail(c, http.StatusServiceUnavailable, "writes_disabled", "这个服务当前只读。")
		return
	}
	if !s.limiter.Allow(clientip.ClientIP(c.Request)) {
		c.Header("Retry-After", strconv.Itoa(s.limiter.RetryAfter()))
		fail(c, http.StatusTooManyRequests, "rate_limited", "写得太频繁了，稍后再试。")
		return
	}

	ctx := c.Request.Context()
	existing, err := s.store.Get(ctx, suffix)
	if err != nil && !errors.Is(err, ErrNotFound) {
		s.log.Error("get clip for clear password", "suffix", suffix, "error", err)
		fail(c, http.StatusInternalServerError, "internal_error", "could not read the clip")
		return
	}
	if existing == nil {
		fail(c, http.StatusNotFound, "not_found", "没有这份剪贴板。")
		return
	}
	proof, allowed := s.clipAccess(c, existing, passwordFrom(c))
	if !allowed {
		return
	}
	if err := s.store.ClearPassword(ctx, suffix, proof); err != nil {
		if errors.Is(err, ErrNotFound) {
			fail(c, http.StatusNotFound, "not_found", "没有这份剪贴板，或者密码不对。")
			return
		}
		s.writeStoreError(c, err)
		return
	}

	clip, err := s.store.Get(ctx, suffix)
	if err != nil {
		fail(c, http.StatusInternalServerError, "internal_error", "saved, but could not read it back")
		return
	}
	s.hub.publish(suffix)
	c.Header("Cache-Control", "no-store")
	ok(c, http.StatusOK, s.clipData(clip))
}

// ---------------------------------------------------------------------------
// public path dispatch
// ---------------------------------------------------------------------------

// handlePublicPath serves everything that is not under /api: the clipboard page
// and its raw views.
//
// It is a NoRoute handler rather than a route pattern because the first path
// segment is user-chosen: `/<suffix>`. Registering that as a router parameter
// alongside static siblings (`/api/...`, `/robots.txt`) puts Gin's radix tree in
// the one configuration it handles poorly. Dispatching here keeps the rule
// explicit and in one readable place — including which shapes are *not* a clip.
func (s *Server) handlePublicPath(c *gin.Context) {
	path := strings.Trim(c.Request.URL.Path, "/")
	parts := strings.Split(path, "/")

	// Anything unmatched under /api is answered in the JSON envelope, never as
	// text/plain. A client that mistypes an endpoint should get the same shape it
	// gets from a real one, or its error handling has to special-case the typo —
	// and the text/plain 404 the page routes use would look like a proxy error.
	// (Found on the live deployment, where the envelope's absence was the only
	// contract break in an otherwise green run.)
	//
	// This branch is deliberately method-agnostic and comes first: no route exists
	// for an unknown /api path under ANY method, so the honest answer is 404 in the
	// envelope shaped like every other API answer.
	if parts[0] == "api" {
		fail(c, http.StatusNotFound, "not_found", "未知的接口路径。")
		return
	}

	// The public namespace is a read view, and NoRoute runs for every method — so
	// without this a POST (or PUT, or DELETE) to /ops1 rendered the page with 200,
	// answering a write-shaped request with a document. Only GET and HEAD read a
	// clip; writes live under /api/clips/<suffix>, where the owner token is
	// checked. 405 with Allow says which methods do work.
	if c.Request.Method != http.MethodGet && c.Request.Method != http.MethodHead {
		c.Header("Allow", "GET, HEAD")
		c.Header("Content-Type", "text/plain; charset=utf-8")
		c.Header("Cache-Control", "no-store")
		c.String(http.StatusMethodNotAllowed, "method not allowed\n")
		return
	}

	if path == "" {
		// Gin sets 404 on the response BEFORE running a NoRoute handler, so any
		// handler here that produces real content has to put the status back.
		// Forgetting it renders a correct page that reports 404 — a page that
		// looks fine in a browser and lies to every script and monitor.
		c.Status(http.StatusOK)
		renderLanding(c.Writer)
		return
	}

	// The share entrance. Dispatched before the clip-suffix shapes because `s` is a
	// reserved suffix (see suffix.go) — no user can hold it — and because reaching
	// here means the first segment was exactly "s", which no clip path can be.
	//
	// Only GET/HEAD arrive here (the method gate above), which is what makes a
	// share read-only at the route layer rather than by convention.
	if parts[0] == "s" {
		s.serveShare(c, parts[1:])
		return
	}

	switch len(parts) {
	case 1:
		s.serveClipPage(c, parts[0])
	case 2:
		if parts[1] != "raw" {
			notFoundPlain(c)
			return
		}
		s.serveRawText(c, parts[0])
	case 3:
		if parts[1] != "raw" {
			notFoundPlain(c)
			return
		}
		n, err := strconv.Atoi(parts[2])
		if err != nil || n < 0 || n > 1<<15 {
			notFoundPlain(c)
			return
		}
		s.serveRawAsset(c, parts[0], int16(n))
	default:
		notFoundPlain(c)
	}
}

// serveShare dispatches the shapes under /s/: the page, its raw text and its
// images. Anything else under /s/ is a 404 in the same text/plain shape, so a
// visitor (or a script) parsing these routes never has to switch parsers.
func (s *Server) serveShare(c *gin.Context, rest []string) {
	switch len(rest) {
	case 1:
		s.serveSharePage(c, rest[0])
	case 2:
		if rest[1] != "raw" {
			notFoundPlain(c)
			return
		}
		s.serveShareRawText(c, rest[0])
	case 3:
		if rest[1] != "raw" {
			notFoundPlain(c)
			return
		}
		n, err := strconv.Atoi(rest[2])
		if err != nil || n < 0 || n > 1<<15 {
			notFoundPlain(c)
			return
		}
		s.serveShareRawAsset(c, rest[0], int16(n))
	default:
		notFoundPlain(c)
	}
}

func (s *Server) serveClipPage(c *gin.Context, suffix string) {
	if err := ValidateSuffix(suffix); err != nil {
		c.Header("Content-Type", "text/html; charset=utf-8")
		c.Header("Cache-Control", "no-store")
		c.String(http.StatusBadRequest,
			"<!doctype html><meta charset=utf-8><title>后缀不合法</title>"+
				"<p>这个后缀不能用作剪贴板地址：%s</p><p><a href=\"/\">回首页</a></p>", err.Error())
		return
	}
	clip, err := s.store.Get(c.Request.Context(), suffix)
	if err != nil && !errors.Is(err, ErrNotFound) {
		s.log.Error("get clip for page", "suffix", suffix, "error", err)
		c.String(http.StatusInternalServerError, "internal error")
		return
	}
	// See handlePublicPath: NoRoute already set 404, and this path is a success.
	c.Status(http.StatusOK)
	renderPage(c.Writer, s.cfg, suffix, clip)
}

// serveRawText is the machine-readable read path: `curl <url>/raw`.
//
// This is the half of the feature that a Linux operator actually wants half the
// time — no browser, no HTML, no selecting text, just the bytes on stdout.
func (s *Server) serveRawText(c *gin.Context, suffix string) {
	if err := ValidateSuffix(suffix); err != nil {
		notFoundPlain(c)
		return
	}
	clip, err := s.store.Get(c.Request.Context(), suffix)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			notFoundPlain(c)
			return
		}
		s.log.Error("get clip for raw", "suffix", suffix, "error", err)
		c.String(http.StatusInternalServerError, "internal error")
		return
	}
	// /raw is the route the README promises to `curl`, so it is gated exactly
	// like the page: a password that the machine-readable view skipped would not
	// be a lock at all.
	if _, allowed := s.rawAccess(c, clip, passwordFrom(c)); !allowed {
		return
	}
	h := c.Writer.Header()
	// text/plain is not a scripting context, but the body is arbitrary bytes:
	// `nosniff` stops a browser from second-guessing the type, and the CSP is
	// belt-and-braces for a client that renders it as a document anyway.
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Robots-Tag", "noindex, nofollow")
	h.Set("Referrer-Policy", "no-referrer")
	c.Writer.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(c.Writer, clip.Body)
}

func (s *Server) serveRawAsset(c *gin.Context, suffix string, position int16) {
	if err := ValidateSuffix(suffix); err != nil {
		notFoundPlain(c)
		return
	}
	// Read the clip first: whether this asset needs a password is a property of
	// the clip, and the row that carries the hash is the only place to learn it.
	clip, err := s.store.Get(c.Request.Context(), suffix)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			notFoundPlain(c)
			return
		}
		s.log.Error("get clip for asset", "suffix", suffix, "error", err)
		c.String(http.StatusInternalServerError, "internal error")
		return
	}
	if _, allowed := s.rawAccess(c, clip, passwordFrom(c)); !allowed {
		return
	}
	mime, data, err := s.store.Asset(c.Request.Context(), suffix, position)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			notFoundPlain(c)
			return
		}
		s.log.Error("get asset", "suffix", suffix, "position", position, "error", err)
		c.String(http.StatusInternalServerError, "internal error")
		return
	}
	if _, allowed := allowedImageTypes[mime]; !allowed {
		// A stored type outside the allow-list means the row predates this check
		// or was written by another path. Refuse rather than serve it: the whole
		// point of the allow-list is that this origin never emits an unknown
		// content type.
		s.log.Error("refusing to serve non-allowlisted asset", "suffix", suffix, "mime", mime)
		notFoundPlain(c)
		return
	}
	h := c.Writer.Header()
	h.Set("Content-Type", mime)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	h.Set("Content-Disposition", "inline")
	// no-store rather than a long max-age: replacing a clip's images replaces
	// them at the same positions, so this URL is not immutable.
	h.Set("Cache-Control", "no-store")
	h.Set("X-Robots-Tag", "noindex, nofollow")
	c.Writer.WriteHeader(http.StatusOK)
	_, _ = c.Writer.Write(data)
}

func notFoundPlain(c *gin.Context) {
	c.Header("Content-Type", "text/plain; charset=utf-8")
	c.Header("Cache-Control", "no-store")
	c.String(http.StatusNotFound, "not found\n")
}

// ---------------------------------------------------------------------------
// health
// ---------------------------------------------------------------------------

func (s *Server) handleHealth(c *gin.Context) {
	stats, err := s.store.Stats(c.Request.Context())
	if err != nil {
		s.log.Error("health stats", "error", err)
		fail(c, http.StatusServiceUnavailable, "internal_error", "database unavailable")
		return
	}
	ok(c, http.StatusOK, gin.H{
		"status":         "ok",
		"service":        "clip",
		"version":        s.version,
		"environment":    s.cfg.Environment,
		"writes_enabled": s.cfg.WritesEnabled,
		"uptime_seconds": int64(time.Since(s.started).Seconds()),
		"clips":          stats.Clips,
		"bytes":          stats.TotalBytes,
	})
}

// ---------------------------------------------------------------------------
// request parsing helpers
// ---------------------------------------------------------------------------

type tooLargeError struct{ msg string }

func (e *tooLargeError) Error() string { return e.msg }

type unsupportedTypeError struct{ msg string }

func (e *unsupportedTypeError) Error() string { return e.msg }

// readJSONBody decodes the request body with unknown fields rejected, matching
// the teleport API's rule: a client that sends a field this service does not
// understand has a bug or a wrong idea of the contract, and silently ignoring it
// turns "my TTL was ignored" into a mystery.
// readJSONBody reads a write request.
//
// It is a thin wrapper over readJSONInto so the password endpoints can use their
// own (much smaller) body shape without duplicating the content-type check, the
// size cap and the error taxonomy.
func readJSONBody(c *gin.Context, maxBytes int64) (putRequest, error) {
	var req putRequest
	err := readJSONInto(c, maxBytes, &req)
	// The text limit is a property of a *body*, not of JSON: only a write request
	// has one, so it is checked here rather than in the shared decoder.
	if err == nil && int64(len(req.Body)) > maxBytes {
		err = &tooLargeError{msg: fmt.Sprintf("文本超过上限（%s）。", humanBytes(maxBytes))}
	}
	return req, err
}

// readJSONInto decodes a JSON request body into dst, bounding the read and
// rejecting a content type this service does not accept.
func readJSONInto(c *gin.Context, maxBytes int64, dst any) error {
	if ct := c.GetHeader("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		return &unsupportedTypeError{msg: "Content-Type 必须是 application/json"}
	}
	// A little slack over the limit so the error is "body too large" rather than
	// a confusing JSON syntax error at the exact boundary.
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBytes+4096)
	dec := json.NewDecoder(c.Request.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return &tooLargeError{msg: fmt.Sprintf("文本超过上限（%s）。", humanBytes(maxBytes))}
		}
		return fmt.Errorf("请求体不是合法的 JSON：%w", err)
	}
	return nil
}

func (s *Server) writeReadError(c *gin.Context, err error) {
	var tooLarge *tooLargeError
	var badType *unsupportedTypeError
	switch {
	case errors.As(err, &tooLarge):
		fail(c, http.StatusRequestEntityTooLarge, "payload_too_large", tooLarge.Error())
	case errors.As(err, &badType):
		fail(c, http.StatusBadRequest, "bad_request", badType.Error())
	default:
		fail(c, http.StatusBadRequest, "bad_request", err.Error())
	}
}

func (s *Server) writeStoreError(c *gin.Context, err error) {
	if errors.Is(err, ErrQuota) {
		fail(c, http.StatusServiceUnavailable, "quota_exceeded",
			"这个服务的总容量已经用满了，暂时不能写入。")
		return
	}
	s.log.Error("store write", "error", err, "requestId", c.GetString("requestId"))
	fail(c, http.StatusInternalServerError, "internal_error", "存储写入失败。")
}

// readImage validates and reads one uploaded file.
//
// The media type comes from the bytes, not from the upload's headers, so a file
// named .png that actually contains HTML is rejected rather than served later
// with an image content type (or worse, an HTML one).
func readImage(fh *multipart.FileHeader, maxBytes int64) (string, []byte, error) {
	if fh.Size > maxBytes {
		return "", nil, &tooLargeError{msg: fmt.Sprintf("单张图片超过上限（%s）。", humanBytes(maxBytes))}
	}
	f, err := fh.Open()
	if err != nil {
		return "", nil, err
	}
	defer f.Close()

	// Read one byte past the limit so an understated Size header cannot smuggle
	// a large file through.
	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return "", nil, err
	}
	if int64(len(data)) > maxBytes {
		return "", nil, &tooLargeError{msg: fmt.Sprintf("单张图片超过上限（%s）。", humanBytes(maxBytes))}
	}

	sniff := data
	if len(sniff) > 512 {
		sniff = sniff[:512]
	}
	mime := http.DetectContentType(sniff)
	// DetectContentType returns e.g. "image/png" or "text/html; charset=utf-8";
	// compare only the media type.
	mime = strings.TrimSpace(strings.SplitN(mime, ";", 2)[0])
	if _, allowed := allowedImageTypes[mime]; !allowed {
		return "", nil, &unsupportedTypeError{msg: fmt.Sprintf(
			"只支持 PNG / JPEG / GIF / WebP 图片（这个文件看起来是 %s）。", mime)}
	}
	return mime, data, nil
}

// expiresAtFromDefault applies the configured default TTL.
func expiresAtFromDefault(cfg *Config) int64 {
	return time.Now().Add(cfg.DefaultTTL).UnixMilli()
}

// expiryFromHours converts a client-supplied TTL to an absolute deadline.
//
// 0 means "never" (expires_at = 0), and a negative value is treated as an error
// by the caller's validation — a negative TTL would produce a clip that is born
// expired, which is never what someone means.
func expiryFromHours(hours float64) int64 {
	if hours <= 0 {
		return 0
	}
	return time.Now().Add(time.Duration(hours * float64(time.Hour))).UnixMilli()
}
