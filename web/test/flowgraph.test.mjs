// Flow graph: layout, edge geometry, captions, the view transform and the
// live-refresh filter (the pure half of the Flow view).
import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  layoutDir, layers, orderLayers, layoutGraph, edgeGeometry, edgeText, edgeTitle, edgeStyle, nodeTone,
  fitText, textWidth, fmtCount, fitTransform, zoomAt, wheelFactor, pinchTransform, transformAttr,
  isDoubleTap, sessionIds, wantsRefresh, sameShape, ports, nodeBox, SIZES, ZOOM, bezierAt, placeChips,
  isLive, liveEdge, isWaiting, nodeTitle,
} from '../dist/assets/js/flowgraph.js';
import { parseRoute } from '../dist/assets/js/util.js';

// A planner session with one child; both delegate to local models.
const PLANNER = {
  nodes: [
    { id: 'you', kind: 'you', label: 'You', sub: 'Plan the release', state: '', href: '' },
    { id: 's:root', kind: 'session', label: 'planner', sub: 'planner · ollama', state: 'running', href: '#/s/root' },
    { id: 's:kid', kind: 'session', label: 'child', sub: 'demo', state: 'passed', href: '#/s/kid' },
    { id: 'm:qwen', kind: 'model', label: 'qwen', sub: 'ollama', state: 'loaded', href: '#/models' },
    { id: 'm:llama', kind: 'model', label: 'llama', sub: 'ollama', state: 'unloaded', href: '#/models' },
  ],
  edges: [
    { from: 'you', to: 's:root', kind: 'task', label: 'task', count: 1 },
    { from: 's:root', to: 's:kid', kind: 'spawn', label: 'spawned', count: 1 },
    { from: 's:root', to: 'm:qwen', kind: 'ask', count: 2, tokens: 3400, good: 1, bad: 0, corrected: 1 },
    { from: 's:kid', to: 'm:llama', kind: 'ask', count: 1, tokens: 90, good: 0, bad: 1, corrected: 0 },
    { from: 's:kid', to: 'm:qwen', kind: 'answer', count: 5, tokens: 12000, good: 0, bad: 0, corrected: 0 },
  ],
  updated: '2026-11-02T10:00:00Z',
};

test('layoutDir: phones run top to bottom', () => {
  assert.equal(layoutDir(360), 'tb');
  assert.equal(layoutDir(639), 'tb');
  assert.equal(layoutDir(640), 'lr');
  assert.equal(layoutDir(1280), 'lr');
  assert.equal(layoutDir(0), 'lr'); // not measured yet
});

test('layers: longest path from the sources', () => {
  const { layer } = layers(PLANNER.nodes, PLANNER.edges);
  assert.equal(layer.get('you'), 0);
  assert.equal(layer.get('s:root'), 1);
  assert.equal(layer.get('s:kid'), 2);
  // qwen is asked by the root (layer 1) and answers the child (layer 2).
  assert.equal(layer.get('m:qwen'), 3);
  assert.equal(layer.get('m:llama'), 3);
});

test('layers: a cycle and unknown ends do not hang or throw', () => {
  const nodes = [{ id: 'a' }, { id: 'b' }, { id: 'c' }];
  const edges = [{ from: 'a', to: 'b' }, { from: 'b', to: 'c' }, { from: 'c', to: 'a' }, { from: 'a', to: 'zz' }, { from: 'b', to: 'b' }];
  const { layer } = layers(nodes, edges);
  assert.deepEqual([layer.get('a'), layer.get('b'), layer.get('c')], [0, 1, 2]);
});

test('orderLayers: children follow their parents to avoid crossings', () => {
  // p1 and p2 in layer 1; c2 (child of p2) is listed before c1 (child of p1).
  const nodes = [{ id: 'r' }, { id: 'p1' }, { id: 'p2' }, { id: 'c2' }, { id: 'c1' }];
  const edges = [{ from: 'r', to: 'p1' }, { from: 'r', to: 'p2' }, { from: 'p2', to: 'c2' }, { from: 'p1', to: 'c1' }];
  assert.deepEqual(orderLayers(nodes, edges), [['r'], ['p1', 'p2'], ['c1', 'c2']]);
  assert.deepEqual(orderLayers([], []), []);
});

