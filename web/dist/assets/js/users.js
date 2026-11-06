// Users and roles (phase 4a): pure helpers for the sign-in screens, the
// role-aware navigation and the Settings → Users panel. No DOM, no network,
// so node --test can import them (web/test/users.test.mjs).
//
// The server is the authority: GET /api/me says who is signed in, their
// role and their caps. These helpers only decide what the UI shows.

// ---------- roles and caps ----------

export const ROLES = ['admin', 'user', 'viewer', 'monitor', 'chat'];

export const ROLE_LABEL = { admin: 'Admin', user: 'User', viewer: 'Viewer', monitor: 'Monitor', chat: 'Chat only' };

// One line per role, for the role select and the Account page.
export const ROLE_HELP = {
  admin: 'Everything: users, settings, secrets, models, and every session and chat.',
  user: 'Their own chats, sessions and flows, sandboxed, within their budget.',
  viewer: 'Watches every session and the Monitor, read only. No chats.',
  monitor: 'The Monitor page only: power, energy, bill and processes.',
  chat: 'Their own LLM chats only: plain chat with images, no tools.',
};

export const ALL_CAPS = [
  'chat', 'chat.tools', 'chat.import', 'sessions', 'sessions.watch_all', 'sessions.write', 'flows',
  'monitor', 'monitor.signal', 'doctor', 'doctor.counts', 'models.read', 'models.manage', 'usage', 'usage.all',
  'gateway', 'voice', 'devices', 'devices.all', 'settings', 'secrets', 'users', 'audit', 'finetune', 'host',
];

// The role table of docs/SCOPE.md "Users and roles", used only when a reply
// carries no caps (the server sends them in GET /api/me).
export const ROLE_CAPS = {
  admin: ALL_CAPS,
  user: ['chat', 'chat.tools', 'sessions', 'sessions.write', 'flows', 'models.read', 'usage', 'gateway', 'voice', 'devices'],
  viewer: ['sessions.watch_all', 'monitor', 'doctor.counts', 'devices'],
  monitor: ['monitor', 'doctor.counts', 'devices'],
  chat: ['chat', 'usage', 'voice', 'devices'],
};

export const ROLE_HOME = { admin: '#/chat', user: '#/chat', viewer: '#/sessions', monitor: '#/monitor', chat: '#/chat' };

const str = (v) => (v == null ? '' : String(v));
const num = (v) => (Number.isFinite(+v) ? +v : 0);

// LEGACY_ME stands in for a daemon from before users (no GET /api/me): the
// admin token, everything allowed, as before.
export const LEGACY_ME = Object.freeze({
  kind: 'admin-token', user: null, role: 'admin', caps: ALL_CAPS, home: '#/chat', hasUsers: false,
  bootstrap: false, mustChange: false, budget: null, apiKey: { set: false, hint: '', made: '' }, device: '', legacy: true,
});

// normMe turns GET /api/me into the shape the UI uses.
export function normMe(raw) {
  if (!raw || typeof raw !== 'object') return LEGACY_ME;
  const role = ROLES.includes(raw.role) ? raw.role : (raw.user && ROLES.includes(raw.user.role) ? raw.user.role : 'admin');
  const caps = Array.isArray(raw.caps) ? raw.caps.map(str) : ROLE_CAPS[role].slice();
  const home = typeof raw.home === 'string' && raw.home.startsWith('#/') ? raw.home : ROLE_HOME[role];
  const u = raw.user && typeof raw.user === 'object' ? {
    id: str(raw.user.id), name: str(raw.user.name), display: str(raw.user.display), role: ROLES.includes(raw.user.role) ? raw.user.role : role,
    mustChange: !!raw.user.must_change, created: str(raw.user.created), lastLogin: str(raw.user.last_login),
  } : null;
  const b = raw.budget && typeof raw.budget === 'object' ? raw.budget : null;
  const k = raw.api_key && typeof raw.api_key === 'object' ? raw.api_key : {};
  return {
    kind: str(raw.kind) || (u ? 'login' : 'admin-token'),
    user: u,
    role,
    caps,
    home,
    hasUsers: !!raw.has_users,
    bootstrap: !!raw.bootstrap,
    mustChange: !!(u && u.mustChange),
    budget: b ? { monthlyUSD: num(b.monthly_usd), monthlyTokens: num(b.monthly_tokens), spentUSD: num(b.spent_usd), spentTokens: num(b.spent_tokens) } : null,
    apiKey: { set: !!k.set, hint: str(k.hint), made: str(k.made) },
    device: str(raw.device),
    legacy: false,
  };
}

