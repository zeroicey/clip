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
	log     *slog.Logger
	version string
	started time.Time
}

// NewServer builds the handler for the whole clip service.
func NewServer(cfg *Config, st *Store, version string, log *slog.Logger) (*Server, *gin.Engine) {
	s := &Server{
		cfg:     cfg,
		store:   st,
		hub:     newHub(1024),
		limiter: ratelimit.New(cfg.WritePerHour, time.Hour),
		log:     log,
		version: version,
		started: time.Now(),
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
	Suffix      string      `json:"suffix"`
	Exists      bool        `json:"exists"`
	Rev         int64       `json:"rev"`
	Body        string      `json:"body"`
	ExpiresAt   int64       `json:"expires_at"`
	CreatedAt   int64       `json:"created_at"`
	UpdatedAt   int64       `json:"updated_at"`
	Assets      []assetData `json:"assets"`
	URL         string      `json:"url"`
	RawURL      string      `json:"raw_url"`
	ExpiresText string      `json:"expires_text"`
	// OwnerToken is present ONLY in the response to the request that created the
	// clip. It is the one moment the plaintext exists; after that the server
	// holds only its hash.
	OwnerToken string `json:"owner_token,omitempty"`
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

	token := c.GetHeader("X-Relay-Clip-Token")
	ctx := c.Request.Context()

	// A negative TTL would produce a clip that is born expired, which is never
	// what someone means; treating it as "never expires" (the <=0 branch) would
	// be the worst possible reading of it.
	if body.ExpiresInHours != nil && *body.ExpiresInHours < 0 {
		fail(c, http.StatusBadRequest, "bad_request", "expiresInHours 不能为负数（0 表示永不过期）。")
		return
	}

	if token != "" {
		// A caller with a token is trying to update. If the suffix is not
		// theirs, fall through to the create path — the answer either way is
		// the same, and this ordering means one suffix cannot be hijacked by
		// simply omitting the token.
		expiresAt := int64(-1) // keep whatever the clip already has
		if body.ExpiresInHours != nil {
			expiresAt = expiryFromHours(*body.ExpiresInHours)
		}
		res, err := s.store.Update(ctx, suffix, hashOwnerToken(token), body.Body, expiresAt)
		if err == nil {
			s.hub.publish(suffix)
			clip, gerr := s.store.Get(ctx, suffix)
			if gerr != nil {
				fail(c, http.StatusInternalServerError, "internal_error", "saved, but could not read it back")
				return
			}
			_ = res
			ok(c, http.StatusOK, s.clipData(clip))
			return
		}
		if !errors.Is(err, ErrNotFound) {
			s.writeStoreError(c, err)
			return
		}
	}

	// Create. The token is minted here and returned exactly once.
	expiresAt := expiresAtFromDefault(s.cfg)
	if body.ExpiresInHours != nil {
		expiresAt = expiryFromHours(*body.ExpiresInHours)
	}
	newToken, hash, err := newOwnerToken()
	if err != nil {
		s.log.Error("mint owner token", "error", err)
		fail(c, http.StatusInternalServerError, "internal_error", "could not mint a write token")
		return
	}
	if _, err := s.store.Create(ctx, suffix, body.Body, hash, expiresAt); err != nil {
		if errors.Is(err, ErrTaken) {
			fail(c, http.StatusConflict, "suffix_taken",
				fmt.Sprintf("后缀 %q 已经被占用了，换一个，或者用编辑钥匙链接打开。", suffix))
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
	d := s.clipData(clip)
	d.OwnerToken = newToken
	ok(c, http.StatusCreated, d)
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
	token := c.GetHeader("X-Relay-Clip-Token")
	if token == "" {
		// No token is not "not yours, but try again": without it there is no
		// way this request could ever be authorised, so say so rather than
		// returning a bare 404 that reads like a bug.
		fail(c, http.StatusUnauthorized, "unauthorized", "删除需要编辑钥匙（X-Relay-Clip-Token）。")
		return
	}
	deleted, err := s.store.Delete(c.Request.Context(), suffix, hashOwnerToken(token))
	if err != nil {
		s.writeStoreError(c, err)
		return
	}
	if !deleted {
		fail(c, http.StatusNotFound, "not_found", "没有这份剪贴板，或者钥匙不对。")
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
	token := c.GetHeader("X-Relay-Clip-Token")
	if token == "" {
		fail(c, http.StatusUnauthorized, "unauthorized", "上传需要编辑钥匙（X-Relay-Clip-Token）。")
		return
	}
	if !s.limiter.Allow(clientip.ClientIP(c.Request)) {
		c.Header("Retry-After", strconv.Itoa(s.limiter.RetryAfter()))
		fail(c, http.StatusTooManyRequests, "rate_limited", "写得太频繁了，稍后再试。")
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

	if _, err := s.store.SetAssets(c.Request.Context(), suffix, hashOwnerToken(token), assets); err != nil {
		if errors.Is(err, ErrNotFound) {
			fail(c, http.StatusNotFound, "not_found", "没有这份剪贴板，或者钥匙不对。")
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
func readJSONBody(c *gin.Context, maxBytes int64) (putRequest, error) {
	var req putRequest
	if ct := c.GetHeader("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		return req, &unsupportedTypeError{msg: "Content-Type 必须是 application/json"}
	}
	// A little slack over the limit so the error is "body too large" rather than
	// a confusing JSON syntax error at the exact boundary.
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBytes+4096)
	dec := json.NewDecoder(c.Request.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return req, &tooLargeError{msg: fmt.Sprintf("文本超过上限（%s）。", humanBytes(maxBytes))}
		}
		return req, fmt.Errorf("请求体不是合法的 JSON：%w", err)
	}
	if int64(len(req.Body)) > maxBytes {
		return req, &tooLargeError{msg: fmt.Sprintf("文本超过上限（%s）。", humanBytes(maxBytes))}
	}
	return req, nil
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
