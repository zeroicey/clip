#!/usr/bin/env node
/**
 * Real-browser verification of the clip page.
 *
 * The Go tests cover the HTTP contract; this covers what only a browser can
 * answer: does the inline script actually run under the CSP it is served with,
 * does a real click save, does the second machine really see the text, and does
 * the long poll update it without a reload.
 *
 * It deliberately drives raw CDP rather than adding puppeteer to the repo (the
 * same choice the earlier verification scripts made): the dependency would exist
 * only for this file, and `ws` is already in the pnpm store.
 *
 * Two pages are used, with localStorage cleared between them, because the whole
 * feature is "one browser writes, a different browser reads". A second tab that
 * still holds the owner token would prove nothing.
 *
 * Usage: node scripts/verify-clip-browser.cjs [base-url]
 *        CHROME=/path/to/chrome node scripts/verify-clip-browser.cjs [base-url]
 */
const { spawn } = require('node:child_process');
const fs = require('node:fs');
const net = require('node:net');
const path = require('node:path');

const BASE = process.argv[2] || 'http://127.0.0.1:8790';
// Chromium binary. Override with CHROME=... when the local Playwright/puppeteer
// cache has a different revision — a hardcoded revision makes this script stop
// working for everyone else (and for us after the next browser update).
const CHROME = process.env.CHROME
  || `${process.env.HOME}/.cache/ms-playwright/chromium-1248/chrome-linux64/chrome`;
// Artifacts (screenshots, throwaway Chrome profiles) go under the repository,
// resolved from this file's own location rather than a hardcoded absolute path —
// an absolute path silently breaks the moment the checkout moves or is renamed.
// `bin/` is gitignored.
const OUT = process.env.VERIFY_OUT
  || path.join(__dirname, '..', 'bin', 'e2e', 'localtest');
const SFX = `br${Date.now().toString(36)}`;
// Printed up front so a failure can be correlated with the server's own access
// log (`journalctl -u clip | grep <suffix>`). Without it, a flaky browser failure
// leaves you guessing which requests belonged to the run.
console.log(`clip suffix: ${SFX}`);
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

const results = [];
const check = (name, ok, detail = '') => {
  results.push({ name, ok, detail });
  console.log(`${ok ? 'PASS' : 'FAIL'}  ${name}${detail ? `  — ${detail}` : ''}`);
};

// A 429 is almost never a product bug when this script is the client: the write
// limiter is per source address across ALL clips (CLIP_WRITE_PER_HOUR, default
// 30/h), and one full run costs about seven writes. So the fourth run inside an
// hour starts failing in ways that look like broken features — a clip that never
// gets its password, a cleanup that 404s, a watcher that never switches. Naming
// the cause here is the difference between a five-minute diagnosis and an hour
// spent hunting a phantom.
const THROTTLE_HELP = [
  'being rate-limited (HTTP 429): the per-IP write budget is exhausted.',
  '  Each run of this script costs ~10 writes; the default budget is 30/hour.',
  '  Wait for the window, or raise CLIP_WRITE_PER_HOUR on the instance under test.',
  '  Do not read the failures below as product bugs.',
].join('\n    ');

function failThrottled(where, status) {
  if (status !== 429) return false;
  console.log(`\n  ${where}: ${THROTTLE_HELP}`);
  process.exit(3);
}

async function getJSON(url, method = 'GET') {
  const res = await fetch(url, { method });
  const body = await res.text();
  try {
    return JSON.parse(body);
  } catch {
    throw new Error(`${url} -> ${body.slice(0, 120)}`);
  }
}

