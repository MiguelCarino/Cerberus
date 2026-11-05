// Notification centre (day phase 6): the bell in the navbar's actions block
// with the unread badge, and its list: a panel under the bar on desktop, a
// bottom sheet on phones. Items come from GET /api/inbox and then from the
// existing /api/events stream ("inbox" events, see store.js), so there is no
// extra connection and no polling. Tapping an item opens its in-app route
// and marks it read; "Mark all read" marks everything. A flow approval that
// still waits (unread "flow-approve" item) has Approve / Reject in place.
//
// Desktop notifications are opt-in: the permission is only requested from a
// click on the toggle, and a notification is only shown while the tab is
// hidden. Only then does the event stream stay open while hidden (one idle
// connection); otherwise it closes as every stream does.

import { api, apiResult, post } from '../api.js';
import { h, clear, toast, errText } from '../dom.js';
import { store } from '../store.js';
import { snapshotLoader, approvalWaits } from '../util.js';
import { permCard } from './chatperm.js';
import { approvalButtons, approvalText } from './approval.js';
import { sessionName } from '../control.js';
import { canWriteSessions } from '../users.js';
import {
  normInbox, applyEvent, markRead, groupByDay, clockTime, safeRoute, badgeText, shouldNotify,
  keepStreamWhenHidden, levelTone, flowApprovalRun,
} from '../inbox.js';

const KEY_DESKTOP = 'cerberus.inbox.desktop';
const LIMIT = 200;
const SVG = 'http://www.w3.org/2000/svg';

let state = { items: [], unread: 0 };
let btn = null;
let badge = null;
let panel = null;
let scrim = null;
let listEl = null;
let allBtn = null;
let deskBtn = null;
let isOpen = false;
const decided = new Map(); // inbox item id → "Approved" | "Rejected" (this page's answers)

const hasNotifications = () => typeof window.Notification === 'function';
const permission = () => (hasNotifications() ? Notification.permission : 'unsupported');

function optedIn() {
  try { return localStorage.getItem(KEY_DESKTOP) === '1'; } catch { return false; }
}
function setOptedIn(on) {
  try { localStorage.setItem(KEY_DESKTOP, on ? '1' : '0'); } catch { /* private mode */ }
}

// bellIcon is a plain bell outline (a UI glyph, not a brand mark), built
// with DOM calls.
function bellIcon() {
  const svg = document.createElementNS(SVG, 'svg');
  for (const [k, v] of Object.entries({ viewBox: '0 0 24 24', fill: 'none', stroke: 'currentColor', 'stroke-width': '2',
    'stroke-linecap': 'round', 'stroke-linejoin': 'round', 'aria-hidden': 'true' })) svg.setAttribute(k, v);
  for (const d of ['M6 8a6 6 0 0 1 12 0c0 7 3 9 3 9H3s3-2 3-9', 'M10.3 21a1.94 1.94 0 0 0 3.4 0']) {
    const p = document.createElementNS(SVG, 'path');
    p.setAttribute('d', d);
    svg.append(p);
  }
  return svg;
}

// initBell adds the bell to the navbar actions block and follows the inbox.
export function initBell() {
  const actions = document.querySelector('[data-carino-actions]');
  if (!actions || btn) return;
  badge = h('span', { class: 'bell-badge', hidden: true, 'aria-hidden': 'true' });
  // data-priority="1": the navbar keeps it in the bar when the tabs fold.
  btn = h('button', {
    class: 'icon-btn bell-btn', type: 'button', 'data-priority': '1', 'aria-haspopup': 'dialog',
    'aria-expanded': 'false', 'aria-controls': 'bellPanel', title: 'Notifications', onclick: (e) => { e.stopPropagation(); toggle(); },
  }, bellIcon(), badge);
  actions.append(btn);
  renderBadge();

  store.setKeepHidden(() => keepStreamWhenHidden(optedIn(), permission()));
  store.subscribe((kind, payload) => {
    if (kind === 'events-open' || (kind === 'resync' && payload && payload.what === 'inbox')) load();
    else if (kind === 'inbox') onEvent(payload);
    else if (kind === 'asks') onAsks(payload);
    else if (kind === 'sessions' || kind === 'session' || kind === 'me') onSessions();
  });
  onSessions();
  // A tap on a desktop notification shown by the service worker.
  navigator.serviceWorker?.addEventListener?.('message', (e) => {
    if (e.data && e.data.type === 'route') go(e.data.link);
  });
  document.addEventListener('keydown', (e) => { if (e.key === 'Escape' && isOpen) { toggle(false); btn.focus(); } });
  document.addEventListener('click', (e) => {
    if (isOpen && panel && !panel.contains(e.target) && !btn.contains(e.target)) toggle(false);
  });
}

