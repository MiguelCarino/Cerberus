// Users and roles (phase 4a): the role capability map, tabs per role, the
// route guard, parseRoute's new views, sign-in error mapping and the
// Settings → Users helpers. Run: node --test web/test/
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import {
  ROLES, ROLE_LABEL, ROLE_HELP, ROLE_CAPS, ROLE_HOME, ALL_CAPS, LEGACY_ME, normMe, can, canWriteSessions, roleHome, whoText, initialOf,
  VIEW_CAPS, viewAllowed, guardRoute, isBare, tabVisible, moreItems, TAB_OF,
  nameError, cleanName, passwordError, newPasswordError, displayError,
  lockMinutes, loginErrorText, tokenErrorText, passwordChangeErrorText,
  normUser, normUsers, userStatus, usd, fmtTokens, spendText, budgetShare, budgetText, parseBudget, listText,
  enabledAdmins, lastAdminReason, selfReason, deleteSummary, deletedText, cutText,
  isCLIAgent, agentChoices, repoChoices, ownerOf, ownerLabel, ownerChoices, ownedBy, isOwn,
  auditUserOptions, browserName, normLogins,
} from '../dist/assets/js/users.js';
import { parseRoute } from '../dist/assets/js/util.js';
import { auditQuery, normAudit, normDevice, doctorCountsOf, normDoctor } from '../dist/assets/js/security.js';

// A Me for a role, as GET /api/me sends it.
const meFor = (role, extra = {}) => normMe({
  kind: 'login', role, caps: ROLE_CAPS[role], home: ROLE_HOME[role], has_users: true, bootstrap: false,
  user: { id: `u-${role}0000000`, name: role === 'chat' ? 'chatty' : role, display: '', role, must_change: false },
  ...extra,
});
const ME = Object.fromEntries(ROLES.map((r) => [r, meFor(r)]));

// ---------- roles and caps ----------

