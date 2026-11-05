// Pure helpers added by the day phase 7 fresh-install walkthrough.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { versionLabel, termInputHint, fmtEnergy, takesFollowUp } from '../dist/assets/js/util.js';
import { planErrTone } from '../dist/assets/js/spend.js';
import { emptyText } from '../dist/assets/js/leaderboard.js';

test('versionLabel prefixes v only for release numbers', () => {
  assert.equal(versionLabel('1.2.0'), 'v1.2.0');
  assert.equal(versionLabel('e0f264e-dirty'), 'e0f264e-dirty');
  assert.equal(versionLabel('dev'), 'dev');
  assert.equal(versionLabel(''), '');
  assert.equal(versionLabel(undefined), '');
});

test('termInputHint says what typing does in each state', () => {
  assert.match(termInputHint('running'), /Type and press Send/);
  assert.match(termInputHint('waiting'), /Type and press Send/);
  assert.match(termInputHint('passed'), /Follow-up/);
  assert.match(termInputHint('queued'), /^Queued/);
  assert.match(termInputHint('checking'), /^Busy/);
  assert.equal(termInputHint('accepted'), 'Session closed');
  assert.equal(termInputHint(undefined), 'Session closed');
  assert.match(termInputHint('passed', 'finetune-train'), /^No follow-ups/);
  assert.match(termInputHint('running', 'finetune-train'), /Type and press Send/);
});

test('takesFollowUp refuses internal fine-tuning runs', () => {
  assert.equal(takesFollowUp({ State: 'passed', Agent: 'claude' }), true);
  assert.equal(takesFollowUp({ State: 'passed', Agent: 'finetune-train' }), false);
  assert.equal(takesFollowUp({ State: 'running', Agent: 'claude' }), false);
  assert.equal(takesFollowUp(null), false);
});

test('planErrTone: missing transcripts on a fresh install are a note', () => {
  assert.equal(planErrTone('no Claude Code transcripts at /home/x/.claude/projects'), 'info');
  assert.equal(planErrTone('open /x: permission denied'), 'err');
  assert.equal(planErrTone(''), 'err');
});

test('leaderboard emptyText tells zero-cost requests from no requests', () => {
  assert.equal(emptyText({ rows: [], total: { requests: 0 } }), 'Nothing recorded in this period.');
  assert.equal(emptyText(null), 'Nothing recorded in this period.');
  assert.match(emptyText({ metric: 'cost', rows: [], total: { requests: 3 } }), /^3 requests .*local models cost energy/);
  assert.match(emptyText({ metric: 'time', rows: [], total: { requests: 1 } }), /^1 request in this period/);
});

test('fmtEnergy never reads "0.0 J" for a tiny non-zero amount', () => {
  assert.equal(fmtEnergy(0), '–');
  assert.equal(fmtEnergy(0.03), '<0.1 J');
  assert.equal(fmtEnergy(0.05), '0.1 J');
  assert.equal(fmtEnergy(250), '250 J');
  assert.equal(fmtEnergy(7200), '2.0 Wh');
});
