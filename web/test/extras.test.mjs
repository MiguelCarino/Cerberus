// Unit tests for the day phase 6 web helpers: the inbox (events, grouping
// by day, safe routes, the desktop gate), the asciicast replay maths, the
// Get models box and progress, the leaderboard bars and the replay route.
// Run: node --test web/test/*.test.mjs
import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  normItem, normInbox, applyEvent, markRead, dayKey, dayLabel, groupByDay, clockTime, safeRoute, badgeText,
  shouldNotify, keepStreamWhenHidden, levelTone, sortItems, normAsks,
} from '../dist/assets/js/inbox.js';
import {
  SPEEDS, IDLE_CAP, parseCast, capIdle, lastIndexAtOrBefore, framesUpTo, activityBins, nextDelay, fmtClock,
  replayHash, parseReplayTime, normRuns,
} from '../dist/assets/js/cast.js';
import {
  classifyRef, normDownloads, sortDownloads, mergeDownload, fmtETA, progressPct, progressText, isFinal, engineLabel,
} from '../dist/assets/js/downloads.js';
import { normChoice, leaderboardQuery, barRows, metricText, fmtWall, rowLink, totalText } from '../dist/assets/js/leaderboard.js';
import { parseRoute, snapshotLoader } from '../dist/assets/js/util.js';

// ---------- inbox ----------

const item = (id, time, extra = {}) => ({ ID: id, Time: time, Kind: 'session', Level: 'info', Title: `t${id}`, Body: '', Link: `#/s/${id}`, Read: false, ...extra });

test('normInbox accepts a list or {items, unread} and sorts newest first', () => {
  const a = normInbox([item('a', '2026-11-01T10:00:00Z'), item('b', '2026-11-01T12:00:00Z', { Read: true }), { nope: 1 }, null]);
  assert.deepEqual(a.items.map((i) => i.ID), ['b', 'a']);
  assert.equal(a.unread, 1);
  const b = normInbox({ items: [item('a', '2026-11-01T10:00:00Z')], unread: 7 });
  assert.equal(b.unread, 7);
  assert.deepEqual(normInbox(null), { items: [], unread: 0 });
  assert.equal(normItem({ ID: 'x', Level: 'bogus' }).Level, 'info');
  assert.equal(levelTone('crit'), 'err');
  assert.equal(levelTone('warn'), 'warn');
  assert.equal(levelTone('?'), 'info');
});

test('applyEvent adds, marks read and deletes without mutating', () => {
  const s0 = normInbox([item('a', '2026-11-01T10:00:00Z')]);
  const s1 = applyEvent(s0, { Type: 'added', Item: item('b', '2026-11-01T11:00:00Z'), Unread: 2 });
  assert.deepEqual(s1.items.map((i) => i.ID), ['b', 'a']);
  assert.equal(s1.unread, 2);
  assert.equal(s0.items.length, 1, 'state not mutated');
  // A duplicate add replaces.
  const s1b = applyEvent(s1, { Type: 'added', Item: item('b', '2026-11-01T11:00:00Z', { Title: 'new' }) });
  assert.equal(s1b.items.length, 2);
  assert.equal(s1b.items[0].Title, 'new');
  const s2 = applyEvent(s1, { Type: 'read', IDs: ['a'], Unread: 1 });
  assert.equal(s2.items.find((i) => i.ID === 'a').Read, true);
  assert.equal(s2.unread, 1);
  const s3 = applyEvent(s2, { Type: 'read', All: true, Unread: 0 });
  assert.ok(s3.items.every((i) => i.Read));
  const s4 = applyEvent(s3, { Type: 'deleted', IDs: ['b'], Unread: 0 });
  assert.deepEqual(s4.items.map((i) => i.ID), ['a']);
  // Without Unread the count is derived.
  const s5 = markRead(s1, ['b']);
  assert.equal(s5.unread, 1);
  assert.equal(applyEvent(s1, { Type: 'weird' }), s1);
  assert.equal(applyEvent(s1, null), s1);
  // keep caps the ring.
  let s = { items: [], unread: 0 };
  for (let i = 0; i < 5; i++) s = applyEvent(s, { Type: 'added', Item: item(`i${i}`, new Date(Date.UTC(2026, 10, 1, i)).toISOString()) }, 3);
  assert.deepEqual(s.items.map((i) => i.ID), ['i4', 'i3', 'i2']);
});