test('five roles with labels, help and homes', () => {
  assert.deepEqual(ROLES, ['admin', 'user', 'viewer', 'monitor', 'chat']);
  for (const r of ROLES) {
    assert.ok(ROLE_LABEL[r], r);
    assert.ok(ROLE_HELP[r], r);
    assert.match(ROLE_HOME[r], /^#\//, r);
  }
  assert.equal(ROLE_LABEL.chat, 'Chat only');
  assert.deepEqual(ROLE_HOME, { admin: '#/chat', user: '#/chat', viewer: '#/sessions', monitor: '#/monitor', chat: '#/chat' });
});

test('the role capability map follows the SCOPE role table', () => {
  assert.equal(ALL_CAPS.length, 25);
  assert.deepEqual([...ROLE_CAPS.admin].sort(), [...ALL_CAPS].sort());
  const has = (role, cap) => can(ME[role], cap);
  // chat: admin, user, chat
  assert.deepEqual(ROLES.filter((r) => has(r, 'chat')), ['admin', 'user', 'chat']);
  // chat tools: admin, user (chat-only is plain chat)
  assert.deepEqual(ROLES.filter((r) => has(r, 'chat.tools')), ['admin', 'user']);
  // start sessions: admin, user
  assert.deepEqual(ROLES.filter((r) => has(r, 'sessions')), ['admin', 'user']);
  // watch everyone's sessions: admin, viewer
  assert.deepEqual(ROLES.filter((r) => has(r, 'sessions.watch_all')), ['admin', 'viewer']);
  // flows: admin, user
  assert.deepEqual(ROLES.filter((r) => has(r, 'flows')), ['admin', 'user']);
  // monitor page: admin, viewer, monitor (not user)
  assert.deepEqual(ROLES.filter((r) => has(r, 'monitor')), ['admin', 'viewer', 'monitor']);
  // process signals: admin only
  assert.deepEqual(ROLES.filter((r) => has(r, 'monitor.signal')), ['admin']);
  // doctor counts: viewer, monitor; full report: admin
  assert.deepEqual(ROLES.filter((r) => has(r, 'doctor.counts')), ['admin', 'viewer', 'monitor']);
  assert.deepEqual(ROLES.filter((r) => has(r, 'doctor')), ['admin']);
  // gateway keys: admin, user
  assert.deepEqual(ROLES.filter((r) => has(r, 'gateway')), ['admin', 'user']);
  // own usage: admin, user, chat (Chat only sees it in Account)
  assert.deepEqual(ROLES.filter((r) => has(r, 'usage')), ['admin', 'user', 'chat']);
  assert.deepEqual(ROLES.filter((r) => has(r, 'sessions.write')), ['admin', 'user']);
  // voice: admin, user, chat
  assert.deepEqual(ROLES.filter((r) => has(r, 'voice')), ['admin', 'user', 'chat']);
  // own devices: everyone; all devices: admin
  assert.deepEqual(ROLES.filter((r) => has(r, 'devices')), ROLES);
  assert.deepEqual(ROLES.filter((r) => has(r, 'devices.all')), ['admin']);
  // admin-only caps
  for (const cap of ['settings', 'secrets', 'users', 'audit', 'finetune', 'host', 'chat.import', 'models.manage', 'usage.all']) {
    assert.deepEqual(ROLES.filter((r) => has(r, cap)), ['admin'], cap);
  }
});

test('normMe: the API example, the admin token and an older daemon', () => {
  const m = normMe({
    kind: 'login', role: 'user', home: '#/chat', has_users: true, bootstrap: false,
    user: { id: 'u-3f9a1c2b7d01', name: 'ana', display: 'Ana', role: 'user', must_change: false, created: '2026-11-01T00:00:00Z', last_login: '2026-11-03T08:00:00Z' },
    caps: ['chat', 'chat.tools', 'sessions', 'flows', 'models.read', 'usage', 'gateway', 'voice', 'devices'],
    budget: { monthly_usd: 5, monthly_tokens: 0, spent_usd: 1.25, spent_tokens: 48211 },
    api_key: { set: true, hint: '9f2c', made: '2026-11-02T00:00:00Z' }, device: '',
  });
  assert.equal(m.role, 'user');
  assert.equal(m.user.name, 'ana');
  assert.equal(whoText(m), 'Ana');
  assert.equal(initialOf(m), 'A');
  assert.equal(m.budget.spentUSD, 1.25);
  assert.equal(m.apiKey.hint, '9f2c');
  assert.equal(m.mustChange, false);
  assert.ok(can(m, 'sessions') && !can(m, 'settings'));

  const tok = normMe({ kind: 'admin-token', user: null, role: 'admin', caps: ALL_CAPS, home: '#/chat', has_users: false, bootstrap: true });
  assert.equal(tok.bootstrap, true);
  assert.equal(whoText(tok), 'Admin token');
  assert.equal(initialOf(tok), '');

  // No caps in the reply: the role table.
  assert.deepEqual(normMe({ role: 'viewer' }).caps, ROLE_CAPS.viewer);
  // No /api/me at all: the admin token as before users.
  assert.equal(normMe(null), LEGACY_ME);
  assert.equal(LEGACY_ME.role, 'admin');
  assert.ok(can(LEGACY_ME, 'settings'));
  // A must-change sign-in.
  assert.equal(meFor('user', { user: { id: 'u-1', name: 'ana', role: 'user', must_change: true } }).mustChange, true);
  // Bad home falls back to the role's.
  assert.equal(normMe({ role: 'monitor', home: 'javascript:x' }).home, '#/monitor');
});

test('canWriteSessions: admin and user steer; viewer, monitor and chat do not', () => {
  assert.deepEqual(ROLES.filter((r) => canWriteSessions(ME[r])), ['admin', 'user']);
  assert.equal(canWriteSessions(null), false);
  assert.equal(roleHome(ME.viewer), '#/sessions');
  assert.equal(roleHome(null), '#/chat');
});

// ---------- tabs per role ----------

const TABS = ['chat', 'sessions', 'flows', 'models', 'monitor', 'usage', 'settings'];
const visible = (me) => TABS.filter((t) => tabVisible(t, me));

test('tabs per role', () => {
  assert.deepEqual(visible(ME.admin), TABS);
  assert.deepEqual(visible(ME.user), ['chat', 'sessions', 'flows', 'models', 'usage']);
  assert.deepEqual(visible(ME.viewer), ['sessions', 'monitor']);
  assert.deepEqual(visible(ME.monitor), ['monitor']);
  assert.deepEqual(visible(ME.chat), ['chat']);
  for (const r of ROLES) assert.equal(tabVisible('more', ME[r]), true, r);
});

test('every tab in index.html has a rule, and the admin sees it', () => {
  const html = readFileSync(new URL('../dist/index.html', import.meta.url), 'utf8');
  const tabs = [...html.matchAll(/data-tab="([^"]+)"/g)].map((m) => m[1]);
  assert.ok(tabs.length >= 8);
  for (const t of tabs) {
    assert.ok(t === 'more' || VIEW_CAPS[t], `no rule for tab ${t}`);
    assert.equal(tabVisible(t, ME.admin), true, t);
  }
});

test('the More sheet holds what the role opens, then Account', () => {
  const keys = (me) => moreItems(me).map(([k]) => k);
  assert.deepEqual(keys(ME.admin), ['flows', 'models', 'usage', 'settings', 'account']);
  assert.deepEqual(keys(ME.user), ['flows', 'models', 'usage', 'account']);
  assert.deepEqual(keys(ME.viewer), ['account']);
  assert.deepEqual(keys(ME.monitor), ['account']);
  assert.deepEqual(keys(ME.chat), ['account']);
  assert.equal(TAB_OF.account, 'account');
});

// ---------- the route guard ----------

test('viewAllowed per role', () => {
  const ok = (view, me, extra = {}) => viewAllowed({ view, ...extra }, me);
  assert.ok(ok('session', ME.viewer) && ok('wall', ME.viewer) && ok('diff', ME.viewer));
  assert.ok(!ok('chat', ME.viewer));
  assert.ok(!ok('flows', ME.viewer) && !ok('flowedit', ME.viewer));
  assert.ok(!ok('monitor', ME.user));
  assert.ok(ok('monitor', ME.monitor) && !ok('sessions', ME.monitor) && !ok('chat', ME.monitor));
  assert.ok(ok('chat', ME.chat) && !ok('sessions', ME.chat) && !ok('models', ME.chat));
  assert.ok(ok('settings', ME.admin, { group: 'users' }));
  assert.ok(!ok('settings', ME.user, { group: 'users' }));
  for (const r of ROLES) {
    for (const v of ['account', 'notfound', 'login']) assert.ok(ok(v, ME[r]), `${r} ${v}`);
  }
  assert.ok(!ok('nope', ME.admin));
});

test('guardRoute: bootstrap, must-change, home, not found', () => {
  const g = (hash, me) => guardRoute(parseRoute(hash), me, hash);
  // Signed out: Login (Login itself is always shown).
  assert.deepEqual(g('#/chat', null), { redirect: '#/login' });
  assert.deepEqual(g('#/login', null), { route: { view: 'login' } });
  // Signed in with a password: Login sends them home; a token holder may
  // still sign in with a password.
  assert.deepEqual(g('#/login', ME.viewer), { redirect: '#/sessions' });
  assert.deepEqual(g('#/login', ME.admin), { redirect: '#/chat' });
  // The admin token with no accounts: the first-run screen, whatever was asked.
  const boot = normMe({ kind: 'admin-token', role: 'admin', caps: ALL_CAPS, has_users: false, bootstrap: true });
  assert.deepEqual(g('#/settings', boot), { redirect: '#/first-admin' });
  assert.deepEqual(g('#/first-admin', boot), { route: { view: 'first-admin' } });
  assert.deepEqual(g('#/login', boot), { route: { view: 'login' } });
  assert.deepEqual(g('#/first-admin', ME.admin), { redirect: '#/chat' });
  // Must change: only the password screen.
  const must = meFor('user', { user: { id: 'u-1', name: 'ana', role: 'user', must_change: true } });
  assert.deepEqual(g('#/sessions', must), { redirect: '#/password' });
  assert.deepEqual(g('#/account', must), { redirect: '#/password' });
  assert.deepEqual(g('#/password', must), { route: { view: 'password' } });
  assert.deepEqual(g('#/password', ME.user), { redirect: '#/account' });
  // Empty hash: the role's home.
  assert.deepEqual(g('', ME.viewer), { redirect: '#/sessions' });
  assert.deepEqual(g('', ME.monitor), { redirect: '#/monitor' });
  assert.deepEqual(g('#/', ME.chat), { redirect: '#/chat' });
  // A view the role cannot open is Not found (it does not say it exists).
  assert.deepEqual(g('#/chat', ME.viewer), { route: { view: 'notfound', path: '#/chat' } });
  assert.deepEqual(g('#/settings/users', ME.user), { route: { view: 'notfound', path: '#/settings/users' } });
  assert.deepEqual(g('#/monitor', ME.chat), { route: { view: 'notfound', path: '#/monitor' } });
  assert.deepEqual(g('#/nope/deeper', ME.admin), { route: { view: 'notfound', path: '#/nope/deeper' } });
  // Allowed views pass through.
  assert.deepEqual(g('#/settings/users', ME.admin), { route: { view: 'settings', group: 'users' } });
  assert.deepEqual(g('#/s/abc', ME.viewer), { route: { view: 'session', id: 'abc' } });
  assert.deepEqual(g('#/account', ME.monitor), { route: { view: 'account' } });
});

test('bare screens: no tabs, no events', () => {
  assert.ok(isBare('login') && isBare('first-admin') && isBare('password'));
  assert.ok(!isBare('account') && !isBare('notfound') && !isBare('chat'));
});

test('parseRoute: the new views', () => {
  assert.deepEqual(parseRoute(''), { view: 'home' });
  assert.deepEqual(parseRoute('#'), { view: 'home' });
  assert.deepEqual(parseRoute('#/'), { view: 'home' });
  assert.deepEqual(parseRoute('#/account'), { view: 'account' });
  assert.deepEqual(parseRoute('#/first-admin'), { view: 'first-admin' });
  assert.deepEqual(parseRoute('#/password'), { view: 'password' });
  assert.deepEqual(parseRoute('#/settings/users'), { view: 'settings', group: 'users' });
  assert.deepEqual(parseRoute('#/nope'), { view: 'notfound', path: '#/nope' });
  assert.deepEqual(parseRoute('#/a%20b/c'), { view: 'notfound', path: '#/a b/c' });
  // Existing routes are unchanged.
  assert.deepEqual(parseRoute('#/login'), { view: 'login' });
  assert.deepEqual(parseRoute('#/chat'), { view: 'chat' });
});

// ---------- forms ----------

test('user names follow the server rules', () => {
  assert.equal(cleanName('  Ana '), 'ana');
  assert.equal(nameError('ana'), '');
  assert.equal(nameError('Ana.B-2_x'), '');
  assert.equal(nameError('9lives'), '');
  assert.match(nameError(''), /Enter/);
  assert.match(nameError('.ana'), /start/);
  assert.match(nameError('ana b'), /Lowercase/);
  assert.match(nameError('ána'), /Lowercase/);
  assert.match(nameError('a'.repeat(33)), /32/);
  assert.equal(nameError('a'.repeat(32)), '');
});

test('passwords: 10 to 256 bytes, not the name, typed twice', () => {
  assert.match(passwordError(''), /Enter/);
  assert.match(passwordError('short'), /10/);
  assert.equal(passwordError('long enough!'), '');
  // Bytes, not characters: five 2-byte letters are 10 bytes.
  assert.equal(passwordError('ééééé'), '');
  assert.match(passwordError('x'.repeat(257)), /256/);
  assert.match(passwordError('AnaBanana1', 'anabanana1'), /user name/);
  assert.equal(newPasswordError('correct horse', 'correct horse'), '');
  assert.match(newPasswordError('correct horse', 'correct house'), /match/);
  assert.match(newPasswordError('short', 'short'), /10/);
  assert.equal(displayError('Ana'), '');
  assert.match(displayError('x'.repeat(61)), /60/);
});

// ---------- sign-in errors ----------

test('login error mapping: 401, 429 with minutes, 403, 400, offline', () => {
  const generic = 'Wrong user name or password.';
  assert.equal(loginErrorText(401, { error: generic, code: 'login' }), generic);
  // A disabled account and an unknown name read the same (the server sends 401 for all).
  assert.equal(loginErrorText(401, null), generic);
  assert.equal(loginErrorText(429, { code: 'locked', retry_after: 900 }), 'Too many attempts. Try again in 15 minutes.');
  assert.equal(loginErrorText(429, { code: 'locked', retry_after: 60 }), 'Too many attempts. Try again in 1 minute.');
  assert.equal(loginErrorText(429, { code: 'locked', retry_after: 61 }), 'Too many attempts. Try again in 2 minutes.');
  assert.equal(loginErrorText(429, null, '120'), 'Too many attempts. Try again in 2 minutes.');
  assert.equal(loginErrorText(429, {}), 'Too many attempts. Try again in 15 minutes.');
  assert.equal(loginErrorText(403, { code: 'must_change' }), 'Change your password first.');
  assert.match(loginErrorText(403, { error: 'cross-site' }), /refused/);
  assert.match(loginErrorText(400, {}), /user name and password/);
  assert.match(loginErrorText(0, null), /reach/);
  assert.match(loginErrorText(500, { error: 'boom' }), /boom/);
  assert.equal(tokenErrorText(401, { code: 'login' }), 'That token is not valid.');
  assert.match(tokenErrorText(429, { retry_after: 600 }), /10 minutes/);
  assert.equal(lockMinutes(0), 1);
  assert.equal(lockMinutes(899), 15);
});

test('password change error mapping', () => {
  assert.match(passwordChangeErrorText(403, { code: 'login' }), /current password is wrong/);
  assert.match(passwordChangeErrorText(400, { code: 'password', error: 'At least 10 bytes.' }), /At least 10/);
  assert.match(passwordChangeErrorText(400, { code: 'no_user' }), /admin token has no password/);
  assert.match(passwordChangeErrorText(429, { code: 'locked', retry_after: 300 }), /5 minutes/);
});

// ---------- the Users panel ----------

const ROWS = normUsers({
  users: [
    { id: 'u-b', name: 'bea', role: 'viewer', last_login: '2026-11-02T00:00:00Z', sessions: 0, chats: 0 },
    { id: 'u-a', name: 'ana', display: 'Ana', role: 'user', budget: { monthly_usd: 5, monthly_tokens: 0 }, spent_usd: 1.25, spent_tokens: 48211, sessions: 12, chats: 30, flows: 2, devices: 1, repos: [{ path: '/r/topo', accept: true }], agents: [], api_key: { set: true, hint: '9f2c' } },
    { id: 'u-m', name: 'miguel', role: 'admin', legacy_owner: true },
    { id: 'u-x', name: 'old', role: 'admin', disabled: true },
  ],
  roles: ['admin', 'user', 'viewer', 'monitor', 'chat', 'bogus'],
  agents: ['cerberus', 'claude', 'opencode'],
});

test('normUsers sorts admins first, then by name; keeps known roles', () => {
  assert.deepEqual(ROWS.users.map((u) => u.name), ['miguel', 'old', 'ana', 'bea']);
  assert.deepEqual(ROWS.roles, ROLES);
  assert.deepEqual(ROWS.agents, ['cerberus', 'claude', 'opencode']);
  const ana = ROWS.users.find((u) => u.name === 'ana');
  assert.equal(ana.budget.monthlyUSD, 5);
  assert.deepEqual(ana.repos, [{ path: '/r/topo', accept: true }]);
  assert.equal(ana.apiKey.hint, '9f2c');
  assert.equal(ROWS.users[0].legacyOwner, true);
  assert.equal(normUser({ id: 'x', role: 'root' }).role, 'user');
  assert.deepEqual(normUsers(null).users, []);
});

test('userStatus: disabled, locked, must change, active', () => {
  assert.deepEqual(userStatus({ disabled: true, locked: true, mustChange: true }), { key: 'disabled', label: 'Disabled', tone: 'muted' });
  assert.equal(userStatus({ locked: true, mustChange: true }).label, 'Locked');
  assert.equal(userStatus({ mustChange: true }).label, 'Must change password');
  assert.equal(userStatus({}).label, 'Active');
});

test('spend text, tokens and budgets', () => {
  assert.equal(usd(1.255), 'US$1.25');
  assert.equal(fmtTokens(950), '950');
  assert.equal(fmtTokens(48211), '48k');
  assert.equal(fmtTokens(4821), '4.8k');
  assert.equal(fmtTokens(1000), '1k');
  assert.equal(fmtTokens(1300000), '1.3M');
  assert.equal(spendText(1.25, 48211, { monthlyUSD: 5, monthlyTokens: 0 }), 'US$1.25 of US$5.00 · 48k tokens');
  assert.equal(spendText(0, 0, {}), 'US$0.00 · 0 tokens');
  assert.equal(spendText(2, 5000, { monthlyUSD: 0, monthlyTokens: 100000 }), 'US$2.00 · 5k of 100k tokens');
  assert.equal(budgetShare(4, 0, { monthlyUSD: 5 }), 0.8);
  assert.equal(budgetShare(1, 90000, { monthlyUSD: 5, monthlyTokens: 100000 }), 0.9);
  assert.equal(budgetShare(9, 9, {}), -1);
  assert.equal(budgetText({ monthlyUSD: 0, monthlyTokens: 0 }), 'No cap');
  assert.equal(budgetText({ monthlyUSD: 10, monthlyTokens: 2000000 }), 'US$10.00 a month · 2M tokens a month');
});

test('parseBudget validates the budget form', () => {
  assert.deepEqual(parseBudget('', ''), { budget: { monthly_usd: 0, monthly_tokens: 0 } });
  assert.deepEqual(parseBudget('US$7.5', '1,000,000'), { budget: { monthly_usd: 7.5, monthly_tokens: 1000000 } });
  assert.deepEqual(parseBudget('$3.333', '0'), { budget: { monthly_usd: 3.33, monthly_tokens: 0 } });
  assert.match(parseBudget('-1', '').error, /US\$/);
  assert.match(parseBudget('abc', '').error, /US\$/);
  assert.match(parseBudget('1', '1.5').error, /whole/);
});

test('last-admin and self guards', () => {
  const [miguel, old, ana] = ROWS.users;
  assert.equal(enabledAdmins(ROWS.users), 1);
  assert.match(lastAdminReason(miguel, ROWS.users), /last admin/);
  assert.equal(lastAdminReason(old, ROWS.users), ''); // disabled already
  assert.equal(lastAdminReason(ana, ROWS.users), '');
  const two = [...ROWS.users, normUser({ id: 'u-z', name: 'zoe', role: 'admin' })];
  assert.equal(lastAdminReason(miguel, two), '');
  const meMiguel = normMe({ kind: 'login', role: 'admin', caps: ALL_CAPS, has_users: true, user: { id: 'u-m', name: 'miguel', role: 'admin' } });
  assert.match(selfReason(miguel, meMiguel), /yourself/);
  assert.equal(selfReason(ana, meMiguel), '');
  assert.equal(selfReason(miguel, LEGACY_ME), '');
});

test('delete summary and the toast after it', () => {
  const ana = ROWS.users.find((u) => u.name === 'ana');
  assert.equal(deleteSummary(ana), 'Moves to you: 12 sessions, 30 chats and 2 flows. Nothing is deleted. Any of their sessions still running stop. Their flows do not run until you open and save each one. Their paired device is signed out.');
  assert.equal(deleteSummary({ sessions: 1, chats: 0, flows: 0, devices: 0 }, 'zoe'), 'Moves to zoe: 1 session. Nothing is deleted. Any of their sessions still running stop.');
  assert.equal(deleteSummary({ sessions: 0, chats: 1, flows: 1, devices: 0 }), 'Moves to you: 1 chat and 1 flow. Nothing is deleted. Their flow does not run until you open and save it.');
  assert.equal(deleteSummary({ sessions: 0, chats: 0, flows: 0, devices: 2 }), 'They have no sessions, chats or flows. Nothing is deleted. Their 2 paired devices are signed out.');
  assert.equal(deletedText('ana', { moved: { sessions: 12, chats: 30, flows: 2, runs: 7, inbox: 4 } }),
    'Deleted ana. 12 sessions, 30 chats, 2 flows, 7 flow runs and 4 notifications moved to you.');
  assert.equal(deletedText('bea', { moved: {} }), 'Deleted bea.');
  assert.equal(cutText(2), ' 2 live connections ended.');
  assert.equal(cutText(0), '');
  assert.equal(listText(['a']), 'a');
  assert.equal(listText(['a', 'b']), 'a and b');
});

test('agents and repos for a confined user', () => {
  assert.ok(isCLIAgent('claude') && isCLIAgent('OpenCode') && !isCLIAgent('cerberus'));
  assert.deepEqual(agentChoices(['claude']).map((a) => [a.name, a.cli, a.builtin]), [['cerberus', false, true], ['claude', true, false]]);
  const rc = repoChoices([{ path: '/r/topo' }, { path: '/r/atrium' }], [{ path: '/r/topo', accept: true }, { path: '/r/gone', accept: false }]);
  assert.deepEqual(rc.map((r) => [r.name, r.shared, r.accept]), [['atrium', false, false], ['gone', true, false], ['topo', true, true]]);
});

// ---------- owners on shared lists ----------

test('owner chips and the owner filter', () => {
  const sessions = [
    { ID: 's1', Owner: 'u-viewer0000000', OwnerName: 'viewer' },
    { ID: 's2', Owner: 'u-a', OwnerName: 'ana' },
    { ID: 's3', Owner: 'u-a', OwnerName: 'ana' },
    { ID: 's4' },
  ];
  assert.deepEqual(ownerOf({ owner: 'u-a', owner_name: 'ana' }), { id: 'u-a', name: 'ana' });
  // Own items carry no chip; others do; none before users exist.
  assert.equal(ownerLabel(sessions[0], ME.viewer), '');
  assert.equal(ownerLabel(sessions[1], ME.viewer), 'ana');
  assert.equal(ownerLabel(sessions[3], ME.viewer), '');
  assert.equal(ownerLabel(sessions[1], LEGACY_ME), '');
  assert.deepEqual(ownerChoices(sessions), [{ id: 'u-a', name: 'ana' }, { id: 'u-viewer0000000', name: 'viewer' }]);
  assert.deepEqual(ownedBy(sessions, 'u-a').map((s) => s.ID), ['s2', 's3']);
  assert.equal(ownedBy(sessions, '').length, 4);
  assert.equal(isOwn({ owner: 'u-a' }, ME.admin), false);
  assert.equal(isOwn({ owner: ME.admin.user.id }, ME.admin), true);
  assert.equal(isOwn({}, ME.admin), true);
  assert.equal(isOwn({ owner: 'u-a' }, LEGACY_ME), true);
});

// ---------- audit, devices, doctor ----------

test('the audit user filter', () => {
  assert.deepEqual(auditUserOptions([{ name: 'ana', display: 'Ana' }, { name: 'bea', display: '' }]),
    [['', 'Any user'], ['-', 'Admin token or system'], ['ana', 'ana (Ana)'], ['bea', 'bea']]);
  const q = new URL(auditQuery({ rangeKey: 'all', user: 'ana', now: 0 }), 'http://x').searchParams;
  assert.equal(q.get('user'), 'ana');
  const none = new URL(auditQuery({ rangeKey: 'all', user: '-', now: 0 }), 'http://x').searchParams;
  assert.equal(none.get('user'), '-');
  assert.equal(new URL(auditQuery({ rangeKey: 'all', now: 0 }), 'http://x').searchParams.has('user'), false);
  assert.equal(normAudit({ actor: 'user:ana', user: 'ana' }).user, 'ana');
  assert.equal(normAudit({ actor: 'owner:admin' }).user, '');
});

test('devices carry their user', () => {
  const d = normDevice({ id: 'd1', name: 'phone', user: 'u-a', user_name: 'ana' });
  assert.equal(d.user, 'u-a');
  assert.equal(d.userName, 'ana');
  assert.equal(normDevice({ id: 'd2' }).user, '');
});

test('doctor counts for viewers and monitors', () => {
  const raw = { counts: { ok: 14, warn: 2, fail: 0 }, checks: [] };
  assert.deepEqual(doctorCountsOf(raw, normDoctor(raw)), { ok: 14, warn: 2, fail: 0, total: 16 });
  const full = { counts: { ok: 9, warn: 9, fail: 9 }, checks: [{ id: 'a', status: 'ok', title: 'A' }, { id: 'b', status: 'fail', title: 'B' }] };
  // Admins: the checks themselves are the truth.
  assert.deepEqual(doctorCountsOf(full, normDoctor(full)), { ok: 1, warn: 0, fail: 1, total: 2 });
  assert.deepEqual(doctorCountsOf([], []), { ok: 0, warn: 0, fail: 0, total: 0 });
});

// ---------- signed-in browsers ----------

test('signed-in browsers: this one first, then by last use', () => {
  const list = normLogins({ logins: [
    { id: 'l-1', last_seen: '2026-11-01T00:00:00Z', agent: 'Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0 Safari/537.36' },
    { id: 'l-2', last_seen: '2026-11-03T00:00:00Z', agent: 'Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 Version/18.0 Mobile Safari/604.1' },
    { id: 'l-3', last_seen: '2026-10-01T00:00:00Z', current: true, agent: 'Mozilla/5.0 (X11; Linux x86_64; rv:132.0) Gecko/20100101 Firefox/132.0' },
    { last_seen: 'x' },
  ] });
  assert.deepEqual(list.map((l) => l.id), ['l-3', 'l-2', 'l-1']);
  assert.equal(browserName(list[0].agent), 'Firefox on Linux');
  assert.equal(browserName(list[1].agent), 'Safari on iOS');
  assert.equal(browserName(list[2].agent), 'Chrome on Linux');
  assert.equal(browserName(''), 'Unknown browser');
  assert.equal(browserName('curl/8.9.1'), 'curl');
});