class Session {
  constructor(ws) {
    this.ws = ws;
    this.id = 0;
    this.pending = new Map();
    this.console = [];
    this.failed = [];
    this.dialogs = [];
    ws.onmessage = (ev) => {
      // A malformed or torn frame must not throw inside an event handler: the
      // exception would be swallowed by the event loop, the pending CDP call
      // would hang until its timeout, and the failure would look like a browser
      // that stopped responding rather than like a bad frame.
      let m;
      try {
        m = JSON.parse(ev.data);
      } catch {
        return;
      }
      if (m.id && this.pending.has(m.id)) {
        const { resolve, reject } = this.pending.get(m.id);
        this.pending.delete(m.id);
        if (m.error) {
          reject(new Error(JSON.stringify(m.error)));
        } else {
          resolve(m.result);
        }
        return;
      }
      // Console errors are how a CSP violation shows up: the page silently loses
      // its behaviour instead of failing loudly. That is exactly the class of
      // failure the earlier UI round shipped once, so it is asserted here.
      if (m.method === 'Page.javascriptDialogOpening') {
        // The page uses confirm() for destructive actions. A headless run has no
        // user to answer, and an unanswered dialog blocks the renderer — which
        // would present as a hung click rather than as a failed assertion. Accept
        // it and record the message, so a test can still assert the confirmation
        // was shown.
        this.dialogs.push(m.params.message);
        ws.send(JSON.stringify({ id: ++this.id, method: 'Page.handleJavaScriptDialog', params: { accept: true } }));
      }
      if (m.method === 'Runtime.consoleAPICalled' && ['error', 'warning'].includes(m.params.type)) {
        this.console.push(m.params.args.map((a) => a.value ?? a.description ?? '').join(' '));
      }
      if (m.method === 'Log.entryAdded' && ['error', 'warning'].includes(m.params.entry.level)) {
        this.console.push(`${m.params.entry.source}: ${m.params.entry.text}`);
      }
      if (m.method === 'Network.loadingFailed') {
        this.failed.push(`${m.params.type} ${m.params.errorText}`);
      }
    };
  }

  send(method, params = {}) {
    const id = ++this.id;
    return new Promise((resolve, reject) => {
      this.pending.set(id, { resolve, reject });
      this.ws.send(JSON.stringify({ id, method, params }));
      setTimeout(() => {
        if (this.pending.has(id)) {
          this.pending.delete(id);
          reject(new Error(`CDP timeout: ${method}`));
        }
      }, 30000);
    });
  }

  async eval(expression) {
    const r = await this.send('Runtime.evaluate', {
      expression,
      awaitPromise: true,
      returnByValue: true,
    });
    if (r.exceptionDetails) throw new Error(JSON.stringify(r.exceptionDetails).slice(0, 300));
    return r.result.value;
  }

  async goto(url) {
    await this.send('Page.navigate', { url });
    for (let i = 0; i < 100; i += 1) {
      await sleep(150);
      try {
        if ((await this.eval('document.readyState')) === 'complete') return;
      } catch {
        /* still navigating */
      }
    }
    throw new Error(`page never loaded: ${url}`);
  }

  /** Real input events: a synthetic .click() produces no user activation. */
  async realClick(selectorExpr) {
    const box = await this.eval(`(() => {
      const el = ${selectorExpr};
      if (!el) return null;
      el.scrollIntoView({ block: 'center' });
      const r = el.getBoundingClientRect();
      return r.width || r.height ? { x: r.left + r.width / 2, y: r.top + r.height / 2 } : null;
    })()`);
    if (!box) throw new Error(`nothing to click: ${selectorExpr}`);
    for (const type of ['mousePressed', 'mouseReleased']) {
      await this.send('Input.dispatchMouseEvent', { type, x: box.x, y: box.y, button: 'left', clickCount: 1 });
    }
  }

  async shot(name) {
    try {
      const { data } = await this.send('Page.captureScreenshot', { format: 'png' });
      fs.writeFileSync(path.join(OUT, name), Buffer.from(data, 'base64'));
    } catch {
      // A screenshot is evidence, not an assertion: a slow frame in headless
      // mode must not turn a passing run red.
    }
  }
}

