// Flow graph geometry: the pure half of the Flow view (views/flow.js draws
// it). Nothing here touches the DOM, so node --test covers all of it.
//
// The input is the reply of GET /api/sessions/{id}/flow or
// /api/chats/{id}/flow ({nodes, edges, updated}; see internal/api/flow.go).
// layoutGraph places the nodes in layers (longest path from the sources,
// then ordered inside a layer by their parents' order to keep crossings
// down), left to right ('lr') or, on phones, top to bottom ('tb'), and
// computes each edge as a cubic curve from the source's out port to the
// target's in port, with its arrowhead and the middle point for its label
// chip. The view transform {x, y, k} maps graph coordinates to screen:
// screen = graph * k + (x, y); fitTransform, zoomAt and pinchTransform
// compute it for Fit, the wheel and two-finger pinch.

// ---------- sizes ----------

// Cards (sessions, chats) and circles (you, models). A circle node's box
// holds the circle and its two text lines: below it left to right, beside
// it top to bottom.
export const SIZES = {
  lr: { cardW: 220, cardH: 76, circleBoxW: 160, circleBoxH: 92, r: 26, gapLayer: 128, gapNode: 28 },
  tb: { cardW: 200, cardH: 76, circleBoxW: 200, circleBoxH: 52, r: 26, gapLayer: 64, gapNode: 20 },
};

// Font metrics used to cut text before it would overflow a card: average
// advance per character as a share of the font size.
export const FONT = { label: 13, sub: 11, chip: 11, pill: 9.5 };
const SANS_ADV = 0.56;
const MONO_ADV = 0.61;

export const PHONE_MAX = 640;

// layoutDir: the phone layout runs top to bottom.
export function layoutDir(width) {
  return width > 0 && width < PHONE_MAX ? 'tb' : 'lr';
}

// isCircle: "you" and model nodes are circles, the rest cards.
export const isCircle = (node) => node && (node.kind === 'you' || node.kind === 'model');

// ---------- text ----------

// textWidth estimates the drawn width of s at size px. Wide glyphs
// (emoji, CJK) count double.
export function textWidth(s, px, mono = false) {
  let units = 0;
  for (const ch of String(s || '')) units += wide(ch) ? 2 : 1;
  return units * px * (mono ? MONO_ADV : SANS_ADV);
}

function wide(ch) {
  const c = ch.codePointAt(0);
  return c >= 0x1100 && (c <= 0x115f || (c >= 0x2e80 && c <= 0xa4cf) || (c >= 0xac00 && c <= 0xd7a3)
    || (c >= 0xf900 && c <= 0xfaff) || (c >= 0xfe30 && c <= 0xfe4f) || (c >= 0xff00 && c <= 0xff60)
    || (c >= 0x1f300 && c <= 0x1faff) || c === 0x270e);
}

// fitText cuts s with an ellipsis so it fits maxWidth at size px. It
// returns {text, cut}; the view sets the full text as the title when cut.
export function fitText(s, maxWidth, px, mono = false) {
  const str = String(s || '').replace(/\s+/g, ' ').trim();
  if (textWidth(str, px, mono) <= maxWidth) return { text: str, cut: false };
  const chars = [...str];
  let lo = 0;
  let hi = chars.length;
  while (lo < hi) { // the longest prefix that fits with the ellipsis
    const mid = (lo + hi + 1) >> 1;
    if (textWidth(chars.slice(0, mid).join('') + '…', px, mono) <= maxWidth) lo = mid;
    else hi = mid - 1;
  }
  return { text: lo ? chars.slice(0, lo).join('').trimEnd() + '…' : '…', cut: true };
}

// fmtCount: 950 → "950", 3 400 → "3.4k", 12 000 → "12k", 2 100 000 → "2.1M".
export function fmtCount(n) {
  const v = Math.max(0, Number(n) || 0);
  if (v >= 1e6) return `${(v / 1e6).toFixed(v >= 1e7 ? 0 : 1)}M`;
  if (v >= 1e3) return `${(v / 1e3).toFixed(v >= 1e4 ? 0 : 1)}k`;
  return String(Math.round(v));
}

const plural = (n, one, many) => `${n} ${n === 1 ? one : many}`;

// replies: an answer edge counted in replies (a chat without session
// ids), as the server's default caption says.
const replies = (e) => /repl/i.test(e.label || '');

