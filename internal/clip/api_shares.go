package clip

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/zeroicey/clip/internal/clientip"
)

// ---------------------------------------------------------------------------
// shares: the read-only second entrance (/s/<token>)
//
// Design: .ai/decisions/2026-10-16-clip-share-links.md. The shape that matters
// for every handler below is that this entrance is READ-ONLY BY CONSTRUCTION:
// /s/... only ever answers GET and HEAD (see handlePublicPath), so there is no
// write endpoint here to hide behind a hidden button. The owner's control over a
// share is creating it and revoking it, and both of those live under
// /api/clips/<suffix>/shares — behind the clip's own master password.
// ---------------------------------------------------------------------------

// shareData is one share as it appears on the wire.
//
// The same type answers creation, the owner's list, unlock and reads, so a client
// never has to reconcile two spellings of "what is a share". When the answer is a
// locked shell the content fields are simply absent; when it is the creation
// response the one-time `code` is present.
//
// The underlying clip suffix is deliberately NOT a field: /s/<token> must not
// reveal (nor help enumerate) the clip's own address, which is the whole reason
// the token exists as a separate address.
type shareData struct {
	Token string `json:"token"`
	URL   string `json:"url"`
	// RawURL is what a script curls. On a protected share it needs
	// `-H 'X-Clip-Password: <口令>'` exactly like the clip's own /raw.
	RawURL      string      `json:"raw_url"`
	Exists      bool        `json:"exists"`
	Rev         int64       `json:"rev"`
	Body        string      `json:"body"`
	Assets      []assetData `json:"assets"`
	UpdatedAt   int64       `json:"updated_at"`
	ExpiresAt   int64       `json:"expires_at"`
	ExpiresText string      `json:"expires_text"`
	CreatedAt   int64       `json:"created_at"`
	// HasPassword/HasCode say which secret this share wants, so the page can word
	// its prompt ("分享密码" vs "6 位取件码") without guessing. Note there is no
	// `locked` flag: a protected share answers an unauthorised read with a 401 in
	// the envelope (never a 200 carrying an empty body), the same contract a
	// protected clip follows — so a client learns "there is content behind a
	// secret" from the status alone, with no second shape to parse.
	HasPassword bool `json:"has_password"`
	HasCode     bool `json:"has_code"`
	// Code is the plaintext pickup code, and it is present ONLY on the response
	// that created it. The database holds a hash, so this is the one and only
	// moment the code exists outside the visitor's head — the UI has to say so.
	Code string `json:"code,omitempty"`
}

func (s *Server) shareBaseData(sh *Share) shareData {
	return shareData{
		Token:       sh.Token,
		URL:         s.cfg.ShareURL(sh.Token),
		RawURL:      s.cfg.ShareRawURL(sh.Token),
		ExpiresAt:   sh.ExpiresAt,
		ExpiresText: expiresText(sh.ExpiresAt),
		CreatedAt:   sh.CreatedAt,
		HasPassword: sh.HasPassword(),
		HasCode:     sh.HasCode(),
		Assets:      []assetData{},
	}
}

// shareContentData is shareBaseData plus the clip behind it.
//
// `clip` is the live clip row, fetched through the share's own reference — the
// same row the owner edits, which is why an edit shows up here with no copy to
// invalidate.
func (s *Server) shareContentData(sh *Share, clip *Clip) shareData {
	d := s.shareBaseData(sh)
	d.Exists = true
	d.Rev = clip.Rev
	d.Body = clip.Body
	d.UpdatedAt = clip.UpdatedAt
	for _, a := range clip.Assets {
		d.Assets = append(d.Assets, assetData{
			Position: a.Position,
			Mime:     a.Mime,
			ByteSize: a.ByteSize,
			URL:      s.cfg.ShareAssetURL(sh.Token, a.Position),
		})
	}
	return d
}

// resolveShare turns a token into the share and the clip it points at.
//
// ErrNotFound covers "malformed token", "no such share", "expired share" and "the
// clip behind it is gone" alike — from the visitor's side those are all "this link
// does not work", and there is no user value in telling them apart. (A wrong
// secret is a different answer, and deliberately so: see shareDecision.)
func (s *Server) resolveShare(ctx context.Context, token string) (*Share, *Clip, error) {
	if err := ValidateShareToken(token); err != nil {
		return nil, nil, ErrNotFound
	}
	sh, err := s.store.GetShare(ctx, token)
	if err != nil {
		return nil, nil, err
	}
	// The clip is fetched through the share's suffix, NOT through anything the
	// caller supplied, so a share can only ever expose its own clip.
	clip, err := s.store.Get(ctx, sh.Suffix)
	if err != nil {
		return nil, nil, err
	}
	return sh, clip, nil
}

