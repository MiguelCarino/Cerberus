// Unit tests for the chat view's reconnect, replay and keyboard helpers:
// a stream's hello (restart / gap detection), how a busy chat replays,
// which queued messages a replay leaves out, and the phone keyboard check.
// Run: node --test web/test/*.test.mjs
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { streamVerdict, replayPlan, preRunQueue } from '../dist/assets/js/chat.js';
import { keyboardOpen } from '../dist/assets/js/util.js';

test('streamVerdict: a restarted daemon or a gap reloads, a normal reconnect does not', () => {
  assert.equal(streamVerdict({ epoch: 'a', base: 0, next: 900 }, { epoch: 'a', want: 800 }), 'ok');
  assert.equal(streamVerdict({ epoch: 'b', base: 0, next: 3 }, { epoch: 'a', want: 800 }), 'reload', 'another epoch');
  assert.equal(streamVerdict({ epoch: 'a', base: 0, next: 3 }, { epoch: 'a', want: 800 }), 'reload', 'log behind the page');
  assert.equal(streamVerdict({ epoch: 'a', base: 5200, next: 10200 }, { epoch: 'a', want: 4000 }), 'reload', 'events trimmed');
  assert.equal(streamVerdict({ epoch: 'a', base: 0, next: 0 }, { epoch: '', want: 0 }), 'ok', 'a new chat');
  assert.equal(streamVerdict(null, { epoch: 'a', want: 1 }), 'ok');
});

test('replayPlan: replay the running message only when the log still holds its start', () => {
  assert.deepEqual(replayPlan({ status: { busy: true }, next: 40, base: 0, run_from: 30 }), { replay: true, from: 30, until: 40 });
  assert.deepEqual(replayPlan({ status: { busy: true }, next: 9000, base: 4000, run_from: 1200 }), { replay: false, from: 9000, until: 0 },
    'the run started before the log base: show the stored messages');
  assert.deepEqual(replayPlan({ status: { busy: false }, next: 12, base: 0, run_from: -1 }), { replay: false, from: 12, until: 0 });
  assert.deepEqual(replayPlan({ status: { busy: true }, next: 5, base: 0, run_from: -1 }), { replay: false, from: 5, until: 0 });
  assert.deepEqual(replayPlan(null), { replay: false, from: 0, until: 0 });
});

test('preRunQueue: messages queued before the running one started are not in the replay', () => {
  const replayed = [{ type: 'user', text: 'B' }, { type: 'text' }, { type: 'queued', text: 'D' }];
  assert.deepEqual(preRunQueue(['C', 'D'], replayed), ['C']);
  assert.deepEqual(preRunQueue(['D'], replayed), []);
  assert.deepEqual(preRunQueue([], replayed), []);
  assert.deepEqual(preRunQueue(['C'], []), ['C']);
  assert.deepEqual(preRunQueue(undefined, null), []);
});

test('keyboardOpen: a big visual-viewport shrink while typing, not a pinch zoom', () => {
  assert.equal(keyboardOpen(740, 450, 1, true), true);
  assert.equal(keyboardOpen(740, 700, 1, true), false, 'a URL bar, not a keyboard');
  assert.equal(keyboardOpen(740, 450, 1, false), false, 'not typing');
  assert.equal(keyboardOpen(740, 370, 2, true), false, 'zoomed in');
  assert.equal(keyboardOpen(0, 0), false);
});
