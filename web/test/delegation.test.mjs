// Words for delegated work and training examples.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { fmtTokens, fmtWh, fmtSaved, delegationText, finetuneText } from '../dist/assets/js/delegation.js';

test('number formats', () => {
  assert.equal(fmtTokens(950), '950');
  assert.equal(fmtTokens(12345), '12k');
  assert.equal(fmtTokens(1234), '1.2k');
  assert.equal(fmtTokens(2100000), '2.1M');
  assert.equal(fmtWh(0), '0 Wh');
  assert.equal(fmtWh(36), '<0.1 Wh');
  assert.equal(fmtWh(3600), '1.0 Wh');
  assert.equal(fmtWh(3600 * 1500), '1.50 kWh');
  assert.equal(fmtSaved(0), '');
  assert.equal(fmtSaved(0.004), '<$0.01');
  assert.equal(fmtSaved(1.234), '$1.23');
});

test('delegationText', () => {
  assert.equal(delegationText(null), '');
  assert.equal(delegationText({ requests: 0 }), '');
  const row = { requests: 4, prompt_tokens: 10000, completion_tokens: 2300, energy_j: 7200, saved_usd: 0.04 };
  assert.equal(delegationText(row, 'openrouter/claude'), '4 jobs to local models · 12k tokens · 2.0 Wh · ~$0.04 saved vs claude');
  assert.equal(delegationText({ requests: 2, prompt_tokens: 80, completion_tokens: 40, saved_usd: 0.00084 }, 'p/sonnet'),
    '2 jobs to local models · 120 tokens · under $0.01 saved vs sonnet');
  // no reference model: no money claim
  assert.equal(delegationText(row, ''), '4 jobs to local models · 12k tokens · 2.0 Wh');
  assert.equal(delegationText({ requests: 1, prompt_tokens: 5, completion_tokens: 5 }), '1 job to local models · 10 tokens');
});

test('finetuneText', () => {
  assert.match(finetuneText({ capture: 'off', stats: { examples: 0 } }), /Not collecting/);
  assert.equal(finetuneText({ capture: 'delegated', stats: { examples: 3, rated: 2, usable: 1 } }),
    'Collecting delegated jobs and their ratings. 3 examples, 2 rated, 1 ready to train on.');
  assert.match(finetuneText({ capture: 'cloud', stats: { examples: 1 } }), /cloud answers\. 1 example,/);
});
