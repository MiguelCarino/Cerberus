// Pure helpers for the Security view and the Monitor doctor card: shape
// normalising for /api/audit, /api/devices and /api/doctor (Go field names
// or JSON tags, both accepted), the audit query and paging, doctor counts,
// the two-press revoke state, the rotate confirm word and QR rendering
// (a module matrix turned into one SVG path, or ASCII shown as text).
// No DOM here, so node --test can cover all of it.

// pick returns the first defined, non-null field of o among keys.
export function pick(o, ...keys) {
  if (!o || typeof o !== 'object') return undefined;
  for (const k of keys) if (o[k] !== undefined && o[k] !== null) return o[k];
  return undefined;
}

// listOf accepts a bare array or {items|devices|checks|records: [...]}.
export function listOf(v) {
  if (Array.isArray(v)) return v;
  const inner = pick(v, 'items', 'Items', 'devices', 'Devices', 'checks', 'Checks', 'records', 'Records');
  return Array.isArray(inner) ? inner : [];
}

const str = (v) => (v == null ? '' : String(v));
const timeMs = (v) => {
  if (!v) return 0;
  const t = typeof v === 'number' ? v : Date.parse(v);
  return Number.isFinite(t) && t > 0 && !/^0001-/.test(str(v)) ? t : 0;
};

// ---------- audit ----------

export const AUDIT_PAGE = 50;
export const AUDIT_MAX = 5000; // the server's cap (audit.MaxLimit)

// Time windows for the audit filter; ms = 0 means any time.
export const AUDIT_RANGES = [
  { key: '1h', label: '1 h', ms: 3600e3 },
  { key: '24h', label: '24 h', ms: 24 * 3600e3 },
  { key: '7d', label: '7 d', ms: 7 * 24 * 3600e3 },
  { key: '30d', label: '30 d', ms: 30 * 24 * 3600e3 },
  { key: 'all', label: 'All', ms: 0 },
];

// Actor kinds for the filter. The server matches a value without ":" as a
// kind ("agent" matches "agent:abc") and anything else exactly.
export const ACTOR_KINDS = [
  ['', 'Everyone'], ['owner', 'People'], ['agent', 'Agents'], ['cli', 'CLI'], ['system', 'System'],
];

// normAudit turns one record into {time, actor, kind, who, remote, method,
// route, targets, status, detail}.
export function normAudit(r) {
  const actor = str(pick(r, 'actor', 'Actor'));
  const [kind, ...rest] = actor.split(':');
  const targets = pick(r, 'targets', 'Targets');
  return {
    time: timeMs(pick(r, 'time', 'Time')),
    actor,
    kind: kind || 'system',
    who: rest.join(':'),
    remote: str(pick(r, 'remote', 'Remote')),
    method: str(pick(r, 'method', 'Method')),
    route: str(pick(r, 'route', 'Route')),
    targets: Array.isArray(targets) ? targets.map(str).filter(Boolean) : [],
    status: Number(pick(r, 'status', 'Status')) || 0,
    detail: str(pick(r, 'detail', 'Detail')),
    user: str(pick(r, 'user', 'User')),
  };
}

// routeLabel is the route without a repeated method ("POST /api/x" with
// method POST reads "/api/x").
export function routeLabel(rec) {
  if (rec.method && rec.route.startsWith(rec.method + ' ')) return rec.route.slice(rec.method.length + 1);
  return rec.route;
}

// statusTone colours an HTTP status: 2xx/3xx ok, 401/403/429 err (auth),
// other 4xx warn, 5xx err, 0 (an event) info.
export function statusTone(status) {
  if (!status) return 'info';
  if (status < 400) return 'ok';
  if (status === 401 || status === 403 || status === 429 || status >= 500) return 'err';
  return 'warn';
}

// actorTone colours an actor pill.
export function actorTone(kind) {
  return { owner: 'ok', agent: 'info', cli: 'muted', system: 'muted' }[kind] || 'muted';
}

// auditQuery builds the /api/audit URL. since is RFC 3339 (absent for
// "all"); actor is a kind or an exact actor; limit asks for one record more
// than the pages shown, so the view knows whether there is more.
// user filters by the acting user's name ("-" = no user: the admin token
// and the system).
export function auditQuery({ rangeKey = '24h', actor = '', user = '', pages = 1, now = Date.now() } = {}) {
  const range = AUDIT_RANGES.find((r) => r.key === rangeKey) || AUDIT_RANGES[1];
  const q = new URLSearchParams();
  if (range.ms) q.set('since', new Date(now - range.ms).toISOString());
  const a = str(actor).trim();
  if (a) q.set('actor', a);
  const u = str(user).trim();
  if (u) q.set('user', u);
  q.set('limit', String(Math.min(pages * AUDIT_PAGE + 1, AUDIT_MAX)));
  return `/api/audit?${q}`;
}

// auditPage normalises a response, sorts it newest first and cuts it to
// the pages shown: {rows, more}.
export function auditPage(raw, pages = 1) {
  const all = listOf(raw).map(normAudit).sort((a, b) => b.time - a.time);
  const n = Math.min(pages * AUDIT_PAGE, AUDIT_MAX);
  return { rows: all.slice(0, n), more: all.length > n && n < AUDIT_MAX };
}