test('layoutGraph left to right: layers advance on x, no overlaps', () => {
  const g = layoutGraph(PLANNER, 'lr');
  assert.equal(g.dir, 'lr');
  assert.equal(g.nodes.length, 5);
  assert.equal(g.edges.length, 5);
  const at = Object.fromEntries(g.nodes.map((n) => [n.id, n]));
  assert.ok(at.you.x < at['s:root'].x && at['s:root'].x < at['s:kid'].x && at['s:kid'].x < at['m:qwen'].x);
  assert.equal(at['m:qwen'].layer, at['m:llama'].layer);
  assert.equal(at['s:root'].w, SIZES.lr.cardW);
  assert.deepEqual(nodeBox(at.you, 'lr'), { w: SIZES.lr.circleBoxW, h: SIZES.lr.circleBoxH });
  for (const a of g.nodes) {
    assert.ok(a.x >= 0 && a.y >= 0 && a.x + a.w <= g.width + 0.01 && a.y + a.h <= g.height + 0.01, `${a.id} inside the bounds`);
    for (const b of g.nodes) {
      if (a === b) continue;
      const apart = a.x + a.w <= b.x || b.x + b.w <= a.x || a.y + a.h <= b.y || b.y + b.h <= a.y;
      assert.ok(apart, `${a.id} and ${b.id} overlap`);
    }
  }
  // Edges run from the source's out port to the target's in port.
  const ask = g.edges.find((e) => e.kind === 'ask' && e.from === 's:root');
  assert.ok(ask.geo.d.startsWith(`M${at['s:root'].x + at['s:root'].w},`));
  assert.deepEqual(ask.geo.end, at['m:qwen'].ports.in);
  assert.equal(ask.style, 'dashed');
  assert.equal(ask.text, '2 jobs · 👍1 ✎1');
  assert.match(ask.title, /^planner → qwen: 2 delegated jobs · 3.4k tokens · 1 good · 1 corrected$/);
});

test('layoutGraph top to bottom: layers advance on y', () => {
  const g = layoutGraph(PLANNER, 'tb');
  const at = Object.fromEntries(g.nodes.map((n) => [n.id, n]));
  assert.ok(at.you.y < at['s:root'].y && at['s:root'].y < at['s:kid'].y && at['s:kid'].y < at['m:qwen'].y);
  assert.equal(at['m:qwen'].y, at['m:llama'].y);
  // Ports are top and bottom.
  const p = ports(at['s:root'], 'tb');
  assert.deepEqual(p.in, { x: at['s:root'].x + at['s:root'].w / 2, y: at['s:root'].y });
  assert.equal(p.out.y, at['s:root'].y + at['s:root'].h);
  assert.ok(g.width <= SIZES.tb.cardW * 2 + SIZES.tb.gapNode + 0.01);
});

test('layoutGraph: an edge that skips a layer takes a lane, never a card', () => {
  for (const dir of ['lr', 'tb']) {
    const g = layoutGraph(PLANNER, dir);
    const long = g.edges.find((e) => e.from === 's:root' && e.to === 'm:qwen');
    const kid = g.nodes.find((n) => n.id === 's:kid');
    // The chip sits in the child's layer, beside the child card.
    const c = long.geo.mid;
    const w = long.chip.w / 2;
    const hh = long.chip.h / 2;
    const apart = c.x + w <= kid.x || kid.x + kid.w <= c.x - w || c.y + hh <= kid.y || kid.y + kid.h <= c.y - hh;
    assert.ok(apart, `${dir}: the lane chip overlaps the child card`);
    if (dir === 'lr') assert.ok(c.x > kid.x && c.x < kid.x + kid.w);
    else assert.ok(c.y > kid.y && c.y < kid.y + kid.h);
    assert.equal((long.geo.d.match(/C/g) || []).length, 2); // two hops
    // Lanes are not nodes.
    assert.equal(g.nodes.length, 5);
  }
});

test('edgeGeometry through via points', () => {
  const g = edgeGeometry({ x: 0, y: 0 }, { x: 208, y: 100 }, 'lr', 8, [{ x: 100, y: 40 }]);
  assert.equal(g.d, 'M0,0 C50,0 50,40 100,40 C150,40 150,100 200,100');
  assert.deepEqual(g.mid, { x: 100, y: 40 });
  const two = edgeGeometry({ x: 0, y: 0 }, { x: 0, y: 308 }, 'tb', 8, [{ x: 0, y: 100 }, { x: 10, y: 200 }]);
  assert.deepEqual(two.mid, { x: 0, y: 100 });
  assert.equal((two.d.match(/C/g) || []).length, 3);
});