// can: does the signed-in person have this cap?
export const can = (me, cap) => !!me && Array.isArray(me.caps) && me.caps.includes(cap);

// canWriteSessions: may type into terminals, approve, start, stop, delete
// (own sessions for a user; any for an admin).
export const canWriteSessions = (me) => can(me, 'sessions.write') || can(me, 'sessions');

// canSavePermits: saved permissions are for people who run tools (the
// server's own-work class: admin and user).
export const canSavePermits = (me) => can(me, 'settings') || can(me, 'sessions.write') || can(me, 'chat.tools');

export const roleHome = (me) => (me && me.home) || '#/chat';

// whoText is how the navbar and Account name the person.
export function whoText(me) {
  if (!me) return '';
  if (me.user) return me.user.display || me.user.name;
  if (me.kind === 'device') return 'This device';
  return 'Admin token';
}

// initialOf is the account button's letter.
export function initialOf(me) {
  const t = whoText(me).trim();
  if (!me || !me.user) return '';
  const ch = [...t][0] || '';
  return ch.toUpperCase();
}

// ---------- views, tabs and the route guard ----------

// The caps that open each view (any one of them).
const SESSIONS_CAPS = ['sessions', 'sessions.watch_all'];
export const VIEW_CAPS = {
  sessions: SESSIONS_CAPS, session: SESSIONS_CAPS, diff: SESSIONS_CAPS, batch: SESSIONS_CAPS, wall: SESSIONS_CAPS,
  chat: ['chat'],
  flows: ['flows'], flowedit: ['flows'], flowrun: ['flows'],
  models: ['models.read'],
  monitor: ['monitor'],
  usage: ['usage'],
  settings: ['settings'],
};

// Views every signed-in person may open.
const OPEN_VIEWS = new Set(['account', 'notfound', 'login', 'first-admin', 'password', 'home']);

// viewAllowed: may this person open this route?
export function viewAllowed(r, me) {
  if (!r) return false;
  if (OPEN_VIEWS.has(r.view)) return true;
  if (r.view === 'settings' && r.group === 'users') return can(me, 'users');
  // Saved permissions are own work: people who run tools (users) reach
  // Settings → Permissions without the rest of Settings.
  if (r.view === 'settings' && (r.group === 'permissions' || !r.group) && !can(me, 'settings')) return canSavePermits(me);
  // Chat only has its own spend in Account; the Usage page (requests,
  // plans, the leaderboard) is for roles that run sessions.
  if (r.view === 'usage' || r.view === 'evalrun') return can(me, 'usage') && (can(me, 'sessions') || can(me, 'usage.all'));
  const caps = VIEW_CAPS[r.view];
  if (!caps) return false;
  return caps.some((c) => can(me, c));
}

// Bare views: no navbar tabs, no live events, no feed.
export const BARE_VIEWS = new Set(['login', 'first-admin', 'password']);
export const isBare = (view) => BARE_VIEWS.has(view);

// guardRoute decides what to show for route r (from parseRoute) and the
// signed-in person: {redirect: hash} or {route}. A view the role cannot use
// shows Not found (it does not confirm that the view exists).
export function guardRoute(r, me, hash = '') {
  // A person already signed in with a password goes home; a token or device
  // holder may still sign in with a password (after the first-run screen).
  if (r && r.view === 'login' && me && me.user) return { redirect: me.mustChange ? '#/password' : roleHome(me) };
  if (!r || r.view === 'login') return { route: r || { view: 'login' } };
  if (!me) return { redirect: '#/login' };
  if (me.bootstrap) return r.view === 'first-admin' ? { route: r } : { redirect: '#/first-admin' };
  if (r.view === 'first-admin') return { redirect: roleHome(me) };
  if (me.mustChange) return r.view === 'password' ? { route: r } : { redirect: '#/password' };
  if (r.view === 'password') return { redirect: '#/account' };
  if (r.view === 'home') return { redirect: roleHome(me) };
  if (!viewAllowed(r, me)) return { route: { view: 'notfound', path: r.path || hash } };
  return { route: r };
}

// The tab each view lights up.
export const TAB_OF = {
  sessions: 'sessions', session: 'sessions', diff: 'sessions', batch: 'sessions', wall: 'sessions', chat: 'chat',
  models: 'models', monitor: 'monitor', usage: 'usage', secrets: 'settings', settings: 'settings',
  flows: 'flows', flowedit: 'flows', flowrun: 'flows', account: 'account', evalrun: 'usage',
};

