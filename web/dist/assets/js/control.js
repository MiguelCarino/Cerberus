// Pure helpers for the control center (wall, bulk bar, tags, tree, feed,
// keyboard shortcuts): no DOM, no network, so node --test can import them
// (see web/test/control.test.mjs).

import { isLive } from './util.js';

// ---------- names, tags, priority, limits ----------

// sessionName is what a session is called in lists: the user's Name, then
// the title, the task, the id.
export const sessionName = (s) => (s && (s.Name || s.Title || s.Task || s.ID)) || '';

export const MAX_TAGS = 8;
const TAG_RE = /^[a-z0-9-]{1,24}$/;

// normTag lowercases and trims one tag; "" when it is not valid
// ([a-z0-9-], 1 to 24 characters, the server's rule).
export function normTag(t) {
  const v = String(t ?? '').trim().toLowerCase();
  return TAG_RE.test(v) ? v : '';
}

// parseTags splits "a, b c" into valid unique tags (at most MAX_TAGS) and
// lists the rejected words, so the UI can say what was wrong.
export function parseTags(text, existing = []) {
  const tags = [...(existing || [])];
  const errors = [];
  for (const word of String(text ?? '').split(/[\s,]+/).filter(Boolean)) {
    const t = normTag(word);
    if (!t) { errors.push(`"${word}" is not a valid tag (a-z, 0-9 and -, up to 24)`); continue; }
    if (tags.includes(t)) continue;
    if (tags.length >= MAX_TAGS) { errors.push(`at most ${MAX_TAGS} tags`); break; }
    tags.push(t);
  }
  return { tags, errors };
}

// allTags is every tag in the list, sorted, for the filter chips.
export function allTags(list) {
  const set = new Set();
  for (const s of list || []) for (const t of s.Tags || []) set.add(t);
  return [...set].sort();
}

export const hasTag = (s, tag) => !tag || (s.Tags || []).includes(tag);

export const PRIORITY_MIN = -10;
export const PRIORITY_MAX = 10;
export function clampPriority(n) {
  const v = Math.round(Number(n) || 0);
  return Math.max(PRIORITY_MIN, Math.min(PRIORITY_MAX, v));
}

const MEM_RE = /^(\d+(\.\d+)?[KMGT]?|infinity)$/i;

// validateLimits checks the limits form ({MaxMinutes, MemoryMax,
// MaxOutputMiB} as strings or numbers) and returns { limits, error }.
// Empty fields mean "default" (0 or "").
export function validateLimits(form) {
  const num = (v, name) => {
    const s = String(v ?? '').trim();
    if (!s) return 0;
    if (!/^\d+$/.test(s)) throw new Error(`${name} must be a whole number`);
    return parseInt(s, 10);
  };
  try {
    const limits = {
      MaxMinutes: num(form.MaxMinutes, 'Time limit'),
      MemoryMax: String(form.MemoryMax ?? '').trim().toUpperCase().replace('INFINITY', 'infinity'),
      MaxOutputMiB: num(form.MaxOutputMiB, 'Output limit'),
    };
    if (limits.MemoryMax && !MEM_RE.test(limits.MemoryMax)) throw new Error('Memory must look like 512M, 4G or 1.5G');
    if (limits.MaxMinutes > 7 * 24 * 60) throw new Error('Time limit is at most a week (10080 minutes)');
    return { limits, error: '' };
  } catch (e) {
    return { limits: null, error: e.message };
  }
}

// limitsText is a short summary ("30 min · 4G memory · 50 MiB output"),
// "" for defaults. guard is /api/status guard: when memory caps are not
// enforced (no systemd scope) the memory part says so.
export function limitsText(l, guard) {
  if (!l) return '';
  const memOff = !!guard && guard.memory_enforced === false;
  return [l.MaxMinutes ? `${l.MaxMinutes} min` : '',
    l.MemoryMax ? `${l.MemoryMax} memory${memOff ? ' (not enforced)' : ''}` : '',
    l.MaxOutputMiB ? `${l.MaxOutputMiB} MiB output` : ''].filter(Boolean).join(' · ');
}

