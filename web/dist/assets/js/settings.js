// Pure helpers for the Settings view and the first-run welcome (day phase
// 4): schema normalising and grouping, value paths, the dirty diff sent as
// PATCH /api/config {set}, value parsing and formatting by field kind
// (durations with a friendly display), search, problem mapping, list and
// map editing, the welcome's changes and the restart reconnect check.
// No DOM here, so node --test covers all of it.

// Groups in display order (internal/config.Groups); unknown groups follow.
export const GROUP_ORDER = ['Server', 'Models', 'Gateway & spend', 'Sessions', 'Agents', 'Safety',
  'Monitor & energy', 'Voice', 'Notifications', 'Files', 'Advanced'];

// The Security subsection is not a schema group: it is the phase 3
// Security view (devices, token, doctor, audit) shown inside Settings.
export const SECURITY = 'Security';

// slug turns a group name into a route part: "Gateway & spend" → "gateway-spend".
export function slug(group) {
  return String(group || '').toLowerCase().replace(/[^a-z0-9]+/g, '-').replace(/^-|-$/g, '');
}

const pickKey = (o, ...keys) => {
  if (!o || typeof o !== 'object') return undefined;
  for (const k of keys) if (o[k] !== undefined) return o[k];
  return undefined;
};

const numOrNull = (v) => (typeof v === 'number' && Number.isFinite(v) ? v : null);

// normField accepts the JSON tags (path, label…) or Go names (Path, Label…).
export function normField(f) {
  const kind = String(pickKey(f, 'kind', 'Kind') || 'string');
  const enumv = pickKey(f, 'enum', 'Enum');
  const items = pickKey(f, 'items', 'Items');
  return {
    path: String(pickKey(f, 'path', 'Path') || ''),
    label: String(pickKey(f, 'label', 'Label') || ''),
    help: String(pickKey(f, 'help', 'Help') || ''),
    group: String(pickKey(f, 'group', 'Group') || ''),
    level: pickKey(f, 'level', 'Level') === 'basic' ? 'basic' : 'advanced',
    kind,
    format: String(pickKey(f, 'format', 'Format') || ''),
    enum: Array.isArray(enumv) ? enumv.map(String) : [],
    min: numOrNull(pickKey(f, 'min', 'Min')),
    max: numOrNull(pickKey(f, 'max', 'Max')),
    unit: String(pickKey(f, 'unit', 'Unit') || ''),
    restart: !!pickKey(f, 'restart', 'Restart'),
    sensitive: !!pickKey(f, 'sensitive', 'Sensitive'),
    readOnly: !!pickKey(f, 'read_only', 'readOnly', 'ReadOnly') || kind === 'time',
    required: !!pickKey(f, 'required', 'Required'),
    default: pickKey(f, 'default', 'Default'),
    items: Array.isArray(items) ? items.map(normField) : [],
  };
}

// normSchema accepts a bare list or {fields: [...]}.
export function normSchema(raw) {
  const list = Array.isArray(raw) ? raw : (raw && (raw.fields || raw.Fields || raw.schema)) || [];
  return Array.isArray(list) ? list.map(normField).filter((f) => f.path) : [];
}

// visible: what the Settings view shows (never the sensitive token; the
// setup flag is the welcome's business).
export const visible = (f) => !f.sensitive && f.path !== 'server.init_done';

// groupSchema returns [{group, slug, basic, advanced}] in display order,
// skipping hidden fields and empty groups. A field without a group goes to
// Advanced.
export function groupSchema(fields) {
  const by = new Map();
  for (const f of fields) {
    if (!visible(f)) continue;
    const g = f.group || 'Advanced';
    if (!by.has(g)) by.set(g, { group: g, slug: slug(g), basic: [], advanced: [] });
    by.get(g)[f.level === 'basic' ? 'basic' : 'advanced'].push(f);
  }
  const rank = (g) => { const i = GROUP_ORDER.indexOf(g); return i < 0 ? GROUP_ORDER.length : i; };
  return [...by.values()].sort((a, b) => rank(a.group) - rank(b.group) || a.group.localeCompare(b.group));
}

// ---------- paths ----------

// getAt reads a dotted path ("engines.idle_ttl_sec") from a plain object.
export function getAt(obj, path) {
  let v = obj;
  for (const k of String(path).split('.')) {
    if (v == null || typeof v !== 'object') return undefined;
    v = v[k];
  }
  return v;
}

// setAt writes a dotted path into obj (mutating it), creating objects.
export function setAt(obj, path, value) {
  const keys = String(path).split('.');
  let o = obj;
  for (let i = 0; i < keys.length - 1; i++) {
    if (o[keys[i]] == null || typeof o[keys[i]] !== 'object') o[keys[i]] = {};
    o = o[keys[i]];
  }
  o[keys[keys.length - 1]] = value;
  return obj;
}