test('groupByDay labels Today, Yesterday and older days, newest first', () => {
  const now = new Date(2026, 10, 1, 15, 0).getTime(); // local 1 Nov 2026
  const items = [
    item('a', new Date(2026, 10, 1, 9, 5).toISOString()),
    item('b', new Date(2026, 9, 31, 23, 59).toISOString()),
    item('c', new Date(2026, 9, 27, 8, 0).toISOString()),
    item('d', new Date(2026, 10, 1, 14, 0).toISOString()),
    item('e', new Date(2025, 11, 24, 8, 0).toISOString()),
  ];
  const g = groupByDay(items, now);
  assert.deepEqual(g.map((x) => x.label), ['Today', 'Yesterday', 'Tue 27 Oct', 'Wed 24 Dec 2025']);
  assert.deepEqual(g[0].items.map((i) => i.ID), ['d', 'a']);
  assert.equal(dayKey(new Date(2026, 0, 5, 1).getTime()), '2026-01-05');
  assert.equal(dayKey('garbage'), '');
  assert.equal(dayLabel('', now), 'Earlier');
  assert.equal(clockTime(new Date(2026, 10, 1, 9, 5).toISOString()), '09:05');
  assert.equal(clockTime('x'), '');
  assert.deepEqual(sortItems([item('a', 'x'), item('b', 'x')]).map((i) => i.ID), ['b', 'a']);
});

test('safeRoute only allows in-app hash routes', () => {
  assert.equal(safeRoute('#/s/abc'), '#/s/abc');
  assert.equal(safeRoute(' #/models '), '#/models');
  for (const bad of ['https://evil.example/', 'javascript:alert(1)', '/api/status', '#x', '#/a b', '', null, 42, '//evil']) {
    assert.equal(safeRoute(bad), null, String(bad));
  }
});

test('badge text and the desktop notification gate', () => {
  assert.equal(badgeText(0), '');
  assert.equal(badgeText(-1), '');
  assert.equal(badgeText(NaN), '');
  assert.equal(badgeText(5), '5');
  assert.equal(badgeText(120), '99+');
  const it = { Read: false };
  assert.equal(shouldNotify({ optedIn: true, permission: 'granted', hidden: true, item: it }), true);
  assert.equal(shouldNotify({ optedIn: true, permission: 'granted', hidden: false, item: it }), false, 'visible tab: no desktop notification');
  assert.equal(shouldNotify({ optedIn: false, permission: 'granted', hidden: true, item: it }), false);
  assert.equal(shouldNotify({ optedIn: true, permission: 'default', hidden: true, item: it }), false);
  assert.equal(shouldNotify({ optedIn: true, permission: 'granted', hidden: true, item: { Read: true } }), false);
  assert.equal(keepStreamWhenHidden(true, 'granted'), true);
  assert.equal(keepStreamWhenHidden(true, 'denied'), false);
  assert.equal(keepStreamWhenHidden(false, 'granted'), false);
});

// ---------- replay ----------

const CAST = [
  JSON.stringify({ version: 2, width: 100, height: 30, timestamp: 1767225600, title: 'demo' }),
  '[0.5, "o", "hello "]',
  '[0.7, "i", "secret input"]',
  '[1.0, "o", "world\\r\\n"]',
  'not json',
  '[11.0, "o", "after a long pause"]',
  '[11.5, "m", "recording stopped: size limit"]',
  '[12.0, "o", "end"]',
  '[12.5, "o", ',
].join('\n');