// edgeText is the short caption on an edge's chip: "spawned", "task",
// "2 jobs · 👍1 ✎1", "5 requests", "3 replies · 👎1".
export function edgeText(e) {
  if (!e) return '';
  const n = Math.max(0, Number(e.count) || 0);
  let head;
  switch (e.kind) {
    case 'task': return 'task';
    case 'spawn': return 'spawned';
    // Flow-run edges: a plain step-to-step arrow and an item arrow need no
    // caption; a branch says which way it went.
    case 'next': case 'item': return '';
    case 'then': return 'then';
    case 'else': return 'else';
    case 'ask': if (n === 0) return ''; head = plural(n, 'job', 'jobs'); break;
    case 'answer': head = replies(e) ? plural(n, 'reply', 'replies') : plural(n, 'request', 'requests'); break;
    default: return e.label || e.kind || '';
  }
  const marks = [];
  if (e.good > 0) marks.push(`👍${e.good}`);
  if (e.bad > 0) marks.push(`👎${e.bad}`);
  if (e.corrected > 0) marks.push(`✎${e.corrected}`);
  return [head, marks.join(' ')].filter(Boolean).join(' · ');
}

// edgeTitle is the longer tooltip of an edge: every number in words.
export function edgeTitle(e, fromLabel = '', toLabel = '') {
  if (!e) return '';
  const who = fromLabel && toLabel ? `${fromLabel} → ${toLabel}: ` : '';
  const n = Math.max(0, Number(e.count) || 0);
  switch (e.kind) {
    case 'task': return `${who}the task`;
    case 'spawn': return `${who}spawned a child session`;
    case 'next': return `${who}next step`;
    case 'item': return `${who}fan-out item`;
    case 'then': return `${who}the test held`;
    case 'else': return `${who}the test did not hold`;
    default: {
      const parts = [e.kind === 'ask' ? `${plural(n, 'delegated job', 'delegated jobs')}`
        : replies(e) ? plural(n, 'reply', 'replies') : plural(n, 'request', 'requests')];
      if (e.tokens > 0) parts.push(`${fmtCount(e.tokens)} tokens`);
      if (e.good > 0) parts.push(`${e.good} good`);
      if (e.bad > 0) parts.push(`${e.bad} bad`);
      if (e.corrected > 0) parts.push(`${e.corrected} corrected`);
      return who + parts.join(' · ');
    }
  }
}

// edgeStyle: spawn (and the task, and flow-run arrows) solid, ask dashed,
// answer dotted, a flow's else branch dashed.
export function edgeStyle(kind) {
  switch (kind) {
    case 'ask': case 'else': return 'dashed';
    case 'answer': return 'dotted';
    default: return 'solid';
  }
}

// nodeTone is the state pill's tone: ok | warn | err | info | muted.
export function nodeTone(node) {
  const s = (node && node.state) || '';
  switch (s) {
    case 'loaded': case 'passed': case 'accepted': case 'done': return 'ok';
    case 'waiting': case 'queued': case 'limited': return 'warn';
    case 'failed': case 'error': case 'rejected': return 'err';
    case 'cloud': case 'starting': case 'running': case 'checking': case 'retrying': case 'escalated':
    case 'thinking': case 'writing': case 'tool': case 'busy': return 'info';
    default: return 'muted';
  }
}

// LIVE_STATES are the states of work happening now: the graph highlights
// those nodes (a pulse) and the edges carrying work into them.
export const LIVE_STATES = new Set(['starting', 'running', 'checking', 'retrying', 'escalated',
  'thinking', 'writing', 'tool', 'busy']);

// isLive: the node is executing now.
export function isLive(node) {
  return !!node && LIVE_STATES.has(node.state || '');
}

// liveEdge: the edge carries work that is happening now: it ends at a live
// node, or a live node is asking a model / handing out items over it. An
// edge into a waiting node (waitIds: an item the engine holds) is not live.
export function liveEdge(edge, liveIds, waitIds) {
  if (!edge || !liveIds) return false;
  if (waitIds && waitIds.has(edge.to)) return false;
  if (liveIds.has(edge.to)) return true;
  return liveIds.has(edge.from) && ['ask', 'item', 'spawn', 'task'].includes(edge.kind);
}

// WAIT_STATES are the states of work held back: waiting for an approval,
// or queued (a flow step or item whose model call the engine holds until
// a slot or room frees up). They get the warn stroke and never pulse.
export const WAIT_STATES = new Set(['waiting', 'queued']);

// isWaiting: the node waits (not live, drawn with the .fl-wait look).
export function isWaiting(node) {
  return !!node && WAIT_STATES.has(node.state || '');
}