export const clone = (v) => (v === undefined ? undefined : JSON.parse(JSON.stringify(v)));

// itemRel returns an item field's path inside one item: "agents[].command"
// under "agents" → "command"; "prices.*.in" under "prices" → "in"; a scalar
// item ("repo_roots[]") → "".
export function itemRel(parent, child) {
  for (const sep of ['[]', '.*']) {
    const pre = parent.path + sep;
    if (child.path === pre) return '';
    if (child.path.startsWith(pre + '.')) return child.path.slice(pre.length + 1);
  }
  return child.path;
}

// scalarItem is the single item field of a list or map of scalars, or null
// when the items are structs.
export function scalarItem(f) {
  return f.items.length === 1 && itemRel(f, f.items[0]) === '' ? f.items[0] : null;
}

// mapKeyPath is the concrete path of a map value, quoting keys with dots.
export function mapKeyPath(base, key) {
  return /^[A-Za-z0-9_-]+$/.test(key) ? `${base}.${key}` : `${base}[${JSON.stringify(key)}]`;
}

// ---------- equality and the dirty diff ----------

const isEmpty = (v) => v == null || (Array.isArray(v) && v.length === 0)
  || (typeof v === 'object' && !Array.isArray(v) && Object.keys(v).length === 0);

// same compares JSON values; null, missing, [] and {} are all "empty"
// (Go writes a nil list as null and an empty one as []).
export function same(a, b) {
  if (isEmpty(a) && isEmpty(b)) return true;
  if (typeof a !== typeof b || Array.isArray(a) !== Array.isArray(b) || a === null || b === null) return a === b;
  if (typeof a !== 'object') return a === b;
  if (Array.isArray(a)) return a.length === b.length && a.every((x, i) => same(x, b[i]));
  const keys = new Set([...Object.keys(a), ...Object.keys(b)]);
  for (const k of keys) if (!same(a[k], b[k])) return false;
  return true;
}

// dirtyDiff returns the {path: value} changes between the loaded config and
// the draft, one entry per top-level setting (a whole list or map counts as
// one setting). Hidden and read-only settings are never sent.
export function dirtyDiff(orig, draft, fields) {
  const out = {};
  for (const f of fields) {
    if (!visible(f) || f.readOnly) continue;
    const a = getAt(orig, f.path);
    const b = getAt(draft, f.path);
    if (!same(a, b)) out[f.path] = b === undefined ? null : clone(b);
  }
  return out;
}

export const changeCount = (set) => Object.keys(set || {}).length;

// needsRestart: does any changed path need a daemon restart?
export function needsRestart(set, fields) {
  const byPath = new Map(fields.map((f) => [f.path, f]));
  return Object.keys(set || {}).some((p) => byPath.get(p)?.restart);
}

// ---------- durations ----------

const UNIT_SEC = { s: 1, sec: 1, secs: 1, second: 1, seconds: 1, m: 60, min: 60, mins: 60, minute: 60, minutes: 60,
  h: 3600, hr: 3600, hrs: 3600, hour: 3600, hours: 3600, d: 86400, day: 86400, days: 86400 };

// parseDuration reads "90", "10m", "1h30m", "1h 30 min", "1.5h" or "2 days"
// into whole seconds; null when it is not a duration.
export function parseDuration(text) {
  const s = String(text ?? '').trim().toLowerCase();
  if (s === '') return null;
  if (/^\d+(\.\d+)?$/.test(s)) return Math.round(Number(s));
  const re = /(\d+(?:\.\d+)?)\s*([a-z]+)\s*/g;
  let total = 0;
  let used = 0;
  let m;
  while ((m = re.exec(s)) !== null) {
    if (m.index !== used) return null;
    const unit = UNIT_SEC[m[2]];
    if (!unit) return null;
    total += Number(m[1]) * unit;
    used = re.lastIndex;
  }
  return used === s.length && used > 0 ? Math.round(total) : null;
}

// formatDuration writes seconds compactly for the input: 600 → "10m",
// 5400 → "1h30m", 45 → "45s", 0 → "0".
export function formatDuration(sec) {
  let n = Math.max(0, Math.round(Number(sec) || 0));
  if (n === 0) return '0';
  const parts = [];
  for (const [u, k] of [['d', 86400], ['h', 3600], ['m', 60], ['s', 1]]) {
    if (n >= k) { parts.push(Math.floor(n / k) + u); n %= k; }
  }
  return parts.join('');
}