// The inbox snapshot (GET /api/inbox) on every event-stream (re)connect,
// so nothing missed while hidden or offline is lost. Events that arrive
// while it is in flight are re-applied on top of it.
const NO_INBOX = Symbol('no inbox');
const inbox = snapshotLoader({
  async fetch() {
    const res = await apiResult(`/api/inbox?limit=${LIMIT}`);
    // A daemon without the inbox (older, or not wired): no bell.
    if (res.status === 404 || res.status === 503) return NO_INBOX;
    if (!res.ok) throw new Error(res.message);
    return res.body;
  },
  onSnapshot(body, events) {
    if (body === NO_INBOX) { btn.hidden = true; return; }
    btn.hidden = false;
    let next = normInbox(body);
    for (const ev of events) next = applyEvent(next, ev, LIMIT);
    state = next;
    renderBadge();
    if (isOpen) renderList();
  },
  onError(e) { if (isOpen) toast(errText(e), 'err'); },
});

const load = () => inbox.load();

function onEvent(ev) {
  inbox.track(ev);
  const before = new Set(state.items.map((i) => i.ID));
  state = applyEvent(state, ev, LIMIT);
  renderBadge();
  if (isOpen) renderList();
  if (ev && ev.Type === 'added' && ev.Item && !before.has(ev.Item.ID)) notify(ev.Item);
}

function renderBadge() {
  if (!btn) return;
  const n = asks.length + sessWaits.length;
  const t = badgeText(state.unread + n);
  badge.textContent = t;
  badge.hidden = !t;
  badge.classList.toggle('ask', n > 0);
  const parts = [];
  if (n) parts.push(`${n} ${n === 1 ? 'approval' : 'approvals'} waiting`);
  if (state.unread) parts.push(`${state.unread} unread`);
  btn.setAttribute('aria-label', parts.length ? `Notifications, ${parts.join(', ')}` : 'Notifications');
}

// ---------- open chat permission requests ----------
// Every chat's open cards (store.asks(), live from /api/events), answered
// here with the chat's own card: the same arming delay, two presses for a
// command, Allow for this session, Deny with a note.
let asks = [];
const cards = new Map(); // ask key → permCard (kept across re-renders so arming and presses survive)
const answeredAt = new Map(); // ask key → when this page answered it
const ANSWER_SHOW_MS = 4000; // an answered card's line stays this long
const sending = new Set(); // ask keys whose answer this page has in flight (the stream can say "gone" first)

function onAsks(list) {
  asks = store.can('chat') ? (list || []) : [];
  const open = new Set(asks.map((a) => a.key));
  for (const [k, c] of cards) if (!open.has(k) && !sending.has(k) && (c.isOpen() || !isOpen)) { cards.delete(k); answeredAt.delete(k); }
  renderBadge();
  if (isOpen) renderList();
}

function askCard(a) {
  let c = cards.get(a.key);
  if (c) return c;
  c = permCard(a.req, async (decision, note, option) => {
    sending.add(a.key);
    try {
      const body = { req: a.req.id, decision, note };
      if (decision === 'always') body.option = option;
      const res = await apiResult(`/api/chats/${encodeURIComponent(a.chat)}/answer`, { method: 'POST', body });
      if (res.ok) {
        const rule = res.body && res.body.rule;
        const shown = decision === 'always' ? 'always' : rule ? 'session' : decision === 'session' ? 'once' : decision;
        c.answered({ decision: shown, by: 'user', note, rule });
        answeredAt.set(a.key, Date.now());
        setTimeout(() => { if (isOpen) renderList(); }, ANSWER_SHOW_MS + 50);
        return true;
      }
      if (res.status === 409 || res.status === 404) {
        cards.delete(a.key);
        toast('That request was already answered.', 'warn');
        store.refreshAsks();
        if (isOpen) renderList();
        return true;
      }
      toast(res.message, 'err');
    } catch (e) {
      toast(errText(e), 'err');
    } finally {
      sending.delete(a.key);
    }
    return false;
  });
  cards.set(a.key, c);
  return c;
}