// ---------- devices ----------

// normDevice turns one device into {id, name, created, lastSeen, lastAddr,
// current, client}. client is true for an MCP client (scope "mcp"): an
// outside agent's token that opens only the agent scope.
export function normDevice(d) {
  return {
    id: str(pick(d, 'id', 'ID', 'Id')),
    name: str(pick(d, 'name', 'Name')) || 'unnamed',
    created: timeMs(pick(d, 'created', 'Created', 'created_at', 'CreatedAt')),
    lastSeen: timeMs(pick(d, 'last_seen', 'LastSeen', 'lastSeen')),
    lastAddr: str(pick(d, 'last_addr', 'LastAddr', 'last_address', 'LastAddress', 'lastAddr')),
    current: !!pick(d, 'current', 'Current', 'this', 'self'),
    client: str(pick(d, 'scope', 'Scope')) === 'mcp',
    // Users and roles: who the device acts for ("" = the first admin, from
    // before users).
    user: str(pick(d, 'user', 'User')),
    userName: str(pick(d, 'user_name', 'UserName')),
  };
}

// normDevices sorts the current device first, then by last use. The server
// names the caller's device in a top-level "current" (an id, or "admin"
// for the admin token); a per-device flag works too.
export function normDevices(raw) {
  const cur = Array.isArray(raw) ? '' : str(pick(raw, 'current', 'Current'));
  return listOf(raw).map(normDevice).filter((d) => d.id)
    .map((d) => (cur && d.id === cur ? { ...d, current: true } : d))
    .sort((a, b) => (b.current - a.current) || ((b.lastSeen || b.created) - (a.lastSeen || a.created)));
}