// limitsHelp is the per-field wording of the limits dialog: time and output
// have no configured default (empty = no limit); memory falls back to the
// guard's session_memory_max. memoryNote is "" unless caps are not enforced.
export function limitsHelp(guard) {
  const dflt = (guard && guard.session_memory_max) || '';
  const memOff = !!guard && guard.memory_enforced === false;
  return {
    minutes: 'no limit',
    output: 'no limit',
    memory: dflt ? `default ${dflt}` : 'no cap',
    memoryNote: memOff ? 'Not enforced right now: runs get no systemd scope (no user bus), so nothing caps their memory.' : '',
  };
}

// rowKey is what a sessions-list row is built from, apart from its live
// activity line and time: a session event that leaves it unchanged only
// patches those in place (rebuilding rows about once a second per busy
// agent would swallow clicks on them).
export function rowKey(s) {
  if (!s) return '';
  return JSON.stringify([s.State, !!s.Pinned, s.Priority || 0, s.Tags || [], sessionName(s), s.Name || '',
    s.Title || '', s.Parent || '', s.Depth || 0, s.WaitingReason || '', s.Agent || '', s.Model || '',
    s.Attempt || 0, s.Workspace || '', s.Dir || '', s.Repo || '', s.Group || '', s.WaitingAsk || '', s.AskSeq || 0,
    s.Preview ? [!!s.Preview.Static, s.Preview.Live || [], s.Preview.Managed || ''] : null]);
}

// needsApproval: a spawned child held for a person's yes or no.
export const needsApproval = (s) => !!s && s.State === 'queued' && /needs approval/i.test(s.WaitingReason || '');

// ---------- list order and parent/child tree ----------

// byListOrder: pinned first, then higher priority, then most recently updated.
export function byListOrder(a, b) {
  return (!!b.Pinned - !!a.Pinned) || ((b.Priority || 0) - (a.Priority || 0)) ||
    ((Date.parse(b.Updated) || 0) - (Date.parse(a.Updated) || 0));
}

// buildTree orders a flat list so every child follows its parent, and gives
// each row its indent level: [{ s, level, orphan }]. A child whose parent is
// not in the list is a root with orphan = true (the UI says where it came
// from). Siblings keep the order of cmp. Cycles cannot loop.
export function buildTree(list, cmp = byListOrder) {
  const items = [...(list || [])].sort(cmp);
  const ids = new Set(items.map((s) => s.ID));
  const kids = new Map();
  const roots = [];
  for (const s of items) {
    if (s.Parent && s.Parent !== s.ID && ids.has(s.Parent)) {
      if (!kids.has(s.Parent)) kids.set(s.Parent, []);
      kids.get(s.Parent).push(s);
    } else roots.push(s);
  }
  const out = [];
  const seen = new Set();
  const walk = (s, level, orphan) => {
    if (seen.has(s.ID)) return;
    seen.add(s.ID);
    out.push({ s, level, orphan });
    for (const c of kids.get(s.ID) || []) walk(c, level + 1, false);
  };
  for (const r of roots) walk(r, 0, !!r.Parent);
  // Anything left sits in a cycle: show it flat rather than hide it.
  for (const s of items) if (!seen.has(s.ID)) walk(s, 0, true);
  return out;
}

// ---------- wall ----------

const SLOTS = [1, 2, 4, 6, 9];
export const WALL_MAX = 9;

// gridFit picks the wall layout for n tiles: the smallest of 1/2/4/6/9 slots
// that holds them, as { cols, rows, slots }. maxCols narrows it on small
// screens; aspect (width / height) < 1 stacks 2 as 1×2 and 6 as 2×3.
export function gridFit(n, { maxCols = 3, aspect = 16 / 9 } = {}) {
  const count = Math.max(0, Math.min(WALL_MAX, Math.floor(n) || 0));
  if (!count) return { cols: 0, rows: 0, slots: 0 };
  const slots = SLOTS.find((s) => s >= count);
  let cols = { 1: 1, 2: 2, 4: 2, 6: 3, 9: 3 }[slots];
  if (aspect < 1 && (slots === 2 || slots === 6)) cols = slots === 2 ? 1 : 2;
  cols = Math.max(1, Math.min(cols, maxCols));
  return { cols, rows: Math.ceil(slots / cols), slots };
}