// humanDuration is the friendly display: 5400 → "1 hour 30 minutes".
export function humanDuration(sec, zero = 'off') {
  let n = Math.max(0, Math.round(Number(sec) || 0));
  if (n === 0) return zero;
  const parts = [];
  for (const [u, k] of [['day', 86400], ['hour', 3600], ['minute', 60], ['second', 1]]) {
    if (n >= k) { const q = Math.floor(n / k); parts.push(`${q} ${u}${q === 1 ? '' : 's'}`); n %= k; }
  }
  return parts.slice(0, 2).join(' ');
}

// ---------- parse and format by kind ----------

const fmtNumber = (n) => (Number.isFinite(n) ? String(n) : '');

// formatValue writes a value into an input's text.
export function formatValue(field, value) {
  if (value == null) return '';
  switch (field.kind) {
    case 'duration-seconds': return formatDuration(value);
    case 'int': case 'float': return fmtNumber(Number(value));
    case 'bool': return value ? 'on' : 'off';
    case 'time': return value && !String(value).startsWith('0001-') ? String(value) : '';
    case 'list': case 'map': return JSON.stringify(value);
    default: return String(value);
  }
}

function rangeError(field, n) {
  const u = field.unit && field.kind !== 'duration-seconds' ? ` ${field.unit}` : '';
  if (field.min != null && n < field.min) {
    return field.kind === 'duration-seconds' ? `At least ${humanDuration(field.min, '0')}` : `At least ${field.min}${u}`;
  }
  if (field.max != null && n > field.max) {
    return field.kind === 'duration-seconds' ? `At most ${humanDuration(field.max, '0')}` : `At most ${field.max}${u}`;
  }
  return '';
}

// parseValue reads an input's text (or a checkbox's boolean) for a field.
// Returns {value} or {error}; empty numbers mean 0.
export function parseValue(field, input) {
  const text = typeof input === 'string' ? input.trim() : input;
  switch (field.kind) {
    case 'bool': {
      if (typeof text === 'boolean') return { value: text };
      if (/^(on|true|yes|1)$/i.test(text)) return { value: true };
      if (/^(off|false|no|0)$/i.test(text)) return { value: false };
      return { error: 'Use on or off' };
    }
    case 'int': {
      if (text === '') return field.required ? { error: 'Required' } : rangeChecked(field, 0);
      if (!/^-?\d+$/.test(text)) return { error: 'A whole number' };
      return rangeChecked(field, Number(text));
    }
    case 'float': {
      if (text === '') return field.required ? { error: 'Required' } : rangeChecked(field, 0);
      const n = Number(String(text).replace(',', '.'));
      if (!Number.isFinite(n) || !/^-?(\d+([.,]\d*)?|[.,]\d+)$/.test(text)) return { error: 'A number' };
      return rangeChecked(field, n);
    }
    case 'duration-seconds': {
      if (text === '') return rangeChecked(field, 0);
      const n = parseDuration(text);
      if (n == null) return { error: 'A duration such as 90, 10m or 1h30m' };
      return rangeChecked(field, n);
    }
    case 'enum': {
      if (field.enum.length && !field.enum.includes(text)) return { error: `One of ${field.enum.filter(Boolean).join(', ')}` };
      return { value: text };
    }
    default: {
      const s = typeof text === 'string' ? text : String(text ?? '');
      if (field.required && s === '') return { error: 'Required' };
      return { value: s };
    }
  }
}

function rangeChecked(field, n) {
  const e = rangeError(field, n);
  return e ? { error: e } : { value: n };
}

// unitLabel is what follows a number input ("s" is shown as the friendly
// duration instead).
export const unitLabel = (field) => (field.kind === 'duration-seconds' ? '' : field.unit);

// ---------- search ----------

const words = (q) => String(q || '').toLowerCase().split(/\s+/).filter(Boolean);

function haystack(f) {
  const parts = [f.label, f.help, f.path, f.group];
  for (const it of f.items) parts.push(it.label, it.help);
  return parts.join(' ').toLowerCase();
}

// filterFields keeps the visible fields matching every word of q in their
// label, help, path, group or item labels. An empty query matches nothing
// (the view then shows groups).
export function filterFields(fields, q) {
  const w = words(q);
  if (!w.length) return [];
  return fields.filter((f) => visible(f) && w.every((x) => haystack(f).includes(x)));
}

// ---------- problems ----------

export function normProblems(list) {
  return (Array.isArray(list) ? list : []).map((p) => ({
    path: String(pickKey(p, 'path', 'Path') || ''),
    message: String(pickKey(p, 'message', 'Message') || ''),
    level: pickKey(p, 'level', 'Level') === 'warn' ? 'warn' : 'error',
  })).filter((p) => p.message);
}

