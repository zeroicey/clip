package clip

import (
	"crypto/rand"
	"embed"
	"encoding/base64"
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"time"
)

// pageFS embeds the page. It is a real .html file rather than a Go string so the
// markup and the inline CSS/JS keep their syntax highlighting — and so a
// mistake in it is a formatting error rather than a silently concatenated
// program.
//
//go:embed page.html
var pageFS embed.FS

// pageTemplate is parsed once at startup. Parsing per request would re-parse a
// 12 KiB template on a public endpoint for no benefit.
var pageTemplate = template.Must(template.ParseFS(pageFS, "page.html"))

// pageData is what the page needs to render. Everything user-supplied goes
// through html/template's contextual escaping: the clip body is arbitrary text
// from an unauthenticated author, so it must be treated as hostile in every
// context it appears in (element content, attribute, URL).
type pageData struct {
	Nonce       string
	Suffix      string
	Rev         int64
	State       string // "new" | "existing"
	Base        string
	Body        string
	Assets      []AssetMeta
	ExpiresText string
	Hint        string
	KeyState    string
	// Editable is the server's view: a suffix nobody has claimed yet is
	// editable by whoever got there first. The client refines this with the
	// owner token it holds.
	Editable bool
	CanWrite bool
	// MaxAssetText is shown in error messages, so a person learns the limit
	// before they hit it twice.
	MaxAssetText string
}

// PageCSP is the content security policy for the clipboard page.
//
// It is stricter than the dashboard's and comparable to the share page's, and
// for a stronger reason: the content on this page was written by an
// unauthenticated stranger. `script-src 'nonce-…'` (rather than
// `'unsafe-inline'`) means even a successful HTML injection into the body
// cannot execute anything, and `img-src 'self'` means an injected image cannot
// phone home with the suffix in its URL. `frame-ancestors 'none'` blocks the
// clickjacking variant of "get someone to save something".
//
// The single nonce covers the one <style> and the one <script> the page ships;
// there are no third-party origins at all, which is also why this page needs no
// webfont (see the note in page.html).
const PageCSP = "default-src 'none'; " +
	"script-src 'nonce-%s'; " +
	"style-src 'nonce-%s'; " +
	"img-src 'self' data:; " +
	"connect-src 'self'; " +
	"form-action 'none'; " +
	"base-uri 'none'; " +
	"frame-ancestors 'none'"

// newNonce mints a per-response nonce for the inline style/script.
//
// Per response, not per process: a nonce that never changes is a nonce an
// injected string can simply copy.
//
// RawURLEncoding rather than RawStdEncoding, and that is not cosmetic:
// html/template escapes `+` to `&#43;` in attribute context (a UTF-7 defence),
// so a standard-alphabet nonce appears in the HTML in a different byte form from
// the one in the CSP header. A browser decodes the entity and the page works, but
// every byte-level check disagrees with the policy — the test for this property
// failed on roughly 30% of runs before this was changed. The URL-safe alphabet
// (`A-Za-z0-9-_`) is attribute-clean and is explicitly valid in a CSP nonce.
func newNonce() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// renderPage writes the clipboard page for a suffix.
//
// A suffix that does not exist is rendered as an empty editor with a 200, not a
// 404 — the very first thing a person does with a new suffix is create it, and
// answering "not found" would make the intended action look like an error. The
// cost is that the response cannot distinguish "empty" from "absent"; nothing
// in the response differs either way, so it leaks nothing new (the body of a
// live clip is public to anyone who knows its suffix anyway).
func renderPage(w http.ResponseWriter, cfg *Config, suffix string, clip *Clip) {
	nonce, err := newNonce()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	data := pageData{
		Nonce:        nonce,
		Suffix:       suffix,
		Base:         cfg.PublicBaseURL,
		State:        "new",
		Editable:     true,
		CanWrite:     true,
		KeyState:     "无（只能读）",
		ExpiresText:  fmt.Sprintf("%d 小时后", int(cfg.DefaultTTL.Hours())),
		MaxAssetText: humanBytes(cfg.MaxAssetBytes),
		Hint: "这个后缀还没人用过 —— 写点东西，点保存，然后在另一台机器上打开同一个网址。" +
			"后缀是公开的，别放密钥。",
	}

	if clip != nil {
		data.State = "existing"
		data.Editable = false
		data.CanWrite = false
		data.Rev = clip.Rev
		data.Body = clip.Body
		data.Assets = clip.Assets
		data.ExpiresText = expiresText(clip.ExpiresAt)
		data.Hint = "只读。如果你是在另一台机器上，直接全选复制即可；" +
			"想编辑需要这台浏览器持有创建时的编辑钥匙。"
		if len(clip.Assets) > 0 {
			data.Hint += fmt.Sprintf("（含 %d 张图片，可点开原图或用 /raw 链接下载。）", len(clip.Assets))
		}
	}

	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", fmt.Sprintf(PageCSP, nonce, nonce))
	// no-store: the whole point is that this page shows the *current* content,
	// and a cached copy would hand the next visitor stale text.
	h.Set("Cache-Control", "no-store, must-revalidate")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Robots-Tag", "noindex, nofollow, noarchive")

	if err := pageTemplate.Execute(w, data); err != nil {
		// The status line is already written by now; log through the caller's
		// logger rather than trying to recover a response.
		_ = err
	}
}