test('parseCast keeps output, markers and the header; skips input and junk', () => {
  const c = parseCast(CAST);
  assert.equal(c.header.width, 100);
  assert.equal(c.header.height, 30);
  assert.equal(c.header.title, 'demo');
  assert.deepEqual(c.frames.map((f) => f.data), ['hello ', 'world\r\n', 'after a long pause', 'end']);
  assert.ok(!c.frames.some((f) => f.data.includes('secret')), 'input events are never replayed');
  assert.deepEqual(c.markers, [{ t: 11.5, label: 'recording stopped: size limit' }]);
  assert.equal(c.duration, 12);
  assert.equal(parseCast(''), null);
  assert.equal(parseCast('{"version":1}\n'), null);
  assert.equal(parseCast('nope'), null);
  // A time going backwards is clamped.
  const back = parseCast('{"version":2,"width":80,"height":24}\n[2,"o","a"]\n[1,"o","b"]');
  assert.deepEqual(back.frames.map((f) => f.t), [2, 2]);
  // Defaults for a sparse header.
  assert.deepEqual(parseCast('{"version":2}').header, { width: 80, height: 24, timestamp: 0, title: '' });
});

test('capIdle shortens long gaps to the cap and maps times', () => {
  assert.equal(IDLE_CAP, 2);
  const c = parseCast(CAST);
  const { times, map, duration } = capIdle(c.frames);
  // 1.0 → 11.0 is a 10 s gap: capped at 2 s.
  assert.deepEqual(times, [0.5, 1.0, 3.0, 4.0]);
  assert.equal(duration, 4.0);
  assert.equal(map(0.5), 0.5);
  assert.equal(map(5), 3.0, 'inside a long gap, time holds at the cap');
  assert.equal(map(11.5), 3.5);
  assert.equal(map(0.2), 0.2);
  // A leading gap is capped too.
  assert.deepEqual(capIdle([{ t: 30, data: 'x' }]).times, [2]);
  assert.equal(capIdle([]).duration, 0);
});

test('seek helpers: binary search, joined output, delays, activity', () => {
  assert.equal(lastIndexAtOrBefore([0.5, 1, 3, 4], 0.1), -1);
  assert.equal(lastIndexAtOrBefore([0.5, 1, 3, 4], 1), 1);
  assert.equal(lastIndexAtOrBefore([0.5, 1, 3, 4], 3.9), 2);
  assert.equal(lastIndexAtOrBefore([0.5, 1, 3, 4], 99), 3);
  assert.equal(lastIndexAtOrBefore([], 1), -1);
  const frames = [{ t: 0, data: 'a' }, { t: 1, data: 'bb' }, { t: 2, data: 'ccc' }];
  assert.equal(framesUpTo(frames, -1), '');
  assert.equal(framesUpTo(frames, 1), 'abb');
  assert.equal(framesUpTo(frames, 9), 'abbccc');
  assert.equal(nextDelay([0.5, 1, 3], -1, 1), 500);
  assert.equal(nextDelay([0.5, 1, 3], 1, 4), 500);
  assert.equal(nextDelay([0.5, 1, 3], 2, 1), -1);
  const bins = activityBins([0, 1, 2], frames, 2, 4);
  assert.equal(bins.length, 4);
  assert.equal(Math.max(...bins), 1);
  assert.equal(bins[3], 1, 'the last frame falls in the last bin');
  assert.deepEqual(activityBins([], [], 0, 3), [0, 0, 0]);
  assert.deepEqual(SPEEDS, [1, 2, 4, 16]);
});

