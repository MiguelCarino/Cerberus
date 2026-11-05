// Activity feed: a collapsible side panel on desktop (≥ 900 px) and a
// drawer on smaller screens, with kind filters. It loads GET /api/feed and
// then follows GET /api/feed/stream, but only while open and visible (the
// stream is a LiveSource, so it closes when the tab is hidden). Clicking an
// item opens its session.

import { api, LiveSource } from '../api.js';
import { h, clear } from '../dom.js';
import { FEED_KINDS, feedTone, mergeFeed, filterFeed } from '../control.js';
import { relTime, debounce } from '../util.js';

const KEY_OPEN = 'cerberus.feed.open';
const KEY_KINDS = 'cerberus.feed.kinds';
const wide = matchMedia('(min-width: 900px)');

let panel = null;
let listEl = null;
let chipsEl = null;
let stream = null;
let feed = [];
let open = false;
const kinds = new Set(load(KEY_KINDS, []));
const buttons = new Set();
const CLOCK_MS = 30000; // "just now" / "3 min ago" refresh while the panel is open and visible
let clock = null;

function load(key, dflt) {
  try { const v = JSON.parse(localStorage.getItem(key)); return v ?? dflt; } catch { return dflt; }
}
function save(key, v) {
  try { localStorage.setItem(key, JSON.stringify(v)); } catch { /* private mode */ }
}

function build() {
  listEl = h('ol', { class: 'feed-list', 'aria-live': 'polite' });
  chipsEl = h('div', { class: 'feed-chips', role: 'group', 'aria-label': 'Filter by kind' });
  panel = h('aside', { class: 'feed-panel', 'aria-label': 'Activity feed', hidden: true },
    h('div', { class: 'feed-head' },
      h('h2', { class: 'group-title', text: 'Activity' }),
      h('button', { class: 'modal-x', type: 'button', 'aria-label': 'Close activity feed', onclick: () => toggleFeed(false) }, '✕')),
    chipsEl, listEl);
  const scrim = h('div', { class: 'feed-scrim', onclick: () => toggleFeed(false) });
  document.body.append(scrim, panel);
  renderChips();
}

function renderChips() {
  clear(chipsEl).append(
    h('button', { class: 'fchip', type: 'button', 'aria-pressed': String(!kinds.size), onclick: () => { kinds.clear(); changed(); } }, 'All'),
    ...FEED_KINDS.map((k) => h('button', {
      class: 'fchip', type: 'button', 'aria-pressed': String(kinds.has(k)),
      onclick: () => { if (kinds.has(k)) kinds.delete(k); else kinds.add(k); changed(); },
    }, k)));
}

function changed() {
  save(KEY_KINDS, [...kinds]);
  renderChips();
  renderList();
}

const renderList = () => {
  if (!listEl) return;
  const items = filterFeed(feed, kinds);
  clear(listEl);
  if (!items.length) {
    listEl.append(h('li', { class: 'feed-empty', text: feed.length ? 'Nothing of these kinds yet.' : 'No activity yet.' }));
    return;
  }
  for (const f of items.slice(0, 150)) {
    listEl.append(h('li', {},
      h('a', {
        class: `feed-item tone-${feedTone(f.Kind)}`, href: f.SessionID ? `#/s/${encodeURIComponent(f.SessionID)}` : '#/sessions',
        onclick: () => { if (!wide.matches) toggleFeed(false); },
      },
      h('div', { class: 'feed-top' },
        h('span', { class: `feed-kind tone-${feedTone(f.Kind)}`, text: f.Kind }),
        h('span', { class: 'feed-name', text: f.Name || f.SessionID || '' }),
        h('time', { class: 'feed-time', datetime: f.Time, title: f.Time, text: relTime(f.Time) })),
      f.Text ? h('div', { class: 'feed-text', text: f.Text }) : null)));
  }
};
const renderSoon = debounce(renderList, 200);

async function backfill() {
  try {
    const out = await api('/api/feed?limit=200');
    feed = mergeFeed(feed, Array.isArray(out) ? out : (out && out.items) || []);
    renderList();
  } catch { /* the stream still fills it */ }
}

function onItem(data) {
  let f;
  try { f = JSON.parse(data); } catch { return; }
  feed = mergeFeed(feed, Array.isArray(f) ? f : [f]);
  renderSoon();
}

function startStream() {
  if (stream) return;
  // Each (re)connect backfills, so nothing missed while hidden is lost.
  stream = new LiveSource('/api/feed/stream', { open: backfill, feed: onItem, message: onItem }).start();
}

function stopStream() {
  if (stream) { stream.stop(); stream = null; }
}

// refreshTimes rewrites the relative times in place (no list rebuild).
function refreshTimes() {
  if (!listEl) return;
  for (const t of listEl.querySelectorAll('time.feed-time')) t.textContent = relTime(t.getAttribute('datetime'));
}

// syncClock runs the slow time refresh only while the panel is open and
// the tab is visible: no timer otherwise.
function syncClock() {
  const want = open && document.visibilityState === 'visible';
  if (want && !clock) {
    refreshTimes();
    clock = setInterval(refreshTimes, CLOCK_MS);
  } else if (!want && clock) {
    clearInterval(clock);
    clock = null;
  }
}
document.addEventListener('visibilitychange', syncClock);

function syncButtons() {
  for (const b of buttons) {
    if (!b.isConnected) { buttons.delete(b); continue; }
    b.setAttribute('aria-pressed', String(open));
    b.classList.toggle('on', open);
  }
}

// toggleFeed opens or closes the feed (force = true/false, or flip).
export function toggleFeed(force) {
  if (!panel) build();
  open = force ?? !open;
  panel.hidden = !open;
  document.body.classList.toggle('feed-open', open);
  if (wide.matches) save(KEY_OPEN, open);
  if (open) { renderList(); startStream(); } else stopStream();
  syncClock();
  syncButtons();
}

// feedButton is the "Activity" toggle placed in a view's head.
export function feedButton() {
  const b = h('button', { class: 'btn btn-big feed-btn', type: 'button', 'aria-pressed': String(open), onclick: () => toggleFeed() }, 'Activity');
  b.classList.toggle('on', open);
  buttons.add(b);
  return b;
}

// initFeed restores the desktop panel when it was left open.
export function initFeed() {
  if (wide.matches && load(KEY_OPEN, false)) toggleFeed(true);
  // Growing past 900 px turns the drawer into the docked panel; shrinking
  // below closes it so it doesn't cover the page.
  wide.addEventListener('change', () => { if (!wide.matches && open) toggleFeed(false); });
}