// nodeTitle is a node's tooltip: label, sub, state and the state's detail
// (why it waits, a retry) on their own lines.
export function nodeTitle(node) {
  if (!node) return '';
  return [node.label, node.sub, node.state ? `state: ${node.state}` : '', node.detail || ''].filter(Boolean).join('\n');
}

// ---------- layout ----------

// layers gives each node its layer: the longest path from a node without
// incoming edges. Edges that would close a cycle (the contract says there
// are none) and edges to unknown ids are ignored.
export function layers(nodes, edges) {
  const ids = nodes.map((n) => n.id);
  const known = new Set(ids);
  const out = new Map(ids.map((id) => [id, []]));
  for (const e of edges || []) {
    if (known.has(e.from) && known.has(e.to) && e.from !== e.to) out.get(e.from).push(e.to);
  }
  // Depth first in node order: drop back edges, then a topological order.
  const mark = new Map();
  const order = [];
  const keep = new Map(ids.map((id) => [id, []]));
  const visit = (id) => {
    mark.set(id, 1);
    for (const to of out.get(id)) {
      if (mark.get(to) === 1) continue; // back edge: a cycle
      keep.get(id).push(to);
      if (!mark.has(to)) visit(to);
    }
    mark.set(id, 2);
    order.push(id);
  };
  for (const id of ids) if (!mark.has(id)) visit(id);
  order.reverse();
  const layer = new Map(ids.map((id) => [id, 0]));
  for (const id of order) {
    for (const to of keep.get(id)) layer.set(to, Math.max(layer.get(to), layer.get(id) + 1));
  }
  return { layer, kept: keep };
}

// orderLayers groups node ids by layer and orders each layer by the
// average position of its parents in the layer before it (input order
// breaks ties and places nodes without parents).
export function orderLayers(nodes, edges) {
  const { layer, kept } = layers(nodes, edges);
  const index = new Map(nodes.map((n, i) => [n.id, i]));
  const parents = new Map(nodes.map((n) => [n.id, []]));
  for (const [from, tos] of kept) for (const to of tos) parents.get(to).push(from);
  const count = nodes.length ? Math.max(...layer.values()) + 1 : 0;
  const rows = Array.from({ length: count }, () => []);
  for (const n of nodes) rows[layer.get(n.id)].push(n.id);
  const pos = new Map();
  for (const row of rows) {
    const key = new Map(row.map((id) => {
      const ps = parents.get(id).filter((p) => pos.has(p));
      const bary = ps.length ? ps.reduce((s, p) => s + pos.get(p), 0) / ps.length : Infinity;
      return [id, bary];
    }));
    row.sort((a, b) => {
      const ka = key.get(a);
      const kb = key.get(b);
      if (ka !== kb) return ka === Infinity ? 1 : kb === Infinity ? -1 : ka - kb;
      return index.get(a) - index.get(b);
    });
    row.forEach((id, i) => pos.set(id, i));
  }
  return rows;
}

// nodeBox is a node's size in a direction.
export function nodeBox(node, dir = 'lr') {
  const z = SIZES[dir] || SIZES.lr;
  return isCircle(node) ? { w: z.circleBoxW, h: z.circleBoxH } : { w: z.cardW, h: z.cardH };
}

// circleOf is where a circle node's circle sits inside its placed box.
export function circleOf(n, dir = 'lr') {
  const z = SIZES[dir] || SIZES.lr;
  return dir === 'tb' ? { cx: n.x + z.r, cy: n.y + z.r, r: z.r } : { cx: n.x + n.w / 2, cy: n.y + z.r, r: z.r };
}

// ports are a placed node's connection points: "in" on the left (top on
// phones), "out" on the right (bottom).
export function ports(n, dir = 'lr') {
  if (isCircle(n)) {
    const c = circleOf(n, dir);
    return dir === 'tb'
      ? { in: { x: c.cx, y: c.cy - c.r }, out: { x: c.cx, y: c.cy + c.r } }
      : { in: { x: c.cx - c.r, y: c.cy }, out: { x: c.cx + c.r, y: c.cy } };
  }
  return dir === 'tb'
    ? { in: { x: n.x + n.w / 2, y: n.y }, out: { x: n.x + n.w / 2, y: n.y + n.h } }
    : { in: { x: n.x, y: n.y + n.h / 2 }, out: { x: n.x + n.w, y: n.y + n.h / 2 } };
}

export const ARROW = 8;