// shareDecision decides whether a visitor's secret opens a share.
//
// It is the counterpart of clipAccess, and the one thing it deliberately does NOT
// do is consult the clip's master password: a share secret is independent of it,
// which is the entire reason the feature exists ("do not hand over the master
// password"). So a share on a password-protected clip is readable with the share
// secret alone.
//
// status == 0 means allowed. Callers shape the refusal for their own audience
// (envelope under /api, text/plain under /s).
func (s *Server) shareDecision(c *gin.Context, sh *Share, secret string) (int, string, string) {
	if sh == nil || !sh.NeedsSecret() {
		// A link-only share: the link IS the credential, and it is 128 bits of
		// randomness, so there is nothing to guess.
		return 0, "", ""
	}

	// The same two-namespace rule as clipAccess, with the share token in place of
	// the suffix: per source address AND per share. Either alone is insufficient —
	// per-IP lets an attacker with many addresses hammer one share, and per-share
	// lets one address spray every token it can think of.
	//
	// This bucket is what makes a 6-digit code safe at all: 10^6 possibilities are
	// enumerable if guesses are free, and here every FAILURE costs one of a small
	// per-token budget (default 20/hour), on top of the code's short TTL.
	ipKey := "ip:" + clientip.ClientIP(c.Request)
	tokenKey := "share:" + sh.Token

	// Checked before verifying, because verification is the expensive part
	// (argon2 at 19 MiB) and an unthrottled guesser is a memory/CPU amplifier.
	if s.unlockLimiter.Blocked(ipKey) || s.unlockLimiter.Blocked(tokenKey) {
		c.Header("Retry-After", strconv.Itoa(s.unlockLimiter.RetryAfter()))
		return http.StatusTooManyRequests, "rate_limited", "口令试得太频繁了，稍后再试。"
	}

	if secret == "" {
		return http.StatusUnauthorized, "password_required", shareSecretPrompt(sh)
	}
	if !sh.Verify(secret) {
		// Only failures consume budget: a correct secret never exhausts anything,
		// so a reader may poll as often as it likes while a guesser is throttled.
		s.unlockLimiter.Allow(ipKey)
		s.unlockLimiter.Allow(tokenKey)
		return http.StatusUnauthorized, "password_incorrect", shareSecretWrong(sh)
	}
	return 0, "", ""
}

// shareSecretPrompt and shareSecretWrong word the same refusal for whichever
// secret this share actually carries — "取件码" and "分享密码" are different things
// to the person reading the screen, and one message for both would send someone
// hunting for a password that does not exist.
func shareSecretPrompt(sh *Share) string {
	if sh.HasCode() {
		return "这个分享链接需要取件码。"
	}
	return "这个分享链接需要分享密码。"
}

func shareSecretWrong(sh *Share) string {
	if sh.HasCode() {
		return "取件码不对。"
	}
	return "分享密码不对。"
}

// shareAccess is shareDecision for the JSON API: refusals come back in the
// documented envelope.
func (s *Server) shareAccess(c *gin.Context, sh *Share, secret string) bool {
	status, code, msg := s.shareDecision(c, sh, secret)
	if status != 0 {
		fail(c, status, code, msg)
		return false
	}
	return true
}

// shareRawAccess is shareDecision for the /s/<token>/raw namespace, which answers
// in text/plain — the same shape as its 404s, so a script parsing these routes
// never has to switch parsers on the status code.
func (s *Server) shareRawAccess(c *gin.Context, sh *Share, secret string) bool {
	status, _, msg := s.shareDecision(c, sh, secret)
	if status != 0 {
		c.Header("Content-Type", "text/plain; charset=utf-8")
		c.Header("Cache-Control", "no-store")
		if status == http.StatusUnauthorized {
			msg += "用 curl 的话：加 -H 'X-Clip-Password: <口令>'。"
		}
		c.String(status, msg+"\n")
		return false
	}
	return true
}

// ---------------------------------------------------------------------------
// owner-side share management (behind the clip's own password)
// ---------------------------------------------------------------------------

