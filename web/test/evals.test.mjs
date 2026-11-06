// Eval helpers (E1).
import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  cellMark, cellTone, cellText, effectiveGates, gateColumns, matrixRows, runTone, isFinalRun, canCancel, canRejudge,
  fmtWall, usageText, runTotals, gatesText, lastRunText, parseEvalJSON, editableJSON, TEMPLATE, evalRunEventFor, problemsText,
} from '../dist/assets/js/evals.js';
import { parseRoute } from '../dist/assets/js/util.js';
import { viewAllowed, TAB_OF } from '../dist/assets/js/users.js';

const ev = {
  gates: [{ id: 'build', name: 'Build', max_iterations: 1 }, { id: 'test', name: 'Test' }],
  criteria: [
    { id: 'a', gate: 'build' }, { id: 'b', gate: 'test' }, { id: 'c', gate: 'build', depends_on: ['a'] }, { id: 'd' },
  ],
  repeat: 2,
};

test('marks and tones', () => {
  assert.equal(cellMark('pass'), '✓');
  assert.equal(cellMark('fail'), '✗');
  assert.equal(cellMark('skipped'), '–');
  assert.equal(cellMark('error'), '!');
  assert.equal(cellMark(''), '·');
  assert.equal(cellTone('pass'), 'ok');
  assert.equal(cellTone('fail'), 'err');
  assert.equal(cellTone('skipped'), 'muted');
  assert.equal(cellText(undefined), 'not judged yet');
  assert.equal(runTone('done'), 'ok');
  assert.equal(runTone('running'), 'info');
});

test('gates and columns', () => {
  assert.deepEqual(effectiveGates({}), [{ id: 'main', name: 'Main', max_iterations: 1 }]);
  assert.equal(effectiveGates(ev)[1].max_iterations, 1);
  // A criterion without a gate belongs to the first one; definition order inside a gate.
  assert.deepEqual(gateColumns(ev), [
    { gate: 'build', name: 'Build', criteria: ['a', 'c', 'd'] },
    { gate: 'test', name: 'Test', criteria: ['b'] },
  ]);
  assert.deepEqual(gateColumns({ criteria: [{ id: 'x' }] }), [{ gate: 'main', name: 'Main', criteria: ['x'] }]);
});

test('matrixRows', () => {
  const detail = {
    run: {
      snapshot: ev,
      sessions: [
        { setup: 'qwen', repeat: 1, session: 's1', state: 'judged', live: 'passed' },
        { setup: 'qwen', repeat: 2, session: 's2', state: 'running' },
        { setup: 'claude', repeat: 1, session: '', state: 'error', error: 'did not start' },
      ],
    },
    reports: [{ session: 's1', results: [{ criterion: 'a', state: 'pass' }, { criterion: 'c', state: 'fail', rationale: 'no' }] }],
  };
  const rows = matrixRows(detail);
  assert.equal(rows.length, 3);
  assert.equal(rows[0].label, 'qwen #1');
  assert.equal(rows[0].cells.a.state, 'pass');
  assert.equal(rows[0].cells.c.result.rationale, 'no');
  assert.equal(rows[0].cells.b, undefined);
  assert.equal(rows[1].report, null);
  assert.equal(rows[2].error, 'did not start');
  assert.equal(matrixRows({ run: { sessions: [{ setup: 'x', repeat: 1 }] } })[0].label, 'x');
  assert.deepEqual(matrixRows(null), []);
});

test('run states', () => {
  assert.ok(isFinalRun('done') && isFinalRun('canceled') && isFinalRun('failed') && !isFinalRun('judging'));
  assert.ok(canCancel('running') && !canCancel('judging'));
  assert.ok(canRejudge({ state: 'done', sessions: [{ session: 's1' }] }));
  assert.ok(!canRejudge({ state: 'done', sessions: [{ session: '' }] }));
  assert.ok(!canRejudge({ state: 'running', sessions: [{ session: 's1' }] }));
});

