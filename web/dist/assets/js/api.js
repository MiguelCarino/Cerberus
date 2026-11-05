// Network layer: JSON fetches against /api, and EventSource streams that
// close while the tab is hidden and reconnect when it is visible again.

let onUnauthorized = () => {};

// setUnauthorizedHandler registers what happens on any 401 (show Login).
export function setUnauthorizedHandler(fn) { onUnauthorized = fn; }

// notifyUnauthorized runs the 401 handler for requests made outside api()
// (XHR uploads).
export function notifyUnauthorized() { onUnauthorized(); }

export class AuthError extends Error {}

// ApiError is a refused request: the server's message, plus the HTTP
// status, the error code ({error, code}: role, must_change, budget, …) and
// retry_after seconds (429 locked).
export class ApiError extends Error {
  constructor(message, { status = 0, code = '', retryAfter = 0, body = null } = {}) {
    super(message);
    this.status = status;
    this.code = code;
    this.retryAfter = retryAfter;
    this.body = body;
  }
}

// Code handlers (users and roles): must_change sends the page to the
// forced password change; role tells the person their role cannot do it.
let onCode = () => {};

// setCodeHandler registers fn(code, {status, message, method, path}) for
// refusals that carry an error code.
export function setCodeHandler(fn) { onCode = fn; }

// codeOf extracts the error code from an error body ("" when none).
export function codeOf(raw) {
  try {
    const j = typeof raw === 'string' ? JSON.parse(raw) : raw;
    if (j && typeof j.code === 'string') return j.code;
  } catch { /* not JSON */ }
  return '';
}

function parseBody(raw) {
  try { return raw ? JSON.parse(raw) : null; } catch { return null; }
}

function refused(r, raw, method, path) {
  const body = parseBody(raw);
  const code = codeOf(body);
  const message = messageOf(raw, r.status, r.statusText);
  const retryAfter = Number((body && body.retry_after) || r.headers.get('Retry-After')) || 0;
  if (code) {
    try { onCode(code, { status: r.status, message, method, path }); } catch (e) { console.error(e); }
  }
  return new ApiError(message, { status: r.status, code, retryAfter, body });
}

// ---------- reachability (the offline banner) ----------

let reachable = true;
const reachListeners = new Set();

// onReachability(fn) calls fn(true|false) when the host stops or starts
// answering. A network error (or a proxy's 502/504) means unreachable;
// any other reply means reachable.
export function onReachability(fn) { reachListeners.add(fn); return () => reachListeners.delete(fn); }
export const isReachable = () => reachable;

function setReachable(v) {
  if (v === reachable) return;
  reachable = v;
  for (const fn of reachListeners) {
    try { fn(v); } catch (e) { console.error(e); }
  }
}

// netFetch is fetch with reachability tracking.
async function netFetch(path, opts) {
  let r;
  try {
    r = await fetch(path, opts);
  } catch (e) {
    if (!(opts && opts.signal && opts.signal.aborted)) setReachable(false);
    throw e;
  }
  setReachable(!unreachableStatus(r.status));
  return r;
}

// unreachableStatus: the gateway in front of the host (Tailscale serve, a
// reverse proxy) answers 502/504 when Cerberus itself is down.
export const unreachableStatus = (status) => status === 502 || status === 504;

// api performs a request and returns parsed JSON (or text). body is sent
// as JSON, except a Blob, which is sent raw with its own type. A 401 calls the
// unauthorized handler and throws AuthError; other failures throw Error with
// the server's message.
export async function api(path, { method = 'GET', body, text = false, signal } = {}) {
  // X-Cerberus marks the request as coming from the UI itself: the server
  // refuses cookie-authenticated state changes without it (CSRF guard).
  const opts = { method, headers: { 'X-Cerberus': '1' }, credentials: 'same-origin', signal };
  if (body instanceof Blob) {
    // Raw uploads (recorded audio) go as-is, typed by the blob.
    opts.headers['Content-Type'] = body.type || 'application/octet-stream';
    opts.body = body;
  } else if (body !== undefined) {
    opts.headers['Content-Type'] = 'application/json';
    opts.body = JSON.stringify(body);
  }
  const r = await netFetch(path, opts);
  if (r.status === 401) {
    onUnauthorized();
    throw new AuthError('Sign in required');
  }
  if (!r.ok) throw refused(r, (await r.text().catch(() => '')).trim(), method, path);
  if (text) return r.text();
  if ((r.headers.get('Content-Type') || '').includes('json')) return r.json();
  return r.text();
}

// messageOf extracts the server's message from an error body ({"error": ...}
// or plain text).
export function messageOf(raw, status, statusText = '') {
  raw = (raw || '').trim();
  try {
    const j = JSON.parse(raw);
    const e = j.error ?? j.message;
    if (typeof e === 'string') return e;
    if (e && typeof e.message === 'string') return e.message;
  } catch { /* not JSON */ }
  return raw || `${status} ${statusText}`.trim();
}

