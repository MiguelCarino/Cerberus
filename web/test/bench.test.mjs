// Bench helpers (gateway phase G6).
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { runSummary, fmtRate, benchBody } from '../dist/assets/js/bench.js';

test('runSummary', () => {
  const s = runSummary({
    id: 'r1', state: 'done', time: '2026-11-05T10:00:00Z', options: { model: 'qwen', label: 'cap 300' }, config: { draft: '/m/d.gguf' },
    kinds: [
      { kind: 'code', median: { decode_tps: 60, ttft_ms: 200, tok_per_j: 1.5 } },
      { kind: 'chat', median: { decode_tps: 80, ttft_ms: 100, tok_per_j: 2.5 }, errors: 1 },
      { kind: 'prefill_8k', median: { prefill_tps: 1800 } },
    ],
  });
  assert.equal(s.decode, 70);
  assert.equal(s.ttft, 150);
  assert.equal(s.prefill, 1800);
  assert.equal(s.tokJ, 2);
  assert.equal(s.errors, 1);
  assert.equal(s.draft, true);
  assert.equal(s.model, 'qwen');
  assert.equal(runSummary({ id: 'x' }).decode, 0);
});

test('fmtRate and benchBody', () => {
  assert.equal(fmtRate(0), '–');
  assert.equal(fmtRate(73.25), '73.3 tok/s');
  assert.equal(fmtRate(1821.4), '1821 tok/s');
  assert.deepEqual(benchBody({ model: ' qwen ', label: '  ', repeat: '5', prefill: true }), { model: 'qwen', repeat: 5, prefill: true });
  assert.deepEqual(benchBody({ model: 'm', repeat: 99 }), { model: 'm' });
});
