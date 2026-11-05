// Unit tests for the Security view's pure helpers: audit normalising,
// query and paging, devices, the two-press revoke, rotate confirm, doctor
// counts and ordering, and QR reading and drawing.
// Run: node --test web/test/*.test.mjs
import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  pick, listOf, normAudit, routeLabel, statusTone, actorTone, auditQuery, auditPage, AUDIT_PAGE, AUDIT_MAX,
  normDevice, normDevices, deviceNameError, normPairing, otherPairURLs, canPair, nextPress, rotateReady, normRotate,
  normStatus, normDoctor, doctorCounts, doctorTone, qrMatrix, qrText, qrImage, qrPath,
} from '../dist/assets/js/security.js';
import { parseRoute } from '../dist/assets/js/util.js';

test('parseRoute: #/security', () => {
  assert.deepEqual(parseRoute('#/security'), { view: 'security' });
});

test('pick and listOf accept Go names, JSON tags and wrapped lists', () => {
  assert.equal(pick({ Name: 'a' }, 'name', 'Name'), 'a');
  assert.equal(pick({ name: 0 }, 'name'), 0);
  assert.equal(pick(null, 'x'), undefined);
  assert.deepEqual(listOf([1]), [1]);
  assert.deepEqual(listOf({ items: [2] }), [2]);
  assert.deepEqual(listOf({ Devices: [3] }), [3]);
  assert.deepEqual(listOf(null), []);
  assert.deepEqual(listOf('x'), []);
});

test('normAudit: both casings, actor split, zero time', () => {
  const a = normAudit({ time: '2026-11-01T10:00:00Z', actor: 'agent:abc:1', remote: '10.0.0.2:5', method: 'POST', route: 'POST /api/sessions/{id}/stop', targets: ['abc'], status: 200, detail: 'x' });
  assert.equal(a.time, Date.parse('2026-11-01T10:00:00Z'));
  assert.equal(a.kind, 'agent');
  assert.equal(a.who, 'abc:1');
  assert.deepEqual(a.targets, ['abc']);
  const b = normAudit({ Time: '0001-01-01T00:00:00Z', Actor: 'cli', Status: 0, Targets: null });
  assert.equal(b.time, 0);
  assert.equal(b.kind, 'cli');
  assert.equal(b.who, '');
  assert.deepEqual(b.targets, []);
  assert.equal(normAudit({}).kind, 'system');
});

test('routeLabel drops a repeated method', () => {
  assert.equal(routeLabel({ method: 'POST', route: 'POST /api/x' }), '/api/x');
  assert.equal(routeLabel({ method: '', route: 'sandbox.fallback' }), 'sandbox.fallback');
  assert.equal(routeLabel({ method: 'GET', route: '/api/y' }), '/api/y');
});

test('statusTone and actorTone', () => {
  assert.equal(statusTone(0), 'info');
  assert.equal(statusTone(200), 'ok');
  assert.equal(statusTone(302), 'ok');
  assert.equal(statusTone(404), 'warn');
  assert.equal(statusTone(401), 'err');
  assert.equal(statusTone(429), 'err');
  assert.equal(statusTone(500), 'err');
  assert.equal(actorTone('owner'), 'ok');
  assert.equal(actorTone('agent'), 'info');
  assert.equal(actorTone('weird'), 'muted');
});

test('auditQuery: since as RFC 3339, actor, limit one past the pages', () => {
  const now = Date.parse('2026-11-01T12:00:00Z');
  const u = new URL(auditQuery({ rangeKey: '1h', actor: ' owner:phone ', pages: 2, now }), 'http://x');
  assert.equal(u.pathname, '/api/audit');
  assert.equal(u.searchParams.get('since'), '2026-11-01T11:00:00.000Z');
  assert.equal(u.searchParams.get('actor'), 'owner:phone');
  assert.equal(u.searchParams.get('limit'), String(2 * AUDIT_PAGE + 1));
  const all = new URL(auditQuery({ rangeKey: 'all', now }), 'http://x');
  assert.equal(all.searchParams.has('since'), false);
  assert.equal(all.searchParams.has('actor'), false);
  const big = new URL(auditQuery({ pages: 1000, now }), 'http://x');
  assert.equal(big.searchParams.get('limit'), String(AUDIT_MAX));
  // Unknown range falls back to 24 h.
  const bad = new URL(auditQuery({ rangeKey: 'nope', now }), 'http://x');
  assert.equal(bad.searchParams.get('since'), '2026-10-31T12:00:00.000Z');
});