test('clock, replay links and their parsing round-trip', () => {
  assert.equal(fmtClock(0), '0:00');
  assert.equal(fmtClock(83.6), '1:23');
  assert.equal(fmtClock(3723), '1:02:03');
  assert.equal(fmtClock(-4), '0:00');
  assert.equal(replayHash('a b', 3, 83.56), '#/s/a%20b/replay/3/83.6');
  assert.equal(replayHash('x', 2, 0), '#/s/x/replay/2');
  assert.equal(replayHash('x', 0, 5), '#/s/x/replay');
  assert.equal(parseReplayTime('83.5'), 83.5);
  assert.equal(parseReplayTime('1:23.5'), 83.5);
  assert.equal(parseReplayTime('1:02:03'), 3723);
  for (const bad of ['', null, 'x', '-1', '1:2:3:4', '1e3']) assert.equal(parseReplayTime(bad), 0, String(bad));
  assert.deepEqual(parseRoute('#/s/x/replay/3/83.6'), { view: 'session', id: 'x', tab: 'replay', attempt: 3, t: 83.6 });
  assert.deepEqual(parseRoute('#/s/x/replay'), { view: 'session', id: 'x', tab: 'replay', attempt: 0, t: 0 });
  assert.deepEqual(parseRoute('#/s/x/replay/zz/5'), { view: 'session', id: 'x', tab: 'replay', attempt: 0, t: 0 });
  assert.deepEqual(parseRoute(replayHash('a b', 2, 10)), { view: 'session', id: 'a b', tab: 'replay', attempt: 2, t: 10 });
  assert.deepEqual(parseRoute('#/s/x/files'), { view: 'session', id: 'x', tab: 'files' });
});

test('normRuns accepts both shapes, newest attempt first', () => {
  const r = normRuns([{ attempt: 1, size: 10, modified: 'm', recording: false }, { Attempt: 2, Size: 5, Recording: true }, { attempt: 0 }]);
  assert.deepEqual(r.map((x) => x.attempt), [2, 1]);
  assert.equal(r[0].recording, true);
  assert.deepEqual(normRuns({ runs: [{ attempt: 4 }] }).map((x) => x.attempt), [4]);
  assert.deepEqual(normRuns(null), []);
});

// ---------- downloads ----------

test('classifyRef routes HF links, Ollama names and LM Studio queries', () => {
  assert.deepEqual(classifyRef(''), { engine: '', ref: '', kind: 'empty', error: '' });
  const hf = classifyRef('https://huggingface.co/Qwen/Qwen2.5-7B-Instruct-GGUF/resolve/main/qwen2.5-7b-instruct-q4_k_m.gguf');
  assert.equal(hf.engine, 'llamacpp');
  assert.equal(hf.kind, 'hf');
  assert.equal(hf.error, '');
  assert.equal(classifyRef('hf.co/a/b/resolve/main/x.gguf').ref, 'https://hf.co/a/b/resolve/main/x.gguf');
  assert.deepEqual(classifyRef('https://huggingface.co/Qwen/Qwen2.5-7B-Instruct-GGUF'),
    { engine: 'lmstudio', ref: 'https://huggingface.co/Qwen/Qwen2.5-7B-Instruct-GGUF', kind: 'hfrepo', error: '' });
  assert.equal(classifyRef('huggingface.co/unsloth/X-GGUF/tree/main').ref, 'https://huggingface.co/unsloth/X-GGUF');
  assert.equal(classifyRef('lms https://huggingface.co/unsloth/X-GGUF').engine, 'lmstudio');
  assert.match(classifyRef('https://huggingface.co/a/b/resolve/main/x.bin').error, /\.gguf/);
  assert.match(classifyRef('http://huggingface.co/a/b/x.gguf').error, /https/);
  assert.match(classifyRef('https://example.com/x.gguf').error, /Hugging Face/);
  assert.deepEqual(classifyRef('qwen2.5-coder:7b'), { engine: 'ollama', ref: 'qwen2.5-coder:7b', kind: 'ollama', error: '' });
  assert.equal(classifyRef('ollama llama3.2').engine, 'ollama');
  assert.equal(classifyRef('ollama: llama3.2').ref, 'llama3.2');
  assert.match(classifyRef('ollama Bad Name!').error, /Ollama/);
  // A bare name is left to the server (LM Studio when lms exists, else Ollama).
  assert.deepEqual(classifyRef('qwen 2.5 coder 7b'), { engine: '', ref: 'qwen 2.5 coder 7b', kind: 'auto', error: '' });
  assert.deepEqual(classifyRef('llama3.2'), { engine: '', ref: 'llama3.2', kind: 'auto', error: '' });
  assert.deepEqual(classifyRef('qwen3'), { engine: '', ref: 'qwen3', kind: 'auto', error: '' });
  assert.equal(classifyRef('lms: gemma 3').engine, 'lmstudio');
  assert.equal(classifyRef('hf Qwen/x/resolve/main/y.gguf').ref, 'https://huggingface.co/Qwen/x/resolve/main/y.gguf');
  assert.equal(engineLabel('llamacpp'), 'llama.cpp');
});