// wallStates: sessions worth watching when nothing is chosen.
const WATCH = new Set(['starting', 'running', 'waiting', 'checking', 'retrying', 'escalated']);
export const isWatchable = (s) => WATCH.has(s.State) || isLive(s.State);

// wallIds decides which sessions the wall shows. Ids in the route (a chosen
// set) show exactly those. Otherwise the tiles kept on the wall come first,
// even when finished, then every other active session (waiting first, then
// list order). Unknown ids are dropped; at most max.
export function wallIds(sessions, { routeIds = [], kept = [], max = WALL_MAX } = {}) {
  const byId = new Map((sessions || []).map((s) => [s.ID, s]));
  const pick = (ids) => (ids || []).filter((id, i, a) => byId.has(id) && a.indexOf(id) === i);
  if (routeIds && routeIds.length) return pick(routeIds).slice(0, max);
  const ids = pick(kept);
  const auto = (sessions || []).filter((s) => isWatchable(s) && !ids.includes(s.ID))
    .sort((a, b) => ((b.State === 'waiting') - (a.State === 'waiting')) || byListOrder(a, b))
    .map((s) => s.ID);
  return [...ids, ...auto].slice(0, max);
}

// wallStreamURL is the one multiplexed stream for a set of ids (≤ 16).
export function wallStreamURL(ids) {
  return `/api/wall/stream?ids=${(ids || []).slice(0, 16).map(encodeURIComponent).join(',')}`;
}

// parseWallEvent reads an `out` event's data ({id, b64}); null when broken.
export function parseWallEvent(data) {
  try {
    const v = JSON.parse(data);
    if (!v || typeof v.id !== 'string' || typeof v.b64 !== 'string') return null;
    return v;
  } catch { return null; }
}

// miniTermSize fits a session's PTY width into a tile: the font size that
// shows all ptyCols columns across boxW pixels (clamped), and how many rows
// of that size fit in boxH. Mono cells are about 0.6 em wide, 1.2 em tall.
export function miniTermSize(boxW, boxH, ptyCols, { min = 5, max = 13 } = {}) {
  const cols = Math.max(20, Math.floor(ptyCols) || 100);
  const fs = Math.max(min, Math.min(max, Math.floor((Math.max(0, boxW - 8) / (cols * 0.6)) * 2) / 2));
  const rows = Math.max(4, Math.floor(Math.max(0, boxH - 8) / (fs * 1.2)));
  return { fontSize: fs, cols, rows };
}

// snapIndex is the tile a horizontal scroll-snap strip rests on.
export function snapIndex(scrollLeft, width, n) {
  if (!width || n <= 0) return 0;
  return Math.max(0, Math.min(n - 1, Math.round(scrollLeft / width)));
}

// fmtElapsed is "42s", "12m", "3h 05m", "2d 4h" since an RFC 3339 time.
export function fmtElapsed(iso, now = Date.now()) {
  const t = Date.parse(iso);
  if (!t || t < 0 || String(iso).startsWith('0001-')) return '–';
  const s = Math.max(0, Math.floor((now - t) / 1000));
  if (s < 60) return `${s}s`;
  if (s < 3600) return `${Math.floor(s / 60)}m`;
  if (s < 86400) return `${Math.floor(s / 3600)}h ${String(Math.floor((s % 3600) / 60)).padStart(2, '0')}m`;
  return `${Math.floor(s / 86400)}d ${Math.floor((s % 86400) / 3600)}h`;
}

// ---------- bulk ----------

export const BULK_MAX = 100;

// Destructive bulk kinds ask first.
export const BULK_CONFIRM = new Set(['stop', 'discard', 'accept']);

// bulkBody builds the POST /api/sessions/bulk body.
export function bulkBody(ids, op) {
  return {
    ids: [...new Set(ids || [])].slice(0, BULK_MAX),
    op: { Kind: op.Kind, Data: op.Data || '', Push: !!op.Push, Message: op.Message || '', Priority: clampPriority(op.Priority), Tag: op.Tag || '', Force: !!op.Force },
  };
}

