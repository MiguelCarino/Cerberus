// Wall view (#/wall, #/wall/a,b,c): a grid of live mini terminals, one per
// session, fed by ONE multiplexed EventSource (/api/wall/stream?ids=...).
// Tiles are read-only until focused; a focused tile fills the wall and gets
// the session's input bar (terminput.js). The grid auto-fits 1/2/4/6/9; on
// phones it is one tile per screen with native scroll-snap swiping and dots.
// Tiles kept on the wall stay even after their session finishes.

import { previewChip } from '../preview.js';
import { Terminal } from '../../vendor/xterm/xterm.mjs';
import { FitAddon } from '../../vendor/addon-fit/addon-fit.mjs';
import { LiveSource } from '../api.js';
import { h, clear, openDialog, pill } from '../dom.js';
import { store } from '../store.js';
import { b64ToBytes, debounce, stateTone, isLive, isClosed, fmtEnergy } from '../util.js';
import {
  sessionName, gridFit, wallIds, wallStreamURL, parseWallEvent, miniTermSize, snapIndex, fmtElapsed,
  moveCursor, byListOrder, WALL_MAX,
} from '../control.js';
import { TERM_THEME, TERM_FONT, termInput } from './terminput.js';
import { canWriteSessions, ownerLabel } from '../users.js';
import { feedButton } from './feed.js';
import { setShortcutTarget } from '../shortcuts.js';
import { deleteSessions, trashIcon } from './ctl.js';

const KEY_KEPT = 'cerberus.wall.kept';
const phone = matchMedia('(max-width: 640px)');

function loadKept() {
  try { const v = JSON.parse(localStorage.getItem(KEY_KEPT)); return Array.isArray(v) ? v.filter((x) => typeof x === 'string') : []; } catch { return []; }
}
function saveKept(ids) {
  try { localStorage.setItem(KEY_KEPT, JSON.stringify(ids)); } catch { /* private mode */ }
}

