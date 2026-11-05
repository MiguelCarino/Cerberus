// Cerberus web UI entry point: hash router, view tabs (moved into the shared
// Carino navbar by carino-navbar.js), the waiting badge and the 401 → Login switch.
//
// Users and roles (phase 4a): GET /api/me (store.loadMe) says who is signed
// in. The route guard (users.js guardRoute) sends a fresh admin token to the
// first-run screen, a must-change sign-in to the forced password change,
// and a view the role cannot use to Not found. Tabs and the More sheet show
// only what the role opens; the server is the authority either way.

import { setUnauthorizedHandler, setCodeHandler } from './api.js';
import { h, clear, openDialog, toast } from './dom.js';
import { store } from './store.js';
import { parseRoute, keyboardOpen } from './util.js';
import { guardRoute, tabVisible, moreItems, isBare, TAB_OF, canWriteSessions } from './users.js';
import { initShortcuts } from './shortcuts.js';
import { initFeed, toggleFeed } from './views/feed.js';
import * as sessions from './views/sessions.js';
import * as wall from './views/wall.js';
import * as session from './views/session.js';
import * as diff from './views/diff.js';
import * as models from './views/models.js';
import * as usage from './views/usage.js';
import * as secrets from './views/secrets.js';
import * as settings from './views/settings.js';
import { maybeWelcome } from './views/welcome.js';
import * as monitor from './views/monitor.js';
import * as chat from './views/chat.js';
import * as batch from './views/batch.js';
import * as login from './views/login.js';
import * as firstadmin from './views/firstadmin.js';
import * as password from './views/password.js';
import * as account from './views/account.js';
import * as notfound from './views/notfound.js';
import * as flows from './views/flows.js';
import * as flowedit from './views/flowedit.js';
import * as flowrun from './views/flowrun.js';
import { initDrop } from './views/drop.js';
import { initBridgeReceive } from './views/bridge.js';
import { initBell } from './views/bell.js';
import { initAccount } from './views/accountmenu.js';
import { initPWA } from './pwa.js';

const VIEWS = {
  sessions, session, diff, batch, wall, chat, models, monitor, usage, secrets, settings, login, flows, flowedit, flowrun,
  'first-admin': firstadmin, password, account, notfound,
};

const app = document.getElementById('app');
let unmount = null;
let routeSeq = 0;
let current = null; // the route on screen

async function route() {
  const seq = ++routeSeq;
  const r = parseRoute(location.hash);
  // The phase 3 Security view lives in Settings now; keep old links working.
  if (r.view === 'security') { location.replace('#/settings/security'); return; }
  // Secrets and About live in Settings now; keep old links working.
  if (r.view === 'secrets') { location.replace('#/settings/secrets'); return; }
  if (r.view !== 'login' && !store.me()) {
    if (!unmount) clear(app).append(h('p', { class: 't-sec app-loading', text: 'Loading…' }));
    try { await store.loadMe(); } catch { return; } // a 401 shows Login
    if (seq !== routeSeq) return;
  }
  const g = guardRoute(r, store.me(), location.hash);
  if (g.redirect && g.redirect !== location.hash) { location.replace(g.redirect); return; }
  show(g.route || { view: 'notfound', path: location.hash });
}

function show(r) {
  if (unmount) { unmount(); unmount = null; }
  current = r;
  clear(app);
  document.body.dataset.view = r.view;
  document.body.toggleAttribute('data-bare', isBare(r.view));
  applyRole();
  const tab = TAB_OF[r.view];
  for (const a of document.querySelectorAll('[data-tab]')) {
    a.toggleAttribute('aria-current', a.dataset.tab === tab || (a.dataset.tabAlso || '').split(' ').includes(tab));
  }
  if (isBare(r.view)) { store.stopEvents(); toggleFeed(false); }
  else { store.startEvents(); adminExtras(); }
  unmount = VIEWS[r.view].mount(app, r) || null;
  app.scrollTop = 0;
}

// The activity feed's docked panel and the first-run welcome are for
// admins (the welcome edits the config). Once per page load.
let extrasDone = false;
function adminExtras() {
  if (extrasDone || !store.can('settings')) return;
  extrasDone = true;
  initFeed();
  maybeWelcome();
}