// maxShareRequestBytes bounds share-creation bodies. Small: the body carries at
// most a password and a couple of numbers, so a large body here is a mistake or
// an attack, and the parse must not be usable to allocate.
const maxShareRequestBytes = 4 << 10

// shareRequest is the body of share creation. Camel-case on the way in, per the
// project's convention.
type shareRequest struct {
	// Password is the owner's chosen share secret. An empty string means "no
	// secret" — a link-only share — rather than a password nobody can type, which
	// would be an unopenable share and never what someone means.
	Password *string `json:"password"`
	// Code asks the server to mint a 6-digit pickup code (the parcel-locker
	// mode). Mutually exclusive with Password: one share carries one secret.
	Code bool `json:"code"`
	// ExpiresInHours mirrors the clip write: absent = the default TTL, 0 = never
	// (still capped by the clip's own expiry). A negative value is refused.
	ExpiresInHours *float64 `json:"expiresInHours"`
}

// handleListShares returns every share of a clip, for the owner's panel.
//
// It is a read, and it is gated by clipAccess, so a protected clip's share list
// is only visible to someone who can already read the clip. The rows carry
// booleans, never hashes or plaintext.
func (s *Server) handleListShares(c *gin.Context) {
	suffix := c.Param("suffix")
	if err := ValidateSuffix(suffix); err != nil {
		failInvalidSuffix(c, err)
		return
	}
	ctx := c.Request.Context()
	clip, err := s.store.Get(ctx, suffix)
	if err != nil && !errors.Is(err, ErrNotFound) {
		s.log.Error("get clip for share list", "suffix", suffix, "error", err)
		fail(c, http.StatusInternalServerError, "internal_error", "could not read the clip")
		return
	}
	if clip == nil {
		fail(c, http.StatusNotFound, "not_found", "没有这份剪贴板。")
		return
	}
	if _, allowed := s.clipAccess(c, clip, passwordFrom(c)); !allowed {
		return
	}
	shares, err := s.store.ListShares(ctx, suffix)
	if err != nil {
		s.log.Error("list shares", "suffix", suffix, "error", err)
		fail(c, http.StatusInternalServerError, "internal_error", "could not list shares")
		return
	}
	list := make([]shareData, 0, len(shares))
	for i := range shares {
		list = append(list, s.shareBaseData(&shares[i]))
	}
	c.Header("Cache-Control", "no-store")
	ok(c, http.StatusOK, gin.H{"shares": list})
}

