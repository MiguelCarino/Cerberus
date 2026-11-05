// The account button in the navbar's actions block (users and roles): the
// person's initial (a person icon for the admin token or a device) and, on
// desktop, their name. It opens a small menu with Account and Sign out.
// Like the bell, it joins the actions block before carino-navbar.js moves
// the block into the bar. Phones also reach Account from the More sheet.
//
// hardGo and signOut live here so the sign-in screens share them: a sign
// in or out reloads the page, so nothing of the previous person stays in
// the page's memory (sessions, chats, drafts).

import { apiResult, stopAllLive } from '../api.js';
import { h, clear } from '../dom.js';
import { store } from '../store.js';
import { whoText, initialOf, ROLE_LABEL } from '../users.js';

const SVG = 'http://www.w3.org/2000/svg';

// hardGo opens hash on a fresh page load (at the app's root, without a
// ?token= or the /login path; a ?theme= stays).
export function hardGo(hash) {
  const u = new URL(location.href);
  const theme = u.searchParams.get('theme');
  u.search = theme ? `?theme=${encodeURIComponent(theme)}` : '';
  u.pathname = u.pathname.replace(/login\/?$/, '');
  u.hash = hash;
  history.replaceState(null, '', u.href);
  location.reload();
}

// signOut ends this browser's sign-in and shows Login.
export async function signOut() {
  // Streams first: the logout cuts them on the server, and their error
  // check would probe /api/status with the revoked cookie (a 401).
  store.stopEvents();
  stopAllLive();
  try { await apiResult('/api/logout', { method: 'POST', body: {}, quiet: true, noAuth: true }); } catch { /* signed out anyway */ }
  store.clearMe();
  hardGo('#/login');
}

function personIcon() {
  const svg = document.createElementNS(SVG, 'svg');
  for (const [k, v] of Object.entries({ viewBox: '0 0 24 24', fill: 'none', stroke: 'currentColor', 'stroke-width': '2',
    'stroke-linecap': 'round', 'stroke-linejoin': 'round', 'aria-hidden': 'true' })) svg.setAttribute(k, v);
  for (const d of ['M20 21a8 8 0 0 0-16 0', 'M12 13a5 5 0 1 0 0-10 5 5 0 0 0 0 10z']) {
    const p = document.createElementNS(SVG, 'path');
    p.setAttribute('d', d);
    svg.append(p);
  }
  return svg;
}

let btn = null;
let menu = null;
let isOpen = false;

// initAccount adds the button to the navbar actions block.
export function initAccount() {
  const actions = document.querySelector('[data-carino-actions]');
  if (!actions || btn) return;
  btn = h('button', {
    class: 'icon-btn acct-btn', type: 'button', 'data-priority': '1', 'aria-haspopup': 'menu', 'aria-expanded': 'false',
    'aria-controls': 'acctMenu', title: 'Account', hidden: true, onclick: (e) => { e.stopPropagation(); toggle(); },
  });
  actions.append(btn);
  render();
  store.subscribe((kind) => { if (kind === 'me') render(); });
  document.addEventListener('keydown', (e) => { if (e.key === 'Escape' && isOpen) { toggle(false); btn.focus(); } });
  document.addEventListener('click', (e) => { if (isOpen && menu && !menu.contains(e.target) && !btn.contains(e.target)) toggle(false); });
  window.addEventListener('hashchange', () => { if (isOpen) toggle(false); });
}

function render() {
  if (!btn) return;
  const me = store.me();
  btn.hidden = !me;
  if (!me) return;
  const letter = initialOf(me);
  const who = whoText(me);
  clear(btn).append(
    letter ? h('span', { class: 'acct-initial', 'aria-hidden': 'true', text: letter }) : personIcon(),
    h('span', { class: 'acct-name', text: who }));
  btn.setAttribute('aria-label', `Account: ${who}`);
  if (isOpen) buildMenu();
}

function buildMenu() {
  const me = store.me();
  if (!menu) {
    menu = h('div', { class: 'acct-menu', id: 'acctMenu', role: 'menu', hidden: true });
    document.body.append(menu);
  }
  clear(menu).append(
    h('div', { class: 'acct-menu-head' },
      h('span', { class: 'acct-menu-name', text: whoText(me) }),
      h('span', { class: 'acct-menu-role', text: me && me.user ? `${me.user.name} · ${ROLE_LABEL[me.role] || me.role}` : (ROLE_LABEL[me && me.role] || '') })),
    h('a', { class: 'acct-menu-item', role: 'menuitem', href: '#/account', onclick: () => toggle(false) }, 'Account'),
    me && !me.legacy ? h('button', { class: 'acct-menu-item', role: 'menuitem', type: 'button', onclick: () => { toggle(false); signOut(); } }, 'Sign out') : null);
}

function toggle(force) {
  isOpen = force ?? !isOpen;
  if (isOpen) buildMenu();
  if (menu) menu.hidden = !isOpen;
  btn.setAttribute('aria-expanded', String(isOpen));
  if (isOpen) setTimeout(() => menu.querySelector('.acct-menu-item')?.focus({ preventScroll: true }), 0);
}
