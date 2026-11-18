// Approvals holding a flow run: the pure helpers of the run page
// (web/dist/assets/js/flowasks.js) over GET /api/flows/runs/{run} → asks.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  normAsks, askSession, askKey, asksKey, asksFor, asksElsewhere, askWhat, askWhere, askLine,
} from '../dist/assets/js/flowasks.js';
import { approvalWait } from '../dist/assets/js/util.js';

const tool = {
  session: 's1', name: 'Site · Assemble', kind: 'tool', step: 'build', ask: 'run_command: npm test', seq: 3,
  always: [{ Text: 'Bash(npm test)', Tool: 'Bash', Content: 'npm test' }], since: '2026-11-16T06:52:00Z',
};
const spawn = { session: 'c1', name: 'helper', kind: 'spawn', step: 'build', via: 's1', seq: 0, ask: '' };

test('normAsks keeps well-formed asks and fills the gaps', () => {
  assert.deepEqual(normAsks(null), []);
  const got = normAsks([tool, spawn, { kind: 'tool' }, { session: 'x', kind: 'other' }, null]);
  assert.equal(got.length, 2);
  assert.deepEqual(got[0], { ...tool, item: null, via: '' });
  assert.equal(got[1].item, null);
  assert.deepEqual(got[1].always, []);
  assert.equal(normAsks([{ session: 's2', kind: 'tool', item: 1 }])[0].item, 1);
  assert.equal(normAsks([{ session: 's2', kind: 'tool' }])[0].name, 's2');
});

test('askSession is what the approval buttons answer', () => {
  const [a] = normAsks([tool]);
  const s = askSession(a);
  assert.deepEqual(approvalWait(s), { seq: 3, ask: 'run_command: npm test', tool: 'run_command', always: tool.always });
  assert.equal(s.ID, 's1');
  assert.equal(askSession(normAsks([spawn])[0]), null);
  assert.equal(askSession(null), null);
});

test('keys change with a new prompt only', () => {
  const [a] = normAsks([tool]);
  assert.equal(askKey(a), askKey({ ...a, since: 'later' }));
  assert.notEqual(askKey(a), askKey({ ...a, seq: 4 }));
  assert.equal(asksKey([]), '');
  assert.notEqual(asksKey([a]), asksKey([a, normAsks([spawn])[0]]));
});

test('asksFor and asksElsewhere place asks on steps and items', () => {
  const list = normAsks([tool, spawn, { session: 'i', kind: 'tool', step: 'parts', item: 2 }, { session: 'z', kind: 'tool' }]);
  assert.deepEqual(asksFor(list, 'build').map((a) => a.session), ['s1', 'c1']);
  assert.deepEqual(asksFor(list, 'parts', 2).map((a) => a.session), ['i']);
  assert.deepEqual(asksFor(list, 'parts', 1), []);
  assert.deepEqual(asksElsewhere(list, ['build']).map((a) => a.session), ['i', 'z']);
});

test('askWhat, askWhere and askLine say it in words', () => {
  const [a, c] = normAsks([tool, spawn]);
  assert.equal(askWhat(a), 'run_command: npm test');
  assert.equal(askWhat({ ...a, ask: '' }), 'An approval (open the session to see what for)');
  assert.equal(askWhat(c), 'helper asks to start');
  const steps = [{ id: 'build', name: 'Assemble' }];
  assert.equal(askWhere(a, steps), 'Assemble');
  assert.equal(askWhere(c, steps), 'Assemble · a session it started');
  assert.equal(askWhere({ ...a, step: 'parts', item: 1 }, [], [{ index: 1, title: 'Style' }]), 'parts · Style');
  assert.equal(askWhere({ ...a, step: 'parts', item: 4 }, [], []), 'parts · item 5');
  assert.equal(askLine([]), '');
  assert.equal(askLine([a]), 'waiting for your approval: run_command: npm test');
  assert.equal(askLine([a, c]), 'waiting for your approval (2)');
});
