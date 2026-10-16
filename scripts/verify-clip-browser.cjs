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
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

const results = [];
const check = (name, ok, detail = '') => {
  results.push({ name, ok, detail });
  console.log(`${ok ? 'PASS' : 'FAIL'}  ${name}${detail ? `  — ${detail}` : ''}`);
};

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
        m.error ? reject(new Error(JSON.stringify(m.error))) : resolve(m.result);
        return;
      }
      // Console errors are how a CSP violation shows up: the page silently loses
      // its behaviour instead of failing loudly. That is exactly the class of
      // failure the earlier UI round shipped once, so it is asserted here.
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

    await writer.realClick('document.getElementById("save")');
    const saved = await waitFor(writer,
      'document.getElementById("status").textContent.includes("已保存") || document.getElementById("status").textContent.includes("已创建并保存")');
    check('clicking 保存 reports success', saved,
      await writer.eval('document.getElementById("status").textContent'));
    check('a token was stored in localStorage',
      (await writer.eval(`(localStorage.getItem('relay.clip.${SFX}') || '').length >= 40`)) === true);
    check('save button stayed visible for the owner',
      (await writer.eval('!document.getElementById("save").hidden')) === true);

    // Lift the token into Node before touching storage: the reader step below
    // clears this origin's localStorage, and two tabs of one browser share it,
    // so reading it later would return null and every subsequent API call would
    // go out unauthenticated.
    const OWNER_TOKEN = await writer.eval(`localStorage.getItem('relay.clip.${SFX}')`);
    check('the owner token was captured for later steps', (OWNER_TOKEN || '').length >= 40);
    await writer.shot('clip-writer.png');

    // The text reached the database, not just the DOM.
    const raw = await fetchText(`${BASE}/${SFX}/raw`);
    check('the server has the exact bytes', raw === TEXT, JSON.stringify(raw.slice(0, 40)));

    // ---- machine B: a browser with no token ---------------------------------
    reader = await newPage(port);
    // Clear only this origin's localStorage so this page behaves like a
    // different machine rather than the one that created the clip.
    await reader.goto(`${BASE}/${SFX}`);
    await reader.send('Storage.clearDataForOrigin', { origin: BASE, storageTypes: 'local_storage' });
    await reader.goto(`${BASE}/${SFX}`);

    check('the second browser sees the text', (await reader.eval('document.getElementById("body").value')) === TEXT);
    check('and it is read-only there', (await reader.eval('document.getElementById("body").readOnly')) === true);
    check('no save button for a reader', (await reader.eval('document.getElementById("save").hidden')) === true);
    check('reader has no token in storage',
      (await reader.eval(`localStorage.getItem('relay.clip.${SFX}') === null`)) === true);
    check('reader is told why it is read-only',
      (await reader.eval('document.getElementById("hint").textContent')).includes('只读'));
    await reader.shot('clip-reader.png');

    // ---- live update: A writes, B is watching -------------------------------
    const before = await reader.eval('document.getElementById("body").value');
    const updated = `${TEXT}appended from machine A\n`;
    const put = await fetchJSON(`${BASE}/api/clips/${SFX}`, 'PUT', {
      body: JSON.stringify({ body: updated }),
      headers: {
        'Content-Type': 'application/json',
        'X-Relay-Clip-Token': OWNER_TOKEN,
      },
    });
    check('the owner can update over the API', put.status === 200, `status ${put.status} ${put.body.slice(0, 120)}`);

    const live = await waitFor(reader, `document.getElementById("body").value.includes('appended from machine A')`, 35000);
    check('the watching reader updated without a reload', live,
      `before=${JSON.stringify(before.slice(0, 20))}`);
    check('the reader was told it synced',
      (await reader.eval('document.getElementById("status").textContent')).length > 0,
      await reader.eval('document.getElementById("status").textContent'));

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
    const imgStatus = await fetchStatus(`${BASE}/${SFX}/raw/0`);
    check('the image is served back', imgStatus === 200, `status ${imgStatus}`);
    await writer.shot('clip-with-image.png');

    // ---- console hygiene ----------------------------------------------------
    const writerNoise = writer.console.filter((l) => !l.includes('favicon'));
    const readerNoise = reader.console.filter((l) => !l.includes('favicon'));
    check('no console errors or CSP violations on the writer page', writerNoise.length === 0, writerNoise.join(' | '));
    check('no console errors or CSP violations on the reader page', readerNoise.length === 0, readerNoise.join(' | '));
    check('no failed requests', writer.failed.length === 0 && reader.failed.length === 0,
      [...writer.failed, ...reader.failed].join(' | '));

    // ---- cleanup ------------------------------------------------------------
    const del = await fetchJSON(`${BASE}/api/clips/${SFX}`, 'DELETE', {
      headers: { 'X-Relay-Clip-Token': OWNER_TOKEN },
    });
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
async function fetchText(url) {
  const res = await fetch(url, { redirect: 'manual' });
  return await res.text();
}

async function fetchStatus(url) {
  const res = await fetch(url, { redirect: 'manual' });
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
