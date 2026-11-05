// PWA tests (day phase 6): sw.js is run in a sandbox and its routing is
// checked request by request (never /api, /v1, /preview or event streams;
// only the shell list is cached), its NEVER table is parsed from the
// source, and the manifest and icons are checked.
// Run: node --test web/test/*.test.mjs
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync, existsSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import path from 'node:path';
import vm from 'node:vm';
import { updateReady } from '../dist/assets/js/pwa-core.js';

const DIST = path.join(path.dirname(fileURLToPath(import.meta.url)), '..', 'dist');
const SRC = readFileSync(path.join(DIST, 'sw.js'), 'utf8');
const ORIGIN = 'https://host.tail1234.ts.net';

// loadSW runs sw.js with a fake worker global and returns its listeners
// and the test hook.
function loadSW(env = {}) {
  const listeners = {};
  const self = {
    location: new URL(`${ORIGIN}/sw.js`),
    addEventListener: (type, fn) => { listeners[type] = fn; },
    clients: { claim: async () => {}, matchAll: async () => [], openWindow: async () => {} },
    skipWaiting: () => {},
  };
  const ctx = vm.createContext({ self, URL, Request: class {}, Response: class {}, caches: env.caches || {}, fetch: env.fetch || (() => { throw new Error('no network in tests'); }), console });
  vm.runInContext(SRC, ctx, { filename: 'sw.js' });
  return { listeners, sw: self.cerberusSW };
}

const req = (url, { method = 'GET', mode = 'cors', accept = '*/*' } = {}) => ({
  url: url.startsWith('http') ? url : ORIGIN + url, method, mode,
  headers: { get: (k) => (k.toLowerCase() === 'accept' ? accept : null) },
});

// fetchEvent dispatches a fake fetch event and reports whether the worker
// answered it (respondWith) or let it pass.
function answered(listeners, r) {
  let called = false;
  listeners.fetch({ request: r, respondWith: (p) => { called = true; Promise.resolve(p).catch(() => {}); } });
  return called;
}

test('sw.js never answers /api, /v1, /preview, /healthz, itself or event streams', () => {
  const { listeners, sw } = loadSW();
  const never = [
    '/api', '/api/', '/api/status', '/api/events', '/api/sessions/abc/stream', '/api/inbox?unread=1',
    '/api/sessions/abc/runs/1.cast', '/api/downloads', '/api/monitor/stream', '/api/feed/stream',
    '/v1', '/v1/', '/v1/models', '/v1/chat/completions', '/preview/abc/', '/preview/abc/index.html',
    '/healthz', '/sw.js',
  ];
  for (const u of never) {
    for (const mode of ['cors', 'navigate', 'same-origin']) {
      const r = req(u, { mode });
      assert.equal(sw.route(r, ORIGIN), 'pass', `${u} (${mode}) must pass`);
      assert.equal(answered(listeners, r), false, `${u} (${mode}) must not be answered`);
    }
  }
  // Any event stream, whatever its path.
  for (const u of ['/', '/assets/app.css', '/anything']) {
    const r = req(u, { accept: 'text/event-stream' });
    assert.equal(sw.route(r, ORIGIN), 'pass', `${u} as SSE`);
    assert.equal(answered(listeners, r), false);
  }
});

test('sw.js passes writes, other origins and queries', () => {
  const { sw } = loadSW();
  for (const m of ['POST', 'PUT', 'PATCH', 'DELETE']) assert.equal(sw.route(req('/assets/app.css', { method: m }), ORIGIN), 'pass');
  assert.equal(sw.route(req('https://example.com/assets/app.css'), ORIGIN), 'pass');
  // ?token= login must reach the server (it sets the cookie).
  assert.equal(sw.route(req('/?token=abc', { mode: 'navigate' }), ORIGIN), 'pass');
  assert.equal(sw.route(req('/assets/app.css?v=1'), ORIGIN), 'pass');
  assert.equal(sw.route(null, ORIGIN), 'pass');
  assert.equal(sw.route(req('not a url'), ORIGIN), 'pass');
});

test('sw.js caches only the shell list and serves the shell for navigations', () => {
  const { listeners, sw } = loadSW();
  assert.equal(sw.route(req('/', { mode: 'navigate' }), ORIGIN), 'navigate');
  assert.equal(sw.route(req('/login', { mode: 'navigate' }), ORIGIN), 'navigate');
  assert.equal(sw.route(req('/other', { mode: 'navigate' }), ORIGIN), 'pass');
  assert.equal(sw.route(req('/assets/app.css'), ORIGIN), 'shell');
  assert.equal(sw.route(req('/assets/js/app.js'), ORIGIN), 'shell');
  assert.equal(sw.route(req('/manifest.webmanifest'), ORIGIN), 'shell');
  assert.equal(sw.route(req('/assets/icons/icon-192.png'), ORIGIN), 'shell');
  assert.equal(sw.route(req('/assets/not-there.js'), ORIGIN), 'pass');
  assert.equal(answered(listeners, req('/assets/app.css')), true);
  for (const p of sw.SHELL) {
    assert.ok(!/^\/(api|v1|preview)(\/|$)/.test(p), `shell lists ${p}`);
    assert.ok(p === '/' || existsSync(path.join(DIST, p === '/manifest.webmanifest' ? 'manifest.webmanifest' : p === '/carino-clock.js' ? 'assets/carino-clock.js' : p.slice(1))), `shell file ${p} exists`);
  }
  assert.ok(sw.SHELL.includes('/') && sw.SHELL.includes('/assets/js/app.js') && sw.SHELL.includes('/assets/vendor/xterm/xterm.mjs'));
  assert.match(sw.CACHE, /^cerberus-shell-[0-9a-f]{12}$/);
  assert.equal(sw.CACHE, `cerberus-shell-${sw.BUILD}`);
});

