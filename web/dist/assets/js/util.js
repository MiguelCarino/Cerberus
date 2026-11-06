// Pure helpers for the Cerberus UI: no DOM, no network, so node --test can
// import them directly (see web/test/smoke.test.mjs).

import { parseReplayTime } from './cast.js';

// ---------- session states ----------

// Display groups for the Sessions list, in the order they are shown.
export const GROUPS = [
  { key: 'waiting', label: 'Needs you', states: ['waiting'] },
  { key: 'active', label: 'Running', states: ['starting', 'running', 'checking', 'retrying', 'escalated'] },
  { key: 'review', label: 'Ready for review', states: ['passed', 'failed'] },
  { key: 'queued', label: 'Queued', states: ['queued'] },
  { key: 'done', label: 'Finished', states: ['accepted', 'discarded', 'stopped', 'error'] },
];

const groupOf = new Map(GROUPS.flatMap((g) => g.states.map((s) => [s, g.key])));

// groupSessions buckets sessions by display group, newest first inside each
// group, and drops empty groups. Unknown states land in "active" so a new
// server state is never hidden.
export function groupSessions(list) {
  const buckets = new Map(GROUPS.map((g) => [g.key, []]));
  for (const s of list || []) buckets.get(groupOf.get(s.State) || 'active').push(s);
  return GROUPS.map((g) => ({ key: g.key, label: g.label, items: buckets.get(g.key).sort(byUpdatedDesc) }))
    .filter((g) => g.items.length > 0);
}

function byUpdatedDesc(a, b) {
  return (Date.parse(b.Updated) || 0) - (Date.parse(a.Updated) || 0);
}

// stateTone maps a session state to a Branding status colour.
export function stateTone(state) {
  switch (state) {
    case 'waiting': return 'warn';
    case 'passed': case 'accepted': return 'ok';
    case 'failed': case 'error': return 'err';
    case 'starting': case 'running': case 'checking': case 'retrying': case 'escalated': return 'info';
    default: return 'muted';
  }
}

// The agent process (and its PTY) is alive: keystrokes reach it.
export const isLive = (s) => s === 'starting' || s === 'running' || s === 'waiting';
// Finished with the worktree still there: a follow-up starts a new attempt.
export const canFollowUp = (s) => s === 'passed' || s === 'failed' || s === 'stopped' || s === 'error';
export const canAccept = canFollowUp;
// Resume starts a new attempt in the same workspace (the agent's own resume
// when the profile has one): the same final, non-closed states.
export const canResume = canFollowUp;
export const canStop = (s) => isLive(s) || s === 'queued' || s === 'checking' || s === 'retrying' || s === 'escalated';
export const canDiscard = (s) => s !== 'accepted' && s !== 'discarded';
export const isClosed = (s) => s === 'accepted' || s === 'discarded';

// Internal sessions (a fine-tuning run) take no follow-ups: the server
// refuses them, since a new attempt would rerun the trainer over the first
// run's output.
export const FINETUNE_TRAIN_AGENT = 'finetune-train';
export const isInternalAgent = (agent) => agent === FINETUNE_TRAIN_AGENT;
// takesFollowUp: a follow-up (a new attempt) can be sent to this session.
export const takesFollowUp = (info) => !!info && canFollowUp(info.State) && !isInternalAgent(info.Agent);

// termInputHint is the terminal input's placeholder for a session state:
// what typing does now, or why it cannot. agent is the session's agent.
export function termInputHint(state, agent) {
  if (isLive(state)) return 'Type and press Send (Enter is added)';
  if (canFollowUp(state) && isInternalAgent(agent)) return 'No follow-ups for a training run';
  if (canFollowUp(state)) return 'Follow-up task for a new attempt…';
  if (state === 'queued') return 'Queued: type once it starts';
  if (state === 'checking' || state === 'retrying' || state === 'escalated') return 'Busy: type once this run finishes';
  return 'Session closed';
}

export function countWaiting(list) {
  let n = 0;
  for (const s of list || []) if (s.State === 'waiting') n++;
  return n;
}

// ---------- terminal stream ----------

// b64ToBytes decodes the base64 payload of a stream event to raw bytes, so
// xterm.js can do the UTF-8 decoding (multi-byte characters may be split
// across events).
export function b64ToBytes(b64) {
  const bin = atob(b64 || '');
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out;
}

// Quick-key bar: label → bytes sent to the PTY.
export const QUICK_KEYS = [
  ['Enter', '\r'], ['y', 'y'], ['n', 'n'], ['a', 'a'], ['Esc', '\x1b'],
  ['Ctrl-C', '\x03'], ['↑', '\x1b[A'], ['↓', '\x1b[B'], ['Tab', '\t'],
];