// tabVisible: the navbar and bottom tabs a role sees ("more" always: it
// holds Account).
export function tabVisible(tab, me) {
  if (tab === 'more' || tab === 'account') return true;
  // Settings is a tab for admins; a user reaches its Permissions page from Account.
  if (tab === 'settings') return can(me, 'settings');
  return viewAllowed({ view: tab }, me);
}

// The phone's More sheet: [tab, label, sub].
export const MORE_ITEMS = [
  ['flows', 'Flows', 'Designed, scheduled and triggered tasks'],
  ['models', 'Models', 'GPUs, loaded models, tok/s'],
  ['usage', 'Usage', 'Requests, spend and plans'],
  ['settings', 'Settings', 'Every option, users, secrets, devices, about'],
];

// moreItems is the More sheet for this person: the views their role opens,
// then Account.
export function moreItems(me) {
  return [...MORE_ITEMS.filter(([tab]) => tabVisible(tab, me)),
    ['account', 'Account', me && me.user ? 'Password, browsers, devices, sign out' : 'Devices, sign out']];
}

// ---------- forms ----------

export const NAME_RE = /^[a-z0-9][a-z0-9._-]{0,31}$/;

// cleanName is what a typed user name becomes (the server's spelling).
export const cleanName = (v) => str(v).trim().toLowerCase();

// nameError explains a bad user name ("" when fine).
export function nameError(v) {
  const n = cleanName(v);
  if (!n) return 'Enter a user name.';
  if (n.length > 32) return 'At most 32 characters.';
  if (!NAME_RE.test(n)) return 'Lowercase letters, digits, dot, dash or underscore; start with a letter or digit.';
  return '';
}

const byteLen = (s) => new TextEncoder().encode(str(s)).length;

// passwordError checks the server's rules: 10 to 256 bytes, not the name.
export function passwordError(pw, name = '') {
  const p = str(pw);
  if (!p) return 'Enter a password.';
  const n = byteLen(p);
  if (n < 10) return 'At least 10 characters.';
  if (n > 256) return 'At most 256 bytes.';
  if (name && p.toLowerCase() === cleanName(name)) return 'The password cannot be the user name.';
  return '';
}

// newPasswordError checks a new password typed twice.
export function newPasswordError(pw, again, name = '') {
  const e = passwordError(pw, name);
  if (e) return e;
  if (str(pw) !== str(again)) return 'The two passwords do not match.';
  return '';
}

export const displayError = (v) => ([...str(v).trim()].length > 60 ? 'At most 60 characters.' : '');

// ---------- sign-in errors ----------

// lockMinutes turns retry_after seconds into whole minutes (at least 1).
export const lockMinutes = (sec) => Math.max(1, Math.ceil(num(sec) / 60));

const minutesText = (n) => `${n} minute${n === 1 ? '' : 's'}`;

// loginErrorText maps a refused POST /api/login (status, body {error, code,
// retry_after}, Retry-After header) to the one line the login card shows.
// Wrong name, wrong password and a disabled account read the same.
export function loginErrorText(status, body, retryHeader = '') {
  const b = body && typeof body === 'object' ? body : {};
  if (status === 429 || b.code === 'locked') {
    const sec = num(b.retry_after) || num(retryHeader) || 900;
    return `Too many attempts. Try again in ${minutesText(lockMinutes(sec))}.`;
  }
  if (status === 401 || b.code === 'login') return 'Wrong user name or password.';
  if (status === 403 && b.code === 'must_change') return 'Change your password first.';
  if (status === 403) return 'This sign-in was refused. Open Cerberus at its own address and try again.';
  if (status === 400) return 'Enter your user name and password.';
  if (status === 0) return 'Cannot reach Cerberus. Check the connection and try again.';
  return (typeof b.error === 'string' && b.error) || `Sign-in failed (${status}).`;
}

// tokenErrorText is the same for the admin-token sign-in.
export function tokenErrorText(status, body, retryHeader = '') {
  if (status === 401) return 'That token is not valid.';
  return loginErrorText(status, body, retryHeader);
}