// ---------- agent sessions waiting at an approval prompt ----------
// From the session list in the store (live from /api/events): the built-in
// agent's y / a / n prompt, answered with Yes / Always / No.
let sessWaits = [];
let sessSig = '';
const sessBtns = new Map(); // "id/seq" → buttons (kept so their arming survives re-renders)

function onSessions() {
  // A viewer sees every session but answers none of them.
  const next = canWriteSessions(store.me()) ? approvalWaits(store.sessions()) : [];
  const sig = next.map((x) => x.ID + '/' + x.AskSeq + '/' + (x.WaitingAsk || '')).join(' ');
  if (sig === sessSig) return;
  sessSig = sig;
  sessWaits = next;
  const live = new Set(next.map((x) => x.ID + '/' + x.AskSeq));
  for (const k of sessBtns.keys()) if (!live.has(k)) sessBtns.delete(k);
  renderBadge();
  if (isOpen) renderList();
}

function sessionAsk(x) {
  const k = x.ID + '/' + x.AskSeq;
  if (!sessBtns.has(k)) sessBtns.set(k, approvalButtons(x));
  const route = '#/s/' + encodeURIComponent(x.ID);
  return h('div', { class: 'bell-ask bell-sess' },
    h('a', { class: 'bell-ask-chat', href: route, onclick: (e) => { e.preventDefault(); toggle(false); go(route); } },
      h('span', { class: 'bell-title', text: sessionName(x) }), h('span', { class: 'bell-time', text: x.Agent || 'session' })),
    h('div', { class: 'bell-sess-ask', text: approvalText(x) }),
    sessBtns.get(k));
}

function askSection() {
  const shown = asks;
  // A card answered here stays (collapsed to its line) for a few seconds.
  const live = new Set(asks.map((a) => a.key));
  const now = Date.now();
  for (const [k, c] of cards) {
    if (!live.has(k) && !sending.has(k) && !c.isOpen() && !(now - (answeredAt.get(k) || 0) < ANSWER_SHOW_MS)) { cards.delete(k); answeredAt.delete(k); }
  }
  // Answered here (or being answered): shown below the open ones.
  const done = [...cards.entries()].filter(([k, c]) => !live.has(k) && (!c.isOpen() || sending.has(k)));
  const total = shown.length + sessWaits.length;
  if (!total && !done.length) return null;
  const sec = h('section', { class: 'bell-asks', 'aria-label': 'Permission requests' },
    h('div', { class: 'bell-day', role: 'presentation', text: total ? `Waiting for you · ${total}` : 'Answered' }));
  for (const x of sessWaits) sec.append(sessionAsk(x));
  for (const a of shown) {
    const route = `#/chat/${encodeURIComponent(a.chat)}`;
    sec.append(h('div', { class: 'bell-ask' },
      h('a', { class: 'bell-ask-chat', href: route, onclick: (e) => { e.preventDefault(); toggle(false); go(route); } },
        h('span', { class: 'bell-title', text: a.title }), a.model ? h('span', { class: 'bell-time', text: a.model }) : null),
      askCard(a).el));
  }
  for (const [k, c] of done) {
    sec.append(h('div', { class: 'bell-ask' }, c.el));
  }
  return sec;
}