// quickKeyData returns what a quick key sends. The built-in agent reads a
// whole line for approvals, so y/n/a also press Enter there.
export function quickKeyData(label, waitingReason) {
  const k = QUICK_KEYS.find(([l]) => l === label);
  if (!k) return '';
  if (waitingReason === 'approval' && (label === 'y' || label === 'n' || label === 'a')) return k[1] + '\r';
  return k[1];
}

// ---------- unified diff ----------

const HUNK_RE = /^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@(.*)$/;

function stripPath(p) {
  p = p.replace(/\t.*$/, '').trim();
  if (p.startsWith('"') && p.endsWith('"')) p = p.slice(1, -1);
  if (p === '/dev/null') return null;
  return p.replace(/^[ab]\//, '');
}

function newFile() {
  return { oldPath: null, newPath: null, path: '', status: 'modified', binary: false, adds: 0, dels: 0, hunks: [] };
}

function finishFile(f) {
  if (f.newPath === null && f.oldPath !== null && f.status === 'modified') f.status = 'deleted';
  if (f.oldPath === null && f.newPath !== null && f.status === 'modified') f.status = 'added';
  f.path = f.newPath ?? f.oldPath ?? f.path;
  return f;
}

// parseDiff turns `git diff` output into files → hunks → lines. Each line has
// type add | del | ctx | meta and its old/new line numbers, so a comment can
// cite file:line. Lines inside a hunk are counted against the hunk header,
// so a removed line that itself starts with "-- " is not taken for a header.
export function parseDiff(text) {
  const files = [];
  let f = null;
  let h = null;
  let oldLeft = 0;
  let newLeft = 0;
  let oldNo = 0;
  let newNo = 0;
  const lines = (text || '').split('\n');
  if (lines.length && lines[lines.length - 1] === '') lines.pop();

  const start = () => { if (f) files.push(finishFile(f)); f = newFile(); h = null; oldLeft = newLeft = 0; };

  for (const raw of lines) {
    const line = raw.endsWith('\r') ? raw.slice(0, -1) : raw;
    if (h && (oldLeft > 0 || newLeft > 0)) {
      const c = line[0];
      if (c === '+') { h.lines.push({ type: 'add', text: line.slice(1), oldNo: null, newNo: newNo++ }); newLeft--; f.adds++; continue; }
      if (c === '-') { h.lines.push({ type: 'del', text: line.slice(1), oldNo: oldNo++, newNo: null }); oldLeft--; f.dels++; continue; }
      if (c === ' ' || line === '') { h.lines.push({ type: 'ctx', text: line.slice(1), oldNo: oldNo++, newNo: newNo++ }); oldLeft--; newLeft--; continue; }
      if (c === '\\') { h.lines.push({ type: 'meta', text: line, oldNo: null, newNo: null }); continue; }
      // Malformed counts: fall through and treat the line as a header.
      oldLeft = newLeft = 0;
    }
    if (line.startsWith('\\') && h) { h.lines.push({ type: 'meta', text: line, oldNo: null, newNo: null }); continue; }
    if (line.startsWith('diff --git ')) {
      start();
      const m = /^diff --git (?:"?a\/(.+?)"?) (?:"?b\/(.+?)"?)$/.exec(line);
      if (m) { f.oldPath = m[1]; f.newPath = m[2]; f.path = m[2]; }
      continue;
    }
    if (line.startsWith('--- ')) {
      if (!f || f.hunks.length) start();
      f.oldPath = stripPath(line.slice(4));
      continue;
    }
    if (line.startsWith('+++ ') && f) { f.newPath = stripPath(line.slice(4)); continue; }
    const hm = HUNK_RE.exec(line);
    if (hm && f) {
      oldNo = +hm[1]; newNo = +hm[3];
      oldLeft = hm[2] === undefined ? 1 : +hm[2];
      newLeft = hm[4] === undefined ? 1 : +hm[4];
      h = { header: line, section: hm[5].trim(), lines: [] };
      f.hunks.push(h);
      continue;
    }
    if (!f) continue;
    if (line.startsWith('new file mode')) f.status = 'added';
    else if (line.startsWith('deleted file mode')) f.status = 'deleted';
    else if (line.startsWith('rename from ')) { f.status = 'renamed'; f.oldPath = line.slice(12); }
    else if (line.startsWith('rename to ')) { f.status = 'renamed'; f.newPath = line.slice(10); }
    else if (line.startsWith('Binary files ') || line === 'GIT binary patch') f.binary = true;
  }
  if (f) files.push(finishFile(f));
  return files;
}

// lineRef is the file:line a comment points at: the new line number, or the
// old one (marked) for a removed line.
export function lineRef(path, line) {
  if (line.newNo != null) return `${path}:${line.newNo}`;
  if (line.oldNo != null) return `${path}:${line.oldNo} (removed line)`;
  return path;
}

// formatComments builds the single follow-up message for a batch of diff
// comments: [{ref, code, text}].
export function formatComments(comments) {
  const items = (comments || []).filter((c) => c.text && c.text.trim());
  if (!items.length) return '';
  const body = items.map((c, i) => {
    const code = c.code != null && c.code.trim() ? `\n   > ${c.code.trim()}` : '';
    return `${i + 1}. ${c.ref}${code}\n   ${c.text.trim().replace(/\n/g, '\n   ')}`;
  });
  return `Review comments on your changes. Address each one, then stop.\n\n${body.join('\n\n')}`;
}

// ---------- formatting ----------

export function fmtNum(n, digits = 1) {
  if (n == null || !Number.isFinite(n) || n === 0) return '–';
  return n >= 100 ? Math.round(n).toLocaleString('en') : n.toFixed(digits);
}

export function fmtInt(n) {
  return n == null || !Number.isFinite(n) ? '–' : Math.round(n).toLocaleString('en');
}

export function fmtMiB(mib) {
  if (mib == null || mib <= 0) return '–';
  return mib >= 1024 ? `${(mib / 1024).toFixed(1)} GiB` : `${mib} MiB`;
}

export function fmtMs(ms) {
  if (ms == null || ms <= 0) return '–';
  return ms >= 10000 ? `${(ms / 1000).toFixed(0)} s` : ms >= 1000 ? `${(ms / 1000).toFixed(1)} s` : `${ms} ms`;
}

export function fmtEnergy(j) {
  if (j == null || j <= 0) return '–';
  if (j >= 3.6e6) return `${(j / 3.6e6).toFixed(2)} kWh`;
  if (j >= 3600) return `${(j / 3600).toFixed(1)} Wh`;
  if (j < 0.05) return '<0.1 J'; // a short request: never "0.0 J" beside a cost
  return `${j.toFixed(j >= 100 ? 0 : 1)} J`;
}

// relTime renders an RFC 3339 time relative to now ("3 min ago"); Go's zero
// time and unparsable values come back as "–".
export function relTime(iso, now = Date.now()) {
  const t = Date.parse(iso);
  if (!t || t < 0 || iso.startsWith('0001-')) return '–';
  const s = Math.round((now - t) / 1000);
  if (s < 45) return 'just now';
  if (s < 3600) return `${Math.round(s / 60)} min ago`;
  if (s < 86400) return `${Math.round(s / 3600)} h ago`;
  return `${Math.round(s / 86400)} d ago`;
}

export function fmtUptime(sec) {
  if (sec == null || sec < 0) return '–';
  const d = Math.floor(sec / 86400);
  const h = Math.floor((sec % 86400) / 3600);
  const m = Math.floor((sec % 3600) / 60);
  return d ? `${d}d ${h}h` : h ? `${h}h ${m}m` : `${m}m`;
}

// versionLabel shows a build version: "v1.2.0" for a release number, the
// bare text for a git description ("e0f264e-dirty") or "dev".
export function versionLabel(v) {
  const s = String(v || '').trim();
  if (!s) return '';
  return /^\d/.test(s) ? 'v' + s : s;
}

export function debounce(fn, ms) {
  let t = 0;
  const d = (...args) => { clearTimeout(t); t = setTimeout(() => fn(...args), ms); };
  d.cancel = () => clearTimeout(t);
  return d;
}

// parseRoute maps a location hash to a view and its parameters.
export function parseRoute(hash) {
  const parts = (hash || '').replace(/^#\/?/, '').split('/').filter(Boolean).map(decodeURIComponent);
  switch (parts[0]) {
    case 's':
      if (!parts[1]) return { view: 'sessions' };
      if (parts[2] === 'diff') return { view: 'diff', id: parts[1] };
      // #/s/<id>/replay[/<attempt>[/<seconds>]]
      if (parts[2] === 'replay') {
        const attempt = /^[1-9][0-9]{0,8}$/.test(parts[3] || '') ? +parts[3] : 0;
        return { view: 'session', id: parts[1], tab: 'replay', attempt, t: attempt ? parseReplayTime(parts[4]) : 0 };
      }
      // #/s/<id>/files, #/s/<id>/flow and #/s/<id>/preview open those tabs.
      if (parts[2] === 'files' || parts[2] === 'flow' || parts[2] === 'preview') return { view: 'session', id: parts[1], tab: parts[2] };
      return { view: 'session', id: parts[1] };
    case 'b': return parts[1] ? { view: 'batch', id: parts[1] } : { view: 'sessions' };
    // #/wall opens Sessions in its Wall layout; #/wall/a,b,c is the
    // full-screen watch page for those ids.
    case 'wall': return parts[1] ? { view: 'wall', ids: parts[1].split(',').filter(Boolean) } : { view: 'sessions', layout: 'wall' };
    // #/chat opens the last chat (or a new one), #/chat/new a new one,
    // #/chat/<id> that chat.
    case 'chat': return parts[1] ? { view: 'chat', id: parts[1] } : { view: 'chat' };
    case 'models': return { view: 'models' };
    case 'monitor': return { view: 'monitor' };
    case 'usage': return { view: 'usage' };
    case 'secrets': return { view: 'secrets' }; // redirects to #/settings/secrets
    case 'security': return { view: 'security' }; // redirects to #/settings/security
    // #/settings, #/settings/<group slug> or #/settings/security.
    case 'settings': return { view: 'settings', group: parts[1] || '' };
    case 'sessions': return { view: 'sessions' };
    // Flows: #/flows (the list and recent runs), #/flows/new[/<template
    // key>] (the editor on a new draft), #/flows/<id> (the editor) and
    // #/flows/runs/<run> (a run's page).
    case 'flows':
      if (parts[1] === 'runs') return parts[2] ? { view: 'flowrun', run: parts[2] } : { view: 'flows' };
      if (parts[1] === 'new') return parts[2] ? { view: 'flowedit', id: 'new', template: parts[2] } : { view: 'flowedit', id: 'new' };
      return parts[1] ? { view: 'flowedit', id: parts[1] } : { view: 'flows' };
    // Evals (E1): #/evals/runs/<run> is a run's page; the list lives in Usage.
    case 'evals': return parts[1] === 'runs' && parts[2] ? { view: 'evalrun', run: parts[2] } : { view: 'usage' };
    // Files handed over by another Carino page land on Sessions (choose a session or a new task).
    case 'carino-bridge': return { view: 'sessions' };
    case 'login': return { view: 'login' };
    // Users and roles (phase 4a): the first-run admin, the forced password
    // change and the Account page (Settings → Users is #/settings/users).
    case 'first-admin': return { view: 'first-admin' };
    case 'password': return { view: 'password' };
    case 'account': return { view: 'account' };
    // An empty hash opens the role's home (app.js resolves it).
    case undefined: return { view: 'home' };
    // Anything else is the Not found view.
    default: return { view: 'notfound', path: '#/' + parts.join('/') };
  }
}

// repoName is the last path element, for compact labels.
export function repoName(path) {
  if (!path) return '';
  const p = path.replace(/[\\/]+$/, '');
  return p.slice(Math.max(p.lastIndexOf('/'), p.lastIndexOf('\\')) + 1);
}

// ---------- workspaces ----------

// workspaceKind is a session's workspace: repo | folder | scratch. Sessions
// from before workspaces have only Repo.
export function workspaceKind(s) {
  if (!s) return 'scratch';
  if (s.Workspace) return s.Workspace;
  return s.Repo ? 'repo' : 'scratch';
}

// workspaceLabel names where a session works, for lists and headers.
export function workspaceLabel(s) {
  const kind = workspaceKind(s);
  if (kind === 'scratch') return 'scratch';
  return repoName(s.Dir || s.Repo);
}

export const canDiff = (s) => workspaceKind(s) === 'repo' && !isClosed(s.State);
export const canAcceptSession = (s) => workspaceKind(s) === 'repo' && canAccept(s.State);
export const canSave = (s) => workspaceKind(s) === 'scratch' && s.State !== 'discarded';

// breadcrumbs splits path into clickable crumbs starting at home ("~").
// Paths outside home (should not happen) start at "/".
export function breadcrumbs(path, home) {
  const norm = (p) => (p || '').replace(/\/+$/, '') || '/';
  path = norm(path);
  home = home ? norm(home) : '';
  let base = '/';
  let rest = path;
  const crumbs = [];
  if (home && (path === home || path.startsWith(home + '/'))) {
    crumbs.push({ name: '~', path: home });
    base = home;
    rest = path.slice(home.length);
  } else {
    crumbs.push({ name: '/', path: '/' });
    base = '';
  }
  for (const part of rest.split('/').filter(Boolean)) {
    base = base === '/' ? '/' + part : base + '/' + part;
    crumbs.push({ name: part, path: base });
  }
  return crumbs;
}

// folderNote says what choosing a folder means, as { tone, text } for an
// alert: a git repo gets an isolated worktree, a plain folder is edited live.
export function folderNote(sel, purpose = 'task') {
  if (!sel) return null;
  if (purpose === 'save') {
    return { tone: 'info', text: 'The scratch files are copied into this folder. Existing files are kept unless you allow overwriting.' };
  }
  if (purpose === 'chat') {
    return { tone: 'info', text: 'The chat’s tools work in this folder, live (no worktree). Reads outside it ask first.' };
  }
  if (sel.git) {
    return { tone: 'info', text: 'Git repo → the agent works in an isolated worktree. You review the diff and accept it onto main.' };
  }
  return { tone: 'warn', text: 'Plain folder → the agent edits it directly. Changes are live and cannot be undone by Cerberus.' };
}

// joinPath appends a relative sub-folder to an absolute path, refusing
// anything that would climb out of it ("" when invalid).
export function joinPath(dir, sub) {
  sub = (sub || '').trim().replace(/^\/+|\/+$/g, '');
  if (!sub) return dir;
  const parts = sub.split('/').filter(Boolean);
  if (parts.some((p) => p === '..' || p === '.')) return '';
  return dir.replace(/\/+$/, '') + '/' + parts.join('/');
}

// keyboardOpen tells a phone's on-screen keyboard from the visual viewport:
// it covers more than 120 px of the layout viewport while text is being
// typed, at no pinch zoom (zooming shrinks the visual viewport too).
export function keyboardOpen(innerHeight, viewportHeight, scale = 1, editing = true) {
  if (!editing || !(innerHeight > 0) || !(viewportHeight > 0)) return false;
  if (Math.abs((scale || 1) - 1) > 0.05) return false;
  return innerHeight - viewportHeight > 120;
}

// ---------- snapshot + stream ----------

// snapshotLoader keeps a list fetched with GET in step with events from
// the stream. load() fetches a snapshot; events that arrive while it is in
// flight are recorded with track() and handed to onSnapshot(snapshot,
// events) to re-apply on top, so a reply built before them cannot undo
// them. A load() asked for while one is in flight runs again after it
// (the missed events may postdate the first reply); one promise covers
// both. onError gets a failed fetch.
export function snapshotLoader({ fetch, onSnapshot, onError }) {
  let inflight = null;
  let again = false;
  let buffer = null;
  async function once() {
    buffer = [];
    try {
      const snap = await fetch();
      const events = buffer;
      buffer = null;
      onSnapshot(snap, events);
    } catch (e) {
      buffer = null;
      if (onError) onError(e);
    }
  }
  return {
    load() {
      if (inflight) { again = true; return inflight; }
      inflight = (async () => {
        try {
          do { again = false; await once(); } while (again);
        } finally {
          inflight = null;
        }
      })();
      return inflight;
    },
    // track records a stream event applied while a snapshot is in flight.
    track(ev) { if (buffer) buffer.push(ev); },
    busy: () => inflight !== null,
  };
}

// ---------- approval waits answered outside the terminal ----------

// approvalWait is a session's open approval prompt (the built-in agent's
// y / a / n), or null: {seq, ask, tool}. ask is what it asks for
// ("write_file: notes.txt", "" when the agent did not say); seq is the
// prompt's number, sent back so a late answer never lands on a later one.
export function approvalWait(s) {
  if (!s || s.State !== 'waiting' || s.WaitingReason !== 'approval') return null;
  const ask = typeof s.WaitingAsk === 'string' ? s.WaitingAsk : '';
  const seq = Number.isInteger(s.AskSeq) ? s.AskSeq : 0;
  const i = ask.indexOf(':');
  const always = Array.isArray(s.WaitingAlways) ? s.WaitingAlways : [];
  return { seq, ask, tool: i > 0 ? ask.slice(0, i).trim() : '', always };
}

// approvalWaits are the sessions waiting at an approval prompt, oldest
// change first (the one waiting longest leads).
export function approvalWaits(list) {
  return [...(list || [])].filter((s) => approvalWait(s))
    .sort((a, b) => String(a.Updated || '').localeCompare(String(b.Updated || '')));
}
