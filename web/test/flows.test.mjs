// Flows views: the pure helpers in web/dist/assets/js/flows.js (trigger
// text, the schedule picker's model, the step forms' model, problems on
// fields, run states and usage, the preview graph) and the #/flows routes.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  parseSchedule, buildSchedule, describeSchedule, triggerText, cronFor, parseDays, parseClock, nextText,
  STEP_KINDS, KIND_FIELDS, stepFields, newStep, uniqueId, duplicateStep, moveStep, removeStep, renameStepRefs,
  placeholdersFor, cleanStep, cleanFlow, blankFlow, fromTemplate, limitsText, stepSummary, clip,
  indexProblems, problemsAt, problemSummary, problemText,
  isFinalRun, canCancelRun, canContinueRun, runTone, stateText, fmtDuration, runDuration, usageText, stepUsage, outputText,
  stepRows, sessionLinks, draftGraph, previewChip, runEventFor, isZeroTime,
  selectDefault, setStepField, rowKey, itemKey, runLog, runLogName, retriedText,
} from '../dist/assets/js/flows.js';
import { parseRoute } from '../dist/assets/js/util.js';
import { layoutGraph } from '../dist/assets/js/flowgraph.js';

// ---------- routes ----------

test('parseRoute knows the Flows routes', () => {
  assert.deepEqual(parseRoute('#/flows'), { view: 'flows' });
  assert.deepEqual(parseRoute('#/flows/'), { view: 'flows' });
  assert.deepEqual(parseRoute('#/flows/new'), { view: 'flowedit', id: 'new' });
  assert.deepEqual(parseRoute('#/flows/new/website'), { view: 'flowedit', id: 'new', template: 'website' });
  assert.deepEqual(parseRoute('#/flows/fl-morning'), { view: 'flowedit', id: 'fl-morning' });
  assert.deepEqual(parseRoute('#/flows/runs/fr-20261102-080000-ab12'), { view: 'flowrun', run: 'fr-20261102-080000-ab12' });
  assert.deepEqual(parseRoute('#/flows/runs'), { view: 'flows' });
  // Sessions and the rest are unchanged.
  assert.deepEqual(parseRoute('#/sessions'), { view: 'sessions' });
  // Unknown routes are the Not found view (users and roles, phase 4a).
  assert.deepEqual(parseRoute('#/nope'), { view: 'notfound', path: '#/nope' });
});

// ---------- schedules and triggers ----------

test('trigger text: manual, friendly forms and cron, like the server', () => {
  assert.equal(triggerText({ kind: 'manual' }), 'Manual');
  assert.equal(triggerText(null), 'Manual');
  assert.equal(triggerText({ kind: 'schedule', schedule: 'weekdays 08:00' }), 'Weekdays at 08:00');
  assert.equal(triggerText({ kind: 'schedule', schedule: 'Daily 8:05' }), 'Daily at 08:05');
  assert.equal(triggerText({ kind: 'schedule', schedule: 'weekends 10:00' }), 'Weekends at 10:00');
  assert.equal(triggerText({ kind: 'schedule', schedule: 'thu,mon 19:30' }), 'Mon, Thu at 19:30');
  assert.equal(triggerText({ kind: 'schedule', schedule: 'sun,sat 09:00' }), 'Sat, Sun at 09:00');
  assert.equal(triggerText({ kind: 'schedule', schedule: 'mon-fri 07:15' }), 'Mon, Tue, Wed, Thu, Fri at 07:15');
  assert.equal(triggerText({ kind: 'schedule', schedule: 'every 6h' }), 'Every 6 hours');
  assert.equal(triggerText({ kind: 'schedule', schedule: 'every 60m' }), 'Every hour');
  assert.equal(triggerText({ kind: 'schedule', schedule: 'every 1h' }), 'Every hour');
  assert.equal(triggerText({ kind: 'schedule', schedule: 'every 30m' }), 'Every 30 minutes');
  assert.equal(triggerText({ kind: 'schedule', schedule: 'every 1m' }), 'Every minute');
  assert.equal(triggerText({ kind: 'schedule', schedule: '0  8 * * 1-5' }), 'Cron 0 8 * * 1-5');
  // Not a schedule: the raw text (the server flags it), or a placeholder.
  assert.equal(triggerText({ kind: 'schedule', schedule: 'sometimes' }), 'sometimes');
  assert.equal(triggerText({ kind: 'schedule', schedule: '' }), 'Schedule (not set)');
  assert.equal(describeSchedule('every 25h'), '');
  assert.equal(describeSchedule('daily 24:00'), '');
});