test('layoutGraph drops bad input', () => {
  assert.deepEqual(layoutGraph(null, 'lr'), { dir: 'lr', nodes: [], edges: [], width: 0, height: 0 });
  const g = layoutGraph({ nodes: [{ id: 'a', kind: 'you' }, { id: 'a', kind: 'you' }, null, { kind: 'x' }],
    edges: [{ from: 'a', to: 'gone', kind: 'ask' }, { from: 'a', to: 'a', kind: 'ask' }] }, 'lr');
  assert.equal(g.nodes.length, 1);
  assert.equal(g.edges.length, 0);
});

test('edgeGeometry: a cubic along the main axis with the arrow at the tip', () => {
  const a = { x: 0, y: 0 };
  const b = { x: 200, y: 100 };
  const lr = edgeGeometry(a, b, 'lr', 8);
  assert.equal(lr.d, 'M0,0 C96,0 96,100 192,100');
  assert.deepEqual(lr.mid, { x: 96, y: 50 });
  assert.equal(lr.arrow, '192,95.2 200,100 192,104.8');
  const tb = edgeGeometry(a, b, 'tb', 8);
  assert.equal(tb.d, 'M0,0 C0,46 200,46 200,92');
  assert.equal(tb.arrow, '195.2,92 200,100 204.8,92');
  // Short hops still bend (a minimum pull).
  assert.equal(edgeGeometry({ x: 0, y: 0 }, { x: 40, y: 0 }, 'lr', 8).d, 'M0,0 C32,0 0,0 32,0');
  // Backwards (never in a layered graph): the arrow still points at b.
  assert.equal(edgeGeometry({ x: 100, y: 0 }, { x: 0, y: 0 }, 'lr', 8).arrow, '8,-4.8 0,0 8,4.8');
});

test('edgeText and edgeTitle', () => {
  assert.equal(edgeText({ kind: 'task' }), 'task');
  assert.equal(edgeText({ kind: 'spawn', count: 1 }), 'spawned');
  assert.equal(edgeText({ kind: 'ask', count: 1 }), '1 job');
  assert.equal(edgeText({ kind: 'ask', count: 3, good: 2, bad: 1, corrected: 1 }), '3 jobs · 👍2 👎1 ✎1');
  assert.equal(edgeText({ kind: 'answer', count: 5 }), '5 requests');
  assert.equal(edgeText({ kind: 'answer', count: 1 }), '1 request');
  assert.equal(edgeText({ kind: 'answer', count: 3, label: '3 replies', bad: 1 }), '3 replies · 👎1');
  assert.equal(edgeText({ kind: 'tool', label: 'mcp' }), 'mcp');
  assert.equal(edgeText(null), '');
  assert.equal(edgeTitle({ kind: 'spawn' }, 'a', 'b'), 'a → b: spawned a child session');
  assert.equal(edgeTitle({ kind: 'task' }), 'the task');
  assert.equal(edgeTitle({ kind: 'answer', count: 2, tokens: 12500, label: '' }), '2 requests · 13k tokens');
  assert.equal(edgeStyle('spawn'), 'solid');
  assert.equal(edgeStyle('task'), 'solid');
  assert.equal(edgeStyle('ask'), 'dashed');
  assert.equal(edgeStyle('answer'), 'dotted');
});

test('nodeTone', () => {
  assert.equal(nodeTone({ state: 'loaded' }), 'ok');
  assert.equal(nodeTone({ state: 'cloud' }), 'info');
  assert.equal(nodeTone({ state: 'unloaded' }), 'muted');
  assert.equal(nodeTone({ state: 'running' }), 'info');
  assert.equal(nodeTone({ state: 'waiting' }), 'warn');
  assert.equal(nodeTone({ state: 'failed' }), 'err');
  assert.equal(nodeTone({ state: 'thinking' }), 'info');
  assert.equal(nodeTone({ state: 'idle' }), 'muted');
  assert.equal(nodeTone({}), 'muted');
});

test('fitText never overflows and says when it cut', () => {
  assert.deepEqual(fitText('short', 100, 13), { text: 'short', cut: false });
  const long = 'a-very-long-session-name-that-will-not-fit-in-a-card';
  const r = fitText(long, 120, 13);
  assert.equal(r.cut, true);
  assert.ok(r.text.endsWith('…'));
  assert.ok(textWidth(r.text, 13) <= 120);
  assert.ok(textWidth(fitText(long, 80, 11, true).text, 11, true) <= 80);
  assert.deepEqual(fitText('  two\n lines ', 200, 13), { text: 'two lines', cut: false });
  assert.deepEqual(fitText('', 50, 13), { text: '', cut: false });
  assert.equal(fitText('abcdef', 1, 13).text, '…');
  // Emoji are wide.
  assert.ok(textWidth('👍', 10) > textWidth('a', 10));
});