// bulkResults accepts the server's reply as [BulkResult] or {results}.
export function bulkResults(out) {
  if (Array.isArray(out)) return out;
  if (out && Array.isArray(out.results)) return out.results;
  if (out && Array.isArray(out.Results)) return out.Results;
  return [];
}

// summarizeBulk turns [BulkResult] into counts, one toast line and a tone.
// The first error's detail is quoted, named by the session when known.
export function summarizeBulk(results, nameOf = (id) => id) {
  const c = { ok: 0, skipped: 0, error: 0 };
  let firstErr = null;
  let firstSkip = null;
  for (const r of results || []) {
    const k = r.Result === 'ok' || r.Result === 'skipped' ? r.Result : 'error';
    c[k]++;
    if (k === 'error' && !firstErr) firstErr = r;
    if (k === 'skipped' && !firstSkip) firstSkip = r;
  }
  const total = c.ok + c.skipped + c.error;
  const parts = [`${c.ok} of ${total} done`];
  if (c.skipped) parts.push(`${c.skipped} skipped`);
  if (c.error) parts.push(`${c.error} failed`);
  let text = total ? parts.join(' · ') : 'Nothing to do';
  const why = firstErr || firstSkip;
  if (why && why.Detail) text += ` (${nameOf(why.ID)}: ${why.Detail})`;
  const tone = !total ? 'warn' : c.error ? (c.ok ? 'warn' : 'err') : c.skipped ? (c.ok ? 'ok' : 'warn') : 'ok';
  return { ...c, total, text, tone };
}

// broadcastKey is what a quick key sends to every selected session. The
// built-in agent reads a whole line for approvals, so y/n/a also press Enter
// when every selected session waits on an approval.
export function broadcastKey(label, sessions) {
  const keys = { Enter: '\r', y: 'y', n: 'n', a: 'a', 'Ctrl-C': '\x03', Esc: '\x1b' };
  const d = keys[label];
  if (d == null) return '';
  const list = sessions || [];
  const approval = list.length > 0 && list.every((s) => s.State === 'waiting' && s.WaitingReason === 'approval');
  return approval && 'yna'.includes(label) ? d + '\r' : d;
}

// ---------- feed ----------

export const FEED_KINDS = ['created', 'state', 'waiting', 'passed', 'failed', 'accepted', 'followup', 'input', 'spawned', 'limit', 'update', 'warning'];
export const FEED_MAX = 300;

export function feedTone(kind) {
  switch (kind) {
    case 'waiting': case 'limit': return 'warn';
    case 'passed': case 'accepted': return 'ok';
    case 'failed': case 'warning': return 'err'; // warning: e.g. the sandbox fell back to none
    case 'spawned': case 'created': return 'info';
    default: return 'muted';
  }
}

const feedKey = (f) => `${f.Time}|${f.SessionID}|${f.Kind}|${f.Text}`;

// mergeFeed adds new items to a newest-first feed without duplicates (a
// reconnect replays recent items) and keeps at most max.
export function mergeFeed(feed, items, max = FEED_MAX) {
  const seen = new Set((feed || []).map(feedKey));
  const add = [];
  for (const f of items || []) {
    if (!f || !f.Kind) continue;
    const k = feedKey(f);
    if (seen.has(k)) continue;
    seen.add(k);
    add.push(f);
  }
  return [...add, ...(feed || [])]
    .sort((a, b) => (Date.parse(b.Time) || 0) - (Date.parse(a.Time) || 0))
    .slice(0, max);
}

// filterFeed keeps the kinds switched on (an empty set means all).
export function filterFeed(feed, kinds) {
  if (!kinds || !kinds.size) return feed || [];
  return (feed || []).filter((f) => kinds.has(f.Kind));
}

// ---------- keyboard shortcuts ----------