test('the picker reads and writes every friendly form', () => {
  for (const s of ['every 6h', 'every 30m', 'daily 08:00', 'weekdays 08:00', 'weekends 10:30', 'mon,thu 19:30', '0 8 * * 1-5']) {
    const m = parseSchedule(s);
    assert.equal(m.ok, true, s);
    assert.equal(buildSchedule(m), s, s);
  }
  assert.deepEqual(parseSchedule('every 6h'), { mode: 'every', n: 6, unit: 'h', at: '08:00', days: [1], cron: '', ok: true });
  assert.equal(parseSchedule('mon-wed 07:00').mode, 'days');
  assert.deepEqual(parseSchedule('mon-wed 07:00').days, [1, 2, 3]);
  // Sunday written last, Monday first.
  assert.equal(buildSchedule({ mode: 'days', days: [0, 1, 4], at: '9:05' }), 'mon,thu,sun 09:05');
  assert.equal(buildSchedule({ mode: 'days', days: [], at: '09:00' }), '');
  assert.equal(buildSchedule({ mode: 'every', n: 0, unit: 'm' }), 'every 1m');
  assert.equal(buildSchedule({ mode: 'daily', at: 'bad' }), 'daily 08:00');
  assert.equal(buildSchedule({ mode: 'cron', cron: '  */15   * * * * ' }), '*/15 * * * *');
  // Text the picker cannot show stays in the cron field.
  const odd = parseSchedule('Lunchtime');
  assert.equal(odd.mode, 'cron');
  assert.equal(odd.cron, 'lunchtime');
  assert.equal(odd.ok, false);
  assert.equal(parseSchedule('').ok, false);
  assert.equal(parseSchedule('funday 08:00').mode, 'cron');
});

test('days, clock and cron lines for the picker', () => {
  assert.deepEqual(parseDays('mon,thu'), [1, 4]);
  assert.deepEqual(parseDays('sat,sun'), [0, 6]);
  assert.equal(parseDays('fri-mon'), null);
  // A range may close on Sunday, as the server reads it.
  assert.deepEqual(parseDays('fri-sun'), [0, 5, 6]);
  assert.deepEqual(parseDays('mon-sun'), [0, 1, 2, 3, 4, 5, 6]);
  assert.deepEqual(parseDays('sun-tue'), [0, 1, 2]);
  const wrap = parseSchedule('fri-sun 10:00');
  assert.equal(wrap.mode, 'days');
  assert.equal(wrap.ok, true);
  assert.deepEqual(wrap.days, [0, 5, 6]);
  assert.equal(describeSchedule('fri-sun 10:00'), 'Fri, Sat, Sun at 10:00');
  assert.equal(parseDays('mon-tue-wed'), null);
  assert.equal(parseDays(''), null);
  assert.equal(parseClock('8:00'), 480);
  assert.equal(parseClock('23:59'), 1439);
  assert.equal(parseClock('24:00'), -1);
  assert.equal(parseClock('8:0'), -1);
  assert.equal(cronFor({ mode: 'weekdays', at: '08:30' }), '30 8 * * 1-5');
  assert.equal(cronFor({ mode: 'weekends', at: '10:00' }), '0 10 * * 0,6');
  assert.equal(cronFor({ mode: 'days', at: '19:30', days: [4, 1] }), '30 19 * * 1,4');
  assert.equal(cronFor({ mode: 'every', n: 15, unit: 'm' }), '*/15 * * * *');
  assert.equal(cronFor({ mode: 'every', n: 6, unit: 'h' }), '0 */6 * * *');
  assert.equal(cronFor({ mode: 'daily', at: '07:05' }), '5 7 * * *');
  for (const c of ['30 8 * * 1-5', '0 10 * * 0,6', '5 7 * * *']) assert.equal(parseSchedule(c).ok, true);
});

test('next run in words, in local time', () => {
  const now = new Date(2026, 10, 2, 9, 0); // Mon 2 Nov 2026, 09:00 local
  const at = (d, h, m) => new Date(2026, 10, d, h, m).toISOString();
  assert.equal(nextText(at(2, 18, 0), now), 'today 18:00');
  assert.equal(nextText(at(3, 8, 0), now), 'tomorrow 08:00');
  assert.equal(nextText(at(5, 8, 0), now), 'Thu 08:00');
  assert.equal(nextText(at(12, 8, 0), now), '12 Nov 08:00');
  assert.equal(nextText(null, now), '');
  assert.equal(nextText('0001-01-01T00:00:00Z', now), '');
});

// ---------- steps ----------

test('every kind has a label, its fields and a form', () => {
  for (const k of STEP_KINDS) {
    assert.ok(KIND_FIELDS[k.kind], k.kind);
    const s = newStep(k.kind, [newStep('plan', [])]);
    const fields = stepFields(s, [newStep('plan', []), s]);
    assert.ok(fields.length > 0, k.kind);
    for (const f of fields) assert.ok(KIND_FIELDS[k.kind].includes(f.key), `${k.kind}.${f.key} is a kind field`);
  }
  assert.deepEqual(stepFields({ kind: 'nope' }, []), []);
});