test('fmtCount', () => {
  assert.equal(fmtCount(950), '950');
  assert.equal(fmtCount(3400), '3.4k');
  assert.equal(fmtCount(12000), '12k');
  assert.equal(fmtCount(2100000), '2.1M');
  assert.equal(fmtCount(-5), '0');
});

test('fitTransform centres and never zooms past 1', () => {
  assert.deepEqual(fitTransform(200, 100, 1000, 500, 0), { x: 400, y: 200, k: 1 });
  const t = fitTransform(2000, 400, 1000, 500, 0);
  assert.equal(t.k, 0.5);
  assert.deepEqual([t.x, t.y], [0, 150]);
  // Tiny viewports clamp at the minimum zoom.
  assert.equal(fitTransform(100000, 100, 300, 300, 0).k, ZOOM.min);
  assert.deepEqual(fitTransform(10, 10, 0, 0), { x: 0, y: 0, k: 1 });
  // top: a short graph sits at the top margin; a tall one still centres.
  assert.deepEqual(fitTransform(200, 100, 1000, 500, 16, ZOOM, true), { x: 400, y: 16, k: 1 });
  assert.equal(fitTransform(200, 1000, 1000, 500, 0, ZOOM, true).y, 0);
});

test('zoomAt keeps the point under the cursor', () => {
  const t = { x: 10, y: 20, k: 1 };
  const z = zoomAt(t, 2, 110, 120);
  assert.equal(z.k, 2);
  // Graph point under (110, 120) before: (100, 100); after: the same.
  assert.deepEqual([(110 - z.x) / z.k, (120 - z.y) / z.k], [100, 100]);
  assert.equal(zoomAt(t, 100, 0, 0).k, ZOOM.max);
  assert.equal(zoomAt(t, 0.001, 0, 0).k, ZOOM.min);
});

test('wheelFactor: down zooms out, up zooms in, bounded', () => {
  assert.ok(wheelFactor(100) < 1);
  assert.ok(wheelFactor(-100) > 1);
  assert.equal(wheelFactor(0), 1);
  assert.equal(wheelFactor(3, 1), wheelFactor(48, 0)); // lines
  assert.equal(wheelFactor(1e6), wheelFactor(300));
});

test('pinchTransform: scale by finger distance around the midpoint', () => {
  const t0 = { x: 0, y: 0, k: 1 };
  // Fingers spread from 100 px to 200 px apart around (100, 100).
  const t = pinchTransform(t0, { x: 50, y: 100 }, { x: 150, y: 100 }, { x: 0, y: 100 }, { x: 200, y: 100 });
  assert.equal(t.k, 2);
  assert.deepEqual([(100 - t.x) / t.k, (100 - t.y) / t.k], [100, 100]);
  // Both fingers move 30 px right without spreading: a pan.
  const p = pinchTransform(t0, { x: 0, y: 0 }, { x: 100, y: 0 }, { x: 30, y: 0 }, { x: 130, y: 0 });
  assert.deepEqual(p, { x: 30, y: 0, k: 1 });
  // Same points (distance 0) do not divide by zero.
  const same = pinchTransform(t0, { x: 5, y: 5 }, { x: 5, y: 5 }, { x: 5, y: 5 }, { x: 5, y: 5 });
  assert.ok(Number.isFinite(same.k) && Number.isFinite(same.x));
});

test('transformAttr', () => {
  assert.equal(transformAttr({ x: 1.234, y: -5, k: 0.33333 }), 'translate(1.2,-5) scale(0.333)');
});

test('isDoubleTap', () => {
  assert.equal(isDoubleTap(null, { t: 0, x: 0, y: 0 }), false);
  assert.equal(isDoubleTap({ t: 0, x: 0, y: 0 }, { t: 250, x: 10, y: 10 }), true);
  assert.equal(isDoubleTap({ t: 0, x: 0, y: 0 }, { t: 400, x: 0, y: 0 }), false);
  assert.equal(isDoubleTap({ t: 0, x: 0, y: 0 }, { t: 100, x: 40, y: 0 }), false);
});