// expiresText renders an expiry the way a person reads it: an absolute local
// time plus the relative distance, because "in 23 hours" is easy to act on and
// a bare timestamp is easy to verify.
func expiresText(expiresAt int64) string {
	if expiresAt == 0 {
		return "永不过期"
	}
	t := time.UnixMilli(expiresAt)
	left := time.Until(t)
	if left <= 0 {
		return "已过期"
	}
	return fmt.Sprintf("%s（约 %s 后）", t.Format("2006-01-02 15:04"), humanDuration(left))
}

// humanDuration rounds to the largest useful unit — nobody needs "23h 58m 12s"
// to decide whether a transfer window is long enough.
func humanDuration(d time.Duration) string {
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%.0f 天", d.Hours()/24)
	case d >= time.Hour:
		return fmt.Sprintf("%.0f 小时", d.Hours())
	case d >= time.Minute:
		return fmt.Sprintf("%.0f 分钟", d.Minutes())
	default:
		return "不到 1 分钟"
	}
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 3; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.0f %ciB", float64(n)/float64(div), "KMGT"[exp])
}

// landingHTML is the front page of the clipboard host.
//
// It exists because the URL people are given is a bare hostname as often as it
// is a suffix: someone told "put it on clip.hcyj.xyz" should find out what that
// means without being told separately. It is a constant rather than a template
// because it interpolates nothing.
const landingHTML = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex, nofollow">
<title>Relay 网络剪贴板</title>
<style>
:root{--bg:#f6f7f9;--fg:#14161a;--muted:#5c6470;--line:#d8dce3;--card:#fff;--accent:#1f6feb;--accent-fg:#fff}
@media (prefers-color-scheme:dark){:root{--bg:#0f1115;--fg:#e8eaee;--muted:#9aa3b0;--line:#272c34;--card:#161a20;--accent:#4c8dff;--accent-fg:#0b0d10}}
*{box-sizing:border-box}
body{margin:0;min-height:100%;background:var(--bg);color:var(--fg);
  font:16px/1.65 system-ui,-apple-system,"Segoe UI",Roboto,"Noto Sans SC",sans-serif;
  display:flex;justify-content:center;padding:8vh 20px}
main{max-width:620px;width:100%}
h1{font-size:22px;margin:0 0 6px}
p{color:var(--muted)}
code,kbd{font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:.92em;
  background:var(--card);border:1px solid var(--line);border-radius:5px;padding:1px 5px}
ol{padding-left:20px}
li{margin:6px 0}
form{display:flex;gap:10px;flex-wrap:wrap;margin:22px 0}
input{flex:1;min-width:200px;padding:11px 13px;border:1px solid var(--line);border-radius:9px;
  background:var(--card);color:var(--fg);font:inherit}
button{appearance:none;border:1px solid var(--accent);background:var(--accent);color:var(--accent-fg);
  border-radius:9px;padding:11px 18px;font:inherit;font-weight:600;cursor:pointer;min-height:44px}
small{color:var(--muted)}
</style>
</head>
<body>
<main>
<h1>Relay 网络剪贴板</h1>
<p>把一个后缀当成一块公共写字板：在一台机器上写，在另一台机器上打开同一个网址就能读到。</p>
<form action="/" method="get" onsubmit="return false">
  <input id="sfx" name="s" placeholder="随便打几个字符，比如 ops1" aria-label="后缀" autocapitalize="off" autocomplete="off" spellcheck="false">
  <button id="go" type="button">打开</button>
</form>
<ol>
  <li>在上面输入几个字符（只能用 <code>a-z 0-9 - _</code>），点「打开」。</li>
  <li>写出你要搬过去的内容，点「保存」。这个后缀就是你的了 —— 浏览器会替你记住写权限。</li>
  <li>在另一台机器上打开同一个网址，全选复制即可。想用命令行：<code>curl &lt;网址&gt;/raw</code>。</li>
</ol>
<p><small>后缀是公开的：知道后缀的人都能读。别往里放密码、密钥或任何机密。
默认 24 小时后自动清空；图片单张上限 5 MiB，最多 4 张。</small></p>
<script>
(function(){
  var input=document.getElementById("sfx"),go=document.getElementById("go");
  function open_(){var v=(input.value||"").trim();
    if(!v){input.focus();return}
    location.assign("/"+encodeURIComponent(v));}
  go.addEventListener("click",open_);
  input.addEventListener("keydown",function(e){if(e.key==="Enter"){e.preventDefault();open_()}});
  // A random suffix is the fastest path for "I just need to move this text and
  // do not care what the URL looks like".
  if(!input.value){
    var a="abcdefghjkmnpqrstuvwxyz23456789",s="";
    var buf=new Uint8Array(6);(crypto.getRandomValues?crypto.getRandomValues(buf):null);
    for(var i=0;i<6;i++)s+=a[(buf[i]||Math.floor(Math.random()*256))%a.length];
    input.placeholder=s;
  }
})();
</script>
</main>
</body>
</html>
`

// landingCSP allows the landing page's inline script only through the same
// nonce discipline as the clipboard page. It is a separate policy from PageCSP
// because the landing page has no user content and therefore no reason to allow
// images at all.
func landingCSP(nonce string) string {
	return "default-src 'none'; script-src 'nonce-" + nonce + "'; " +
		"style-src 'nonce-" + nonce + "'; form-action 'none'; base-uri 'none'; " +
		"frame-ancestors 'none'"
}

// renderLanding serves the front page.
func renderLanding(w http.ResponseWriter) {
	nonce, err := newNonce()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", landingCSP(nonce))
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Robots-Tag", "noindex, nofollow")
	h.Set("Referrer-Policy", "no-referrer")
	// The style/script blocks in this constant carry no nonce attribute because
	// it is a compile-time constant; substitute it in at send time. Doing it as
	// a string replace (rather than a template) keeps the page readable as HTML
	// in the source file.
	out := strings.ReplaceAll(landingHTML, "<style>", `<style nonce="`+nonce+`">`)
	out = strings.ReplaceAll(out, "<script>", `<script nonce="`+nonce+`">`)
	_, _ = w.Write([]byte(out))
}

// robotsTXT keeps crawlers out of the whole host. A suffix is short and often
// guessable, so an indexed suffix is a published one.
const robotsTXT = "User-agent: *\nDisallow: /\n"

// faviconSVG is inline so the service needs no static file pipeline at all.
const faviconSVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32">` +
	`<rect width="32" height="32" rx="7" fill="#1f6feb"/>` +
	`<path d="M9 11h14M9 16h14M9 21h9" stroke="#fff" stroke-width="2.4" stroke-linecap="round"/></svg>`
