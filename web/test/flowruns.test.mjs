// Flows list: a flow with its runs below it (web/dist/assets/js/flowruns.js):
// grouping a per_flow runs list by flow, deleted flows' runs, which runs a
// card shows, the toggle's words, Stop / Run again by state, the input on
// one line, the last input for Run and when a run ran.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  PREVIEW, groupRuns, runTotal, shownRuns, toggleText, needsFetch, runActions, inputLine, lastInput, runWhen,
} from '../dist/assets/js/flowruns.js';

const run = (id, flow, extra = {}) => ({ id, flow, flow_name: flow.toUpperCase(), state: 'passed', input: '', ...extra });

test('groupRuns puts each run under its flow, order kept', () => {
  const flows = [{ id: 'a' }, { id: 'b' }, { id: 'c' }];
  const runs = [run('r5', 'a'), run('r4', 'b'), run('r3', 'a'), run('r2', 'gone'), run('r1', 'old', { flow_name: '' })];
  const { byFlow, orphans } = groupRuns(flows, runs);
  assert.deepEqual(byFlow.get('a').map((r) => r.id), ['r5', 'r3']);
  assert.deepEqual(byFlow.get('b').map((r) => r.id), ['r4']);
  assert.deepEqual(byFlow.get('c'), [], 'a flow with no runs has an empty list');
  // Runs of flows not listed: one group each, newest group first, named.
  assert.deepEqual(orphans.map((g) => [g.flow, g.name, g.runs.map((r) => r.id)]),
    [['gone', 'GONE', ['r2']], ['old', 'old', ['r1']]]);
});

test('groupRuns copes with nothing and with junk', () => {
  const { byFlow, orphans } = groupRuns([], [null, { id: 'x' }]);
  assert.equal(byFlow.size, 0);
  assert.deepEqual(orphans, []);
  assert.deepEqual(groupRuns(undefined, undefined).orphans, []);
});

test('runTotal takes the server count, never fewer than in hand', () => {
  assert.equal(runTotal([]), 0);
  assert.equal(runTotal([run('1', 'a', { flow_runs: 12 }), run('2', 'a', { flow_runs: 12 })]), 12);
  assert.equal(runTotal([run('1', 'a'), run('2', 'a')]), 2);
  assert.equal(runTotal([run('1', 'a', { flow_runs: 1 }), run('2', 'a', { flow_runs: 1 })]), 2);
});

test('shownRuns shows the newest few unless opened; toggleText words', () => {
  const rs = Array.from({ length: 8 }, (_, i) => run(`r${i}`, 'a'));
  assert.equal(PREVIEW, 5);
  assert.deepEqual(shownRuns(rs).map((r) => r.id), ['r0', 'r1', 'r2', 'r3', 'r4']);
  assert.equal(shownRuns(rs, true).length, 8);
  assert.equal(shownRuns(rs.slice(0, 2)).length, 2);
  assert.equal(toggleText(8, false), 'Show all 8 runs');
  assert.equal(toggleText(8, true), 'Show fewer');
  assert.equal(toggleText(5, false), '', 'nothing hidden, no toggle');
  assert.equal(toggleText(0, true), '');
});

test('needsFetch: only an opened card missing runs reads the rest', () => {
  const five = Array.from({ length: 5 }, (_, i) => run(`r${i}`, 'a', { flow_runs: 9 }));
  assert.equal(needsFetch(five, false), false);
  assert.equal(needsFetch(five, true), true);
  const all = Array.from({ length: 9 }, (_, i) => run(`r${i}`, 'a', { flow_runs: 9 }));
  assert.equal(needsFetch(all, true), false);
});

test('runActions: Stop while it goes, Run again while its flow exists', () => {
  for (const state of ['queued', 'running', 'waiting']) {
    assert.deepEqual(runActions(run('r', 'a', { state })), { stop: true, again: true }, state);
  }
  for (const state of ['passed', 'failed', 'rejected', 'limited', 'canceled']) {
    assert.deepEqual(runActions(run('r', 'a', { state })), { stop: false, again: true }, state);
  }
  assert.deepEqual(runActions(run('r', 'a', { state: 'running' }), { flowExists: false }), { stop: true, again: false });
  assert.deepEqual(runActions(run('r', 'a', { test: true })), { stop: false, again: false }, 'a draft test run is not run again');
  assert.deepEqual(runActions(null), { stop: false, again: false });
});

test('inputLine folds an input to one line', () => {
  assert.equal(inputLine('  Build a page\n\nwith  a form\t '), 'Build a page with a form');
  assert.equal(inputLine(''), '');
  assert.equal(inputLine(undefined), '');
});

test('lastInput is the newest real run input', () => {
  assert.equal(lastInput([run('1', 'a'), run('2', 'a', { input: 'test one', test: true }), run('3', 'a', { input: 'site for Bob' }), run('4', 'a', { input: 'older' })]), 'site for Bob');
  assert.equal(lastInput([run('1', 'a', { input: '   ' })]), '');
  assert.equal(lastInput([]), '');
});

test('runWhen: start, end and how long', () => {
  const zero = '0001-01-01T00:00:00Z';
  const done = runWhen({ created: '2026-11-16T10:00:00Z', started: '2026-11-16T10:00:01Z', ended: '2026-11-16T10:02:06Z' });
  assert.deepEqual(done, { start: '2026-11-16T10:00:00Z', end: '2026-11-16T10:02:06Z', took: '2 min 05 s' });
  const going = runWhen({ created: '2026-11-16T10:00:00Z', started: '2026-11-16T10:00:00Z', ended: zero }, Date.parse('2026-11-16T10:00:30Z'));
  assert.deepEqual(going, { start: '2026-11-16T10:00:00Z', end: '', took: '30 s' });
  const queued = runWhen({ created: '2026-11-16T10:00:00Z', started: zero, ended: zero });
  assert.deepEqual(queued, { start: '2026-11-16T10:00:00Z', end: '', took: '' });
  assert.deepEqual(runWhen(null), { start: '', end: '', took: '' });
});