// passwordChangeErrorText maps a refused POST /api/me/password.
export function passwordChangeErrorText(status, body) {
  const b = body && typeof body === 'object' ? body : {};
  if (b.code === 'login') return 'The current password is wrong.';
  if (b.code === 'locked' || status === 429) return loginErrorText(429, b);
  if (b.code === 'no_user') return 'The admin token has no password. Sign in with a user to change one.';
  if (b.code === 'password') return (typeof b.error === 'string' && b.error) || 'That password does not meet the rules.';
  return (typeof b.error === 'string' && b.error) || `Could not change the password (${status}).`;
}

// ---------- the Users panel ----------

// normUser turns a UserPublic into the panel's row.
export function normUser(u) {
  const b = (u && u.budget) || {};
  const k = (u && u.api_key) || {};
  return {
    id: str(u && u.id), name: str(u && u.name), display: str(u && u.display),
    role: ROLES.includes(u && u.role) ? u.role : 'user',
    disabled: !!(u && u.disabled), mustChange: !!(u && u.must_change), locked: !!(u && u.locked),
    created: str(u && u.created), lastLogin: str(u && u.last_login), lastAddr: str(u && u.last_addr),
    budget: { monthlyUSD: num(b.monthly_usd), monthlyTokens: num(b.monthly_tokens) },
    spentUSD: num(u && u.spent_usd), spentTokens: num(u && u.spent_tokens),
    sessions: num(u && u.sessions), chats: num(u && u.chats), flows: num(u && u.flows), devices: num(u && u.devices),
    repos: Array.isArray(u && u.repos) ? u.repos.map((r) => ({ path: str(r.path), accept: !!r.accept })).filter((r) => r.path) : [],
    agents: Array.isArray(u && u.agents) ? u.agents.map(str).filter(Boolean) : [],
    apiKey: { set: !!k.set, hint: str(k.hint), made: str(k.made) },
    legacyOwner: !!(u && u.legacy_owner),
  };
}

// normUsers turns GET /api/users into {users (admins first, then by name), roles, agents}.
export function normUsers(raw) {
  const list = Array.isArray(raw) ? raw : (raw && Array.isArray(raw.users) ? raw.users : []);
  const users = list.map(normUser).filter((u) => u.id)
    .sort((a, b) => (ROLES.indexOf(a.role) - ROLES.indexOf(b.role)) || a.name.localeCompare(b.name));
  const roles = raw && Array.isArray(raw.roles) ? raw.roles.filter((r) => ROLES.includes(r)) : ROLES.slice();
  const agents = raw && Array.isArray(raw.agents) ? raw.agents.map(str).filter(Boolean) : [];
  return { users, roles: roles.length ? roles : ROLES.slice(), agents };
}

// userStatus is a row's state: {key, label, tone}. Disabled wins, then
// Locked (too many failed sign-ins), then Must change.
export function userStatus(u) {
  if (u.disabled) return { key: 'disabled', label: 'Disabled', tone: 'muted' };
  if (u.locked) return { key: 'locked', label: 'Locked', tone: 'err' };
  if (u.mustChange) return { key: 'must_change', label: 'Must change password', tone: 'warn' };
  return { key: 'active', label: 'Active', tone: 'ok' };
}

export const usd = (n) => `US$${num(n).toFixed(2)}`;

// fmtTokens: 950, 48.2k, 1.3M.
export function fmtTokens(n) {
  const v = num(n);
  if (v < 1000) return String(Math.round(v));
  if (v < 1e6) return `${(v / 1000).toFixed(v < 1e4 ? 1 : 0).replace(/\.0$/, '')}k`;
  return `${(v / 1e6).toFixed(1).replace(/\.0$/, '')}M`;
}

// spendText is "US$1.25 of US$5.00 · 48.2k tokens" (the budget only when set).
export function spendText(spentUSD, spentTokens, budget = {}) {
  const b = budget || {};
  const money = b.monthlyUSD > 0 ? `${usd(spentUSD)} of ${usd(b.monthlyUSD)}` : usd(spentUSD);
  const toks = b.monthlyTokens > 0 ? `${fmtTokens(spentTokens)} of ${fmtTokens(b.monthlyTokens)} tokens` : `${fmtTokens(spentTokens)} tokens`;
  return `${money} · ${toks}`;
}

// budgetShare is the larger used share of the caps (0..1+), or -1 with no cap.
export function budgetShare(spentUSD, spentTokens, budget = {}) {
  const b = budget || {};
  const parts = [];
  if (b.monthlyUSD > 0) parts.push(num(spentUSD) / b.monthlyUSD);
  if (b.monthlyTokens > 0) parts.push(num(spentTokens) / b.monthlyTokens);
  return parts.length ? Math.max(...parts) : -1;
}

