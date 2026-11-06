// Request records helpers (gateway phase G3).
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { recordsQuery, recordURL, summarize, toolCounts, switches, cacheShare } from '../dist/assets/js/records.js';

test('recordsQuery and recordURL', () => {
  assert.equal(recordsQuery({ day: '2026-11-05', client: 'codex' }), '/api/records?day=2026-11-05&client=codex&limit=200');
  assert.equal(recordsQuery(), '/api/records?limit=200');
  assert.equal(recordURL('2026-11-05/123-456/789'), '/api/records/2026-11-05/123-456/789');
  assert.equal(recordURL('../../etc/passwd'), null);
  assert.equal(recordURL(undefined), null);
});

test('summarize groups by client', () => {
  const lines = [
    { client: 'codex', prompt_tokens: 100, cached_tokens: 50, calls: [{ name: 'shell' }], results: 2, result_errors: 1, finish: 'tool_calls' },
    { client: 'codex', prompt_tokens: 300, cached_tokens: 250, results: 0, finish: 'stop' },
    { client: 'claude-code', prompt_tokens: 0, finish: 'length_in_thinking' },
  ];
  const s = summarize(lines);
  assert.equal(s.clients[0].client, 'codex');
  assert.equal(s.clients[0].requests, 2);
  assert.equal(s.clients[0].avgPrompt, 200);
  assert.equal(s.clients[0].cachePct, 75);
  assert.equal(s.clients[0].errorPct, 50);
  assert.equal(s.clients[1].cachePct, null);
  assert.deepEqual(s.finishes, { tool_calls: 1, stop: 1, length_in_thinking: 1 });
  assert.deepEqual(summarize([]), { clients: [], finishes: {} });
});

test('toolCounts, switches, cacheShare', () => {
  assert.deepEqual(toolCounts([{ calls: [{ name: 'b' }, { name: 'a' }] }, { calls: [{ name: 'b' }] }]),
    [{ name: 'b', n: 2 }, { name: 'a', n: 1 }]);
  assert.equal(switches({ thinking: false, effort: 'low', sampled: false, schema: true, images: 1, budget: 64 }),
    'thinking off · effort low · greedy · schema · 1 image · budget 64');
  assert.equal(switches({ sampled: true }), 'sampled');
  assert.equal(cacheShare({ prompt_tokens: 10, cached_tokens: 9 }), 90);
  assert.equal(cacheShare({}), null);
});

test('guardText', async () => {
  const { guardText } = await import('../dist/assets/js/records.js');
  assert.equal(guardText(null), '');
  assert.equal(guardText({ mode: 'off' }), '');
  assert.equal(guardText({ mode: 'hold', model: 'qg', screened: 9, flagged: 2, held: 1, timed_out: 1 }),
    'Agent guard (hold, qg): 9 tool results read · 2 flagged · 1 calls held · 1 not in time since start.');
  assert.equal(guardText({ mode: 'watch', model: 'qg', screened: 1 }), 'Agent guard (watch, qg): 1 tool results read · 0 flagged since start.');
});

test('guardbenchText (E5 red-team last run)', async () => {
  const { guardbenchText } = await import('../dist/assets/js/records.js');
  assert.equal(guardbenchText(null), '');
  assert.equal(guardbenchText({ state: 'running' }), '');
  const s = guardbenchText({ state: 'done', model: 'qg', detection: 0.92, false_alarm: 0.08, attacks: 36, benign: 25, done: '2026-11-05T10:00:00Z' });
  assert.match(s, /Red-team \(qg,/);
  assert.match(s, /92% of 36 attacks caught/);
  assert.match(s, /8% of 25 benign flagged/);
  const e = guardbenchText({ state: 'done', model: 'qg', detection: 1, false_alarm: 0, attacks: 36, benign: 25, errors: 3 });
  assert.match(e, /3 errored/);
});