// applyRole shows only the tabs this person's role opens.
function applyRole() {
  const me = store.me();
  document.body.dataset.role = me ? me.role : '';
  for (const a of document.querySelectorAll('[data-tab]')) {
    a.hidden = !!me && !tabVisible(a.dataset.tab, me);
  }
}

// Who is signed in changed (a reconnect after a role change, a sign-in in
// another tab): follow the new rules, remounting only when the view on
// screen is no longer the one to show.
store.subscribe((kind) => {
  if (kind !== 'me' || !current) return;
  applyRole();
  updateBadge();
  const g = guardRoute(parseRoute(location.hash), store.me(), location.hash);
  if (g.redirect || (g.route && g.route.view !== current.view)) route();
});

setUnauthorizedHandler(() => {
  store.clearMe();
  if (parseRoute(location.hash).view !== 'login') location.hash = '#/login';
});

// Refusals with a code: a must-change sign-in goes to the forced password
// change; a role refusal of an action says so (reads show their own error).
setCodeHandler((code, { message, method }) => {
  if (code === 'must_change') {
    if (parseRoute(location.hash).view === 'password') return;
    store.loadMe().then(() => { location.hash = '#/password'; }).catch(() => {});
  } else if (code === 'role') {
    if (method && method !== 'GET') toast(message || 'Your role cannot do this.', 'err');
    store.loadMe().catch(() => {});
  }
});

// ---------- waiting badge ----------
function updateBadge() {
  // Waiting sessions need someone who may answer them.
  const n = canWriteSessions(store.me()) ? store.waiting() : 0;
  for (const b of document.querySelectorAll('.nav-badge')) {
    b.textContent = String(n);
    b.hidden = n === 0;
  }
  document.title = n ? `(${n}) Cerberus` : 'Cerberus';
}
store.subscribe((kind) => { if (kind === 'sessions' || kind === 'session') updateBadge(); });

// ---------- phone keyboard: size the app to the visual viewport ----------
// body[data-kb] marks an open on-screen keyboard, so views with fixed parts
// (the chat's stage and header, the bottom tabs) fold them and keep the
// input in view.
if (window.visualViewport) {
  const vv = window.visualViewport;
  const editing = () => {
    const a = document.activeElement;
    return !!a && (a.isContentEditable || /^(INPUT|TEXTAREA|SELECT)$/.test(a.tagName));
  };
  const setH = () => {
    document.documentElement.style.setProperty('--app-h', `${vv.height}px`);
    document.body.toggleAttribute('data-kb', keyboardOpen(window.innerHeight, vv.height, vv.scale, editing()));
  };
  vv.addEventListener('resize', setH);
  document.addEventListener('focusout', () => setTimeout(setH, 0));
  // A keyboard closed while the page was away (or without a viewport
  // resize) must not leave the tools folded: check again on the way back.
  document.addEventListener('focusin', () => setTimeout(setH, 0));
  document.addEventListener('visibilitychange', () => { if (!document.hidden) setH(); });
  window.addEventListener('pageshow', setH);
  window.addEventListener('resize', setH);
  setH();
}

// ---------- phone "More" sheet ----------
function openMore() {
  openDialog((close) => ({
    title: 'More',
    body: h('nav', { class: 'more-list', 'aria-label': 'More views' }, moreItems(store.me()).map(([tab, label, sub]) =>
      h('a', { class: 'more-item', href: `#/${tab}`, onclick: () => close() },
        h('span', { class: 'more-label', text: label }), h('span', { class: 'more-sub', text: sub })))),
  }));
}
for (const b of document.querySelectorAll('[data-more]')) b.addEventListener('click', openMore);

window.addEventListener('hashchange', route);
initShortcuts();
initDrop();
// The bell and the account button join the navbar actions block before
// carino-navbar.js (deferred, after this module) moves the block into the bar.
initBell();
initAccount();
initPWA();
// Before routing reads the hash: a handoff arrives at "#carino-bridge".
initBridgeReceive();
if (/\/login\/?$/.test(location.pathname) && !location.hash) history.replaceState(null, '', '#/login');
updateBadge();
route();
