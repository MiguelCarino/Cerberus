// Evals E2 in the web UI: the flow editor's judge step kind (flows.js)
// and the eval run page's iterations and feedback helpers (evals.js).
import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  STEP_KINDS, KIND_FIELDS, stepFields, newStep, cleanStep, stepSummary, draftGraph, renameStepRefs,
  parseListField, listFieldText, JUDGE_CRITERIA_EXAMPLE,
} from '../dist/assets/js/flows.js';
import { iterationsText, cellHistory, feedbackLabel } from '../dist/assets/js/evals.js';

test('judge is a step kind after session, with its fields', () => {
  const kinds = STEP_KINDS.map((k) => k.kind);
  assert.equal(kinds.indexOf('judge'), kinds.indexOf('session') + 1);
  assert.deepEqual(KIND_FIELDS.judge, ['on', 'criteria', 'gates', 'persona', 'model']);
});

test('a new judge step judges the last session step', () => {
  const work = newStep('session', [], { agent: 'cerberus' });
  const p = newStep('prompt', [work]);
  const j = newStep('judge', [work, p]);
  assert.equal(j.kind, 'judge');
  assert.equal(j.id, 'judge');
  assert.equal(j.on, work.id);
  assert.deepEqual(j.criteria, JSON.parse(JUDGE_CRITERIA_EXAMPLE));
  // its "on" choices are the earlier session steps only
  const on = stepFields(j, [work, p, j]).find((f) => f.key === 'on');
  assert.deepEqual(on.options.map(([v]) => v), [work.id]);
  assert.equal(stepFields(j, [j]).find((f) => f.key === 'on').hint, 'Add an Agent session step before this one.');
  // an if after a judge branches on its verdict
  const iff = newStep('if', [work, j]);
  assert.equal(iff.on, j.id);
  assert.equal(iff.test, 'equals');
  assert.equal(iff.path, 'passed');
  assert.equal(iff.value, 'true');
});

test('an equals test shows an optional path; other tests drop it on save', () => {
  const vis = (s) => stepFields(s, [s]).filter((f) => f.show !== false).map((f) => f.key);
  assert.ok(vis({ kind: 'if', test: 'equals' }).includes('path'));
  assert.ok(!vis({ kind: 'if', test: 'contains' }).includes('path'));
  assert.equal(cleanStep({ id: 'c', kind: 'if', test: 'equals', on: 'j', path: 'passed', value: 'true' }).path, 'passed');
  assert.equal(cleanStep({ id: 'c', kind: 'if', test: 'contains', on: 'j', path: 'passed', value: 'x' }).path, undefined);
});

test('parseListField reads criteria and gates lists', () => {
  assert.deepEqual(parseListField(''), { ok: true, value: [] });
  assert.deepEqual(parseListField(' [{"id":"a","prompt":"p"}] ').value, [{ id: 'a', prompt: 'p' }]);
  assert.equal(parseListField('{"id":"a"}').ok, false);
  assert.match(parseListField('[1]').error, /object/);
  assert.match(parseListField('[{').error, /^Not JSON/);
  assert.equal(listFieldText([]), '');
  assert.equal(listFieldText(undefined), '');
  assert.deepEqual(JSON.parse(listFieldText([{ id: 'a' }])), [{ id: 'a' }]);
});

test('cleanStep keeps a judge step\'s own fields', () => {
  const s = { id: 'judge', kind: 'judge', name: '', on: 'work', criteria: [{ id: 'a', prompt: 'p' }], gates: [],
    persona: '', model: '', task: 'stray', agent: 'x' };
  assert.deepEqual(cleanStep(s), { id: 'judge', kind: 'judge', on: 'work', criteria: [{ id: 'a', prompt: 'p' }] });
  assert.equal(cleanStep({ ...s, criteria: 'not a list' }).criteria, undefined);
  const g = cleanStep({ ...s, gates: [{ id: 'main', max_iterations: 2 }], persona: 'strict', model: 'judge-m' });
  assert.deepEqual(g.gates, [{ id: 'main', max_iterations: 2 }]);
  assert.equal(g.persona, 'strict');
  assert.equal(g.model, 'judge-m');
});

test('judge steps in the summary, the preview graph and renames', () => {
  const steps = [
    { id: 'work', kind: 'session', agent: 'cerberus', task: 'do it' },
    { id: 'check', kind: 'judge', on: 'work', model: 'jm', criteria: [{ id: 'a', prompt: 'It builds' }, { id: 'b', prompt: 'Tests pass' }],
      gates: [{ id: 'g1' }, { id: 'g2' }] },
  ];
  assert.equal(stepSummary(steps[1]), 'check · jm · It builds; Tests pass');
  const g = draftGraph({ steps });
  const node = g.nodes.find((n) => n.id === 'st:check');
  assert.equal(node.sub, 'judge · on work · 2 criteria · 2 gates · jm');
  assert.ok(g.edges.some((e) => e.from === 'st:check' && e.to === 'm:jm' && e.kind === 'ask'));
  const renamed = renameStepRefs(steps, 'work', 'build');
  assert.equal(renamed[1].on, 'build');
});

test('iterationsText names the gates that took more than one iteration', () => {
  assert.equal(iterationsText(null), '');
  assert.equal(iterationsText({ gates: [{ gate: 'build', iterations: 1 }] }), '');
  assert.equal(iterationsText({ gates: [{ gate: 'build', iterations: 2 }, { gate: 'test', iterations: 3 }] }),
    'build: 2 iterations · test: 3 iterations');
});

test('cellHistory and feedbackLabel', () => {
  const rep = { history: [{ criterion: 'a', iteration: 1, state: 'fail' }, { criterion: 'b', iteration: 1, state: 'pass' }] };
  assert.deepEqual(cellHistory(rep, 'a'), [{ criterion: 'a', iteration: 1, state: 'fail' }]);
  assert.deepEqual(cellHistory(null, 'a'), []);
  assert.equal(feedbackLabel({ gate: 'build', iteration: 1, persona: '', sent: true }), 'build · after iteration 1 · reviewer · sent');
  assert.equal(feedbackLabel({ gate: 'build', iteration: 2, persona: 'strict', fallback: true, sent: false }),
    'build · after iteration 2 · persona strict · plain text (the model did not write it) · not sent');
  assert.equal(feedbackLabel(null), '');
});
