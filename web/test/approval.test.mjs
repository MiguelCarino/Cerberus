// Approval prompts answered outside the terminal: which sessions wait at
// one, what they ask, and that a new prompt redraws a session row.
// Run: node --test web/test/*.test.mjs
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { approvalWait, approvalWaits } from '../dist/assets/js/util.js';
import { rowKey } from '../dist/assets/js/control.js';

const waiting = (o) => ({ ID: 's1', State: 'waiting', WaitingReason: 'approval', WaitingAsk: 'write_file: notes.txt', AskSeq: 3, ...o });

test('approvalWait: only a session waiting at an approval prompt', () => {
  assert.deepEqual(approvalWait(waiting()), { seq: 3, ask: 'write_file: notes.txt', tool: 'write_file', always: [] });
  assert.equal(approvalWait(waiting({ State: 'running' })), null);
  assert.equal(approvalWait(waiting({ WaitingReason: 'input needed' })), null);
  assert.equal(approvalWait(waiting({ WaitingReason: 'needs approval', State: 'queued' })), null);
  assert.equal(approvalWait(null), null);
  // An older agent (or an older daemon) says nothing about what it asks.
  assert.deepEqual(approvalWait(waiting({ WaitingAsk: undefined, AskSeq: undefined })), { seq: 0, ask: '', tool: '', always: [] });
  assert.equal(approvalWait(waiting({ WaitingAsk: 'run_command: a:b' })).tool, 'run_command');
});

test('approvalWaits: the approval waits, longest waiting first', () => {
  const list = [
    waiting({ ID: 'b', Updated: '2026-11-03T10:00:02Z' }),
    { ID: 'x', State: 'running' },
    waiting({ ID: 'a', Updated: '2026-11-03T10:00:01Z' }),
    waiting({ ID: 'c', WaitingReason: 'no output for 60s' }),
  ];
  assert.deepEqual(approvalWaits(list).map((s) => s.ID), ['a', 'b']);
  assert.deepEqual(approvalWaits(null), []);
});

test('rowKey changes with a new approval prompt', () => {
  assert.notEqual(rowKey(waiting()), rowKey(waiting({ AskSeq: 4 })));
  assert.notEqual(rowKey(waiting()), rowKey(waiting({ WaitingAsk: 'run_command: ls' })));
});