test('formatting', () => {
  assert.equal(fmtWall(0), '–');
  assert.equal(fmtWall(1234), '1.2 s');
  assert.equal(fmtWall(42000), '42 s');
  assert.equal(fmtWall(125000), '2 min 5 s');
  assert.equal(fmtWall(120000), '2 min');
  assert.equal(usageText({ tokens: 1234, cost_usd: 0.01234, energy_wh: 1.5 }), '1,234 tokens · $0.0123 · 1.50 Wh');
  assert.equal(usageText({}), '');
  const t = runTotals({ run: { judge_usage: { tokens: 80 } }, reports: [
    { session_usage: { tokens: 100, energy_wh: 1 }, wall_ms: 5 }, { session_usage: { tokens: 50 }, wall_ms: 9 }] });
  assert.equal(t.session.tokens, 150);
  assert.equal(t.judge.tokens, 80);
  assert.equal(t.wallMs, 9);
  assert.equal(gatesText({ gates_passed: 1, gates_total: 2, first_failing_gate: 'test' }), '1/2 gates · stopped at test');
  assert.equal(lastRunText(null), 'never run');
  assert.equal(lastRunText({ state: 'done', judged: 2, sessions: 3 }), 'done · 2/3 judged');
});

test('editor helpers', () => {
  assert.equal(parseEvalJSON('{').ok, false);
  assert.equal(parseEvalJSON('[1]').ok, false);
  assert.deepEqual(parseEvalJSON('{"name":"x"}'), { ok: true, eval: { name: 'x' } });
  const shown = JSON.parse(editableJSON({ id: 'ev-x', version: 3, owner: 'u1', owner_name: 'ann', updated: 't', name: 'X' }));
  assert.deepEqual(shown, { id: 'ev-x', version: 3, name: 'X' });
  assert.ok(TEMPLATE.criteria.length >= 2 && TEMPLATE.setups.length >= 1);
  assert.equal(problemsText([{ severity: 'warning', field: 'a', message: 'w' }, { severity: 'error', field: '', message: 'e' }]), 'error: e\nwarning: a: w');
});

test('events and routes', () => {
  assert.ok(evalRunEventFor('evalrun', { run: 'er-1' }, 'er-1'));
  assert.ok(!evalRunEventFor('evalrun', { run: 'er-2' }, 'er-1'));
  assert.ok(evalRunEventFor('evalrun', { run: 'er-2' }, ''));
  assert.ok(evalRunEventFor('resync', { what: 'evals' }, 'er-1'));
  assert.ok(!evalRunEventFor('resync', { what: 'flows' }, 'er-1'));
  assert.ok(!evalRunEventFor('flowrun', { run: 'er-1' }, 'er-1'));
  assert.deepEqual(parseRoute('#/evals/runs/er-20261105-100000-abcdef'), { view: 'evalrun', run: 'er-20261105-100000-abcdef' });
  assert.deepEqual(parseRoute('#/evals'), { view: 'usage' });
  assert.equal(TAB_OF.evalrun, 'usage');
  const user = { role: 'user', caps: ['usage', 'sessions'] };
  const chat = { role: 'chat', caps: ['chat', 'usage'] };
  assert.ok(viewAllowed({ view: 'evalrun', run: 'x' }, user));
  assert.ok(!viewAllowed({ view: 'evalrun', run: 'x' }, chat));
});

test('trajectory links (E4)', async () => {
  const { trajectoryPath, trajectoryFile } = await import('../dist/assets/js/evals.js');
  assert.equal(trajectoryPath('s-1'), '/api/sessions/s-1/trajectory?format=atif');
  assert.equal(trajectoryPath('a b', 'jsonl'), '/api/sessions/a%20b/trajectory?format=jsonl');
  assert.equal(trajectoryPath('s', 'xml'), '/api/sessions/s/trajectory?format=atif');
  assert.equal(trajectoryFile('s-1'), 'cerberus-s-1.atif.json');
  assert.equal(trajectoryFile('../x', 'jsonl'), 'cerberus-.._x.records.jsonl');
  assert.equal(trajectoryFile(''), 'cerberus-session.atif.json');
});