// problemsAt: problems for exactly this concrete path.
export const problemsAt = (problems, path) => problems.filter((p) => p.path === path);

// problemsUnder: problems for this path or anything inside it.
export const problemsUnder = (problems, path) => problems.filter((p) =>
  p.path === path || p.path.startsWith(path + '.') || p.path.startsWith(path + '['));

// fieldForProblem finds the top-level setting a concrete problem path
// belongs to ("agents[2].command" → the "agents" field).
export function fieldForProblem(fields, path) {
  let best = null;
  for (const f of fields) {
    if (path === f.path || path.startsWith(f.path + '.') || path.startsWith(f.path + '[')) {
      if (!best || f.path.length > best.path.length) best = f;
    }
  }
  return best;
}

// problemCounts: {group: {error, warn}} for the group navigation.
export function problemCounts(fields, problems) {
  const out = {};
  for (const p of problems) {
    const f = fieldForProblem(fields, p.path);
    const g = f ? (f.group || 'Advanced') : 'Advanced';
    out[g] ||= { error: 0, warn: 0 };
    out[g][p.level === 'warn' ? 'warn' : 'error']++;
  }
  return out;
}

// ---------- list and map editing ----------

// Starting values for new items of the lists that have list editors.
const NEW_ITEM = {
  providers: { enabled: true },
  budgets: { provider: '*', period: 'month' },
  'tariff.tiers': { name: '' },
  agents: { needs_model: true, interactive: true },
};

function zeroOf(f) {
  switch (f.kind) {
    case 'int': case 'float': case 'duration-seconds': return 0;
    case 'bool': return false;
    case 'list': return [];
    case 'map': return {};
    case 'enum': return f.enum[0] ?? '';
    default: return '';
  }
}

// newItem builds an empty item for a list (or a map value).
export function newItem(field) {
  const sc = scalarItem(field);
  if (sc) return zeroOf(sc);
  const item = {};
  for (const it of field.items) setAt(item, itemRel(field, it), zeroOf(it));
  return Object.assign(item, clone(NEW_ITEM[field.path] || {}));
}

// moveItem returns a copy of list with item i moved by dir (-1 up, +1 down).
export function moveItem(list, i, dir) {
  const out = [...(list || [])];
  const j = i + dir;
  if (i < 0 || i >= out.length || j < 0 || j >= out.length) return out;
  [out[i], out[j]] = [out[j], out[i]];
  return out;
}

export const removeItem = (list, i) => (list || []).filter((_, k) => k !== i);

// itemTitle: a short name for a struct item in a list editor.
export function itemTitle(item, i) {
  if (item && typeof item === 'object') {
    for (const k of ['name', 'id', 'provider']) {
      if (item[k]) return item.period && k === 'provider' ? `${item[k]} / ${item.period}` : String(item[k]);
    }
    if (item.up_to_kwh != null) return item.up_to_kwh ? `up to ${item.up_to_kwh} kWh` : 'the rest';
  }
  return `Item ${i + 1}`;
}

// mapEntries turns a map into editable [key, value] rows, keys sorted.
export function mapEntries(obj) {
  return Object.entries(obj || {}).sort((a, b) => a[0].localeCompare(b[0]));
}

// entriesToMap turns rows back into a map. Empty keys are dropped; a
// repeated key is an error.
export function entriesToMap(entries) {
  const map = {};
  for (const [k, v] of entries) {
    const key = String(k).trim();
    if (!key) continue;
    if (Object.hasOwn(map, key)) return { map, error: `“${key}” appears twice` };
    map[key] = v;
  }
  return { map };
}

// secretChoices lists vault names for a secret-ref picker, keeping a current
// value that is not (or no longer) in the vault, marked missing.
export function secretChoices(names, current) {
  const list = [...new Set((names || []).map(String))].sort((a, b) => a.localeCompare(b)).map((n) => ({ name: n, missing: false }));
  if (current && !list.some((c) => c.name === current)) list.unshift({ name: current, missing: true });
  return list;
}

// ---------- config response ----------

// normConfigResponse reads GET /api/config (or a PATCH reply).
export function normConfigResponse(raw) {
  const r = raw && typeof raw === 'object' ? raw : {};
  return {
    config: r.config && typeof r.config === 'object' ? r.config : (r.Config || null),
    problems: normProblems(r.problems || r.Problems),
    restartPending: !!(r.restart_pending ?? r.RestartPending),
  };
}

// initDone reads server.init_done; a config without the field counts as done
// (older daemons have no welcome).
export function initDone(config) {
  const v = getAt(config, 'server.init_done');
  return v === undefined ? true : !!v;
}