// mount shows the wall. On its own (#/wall/a,b,c) it is a full-screen
// watch page for a chosen set. With opts.embedded the Sessions view hosts
// it above its list: no head, hidden while nothing is live, and no keyboard
// shortcuts of its own (the list keeps them; Sessions passes Escape on
// through the returned unmount's .unfocus()). opts.strip draws it as one
// short row of small tiles (the List layout); setStrip switches it.
export function mount(root, { ids: routeIds = [], embedded = false, strip = false } = {}) {
  // Users and roles: a viewer watches the tiles (no input, no delete).
  const me = store.me();
  const readOnly = !canWriteSessions(me);
  let kept = loadKept();
  let ids = [];
  let focusId = null;
  let cursor = -1;
  let gone = false;
  let fontsReady = false;
  const tiles = new Map();

  const sub = h('p', { class: 'view-sub' });
  const grid = h('div', { class: 'wall-grid' });
  const dots = h('div', { class: 'wall-dots', role: 'tablist', 'aria-label': 'Tiles' });
  const empty = h('div', { class: 'empty wall-empty', hidden: true },
    h('p', { class: 'empty-title', text: 'Nothing to watch' }),
    h('p', { class: 't-sec', text: 'Active sessions show up in Sessions live. Choose sessions to watch them here, finished ones with their last output.' }),
    h('button', { class: 'btn btn-primary', type: 'button', onclick: () => choose() }, 'Choose sessions'));
  const head = embedded ? null : h('div', { class: 'view-head wall-head' },
    h('div', {}, h('h1', { class: 'view-title', text: 'Wall' }), sub),
    h('div', { class: 'head-actions' },
      feedButton(),
      h('a', { class: 'btn btn-big', href: '#/wall', title: 'Sessions with the live tiles of every active session' }, 'All active'),
      h('button', { class: 'btn btn-big', type: 'button', onclick: () => choose() }, 'Choose…')));
  const view = h('section', {
    class: `view-wall${embedded ? ' wall-embed' : ' view'}${strip ? ' wall-strip' : ''}`,
    'aria-label': embedded ? 'Live terminals' : null,
  }, head, embedded ? null : empty, grid, dots);
  root.append(view);

  // Embedded and focused: the tile fills the app (the list steps aside).
  const markFocus = () => {
    if (embedded) document.body.toggleAttribute('data-wall-focus', !!focusId);
  };

  // ---------- tiles ----------
  function makeTile(id) {
    const t = { id, info: store.session(id), fresh: true, term: null, fit: null, ti: null, opened: false };
    t.name = h('span', { class: 'tile-name' });
    t.state = h('span', { class: 'tile-state' });
    t.meta = h('span', { class: 'tile-meta' });
    t.elapsed = h('span', { class: 'tile-elapsed', title: 'Since created' });
    t.energy = h('span', { class: 'tile-energy', title: 'Energy' });
    t.preview = h('a', { class: 'pv-chip', target: '_blank', rel: 'noopener', title: 'Open the preview in a new window', hidden: true, onclick: (e) => e.stopPropagation() });
    t.keep = h('button', { class: 'icon-btn tile-keep', type: 'button', onclick: (e) => { e.stopPropagation(); toggleKeep(id); } }, 'Keep');
    t.back = h('button', { class: 'btn btn-sm tile-back', type: 'button', onclick: (e) => { e.stopPropagation(); setFocus(null); } }, embedded ? '← Sessions' : '← Wall');
    t.box = h('div', { class: 'tile-term', onclick: () => { if (focusId !== id) setFocus(id); } });
    t.foot = h('div', { class: 'tile-foot' });
    t.input = h('div', { class: 'tile-input', hidden: true });
    t.el = h('article', { class: 'tile', 'data-id': id, tabindex: '-1', 'aria-label': 'Session tile' },
      h('header', { class: 'tile-head', onclick: () => { if (focusId !== id) setFocus(id); } },
        t.back,
        h('div', { class: 'tile-title' }, t.name, t.state),
        h('div', { class: 'tile-facts' }, t.meta, t.elapsed, t.energy),
        h('div', { class: 'tile-tools' }, t.preview, t.keep,
          readOnly ? null : h('button', { class: 'icon-btn tile-del', type: 'button', title: 'Delete the session', 'aria-label': 'Delete the session',
            onclick: (e) => { e.stopPropagation(); deleteSessions([id]); } }, trashIcon()),
          h('a', { class: 'icon-btn', href: `#/s/${encodeURIComponent(id)}`, title: 'Open the session', 'aria-label': 'Open the session', onclick: (e) => e.stopPropagation() }, '↗'))),
      t.box, t.foot, t.input);

    t.term = new Terminal({
      fontFamily: TERM_FONT, fontSize: 9, scrollback: 1000, cursorBlink: false, disableStdin: true,
      theme: TERM_THEME, allowProposedApi: false, cols: (t.info && t.info.Cols) || 100, rows: 24,
    });
    t.fit = new FitAddon();
    t.term.loadAddon(t.fit);
    t.term.onData((d) => { if (!readOnly && focusId === id && t.info && isLive(t.info.State) && t.ti) t.ti.send(d); });
    if (fontsReady) openTerm(t);
    renderTile(t);
    return t;
  }

  function openTerm(t) {
    if (t.opened) return;
    t.opened = true;
    t.term.open(t.box);
    layoutTile(t);
  }

  function layoutTile(t) {
    if (!t.opened || !t.box.clientWidth || !t.box.clientHeight) return;
    const focused = focusId === t.id;
    // A focused tile on a phone keeps a readable font and scrolls sideways
    // instead of shrinking the PTY's full width into 390 px.
    const min = focused && phone.matches ? 8 : 5;
    const size = miniTermSize(t.box.clientWidth, t.box.clientHeight, (t.info && t.info.Cols) || 100, { min, max: focused ? 15 : 12 });
    if (t.term.options.fontSize !== size.fontSize) t.term.options.fontSize = size.fontSize;
    let rows = size.rows;
    try { rows = t.fit.proposeDimensions()?.rows || rows; } catch { /* not measured yet */ }
    if (t.term.cols !== size.cols || t.term.rows !== rows) t.term.resize(size.cols, Math.max(4, rows));
  }

  function renderTile(t) {
    const s = t.info;
    if (!s) { t.name.textContent = t.id; return; }
    const tone = stateTone(s.State);
    t.el.className = `tile tone-${tone}${focusId === t.id ? ' is-focus' : ''}${ids[cursor] === t.id ? ' cursor' : ''}`;
    t.name.textContent = sessionName(s);
    t.name.title = sessionName(s);
    clear(t.state).append(s.State === 'waiting' ? h('span', { class: 'badge-wait', text: 'Waiting' }) : pill(s.State, tone));
    const owner = ownerLabel(s, me);
    t.meta.textContent = (owner ? owner + ' · ' : '') + s.Agent + (s.Model ? ` · ${s.Model}` : '');
    t.elapsed.textContent = fmtElapsed(s.Created);
    const pv = previewChip(s);
    t.preview.hidden = !pv;
    if (pv) { t.preview.href = pv.url; t.preview.textContent = `${pv.label} ↗`; }
    const e = fmtEnergy(+s.EnergyJ || 0);
    t.energy.textContent = e === '–' ? '' : e;
    const isKept = kept.includes(t.id);
    t.keep.classList.toggle('on', isKept);
    t.keep.setAttribute('aria-pressed', String(isKept));
    t.keep.title = isKept ? 'Kept on the wall (tap to let it go when it finishes)' : 'Keep on the wall even after it finishes';
    const reason = s.State === 'waiting' && s.WaitingReason ? `Waiting: ${s.WaitingReason}` : '';
    t.foot.textContent = reason || s.Activity || (s.Error ? s.Error : '–');
    t.foot.classList.toggle('warn', !!reason);
    t.ti?.sync();
  }

  function toggleKeep(id) {
    kept = kept.includes(id) ? kept.filter((x) => x !== id) : [...kept, id].slice(-WALL_MAX);
    saveKept(kept);
    const t = tiles.get(id);
    if (t) renderTile(t);
    syncIds();
  }

  // ---------- which tiles ----------
  function syncIds() {
    if (gone) return;
    // Forget kept tiles whose session is gone for good.
    const known = kept.filter((id) => store.session(id) || !store.loaded());
    if (known.length !== kept.length) { kept = known; saveKept(kept); }
    const next = wallIds(store.sessions(), { routeIds, kept });
    if (focusId && !next.includes(focusId) && tiles.has(focusId)) next.push(focusId); // never yank the focused tile
    const changed = next.join(',') !== ids.join(',');
    ids = next;
    for (const [id, t] of tiles) {
      if (!ids.includes(id)) { t.term.dispose(); t.el.remove(); tiles.delete(id); }
    }
    for (const id of ids) if (!tiles.has(id)) tiles.set(id, makeTile(id));
    // Re-insert only when the order changed: moving a node blurs its terminal.
    const els = ids.map((id) => tiles.get(id).el);
    if (els.length !== grid.children.length || els.some((el, i) => grid.children[i] !== el)) grid.replaceChildren(...els);
    const auto = !routeIds.length;
    view.hidden = embedded && !ids.length;
    empty.hidden = ids.length > 0 || !store.loaded();
    sub.textContent = !ids.length ? (store.loaded() ? 'Nothing chosen to watch' : 'Loading…')
      : `${ids.length} session${ids.length === 1 ? '' : 's'}${!auto ? ' · chosen' : kept.length ? ' · kept and active' : ' · active'}`;
    if (cursor >= ids.length) cursor = ids.length - 1;
    relayout();
    renderDots();
    if (changed) restartStream();
  }
  const syncSoon = debounce(syncIds, 300);

  function relayout() {
    const w = grid.clientWidth || view.clientWidth || 1;
    const hgt = grid.clientHeight || 1;
    const fit = gridFit(ids.length, { maxCols: w < 900 ? 2 : 3, aspect: w / hgt });
    grid.style.setProperty('--cols', String(fit.cols || 1));
    grid.style.setProperty('--rows', String(fit.rows || 1));
    view.classList.toggle('focused', !!focusId);
    markFocus();
    requestAnimationFrame(() => { for (const t of tiles.values()) layoutTile(t); });
  }
  const relayoutSoon = debounce(relayout, 120);
  const ro = new ResizeObserver(() => relayoutSoon());
  ro.observe(grid);

  // ---------- phone swipe dots ----------
  let dotIdx = 0;
  function renderDots() {
    clear(dots);
    dots.hidden = ids.length < 2 || !!focusId;
    ids.forEach((id, i) => dots.append(h('button', {
      class: `wall-dot${i === dotIdx ? ' on' : ''}`, type: 'button', role: 'tab', 'aria-selected': String(i === dotIdx),
      'aria-label': sessionName(store.session(id)) || `Tile ${i + 1}`,
      onclick: () => grid.scrollTo({ left: i * grid.clientWidth, behavior: 'smooth' }),
    })));
  }
  let scrollRaf = 0;
  grid.addEventListener('scroll', () => {
    if (!phone.matches || scrollRaf) return;
    scrollRaf = requestAnimationFrame(() => {
      scrollRaf = 0;
      const i = snapIndex(grid.scrollLeft, grid.clientWidth, ids.length);
      if (i === dotIdx) return;
      dotIdx = i;
      dots.querySelectorAll('.wall-dot').forEach((d, j) => { d.classList.toggle('on', j === i); d.setAttribute('aria-selected', String(j === i)); });
    });
  }, { passive: true });

  // ---------- focus ----------
  function setFocus(id) {
    const prev = focusId ? tiles.get(focusId) : null;
    focusId = id && tiles.has(id) ? id : null;
    if (prev) {
      prev.term.options.disableStdin = true;
      prev.input.hidden = true;
      prev.term.blur();
      renderTile(prev);
    }
    const t = focusId ? tiles.get(focusId) : null;
    if (t) {
      if (readOnly) {
        if (!t.input.childElementCount) t.input.append(h('div', { class: 'ss-readonly' }, h('span', { class: 'ro-label', text: 'Read only' })));
      } else if (!t.ti) {
        t.ti = termInput(t.id, { getInfo: () => t.info, getTerm: () => t.term });
        t.input.append(t.ti.keys, t.ti.micNote, t.ti.bar);
      }
      t.input.hidden = false;
      t.term.options.disableStdin = readOnly;
      renderTile(t);
      // No focus on phones: it would pop the keyboard over the terminal.
      if (!phone.matches) setTimeout(() => t.term.focus(), 0);
    }
    relayout();
    renderDots();
    if (!t && prev && phone.matches) {
      const i = ids.indexOf(prev.id);
      requestAnimationFrame(() => grid.scrollTo({ left: i * grid.clientWidth }));
    }
  }

  // ---------- the one stream ----------
  let stream = null;
  function restartStream() {
    if (stream) { stream.stop(); stream = null; }
    if (!ids.length) return;
    const write = (data) => {
      const ev = parseWallEvent(data);
      const t = ev && tiles.get(ev.id);
      if (!t) return;
      if (t.fresh) { t.term.reset(); t.fresh = false; }
      t.term.write(b64ToBytes(ev.b64));
    };
    // Each (re)connect replays a backlog per session: start every tile clean.
    stream = new LiveSource(wallStreamURL(ids), {
      open: () => { for (const t of tiles.values()) t.fresh = true; },
      out: write,
      backlog: write,
    }).start();
  }

  // ---------- choose ----------
  async function choose() {
    const list = store.sessions().filter((s) => !isClosed(s.State)).sort(byListOrder);
    const picked = await openDialog((close) => {
      const boxes = list.map((s) => {
        const cb = h('input', { type: 'checkbox', value: s.ID });
        cb.checked = ids.includes(s.ID);
        return h('label', { class: 'check choose-row' }, cb, h('span', { class: 'choose-name', text: sessionName(s) }), pill(s.State, stateTone(s.State)));
      });
      const done = () => close(boxes.map((b) => b.querySelector('input')).filter((c) => c.checked).map((c) => c.value));
      return {
        title: 'Choose sessions',
        sub: `Up to ${WALL_MAX} tiles`,
        body: list.length ? h('div', { class: 'choose-list' }, boxes) : h('p', { class: 'dlg-msg', text: 'No open sessions.' }),
        foot: [
          h('button', { class: 'btn btn-ghost', type: 'button', onclick: () => close(null) }, 'Cancel'),
          h('button', { class: 'btn', type: 'button', onclick: () => close([]) }, 'All active'),
          h('button', { class: 'btn btn-primary', type: 'button', onclick: done }, 'Show'),
        ],
      };
    });
    if (picked == null) return;
    location.hash = picked.length ? `#/wall/${picked.slice(0, WALL_MAX).map(encodeURIComponent).join(',')}` : '#/wall';
  }

  // ---------- elapsed clock (paused while hidden) ----------
  let clock = 0;
  const tick = () => { for (const t of tiles.values()) if (t.info) t.elapsed.textContent = fmtElapsed(t.info.Created); };
  const startClock = () => { if (!clock && !document.hidden) clock = setInterval(tick, 30000); };
  const stopClock = () => { clearInterval(clock); clock = 0; };
  const onVis = () => { if (document.hidden) stopClock(); else { tick(); startClock(); } };
  document.addEventListener('visibilitychange', onVis);

  // ---------- keyboard ----------
  if (!embedded) setShortcutTarget({
    down: () => { cursor = moveCursor(cursor, 1, ids.length); for (const t of tiles.values()) renderTile(t); },
    up: () => { cursor = moveCursor(cursor, -1, ids.length); for (const t of tiles.values()) renderTile(t); },
    open: () => { if (ids[cursor]) setFocus(ids[cursor]); },
    escape: () => { if (focusId) { setFocus(null); return true; } return false; },
  });

  const unsub = store.subscribe((kind, payload) => {
    if (kind === 'session') {
      const t = tiles.get(payload.ID);
      if (t) {
        const colsChanged = (t.info && t.info.Cols) !== payload.Cols;
        t.info = payload;
        renderTile(t);
        if (colsChanged) layoutTile(t);
      }
      syncSoon();
    } else if (kind === 'sessions') {
      for (const t of tiles.values()) { t.info = store.session(t.id) || t.info; renderTile(t); }
      syncIds();
    }
  });

  document.fonts.load("12px 'IBM Plex Mono'").catch(() => {}).finally(() => {
    if (gone) return;
    fontsReady = true;
    for (const t of tiles.values()) openTerm(t);
  });
  syncIds();
  startClock();
  if (!embedded) store.refreshSessions().catch(() => {});

  const unmount = () => {
    gone = true;
    unsub();
    if (!embedded) setShortcutTarget(null);
    if (embedded) document.body.removeAttribute('data-wall-focus');
    view.remove();
    if (stream) stream.stop();
    stopClock();
    document.removeEventListener('visibilitychange', onVis);
    syncSoon.cancel();
    relayoutSoon.cancel();
    ro.disconnect();
    cancelAnimationFrame(scrollRaf);
    for (const t of tiles.values()) t.term.dispose();
    tiles.clear();
  };
  // unfocus leaves a focused tile (true when there was one).
  unmount.unfocus = () => { if (!focusId) return false; setFocus(null); return true; };
  // setStrip draws the tiles as one short row (List) or a grid (Wall).
  unmount.setStrip = (on) => { view.classList.toggle('wall-strip', !!on); relayout(); };
  return unmount;
}
