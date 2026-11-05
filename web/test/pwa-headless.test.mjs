// Headless PWA check (day phase 6 acceptance): in a real browser, the
// service worker installs, the UI talks to /api (plain requests and the
// /api/events stream), and afterwards no cache under cerberus-shell-*
// holds an /api, /v1, /preview or /healthz response. The vm sandbox tests
// in pwa.test.mjs check route(); this one checks the real fetch path.
//
// Skipped unless a daemon and a browser are given (it never starts either
// on its own):
//   CERBERUS_HEADLESS_URL   the daemon's base URL, e.g. http://127.0.0.1:7381
//                           (localhost: service workers need a secure context)
//   CERBERUS_HEADLESS_TOKEN its token (the page logs in with ?token=)
//   CERBERUS_PLAYWRIGHT     path to a playwright package (one headless Chromium)
// web/tools/pwa_headless.sh starts a throwaway daemon and runs this file.
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

test('a real service worker never caches /api, /v1, /preview or event streams', { skip: chromium ? false : 'set CERBERUS_HEADLESS_URL, CERBERUS_HEADLESS_TOKEN and CERBERUS_PLAYWRIGHT' }, async () => {
  const browser = await chromium.launch({ headless: true });
  try {
    const page = await browser.newPage();
    await page.goto(`${BASE}/?token=${encodeURIComponent(TOKEN)}`, { waitUntil: 'load' });
    // Wait for the worker to control the page (the first load installs it;
    // a reload brings the page under it).
    await page.evaluate(() => navigator.serviceWorker.ready.then(() => true));
    if (!(await page.evaluate(() => !!navigator.serviceWorker.controller))) {
      await page.reload({ waitUntil: 'load' });
      await page.waitForFunction(() => !!navigator.serviceWorker.controller, null, { timeout: 15000 });
    }
    // Use the API through the controlled page: plain GETs, a 404 and the
    // event stream for a moment.
    const statuses = await page.evaluate(async () => {
      const out = [];
      for (const u of ['/api/status', '/api/inbox?limit=5', '/api/downloads', '/v1/models', '/healthz', '/preview/none/']) {
        try { out.push((await fetch(u, { credentials: 'same-origin' })).status); } catch { out.push(0); }
      }
      await new Promise((resolve) => {
        const es = new EventSource('/api/events');
        const done = () => { es.close(); resolve(); };
        es.onmessage = done;
        es.onopen = () => setTimeout(done, 300);
        es.onerror = done;
        setTimeout(done, 3000);
      });
      return out;
    });
    assert.equal(statuses[0], 200, `/api/status reached the daemon (${statuses})`);
    const keys = await page.evaluate(async () => {
      const out = [];
      for (const name of await caches.keys()) {
        const c = await caches.open(name);
        for (const r of await c.keys()) out.push([name, new URL(r.url).pathname]);
      }
      return out;
    });
    assert.ok(keys.length > 0, 'the shell was cached');
    for (const [name, p] of keys) {
      assert.ok(name.startsWith('cerberus-shell-'), `unexpected cache ${name}`);
      assert.ok(!/^\/(api|v1|preview)(\/|$)/.test(p) && p !== '/healthz' && p !== '/sw.js', `${p} must never be cached (${name})`);
    }
  } finally {
    await browser.close();
  }
});