// edgeGeometry is the curve from port a to port b along the layout's main
// axis: one cubic, or with via points (the lanes a long edge takes through
// the layers it skips) one cubic per hop, each leaving and entering along
// the main axis so the joins are smooth. The line stops at the arrow's
// base; the arrow's tip is b. mid is where the label chip goes: the middle
// via point, else the middle of the curve.
// Returns {d, mid, arrow: "x,y x,y x,y", start, end}.
export function edgeGeometry(a, b, dir = 'lr', arrow = ARROW, via = []) {
  const tb = dir === 'tb';
  const sign = (tb ? b.y >= a.y : b.x >= a.x) ? 1 : -1;
  // The base of the arrow: back from the tip along the main axis.
  const end = tb ? { x: b.x, y: b.y - sign * arrow } : { x: b.x - sign * arrow, y: b.y };
  const pts = [a, ...via, end];
  let d = `M${r1(a.x)},${r1(a.y)}`;
  let mid = null;
  let bez = null;
  for (let i = 1; i < pts.length; i++) {
    const p = pts[i - 1];
    const q = pts[i];
    const span = tb ? Math.abs(q.y - p.y) : Math.abs(q.x - p.x);
    const pull = pts.length === 2 ? Math.max(32, span / 2) : span / 2;
    const c1 = tb ? { x: p.x, y: p.y + sign * pull } : { x: p.x + sign * pull, y: p.y };
    const c2 = tb ? { x: q.x, y: q.y - sign * pull } : { x: q.x - sign * pull, y: q.y };
    d += ` C${r1(c1.x)},${r1(c1.y)} ${r1(c2.x)},${r1(c2.y)} ${r1(q.x)},${r1(q.y)}`;
    if (pts.length === 2) { bez = [p, c1, c2, q]; mid = bezierAt(bez, 0.5); }
  }
  if (!mid) mid = via[Math.floor((via.length - 1) / 2)];
  const half = arrow * 0.6;
  const wings = tb
    ? [[end.x - half, end.y], [b.x, b.y], [end.x + half, end.y]]
    : [[end.x, end.y - half], [b.x, b.y], [end.x, end.y + half]];
  return {
    d,
    mid: { x: r1(mid.x), y: r1(mid.y) },
    arrow: wings.map(([x, y]) => `${r1(x)},${r1(y)}`).join(' '),
    start: a,
    end: b,
    bez, // the one cubic [p, c1, c2, q] of an edge without via points, else null
  };
}

// bezierAt is the point at t (0..1) on the cubic [p, c1, c2, q].
export function bezierAt([p, c1, c2, q], t) {
  const u = 1 - t;
  const f = (k) => u * u * u * p[k] + 3 * u * u * t * c1[k] + 3 * u * t * t * c2[k] + t * t * t * q[k];
  return { x: f('x'), y: f('y') };
}

// CHIP_TS are the places along its curve a chip may take, best first.
const CHIP_TS = [0.5, 0.36, 0.64, 0.25, 0.75];

// placeChips keeps label chips apart: a chip that overlaps another moves
// along its own curve to the first free place (CHIP_TS); when its curve has
// none (a short edge), the chip in its way moves along its curve instead.
// A chip in a lane (an edge through skipped layers) only moves when it has
// a curve of its own. Chips that cannot be freed keep the middle. It sets
// each edge's geo.mid and returns the edges.
export function placeChips(edges, air = 4) {
  const clash = (a, at, b, bt) => Math.abs(at.x - bt.x) * 2 < a.chip.w + b.chip.w + 2 * air
    && Math.abs(at.y - bt.y) * 2 < a.chip.h + b.chip.h + 2 * air;
  const freeAt = (e, at) => edges.every((o) => o === e || !clash(e, at, o, o.geo.mid));
  const move = (e) => {
    if (!e.geo.bez) return false;
    const c = CHIP_TS.map((t) => bezierAt(e.geo.bez, t)).find((p) => freeAt(e, p));
    if (c) e.geo.mid = { x: r1(c.x), y: r1(c.y) };
    return !!c;
  };
  for (const e of edges) {
    if (freeAt(e, e.geo.mid) || move(e)) continue;
    for (const o of edges) {
      if (o !== e && clash(e, e.geo.mid, o, o.geo.mid)) move(o);
    }
  }
  return edges;
}

const r1 = (v) => Math.round(v * 10) / 10;

// chipBox is the label chip's size for a caption.
export function chipBox(text) {
  return { w: Math.ceil(textWidth(text, FONT.chip, true)) + 14, h: 18 };
}