const dl = (id, state, extra = {}) => ({ id, state, engine: 'ollama', ref: id, name: id, bytes: 0, total: 0, percent: 0, rate: 0, eta_sec: -1, created: '2026-11-01T10:00:00Z', ...extra });

test('downloads: order, merge, finality', () => {
  const list = sortDownloads([
    dl('done1', 'done', { ended: '2026-11-01T11:00:00Z' }), dl('q2', 'queued', { created: '2026-11-01T10:05:00Z' }),
    dl('run', 'running'), dl('q1', 'queued', { created: '2026-11-01T10:01:00Z' }), dl('done2', 'failed', { ended: '2026-11-01T12:00:00Z' }),
  ]);
  assert.deepEqual(list.map((x) => x.id), ['run', 'q1', 'q2', 'done2', 'done1']);
  const merged = mergeDownload(list, dl('q1', 'running'));
  assert.deepEqual(merged.slice(0, 2).map((x) => x.id).sort(), ['q1', 'run']);
  assert.equal(merged.length, 5);
  assert.equal(mergeDownload(list, null), list);
  assert.ok(isFinal(dl('x', 'canceled')) && !isFinal(dl('x', 'verifying')));
  assert.deepEqual(normDownloads({ items: [dl('a', 'queued'), {}], engines: { ollama: true } }).engines, { ollama: true });
  assert.equal(normDownloads([dl('a', 'queued')]).items.length, 1);
  assert.deepEqual(normDownloads(null), { items: [], engines: {} });
});

test('download progress text and ETA', () => {
  assert.equal(fmtETA(-1), '');
  assert.equal(fmtETA(0.2), '1 s');
  assert.equal(fmtETA(45), '45 s');
  assert.equal(fmtETA(180), '3 min');
  assert.equal(fmtETA(3900), '1 h 05 min');
  assert.equal(progressPct(dl('a', 'running')), null);
  assert.equal(progressPct(dl('a', 'running', { bytes: 50, total: 200 })), 25);
  assert.equal(progressPct(dl('a', 'running', { percent: 40 })), 40);
  assert.equal(progressPct(dl('a', 'done')), 100);
  const t = progressText(dl('a', 'running', { bytes: 1e9, total: 4e9, rate: 25e6, eta_sec: 120, status: 'pulling' }));
  assert.equal(t, '25% · 1.0 GB of 4.0 GB · 25.0 MB/s · 2 min left · pulling');
  assert.equal(progressText(dl('a', 'running')), 'Starting…');
  assert.equal(progressText(dl('a', 'queued')), 'Queued');
  assert.match(progressText(dl('a', 'done', { total: 4e9, verified: true })), /^Done · 4\.0 GB · SHA-256 verified · not loaded$/);
  assert.equal(progressText(dl('a', 'failed', { error: 'checksum mismatch' })), 'Failed: checksum mismatch');
  assert.equal(progressText(dl('a', 'canceled')), 'Canceled');
});