test('step forms show the fields the other values ask for', () => {
  const vis = (s, steps = [s]) => stepFields(s, steps).filter((f) => f.show !== false).map((f) => f.key);
  assert.deepEqual(vis({ kind: 'session', workspace: 'scratch' }), ['agent', 'workspace', 'task']);
  assert.deepEqual(vis({ kind: 'session', workspace: 'repo' }), ['agent', 'workspace', 'repo', 'task']);
  assert.deepEqual(vis({ kind: 'session', workspace: 'folder' }), ['agent', 'workspace', 'folder', 'task']);
  assert.ok(!vis({ kind: 'fan_out', as: 'prompt' }).includes('agent'));
  assert.ok(vis({ kind: 'fan_out', as: 'session' }).includes('agent'));
  assert.ok(vis({ kind: 'if', test: 'json' }).includes('path'));
  assert.ok(!vis({ kind: 'if', test: 'contains' }).includes('path'));
  assert.ok(vis({ kind: 'output', to: 'file' }).includes('file'));
  assert.ok(!vis({ kind: 'output', to: 'inbox' }).includes('file'));
  const state = stepFields({ kind: 'if', test: 'state' }, []).find((f) => f.key === 'value');
  assert.equal(state.type, 'select');
  assert.deepEqual(state.options.map(([v]) => v), ['passed', 'failed', 'stopped', 'error']);
});

test('step pickers offer earlier steps; if jumps offer later ones and end', () => {
  const plan = { id: 'plan', kind: 'plan' };
  const ask = { id: 'ask', kind: 'prompt', json: true };
  const txt = { id: 'txt', kind: 'prompt' };
  const fan = { id: 'fan', kind: 'fan_out' };
  const chk = { id: 'chk', kind: 'if' };
  const done = { id: 'done', kind: 'output' };
  const steps = [plan, ask, txt, fan, chk, done];
  const from = stepFields(fan, steps).find((f) => f.key === 'from');
  assert.deepEqual(from.options.map(([v]) => v), ['plan', 'ask']); // plans and JSON prompts only
  const on = stepFields(chk, steps).find((f) => f.key === 'on');
  assert.deepEqual(on.options.map(([v]) => v), ['plan', 'ask', 'txt', 'fan']);
  const then = stepFields(chk, steps).find((f) => f.key === 'then');
  assert.deepEqual(then.options.map(([v]) => v), ['', 'done', 'end']);
  const lonely = stepFields({ id: 'f', kind: 'fan_out' }, [{ id: 'f', kind: 'fan_out' }]).find((f) => f.key === 'from');
  assert.match(lonely.hint, /Plan step/);
});

test('new steps get unique ids and read the step before them', () => {
  assert.equal(uniqueId('plan', []), 'plan');
  assert.equal(uniqueId('plan', ['plan', 'plan-2']), 'plan-3');
  assert.equal(uniqueId('End', []), 'end-step');
  assert.equal(uniqueId('42 Weird Name!', []), 'weird-name');
  assert.equal(uniqueId('', []), 'step');
  assert.ok(uniqueId('x'.repeat(60), []).length <= 32);
  const first = newStep('prompt', [], { model: 'qwen3' });
  assert.deepEqual(first, { id: 'prompt', kind: 'prompt', name: '', model: 'qwen3', template: '{input}' });
  const plan = newStep('plan', [first]);
  assert.equal(plan.template, 'Split this job into parts: {steps.prompt.output}');
  const fan = newStep('fan_out', [first, plan]);
  assert.equal(fan.from, 'plan');
  assert.equal(fan.id, 'fan');
  const sess = newStep('session', [first], { agent: 'coder' });
  assert.equal(sess.agent, 'coder');
  assert.equal(sess.task, '{steps.prompt.output}');
  const chk = newStep('if', [first]);
  assert.equal(chk.on, 'prompt');
  assert.equal(chk.else, 'end');
  assert.equal(newStep('output', []).to, 'inbox');
  assert.equal(newStep('prompt', [first]).id, 'prompt-2');
});

test('duplicate, move and delete return new lists', () => {
  const a = { id: 'a', kind: 'prompt', name: 'Ask', template: 'x' };
  const b = { id: 'b', kind: 'output', to: 'inbox', template: 'y' };
  const steps = [a, b];
  const d = duplicateStep(steps, 0);
  assert.deepEqual(d.map((s) => s.id), ['a', 'a-copy', 'b']);
  assert.equal(d[1].name, 'Ask (copy)');
  assert.notEqual(d[1], a);
  assert.equal(duplicateStep(steps, 5), steps);
  assert.deepEqual(moveStep(steps, 0, 1).map((s) => s.id), ['b', 'a']);
  assert.equal(moveStep(steps, 0, -1), steps);
  assert.equal(moveStep(steps, 1, 1), steps);
  assert.deepEqual(removeStep(steps, 0).map((s) => s.id), ['b']);
  assert.equal(removeStep(steps, 9), steps);
  assert.deepEqual(steps.map((s) => s.id), ['a', 'b'], 'the input is untouched');
});