export const budgetText = (b) => {
  const parts = [];
  if (b && b.monthlyUSD > 0) parts.push(`${usd(b.monthlyUSD)} a month`);
  if (b && b.monthlyTokens > 0) parts.push(`${fmtTokens(b.monthlyTokens)} tokens a month`);
  return parts.length ? parts.join(' · ') : 'No cap';
};

// parseBudget reads the budget form ("" = 0 = no cap): {budget} or {error}.
export function parseBudget(usdText, tokensText) {
  const u = str(usdText).trim().replace(/^US\$|^\$/, '');
  const t = str(tokensText).trim().replace(/[_,\s]/g, '');
  const usdV = u === '' ? 0 : Number(u);
  const tokV = t === '' ? 0 : Number(t);
  if (!Number.isFinite(usdV) || usdV < 0) return { error: 'The US$ cap must be a number, 0 or more.' };
  if (!Number.isInteger(tokV) || tokV < 0) return { error: 'The token cap must be a whole number, 0 or more.' };
  return { budget: { monthly_usd: Math.round(usdV * 100) / 100, monthly_tokens: tokV } };
}

const plural = (n, one, many = `${one}s`) => `${n} ${n === 1 ? one : many}`;

// listText joins ["a", "b", "c"] as "a, b and c".
export function listText(parts) {
  if (parts.length <= 1) return parts.join('');
  return `${parts.slice(0, -1).join(', ')} and ${parts[parts.length - 1]}`;
}

// enabledAdmins counts the admins who can still sign in.
export const enabledAdmins = (users) => (users || []).filter((u) => u.role === 'admin' && !u.disabled).length;

// lastAdminReason is why a row's role, disable and delete controls are off
// ("" when they are on): the last enabled admin stays.
export function lastAdminReason(u, users) {
  if (u && u.role === 'admin' && !u.disabled && enabledAdmins(users) <= 1) {
    return 'The last admin cannot be demoted, disabled or deleted. Make another admin first.';
  }
  return '';
}

// selfReason is why an admin cannot disable, demote or delete themselves.
export function selfReason(u, me) {
  if (u && me && me.user && me.user.id === u.id) return 'You cannot disable, demote or delete yourself. Ask another admin.';
  return '';
}

// deleteSummary is the delete dialog's sentence: what moves to whom.
export function deleteSummary(u, toName = 'you') {
  const parts = [];
  if (u.sessions) parts.push(plural(u.sessions, 'session'));
  if (u.chats) parts.push(plural(u.chats, 'chat'));
  if (u.flows) parts.push(plural(u.flows, 'flow'));
  const dev = u.devices ? ` ${u.devices === 1 ? 'Their paired device is' : `Their ${u.devices} paired devices are`} signed out.` : '';
  if (!parts.length) return `They have no sessions, chats or flows. Nothing is deleted.${dev}`;
  const stop = u.sessions ? ' Any of their sessions still running stop.' : '';
  const flows = u.flows ? ` ${u.flows === 1 ? 'Their flow does' : 'Their flows do'} not run until you open and save ${u.flows === 1 ? 'it' : 'each one'}.` : '';
  return `Moves to ${toName}: ${listText(parts)}. Nothing is deleted.${stop}${flows}${dev}`;
}

// deletedText is the toast after DELETE /api/users/{id}.
export function deletedText(name, reply, toName = 'you') {
  const m = (reply && reply.moved) || {};
  const parts = [];
  if (m.sessions) parts.push(plural(m.sessions, 'session'));
  if (m.chats) parts.push(plural(m.chats, 'chat'));
  if (m.flows) parts.push(plural(m.flows, 'flow'));
  if (m.runs) parts.push(plural(m.runs, 'flow run'));
  if (m.inbox) parts.push(plural(m.inbox, 'notification'));
  return parts.length ? `Deleted ${name}. ${listText(parts)} moved to ${toName}.` : `Deleted ${name}.`;
}

// cutText notes ended live connections after a revoking change.
export const cutText = (cut) => (num(cut) > 0 ? ` ${plural(num(cut), 'live connection')} ended.` : '');

// CLI_AGENTS are profiles whose own login the admin would share (their
// config folder is bound into the sandbox).
export const CLI_AGENTS = ['claude', 'opencode', 'codex', 'gemini'];
export const isCLIAgent = (name) => CLI_AGENTS.includes(str(name).toLowerCase());