// ---------- leaderboard ----------

const BOARD = {
  period: '7d', by: 'session', metric: 'cost', cost_currency: 'USD', currency: 'MXN',
  rows: [
    { key: 's2', label: 'refactor', value: 0.5, cost_usd: 0.5, requests: 3, tokens: 100, energy_kwh: 0.01, energy_cost: 0.05, wall_sec: 30 },
    { key: 's1', label: 'fix tests', value: 2, cost_usd: 2, requests: 10, tokens: 1000, energy_kwh: 0.2, energy_cost: 0.8, wall_sec: 4000, sessions: 1 },
    { key: '', label: '(direct API)', value: 0, cost_usd: 0, requests: 1 },
  ],
  total: { value: 2.5, cost_usd: 2.5, requests: 14 },
};

test('barRows ranks largest first with bars relative to the top', () => {
  const rows = barRows(BOARD);
  assert.deepEqual(rows.map((r) => r.label), ['fix tests', 'refactor', '(direct API)']);
  assert.deepEqual(rows.map((r) => r.rank), [1, 2, 3]);
  assert.deepEqual(rows.map((r) => r.pct), [100, 25, 0]);
  assert.equal(rows[0].main, '$2.00');
  assert.equal(rows[0].sub, '10 req');
  assert.equal(rows[0].href, '#/s/s1');
  assert.equal(rows[2].href, '');
  assert.equal(totalText(BOARD), '$2.50');
  assert.deepEqual(barRows({}), []);
  assert.deepEqual(barRows({ metric: 'cost', rows: [{ key: 'a', value: 0 }] }).map((r) => r.pct), [0]);
});

test('metric formatting and choices', () => {
  const row = BOARD.rows[1];
  assert.equal(metricText(row, 'tokens').main, '1.0k');
  assert.equal(metricText(row, 'energy', BOARD).main, '200 Wh');
  assert.equal(metricText(row, 'energy', BOARD).sub, '$0.80 MXN');
  assert.equal(metricText(row, 'time').main, '1.1 h');
  assert.equal(metricText(row, 'time').sub, '1 session');
  assert.equal(metricText({ value: 30, cost_usd: 1.5 }, 'cost', { cost_currency: 'MXN' }).main, '$30.00 MXN');
  assert.equal(fmtWall(0), '0 s');
  assert.equal(fmtWall(45), '45 s');
  assert.equal(fmtWall(720), '12 min');
  assert.deepEqual(normChoice({ period: 'year', by: 'repo', metric: 'energy' }), { period: '7d', by: 'repo', metric: 'energy' });
  assert.equal(leaderboardQuery({ period: 'today', by: 'agent', metric: 'time' }), '/api/leaderboard?period=today&by=agent&metric=time&limit=20');
  assert.equal(rowLink('model', { key: 'x' }), '');
  assert.equal(rowLink('session', { key: '(direct API)' }), '');
});

// ---------- snapshot + stream ----------

// deferred is a promise resolved from outside.
function deferred() {
  let resolve;
  const p = new Promise((r) => { resolve = r; });
  return { p, resolve };
}

test('snapshotLoader re-applies events that arrive while the snapshot is in flight', async () => {
  const replies = [];
  let state = [];
  const l = snapshotLoader({
    fetch: () => { const d = deferred(); replies.push(d); return d.p; },
    onSnapshot: (snap, events) => { state = events.reduce(mergeDownload, normDownloads(snap).items); },
  });
  const p = l.load();
  // A final event lands before the (older) reply.
  const done = dl('a', 'done', { bytes: 10, total: 10 });
  l.track(done);
  replies[0].resolve([dl('a', 'running', { bytes: 5, total: 10 })]);
  await p;
  assert.equal(state[0].state, 'done');
  // Outside a load, track keeps nothing.
  l.track(dl('a', 'running'));
  const p2 = l.load();
  replies[1].resolve([dl('a', 'done')]);
  await p2;
  assert.equal(state[0].state, 'done');
});