test('renaming a step id follows its references', () => {
  const steps = [
    { id: 'plan', kind: 'plan', template: '{input}' },
    { id: 'fan', kind: 'fan_out', from: 'plan', template: '{item.task} after {steps.plan.output}' },
    { id: 'chk', kind: 'if', on: 'plan', then: 'out', else: 'end' },
    { id: 'out', kind: 'output', template: '{steps.plan.output} and {steps.planner.output}' },
  ];
  const r = renameStepRefs(steps, 'plan', 'outline');
  assert.equal(r[0].id, 'outline');
  assert.equal(r[1].from, 'outline');
  assert.equal(r[1].template, '{item.task} after {steps.outline.output}');
  assert.equal(r[2].on, 'outline');
  assert.equal(r[3].template, '{steps.outline.output} and {steps.planner.output}', 'only the exact id');
  const r2 = renameStepRefs(r, 'out', 'done');
  assert.equal(r2[2].then, 'done');
  assert.equal(renameStepRefs(steps, 'plan', 'plan'), steps);
});

test('placeholders a step can use', () => {
  const steps = [{ id: 'a' }, { id: 'b' }, { id: 'c' }];
  assert.deepEqual(placeholdersFor(steps, 0), ['{input}', '{date}', '{time}', '{flow.name}']);
  assert.deepEqual(placeholdersFor(steps, 2), ['{input}', '{steps.a.output}', '{steps.b.output}', '{date}', '{time}', '{flow.name}']);
  assert.ok(placeholdersFor(steps, 1, true).includes('{item.task}'));
});

test('cleanStep keeps only its kind fields, without empties', () => {
  assert.deepEqual(cleanStep({ id: ' a ', kind: 'prompt', name: '  ', model: 'm', template: 't', json: false, task: 'stale', routes: [] }),
    { id: 'a', kind: 'prompt', model: 'm', template: 't' });
  assert.deepEqual(cleanStep({ id: 'p', kind: 'plan', name: 'Plan', model: 'm', template: 't', max_items: '6' }),
    { id: 'p', kind: 'plan', name: 'Plan', model: 'm', template: 't', max_items: 6 });
  assert.deepEqual(cleanStep({ id: 'f', kind: 'fan_out', from: 'p', routes: [{ kind: ' html ', model: 'q' }, { kind: '', model: '' }], max_parallel: 0, template: '{item.task}' }),
    { id: 'f', kind: 'fan_out', from: 'p', routes: [{ kind: 'html', model: 'q' }], template: '{item.task}' });
});

test('cleanFlow is what the API takes', () => {
  const d = {
    id: 'fl-x', name: ' Brief ', description: 'd', enabled: true, input_hint: 'h', version: 3,
    trigger: { kind: 'schedule', schedule: ' weekdays 08:00 ' },
    limits: { max_steps: 10, max_cost_usd: 0.5, timeout_min: 0 },
    steps: [{ id: 's', kind: 'approve', message: 'ok?', model: 'stale' }],
    updated: '2026-11-02T00:00:00Z', trigger_text: 'x',
  };
  assert.deepEqual(cleanFlow(d), {
    id: 'fl-x', name: 'Brief', description: 'd', enabled: true, input_hint: 'h', version: 3,
    trigger: { kind: 'schedule', schedule: 'weekdays 08:00' },
    limits: { max_steps: 10, max_cost_usd: 0.5 },
    steps: [{ id: 's', kind: 'approve', message: 'ok?' }],
  });
  const m = cleanFlow({ name: 'n', trigger: { kind: 'manual', schedule: 'daily 08:00' }, steps: [] });
  assert.deepEqual(m.trigger, { kind: 'manual' });
  assert.equal('id' in m, false);
  assert.equal(m.version, 0);
});

test('blank flows and templates as drafts', () => {
  const b = blankFlow({ model: 'q' });
  assert.equal(b.steps.length, 1);
  assert.equal(b.steps[0].model, 'q');
  assert.equal(b.version, 0);
  const t = { key: 'w', name: 'Website', flow: { id: 'fl-w', name: 'Website', trigger: { kind: 'manual' }, steps: [{ id: 'plan', kind: 'plan' }], version: 0 } };
  const d = fromTemplate(t);
  assert.equal(d.name, 'Website');
  assert.equal('id' in d, false);
  assert.equal(d.steps.length, 1);
  d.steps.push({});
  assert.equal(t.flow.steps.length, 1, 'the template is copied');
  t.flow.enabled = false; // Go sends enabled:false on templates
  assert.equal(fromTemplate(t).enabled, true, 'a draft from a template starts enabled');
});