test('auditPage sorts newest first and reports more', () => {
  const recs = [];
  for (let i = 0; i < AUDIT_PAGE + 1; i++) recs.push({ time: new Date(Date.UTC(2026, 10, 1, 0, i)).toISOString(), actor: 'cli' });
  const p = auditPage(recs, 1);
  assert.equal(p.rows.length, AUDIT_PAGE);
  assert.equal(p.more, true);
  assert.ok(p.rows[0].time > p.rows[1].time);
  assert.equal(p.rows[0].time, Date.UTC(2026, 10, 1, 0, AUDIT_PAGE));
  const q = auditPage({ items: recs.slice(0, 3) }, 1);
  assert.equal(q.rows.length, 3);
  assert.equal(q.more, false);
  assert.deepEqual(auditPage(null).rows, []);
});

test('devices: normalise, sort current first then by last use, drop id-less', () => {
  const d = normDevice({ ID: 'a1', Name: 'phone', Created: '2026-10-01T00:00:00Z', LastSeen: '2026-10-30T00:00:00Z', LastAddr: '192.168.1.5' });
  assert.equal(d.id, 'a1');
  assert.equal(d.lastAddr, '192.168.1.5');
  assert.equal(normDevice({ id: 'x' }).name, 'unnamed');
  assert.equal(normDevice({ id: 'x' }).client, false);
  assert.equal(normDevice({ id: 'x', scope: 'mcp' }).client, true);
  assert.equal(normDevice({ ID: 'x', Scope: 'other' }).client, false);
  const list = normDevices([
    { id: 'old', name: 'old', created: '2026-01-01T00:00:00Z' },
    { id: 'new', name: 'new', last_seen: '2026-10-31T00:00:00Z' },
    { id: 'me', name: 'me', current: true },
    { name: 'no id' },
  ]);
  assert.deepEqual(list.map((x) => x.id), ['me', 'new', 'old']);
});

test('deviceNameError', () => {
  assert.match(deviceNameError(''), /name/);
  assert.match(deviceNameError('a/b'), /Letters/);
  assert.match(deviceNameError('x'.repeat(41)), /Letters/);
  assert.equal(deviceNameError('Miguel’s phone'.replace('’', "'")), '');
  assert.equal(deviceNameError('teléfono 2'), '');
});

test('normPairing and normRotate', () => {
  assert.deepEqual(normPairing({ id: '1', name: 'p', url: 'https://h/?token=t', qr: 'X' }), { id: '1', name: 'p', url: 'https://h/?token=t', qr: 'X', urls: [] });
  assert.equal(normPairing({ URL: 'u', QR: [[1]] }).url, 'u');
  assert.deepEqual(normRotate({ token: 't' }), { token: 't', url: '' });
  assert.deepEqual(normRotate(null), { token: '', url: '' });
});

test('otherPairURLs and canPair', () => {
  const p = normPairing({ url: 'http://192.168.1.5:7340/?token=t', urls: ['http://192.168.1.5:7340/?token=t', 'http://127.0.0.1:7340/?token=t', 7] });
  assert.deepEqual(otherPairURLs(p), ['http://127.0.0.1:7340/?token=t', '7']);
  assert.deepEqual(otherPairURLs({ url: 'u' }), []);
  assert.equal(canPair({ devices: [], current: 'admin' }), true);
  assert.equal(canPair({ devices: [], current: 'd123' }), false);
  assert.equal(canPair({ devices: [] }), true);
  assert.equal(canPair([]), true);
});

test('deviceNameError accepts combining marks', () => {
  assert.equal(deviceNameError('te\u0301le\u0301fono'), '');
});

test('nextPress: arm, fire inside the window, re-arm after it', () => {
  let s = nextPress(0, 1000);
  assert.deepEqual(s, { fire: false, armedAt: 1000 });
  assert.deepEqual(nextPress(s.armedAt, 3000), { fire: true, armedAt: 0 });
  assert.deepEqual(nextPress(s.armedAt, 9000), { fire: false, armedAt: 9000 });
  s = nextPress(0, 0); // a clock at 0 still needs two presses
  assert.equal(s.fire, false);
});

test('rotateReady needs the exact word', () => {
  assert.equal(rotateReady('rotate'), true);
  assert.equal(rotateReady(' ROTATE '), true);
  assert.equal(rotateReady('ROTAT'), false);
  assert.equal(rotateReady(undefined), false);
});