export const post = (path, body = {}) => api(path, { method: 'POST', body });

// apiResult is api() for replies whose error bodies carry data (PATCH
// /api/config answers a refused change with its problems): it resolves to
// {ok, status, body} for any HTTP status. A 401 still calls the
// unauthorized handler and throws AuthError; network failures throw.
// opts.quiet skips the code handlers (the sign-in screens map their own
// refusals) and opts.noAuth skips the 401 → Login switch (a wrong password
// on the login card is a 401 too).
export async function apiResult(path, { method = 'GET', body, quiet = false, noAuth = false } = {}) {
  const opts = { method, headers: { 'X-Cerberus': '1' }, credentials: 'same-origin' };
  if (body !== undefined) {
    opts.headers['Content-Type'] = 'application/json';
    opts.body = JSON.stringify(body);
  }
  const r = await netFetch(path, opts);
  if (r.status === 401 && !noAuth) {
    onUnauthorized();
    throw new AuthError('Sign in required');
  }
  const raw = await r.text().catch(() => '');
  let parsed = raw;
  try { parsed = raw ? JSON.parse(raw) : null; } catch { /* plain text */ }
  const code = r.ok ? '' : codeOf(parsed);
  const message = r.ok ? '' : messageOf(raw, r.status, r.statusText);
  if (code && !quiet) {
    try { onCode(code, { status: r.status, message, method, path }); } catch (e) { console.error(e); }
  }
  return { ok: r.ok, status: r.status, body: parsed, message, code, retryAfter: Number((parsed && parsed.retry_after) || r.headers.get('Retry-After')) || 0 };
}
export const sid = (id) => `/api/sessions/${encodeURIComponent(id)}`;

// ---------- live streams ----------

const live = new Set();

// LiveSource wraps an EventSource. It is registered while started, so the
// page can close every stream when hidden. On any error it checks auth once
// (a 401 shows Login) and reconnects with backoff, 2 s doubling to 30 s.
export class LiveSource {
  // opts.keepHidden() may return true to keep the stream open while the
  // tab is hidden (only the event stream, for opted-in desktop
  // notifications; an open SSE connection is idle, not polling).
  constructor(url, handlers, opts = {}) {
    this.url = url;
    this.handlers = handlers; // { open(), <event>(data) ... }
    this.keepHidden = opts.keepHidden || (() => false);
    this.es = null;
    this.timer = 0;
    this.backoff = 2000;
    this.active = false;
  }

  start() {
    this.active = true;
    live.add(this);
    if (this.mayRun()) this.connect();
    return this;
  }

  stop() {
    this.active = false;
    live.delete(this);
    this.disconnect();
  }

  // mayRun: visible, or allowed to stay open while hidden.
  mayRun() { return !document.hidden || this.keepHidden(); }

  connect() {
    if (this.es || !this.active) return;
    const es = new EventSource(this.url);
    this.es = es;
    es.onopen = () => { this.backoff = 2000; this.handlers.open?.(); };
    // Take over reconnecting from the browser so a down server is retried
    // with backoff instead of every few seconds.
    // A server-sent "event: error" (the chat stream has one) is a
    // MessageEvent with data, not a broken connection: leave it to the
    // handler.
    es.onerror = (e) => { if (e && typeof e.data === 'string') return; this.failed(); };
    for (const [name, fn] of Object.entries(this.handlers)) {
      if (name !== 'open') es.addEventListener(name, (e) => fn(e.data));
    }
  }

  disconnect() {
    clearTimeout(this.timer);
    this.timer = 0;
    if (this.es) { this.es.close(); this.es = null; }
  }

  async failed() {
    this.disconnect();
    if (!this.active) return;
    try {
      await api('/api/status');
    } catch (e) {
      if (e instanceof AuthError) return; // Login view takes over
    }
    // Stopped, hidden or already reconnected while the check ran.
    if (!this.active || this.es || this.timer) return;
    this.timer = setTimeout(() => { this.timer = 0; if (this.mayRun()) this.connect(); }, this.backoff);
    this.backoff = Math.min(this.backoff * 2, 30000);
  }
}

// stopAllLive stops every live stream (before signing out: the server cuts
// them, and a stream's error check would then log a 401).
export function stopAllLive() {
  for (const s of [...live]) s.stop();
}

// Close all streams while hidden; reopen on return. Views refresh their data
// through the 'cerberus:visible' event.
document.addEventListener('visibilitychange', () => {
  for (const s of live) {
    if (!s.mayRun()) s.disconnect();
    else s.connect();
  }
  if (!document.hidden) window.dispatchEvent(new Event('cerberus:visible'));
});