// laneBox is the cross-axis room a long edge takes in each layer it passes:
// its chip's height left to right, its chip's width (plus air) top to
// bottom, so chips there never sit on a card.
const laneBox = (chip, dir) => (dir === 'tb' ? { w: chip.w + 8, h: 0 } : { w: 0, h: chip.h + 6 });

// layoutGraph places a flow graph. Returns {dir, nodes, edges, width,
// height}: nodes carry x, y, w, h, layer and their ports; edges their
// geometry, caption, chip size and line style. An edge that skips layers
// runs through a lane of its own in each layer it passes (a virtual node
// ordered like the real ones), so it never crosses a card. Unknown edge
// ends and self loops are left out.
export function layoutGraph(graph, dir = 'lr') {
  const z = SIZES[dir] || SIZES.lr;
  const tb = dir === 'tb';
  const nodes = (graph && Array.isArray(graph.nodes) ? graph.nodes : []).filter((n) => n && n.id);
  const seen = new Set();
  const uniq = nodes.filter((n) => !seen.has(n.id) && seen.add(n.id));
  const edges = (graph && Array.isArray(graph.edges) ? graph.edges : [])
    .filter((e) => e && seen.has(e.from) && seen.has(e.to) && e.from !== e.to)
    .map((e) => { const text = edgeText(e); return { e, text, chip: chipBox(text), via: [] }; });
  const { layer } = layers(uniq, edges.map((x) => x.e));

  // Lanes: virtual nodes for the layers a forward edge skips.
  const all = [...uniq];
  const segs = [];
  edges.forEach((x, i) => {
    const from = layer.get(x.e.from);
    const to = layer.get(x.e.to);
    if (!(to - from > 1)) { segs.push({ from: x.e.from, to: x.e.to }); return; }
    let prev = x.e.from;
    for (let l = from + 1; l < to; l++) {
      const id = `\u0000lane:${i}:${l}`;
      all.push({ id, kind: 'lane', box: laneBox(x.chip, dir) });
      x.via.push(id);
      segs.push({ from: prev, to: id });
      prev = id;
    }
    segs.push({ from: prev, to: x.e.to });
  });

  const rows = orderLayers(all, segs);
  const byId = new Map(all.map((n) => [n.id, n]));
  const box = (id) => { const n = byId.get(id); return n.box || nodeBox(n, dir); };
  // Main axis: one band per layer, as thick as its biggest node. Cross
  // axis: the layer's nodes in order, centred on the widest layer.
  const band = rows.map((row) => Math.max(0, ...row.map((id) => (tb ? box(id).h : box(id).w))));
  const across = rows.map((row) => row.reduce((s, id) => s + (tb ? box(id).w : box(id).h), 0)
    + Math.max(0, row.length - 1) * z.gapNode);
  const crossMax = Math.max(0, ...across);
  const placed = new Map();
  let main = 0;
  rows.forEach((row, li) => {
    let cross = (crossMax - across[li]) / 2;
    for (const id of row) {
      const n = byId.get(id);
      const b = box(id);
      // Centre the node in its band on the main axis.
      const off = (band[li] - (tb ? b.h : b.w)) / 2;
      const p = tb
        ? { ...n, x: cross, y: main + off, w: b.w, h: b.h, layer: li }
        : { ...n, x: main + off, y: cross, w: b.w, h: b.h, layer: li };
      if (n.kind !== 'lane') p.ports = ports(p, dir);
      placed.set(id, p);
      cross += (tb ? b.w : b.h) + z.gapNode;
    }
    main += band[li] + z.gapLayer;
  });
  const outNodes = uniq.map((n) => placed.get(n.id));
  const outEdges = edges.map(({ e, text, chip, via }) => {
    const a = placed.get(e.from);
    const b = placed.get(e.to);
    const points = via.map((id) => { const p = placed.get(id); return { x: p.x + p.w / 2, y: p.y + p.h / 2 }; });
    return {
      ...e,
      key: `${e.from}>${e.to}>${e.kind}`,
      geo: edgeGeometry(a.ports.out, b.ports.in, dir, ARROW, points),
      text,
      title: edgeTitle(e, a.label, b.label),
      chip,
      style: edgeStyle(e.kind),
    };
  });
  placeChips(outEdges);
  const width = tb ? crossMax : Math.max(0, main - z.gapLayer);
  const height = tb ? Math.max(0, main - z.gapLayer) : crossMax;
  return { dir, nodes: outNodes, edges: outEdges, width, height };
}

// ---------- view transform ----------

export const ZOOM = { min: 0.25, max: 2.5 };

