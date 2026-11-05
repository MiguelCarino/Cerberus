// Flow view: the read-only graph of who handed what to whom, drawn as SVG.
// The session view shows it in its Flow tab (#/s/<id>/flow) and the chat
// opens it in a wide sheet (openFlowSheet). The geometry is flowgraph.js;
// this file builds the DOM, handles pan and zoom (Pointer Events: drag
// with a mouse or one finger, pinch with two, wheel, Fit, double-click or
// double-tap on the background) and keeps the graph live from the store's
// events for the sessions in it, at most one fetch per 2 s and never
// while the page is hidden. Text is always set as text, never as markup.

import { api } from '../api.js';
import { h, clear, openDialog, errText } from '../dom.js';
import { store } from '../store.js';
import {
  layoutGraph, layoutDir, fitTransform, zoomAt, wheelFactor, pinchTransform, transformAttr,
  isDoubleTap, TAP_SLOP, fitText, FONT, isCircle, circleOf, nodeTone, nodeTitle, sessionIds,
  wantsRefresh, sameShape, textWidth, isLive, isWaiting, liveEdge,
} from '../flowgraph.js';

const NS = 'http://www.w3.org/2000/svg';
const REFRESH_MS = 2000;
let seq = 0; // unique ids for each panel's <defs>

// s builds an SVG element; like h() but in the SVG namespace, with text
// only through textContent.
function s(tag, attrs = {}, ...children) {
  const el = document.createElementNS(NS, tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (v == null || v === false) continue;
    if (k === 'text') el.textContent = v;
    else if (k === 'class') el.setAttribute('class', v);
    else el.setAttribute(k, String(v));
  }
  for (const c of children.flat()) if (c) el.append(c);
  return el;
}

// ---------- icons (24 × 24, stroked in currentColor) ----------

const ICONS = {
  you: ['M12 12.5a4.25 4.25 0 1 0 0-8.5 4.25 4.25 0 0 0 0 8.5Z', 'M4.5 20.5c.9-3.6 3.9-5.6 7.5-5.6s6.6 2 7.5 5.6'],
  session: ['M3.5 5.5h17v13h-17z', 'M7 10l3 2.5L7 15', 'M12.5 15.5H17'],
  chat: ['M4 5.5h16v10H10l-4.5 3.5v-3.5H4z', 'M8 9.5h8', 'M8 12.5h5'],
  model: ['M7 7h10v10H7z', 'M10 10h4v4h-4z', 'M10 3.5V7M14 3.5V7M10 17v3.5M14 17v3.5M3.5 10H7M3.5 14H7M17 10h3.5M17 14h3.5'],
  input: ['M4 12h10', 'M10.5 8.5 14 12l-3.5 3.5', 'M14.5 4.5h5v15h-5'],
  step: ['M5 4.5h14v15H5z', 'M8.5 9h7', 'M8.5 12h7', 'M8.5 15h4'],
  item: ['M6.5 6.5h11v11h-11z', 'M10 12h4'],
  end: ['M6 20.5v-16', 'M6 5h11l-2.5 3.5L17 12H6'],
  tool: ['M14.5 4.5a4 4 0 0 0-4.9 5.2L4 15.3 6.7 18l5.6-5.6a4 4 0 0 0 5.2-4.9l-2.4 2.4-2.3-.6-.6-2.3Z'],
};

function icon(kind, x, y, size) {
  const k = size / 24;
  return s('g', { class: 'fl-icon', transform: `translate(${x},${y}) scale(${k})`, 'aria-hidden': 'true' },
    (ICONS[kind] || ICONS.tool).map((d) => s('path', { d })));
}

// ---------- the panel ----------