// handleCreateShare mints a share link for a clip.
//
// Three shapes, one per way a visitor might be let in:
//
//	password given   → the owner's own share password
//	code: true       → a server-generated 6-digit pickup code, short TTL
//	neither          → the link itself is enough (still read-only)
//
// All three are gated by the clip's master password (or its publicness), and the
// check travels into the INSERT rather than preceding it — see Store.CreateShare.
func (s *Server) handleCreateShare(c *gin.Context) {
	suffix := c.Param("suffix")
	if err := ValidateSuffix(suffix); err != nil {
		failInvalidSuffix(c, err)
		return
	}
	if !s.cfg.WritesEnabled {
		fail(c, http.StatusServiceUnavailable, "writes_disabled", "这个服务当前只读（管理员关闭了写入）。")
		return
	}
	// Creating a share is a write (it inserts a row that grants read access), so
	// it draws on the same per-address budget as any other write. Without that, a
	// stolen master password could mint unlimited long-lived read links.
	if !s.limiter.Allow(clientip.ClientIP(c.Request)) {
		c.Header("Retry-After", strconv.Itoa(s.limiter.RetryAfter()))
		fail(c, http.StatusTooManyRequests, "rate_limited", "写得太频繁了，稍后再试。")
		return
	}

	var req shareRequest
	if err := readJSONInto(c, maxShareRequestBytes, &req); err != nil {
		s.writeReadError(c, err)
		return
	}
	if req.ExpiresInHours != nil && *req.ExpiresInHours < 0 {
		fail(c, http.StatusBadRequest, "bad_request", "expiresInHours 不能为负数（0 表示永不过期）。")
		return
	}

	passwordMode := req.Password != nil && *req.Password != ""
	if req.Code && passwordMode {
		fail(c, http.StatusBadRequest, "bad_request",
			"分享密码和取件码二选一：一条分享只能有一个口令。")
		return
	}
	if passwordMode {
		if err := CheckPassword(*req.Password); err != nil {
			fail(c, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
	}

	ctx := c.Request.Context()
	clip, err := s.store.Get(ctx, suffix)
	if err != nil && !errors.Is(err, ErrNotFound) {
		s.log.Error("get clip for share create", "suffix", suffix, "error", err)
		fail(c, http.StatusInternalServerError, "internal_error", "could not read the clip")
		return
	}
	if clip == nil {
		fail(c, http.StatusNotFound, "not_found", "没有这份剪贴板（先写下内容创建它，再分享）。")
		return
	}
	proof, allowed := s.clipAccess(c, clip, passwordFrom(c))
	if !allowed {
		return
	}

	var passwordHash *string
	if passwordMode {
		h, herr := hashPassword(*req.Password)
		if herr != nil {
			s.log.Error("hash share password", "error", herr)
			fail(c, http.StatusInternalServerError, "internal_error", "口令处理失败。")
			return
		}
		passwordHash = &h
	}

	// The pickup code is generated here, hashed, and the plaintext is returned
	// exactly once (below). Nothing stores it, so a lost response means creating a
	// new share — which is the honest behaviour for a secret this short.
	var codeHash, plainCode *string
	if req.Code {
		code, cerr := newShareCode()
		if cerr != nil {
			s.log.Error("generate share code", "error", cerr)
			fail(c, http.StatusInternalServerError, "internal_error", "取件码生成失败。")
			return
		}
		h, herr := hashPassword(code)
		if herr != nil {
			s.log.Error("hash share code", "error", herr)
			fail(c, http.StatusInternalServerError, "internal_error", "口令处理失败。")
			return
		}
		codeHash, plainCode = &h, &code
	}

	expiresAt := s.shareExpiry(req, req.Code, clip.ExpiresAt)

	token, err := newShareToken()
	if err != nil {
		s.log.Error("generate share token", "error", err)
		fail(c, http.StatusInternalServerError, "internal_error", "分享链接生成失败。")
		return
	}

	if err := s.store.CreateShare(ctx, suffix, proof, token, passwordHash, codeHash, expiresAt); err != nil {
		if errors.Is(err, ErrNotFound) {
			// The clip is gone, or the master password changed between the check
			// above and the insert. Both mean "this did not happen".
			fail(c, http.StatusNotFound, "not_found", "没有这份剪贴板，或者密码不对。")
			return
		}
		s.writeStoreError(c, err)
		return
	}

	sh := &Share{
		Token:        token,
		Suffix:       suffix,
		PasswordHash: passwordHash,
		CodeHash:     codeHash,
		ExpiresAt:    expiresAt,
		CreatedAt:    time.Now().UnixMilli(),
	}
	data := s.shareBaseData(sh)
	if plainCode != nil {
		data.Code = *plainCode
	}
	c.Header("Cache-Control", "no-store")
	ok(c, http.StatusCreated, data)
}

// shareExpiry turns the request's lifetime into an absolute deadline.
//
// Two clamps, both of which the caller would otherwise have to remember:
//
//   - a code share never lives longer than CLIP_SHARE_CODE_TTL. 10^6
//     possibilities are only safe behind a short window, so this is not a
//     default the caller can opt out of by asking for a year.
//   - no share outlives the clip it points at. A link that says "valid for 24
//     hours" while the clip underneath expires in ten minutes is a link that lies.
func (s *Server) shareExpiry(req shareRequest, codeMode bool, clipExpiresAt int64) int64 {
	var expiresAt int64
	switch {
	case req.ExpiresInHours == nil && codeMode:
		expiresAt = time.Now().Add(s.cfg.ShareCodeTTL).UnixMilli()
	case req.ExpiresInHours == nil:
		expiresAt = expiresAtFromDefault(s.cfg)
	default:
		expiresAt = expiryFromHours(*req.ExpiresInHours)
	}
	if codeMode {
		if max := time.Now().Add(s.cfg.ShareCodeTTL).UnixMilli(); expiresAt == 0 || expiresAt > max {
			expiresAt = max
		}
	}
	if clipExpiresAt != 0 && (expiresAt == 0 || expiresAt > clipExpiresAt) {
		expiresAt = clipExpiresAt
	}
	return expiresAt
}

// handleDeleteShare revokes one share. Revoking is immediate: the very next read
// through that token is a 404, because the row the read resolves through is gone.
func (s *Server) handleDeleteShare(c *gin.Context) {
	suffix := c.Param("suffix")
	if err := ValidateSuffix(suffix); err != nil {
		failInvalidSuffix(c, err)
		return
	}
	token := c.Param("token")
	if err := ValidateShareToken(token); err != nil {
		fail(c, http.StatusNotFound, "not_found", "没有这个分享链接。")
		return
	}
	if !s.cfg.WritesEnabled {
		fail(c, http.StatusServiceUnavailable, "writes_disabled", "这个服务当前只读。")
		return
	}
	ctx := c.Request.Context()
	clip, err := s.store.Get(ctx, suffix)
	if err != nil && !errors.Is(err, ErrNotFound) {
		s.log.Error("get clip for share revoke", "suffix", suffix, "error", err)
		fail(c, http.StatusInternalServerError, "internal_error", "could not read the clip")
		return
	}
	if clip == nil {
		fail(c, http.StatusNotFound, "not_found", "没有这份剪贴板。")
		return
	}
	proof, allowed := s.clipAccess(c, clip, passwordFrom(c))
	if !allowed {
		return
	}
	// The DELETE matches on (suffix, token) and re-states the proof, so revoking
	// through one clip's authority cannot touch another clip's share, and a master
	// password changed in between revokes nothing.
	deleted, err := s.store.DeleteShare(ctx, suffix, proof, token)
	if err != nil {
		s.writeStoreError(c, err)
		return
	}
	if !deleted {
		fail(c, http.StatusNotFound, "not_found", "没有这个分享链接。")
		return
	}
	// Wake any visitor whose long poll is parked on this clip. Without it a revoked
	// share's page would keep showing the content until its poll window expired —
	// the link would be dead on the server and alive on the screen, which is the
	// silent wrong state this project treats as a bug.
	s.hub.publish(suffix)
	c.Status(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// visitor-side reads (/api/shares/<token> and the /s/... paths)
// ---------------------------------------------------------------------------

// handleGetShare is the JSON read behind the share page.
//
// The page cannot use /s/<token> itself for this: a browser navigation cannot
// carry the X-Clip-Password header, so the locked shell asks for the secret and
// then fetches the content over this endpoint (exactly like a locked clip's
// /api/clips/<suffix> + /unlock pair).
//
// `?wait=N` makes it a long poll, sharing the clip's hub: the share points at the
// same row, so an edit made by the owner wakes a visitor watching through it.
func (s *Server) handleGetShare(c *gin.Context) {
	token := c.Param("token")
	ctx := c.Request.Context()
	sh, clip, err := s.resolveShare(ctx, token)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			fail(c, http.StatusNotFound, "not_found", "这个分享链接不存在或已失效。")
			return
		}
		s.log.Error("resolve share", "error", err)
		fail(c, http.StatusInternalServerError, "internal_error", "could not read the share")
		return
	}

	rev := int64(-1)
	if raw := c.Query("rev"); raw != "" {
		n, perr := strconv.ParseInt(raw, 10, 64)
		if perr != nil || n < 0 {
			fail(c, http.StatusBadRequest, "bad_request", "rev must be a non-negative integer")
			return
		}
		rev = n
	}
	wait := 0
	if raw := c.Query("wait"); raw != "" {
		n, perr := strconv.Atoi(raw)
		if perr != nil || n < 0 {
			fail(c, http.StatusBadRequest, "bad_request", "wait must be a non-negative number of seconds")
			return
		}
		wait = n
	}

	// An unauthorised caller refuses here, before any waiting: it must not be able
	// to hold a long-poll connection open, nor to learn from timing that the clip
	// changed.
	if !s.shareAccess(c, sh, passwordFrom(c)) {
		return
	}

	if wait > 0 && rev >= 0 && clip.Rev == rev {
		window := time.Duration(wait) * time.Second
		if window > longPollWindow {
			window = longPollWindow
		}
		if ch, release, okWait := s.hub.subscribe(sh.Suffix); okWait {
			timer := time.NewTimer(window)
			select {
			case <-ch:
			case <-timer.C:
			case <-ctx.Done():
			}
			timer.Stop()
			release()
			// Re-resolved rather than reusing the pre-wait values: the share can be
			// revoked, or the clip's password changed, while this request is parked.
			sh, clip, err = s.resolveShare(ctx, token)
			if err != nil {
				if errors.Is(err, ErrNotFound) {
					fail(c, http.StatusNotFound, "not_found", "这个分享链接不存在或已失效。")
					return
				}
				s.log.Error("re-resolve share after wait", "error", err)
				fail(c, http.StatusInternalServerError, "internal_error", "could not read the share")
				return
			}
			if !s.shareAccess(c, sh, passwordFrom(c)) {
				return
			}
		}
	}

	c.Header("Cache-Control", "no-store")
	ok(c, http.StatusOK, s.shareContentData(sh, clip))
}

// handleUnlockShare exchanges a share secret for the content.
//
// The secret travels in the request body here and in X-Clip-Password everywhere
// else; neither is a URL, so it never reaches an access log, browser history or a
// Referer header. A 6-digit pickup code uses this same field — from the client's
// side it is simply "the thing the visitor types".
func (s *Server) handleUnlockShare(c *gin.Context) {
	token := c.Param("token")
	var body passwordRequest
	if err := readJSONInto(c, maxPasswordBytes, &body); err != nil {
		s.writeReadError(c, err)
		return
	}
	ctx := c.Request.Context()
	sh, clip, err := s.resolveShare(ctx, token)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			// 404, not 401: no secret would help, because there is nothing there.
			fail(c, http.StatusNotFound, "not_found", "这个分享链接不存在或已失效。")
			return
		}
		s.log.Error("resolve share for unlock", "error", err)
		fail(c, http.StatusInternalServerError, "internal_error", "could not read the share")
		return
	}
	if !s.shareAccess(c, sh, body.Password) {
		return
	}
	c.Header("Cache-Control", "no-store")
	ok(c, http.StatusOK, s.shareContentData(sh, clip))
}