// ---------- first-run welcome ----------

export const PHONE_MODES = [
  ['none', 'This computer only', 'Only browsers on this machine can open Cerberus.'],
  ['lan', 'Home network', 'Phones on the same Wi-Fi reach it at this computer’s LAN address.'],
  ['tailscale', 'Tailscale', 'Phones on your tailnet reach it anywhere, at this computer’s Tailscale address.'],
];

// portOf returns the port of a host:port listen address.
export function portOf(listen) {
  const m = /:(\d+)$/.exec(String(listen || ''));
  return m ? m[1] : '7340';
}

// welcomeAddress turns what the owner typed (an IP, a host or host:port)
// into a listen address on the main port. Returns '' when empty.
export function welcomeAddress(text, listen) {
  const s = String(text || '').trim().replace(/^https?:\/\//, '').replace(/\/.*$/, '');
  if (!s) return '';
  if (/^\[.*\](:\d+)?$/.test(s)) return /\]:\d+$/.test(s) ? s : `${s}:${portOf(listen)}`;
  if (s.includes(':') && (s.match(/:/g) || []).length > 1) return `[${s}]:${portOf(listen)}`; // bare IPv6
  return /:\d+$/.test(s) ? s : `${s}:${portOf(listen)}`;
}

// welcomePhoneError checks the phone step: home network and Tailscale need
// this computer's address (the page cannot guess it when it is open on
// localhost). Returns '' when the answers can be saved, else what to fix.
export function welcomePhoneError(answers, listen) {
  const a = answers || {};
  if (a.phone !== 'lan' && a.phone !== 'tailscale') return '';
  const where = a.phone === 'lan' ? 'its LAN IP (ip addr)' : 'its Tailscale IP (tailscale ip -4)';
  if (!String(a.address || '').trim()) return 'Enter this computer’s address, ' + where + ', or choose “This computer only”.';
  const addr = welcomeAddress(a.address, listen);
  const m = /^(\[[^\]]+\]|[^\s:/\[\]]+):(\d+)$/.exec(addr);
  const port = m ? Number(m[2]) : 0;
  if (!m || port < 1 || port > 65535) return '“' + String(a.address).trim() + '” is not an address such as 192.168.1.20 or 100.101.102.103.';
  return '';
}

// welcomeChanges turns the four welcome answers into PATCH changes, only
// for what differs from the current config.
//   answers: {model, phone: 'none'|'lan'|'tailscale', address, ntfy, kwh}
export function welcomeChanges(answers, config) {
  const a = answers || {};
  const set = {};
  const put = (path, v) => { if (!same(getAt(config, path), v)) set[path] = v; };
  if (a.model) put('engines.default_model', a.model);
  if (a.phone === 'none') {
    put('server.extra_listen', []);
  } else if (a.phone === 'lan' || a.phone === 'tailscale') {
    const addr = welcomeAddress(a.address, getAt(config, 'server.listen'));
    if (addr) {
      const cur = getAt(config, 'server.extra_listen') || [];
      if (!cur.includes(addr)) put('server.extra_listen', [...cur, addr]);
    }
  }
  const ntfy = String(a.ntfy || '').trim();
  if (ntfy) put('notify.ntfy_url', /^https?:\/\//.test(ntfy) ? ntfy : `https://ntfy.sh/${ntfy}`);
  const kwh = Number(String(a.kwh ?? '').replace(',', '.'));
  if (String(a.kwh ?? '').trim() !== '' && Number.isFinite(kwh) && kwh >= 0) put('tariff.household_kwh_month', kwh);
  return set;
}

// ntfyTopic makes a hard-to-guess topic URL from random bytes.
export function ntfyTopic(bytes) {
  const hex = [...bytes].map((b) => b.toString(16).padStart(2, '0')).join('');
  return `https://ntfy.sh/cerberus-${hex}`;
}

// ---------- restart ----------

// restarted reports whether the daemon answering now is a new process:
// its uptime is shorter than the old one's would be by now.
export function restarted(uptimeBefore, elapsedSec, uptimeNow) {
  if (typeof uptimeNow !== 'number' || typeof uptimeBefore !== 'number') return false;
  return uptimeNow + 2 < uptimeBefore + elapsedSec;
}

// reconnectDelay: wait before reconnect attempt n (0-based), 1 s growing to 5 s.
export const reconnectDelay = (n) => Math.min(5000, Math.round(1000 * 1.5 ** n));

// RESTART_GIVE_UP: stop trying after this long and reload anyway.
export const RESTART_GIVE_UP_MS = 90_000;