function build() {
  listEl = h('div', { class: 'bell-list', role: 'list' });
  allBtn = h('button', { class: 'btn btn-sm', type: 'button', onclick: markAll }, 'Mark all read');
  deskBtn = h('button', { class: 'btn btn-sm btn-ghost bell-desk', type: 'button', onclick: toggleDesktop });
  panel = h('aside', { class: 'bell-panel', id: 'bellPanel', role: 'dialog', 'aria-label': 'Notifications', hidden: true },
    h('div', { class: 'bell-grip', 'aria-hidden': 'true' }),
    h('div', { class: 'bell-head' },
      h('h2', { class: 'group-title', text: 'Notifications' }),
      allBtn,
      h('button', { class: 'modal-x', type: 'button', 'aria-label': 'Close notifications', onclick: () => toggle(false) }, '✕')),
    listEl,
    hasNotifications() ? h('div', { class: 'bell-foot' }, deskBtn,
      h('span', { class: 'bell-note', text: 'Only while this tab is hidden.' })) : null);
  scrim = h('div', { class: 'bell-scrim', hidden: true, onclick: () => toggle(false) });
  document.body.append(scrim, panel);
}

// toggle opens or closes the list (force = true/false, or flip).
export function toggle(force) {
  if (!panel) build();
  isOpen = force ?? !isOpen;
  panel.hidden = !isOpen;
  scrim.hidden = !isOpen;
  btn.setAttribute('aria-expanded', String(isOpen));
  document.body.classList.toggle('bell-open', isOpen);
  if (isOpen) {
    renderList();
    load();
    setTimeout(() => panel.querySelector('.bell-item, .bell-head .btn')?.focus({ preventScroll: true }), 0);
  }
}

function renderList() {
  allBtn.disabled = state.unread <= 0;
  renderDesktop();
  // A card's note being typed keeps its focus across the re-render (the
  // card element is moved, not rebuilt).
  const act = listEl.contains(document.activeElement) && document.activeElement.closest('.bell-ask') ? document.activeElement : null;
  clear(listEl);
  const sec = askSection();
  if (sec) listEl.append(sec);
  if (act && act.isConnected) act.focus({ preventScroll: true });
  if (!state.items.length) {
    if (sec) return;
    listEl.append(h('p', { class: 'bell-empty', text: 'Nothing yet. Finished, waiting and failed sessions, chats, budgets, alerts and downloads show up here.' }));
    return;
  }
  for (const g of groupByDay(state.items)) {
    listEl.append(h('div', { class: 'bell-day', role: 'presentation', text: g.label }));
    for (const it of g.items) listEl.append(itemRow(it));
  }
}

function itemRow(it) {
  const route = safeRoute(it.Link);
  const tone = levelTone(it.Level);
  const body = [
    h('span', { class: 'bell-top' },
      it.Read ? null : h('span', { class: 'bell-dot', 'aria-label': 'unread' }),
      h('span', { class: 'bell-title', text: it.Title || it.Kind || 'Notification' }),
      h('time', { class: 'bell-time', datetime: it.Time, title: it.Time, text: clockTime(it.Time) })),
    it.Body ? h('span', { class: 'bell-body', text: it.Body }) : null,
    it.Kind ? h('span', { class: `bell-kind tone-${tone}`, text: it.Kind }) : null,
  ];
  const main = route
    ? h('a', { class: 'bell-item', href: route, onclick: (e) => { e.preventDefault(); open(it); } }, body)
    : h('button', { class: 'bell-item', type: 'button', onclick: () => open(it) }, body);
  // An approval still waiting: the daemon marks it read once decided.
  const run = store.can('flows') ? flowApprovalRun(it) : '';
  const answer = run && (!it.Read || decided.has(it.ID)) ? approvalRow(it, run) : null;
  return h('div', { class: `bell-row tone-${tone}${it.Read ? '' : ' unread'}`, role: 'listitem' },
    answer ? h('div', { class: 'bell-stack' }, main, answer) : main,
    h('button', { class: 'bell-del', type: 'button', 'aria-label': `Delete “${it.Title || 'notification'}”`, onclick: () => remove(it) }, '✕'));
}