async function newPage(port) {
  const t = await getJSON(`http://127.0.0.1:${port}/json/new?about:blank`, 'PUT');
  const ws = new WebSocket(t.webSocketDebuggerUrl);
  await new Promise((res, rej) => {
    ws.onopen = res;
    ws.onerror = rej;
  });
  const s = new Session(ws);
  await s.send('Page.enable');
  await s.send('Runtime.enable');
  await s.send('Log.enable').catch(() => {});
  await s.send('Network.enable').catch(() => {});
  await s.send('Emulation.setFocusEmulationEnabled', { enabled: true }).catch(() => {});
  return s;
}

/** Waits for a predicate evaluated in the page. */
async function waitFor(session, expr, timeoutMs = 10000) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    try {
      if (await session.eval(expr)) return true;
    } catch {
      /* page busy */
    }
    await sleep(200);
  }
  return false;
}

const TEXT = 'from machine A: kubectl get pods -A\nsecond line\n';

async function main() {
  fs.mkdirSync(OUT, { recursive: true });
  const port = await new Promise((res) => {
    const s = net.createServer();
    s.listen(0, '127.0.0.1', () => {
      const p = s.address().port;
      s.close(() => res(p));
    });
  });

  const chrome = spawn(
    CHROME,
    ['--headless=new', '--no-sandbox', '--disable-gpu', '--no-first-run', '--disable-crash-reporter',
     `--remote-debugging-port=${port}`, `--user-data-dir=${path.join(OUT, `clip-${port}`)}`, 'about:blank'],
    { stdio: 'ignore', detached: true },
  );

  let writer;
  let reader;
  try {
    for (let i = 0; i < 60; i += 1) {
      try {
        await getJSON(`http://127.0.0.1:${port}/json/version`);
        break;
      } catch {
        await sleep(250);
      }
    }

    // ---- machine A: the first visit creates the clip -----------------------
    writer = await newPage(port);
    await writer.send('Browser.grantPermissions', {
      origin: BASE,
      permissions: ['clipboardReadWrite', 'clipboardSanitizedWrite'],
    }).catch(() => {});
    await writer.goto(`${BASE}/${SFX}`);

    check('page loads with the expected title', (await writer.eval('document.title')).includes(SFX));
    check('page renders in the create state', (await writer.eval('document.body.dataset.state')) === 'new');
    check('textarea is editable on first visit', (await writer.eval('document.getElementById("body").readOnly')) === false);
    check('save button is visible', (await writer.eval('!document.getElementById("save").hidden')) === true);
    check('long poll is NOT running for an unwritten suffix',
      (await writer.eval('typeof window.__polling === "undefined" || window.__polling !== true')) === true);

    // Type for real, then click for real.
    await writer.realClick('document.getElementById("body")');
    await writer.send('Input.insertText', { text: TEXT });
    check('typed text landed in the textarea', (await writer.eval('document.getElementById("body").value')) === TEXT);

    // Autosave: type only, never click. This is the webnote-parity behaviour and
    // the one thing a Go test cannot check — the debounce, the fetch and the
    // status line have to work together in a real browser.
    const autoSaved = await waitFor(writer,
      'document.getElementById("status").textContent.includes("已自动保存")');
    if (!autoSaved) {
      // The page reports a 429 in its own words, so it can be told apart from a
      // real autosave failure without guessing.
      const msg = await writer.eval('document.getElementById("status").textContent');
      if (msg.includes('频繁')) {
        console.log(`\n  autosave: ${THROTTLE_HELP}`);
        process.exit(3);
      }
    }
    check('typing alone triggers autosave (no click)', autoSaved,
      await writer.eval('document.getElementById("status").textContent'));
    check('no credential is kept in localStorage any more',
      (await writer.eval('Object.keys(localStorage).filter((k) => k.startsWith("relay.clip")).length')) === 0);
    check('save button stayed visible',
      (await writer.eval('!document.getElementById("save").hidden')) === true);
    await writer.shot('clip-writer.png');

    // The text reached the database, not just the DOM.
    const raw = await fetchText(`${BASE}/${SFX}/raw`);
    check('the server has the exact bytes', raw === TEXT, JSON.stringify(raw.slice(0, 40)));

    // ---- machine B: a different browser, no password set --------------------
    // Under the model adopted on 2026-10-16 a clip with no password is fully
    // public, so the second browser is NOT read-only. That is asserted rather
    // than skipped: it is the behaviour most likely to be mistaken for a bug
    // later, and it is the price of the simple "password or nothing" model.
    reader = await newPage(port);
    await reader.goto(`${BASE}/${SFX}`);

    check('the second browser sees the text', (await reader.eval('document.getElementById("body").value')) === TEXT);
    check('a passwordless clip is editable in a second browser too',
      (await reader.eval('document.getElementById("body").readOnly')) === false);
    check('the second browser is told the clip is public',
      (await reader.eval('document.getElementById("hint").textContent')).includes('公开'));
    await reader.shot('clip-reader.png');

    // ---- live update: A writes, B is watching -------------------------------
    const before = await reader.eval('document.getElementById("body").value');
    const updated = `${TEXT}appended from machine A\n`;
    const put = await fetchJSON(`${BASE}/api/clips/${SFX}`, 'PUT', {
      body: JSON.stringify({ body: updated }),
      headers: { 'Content-Type': 'application/json' },
    });
    check('a public clip accepts a credential-free update', put.status === 200,
      `status ${put.status} ${put.body.slice(0, 120)}`);
    failThrottled('PUT /api/clips', put.status);

    const live = await waitFor(reader, `document.getElementById("body").value.includes('appended from machine A')`, 35000);
    check('the watching reader updated without a reload', live,
      `before=${JSON.stringify(before.slice(0, 20))}`);
    check('the reader was told it synced',
      (await reader.eval('document.getElementById("status").textContent')).length > 0,
      await reader.eval('document.getElementById("status").textContent'));

    // ---- set a password through the real UI ---------------------------------
    const PW = 'e2e-pass-9f3a';
    await writer.eval(`(() => {
      document.getElementById('newpw').value = ${JSON.stringify(PW)};
      document.getElementById('setpw').click();
      return true;
    })()`);
    const pwSet = await waitFor(writer,
      'document.getElementById("status").textContent.includes("已设密码")');
    check('setting a password from the panel reports success', pwSet,
      await writer.eval('document.getElementById("status").textContent'));
    check('the password is not persisted in localStorage',
      (await writer.eval('Object.keys(localStorage).filter((k) => k.startsWith("relay.clip")).length')) === 0);

    // ---- machine C: the clip is now locked ----------------------------------
    // A THIRD browser: the second one's DOM still shows the old text, and a
    // fresh page is what a stranger actually gets.
    const stranger = await newPage(port);
    await stranger.goto(`${BASE}/${SFX}`);
    check('a protected clip renders in the locked state',
      (await stranger.eval('document.body.dataset.state')) === 'locked');
    check('the locked page has a password input',
      (await stranger.eval('!!document.getElementById("pw")')) === true);
    // The decisive one: the body must not be in the HTML at all. Rendering it and
    // hiding it with CSS would make the password decoration.
    const strangerHtml = await stranger.eval('document.documentElement.outerHTML');
    check('the locked HTML does not contain the clip body',
      !strangerHtml.includes('appended from machine A'));
    check('the locked page hides the editor',
      (await stranger.eval('document.getElementById("content").hidden')) === true);
    await stranger.shot('clip-locked.png');

    // Wrong password: refused, with a message, and still no content.
    await stranger.eval(`(() => {
      document.getElementById('pw').value = 'not-the-password';
      document.getElementById('unlock').click();
      return true;
    })()`);
    // Wait for a TERMINAL state, not merely for non-empty text: the handler sets
    // "解锁中…" synchronously, so waiting for "non-empty" would fire the next
    // attempt while this one is still in flight — which is how two overlapping
    // unlock requests masked a real race in the page (see
    // .ai/pitfalls/cases/2026-10-16-overlapping-unlock-attempts.md).
    const refused = await waitFor(stranger,
      '(() => { const t = document.getElementById("lockstatus").textContent; return t.length > 0 && !t.includes("解锁中"); })()');
    check('a wrong password is refused with a message', refused,
      await stranger.eval('document.getElementById("lockstatus").textContent'));
    check('a refused unlock reveals no content',
      (await stranger.eval('document.getElementById("content").hidden')) === true);

    // Then the right one.
    await stranger.eval(`(() => {
      document.getElementById('pw').value = ${JSON.stringify(PW)};
      document.getElementById('unlock').click();
      return true;
    })()`);
    const unlocked = await waitFor(stranger,
      'document.getElementById("body").value.includes("appended from machine A")');
    // The stale "密码不对。" from the previous attempt must be cleared by a
    // successful unlock, or the page shows an error next to content that is
    // plainly there (observed: a green run whose detail text read "密码不对。").
    check('a successful unlock clears the previous error',
      (await stranger.eval('document.getElementById("lockstatus").textContent')) === '',
      await stranger.eval('document.getElementById("lockstatus").textContent'));
    check('the right password reveals the content', unlocked,
      await stranger.eval('document.getElementById("lockstatus").textContent'));
    if (!unlocked) {
      // The failure that matters here is a page that unlocked and THEN lost the
      // content again (a later 401 calling enterLocked). Those two states look
      // identical from the outside, so print what the page actually saw.
      console.log('    debug: stranger console =', JSON.stringify(stranger.console, null, 2));
      console.log('    debug: stranger failed  =', JSON.stringify(stranger.failed, null, 2));
      console.log('    debug: stranger state   =', await stranger.eval(
        'JSON.stringify({state: document.body.dataset.state, hidden: document.getElementById("content").hidden, body: document.getElementById("body").value.slice(0,20), msg: document.getElementById("lockstatus").textContent})'));
    }

    // ---- the machine-readable paths are gated too ---------------------------
    check('GET /raw without the password is refused',
      (await fetchStatus(`${BASE}/${SFX}/raw`)) !== 200);
    const gatedRaw = await fetchText(`${BASE}/${SFX}/raw`);
    check('the refused /raw does not leak the body',
      !gatedRaw.includes('appended from machine A'), JSON.stringify(gatedRaw.slice(0, 60)));
    const okRaw = await fetchJSON(`${BASE}/${SFX}/raw`, 'GET', {
	    headers: { 'X-Clip-Password': PW },
    });
    check('GET /raw with the password returns the bytes',
      okRaw.status === 200 && okRaw.body.includes('appended from machine A'),
      `status ${okRaw.status}`);
    check('GET /api without the password is refused',
      (await fetchStatus(`${BASE}/api/clips/${SFX}`)) !== 200);

    // ---- image upload by pasting into the page ------------------------------
    const pngB64 = Buffer.concat([
      Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]),
      Buffer.alloc(64, 7),
    ]).toString('base64');
    const pasted = await writer.eval(`(async () => {
      const bin = atob('${pngB64}');
      const bytes = new Uint8Array(bin.length);
      for (let i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i);
      const dt = new DataTransfer();
      dt.items.add(new File([bytes], 'shot.png', { type: 'image/png' }));
      document.dispatchEvent(new ClipboardEvent('paste', { clipboardData: dt, bubbles: true, cancelable: true }));
      return true;
    })()`);
    check('a pasted image dispatches the paste handler', pasted === true);
    const uploaded = await waitFor(writer, 'document.querySelectorAll("#thumbs img").length === 1');
    check('the image appears as a thumbnail', uploaded,
      await writer.eval('document.getElementById("status").textContent'));
    const thumbSrc = await writer.eval('document.querySelector("#thumbs img")?.src || ""');
    check('thumbnail points at the raw asset route', thumbSrc.endsWith(`/${SFX}/raw/0`), thumbSrc);
    check('the image route is gated too',
      (await fetchStatus(`${BASE}/${SFX}/raw/0`)) !== 200);
    const imgStatus = await fetchStatus(`${BASE}/${SFX}/raw/0`, { 'X-Clip-Password': PW });
    check('the image is served back with the password', imgStatus === 200, `status ${imgStatus}`);
    await writer.shot('clip-with-image.png');

    // ---- share links: the read-only second entrance (/s/<token>) ------------
    //
    // The owner mints a share from their own page, a DIFFERENT browser opens it,
    // and what that browser can do is the whole assertion: read, never write. A
    // share that is merely "missing a button" would pass a DOM check and fail a
    // real request, so the write attempts below go over HTTP as well.
    const SHARE_PW = 'share-pw-77c1';
    await writer.eval('(() => { document.getElementById("sharebox").open = true; return true; })()');
    const panelOpen = await waitFor(writer, '!document.getElementById("sharebox").hidden');
    check('the owner page offers a share panel once the clip is readable', panelOpen);
    // Opening the panel loads the (empty) list; wait for that before creating, so
    // the "created" assertion below cannot be satisfied by the wrong reload.
    await waitFor(writer, 'document.getElementById("sharelist").textContent.includes("还没有")');

    await writer.eval(`(() => {
      document.getElementById('sharepw').value = ${JSON.stringify(SHARE_PW)};
      document.getElementById('createshare').click();
      return true;
    })()`);
    const shareCreated = await waitFor(writer,
      'document.getElementById("sharestatus").textContent.includes("已创建")', 20000);
    check('the owner can create a share link from the page', shareCreated,
      await writer.eval('document.getElementById("sharestatus").textContent'));
    failThrottled('POST shares', shareCreated ? 0 : 429);

    const shareURL = await writer.eval('document.querySelector("#sharelist a")?.href || ""');
    check('the share link has the /s/<token> shape', /\/s\/[A-Za-z0-9_-]{16,}$/.test(shareURL), shareURL);
    const shareToken = shareURL.split('/s/')[1] || '';

    // A fresh browser with no sessionStorage: this is what a colleague gets.
    const sharer = await newPage(port);
    await sharer.goto(shareURL);
    check('a share renders in the locked state', (await sharer.eval('document.body.dataset.state')) === 'locked');
    check('the locked share page has a secret input',
      (await sharer.eval('!!document.getElementById("pw")')) === true);
    const shareHtml = await sharer.eval('document.documentElement.outerHTML');
    check('the locked share HTML does not contain the body', !shareHtml.includes('appended from machine A'));
    check('the share page does not reveal the clip suffix', !shareHtml.includes(SFX));
    check('the share page offers no share-management panel',
      (await sharer.eval('!document.getElementById("sharebox")')) === true);
    check('the share page cannot be mistaken for the editor',
      (await sharer.eval('document.getElementById("save").hidden')) === true);

    // Wrong secret, then the right one.
    await sharer.eval(`(() => {
      document.getElementById('pw').value = 'not-the-share-secret';
      document.getElementById('unlock').click();
      return true;
    })()`);
    await waitFor(sharer,
      '(() => { const t = document.getElementById("lockstatus").textContent; return t.length > 0 && !t.includes("解锁中"); })()');
    check('a wrong share secret is refused and reveals nothing',
      (await sharer.eval('document.getElementById("content").hidden')) === true);

    await sharer.eval(`(() => {
      document.getElementById('pw').value = ${JSON.stringify(SHARE_PW)};
      document.getElementById('unlock').click();
      return true;
    })()`);
    const shareUnlocked = await waitFor(sharer,
      'document.getElementById("body").value.includes("appended from machine A")');
    check('the share secret reveals the content', shareUnlocked,
      await sharer.eval('document.getElementById("lockstatus").textContent'));
    check('the shared content is read-only in the visitor browser',
      (await sharer.eval('document.getElementById("body").readOnly')) === true);
    check('the share view offers no save button',
      (await sharer.eval('document.getElementById("save").hidden')) === true);
    check('the share view offers no delete button',
      (await sharer.eval('document.getElementById("del").hidden')) === true);
    check('the share view offers no password controls',
      (await sharer.eval('!document.getElementById("setpw")')) === true);
    await sharer.shot('clip-share-reader.png');

    // The decisive read-only check: the write shapes have no route at all. A
    // DOM-only assertion ("the button is hidden") would pass even if the endpoint
    // existed and merely had no button pointing at it.
    for (const method of ['PUT', 'POST', 'DELETE']) {
      const w = await fetchJSON(`${BASE}/s/${shareToken}`, method, {
        headers: { 'Content-Type': 'application/json', 'X-Clip-Password': SHARE_PW },
        body: JSON.stringify({ body: 'defaced through the share' }),
      });
      check(`a share refuses ${method} with 405`, w.status === 405, `status ${w.status}`);
    }
    check('the attempted write did not reach the content',
      (await fetchText(`${BASE}/s/${shareToken}/raw`, { 'X-Clip-Password': SHARE_PW }))
        .includes('appended from machine A'));

    // The share secret is INDEPENDENT of the master password — that is the whole
    // reason the feature exists — so each must open exactly its own entrance.
    check('the master password does not open the share',
      (await fetchStatus(`${BASE}/s/${shareToken}/raw`, { 'X-Clip-Password': PW })) !== 200);
    check('the share secret does not open the clip',
      (await fetchStatus(`${BASE}/${SFX}/raw`, { 'X-Clip-Password': SHARE_PW })) !== 200);
    check('the share secret opens the share /raw',
      (await fetchStatus(`${BASE}/s/${shareToken}/raw`, { 'X-Clip-Password': SHARE_PW })) === 200);
    check('the share /raw without the secret is refused',
      (await fetchStatus(`${BASE}/s/${shareToken}/raw`)) !== 200);

    // ---- revoking a share kills it immediately -----------------------------
    await writer.eval(`(() => {
      const b = [...document.querySelectorAll('#sharelist button')].find((x) => x.textContent === '撤销');
      if (!b) return false;
      b.click();
      return true;
    })()`);
    const revoked = await waitFor(writer,
      'document.getElementById("sharelist").textContent.includes("还没有")', 20000);
    check('the owner can revoke a share from the page', revoked,
      await writer.eval('document.getElementById("sharestatus").textContent'));
    check('revoking asks for confirmation first', writer.dialogs.length > 0);
    check('a revoked share is immediately dead',
      (await fetchStatus(`${BASE}/s/${shareToken}/raw`, { 'X-Clip-Password': SHARE_PW })) === 404);
    // And the visitor's open page notices, clearing content it may no longer show.
    const shareDead = await waitFor(sharer,
      'document.getElementById("hint").textContent.includes("失效")', 35000);
    check('the visitor page notices the revocation', shareDead,
      await sharer.eval('document.getElementById("hint").textContent'));
    check('and clears the content it may no longer show',
      (await sharer.eval('document.getElementById("body").value')) === '');

    // ---- the pickup-code mode ----------------------------------------------
    await writer.eval(`(() => {
      document.getElementById('sharecode').checked = true;
      document.getElementById('sharepw').value = '';
      document.getElementById('createshare').click();
      return true;
    })()`);
    const codeShown = await waitFor(writer,
      'document.getElementById("sharecodeout").hidden === false', 20000);
    const code = await writer.eval('document.getElementById("sharecodevalue").value');
    check('a pickup-code share shows a 6-digit code', codeShown && /^[0-9]{6}$/.test(code), code);
    const codeURL = await writer.eval('document.querySelector("#sharelist a")?.href || ""');
    const codeToken = codeURL.split('/s/')[1] || '';
    check('the pickup code opens the share',
      (await fetchStatus(`${BASE}/s/${codeToken}/raw`, { 'X-Clip-Password': code })) === 200,
      `status with code ${code}`);
    check('a wrong pickup code does not open the share',
      (await fetchStatus(`${BASE}/s/${codeToken}/raw`, { 'X-Clip-Password': '000000' })) !== 200);

    // ---- console hygiene ----------------------------------------------------
    // Two kinds of noise are expected, and each is paired below with an
    // assertion that it was the expected thing and was handled:
    //
    //   - a 401 on the long poll: the clip became protected while these pages
    //     were watching it, so the server (correctly) refused the request that
    //     was already parked. Chrome logs every failed resource load, so this
    //     shows up as console noise even though the page handles it.
    //     Matched on "status of 401" rather than on the full message: the reason
    //     phrase is an HTTP/1.1 thing. Locally this reads "401 (Unauthorized)",
    //     but the deployed service answers over HTTP/2, where it is just
    //     "401 ()" — a pattern written from the local observation silently stops
    //     matching in production.
    //   - net::ERR_ABORTED: the page tears down its own parked poll when the
    //     credential changes, rather than leaving a request in flight that was
    //     authorised with a credential nobody holds any more.
    const IGNORE = ['status of 401', 'net::ERR_ABORTED', 'ERR_ABORTED'];
    const noise = (list) => list.filter((l) => !l.includes('favicon') && !IGNORE.some((p) => l.includes(p)));
    const writerNoise = noise(writer.console);
    const readerNoise = noise(reader.console);
    check('no console errors or CSP violations on the writer page', writerNoise.length === 0, writerNoise.join(' | '));
    check('no console errors or CSP violations on the reader page', readerNoise.length === 0, readerNoise.join(' | '));
    check('no unexpected failed requests',
      noise(writer.failed).length === 0 && noise(reader.failed).length === 0,
      [...noise(writer.failed), ...noise(reader.failed)].join(' | '));

    // The ignored 401 was the interesting case, so it is asserted rather than
    // merely tolerated: a page that was watching a public clip when it became
    // protected must switch to the lock prompt and STOP showing the old text —
    // not sit on a stale copy of a clip that is no longer public.
    const watcherLocked = await waitFor(reader,
      'document.getElementById("lockbox").hidden === false', 15000);
    check('a watcher of a newly protected clip is switched to the lock prompt', watcherLocked,
      await reader.eval('document.getElementById("lockstatus").textContent'));
    check('and the stale content is cleared from the page',
      (await reader.eval('document.getElementById("body").value')) === '',
      JSON.stringify((await reader.eval('document.getElementById("body").value')).slice(0, 40)));
    check('and it is told why',
      (await reader.eval('document.getElementById("lockstatus").textContent')).includes('密码'));
    await reader.shot('clip-reader-locked.png');

    // ---- cleanup ------------------------------------------------------------
    // The clip now has a password, so the cleanup delete needs it — which is
    // itself a check: a protected clip must not be deletable without it.
    const noPwDelete = await fetchJSON(`${BASE}/api/clips/${SFX}`, 'DELETE');
    check('a protected clip cannot be deleted without the password',
      noPwDelete.status === 401 || noPwDelete.status === 404, `status ${noPwDelete.status}`);
    const del = await fetchJSON(`${BASE}/api/clips/${SFX}`, 'DELETE', {
      headers: { 'X-Clip-Password': PW },
    });
    failThrottled('DELETE /api/clips', del.status);
    check('cleanup: the test clip was deleted', del.status === 204, `status ${del.status}`);
  } finally {
    try {
      process.kill(-chrome.pid);
    } catch {
      /* already gone */
    }
  }

  const failed = results.filter((r) => !r.ok);
  console.log(`\n${results.length - failed.length}/${results.length} passed`);
  process.exit(failed.length ? 1 : 0);
}

// Two functions rather than one with a flag: the call sites read as what they
// want ("the status" vs "the body"), and neither can be called with the wrong
// mode by accident.
async function fetchText(url, headers) {
  const res = await fetch(url, { redirect: 'manual', headers: headers || {} });
  return await res.text();
}

async function fetchStatus(url, headers) {
  const res = await fetch(url, { redirect: 'manual', headers: headers || {} });
  return res.status;
}

async function fetchJSON(url, method, opts = {}) {
  const res = await fetch(url, {
    method,
    headers: opts.headers || {},
    body: opts.body,
  });
  return { status: res.status, body: await res.text() };
}

main().catch((e) => {
  console.error('verification crashed:', e.message);
  process.exit(2);
});