// flowPanel builds the Flow panel for url (the flow route). opts.onNavigate
// (href) runs before a tap on a node changes the address (the sheet
// closes itself). Returns {el, open(), close(), refresh(), destroy()}:
// open starts the live refresh, close stops it. opts.legend picks the
// legend: 'session' (default: spawned / asked / answered) or 'run' (a
// flow run: next / else / model).
export function flowPanel(url, opts = {}) {
  const id = `fl${++seq}`;
  let graph = null; // the last reply
  let laid = null; // its layout
  let t = { x: 0, y: 0, k: 1 };
  let auto = true; // fit on resize and refresh until the person pans or zooms
  let opened = false;
  let gone = false;
  let pending = false; // a refresh was wanted while hidden or too soon
  let lastFetch = 0;
  let timer = 0;
  let loading = false;
  let ids = new Set();
  let unsub = null;
  let size = { w: 0, h: 0 };
  let expanded = false;

  const grid = s('pattern', { id: `${id}-grid`, width: 20, height: 20, patternUnits: 'userSpaceOnUse' },
    s('circle', { cx: 1, cy: 1, r: 1, class: 'fl-dot' }));
  const bg = s('rect', { class: 'fl-bg', x: 0, y: 0, width: '100%', height: '100%', fill: `url(#${id}-grid)` });
  const world = s('g', { class: 'fl-world' });
  const edgesG = s('g', { class: 'fl-edges' });
  const chipsG = s('g', { class: 'fl-chips' });
  const nodesG = s('g', { class: 'fl-nodes' });
  world.append(edgesG, nodesG, chipsG);
  const svg = s('svg', { class: 'fl-svg', role: 'group', 'aria-label': 'Flow graph: who handed what to whom' },
    s('defs', {}, grid), bg, world);

  const status = h('div', { class: 'fl-status', role: 'status', 'aria-live': 'polite' });
  const updated = h('span', { class: 'fl-updated' });
  const fitBtn = h('button', { class: 'btn btn-sm', type: 'button', title: 'Fit the graph (double-click the background)', onclick: () => fit(true) }, 'Fit');
  const zoomOut = h('button', { class: 'btn btn-sm btn-ghost', type: 'button', 'aria-label': 'Zoom out', title: 'Zoom out', onclick: () => zoomBy(1 / 1.25) }, '−');
  const zoomIn = h('button', { class: 'btn btn-sm btn-ghost', type: 'button', 'aria-label': 'Zoom in', title: 'Zoom in', onclick: () => zoomBy(1.25) }, '+');
  const legend = h('div', { class: 'fl-legend', 'aria-label': 'Legend' },
    opts.legend === 'run'
      ? [legendItem('solid', 'next / then'), legendItem('dashed', 'else / model'), legendItem('live', 'running now'), legendItem('wait', 'waiting')]
      : [legendItem('solid', 'spawned'), legendItem('dashed', 'asked'), legendItem('dotted', 'answered'), legendItem('live', 'running now')]);
  // Expand fills the window (Escape or the button again shrinks it back);
  // Full screen asks the browser for the whole screen where it can.
  const expandBtn = h('button', { class: 'btn btn-sm btn-ghost fl-expand', type: 'button', 'aria-pressed': 'false', title: 'Fill the window (Esc to go back)', onclick: () => setExpanded(!expanded) }, 'Expand');
  const canFull = typeof document !== 'undefined' && !!document.fullscreenEnabled;
  const fullBtn = canFull ? h('button', { class: 'btn btn-sm btn-ghost fl-full', type: 'button', title: 'Full screen (Esc to leave)', onclick: () => toggleFull() }, 'Full screen') : null;
  const bar = h('div', { class: 'fl-bar' }, legend, h('div', { class: 'fl-bar-end' }, updated, zoomOut, zoomIn, fitBtn, expandBtn, fullBtn));
  const stage = h('div', { class: 'fl-stage' }, svg, status);
  const el = h('div', { class: 'fl-panel' }, bar, stage);

  // ---------- drawing ----------

  function draw() {
    clear(edgesG); clear(chipsG); clear(nodesG);
    if (!laid) return;
    const live = new Set(laid.nodes.filter(isLive).map((n) => n.id));
    const held = new Set(laid.nodes.filter(isWaiting).map((n) => n.id));
    for (const e of laid.edges) {
      const on = liveEdge(e, live, held);
      edgesG.append(s('g', { class: `fl-edge fl-edge-${e.kind} fl-${e.style}${on ? ' fl-live' : ''}` },
        s('title', { text: e.title }),
        s('path', { class: 'fl-line', d: e.geo.d }),
        s('polygon', { class: 'fl-arrow', points: e.geo.arrow })));
      if (!e.text) continue;
      const { w, h: ch } = e.chip;
      chipsG.append(s('g', { class: `fl-chip fl-chip-${e.kind}`, transform: `translate(${e.geo.mid.x - w / 2},${e.geo.mid.y - ch / 2})` },
        s('title', { text: e.title }),
        s('rect', { width: w, height: ch, rx: ch / 2 }),
        s('text', { x: w / 2, y: ch / 2, 'text-anchor': 'middle', 'dominant-baseline': 'central', text: e.text })));
    }
    for (const n of laid.nodes) nodesG.append(nodeEl(n, laid.dir));
  }

  function nodeEl(n, dir) {
    const tone = nodeTone(n);
    const nav = !!n.href;
    const g = s('g', {
      class: `fl-node fl-node-${n.kind}${nav ? ' fl-nav' : ''}${isLive(n) ? ' fl-live' : ''}${isWaiting(n) ? ' fl-wait' : ''}`,
      transform: `translate(${n.x},${n.y})`,
      'data-id': n.id,
      tabindex: nav ? 0 : null,
      role: nav ? 'link' : 'img',
      'aria-label': [n.label, n.sub, n.state, n.detail].filter(Boolean).join(', '),
    }, s('title', { text: nodeTitle(n) }));
    if (isCircle(n)) {
      const c = circleOf({ ...n, x: 0, y: 0 }, dir);
      g.append(s('circle', { class: `fl-circle fl-tone-${tone}`, cx: c.cx, cy: c.cy, r: c.r }),
        icon(n.kind, c.cx - 11, c.cy - 11, 22));
      if (n.state) g.append(s('circle', { class: `fl-dotstate fl-tone-${tone}`, cx: c.cx + c.r * 0.7, cy: c.cy - c.r * 0.7, r: 5 }));
      // Text: centred below the circle (left to right) or beside it.
      const tb = dir === 'tb';
      const tx = tb ? c.r * 2 + 10 : n.w / 2;
      const maxW = tb ? n.w - tx - 4 : n.w - 8;
      const anchor = tb ? 'start' : 'middle';
      const ly = tb ? c.cy - 3 : c.r * 2 + 18;
      const lab = fitText(n.label, maxW, FONT.label);
      const sub = fitText(n.sub, maxW, FONT.sub, true);
      g.append(s('text', { class: 'fl-label', x: tx, y: ly, 'text-anchor': anchor, text: lab.text }));
      if (sub.text) g.append(s('text', { class: 'fl-sub', x: tx, y: ly + 16, 'text-anchor': anchor, text: sub.text }));
    } else {
      g.append(s('rect', { class: 'fl-card', width: n.w, height: n.h, rx: 12 }),
        s('rect', { class: 'fl-iconbox', x: 12, y: 14, width: 30, height: 30, rx: 8 }),
        icon(n.kind, 15, 17, 24));
      const tx = 52;
      const maxW = n.w - tx - 12;
      const lab = fitText(n.label, maxW, FONT.label);
      const sub = fitText(n.sub, maxW, FONT.sub, true);
      g.append(s('text', { class: 'fl-label', x: tx, y: 24, text: lab.text }));
      if (sub.text) g.append(s('text', { class: 'fl-sub', x: tx, y: 41, text: sub.text }));
      if (n.state) {
        const st = fitText(n.state.toUpperCase(), maxW - 14, FONT.pill, true);
        const pw = Math.ceil(textWidth(st.text, FONT.pill, true)) + 14;
        g.append(s('g', { class: `fl-pill fl-tone-${tone}`, transform: `translate(${tx},${50})` },
          s('rect', { width: pw, height: 16, rx: 8 }),
          s('text', { x: pw / 2, y: 8, 'text-anchor': 'middle', 'dominant-baseline': 'central', text: st.text })));
      }
    }
    // Ports: in (left or top) and out (right or bottom), in node space.
    for (const p of [n.ports.in, n.ports.out]) g.append(s('circle', { class: 'fl-port', cx: p.x - n.x, cy: p.y - n.y, r: 4 }));
    return g;
  }

  function apply() {
    world.setAttribute('transform', transformAttr(t));
    // The grid moves with the graph (scaled within reason so it stays dots).
    grid.setAttribute('patternTransform', transformAttr(t));
  }

  function measure() {
    const r = stage.getBoundingClientRect();
    size = { w: Math.round(r.width), h: Math.round(r.height) };
    return size;
  }

  function relayout() {
    if (!graph) return;
    const dir = layoutDir(size.w || measure().w);
    laid = layoutGraph(graph, dir);
    draw();
  }

  function fit(byPerson = false) {
    if (!laid) return;
    const { w, h: hh } = measure();
    t = fitTransform(laid.width, laid.height, w, hh, w < 640 ? 16 : 32, undefined, laid.dir === 'tb');
    if (byPerson) auto = true;
    apply();
  }

  // ---------- expand and full screen ----------

  function refit() {
    auto = true;
    requestAnimationFrame(() => { if (!gone) { measure(); relayout(); fit(); } });
  }

  function setExpanded(on) {
    if (on === expanded) return;
    expanded = on;
    el.classList.toggle('fl-max', on);
    document.documentElement.classList.toggle('fl-max-open', on);
    expandBtn.textContent = on ? 'Shrink' : 'Expand';
    expandBtn.setAttribute('aria-pressed', String(on));
    expandBtn.title = on ? 'Back to the page (Esc)' : 'Fill the window (Esc to go back)';
    if (on) window.addEventListener('keydown', onEsc, true);
    else window.removeEventListener('keydown', onEsc, true);
    refit();
  }

  function onEsc(e) {
    if (e.key !== 'Escape' || document.fullscreenElement) return;
    e.preventDefault(); e.stopPropagation(); // not the sheet's own close
    setExpanded(false);
    expandBtn.focus();
  }

  function toggleFull() {
    if (document.fullscreenElement === el) { document.exitFullscreen().catch(() => {}); return; }
    el.requestFullscreen().catch(() => setExpanded(true)); // refused: fill the window instead
  }

  function onFullChange() {
    const on = document.fullscreenElement === el;
    el.classList.toggle('fl-fs', on);
    if (fullBtn) fullBtn.textContent = on ? 'Exit full screen' : 'Full screen';
    refit();
  }
  if (canFull) document.addEventListener('fullscreenchange', onFullChange);

  function zoomBy(f) {
    const { w, h: hh } = measure();
    t = zoomAt(t, f, w / 2, hh / 2);
    auto = false;
    apply();
  }

  // ---------- data ----------

  function setStatus(text, kind = '') {
    status.textContent = text;
    status.className = `fl-status${kind ? ' ' + kind : ''}`;
    status.hidden = !text;
  }

  async function load() {
    if (loading) { pending = true; return; }
    loading = true;
    lastFetch = Date.now();
    pending = false;
    try {
      const g = await api(url);
      if (gone) return;
      const same = graph && sameShape(graph, g);
      graph = g;
      ids = sessionIds(g);
      relayout();
      if (!same) auto = true; // new nodes: show them all
      if (auto) fit();
      const n = (g.nodes || []).length;
      setStatus(n ? '' : 'Nothing has been handed over yet.');
      updated.textContent = g.updated ? `updated ${clock(g.updated)}` : '';
    } catch (e) {
      if (gone) return;
      if (!graph) setStatus(`Could not load the flow: ${errText(e)}`, 'err');
    } finally {
      loading = false;
      if (pending && opened) schedule();
    }
  }

  // schedule asks for a refresh: now when the last fetch is 2 s old, else
  // when it will be; nothing while hidden (visibilitychange catches up).
  function schedule() {
    pending = true;
    if (!opened || gone || timer) return;
    if (document.hidden) return;
    const wait = Math.max(0, lastFetch + REFRESH_MS - Date.now());
    timer = setTimeout(() => {
      timer = 0;
      if (!opened || gone || document.hidden) return;
      load();
    }, wait);
  }

  function onVisible() {
    if (!document.hidden && pending && opened) schedule();
  }

  // ---------- pan and zoom ----------

  const pts = new Map(); // pointerId → {x, y} in stage coordinates
  let gesture = null; // {kind: 'pan'|'pinch', ...}
  let downNode = null;
  let moved = 0;
  let lastTap = null;

  const local = (e) => {
    const r = svg.getBoundingClientRect();
    return { x: e.clientX - r.left, y: e.clientY - r.top };
  };

  function startGesture() {
    const list = [...pts.values()];
    if (list.length >= 2) {
      gesture = { kind: 'pinch', t0: { ...t }, a0: list[0], b0: list[1] };
    } else if (list.length === 1) {
      gesture = { kind: 'pan', t0: { ...t }, p0: list[0] };
    } else {
      gesture = null;
    }
  }

  svg.addEventListener('pointerdown', (e) => {
    if (e.button !== 0 && e.pointerType === 'mouse') return;
    pts.set(e.pointerId, local(e));
    if (pts.size === 1) {
      moved = 0;
      downNode = e.target.closest ? e.target.closest('.fl-node') : null;
    } else {
      downNode = null; // a second finger: no tap
    }
    try { svg.setPointerCapture(e.pointerId); } catch { /* not capturable */ }
    startGesture();
  });

  svg.addEventListener('pointermove', (e) => {
    if (!pts.has(e.pointerId) || !gesture) return;
    const p = local(e);
    pts.set(e.pointerId, p);
    if (gesture.kind === 'pan') {
      const dx = p.x - gesture.p0.x;
      const dy = p.y - gesture.p0.y;
      moved = Math.max(moved, Math.hypot(dx, dy));
      if (moved < TAP_SLOP) return;
      t = { x: gesture.t0.x + dx, y: gesture.t0.y + dy, k: gesture.t0.k };
    } else {
      const [a, b] = [...pts.values()];
      t = pinchTransform(gesture.t0, gesture.a0, gesture.b0, a, b);
      moved = Infinity;
    }
    auto = false;
    apply();
  });

  function endPointer(e, cancelled) {
    if (!pts.has(e.pointerId)) return;
    const p = local(e);
    pts.delete(e.pointerId);
    const wasTap = !cancelled && pts.size === 0 && moved < TAP_SLOP;
    if (pts.size) { startGesture(); return; }
    gesture = null;
    if (!wasTap) { lastTap = null; return; }
    if (downNode) { lastTap = null; go(downNode); return; }
    // A tap on the background: two in a row fit (touch has no dblclick).
    const tap = { t: e.timeStamp, x: p.x, y: p.y };
    if (e.pointerType !== 'mouse' && isDoubleTap(lastTap, tap)) { lastTap = null; fit(true); return; }
    lastTap = tap;
  }
  svg.addEventListener('pointerup', (e) => endPointer(e, false));
  svg.addEventListener('pointercancel', (e) => endPointer(e, true));
  svg.addEventListener('dblclick', (e) => {
    if (!(e.target.closest && e.target.closest('.fl-node'))) { e.preventDefault(); fit(true); }
  });
  svg.addEventListener('wheel', (e) => {
    e.preventDefault();
    const p = local(e);
    t = zoomAt(t, wheelFactor(e.deltaY, e.deltaMode), p.x, p.y);
    auto = false;
    apply();
  }, { passive: false });
  svg.addEventListener('keydown', (e) => {
    const node = e.target.closest ? e.target.closest('.fl-node') : null;
    if (node && (e.key === 'Enter' || e.key === ' ')) { e.preventDefault(); go(node); }
  });

  function go(nodeEl) {
    const n = laid && laid.nodes.find((x) => x.id === nodeEl.getAttribute('data-id'));
    if (!n || !n.href) return;
    if (opts.onNavigate) opts.onNavigate(n.href);
    if (location.hash !== n.href) location.hash = n.href;
  }

  // ---------- size ----------

  const ro = new ResizeObserver(() => {
    if (!opened) return;
    const before = size.w;
    const { w } = measure();
    if (!w) return;
    if (layoutDir(before) !== layoutDir(w)) { relayout(); auto = true; }
    if (auto) fit();
  });
  ro.observe(stage);

  function open() {
    if (opened || gone) return;
    opened = true;
    unsub = store.subscribe((kind, payload) => { if (wantsRefresh(kind, payload, ids)) schedule(); });
    document.addEventListener('visibilitychange', onVisible);
    measure();
    if (!graph) { setStatus('Loading the flow…'); load(); } else { relayout(); if (auto) fit(); schedule(); }
  }

  function close() {
    if (!opened) return;
    opened = false;
    if (unsub) { unsub(); unsub = null; }
    document.removeEventListener('visibilitychange', onVisible);
    clearTimeout(timer);
    timer = 0;
  }

  return {
    el,
    open,
    close,
    refresh: () => schedule(),
    destroy() {
      close();
      setExpanded(false);
      if (canFull) {
        document.removeEventListener('fullscreenchange', onFullChange);
        if (document.fullscreenElement === el) document.exitFullscreen().catch(() => {});
      }
      gone = true;
      ro.disconnect();
    },
  };
}