// approvalRow is Approve / Reject under a flow approval item. A run that
// was already decided (here or elsewhere) answers 409, said in a toast.
function approvalRow(it, run) {
  if (decided.has(it.ID)) return h('div', { class: 'bell-answer' }, h('span', { class: 'bell-note', text: decided.get(it.ID) }));
  const answer = async (ok, b) => {
    b.disabled = true;
    try {
      const res = await apiResult(`/api/flows/runs/${encodeURIComponent(run)}/approve`, { method: 'POST', body: { ok, note: '' } });
      if (res.ok) {
        decided.set(it.ID, ok ? 'Approved' : 'Rejected');
        toast(ok ? 'Approved: the run goes on' : 'Rejected: the run stops', ok ? 'ok' : 'warn');
        if (!it.Read) {
          state = markRead(state, [it.ID]);
          renderBadge();
          post('/api/inbox/read', { ids: [it.ID] }).catch(() => {});
        }
      } else if (res.status === 409 || res.status === 404) {
        decided.set(it.ID, 'Already answered');
        toast('This run is no longer waiting for an answer.', 'warn');
      } else {
        toast(res.message, 'err');
      }
    } catch (e) {
      toast(errText(e), 'err');
    }
    if (isOpen) renderList();
  };
  return h('div', { class: 'bell-answer' },
    h('button', { class: 'btn btn-sm btn-primary', type: 'button', onclick: (e) => answer(true, e.currentTarget) }, 'Approve'),
    h('button', { class: 'btn btn-sm', type: 'button', onclick: (e) => answer(false, e.currentTarget) }, 'Reject'));
}

// open marks the item read and follows its route.
function open(it) {
  if (!it.Read) {
    state = markRead(state, [it.ID]);
    renderBadge();
    post('/api/inbox/read', { ids: [it.ID] }).catch(() => {});
  }
  const route = safeRoute(it.Link);
  toggle(false);
  if (route) go(route);
}

function go(link) {
  const route = safeRoute(link);
  if (route && location.hash !== route) location.hash = route;
}

async function markAll() {
  state = markRead(state, [], true);
  renderBadge();
  renderList();
  try { await post('/api/inbox/read', { all: true }); } catch (e) { toast(errText(e), 'err'); load(); }
}

async function remove(it) {
  state = applyEvent(state, { Type: 'deleted', IDs: [it.ID] });
  renderBadge();
  renderList();
  try { await api(`/api/inbox/${encodeURIComponent(it.ID)}`, { method: 'DELETE' }); } catch (e) { toast(errText(e), 'err'); load(); }
}

function renderDesktop() {
  if (!deskBtn) return;
  const on = optedIn() && permission() === 'granted';
  deskBtn.textContent = on ? 'Desktop alerts: on' : 'Desktop alerts: off';
  deskBtn.setAttribute('aria-pressed', String(on));
  deskBtn.title = permission() === 'denied' ? 'Blocked in the browser settings for this site' : '';
}

// toggleDesktop asks for the permission (only ever from this click).
async function toggleDesktop() {
  if (optedIn() && permission() === 'granted') {
    setOptedIn(false);
  } else {
    let p = permission();
    if (p === 'default') {
      try { p = await Notification.requestPermission(); } catch { p = 'denied'; }
    }
    if (p === 'granted') {
      setOptedIn(true);
      toast('Desktop alerts on while this tab is hidden', 'ok');
    } else {
      setOptedIn(false);
      toast('The browser blocks notifications for this site', 'warn');
    }
  }
  renderDesktop();
}

// notify shows a desktop notification for a new item, when allowed.
async function notify(item) {
  if (!shouldNotify({ optedIn: optedIn(), permission: permission(), hidden: document.hidden, item })) return;
  const opts = { body: item.Body || '', tag: item.ID, icon: 'assets/icons/icon-192.png', data: { link: safeRoute(item.Link) || '' } };
  try {
    // Phones only show notifications through the service worker.
    const reg = navigator.serviceWorker && (await navigator.serviceWorker.getRegistration());
    if (reg && reg.active) { await reg.showNotification(item.Title || 'Cerberus', opts); return; }
    const n = new Notification(item.Title || 'Cerberus', opts);
    n.onclick = () => { window.focus(); go(item.Link); n.close(); };
  } catch { /* not allowed here */ }
}