// agentChoices lists the profiles the admin may allow, the built-in first.
export function agentChoices(agents) {
  const set = new Set(['cerberus', ...(agents || [])]);
  return [...set].map((name) => ({ name, cli: isCLIAgent(name), builtin: name === 'cerberus' }));
}

// repoChoices merges /api/repos with a user's shares: [{path, name, shared, accept}].
export function repoChoices(repos, shares) {
  const byPath = new Map((shares || []).map((s) => [s.path, s]));
  const paths = new Set([...(repos || []).map((r) => str(r && (r.path || r.Path || r))).filter(Boolean), ...byPath.keys()]);
  return [...paths].sort().map((path) => ({
    path, name: path.replace(/[\\/]+$/, '').split(/[\\/]/).pop() || path, shared: byPath.has(path), accept: !!(byPath.get(path) || {}).accept,
  }));
}

// ---------- owners on shared lists ----------

// ownerOf reads an object's owner id and name (sessions: Owner/OwnerName;
// chats, flows and runs: owner/owner_name).
export function ownerOf(x) {
  if (!x) return { id: '', name: '' };
  return { id: str(x.Owner ?? x.owner), name: str(x.OwnerName ?? x.owner_name) };
}

// ownerLabel is the chip text on an item someone else owns ("" for one's
// own, or before users exist).
export function ownerLabel(x, me) {
  if (!me || !me.hasUsers) return '';
  const o = ownerOf(x);
  if (!o.id && !o.name) return '';
  if (me.user && o.id && o.id === me.user.id) return '';
  return o.name || o.id;
}

// ownerChoices lists the owners in a list, for the owner filter: [{id, name}].
export function ownerChoices(list) {
  const seen = new Map();
  for (const x of list || []) {
    const o = ownerOf(x);
    if (o.id && !seen.has(o.id)) seen.set(o.id, o.name || o.id);
  }
  return [...seen].map(([id, name]) => ({ id, name })).sort((a, b) => a.name.localeCompare(b.name));
}

// ownedBy filters a list to one owner id ("" = everyone).
export const ownedBy = (list, id) => (id ? (list || []).filter((x) => ownerOf(x).id === id) : (list || []));

// isOwn: the item belongs to the signed-in person (or no users exist).
export function isOwn(x, me) {
  if (!me || !me.hasUsers || !me.user) return true;
  const o = ownerOf(x);
  return !o.id || o.id === me.user.id;
}

// ---------- audit filter ----------

// auditUserOptions is the audit log's user select: [value, label].
// "-" is records with no user (the admin token and the system).
export function auditUserOptions(users) {
  return [['', 'Any user'], ['-', 'Admin token or system'], ...(users || []).map((u) => [u.name, u.display ? `${u.name} (${u.display})` : u.name])];
}

// ---------- signed-in browsers ----------

// browserName is a short label for a User-Agent.
export function browserName(agent) {
  const a = str(agent);
  if (!a) return 'Unknown browser';
  const b = /Edg\//.test(a) ? 'Edge' : /OPR\//.test(a) ? 'Opera' : /Firefox\//.test(a) ? 'Firefox'
    : /Chrome\//.test(a) ? 'Chrome' : /Safari\//.test(a) ? 'Safari' : /curl\//.test(a) ? 'curl' : '';
  const os = /Android/.test(a) ? 'Android' : /iPhone|iPad/.test(a) ? 'iOS' : /Windows/.test(a) ? 'Windows'
    : /Mac OS X|Macintosh/.test(a) ? 'macOS' : /Linux/.test(a) ? 'Linux' : '';
  if (!b && !os) return a.slice(0, 40);
  return [b, os].filter(Boolean).join(' on ');
}

// normLogins sorts GET /api/me/logins: this browser first, then by last use.
export function normLogins(raw) {
  const list = raw && Array.isArray(raw.logins) ? raw.logins : Array.isArray(raw) ? raw : [];
  return list.map((l) => ({
    id: str(l.id), created: str(l.created), lastSeen: str(l.last_seen), expires: str(l.expires),
    addr: str(l.addr), agent: str(l.agent), current: !!l.current,
  })).filter((l) => l.id)
    .sort((a, b) => (b.current - a.current) || ((Date.parse(b.lastSeen) || 0) - (Date.parse(a.lastSeen) || 0)));
}