test('collapsed lines: limits and step summaries', () => {
  assert.match(limitsText({}), /^Defaults: 50 steps/);
  assert.equal(limitsText({ max_steps: 10, max_cost_usd: 0.5 }), 'max steps 10 · max cost (usd) 0.5');
  assert.equal(stepSummary({ id: 'a', model: 'q', template: 'Hello\n  world' }), 'a · q · Hello world');
  assert.equal(clip('abcdef', 4), 'abc…');
  assert.equal(clip('abc', 4), 'abc');
});

// ---------- problems ----------

test('problems land on their fields', () => {
  const ps = [
    { step: '', field: 'name', message: 'give the flow a name', severity: 'error' },
    { step: 'plan', field: 'model', message: 'not installed now', severity: 'warning' },
    { step: 'fan', field: 'routes.0.kind', message: 'route 1 needs a kind', severity: 'error' },
    { step: 'fan', field: 'id', message: 'bad id', severity: 'error' },
    { step: 'fan', field: '', message: 'whole step', severity: 'error' },
    { step: 'fan', field: 'mystery', message: 'unknown field', severity: 'error' },
  ];
  const idx = indexProblems(ps);
  assert.equal(idx.errors, 5);
  assert.equal(idx.warnings, 1);
  assert.deepEqual(problemsAt(idx, '', 'name').map((p) => p.message), ['give the flow a name']);
  assert.deepEqual(problemsAt(idx, 'plan', 'model').map((p) => p.severity), ['warning']);
  assert.deepEqual(problemsAt(idx, 'fan', 'routes').map((p) => p.message), ['route 1 needs a kind']);
  // The card's own slot: the whole step and fields the form does not show.
  assert.deepEqual(problemsAt(idx, 'fan', '', ['id', 'routes']).map((p) => p.message), ['whole step', 'unknown field']);
  assert.deepEqual(problemsAt(idx, 'nope', 'model'), []);
  assert.equal(problemSummary(idx), '5 problems to fix · 1 warning');
  assert.equal(problemSummary(indexProblems([ps[1]])), '1 warning');
  assert.equal(problemSummary(indexProblems([])), 'No problems');
  assert.equal(problemText(ps[2]), 'Step fan · routes.0.kind: route 1 needs a kind');
  assert.equal(problemText({ step: '', field: '', message: 'add at least one step' }), 'add at least one step');
  assert.equal(indexProblems(null).errors, 0);
});

// ---------- runs ----------

test('run states: final, tones and words', () => {
  for (const s of ['passed', 'failed', 'rejected', 'limited', 'canceled']) assert.equal(isFinalRun(s), true, s);
  for (const s of ['queued', 'running', 'waiting']) { assert.equal(isFinalRun(s), false, s); assert.equal(canCancelRun(s), true, s); }
  assert.equal(canCancelRun(''), false);
  const snap = { steps: [{ id: 'a' }, { id: 'b' }] };
  for (const st of ['failed', 'limited', 'canceled']) assert.equal(canContinueRun({ state: st, pc: 1, snapshot: snap }), true, st);
  for (const st of ['passed', 'rejected', 'running', 'waiting', 'queued']) assert.equal(canContinueRun({ state: st, pc: 1, snapshot: snap }), false, st);
  assert.equal(canContinueRun({ state: 'failed', pc: 2, snapshot: snap }), false);
  assert.equal(canContinueRun({ state: 'failed', pc: 0 }), false);
  assert.equal(canContinueRun(null), false);
  assert.equal(runTone('passed'), 'ok');
  assert.equal(runTone('rejected'), 'err');
  assert.equal(runTone('limited'), 'warn');
  assert.equal(runTone('waiting'), 'warn');
  assert.equal(runTone('running'), 'info');
  assert.equal(runTone('skipped'), 'muted');
  assert.equal(stateText('waiting'), 'Waiting for approval');
  assert.equal(stateText('new-state'), 'new-state');
});

test('durations and usage', () => {
  assert.equal(fmtDuration(850), '850 ms');
  assert.equal(fmtDuration(3200), '3.2 s');
  assert.equal(fmtDuration(42000), '42 s');
  assert.equal(fmtDuration(125000), '2 min 05 s');
  assert.equal(fmtDuration(3720000), '1 h 02 min');
  const run = { started: '2026-11-02T08:00:00Z', ended: '2026-11-02T08:00:05Z' };
  assert.equal(runDuration(run), 5000);
  assert.equal(runDuration({ started: '2026-11-02T08:00:00Z', ended: '0001-01-01T00:00:00Z' }, Date.parse('2026-11-02T08:01:00Z')), 60000);
  assert.equal(runDuration({ started: '0001-01-01T00:00:00Z' }), 0);
  assert.equal(isZeroTime('0001-01-01T00:00:00Z'), true);
  assert.equal(usageText({ tokens: 1234, cost_usd: 0.004, energy_wh: 0.31 }), '1.2k tokens · <$0.01 · 0.3 Wh');
  assert.equal(usageText({ tokens: 900, cost_usd: 0, energy_wh: 0 }), '900 tokens');
  assert.equal(usageText({}), '');
  assert.equal(stepUsage({ model: 'qwen3', prompt_tokens: 1000, completion_tokens: 200, duration_ms: 3100 }), 'qwen3 · 1.2k tokens · 3.1 s');
  assert.equal(stepUsage(null), '');
});