// Same rule as the server's devices.CleanName (Go): letters and digits of
// any script, combining marks, space (shown as '-'), dot, dash, underscore
// and apostrophe.
export const DEVICE_NAME_RE = /^[\p{L}\p{M}\p{N} ._'-]{1,40}$/u;

// deviceNameError explains a bad name ("" when fine).
export function deviceNameError(name) {
  const n = str(name).trim();
  if (!n) return 'Give the device a name, e.g. phone';
  if (!DEVICE_NAME_RE.test(n)) return 'Letters, digits, spaces, dot, dash, underscore or apostrophe (max 40)';
  return '';
}

// normPairing turns POST /api/devices's answer ({device: {id, name}, token,
// url, qr}, or the same fields flat) into {id, name, url, qr}.
export function normPairing(p) {
  const d = pick(p, 'device', 'Device') || p;
  return {
    id: str(pick(d, 'id', 'ID')),
    name: str(pick(d, 'name', 'Name')),
    url: str(pick(p, 'url', 'URL', 'Url')),
    qr: pick(p, 'qr', 'QR', 'Qr', 'qr_ascii', 'QRASCII'),
    urls: (() => {
      const u = pick(p, 'urls', 'URLs', 'Urls');
      return Array.isArray(u) ? u.map(str).filter(Boolean) : [];
    })(),
  };
}

// otherPairURLs are the candidate links besides the main one (shown as
// "Other addresses" when the QR's address is not reachable from the device).
export function otherPairURLs(p) {
  const main = str(p && p.url);
  const seen = new Set([main]);
  return ((p && p.urls) || []).filter((u) => u && !seen.has(u) && seen.add(u));
}

// canPair reports whether the caller may pair devices: only the admin
// token can (POST /api/devices answers 403 to a paired device). GET
// /api/devices names the caller in "current" ("admin" or a device id);
// when it is missing, assume admin and let the server decide.
export function canPair(raw) {
  if (!raw || Array.isArray(raw)) return true;
  const cur = str(pick(raw, 'current', 'Current'));
  return !cur || cur === 'admin';
}

// nextPress is the two-press confirm: the first press arms (for windowMs),
// a second press inside the window fires. Returns {fire, armedAt}.
export function nextPress(armedAt, now, windowMs = 4000) {
  if (armedAt && now - armedAt <= windowMs) return { fire: true, armedAt: 0 };
  return { fire: false, armedAt: now };
}

// ---------- token rotation ----------

export const ROTATE_WORD = 'ROTATE';

// rotateReady is whether the typed confirmation unlocks the button.
export function rotateReady(typed) {
  return str(typed).trim().toUpperCase() === ROTATE_WORD;
}

// normRotate pulls the new token and login URL out of the answer, when the
// server returns them.
export function normRotate(r) {
  return {
    token: str(pick(r, 'token', 'Token')),
    url: str(pick(r, 'url', 'URL', 'login_url', 'LoginURL')),
  };
}

// ---------- doctor ----------

const STATUS_ORDER = { fail: 0, warn: 1, ok: 2 };

// normStatus maps what a check returns to ok | warn | fail.
export function normStatus(s) {
  const v = str(s).toLowerCase();
  if (v === 'ok' || v === 'pass' || v === 'passed' || v === 'good') return 'ok';
  if (v === 'fail' || v === 'failed' || v === 'error' || v === 'err' || v === 'bad') return 'fail';
  return 'warn';
}

export function normCheck(c) {
  return {
    id: str(pick(c, 'id', 'ID')),
    title: str(pick(c, 'name', 'Name', 'title', 'Title')) || str(pick(c, 'id', 'ID')),
    status: normStatus(pick(c, 'status', 'Status')),
    detail: str(pick(c, 'detail', 'Detail')),
    fix: str(pick(c, 'fix', 'Fix')),
  };
}

// normDoctor normalises and sorts failures first, then warnings, keeping
// the server's order within each group.
export function normDoctor(raw) {
  return listOf(raw).map(normCheck).map((c, i) => ({ c, i }))
    .sort((a, b) => (STATUS_ORDER[a.c.status] - STATUS_ORDER[b.c.status]) || (a.i - b.i))
    .map(({ c }) => c);
}

// doctorCounts → {ok, warn, fail, total}.
export function doctorCounts(checks) {
  const n = { ok: 0, warn: 0, fail: 0, total: 0 };
  for (const c of checks) { n[c.status] += 1; n.total += 1; }
  return n;
}

// doctorCountsOf prefers the server's counts ({counts: {ok, warn, fail}},
// all a viewer or monitor gets) and counts the checks otherwise.
export function doctorCountsOf(raw, checks) {
  const c = raw && !Array.isArray(raw) && raw.counts && typeof raw.counts === 'object' ? raw.counts : null;
  if (c && !(checks && checks.length)) {
    const n = { ok: +c.ok || 0, warn: +c.warn || 0, fail: +c.fail || 0 };
    return { ...n, total: n.ok + n.warn + n.fail };
  }
  return doctorCounts(checks || []);
}

// doctorTone is the card's overall colour.
export function doctorTone(n) {
  if (n.fail) return 'err';
  if (n.warn) return 'warn';
  return n.total ? 'ok' : 'info';
}

export const STATUS_TONE = { ok: 'ok', warn: 'warn', fail: 'err' };

// ---------- QR ----------

const ON = new Set(['1', '#', '█', 'X', 'x', '*', '■']);

// qrMatrix reads a QR given as a matrix: rows of booleans or 0/1 numbers,
// rows of strings ("1"/"#"/"█" dark), one such string with a row per line
// (qr.Code.String: "#" dark, "." light), or {size, modules|matrix|bits}
// (a serialised qr.Code). It
// returns a square boolean matrix, or null when qr is not a matrix (for
// instance ASCII art made for a terminal, shown as text instead).
export function qrMatrix(qr) {
  if (typeof qr === 'string') {
    const lines = qr.replace(/\s+$/, '').split('\n');
    return /[█▀▄]/.test(qr) ? null : qrMatrix(lines);
  }
  if (qr && !Array.isArray(qr) && typeof qr === 'object') {
    let m = pick(qr, 'modules', 'Modules', 'matrix', 'Matrix', 'bits', 'Bits');
    const size = Number(pick(qr, 'size', 'Size')) || 0;
    // A flat list of size*size cells.
    if (Array.isArray(m) && size && m.length === size * size && !Array.isArray(m[0])) {
      const rows = [];
      for (let y = 0; y < size; y++) rows.push(m.slice(y * size, (y + 1) * size));
      m = rows;
    }
    return qrMatrix(m);
  }
  if (!Array.isArray(qr) || qr.length < 21) return null;
  const rows = qr.map((row) => {
    if (Array.isArray(row)) return row.map((v) => v === true || v === 1 || v === '1');
    if (typeof row === 'string' && /^[01]+$/.test(row)) return [...row].map((ch) => ch === '1');
    if (typeof row === 'string' && /^[#.█ ■*xX]+$/.test(row) && row.length === qr.length) return [...row].map((ch) => ON.has(ch));
    return null;
  });
  if (rows.some((r) => !r || r.length !== qr.length)) return null;
  return rows;
}

// qrText is ASCII QR text to show as-is, or "" when qr is not text.
export function qrText(qr) {
  if (typeof qr === 'string') return qrMatrix(qr) ? '' : qr.replace(/\s+$/, '');
  if (Array.isArray(qr) && qr.length && qr.every((r) => typeof r === 'string') && !qrMatrix(qr)) return qr.join('\n');
  return '';
}

// qrImage is a data: image URL to show in an <img>, or "".
export function qrImage(qr) {
  return typeof qr === 'string' && /^data:image\/(png|gif|svg\+xml);/.test(qr) ? qr : '';
}

// qrPath draws a matrix as one SVG path of 1×1 squares (runs merged per
// row), offset by the quiet zone. Returns {d, size} with size the full
// viewBox side.
export function qrPath(matrix, quiet = 4) {
  const n = matrix.length;
  let d = '';
  for (let y = 0; y < n; y++) {
    let x = 0;
    while (x < n) {
      if (!matrix[y][x]) { x++; continue; }
      const start = x;
      while (x < n && matrix[y][x]) x++;
      d += `M${start + quiet} ${y + quiet}h${x - start}v1h${start - x}z`;
    }
  }
  return { d, size: n + quiet * 2 };
}