export const clampK = (k, lim = ZOOM) => Math.min(lim.max, Math.max(lim.min, k));

// fitTransform centres a width × height graph in a vw × vh viewport with a
// margin, never zooming in past 1 (small graphs stay at their real size).
// With top, a graph shorter than the viewport sits at the top margin
// instead (the top-to-bottom phone layout reads from the top).
export function fitTransform(width, height, vw, vh, pad = 24, lim = ZOOM, top = false) {
  if (!(vw > 0) || !(vh > 0)) return { x: 0, y: 0, k: 1 };
  const w = Math.max(1, width);
  const h = Math.max(1, height);
  const k = clampK(Math.min(1, (vw - 2 * pad) / w, (vh - 2 * pad) / h), lim);
  const y = (vh - h * k) / 2;
  return { x: r1((vw - w * k) / 2), y: r1(top ? Math.min(y, pad) : y), k };
}

// zoomAt scales t by factor around the screen point (px, py): the graph
// point under it stays under it.
export function zoomAt(t, factor, px, py, lim = ZOOM) {
  const k = clampK(t.k * factor, lim);
  const f = k / t.k;
  return { x: px - (px - t.x) * f, y: py - (py - t.y) * f, k };
}

// wheelFactor turns a wheel event's delta into a zoom factor (pixel, line
// or page deltas), one notch ≈ 10 %.
export function wheelFactor(deltaY, deltaMode = 0) {
  const px = deltaY * (deltaMode === 1 ? 16 : deltaMode === 2 ? 400 : 1);
  return Math.exp(-Math.max(-300, Math.min(300, px)) * 0.001);
}

// pinchTransform: from t0 with the two fingers at a0/b0 to now a1/b1. The
// graph point under the start midpoint follows the current midpoint, and
// the scale follows the finger distance.
export function pinchTransform(t0, a0, b0, a1, b1, lim = ZOOM) {
  const d0 = Math.hypot(b0.x - a0.x, b0.y - a0.y) || 1;
  const d1 = Math.hypot(b1.x - a1.x, b1.y - a1.y) || 1;
  const m0 = { x: (a0.x + b0.x) / 2, y: (a0.y + b0.y) / 2 };
  const m1 = { x: (a1.x + b1.x) / 2, y: (a1.y + b1.y) / 2 };
  const k = clampK(t0.k * (d1 / d0), lim);
  const gx = (m0.x - t0.x) / t0.k;
  const gy = (m0.y - t0.y) / t0.k;
  return { x: m1.x - gx * k, y: m1.y - gy * k, k };
}

// transformAttr is the SVG transform attribute for t.
export const transformAttr = (t) => `translate(${r1(t.x)},${r1(t.y)}) scale(${Math.round(t.k * 1000) / 1000})`;

// isDoubleTap: a second tap within 320 ms and 24 px of the first.
export function isDoubleTap(prev, cur) {
  return !!prev && !!cur && cur.t - prev.t <= 320 && Math.hypot(cur.x - prev.x, cur.y - prev.y) <= 24;
}

// TAP_SLOP: a pointer that moved less than this between down and up taps.
export const TAP_SLOP = 6;

// ---------- live refresh ----------

// sessionIds are the session ids of a graph (node ids "s:<id>").
export function sessionIds(graph) {
  const out = new Set();
  for (const n of (graph && graph.nodes) || []) if (n && typeof n.id === 'string' && n.id.startsWith('s:')) out.add(n.id.slice(2));
  return out;
}

// wantsRefresh says whether a store event concerns a graph with these
// session ids: a change of one of them, a new child of one of them, the
// stream reconnecting, or usage recorded for one of them.
export function wantsRefresh(kind, payload, ids) {
  switch (kind) {
    case 'events-open': case 'sessions': return true;
    case 'session': return !!payload && (ids.has(payload.ID) || (!!payload.Parent && ids.has(payload.Parent)));
    case 'usage': {
      const s = payload && (payload.session || payload.Session || payload.session_id);
      return typeof s === 'string' && ids.has(s);
    }
    default: return false;
  }
}

// sameShape: two graphs have the same nodes and edges (ids and kinds), so
// a refresh keeps the person's pan and zoom instead of fitting again.
export function sameShape(a, b) {
  const sig = (g) => JSON.stringify([((g && g.nodes) || []).map((n) => n.id),
    ((g && g.edges) || []).map((e) => `${e.from}>${e.to}>${e.kind}`)]);
  return sig(a) === sig(b);
}