// serveSharePage renders /s/<token>.
//
// A protected share renders the same locked shell a protected clip does, with no
// content anywhere in the HTML: if the text were rendered and hidden with CSS, the
// secret would be decoration. An unprotected share (or one whose visitor opened it
// with a header, which a browser navigation cannot do) renders the read-only view.
func (s *Server) serveSharePage(c *gin.Context, token string) {
	sh, clip, err := s.resolveShare(c.Request.Context(), token)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			notFoundPlain(c)
			return
		}
		s.log.Error("resolve share for page", "error", err)
		c.String(http.StatusInternalServerError, "internal error")
		return
	}
	// See handlePublicPath: NoRoute already set 404, and this path is a success.
	c.Status(http.StatusOK)
	renderSharePage(c.Writer, s.cfg, sh, clip)
}

// serveShareRawText is the machine-readable read through a share:
// `curl <share-url>/raw`. It is gated exactly like the page — a machine-readable
// view that skipped the secret would not be a lock at all.
func (s *Server) serveShareRawText(c *gin.Context, token string) {
	sh, clip, err := s.resolveShare(c.Request.Context(), token)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			notFoundPlain(c)
			return
		}
		s.log.Error("resolve share for raw", "error", err)
		c.String(http.StatusInternalServerError, "internal error")
		return
	}
	if !s.shareRawAccess(c, sh, passwordFrom(c)) {
		return
	}
	h := c.Writer.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Robots-Tag", "noindex, nofollow")
	h.Set("Referrer-Policy", "no-referrer")
	c.Writer.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(c.Writer, clip.Body)
}