test('snapshotLoader runs again when a resync comes during a load', async () => {
  const replies = [];
  let snaps = 0;
  const l = snapshotLoader({
    fetch: () => { const d = deferred(); replies.push(d); return d.p; },
    onSnapshot: () => { snaps++; },
  });
  const p = l.load();
  const p2 = l.load(); // resync while in flight
  assert.equal(p, p2);
  assert.equal(replies.length, 1);
  replies[0].resolve({});
  await new Promise((r) => setTimeout(r, 0));
  assert.equal(replies.length, 2, 'a second fetch after the first');
  replies[1].resolve({});
  await p;
  assert.equal(snaps, 2);
  assert.equal(l.busy(), false);
  // A failed fetch reaches onError and frees the loader.
  let err = null;
  const bad = snapshotLoader({ fetch: async () => { throw new Error('x'); }, onSnapshot: () => {}, onError: (e) => { err = e; } });
  await bad.load();
  assert.equal(err.message, 'x');
  assert.equal(bad.busy(), false);
});

test('inbox events during a load survive a stale snapshot', async () => {
  let state = { items: [], unread: 0 };
  const d = deferred();
  const l = snapshotLoader({
    fetch: () => d.p,
    onSnapshot: (body, events) => { let n = normInbox(body); for (const ev of events) n = applyEvent(n, ev); state = n; },
  });
  const p = l.load();
  l.track({ Type: 'added', Item: { ID: 'n2', Kind: 'session', Title: 'new', Time: '2026-11-01T10:05:00Z' }, Unread: 2 });
  d.resolve({ items: [{ ID: 'n1', Kind: 'session', Title: 'old', Time: '2026-11-01T10:00:00Z' }], unread: 1 });
  await p;
  assert.deepEqual(state.items.map((i) => i.ID).sort(), ['n1', 'n2']);
  assert.equal(state.unread, 2);
});

test('flowApprovalRun finds the run of a flow approval item only', async () => {
  const { flowApprovalRun } = await import('../dist/assets/js/inbox.js');
  assert.equal(flowApprovalRun({ Kind: 'flow-approve', Link: '#/flows/runs/fr-20261102-214830-chue6d' }), 'fr-20261102-214830-chue6d');
  assert.equal(flowApprovalRun({ Kind: 'flow', Link: '#/flows/runs/fr-1' }), '');
  assert.equal(flowApprovalRun({ Kind: 'flow-approve', Link: '#/flows/fl-x' }), '');
  assert.equal(flowApprovalRun({ Kind: 'flow-approve', Link: 'https://evil/#/flows/runs/fr-1' }), '');
  assert.equal(flowApprovalRun({ Kind: 'flow-approve', Link: '#/flows/runs/a/b' }), '');
  assert.equal(flowApprovalRun(null), '');
});

test('normAsks keeps well-formed open chat permission requests, keyed chat/req', () => {
  assert.deepEqual(normAsks(null), []);
  assert.deepEqual(normAsks({}), []);
  const req = { id: 'r1', tool: 'Bash', input: { command: 'ls' } };
  const out = normAsks([
    { chat: 'c1', title: 'Fix the build', model: 'qwen', req },
    { chat: '', req },
    { chat: 'c2', req: {} },
    { chat: 'c3', req: { id: 7 } },
    null,
    { chat: 'c4', req: { id: 'r2' } },
  ]);
  assert.equal(out.length, 2);
  assert.deepEqual(out[0], { key: 'c1/r1', chat: 'c1', title: 'Fix the build', model: 'qwen', req });
  assert.equal(out[1].key, 'c4/r2');
  assert.equal(out[1].title, 'LLM chat');
  assert.equal(out[1].model, '');
});