test('outputs as text', () => {
  assert.equal(outputText('hi'), 'hi');
  assert.equal(outputText(null), '');
  assert.equal(outputText(undefined), '');
  assert.equal(outputText(true), 'true');
  assert.equal(outputText([{ title: 'a' }]), '[\n  {\n    "title": "a"\n  }\n]');
});

test('stepRows pairs the flow steps with their records and states', () => {
  const snapshot = { steps: [{ id: 'a', kind: 'prompt' }, { id: 'b', kind: 'approve' }, { id: 'c', kind: 'output' }] };
  const going = { state: 'waiting', step: 'b', pc: 1, snapshot };
  const recs = [{ id: 'a', state: 'passed' }];
  assert.deepEqual(stepRows(going, recs).map((r) => [r.step.id, r.state, !!r.rec]), [['a', 'passed', true], ['b', 'waiting', false], ['c', 'pending', false]]);
  const ended = { state: 'failed', step: 'a', pc: 0, snapshot };
  assert.deepEqual(stepRows(ended, [{ id: 'a', state: 'failed' }]).map((r) => r.state), ['failed', 'skipped', 'skipped']);
  // Jumped over by an if: before pc without a record.
  const jumped = { state: 'running', step: 'c', pc: 2, snapshot };
  assert.deepEqual(stepRows(jumped, recs).map((r) => r.state), ['passed', 'skipped', 'running']);
  // A record for a step the snapshot lacks is still shown.
  assert.equal(stepRows({ state: 'passed', snapshot: { steps: [] } }, [{ id: 'z', kind: 'prompt', state: 'passed' }])[0].step.id, 'z');
  assert.deepEqual(stepRows({ state: 'queued' }, []), []);
});

test('session links from the run, its steps and items', () => {
  const run = { sessions: ['s1'] };
  const recs = [{ session: 's2' }, { items: [{ session: 's3' }, { session: 's1' }, {}] }];
  assert.deepEqual(sessionLinks(run, recs), ['s1', 's2', 's3']);
  assert.deepEqual(sessionLinks(null, null), []);
});

test('run events: this run, reconnects and flow resyncs', () => {
  assert.equal(runEventFor('flowrun', { run: 'r1' }, 'r1'), true);
  assert.equal(runEventFor('flowrun', { run: 'r2' }, 'r1'), false);
  assert.equal(runEventFor('flowrun', { run: 'r2' }, ''), true);
  assert.equal(runEventFor('events-open', null, 'r1'), true);
  assert.equal(runEventFor('resync', { what: 'flows' }, 'r1'), true);
  assert.equal(runEventFor('resync', { what: 'inbox' }, 'r1'), false);
  assert.equal(runEventFor('session', {}, 'r1'), false);
});

// ---------- the preview graph ----------

test('draftGraph matches the server graph shape and lays out', () => {
  const flow = {
    input_hint: 'Describe the website',
    steps: [
      { id: 'plan', kind: 'plan', model: 'big' },
      { id: 'parts', kind: 'fan_out', from: 'plan', as: 'prompt', routes: [{ kind: 'html', model: 'small' }, { kind: 'css', model: 'small' }], default_model: 'default' },
      { id: 'build', kind: 'session', agent: 'coder' },
      { id: 'check', kind: 'if', on: 'build', test: 'state', value: 'passed', else: 'end' },
      { id: 'ok', kind: 'approve', message: 'ok?' },
      { id: 'done', kind: 'output', to: 'inbox' },
    ],
  };
  const g = draftGraph(flow);
  assert.deepEqual(g.nodes.map((n) => n.id), ['input', 'st:plan', 'st:parts', 'st:build', 'st:check', 'st:ok', 'st:done', 'end', 'm:big', 'm:small', 'm:default']);
  assert.equal(g.nodes[0].sub, 'Describe the website');
  assert.equal(g.nodes[1].sub, 'plan · big');
  assert.equal(g.nodes[2].sub, 'fan_out · from plan · as prompt');
  const e = (from, kind) => g.edges.filter((x) => x.from === from && x.kind === kind).map((x) => x.to);
  assert.deepEqual(e('input', 'next'), ['st:plan']);
  assert.deepEqual(e('st:check', 'then'), ['st:ok']);
  assert.deepEqual(e('st:check', 'else'), ['end']);
  assert.deepEqual(e('st:done', 'next'), ['end']);
  assert.deepEqual(e('st:parts', 'ask'), ['m:small', 'm:default'], 'one ask edge per model');
  assert.deepEqual(e('st:build', 'ask'), [], 'sessions pick their own model');
  const laid = layoutGraph(g, 'tb');
  assert.equal(laid.nodes.length, g.nodes.length);
  assert.ok(laid.width > 0 && laid.height > 0);
  // Only branches and items carry a caption in the preview.
  assert.equal(previewChip({ kind: 'next' }), false);
  assert.equal(previewChip({ kind: 'ask' }), false);
  assert.equal(previewChip({ kind: 'then' }), true);
});

