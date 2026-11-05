// Headless energy check (day phase 7 cross-cutting check): in a real
// browser, every EventSource and polling timer stops when the tab is
// hidden. Each polling view (Sessions with the activity feed open, Wall,
// Chat, Models, Monitor, Usage, Settings with Secrets and About) is mounted, the page is
// made hidden (document.hidden and a visibilitychange event, as a real
// browser does), and then nothing may still run: no open EventSource, no
// interval, no pending timeout of more than a second, and no request to the
// daemon in the next few seconds. Desktop alerts are off (the default), so
// the event stream has no reason to stay open. Showing the page again must
// reopen the event stream.
//
// Chat "busy" (a reply streaming) needs a model, so it is not covered
// here; its frame clock's hidden stop is unit-tested in chat.test.mjs.
//
// Skipped unless a daemon and a browser are given (it never starts either
// on its own); web/tools/pwa_headless.sh starts a throwaway daemon and
// runs it:
//   CERBERUS_HEADLESS_URL   the daemon's base URL, e.g. http://127.0.0.1:7381
//   CERBERUS_HEADLESS_TOKEN its token (the page logs in with ?token=)
//   CERBERUS_PLAYWRIGHT     path to a playwright package (one headless Chromium)
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { pathToFileURL } from 'node:url';
import path from 'node:path';

const BASE = process.env.CERBERUS_HEADLESS_URL || '';
const TOKEN = process.env.CERBERUS_HEADLESS_TOKEN || '';
const PW = process.env.CERBERUS_PLAYWRIGHT || '';

async function loadPlaywright() {
  if (!BASE || !TOKEN || !PW) return null;
  try {
    const mod = await import(pathToFileURL(path.join(PW, 'index.mjs')).href);
    return mod.chromium || (mod.default && mod.default.chromium) || null;
  } catch {
    return null;
  }
}

const chromium = await loadPlaywright();

// instrument runs in the page before any of its scripts: a controllable
// document.hidden, and a record of pending timers and open EventSources.
function instrument() {
  let hidden = false;
  Object.defineProperty(Document.prototype, 'hidden', { configurable: true, get: () => hidden });
  Object.defineProperty(Document.prototype, 'visibilityState', { configurable: true, get: () => (hidden ? 'hidden' : 'visible') });
  const pending = new Map();
  const st = window.setTimeout.bind(window);
  const si = window.setInterval.bind(window);
  const ct = window.clearTimeout.bind(window);
  const ci = window.clearInterval.bind(window);
  const where = () => (new Error().stack || '').split('\n').slice(2, 5).map((l) => l.trim()).join(' < ');
  window.setTimeout = function (fn, delay, ...args) {
    const id = st(function () {
      pending.delete(id);
      if (typeof fn === 'function') fn.apply(this, args);
    }, delay);
    pending.set(id, { kind: 'timeout', delay: Number(delay) || 0, at: where() });
    return id;
  };
  window.setInterval = function (fn, delay, ...args) {
    const id = si(fn, delay, ...args);
    pending.set(id, { kind: 'interval', delay: Number(delay) || 0, at: where() });
    return id;
  };
  window.clearTimeout = (id) => { pending.delete(id); ct(id); };
  window.clearInterval = (id) => { pending.delete(id); ci(id); };
  const Native = window.EventSource;
  const open = new Set();
  window.EventSource = class extends Native {
    constructor(...a) { super(...a); open.add(this); }
    close() { open.delete(this); super.close(); }
  };
  window.__energy = {
    setHidden(v) { hidden = v; document.dispatchEvent(new Event('visibilitychange')); },
    report() {
      return {
        streams: [...open].filter((e) => e.readyState !== 2).map((e) => new URL(e.url).pathname),
        timers: [...pending.values()],
      };
    },
  };
}

const VIEWS = [
  ['#/sessions', true],
  ['#/wall', false],
  ['#/chat', false],
  ['#/models', false],
  ['#/monitor', false],
  ['#/usage', false],
  ['#/settings/secrets', false],
  ['#/settings/about', false],
  ['#/settings', false],
];

const pause = (ms) => new Promise((r) => setTimeout(r, ms));

test('every stream and polling timer stops while the tab is hidden', { skip: chromium ? false : 'set CERBERUS_HEADLESS_URL, CERBERUS_HEADLESS_TOKEN and CERBERUS_PLAYWRIGHT' }, async () => {
  const browser = await chromium.launch({ headless: true });
  try {
    const page = await browser.newPage({ viewport: { width: 1280, height: 900 } });
    await page.addInitScript(instrument);
    let hiddenRequests = [];
    let watching = false;
    page.on('request', (r) => {
      const p = new URL(r.url()).pathname;
      if (watching && /^\/(api|v1)\//.test(p)) hiddenRequests.push(p);
    });
    await page.goto(`${BASE}/?token=${encodeURIComponent(TOKEN)}#/sessions`, { waitUntil: 'load' });
    // The navbar's clock re-scans for its element a few times after boot
    // (a short interval that ends by itself); let that finish first.
    await page.waitForTimeout(4500);
    for (const [hash, feed] of VIEWS) {
      await page.evaluate((h) => { location.hash = h; }, hash);
      await page.waitForTimeout(800);
      if (feed) {
        await page.click('.feed-btn');
        await page.waitForTimeout(400);
      }
      const before = await page.evaluate(() => window.__energy.report());
      assert.ok(before.streams.length > 0, `${hash}: the event stream is open while visible`);

      await page.evaluate(() => window.__energy.setHidden(true));
      await page.waitForTimeout(1500); // requests already in flight finish
      const after = await page.evaluate(() => window.__energy.report());
      assert.deepEqual(after.streams, [], `${hash}: streams still open while hidden`);
      const live = after.timers.filter((t) => t.kind === 'interval' || t.delay > 1000);
      assert.deepEqual(live, [], `${hash}: timers still pending while hidden`);
      hiddenRequests = [];
      watching = true;
      await pause(3000);
      watching = false;
      assert.deepEqual(hiddenRequests, [], `${hash}: requests made while hidden`);

      await page.evaluate(() => window.__energy.setHidden(false));
      await page.waitForTimeout(600);
      const back = await page.evaluate(() => window.__energy.report());
      assert.ok(back.streams.length > 0, `${hash}: the event stream reopens when visible`);
      if (feed) {
        await page.click('.feed-btn');
        await page.waitForTimeout(200);
      }
    }
  } finally {
    await browser.close();
  }
});