test('live refresh: only events about the graph', () => {
  const ids = sessionIds(PLANNER);
  assert.deepEqual([...ids], ['root', 'kid']);
  assert.equal(wantsRefresh('session', { ID: 'root' }, ids), true);
  assert.equal(wantsRefresh('session', { ID: 'new', Parent: 'kid' }, ids), true); // a new grandchild
  assert.equal(wantsRefresh('session', { ID: 'other', Parent: '' }, ids), false);
  assert.equal(wantsRefresh('session', null, ids), false);
  assert.equal(wantsRefresh('events-open', undefined, ids), true);
  assert.equal(wantsRefresh('sessions', undefined, ids), true);
  assert.equal(wantsRefresh('usage', { session: 'kid' }, ids), true);
  assert.equal(wantsRefresh('usage', { session: 'other' }, ids), false);
  assert.equal(wantsRefresh('inbox', {}, ids), false);
  assert.deepEqual([...sessionIds(null)], []);
});

test('sameShape ignores numbers and states', () => {
  const b = structuredClone(PLANNER);
  b.nodes[1].state = 'passed';
  b.edges[2].count = 9;
  assert.equal(sameShape(PLANNER, b), true);
  b.edges.push({ from: 's:root', to: 'm:llama', kind: 'ask' });
  assert.equal(sameShape(PLANNER, b), false);
});

test('parseRoute knows the Flow tab', () => {
  assert.deepEqual(parseRoute('#/s/abc/flow'), { view: 'session', id: 'abc', tab: 'flow' });
  assert.deepEqual(parseRoute('#/s/abc/files'), { view: 'session', id: 'abc', tab: 'files' });
  assert.deepEqual(parseRoute('#/s/abc'), { view: 'session', id: 'abc' });
  assert.deepEqual(parseRoute('#/s/abc/other'), { view: 'session', id: 'abc' });
});

test('bezierAt: ends and middle of a cubic', () => {
  const c = [{ x: 0, y: 0 }, { x: 0, y: 10 }, { x: 10, y: 10 }, { x: 10, y: 20 }];
  assert.deepEqual(bezierAt(c, 0), { x: 0, y: 0 });
  assert.deepEqual(bezierAt(c, 1), { x: 10, y: 20 });
  assert.deepEqual(bezierAt(c, 0.5), { x: 5, y: 10 });
});

test('placeChips: a chip on top of another slides along its own curve', () => {
  const bez = [{ x: 0, y: 0 }, { x: 0, y: 150 }, { x: 0, y: 150 }, { x: 0, y: 300 }];
  const mk = () => ({ chip: { w: 60, h: 18 }, geo: { mid: bezierAt(bez, 0.5), bez } });
  const lane = { chip: { w: 60, h: 18 }, geo: { mid: { x: 0, y: 150 }, bez: null } };
  const [a, b, c] = placeChips([mk(), mk(), lane]);
  assert.deepEqual(c.geo.mid, { x: 0, y: 150 }); // a lane chip keeps its lane
  const ys = [a.geo.mid.y, b.geo.mid.y, c.geo.mid.y].sort((p, q) => p - q);
  assert.ok(ys[1] - ys[0] >= 18 && ys[2] - ys[1] >= 18, `chips apart: ${ys}`); // moved along their curve
  assert.equal(a.geo.mid.x, 0);
  // A short edge with no free place: the chip in its way moves instead.
  const long = [{ x: 0, y: 0 }, { x: -200, y: 0 }, { x: -200, y: 300 }, { x: -200, y: 300 }];
  const sp = { chip: { w: 60, h: 18 }, geo: { mid: { x: 0, y: 5 }, bez: long } };
  const short = { chip: { w: 60, h: 18 }, geo: { mid: { x: 0, y: 5 }, bez: [{ x: 0, y: 0 }, { x: 0, y: 5 }, { x: 0, y: 5 }, { x: 0, y: 10 }] } };
  placeChips([sp, short]);
  assert.deepEqual(short.geo.mid, { x: 0, y: 5 });
  assert.notDeepEqual(sp.geo.mid, { x: 0, y: 5 });
  // A curve too short for any free place keeps the middle.
  const tiny = [{ x: 0, y: 0 }, { x: 0, y: 5 }, { x: 0, y: 5 }, { x: 0, y: 10 }];
  const [, d] = placeChips([{ chip: { w: 60, h: 18 }, geo: { mid: { x: 0, y: 5 }, bez: tiny } },
    { chip: { w: 60, h: 18 }, geo: { mid: { x: 0, y: 5 }, bez: tiny } }]);
  assert.deepEqual(d.geo.mid, { x: 0, y: 5 });
});