test('draftGraph: no steps, a jump to a missing step, names over ids', () => {
  assert.deepEqual(draftGraph({ steps: [] }).edges, [{ from: 'input', to: 'end', kind: 'next', label: 'input', count: 1 }]);
  const g = draftGraph({ steps: [{ id: 'a', kind: 'if', then: 'ghost', else: '' }, { id: 'b', kind: 'prompt', name: 'Answer', model: '' }] });
  assert.deepEqual(g.edges.filter((x) => x.from === 'st:a').map((x) => [x.kind, x.to]), [['else', 'st:b']]);
  assert.equal(g.nodes.find((n) => n.id === 'st:b').label, 'Answer');
  assert.equal(g.nodes.some((n) => n.kind === 'model'), false, 'no empty model node');
  assert.deepEqual(draftGraph(null).nodes.map((n) => n.id), ['input', 'end']);
});

test('selectDefault and setStepField: a state test never keeps an empty value', () => {
  const s = newStep('if', [{ id: 'work', kind: 'session' }]);
  assert.equal(s.value, '');
  assert.equal(selectDefault(s, 'test'), 'contains');
  setStepField(s, 'test', 'state');
  assert.equal(s.test, 'state');
  assert.equal(s.value, 'passed');
  // a session state already chosen stays
  s.value = 'failed';
  setStepField(s, 'test', 'state');
  assert.equal(s.value, 'failed');
  // a value from another test is replaced: the select cannot show it
  const t = { kind: 'if', test: 'state', value: 'yes' };
  assert.equal(selectDefault(t, 'value'), 'passed');
  assert.equal(selectDefault({ kind: 'if', test: 'contains', value: '' }, 'value'), '');
  assert.equal(selectDefault({ kind: 'fan_out' }, 'as'), 'prompt');
  assert.equal(selectDefault({ kind: 'session' }, 'workspace'), 'scratch');
  setStepField(t, 'test', 'contains');
  assert.equal(t.value, 'yes');
});

test('rowKey changes only when a run-page row shows something new', () => {
  const run = { state: 'running', step: 'fan', pc: 1, snapshot: { steps: [{ id: 'plan', kind: 'plan' }, { id: 'fan', kind: 'fan_out' }] } };
  const recs = [{ id: 'plan', kind: 'plan', state: 'passed', output: [1, 2] }];
  const a = stepRows(run, recs).map((r, i) => rowKey(r, i));
  // a fan-out item event (same records, the run still at the same step) keeps every row
  const b = stepRows({ ...run, updated: 'later', tokens: 9 }, recs.map((r) => ({ ...r }))).map((r, i) => rowKey(r, i));
  assert.deepEqual(a, b);
  // the fan-out step ending changes its row only
  const c = stepRows({ ...run, state: 'passed', pc: 2 }, [...recs, { id: 'fan', kind: 'fan_out', state: 'passed', items: [] }]).map((r, i) => rowKey(r, i));
  assert.equal(c[0], a[0]);
  assert.notEqual(c[1], a[1]);
  assert.equal(itemKey('fan', 3), 'fan#3');
});