// serveShareRawAsset serves one image through a share.
func (s *Server) serveShareRawAsset(c *gin.Context, token string, position int16) {
	sh, _, err := s.resolveShare(c.Request.Context(), token)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			notFoundPlain(c)
			return
		}
		s.log.Error("resolve share for asset", "error", err)
		c.String(http.StatusInternalServerError, "internal error")
		return
	}
	if !s.shareRawAccess(c, sh, passwordFrom(c)) {
		return
	}
	mime, data, err := s.store.Asset(c.Request.Context(), sh.Suffix, position)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			notFoundPlain(c)
			return
		}
		s.log.Error("get share asset", "token", token, "position", position, "error", err)
		c.String(http.StatusInternalServerError, "internal error")
		return
	}
	if _, allowed := allowedImageTypes[mime]; !allowed {
		// Same rule as the clip's own asset route: this origin never emits a
		// content type that is not on the allow-list, whatever a stale row says.
		s.log.Error("refusing to serve non-allowlisted share asset", "token", token, "mime", mime)
		notFoundPlain(c)
		return
	}
	h := c.Writer.Header()
	h.Set("Content-Type", mime)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	h.Set("Content-Disposition", "inline")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Robots-Tag", "noindex, nofollow")
	c.Writer.WriteHeader(http.StatusOK)
	_, _ = c.Writer.Write(data)
}