test('sw.js routing table, parsed from the source, lists /api and /v1', () => {
  const never = /const NEVER = \[([^\]]*)\]/.exec(SRC);
  const exact = /const NEVER_EXACT = \[([^\]]*)\]/.exec(SRC);
  assert.ok(never && exact, 'NEVER tables present');
  const list = (m) => [...m[1].matchAll(/'([^']+)'/g)].map((x) => x[1]);
  assert.deepEqual(list(never).sort(), ['/api/', '/preview/', '/v1/']);
  for (const p of ['/api', '/v1', '/healthz', '/sw.js']) assert.ok(list(exact).includes(p), `NEVER_EXACT has ${p}`);
  assert.match(SRC, /text\/event-stream/);
  // The only cache writes are the precache of SHELL.
  const puts = SRC.match(/cache\.put\(/g) || [];
  assert.equal(puts.length, 1);
  assert.match(SRC, /SHELL\.map\(async \(path\) => \{[\s\S]*?cache\.put\(path/);
});

test('sw.js skip-waiting message activates the waiting worker', () => {
  const { listeners } = loadSW();
  let skipped = false;
  const ctxSelf = { skip: () => { skipped = true; } };
  // Re-run with a skipWaiting spy.
  const ls = {};
  const self = { location: new URL(`${ORIGIN}/sw.js`), addEventListener: (t, f) => { ls[t] = f; }, skipWaiting: ctxSelf.skip, clients: {} };
  vm.runInContext(SRC, vm.createContext({ self, URL, caches: {}, fetch() {}, Request: class {}, Response: class {} }));
  ls.message({ data: { type: 'nothing' } });
  assert.equal(skipped, false);
  ls.message({ data: { type: 'skip-waiting' } });
  assert.equal(skipped, true);
  assert.ok(listeners.install && listeners.activate && listeners.fetch && listeners.notificationclick);
});

test('manifest: name, scope, colours and icons that exist', () => {
  const m = JSON.parse(readFileSync(path.join(DIST, 'manifest.webmanifest'), 'utf8'));
  assert.equal(m.name, 'Cerberus');
  assert.ok(m.short_name);
  assert.equal(m.start_url, '/');
  assert.equal(m.scope, '/');
  assert.equal(m.display, 'standalone');
  assert.equal(m.background_color, '#050505');
  assert.equal(m.theme_color, '#050505');
  const sizes = new Set(m.icons.map((i) => `${i.sizes}/${i.purpose}`));
  for (const s of ['192x192/any', '512x512/any', '512x512/maskable']) assert.ok(sizes.has(s), s);
  for (const i of m.icons) {
    const file = path.join(DIST, i.src);
    assert.ok(existsSync(file), i.src);
    const png = readFileSync(file);
    assert.equal(png.subarray(1, 4).toString(), 'PNG');
    const [w, h] = [png.readUInt32BE(16), png.readUInt32BE(20)];
    assert.equal(`${w}x${h}`, i.sizes, `${i.src} is ${w}x${h}`);
  }
  const index = readFileSync(path.join(DIST, 'index.html'), 'utf8');
  assert.match(index, /<link rel="manifest" href="manifest.webmanifest">/);
});

test('updateReady offers a reload only when a worker waits behind a controller', () => {
  assert.equal(updateReady(null, true), false);
  assert.equal(updateReady({ waiting: null }, true), false);
  assert.equal(updateReady({ waiting: {} }, false), false);
  assert.equal(updateReady({ waiting: {} }, true), true);
});

// fakeRes is the part of a Response navigateResponse reads.
const fakeRes = (body, extra = {}) => ({
  ok: true, redirected: false, type: 'basic', ...extra, body,
  clone() { return this; }, text: async () => body,
});

test('sw.js keeps a navigation on the cached build while a newer index.html is served', async () => {
  const cached = fakeRes('<html>build A</html>');
  const caches = { open: async () => ({ match: async (k) => (k === '/' ? cached : undefined) }) };
  let net = fakeRes('<html>build A</html>');
  const { sw } = loadSW({ caches, fetch: async () => net });
  const nav = req('/', { mode: 'navigate' });
  // Same build: the network copy.
  assert.equal(await sw.navigateResponse(nav), net);
  // A new build on the host: this worker's page, matching its assets.
  net = fakeRes('<html>build B</html>');
  assert.equal(await sw.navigateResponse(nav), cached);
  // Redirects (login) and errors still come from the server.
  net = fakeRes('', { type: 'opaqueredirect', ok: false });
  assert.equal(await sw.navigateResponse(nav), net);
  net = fakeRes('down', { ok: false, status: 503 });
  assert.equal(await sw.navigateResponse(nav), net);
  // Offline: the cached shell.
  const off = loadSW({ caches, fetch: async () => { throw new TypeError('offline'); } });
  assert.equal(await off.sw.navigateResponse(nav), cached);
});