test('doctor: status mapping, failures first, counts, tone', () => {
  assert.equal(normStatus('OK'), 'ok');
  assert.equal(normStatus('fail'), 'fail');
  assert.equal(normStatus('error'), 'fail');
  assert.equal(normStatus('warn'), 'warn');
  assert.equal(normStatus(''), 'warn');
  const checks = normDoctor([
    { ID: 'a', Title: 'A', Status: 'ok' },
    { ID: 'b', Title: 'B', Status: 'warn', Fix: 'do b' },
    { id: 'c', status: 'fail', detail: 'bad' },
    { ID: 'd', Title: 'D', Status: 'ok' },
  ]);
  assert.deepEqual(checks.map((c) => c.id), ['c', 'b', 'a', 'd']);
  assert.equal(checks[0].title, 'c'); // id stands in for a missing title
  const n = doctorCounts(checks);
  assert.deepEqual(n, { ok: 2, warn: 1, fail: 1, total: 4 });
  assert.equal(doctorTone(n), 'err');
  assert.equal(doctorTone({ ok: 1, warn: 1, fail: 0, total: 2 }), 'warn');
  assert.equal(doctorTone({ ok: 3, warn: 0, fail: 0, total: 3 }), 'ok');
  assert.equal(doctorTone({ ok: 0, warn: 0, fail: 0, total: 0 }), 'info');
  assert.deepEqual(normDoctor({ checks: [{ ID: 'z', Status: 'ok' }] }).map((c) => c.id), ['z']);
});

const N = 21;
const grid = (fn) => Array.from({ length: N }, (_, y) => Array.from({ length: N }, (_, x) => fn(x, y)));

test('qrMatrix reads booleans, numbers, bit strings, block strings and flat objects', () => {
  const g = grid((x, y) => (x + y) % 2 === 0);
  assert.deepEqual(qrMatrix(g), g);
  assert.deepEqual(qrMatrix(g.map((r) => r.map(Number))), g);
  assert.deepEqual(qrMatrix(g.map((r) => r.map((v) => (v ? '1' : '0')).join(''))), g);
  assert.deepEqual(qrMatrix(g.map((r) => r.map((v) => (v ? '█' : ' ')).join(''))), g);
  assert.deepEqual(qrMatrix({ size: N, modules: g.flat() }), g);
  assert.deepEqual(qrMatrix({ Matrix: g }), g);
  // qr.Code.String() and a serialised qr.Code.
  assert.deepEqual(qrMatrix(g.map((r) => r.map((v) => (v ? '#' : '.')).join('')).join('\n') + '\n'), g);
  assert.deepEqual(qrMatrix({ Version: 1, Mask: 0, Size: N, Modules: g }), g);
});

test('qrMatrix refuses what is not a square matrix', () => {
  assert.equal(qrMatrix('█▀▄'), null);
  assert.equal(qrMatrix([[true]]), null); // smaller than version 1
  assert.equal(qrMatrix(grid(() => true).map((r, i) => (i === 3 ? r.slice(1) : r))), null);
  assert.equal(qrMatrix(Array(N).fill('▀▄'.repeat(N))), null); // half blocks are text
  assert.equal(qrMatrix(null), null);
});

test('qrText and qrImage', () => {
  assert.equal(qrText('█▀\n▄█  \n\n'), '█▀\n▄█');
  assert.equal(qrText(['▀▄', '▄▀']), '▀▄\n▄▀');
  assert.equal(qrText(grid(() => true)), '');
  assert.equal(qrText(grid(() => true).map((r) => r.map(() => '#').join('')).join('\n')), '');
  assert.equal(qrText(42), '');
  assert.equal(qrImage('data:image/png;base64,AAAA'), 'data:image/png;base64,AAAA');
  assert.equal(qrImage('javascript:alert(1)'), '');
  assert.equal(qrImage('data:text/html;base64,AAAA'), '');
});

test('qrPath merges runs and offsets by the quiet zone', () => {
  const m = grid((x, y) => y === 0 && x < 3);
  const { d, size } = qrPath(m);
  assert.equal(size, N + 8);
  assert.equal(d, 'M4 4h3v1h-3z');
  const two = grid((x, y) => y === 1 && (x === 0 || x === 2));
  assert.equal(qrPath(two, 0).d, 'M0 1h1v1h-1zM2 1h1v1h-1z');
  assert.equal(qrPath(grid(() => false)).d, '');
});

test('server shapes: pairing nests device, current is top-level, doctor uses name', () => {
  const p = normPairing({ device: { id: 'd1', name: 'phone' }, token: 't', url: 'http://h/?token=t', qr: '#.\n.#' });
  assert.equal(p.id, 'd1');
  assert.equal(p.name, 'phone');
  assert.equal(p.url, 'http://h/?token=t');
  const list = normDevices({ devices: [{ id: 'a', name: 'A' }, { id: 'b', name: 'B', last_seen: '2026-11-01T10:00:00Z' }], current: 'a' });
  assert.equal(list[0].id, 'a');
  assert.equal(list[0].current, true);
  assert.equal(list[1].current, false);
  assert.equal(normDevices({ devices: [{ id: 'a' }], current: 'admin' })[0].current, false);
  assert.equal(normDoctor({ checks: [{ id: 'tls', name: 'TLS', status: 'ok' }] })[0].title, 'TLS');
});