test('runLog: the whole run as text, nothing cut', () => {
  const big = 'y'.repeat(30000);
  const run = {
    id: 'r-7', flow: 'fl-site', flow_name: 'Website', state: 'failed', trigger: 'manual', flow_version: 3,
    actor: 'owner:admin', created: '2026-11-03T10:00:00Z', started: '2026-11-03T10:00:01Z', ended: '2026-11-03T10:01:01Z',
    input: 'A bakery site', tokens: 1500, models: ['qwen', 'gemma'], reason: 'step build failed', pc: 2,
    snapshot: { steps: [{ id: 'plan', kind: 'plan', name: 'Plan' }, { id: 'fan', kind: 'fan_out' }, { id: 'out', kind: 'output' }] },
  };
  const recs = [
    { id: 'plan', kind: 'plan', state: 'passed', model: 'qwen', input: 'Plan it', output: [{ title: 'HTML' }], duration_ms: 2000 },
    { id: 'fan', kind: 'fan_out', state: 'failed', error: 'one item failed', items: [
      { index: 0, title: 'HTML', kind: 'code', state: 'passed', input: 'Write HTML', output: big },
      { index: 1, title: 'Copy', state: 'failed', error: 'model busy' },
    ] },
  ];
  const t = runLog(run, recs);
  assert.match(t, /^Flow: Website \(fl-site\)\nRun: r-7\nState: Failed\n/);
  assert.match(t, /Run by: trigger manual · version 3 · by owner:admin/);
  assert.match(t, /\nTime: 1 min 00 s\n/);
  assert.match(t, /Reason: step build failed/);
  assert.match(t, /--- input ---\nA bakery site/);
  assert.match(t, /=== 1\. Plan \(Plan\) · Passed · qwen · 2(\.0)? s ===\n--- input ---\nPlan it\n--- output ---\n\[\n {2}\{\n {4}"title": "HTML"/);
  assert.match(t, /=== 2\. fan \([^)]+\) · Failed ===\nError: one item failed/);
  assert.match(t, /\n {2}--- item 1: HTML \(code\) · Passed\n {2}--- input ---\n {2}Write HTML\n {2}--- output ---\n {2}y+/);
  assert.ok(t.includes(big), 'a long output is kept whole');
  assert.match(t, /--- item 2: Copy · Failed\n {2}Error: model busy/);
  assert.match(t, /=== 3\. out \([^)]+\) · Skipped ===\nDid not run\.\n$/);
  assert.equal(runLog(null, []), '');
  assert.match(runLog({ id: 'r', flow: 'f', state: 'queued', snapshot: { steps: [{ id: 'a', kind: 'prompt' }] } }, []), /Not run yet\./);
});

test('retries and waiting for the model: usage line, rows and the log', () => {
  assert.equal(retriedText(0), '');
  assert.equal(retriedText(undefined), '');
  assert.equal(retriedText(1), 'retried once');
  assert.equal(retriedText(2), 'retried 2 times');
  assert.equal(stepUsage({ model: 'qwen', prompt_tokens: 10, completion_tokens: 5, retries: 1 }), 'qwen · 15 tokens · retried once');
  const snapshot = { steps: [{ id: 'plan', kind: 'plan' }, { id: 'fan', kind: 'fan_out' }] };
  const run = { id: 'r', flow: 'f', state: 'running', step: 'fan', pc: 1, waiting: 'item 2: waiting for qwen: 1 of 1 slots busy', snapshot };
  const recs = [{ id: 'plan', kind: 'plan', state: 'passed', model: 'qwen', retries: 1, retry_error: 'model call failed (400): terminated' }];
  const live = [
    { index: 0, title: 'HTML', state: 'running' },
    { index: 1, title: 'CSS', state: 'running', waiting: 'waiting for qwen: 1 of 1 slots busy' },
    { index: 2, title: 'Copy', state: 'passed', retries: 1, retry_error: 'model call failed (502): Channel Error' },
  ];
  const rows = stepRows(run, recs, live);
  assert.equal(rows[0].waiting, undefined, 'a finished step never waits');
  assert.equal(rows[1].waiting, run.waiting);
  assert.equal(rows[1].live, live);
  // the row's key follows the waiting reason
  const k1 = rowKey(rows[1], 1);
  const k2 = rowKey(stepRows({ ...run, waiting: '' }, recs, live)[1], 1);
  assert.notEqual(k1, k2);
  assert.equal(stepRows({ ...run, waiting: '' }, recs, [])[1].waiting, undefined);
  const t = runLog(run, recs, live);
  assert.match(t, /=== 1\. plan \([^)]+\) · Passed · qwen · retried once ===\nRetried: 1 \(after: model call failed \(400\): terminated\)/);
  assert.match(t, /=== 2\. fan \([^)]+\) · Running ===\nRunning now\.\nWaited: item 2: waiting for qwen: 1 of 1 slots busy/);
  assert.match(t, /--- item 2: CSS · Running\n {2}Waited: waiting for qwen: 1 of 1 slots busy/);
  assert.match(t, /--- item 3: Copy · Passed · retried once\n {2}Retried: 1 \(after: model call failed \(502\): Channel Error\)/);
  // without a live list (the editor's test run) the log still reads
  assert.match(runLog(run, recs), /Running now\.\nWaited: item 2/);
});

test('runLogName: a safe file name', () => {
  assert.equal(runLogName({ flow: 'fl-site', id: 'r-7' }), 'fl-site-r-7.log');
  assert.equal(runLogName({ flow_name: 'My flow / test', id: 'a b' }), 'My-flow-test-a-b.log');
  assert.equal(runLogName({}), 'flow-run.log');
  assert.equal(runLogName(null), 'flow-run.log');
});