test('layoutGraph: no two chips overlap, both directions', () => {
  // Siblings from one parent put their chips at the same height on phones.
  const g = {
    nodes: [...PLANNER.nodes, { id: 'm:gemma', kind: 'model', label: 'gemma', sub: 'ollama', state: 'loaded', href: '#/models' }],
    edges: [...PLANNER.edges, { from: 's:root', to: 'm:gemma', kind: 'answer', count: 2, tokens: 10 }],
  };
  for (const dir of ['lr', 'tb']) {
    const { edges } = layoutGraph(g, dir);
    for (let i = 0; i < edges.length; i++) {
      for (let j = i + 1; j < edges.length; j++) {
        const a = edges[i], b = edges[j];
        const apart = Math.abs(a.geo.mid.x - b.geo.mid.x) * 2 >= a.chip.w + b.chip.w
          || Math.abs(a.geo.mid.y - b.geo.mid.y) * 2 >= a.chip.h + b.chip.h;
        assert.ok(apart, `${dir}: chips of ${a.key} and ${b.key} overlap`);
      }
    }
  }
});

test('flow-run edges and step states', () => {
  assert.equal(edgeText({ kind: 'next' }), '');
  assert.equal(edgeText({ kind: 'item' }), '');
  assert.equal(edgeText({ kind: 'then' }), 'then');
  assert.equal(edgeText({ kind: 'else' }), 'else');
  assert.equal(edgeText({ kind: 'ask', count: 0 }), '');
  assert.equal(edgeText({ kind: 'ask', count: 2 }), '2 jobs');
  assert.match(edgeTitle({ kind: 'else' }, 'check', 'end'), /^check → end: /);
  assert.equal(edgeStyle('else'), 'dashed');
  assert.equal(edgeStyle('next'), 'solid');
  assert.equal(nodeTone({ state: 'rejected' }), 'err');
  assert.equal(nodeTone({ state: 'limited' }), 'warn');
  assert.equal(nodeTone({ state: 'skipped' }), 'muted');
  assert.equal(nodeTone({ state: 'canceled' }), 'muted');
});

test('waiting: queued and waiting nodes take the wait look, never the live pulse', () => {
  for (const st of ['waiting', 'queued']) {
    assert.equal(isWaiting({ state: st }), true, st);
    assert.equal(isLive({ state: st }), false, st);
    assert.equal(nodeTone({ state: st }), 'warn', st);
  }
  for (const st of ['running', 'passed', 'pending', '', undefined]) assert.equal(isWaiting({ state: st }), false, String(st));
  assert.equal(isWaiting(null), false);
  const n = { label: 'CSS', sub: 'qwen', state: 'queued', detail: 'waiting for qwen: 1 of 1 slots busy' };
  assert.equal(nodeTitle(n), 'CSS\nqwen\nstate: queued\nwaiting for qwen: 1 of 1 slots busy');
  assert.equal(nodeTitle({ label: 'A', state: 'passed' }), 'A\nstate: passed');
  // a queued item does not light the edge into it
  assert.equal(liveEdge({ from: 'st:f', to: 'it:f:0', kind: 'item' }, new Set()), false);
  // ... also while the fan-out step itself is live (another item runs)
  assert.equal(liveEdge({ from: 'st:f', to: 'it:f:0', kind: 'item' }, new Set(['st:f']), new Set(['it:f:0'])), false);
  assert.equal(liveEdge({ from: 'st:f', to: 'it:f:1', kind: 'item' }, new Set(['st:f']), new Set(['it:f:0'])), true);
});

test('live: running nodes and the edges carrying work into them', () => {
  assert.equal(isLive({ state: 'running' }), true);
  assert.equal(isLive({ state: 'thinking' }), true);
  for (const st of ['passed', 'waiting', 'pending', 'loaded', 'cloud', '', undefined]) assert.equal(isLive({ state: st }), false, String(st));
  assert.equal(isLive(null), false);
  const live = new Set(['b']);
  assert.equal(liveEdge({ from: 'a', to: 'b', kind: 'next' }, live), true, 'into a running step');
  assert.equal(liveEdge({ from: 'b', to: 'm', kind: 'ask' }, live), true, 'a running step asking its model');
  assert.equal(liveEdge({ from: 'b', to: 'i1', kind: 'item' }, live), true, 'handing out items');
  assert.equal(liveEdge({ from: 'b', to: 'c', kind: 'next' }, live), false, 'not the next step yet');
  assert.equal(liveEdge({ from: 'x', to: 'y', kind: 'next' }, live), false);
  assert.equal(liveEdge(null, live), false);
});