// SHORTCUTS: key sequence → action, with the help text for the ? overlay.
export const SHORTCUTS = [
  ['g w', 'wall', 'Go to the wall'],
  ['g s', 'sessions', 'Go to sessions'],
  ['g c', 'chat', 'Go to the LLM chat'],
  ['n', 'new', 'New task'],
  ['/', 'search', 'Search sessions'],
  ['j', 'down', 'Move selection down'],
  ['k', 'up', 'Move selection up'],
  ['x', 'select', 'Select or unselect'],
  ['Enter', 'open', 'Open the highlighted session'],
  ['.', 'bulk', 'Open the bulk bar'],
  ['?', 'help', 'Show this help'],
  ['Escape', 'escape', 'Close, unfocus or clear'],
];

export const SEQ_TIMEOUT = 1500;

// shortcutStep feeds one key to the parser. state is { prefix, at } (or
// null); it returns { state, action } where action is null while a
// sequence is still open or nothing matched. Keys with Ctrl/Alt/Meta never
// match.
export function shortcutStep(state, ev, now = Date.now()) {
  if (!ev || ev.ctrlKey || ev.altKey || ev.metaKey) return { state: null, action: null };
  const key = ev.key;
  const open = state && state.prefix && now - state.at <= SEQ_TIMEOUT ? state.prefix : '';
  if (open) {
    const hit = SHORTCUTS.find(([seq]) => seq === `${open} ${key}`);
    return { state: null, action: hit ? hit[1] : null };
  }
  if (SHORTCUTS.some(([seq]) => seq.startsWith(`${key} `))) return { state: { prefix: key, at: now }, action: null };
  const hit = SHORTCUTS.find(([seq]) => seq === key);
  return { state: null, action: hit ? hit[1] : null };
}

// isTypingContext: shortcuts stay quiet while the user types in a field, a
// terminal has focus, or a dialog is open. t = { tag, editable, inTerminal,
// dialogOpen, type }.
export function isTypingContext(t) {
  if (!t) return false;
  if (t.dialogOpen || t.inTerminal || t.editable) return true;
  const tag = String(t.tag || '').toUpperCase();
  if (tag === 'TEXTAREA' || tag === 'SELECT') return true;
  if (tag === 'INPUT') return !['checkbox', 'radio', 'button', 'submit', 'reset'].includes(String(t.type || '').toLowerCase());
  return false;
}

// moveCursor steps a highlighted index through n rows (−1 = none yet).
export function moveCursor(i, delta, n) {
  if (n <= 0) return -1;
  if (i < 0) return delta > 0 ? 0 : n - 1;
  return Math.max(0, Math.min(n - 1, i + delta));
}

// deleteMessage words the confirm for deleting sessions for good: what goes
// (record, transcript, recordings, Cerberus' own worktree or scratch folder)
// and what stays (a folder session's folder). infos are session Infos.
export function deleteMessage(infos) {
  const list = (infos || []).filter(Boolean);
  const n = list.length;
  const kinds = new Set(list.map((s) => (s.Workspace || (s.Repo ? 'repo' : 'scratch'))));
  const live = list.filter((s) => !['passed', 'failed', 'stopped', 'error', 'accepted', 'discarded'].includes(s.State)).length;
  const parts = [];
  if (live) parts.push(n === 1 ? 'It is stopped first.' : `${live} still running ${live === 1 ? 'is' : 'are'} stopped first.`);
  parts.push(n === 1 ? 'Its transcript, recordings and history go for good.' : 'Their transcripts, recordings and history go for good.');
  if (kinds.has('repo')) parts.push('A repo worktree is deleted; one with work that was not accepted is kept unless you confirm again.');
  if (kinds.has('scratch')) parts.push('A scratch folder is deleted: save or download anything you want first.');
  if (kinds.has('folder')) parts.push('Your own folders are not touched.');
  const name = n === 1 ? (list[0].Name || list[0].Title || list[0].ID) : '';
  return {
    title: n === 1 ? 'Delete session' : `Delete ${n} sessions`,
    message: (name ? `“${name}”. ` : '') + parts.join(' '),
    ok: n === 1 ? 'Delete' : `Delete ${n}`,
  };
}