function legendItem(style, text) {
  // 'wait' is the waiting node's warn stroke (an approval, or a model call
  // the engine holds), not an edge style.
  const line = style === 'wait'
    ? s('path', { class: 'fl-line', d: 'M1 4H25', style: 'stroke: var(--warn); stroke-width: 2' })
    : s('path', { class: 'fl-line', d: 'M1 4H25' });
  return h('span', { class: 'fl-legend-item' },
    s('svg', { class: `fl-legend-line fl-${style}`, width: 26, height: 8, viewBox: '0 0 26 8', 'aria-hidden': 'true' }, line),
    text);
}

function clock(iso) {
  const d = new Date(iso);
  if (Number.isNaN(+d) || d.getFullYear() < 2000) return '';
  return d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
}

// openFlowSheet shows a chat's (or a session's) flow in a wide sheet;
// returns {refresh} for the opener (the chat refreshes it when a reply
// ends) and resolves done when the sheet closes.
export function openFlowSheet(url, { title = 'Flow', sub = 'Who handed what to whom' } = {}) {
  let panel = null;
  const done = openDialog((close) => {
    panel = flowPanel(url, { onNavigate: () => close(undefined) });
    // Opened after the dialog is in the document, so the stage has a size.
    requestAnimationFrame(() => panel && panel.open());
    return { title, sub, wide: true, body: h('div', { class: 'fl-sheet' }, panel.el) };
  }).finally(() => { if (panel) panel.destroy(); panel = null; });
  return { refresh: () => panel && panel.refresh(), done };
}
