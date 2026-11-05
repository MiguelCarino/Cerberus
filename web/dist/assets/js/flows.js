// Pure helpers for the Flows views (views/flows.js, flowedit.js and
// flowrun.js): triggers and schedules in words, the friendly schedule
// picker's model, the step forms' model (kinds, fields, defaults, add /
// duplicate / move / delete, id renames), validation problems placed on
// fields, run and step states, usage text and a draft's preview graph.
// No DOM and no network, so node --test covers all of it
// (web/test/flows.test.mjs). The JSON shapes are the contract at the top
// of internal/api/flows.go.

import { fmtUSD } from './spend.js';
import { fmtTokens } from './delegation.js';

// ---------- schedules and triggers ----------

export const DAY_KEYS = ['sun', 'mon', 'tue', 'wed', 'thu', 'fri', 'sat'];
export const DAY_WORDS = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat'];
// Monday first, as the picker and Describe show them.
export const WEEK_ORDER = [1, 2, 3, 4, 5, 6, 0];

// The friendly picker's modes, in the order the select offers them.
export const SCHEDULE_MODES = [
  ['every', 'Every…'],
  ['daily', 'Every day'],
  ['weekdays', 'Weekdays'],
  ['weekends', 'Weekends'],
  ['days', 'On these days'],
  ['cron', 'Cron line'],
];

const norm = (s) => String(s || '').toLowerCase().trim().split(/\s+/).filter(Boolean).join(' ');

// parseClock reads "8:00" or "08:00" → minutes of the day, or -1.
export function parseClock(v) {
  const m = /^(\d{1,2}):(\d{2})$/.exec(String(v || '').trim());
  if (!m) return -1;
  const hh = +m[1];
  const mm = +m[2];
  return hh <= 23 && mm <= 59 ? hh * 60 + mm : -1;
}

export const clockText = (min) => `${String(Math.floor(min / 60)).padStart(2, '0')}:${String(min % 60).padStart(2, '0')}`;

// parseDays reads a day list ("mon,thu", "mon-fri", "sat,sun") → sorted
// day numbers (0 = Sunday), or null. Only names, like the picker writes.
// A range may end on Sunday ("fri-sun"), as the server reads it.
export function parseDays(spec) {
  const out = new Set();
  for (const part of String(spec || '').split(',')) {
    const [a, b, extra] = part.split('-');
    if (extra !== undefined) return null;
    const i = DAY_KEYS.indexOf(a);
    if (i < 0) return null;
    if (b === undefined) { out.add(i); continue; }
    let j = DAY_KEYS.indexOf(b);
    if (j === 0 && i > 0) j = 7; // Sunday closing a range: day 7, folded back to 0
    if (j < 0 || j < i) return null;
    for (let d = i; d <= j; d++) out.add(d % 7);
  }
  return out.size ? [...out].sort((x, y) => x - y) : null;
}

// parseSchedule reads a schedule string into the picker's model:
// {mode, n, unit ('m'|'h'), at ('HH:MM'), days [0..6], cron, ok}. A
// string the picker cannot show (any 5-field line, or text that does not
// parse) is mode 'cron' with the text in cron; ok says whether the
// string looks valid (the server's validation is the judge).
export function parseSchedule(src) {
  const s = norm(src);
  const base = { mode: 'daily', n: 6, unit: 'h', at: '08:00', days: [1], cron: '', ok: false };
  if (!s) return base;
  const f = s.split(' ');
  if (f[0] === 'every' && f.length === 2) {
    const m = /^(\d+)([hm])$/.exec(f[1]);
    if (m && +m[1] > 0 && +m[1] * (m[2] === 'h' ? 60 : 1) <= 1440) return { ...base, mode: 'every', n: +m[1], unit: m[2], ok: true };
    return { ...base, mode: 'cron', cron: s };
  }
  if (f.length === 2 && f[1].includes(':')) {
    const at = parseClock(f[1]);
    if (at < 0) return { ...base, mode: 'cron', cron: s };
    const time = clockText(at);
    if (f[0] === 'daily' || f[0] === 'weekdays' || f[0] === 'weekends') return { ...base, mode: f[0], at: time, ok: true };
    const days = parseDays(f[0]);
    if (days) return { ...base, mode: 'days', at: time, days, ok: true };
    return { ...base, mode: 'cron', cron: s };
  }
  return { ...base, mode: 'cron', cron: s, ok: f.length === 5 };
}

// buildSchedule writes the picker's model back as a schedule string.
export function buildSchedule(m) {
  const at = parseClock(m && m.at) >= 0 ? clockText(parseClock(m.at)) : '08:00';
  switch (m && m.mode) {
    case 'every': {
      const n = Math.max(1, Math.floor(Number(m.n) || 1));
      return `every ${n}${m.unit === 'm' ? 'm' : 'h'}`;
    }
    case 'daily': case 'weekdays': case 'weekends': return `${m.mode} ${at}`;
    case 'days': {
      const days = WEEK_ORDER.filter((d) => (m.days || []).includes(d));
      return days.length ? `${days.map((d) => DAY_KEYS[d]).join(',')} ${at}` : '';
    }
    default: return norm(m && m.cron);
  }
}

// cronFor is the cron line for the picker's model, the start of the
// cron field when the picker switches to "Cron line".
export function cronFor(m) {
  const t = parseClock(m && m.at);
  const at = t >= 0 ? t : 8 * 60;
  const mm = at % 60;
  const hh = Math.floor(at / 60);
  switch (m && m.mode) {
    case 'every': return m.unit === 'm' ? `*/${m.n} * * * *` : `0 */${m.n} * * *`;
    case 'weekdays': return `${mm} ${hh} * * 1-5`;
    case 'weekends': return `${mm} ${hh} * * 0,6`;
    case 'days': return `${mm} ${hh} * * ${[...(m.days || [])].sort((a, b) => a - b).join(',') || '*'}`;
    default: return `${mm} ${hh} * * *`;
  }
}

// describeSchedule is a schedule in words, like the server's Describe:
// "Every 6 hours", "Daily at 08:00", "Weekdays at 08:00", "Mon, Thu at
// 19:30", "Cron 0 8 * * 1-5"; "" when it does not parse.
export function describeSchedule(src) {
  const m = parseSchedule(src);
  if (!m.ok) return '';
  switch (m.mode) {
    case 'every': {
      const min = m.n * (m.unit === 'h' ? 60 : 1);
      if (min === 60) return 'Every hour';
      if (min % 60 === 0) return `Every ${min / 60} hours`;
      if (min === 1) return 'Every minute';
      return `Every ${min} minutes`;
    }
    case 'daily': return `Daily at ${m.at}`;
    case 'weekdays': return `Weekdays at ${m.at}`;
    case 'weekends': return `Weekends at ${m.at}`;
    case 'days': return `${WEEK_ORDER.filter((d) => m.days.includes(d)).map((d) => DAY_WORDS[d]).join(', ')} at ${m.at}`;
    default: return `Cron ${m.cron}`;
  }
}

// triggerText is a trigger in words: "Manual", the schedule described, or
// the raw schedule when it does not parse.
export function triggerText(trigger) {
  if (!trigger || trigger.kind !== 'schedule') return 'Manual';
  return describeSchedule(trigger.schedule) || String(trigger.schedule || '').trim() || 'Schedule (not set)';
}

const isZero = (iso) => !iso || String(iso).startsWith('0001-') || !(Date.parse(iso) > 0);
export const isZeroTime = isZero;

// nextText is when a flow runs next: "today 08:00", "tomorrow 08:00",
// "Mon 08:00" (this week), "12 Nov 08:00"; "" for none.
export function nextText(iso, now = new Date()) {
  if (isZero(iso)) return '';
  const t = new Date(iso);
  const day = (d) => new Date(d.getFullYear(), d.getMonth(), d.getDate()).getTime();
  const days = Math.round((day(t) - day(now)) / 86400000);
  const clock = clockText(t.getHours() * 60 + t.getMinutes());
  if (days <= 0) return `today ${clock}`;
  if (days === 1) return `tomorrow ${clock}`;
  if (days < 7) return `${DAY_WORDS[t.getDay()]} ${clock}`;
  const mon = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'][t.getMonth()];
  return `${t.getDate()} ${mon} ${clock}`;
}

// ---------- steps ----------

export const STEP_KINDS = [
  { kind: 'prompt', label: 'Prompt', blurb: 'Ask a model once; its reply is the output.' },
  { kind: 'plan', label: 'Plan', blurb: 'Ask a model for a list of parts to hand out.' },
  { kind: 'fan_out', label: 'Fan out', blurb: "Run each part of a plan on a model or an agent." },
  { kind: 'session', label: 'Agent session', blurb: 'Start an agent session and wait until it ends.' },
  { kind: 'if', label: 'If', blurb: 'Jump ahead depending on an earlier output.' },
  { kind: 'approve', label: 'Approve', blurb: 'Wait until you approve or reject.' },
  { kind: 'output', label: 'Output', blurb: 'Send a notification, an inbox entry or a file.' },
];

export const kindLabel = (k) => (STEP_KINDS.find((x) => x.kind === k) || { label: k || 'Step' }).label;

// The fields each kind keeps (besides id, kind and name), as the contract lists them.
export const KIND_FIELDS = {
  prompt: ['model', 'system', 'template', 'json'],
  plan: ['model', 'template', 'max_items'],
  fan_out: ['from', 'routes', 'default_model', 'as', 'template', 'agent', 'workspace', 'repo', 'folder', 'max_parallel'],
  session: ['agent', 'workspace', 'repo', 'folder', 'task'],
  if: ['test', 'on', 'value', 'path', 'then', 'else'],
  approve: ['message'],
  output: ['to', 'template', 'file'],
};

export const IF_TESTS = [
  ['contains', 'contains'], ['equals', 'equals'], ['matches', 'matches (regex)'],
  ['json', 'JSON field equals'], ['state', 'session state is'],
];
export const SESSION_STATES = ['passed', 'failed', 'stopped', 'error'];
export const OUTPUT_TO = [['inbox', 'Inbox'], ['notify', 'Notification'], ['file', 'File in the run folder']];
export const WORKSPACES = [['scratch', 'Scratch folder'], ['repo', 'Repository'], ['folder', 'Folder']];

// stepFields is the short form a card shows for a step, given the steps
// before it: [{key, label, type, options?, hint?, placeholder?, show}]. type
// is text | textarea | template | number | bool | select | model | agent |
// step | routes. show is false for a field the step's other values hide.
export function stepFields(step, steps = []) {
  const i = steps.findIndex((s) => s === step);
  const before = (i < 0 ? steps : steps.slice(0, i)).filter((s) => s && s.id);
  const after = i < 0 ? [] : steps.slice(i + 1).filter((s) => s && s.id);
  const earlier = before.map((s) => [s.id, `${s.id} · ${kindLabel(s.kind)}`]);
  const jumps = [['', 'the next step'], ...after.map((s) => [s.id, s.id]), ['end', 'end (stop)']];
  const ws = (s) => s.workspace || 'scratch';
  switch (step && step.kind) {
    case 'prompt': return [
      { key: 'model', label: 'Model', type: 'model' },
      { key: 'template', label: 'Prompt', type: 'template', placeholder: 'Answer this: {input}' },
      { key: 'system', label: 'System prompt (optional)', type: 'template' },
      { key: 'json', label: 'The reply is JSON (parse it)', type: 'bool' },
    ];
    case 'plan': return [
      { key: 'model', label: 'Model', type: 'model', hint: 'A bigger model plans better.' },
      { key: 'template', label: 'What to plan', type: 'template', placeholder: 'Split this job into parts: {input}' },
      { key: 'max_items', label: 'At most this many parts', type: 'number', placeholder: '8', min: 1, max: 50 },
    ];
    case 'fan_out': {
      const asSession = step.as === 'session';
      const sources = before.filter((s) => s.kind === 'plan' || (s.kind === 'prompt' && s.json)).map((s) => [s.id, `${s.id} · ${kindLabel(s.kind)}`]);
      return [
        { key: 'from', label: 'Parts from', type: 'step', options: sources, hint: sources.length ? '' : 'Add a Plan step (or a JSON prompt) before this one.' },
        { key: 'as', label: 'Run each part as', type: 'select', options: [['prompt', 'a prompt to a model'], ['session', 'an agent session']] },
        { key: 'routes', label: 'Models by part kind', type: 'routes', hint: 'The first route whose kind matches picks the model.' },
        { key: 'default_model', label: asSession ? 'Default model (optional)' : 'Default model', type: 'model' },
        { key: 'template', label: 'Each part gets', type: 'template', inItem: true, placeholder: '{item.task}' },
        { key: 'agent', label: 'Agent', type: 'agent', show: asSession },
        { key: 'workspace', label: 'Workspace', type: 'select', options: WORKSPACES, show: asSession },
        { key: 'repo', label: 'Repository', type: 'repo', show: asSession && ws(step) === 'repo' },
        { key: 'folder', label: 'Folder', type: 'text', placeholder: '/home/me/projects/site', show: asSession && ws(step) === 'folder' },
        { key: 'max_parallel', label: 'At most this many at once', type: 'number', placeholder: '2', min: 1, max: 16 },
      ];
    }
    case 'session': return [
      { key: 'agent', label: 'Agent', type: 'agent' },
      { key: 'workspace', label: 'Workspace', type: 'select', options: WORKSPACES },
      { key: 'repo', label: 'Repository', type: 'repo', show: ws(step) === 'repo' },
      { key: 'folder', label: 'Folder', type: 'text', placeholder: '/home/me/projects/site', show: ws(step) === 'folder' },
      { key: 'task', label: 'Task', type: 'template', placeholder: 'Build it: {steps.plan.output}' },
    ];
    case 'if': {
      const t = step.test || 'contains';
      return [
        { key: 'on', label: 'Test the output of', type: 'step', options: earlier },
        { key: 'test', label: 'Test', type: 'select', options: IF_TESTS },
        { key: 'path', label: 'JSON path', type: 'text', placeholder: 'result.ok', show: t === 'json' },
        t === 'state'
          ? { key: 'value', label: 'State', type: 'select', options: SESSION_STATES.map((x) => [x, x]) }
          : { key: 'value', label: t === 'matches' ? 'Regular expression' : 'Value', type: 'text', placeholder: t === 'matches' ? '^yes\\b' : 'yes' },
        { key: 'then', label: 'When true, go to', type: 'select', options: jumps },
        { key: 'else', label: 'Otherwise, go to', type: 'select', options: jumps },
      ];
    }
    case 'approve': return [
      { key: 'message', label: 'Ask', type: 'template', placeholder: 'Approve to continue?' },
    ];
    case 'output': return [
      { key: 'to', label: 'Send to', type: 'select', options: OUTPUT_TO },
      { key: 'file', label: 'File name', type: 'text', placeholder: 'result-{date}.md', show: step.to === 'file' },
      { key: 'template', label: 'Text', type: 'template', placeholder: '{steps.answer.output}' },
    ];
    default: return [];
  }
}

const BASE_ID = { prompt: 'prompt', plan: 'plan', fan_out: 'fan', session: 'session', if: 'check', approve: 'approve', output: 'output' };

// uniqueId is base, else base-2, base-3, ... (never "end"), as a valid
// step id (a-z, 0-9, - and _, starting with a letter, at most 32).
export function uniqueId(base, ids) {
  let b = String(base || 'step').toLowerCase().replace(/[^a-z0-9_-]+/g, '-').replace(/^[^a-z]+/, '').replace(/-+$/, '').slice(0, 28) || 'step';
  if (b === 'end') b = 'end-step';
  const taken = new Set(ids);
  if (!taken.has(b)) return b;
  for (let n = 2; ; n++) if (!taken.has(`${b}-${n}`)) return `${b}-${n}`;
}

const out = (id) => `{steps.${id}.output}`;

// newStep builds a step of kind with defaults that make sense after steps
// (it reads the last step's output; a fan-out takes the last plan).
// opts: {model, agent} defaults for the pickers.
export function newStep(kind, steps = [], opts = {}) {
  const ids = steps.map((s) => s.id);
  const prev = steps.length ? steps[steps.length - 1].id : '';
  const from = [...steps].reverse().find((s) => s.kind === 'plan' || (s.kind === 'prompt' && s.json));
  const model = opts.model || 'default';
  const s = { id: uniqueId(BASE_ID[kind] || kind, ids), kind, name: '' };
  switch (kind) {
    case 'prompt': return { ...s, model, template: prev ? out(prev) : '{input}' };
    case 'plan': return { ...s, model, template: `Split this job into parts: ${prev ? out(prev) : '{input}'}`, max_items: 5 };
    case 'fan_out': return { ...s, from: from ? from.id : '', as: 'prompt', routes: [], default_model: model, template: '{item.task}' };
    case 'session': return { ...s, agent: opts.agent || '', workspace: 'scratch', task: prev ? out(prev) : '{input}' };
    case 'if': return { ...s, on: prev, test: 'contains', value: '', then: '', else: 'end' };
    case 'approve': return { ...s, message: 'Approve to continue?' };
    case 'output': return { ...s, to: 'inbox', template: prev ? out(prev) : '{input}' };
    default: return s;
  }
}

// selectDefault is the value a step card's select shows (and the step
// should hold) when the step has none that fits: a select with no empty
// option would otherwise show its first option while the step keeps "".
export function selectDefault(step, key) {
  const cur = (step && step[key]) || '';
  switch (key) {
    case 'as': return cur || 'prompt';
    case 'workspace': return cur || 'scratch';
    case 'test': return cur || 'contains';
    case 'value': return step && step.test === 'state' && !SESSION_STATES.includes(cur) ? 'passed' : cur;
    default: return cur;
  }
}

// setStepField sets step[key] = v and keeps dependent values sensible:
// switching an if step's test to "session state is" picks "passed" unless
// the value already is a session state.
export function setStepField(step, key, v) {
  step[key] = v;
  if (key === 'test' && v === 'state') step.value = selectDefault(step, 'value');
  return step;
}

const copy = (v) => JSON.parse(JSON.stringify(v));

// duplicateStep inserts a copy of steps[i] after it with a fresh id.
export function duplicateStep(steps, i) {
  if (i < 0 || i >= steps.length) return steps;
  const c = copy(steps[i]);
  c.id = uniqueId(`${steps[i].id}-copy`, steps.map((s) => s.id));
  if (c.name) c.name = `${c.name} (copy)`;
  return [...steps.slice(0, i + 1), c, ...steps.slice(i + 1)];
}

// moveStep moves steps[i] by delta (−1 up, +1 down); unchanged at an end.
export function moveStep(steps, i, delta) {
  const j = i + delta;
  if (i < 0 || i >= steps.length || j < 0 || j >= steps.length) return steps;
  const next = [...steps];
  [next[i], next[j]] = [next[j], next[i]];
  return next;
}

export function removeStep(steps, i) {
  return i < 0 || i >= steps.length ? steps : [...steps.slice(0, i), ...steps.slice(i + 1)];
}

const TEXT_FIELDS = ['template', 'system', 'task', 'message', 'file'];
const REF_FIELDS = ['from', 'on', 'then', 'else'];

// renameStepRefs renames step id from → to in the steps that point at it:
// from / on / then / else and {steps.<id>.output} in their texts.
export function renameStepRefs(steps, from, to) {
  if (!from || !to || from === to) return steps;
  const re = new RegExp(`\\{steps\\.${from.replace(/[.*+?^${}()|[\]\\-]/g, '\\$&')}\\.output\\}`, 'g');
  return steps.map((s) => {
    const n = { ...s };
    if (n.id === from) n.id = to;
    for (const k of REF_FIELDS) if (n[k] === from) n[k] = to;
    for (const k of TEXT_FIELDS) if (typeof n[k] === 'string') n[k] = n[k].replace(re, out(to));
    return n;
  });
}

// placeholdersFor lists what step i's templates can use, for the chips
// under a text field: {input}, earlier steps' outputs, {item.*} inside a
// fan-out, then {date}, {time} and {flow.name}.
export function placeholdersFor(steps, i, inItem = false) {
  const list = ['{input}'];
  for (const s of steps.slice(0, Math.max(0, i))) if (s && s.id) list.push(out(s.id));
  if (inItem) list.push('{item.task}', '{item.title}', '{item.kind}');
  list.push('{date}', '{time}', '{flow.name}');
  return list;
}

const keep = (v) => !(v === undefined || v === null || v === '' || v === false || (Array.isArray(v) && v.length === 0));

// cleanStep keeps only the step's own kind's fields, without empty values
// (the server leaves them out too), numbers as numbers.
export function cleanStep(step) {
  const o = { id: String(step.id || '').trim(), kind: step.kind };
  if (keep(step.name) && String(step.name).trim()) o.name = String(step.name).trim();
  for (const k of KIND_FIELDS[step.kind] || []) {
    let v = step[k];
    if (k === 'max_items' || k === 'max_parallel') v = Math.max(0, Math.floor(Number(v) || 0)) || undefined;
    if (k === 'routes') v = (v || []).filter((r) => r && (String(r.kind || '').trim() || r.model)).map((r) => ({ kind: String(r.kind || '').trim(), model: r.model || '' }));
    if (keep(v)) o[k] = v;
  }
  return o;
}

export const LIMIT_FIELDS = [
  ['max_steps', 'Max steps', 50],
  ['max_tokens', 'Max tokens', 200000],
  ['max_cost_usd', 'Max cost (USD)', 1],
  ['max_parallel', 'Max parallel', 2],
  ['timeout_min', 'Timeout (minutes)', 60],
];

// limitsText is the collapsed Limits line: the ones set, else the defaults.
export function limitsText(lim = {}) {
  const set = LIMIT_FIELDS.filter(([k]) => lim[k] > 0).map(([k, label]) => `${label.toLowerCase()} ${lim[k]}`);
  return set.length ? set.join(' · ') : 'Defaults: 50 steps, 200k tokens, $1.00, 2 at once, 60 min';
}

// clip cuts s to n characters with an ellipsis.
export const clip = (s, n) => { const t = String(s ?? ''); return t.length > n ? `${t.slice(0, n - 1)}…` : t; };

// stepSummary is a collapsed card's line: "plan · default · Split this…".
export function stepSummary(s) {
  const t = s.template || s.task || s.message || '';
  const parts = [s.id];
  if (s.model) parts.push(s.model);
  if (s.agent) parts.push(s.agent);
  if (t) parts.push(clip(String(t).replace(/\s+/g, ' '), 90));
  return parts.join(' · ');
}

// cleanFlow is the draft as the API takes it.
export function cleanFlow(d) {
  const limits = {};
  for (const [k] of LIMIT_FIELDS) {
    const v = Number(d.limits && d.limits[k]);
    if (v > 0) limits[k] = k === 'max_cost_usd' ? v : Math.floor(v);
  }
  const trigger = d.trigger && d.trigger.kind === 'schedule'
    ? { kind: 'schedule', schedule: String(d.trigger.schedule || '').trim() }
    : { kind: 'manual' };
  const f = {
    name: String(d.name || '').trim(),
    description: String(d.description || ''),
    enabled: !!d.enabled,
    trigger,
    input_hint: String(d.input_hint || ''),
    limits,
    steps: (d.steps || []).map(cleanStep),
    version: Number(d.version) || 0,
  };
  if (d.id) f.id = d.id;
  return f;
}

// blankFlow is a new flow with one prompt step.
export function blankFlow(opts = {}) {
  return { name: '', description: '', enabled: true, trigger: { kind: 'manual' }, input_hint: '', limits: {}, steps: [newStep('prompt', [], opts)], version: 0 };
}

// fromTemplate is a template's flow as a new draft (no id, version 0).
export function fromTemplate(t) {
  const f = copy((t && t.flow) || {});
  delete f.id;
  return { name: '', description: '', input_hint: '', limits: {}, steps: [], ...f, enabled: true, trigger: f.trigger || { kind: 'manual' }, version: 0 };
}

// ---------- validation problems ----------

// indexProblems sorts problems for the form: by step id and field
// (problems on the flow itself under step ""), with counts.
export function indexProblems(problems) {
  const by = new Map();
  let errors = 0;
  let warnings = 0;
  for (const p of problems || []) {
    if (!p) continue;
    if (p.severity === 'warning') warnings++; else errors++;
    const st = p.step || '';
    if (!by.has(st)) by.set(st, []);
    by.get(st).push(p);
  }
  return { by, errors, warnings, all: problems || [] };
}

// problemsAt: the problems of a field (field "routes" also takes
// "routes.0.kind"); field "" takes the step's own problems and those on
// fields the form does not show (known = the shown field keys).
export function problemsAt(idx, step, field, known = []) {
  const list = (idx && idx.by.get(step || '')) || [];
  if (field) return list.filter((p) => p.field === field || (p.field || '').startsWith(field + '.'));
  return list.filter((p) => !p.field || !known.some((k) => p.field === k || p.field.startsWith(k + '.')));
}

// problemSummary: "2 problems to fix · 1 warning", "1 warning", "No problems".
export function problemSummary(idx) {
  if (!idx) return '';
  const parts = [];
  if (idx.errors) parts.push(`${idx.errors} ${idx.errors === 1 ? 'problem' : 'problems'} to fix`);
  if (idx.warnings) parts.push(`${idx.warnings} ${idx.warnings === 1 ? 'warning' : 'warnings'}`);
  return parts.join(' · ') || 'No problems';
}

// problemText words one problem for a list: "Step plan · model: …".
export function problemText(p) {
  const where = [p.step ? `Step ${p.step}` : '', p.field || ''].filter(Boolean).join(' · ');
  return where ? `${where}: ${p.message}` : p.message;
}

// ---------- runs ----------

export const FINAL_STATES = ['passed', 'failed', 'rejected', 'limited', 'canceled'];
export const isFinalRun = (state) => FINAL_STATES.includes(state);
export const canCancelRun = (state) => !!state && !isFinalRun(state);

// runTone maps a run or step state to a Branding status tone.
export function runTone(state) {
  switch (state) {
    case 'passed': return 'ok';
    case 'failed': case 'rejected': return 'err';
    case 'waiting': case 'limited': return 'warn';
    case 'running': return 'info';
    default: return 'muted'; // queued, pending, skipped, canceled
  }
}

const STATE_WORDS = {
  queued: 'Queued', running: 'Running', waiting: 'Waiting for approval', passed: 'Passed', failed: 'Failed',
  rejected: 'Rejected', limited: 'Stopped at a limit', canceled: 'Canceled', pending: 'Pending', skipped: 'Skipped',
};
export const stateText = (s) => STATE_WORDS[s] || s || '';

// fmtDuration: 850 ms, 3.2 s, 2 min 05 s, 1 h 02 min.
export function fmtDuration(ms) {
  const v = Math.max(0, Number(ms) || 0);
  if (v < 1000) return `${Math.round(v)} ms`;
  if (v < 60000) return `${(v / 1000).toFixed(v < 10000 ? 1 : 0)} s`;
  const s = Math.round(v / 1000);
  if (s < 3600) return `${Math.floor(s / 60)} min ${String(s % 60).padStart(2, '0')} s`;
  return `${Math.floor(s / 3600)} h ${String(Math.floor((s % 3600) / 60)).padStart(2, '0')} min`;
}

// runDuration: started → ended (or now while it goes); 0 before it started.
export function runDuration(run, now = Date.now()) {
  if (!run || isZero(run.started)) return 0;
  const end = isZero(run.ended) ? now : Date.parse(run.ended);
  return Math.max(0, end - Date.parse(run.started));
}

export function fmtWh(wh) {
  const v = Number(wh) || 0;
  if (v <= 0) return '';
  if (v >= 1000) return `${(v / 1000).toFixed(2)} kWh`;
  if (v >= 10) return `${v.toFixed(0)} Wh`;
  if (v >= 0.1) return `${v.toFixed(1)} Wh`;
  return '<0.1 Wh';
}

// usageText: "1.2k tokens · <$0.01 · 0.3 Wh" (zero parts left out; a local
// model costs energy only), or "" when nothing was used.
export function usageText({ tokens = 0, cost_usd: cost = 0, energy_wh: wh = 0 } = {}) {
  const parts = [];
  if (tokens > 0) parts.push(`${fmtTokens(tokens)} tokens`);
  if (cost > 0) parts.push(fmtUSD(cost));
  const e = fmtWh(wh);
  if (e) parts.push(e);
  return parts.join(' · ');
}

// stepUsage is a step record's line: "qwen3 · 1.2k tokens · 3.1 s".
export function stepUsage(rec) {
  if (!rec) return '';
  const parts = [];
  if (rec.model) parts.push(rec.model);
  const u = usageText({ tokens: (rec.prompt_tokens || 0) + (rec.completion_tokens || 0), cost_usd: rec.cost_usd, energy_wh: rec.energy_wh });
  if (u) parts.push(u);
  if (rec.duration_ms > 0) parts.push(fmtDuration(rec.duration_ms));
  const rt = retriedText(rec.retries);
  if (rt) parts.push(rt);
  return parts.join(' · ');
}

// retriedText: "retried once" (or "retried 2 times") for model calls the
// runner retried after a transient engine failure; "" for none.
export function retriedText(n) {
  n = Math.max(0, Math.floor(Number(n) || 0));
  if (!n) return '';
  return n === 1 ? 'retried once' : `retried ${n} times`;
}

// outputText shows any step output as text: a string as it is, anything
// else as indented JSON; "" for none.
export function outputText(v) {
  if (v === undefined || v === null) return '';
  if (typeof v === 'string') return v;
  try { return JSON.stringify(v, null, 2); } catch { return String(v); }
}

// stepRows pairs a run's flow steps with their records, in flow order,
// with each step's state as the run graph gives it: the record's state
// when it ran, running/waiting for the current step, skipped for steps
// the run passed over or never reached, else pending. Records for steps
// no longer in the snapshot are appended. The running step's row also
// carries waiting (run.waiting: why the engine holds its model call) and
// live (the fan-out items so far, from the run detail's live list).
export function stepRows(run, records, live) {
  const steps = (run && run.snapshot && run.snapshot.steps) || [];
  const recs = new Map();
  for (const r of records || []) recs.set(r.id, r);
  const final = isFinalRun(run && run.state);
  const rows = steps.map((s, i) => {
    const rec = recs.get(s.id) || null;
    recs.delete(s.id);
    let state = 'pending';
    if (rec) state = rec.state;
    else if (run.step === s.id && i === run.pc && (run.state === 'running' || run.state === 'waiting')) state = run.state;
    else if (i < (run.pc || 0) || final) state = 'skipped';
    const row = { step: s, rec, state };
    if (!rec && state === 'running') {
      if (run.waiting) row.waiting = run.waiting;
      if ((live || []).length) row.live = live;
    }
    return row;
  });
  for (const rec of recs.values()) rows.push({ step: { id: rec.id, kind: rec.kind, name: rec.name }, rec, state: rec.state });
  return rows;
}

// rowKey is what a step row on the run page shows, as a string: a row
// whose key did not change since the last refresh is kept as it is (its
// focus, open items and selected text stay).
export const rowKey = (row, i) => JSON.stringify([i, row.step.id, row.step.name || '', row.step.kind, row.state, row.rec, row.waiting || '', row.live || null]);

// itemKey names one fan-out item (open state and focus across refreshes).
export const itemKey = (stepId, index) => `${stepId}#${index}`;

// runLog is the whole run as plain text, for Copy log and Download log:
// a header (flow, run, state, times, input, usage, models, reason), then
// every step in flow order with its state, usage, error, input and output,
// and fan-out items inside their step. Nothing is cut. A step or item the
// engine holds right now says "Waited: <reason>"; one whose model call was
// retried says "Retried: n" and the error that caused it. live is the run
// detail's live items (the running fan-out step).
export function runLog(run, records, live) {
  if (!run) return '';
  const L = [];
  const when = (t) => (t && !isZero(t) ? String(t) : '');
  const dur = runDuration(run);
  L.push(`Flow: ${run.flow_name || run.flow || '?'}${run.flow && run.flow_name ? ` (${run.flow})` : ''}`);
  L.push(`Run: ${run.id || '?'}${run.test ? ' (test run)' : ''}`);
  L.push(`State: ${stateText(run.state) || '?'}`);
  const meta = [
    run.trigger ? `trigger ${run.trigger}` : '',
    run.flow_version ? `version ${run.flow_version}` : '',
    run.actor ? `by ${run.actor}` : '',
  ].filter(Boolean);
  if (meta.length) L.push(`Run by: ${meta.join(' · ')}`);
  if (when(run.created)) L.push(`Created: ${when(run.created)}`);
  if (when(run.started)) L.push(`Started: ${when(run.started)}`);
  if (when(run.ended)) L.push(`Ended: ${when(run.ended)}`);
  if (dur) L.push(`Time: ${fmtDuration(dur)}${isFinalRun(run.state) ? '' : ' so far'}`);
  const usage = usageText(run);
  if (usage) L.push(`Used: ${usage}`);
  if ((run.models || []).length) L.push(`Models: ${run.models.join(', ')}`);
  if (run.reason) L.push(`Reason: ${run.reason}`);
  if (run.note) L.push(`Note: ${run.note}`);
  if (run.approval && when(run.approval.decided)) {
    L.push(`Approval: ${run.approval.ok ? 'approved' : 'rejected'} by ${run.approval.by || 'someone'} at ${when(run.approval.decided)}${run.approval.note ? `: ${run.approval.note}` : ''}`);
  }
  L.push('', '--- input ---', run.input || '(none)');
  const body = (label, text, pad) => {
    if (!text) return;
    L.push(`${pad}--- ${label} ---`);
    L.push(...String(text).split('\n').map((x) => (pad && x ? pad + x : x)));
  };
  const itemLines = (it) => {
    const iu = stepUsage(it);
    L.push('', `  --- item ${it.index + 1}: ${it.title || 'untitled'}${it.kind ? ` (${it.kind})` : ''} · ${stateText(it.state)}${iu ? ` · ${iu}` : ''}`);
    if (it.waiting) L.push(`  Waited: ${it.waiting}`);
    if (it.retries > 0) L.push(`  Retried: ${it.retries}${it.retry_error ? ` (after: ${it.retry_error})` : ''}`);
    if (it.error) L.push(`  Error: ${it.error}`);
    if (it.session) L.push(`  Session: ${it.session}`);
    body('input', it.input, '  ');
    body('output', outputText(it.output), '  ');
  };
  stepRows(run, records, live).forEach(({ step, rec, state, waiting, live: items }, i) => {
    L.push('');
    const u = stepUsage(rec);
    L.push(`=== ${i + 1}. ${step.name || step.id} (${kindLabel(step.kind)}) · ${stateText(state)}${u ? ` · ${u}` : ''} ===`);
    if (!rec) {
      L.push(state === 'skipped' ? 'Did not run.' : state === 'running' ? 'Running now.' : state === 'waiting' ? 'Waiting for approval.' : 'Not run yet.');
      if (waiting) L.push(`Waited: ${waiting}`);
      for (const it of items || []) itemLines(it);
      return;
    }
    if (rec.retries > 0) L.push(`Retried: ${rec.retries}${rec.retry_error ? ` (after: ${rec.retry_error})` : ''}`);
    if (rec.error) L.push(`Error: ${rec.error}`);
    if (rec.jump) L.push(`Went to: ${rec.jump}`);
    if (rec.session) L.push(`Session: ${rec.session}`);
    if (rec.file) L.push(`Wrote: ${rec.file}`);
    body('input', rec.input, '');
    body('output', outputText(rec.output), '');
    for (const it of rec.items || []) itemLines(it);
  });
  return `${L.join('\n')}\n`;
}

// runLogName is the file name of a run's downloaded log.
export function runLogName(run) {
  const safe = (v, def) => String(v || def).replace(/[^A-Za-z0-9._-]+/g, '-').replace(/^-+|-+$/g, '') || def;
  return `${safe(run && (run.flow || run.flow_name), 'flow')}-${safe(run && run.id, 'run')}.log`;
}

// sessionLinks lists the sessions a run started (run.sessions, then any
// in step and item records), without repeats.
export function sessionLinks(run, records) {
  const seen = new Set((run && run.sessions) || []);
  for (const r of records || []) {
    if (r.session) seen.add(r.session);
    for (const it of r.items || []) if (it.session) seen.add(it.session);
  }
  return [...seen].filter(Boolean);
}

// ---------- preview graph ----------

// draftGraph is a draft's graph in the Flow view's shape, like the
// server's FlowGraph (POST /api/flows/validate answers with the same):
// input → steps → end, then/else edges for an if, model nodes asked by
// prompt, plan and fan-out steps. It draws before the server replies and
// when it cannot.
export function draftGraph(flow) {
  const steps = ((flow && flow.steps) || []).filter((s) => s && s.id);
  const nodes = [{ id: 'input', kind: 'input', label: 'Input', sub: (flow && flow.input_hint) || '', state: '' }];
  const edges = [];
  const models = [];
  const ask = (from, m) => {
    if (!m) return;
    if (!models.includes(m)) models.push(m);
    if (!edges.some((e) => e.from === from && e.to === `m:${m}`)) edges.push({ from, to: `m:${m}`, kind: 'ask', label: '', count: 0 });
  };
  const ids = new Set(steps.map((s) => s.id));
  for (const s of steps) {
    nodes.push({ id: `st:${s.id}`, kind: 'step', label: (s.name && s.name.trim()) || s.id, sub: stepSub(s), state: '' });
  }
  nodes.push({ id: 'end', kind: 'end', label: 'End', sub: '', state: '' });
  const target = (i, to) => {
    if (!to) return i + 1 < steps.length ? `st:${steps[i + 1].id}` : 'end';
    if (to === 'end') return 'end';
    return ids.has(to) ? `st:${to}` : '';
  };
  edges.push({ from: 'input', to: steps.length ? `st:${steps[0].id}` : 'end', kind: 'next', label: 'input', count: 1 });
  steps.forEach((s, i) => {
    const from = `st:${s.id}`;
    if (s.kind === 'if') {
      const t = target(i, s.then);
      const e = target(i, s.else);
      if (t) edges.push({ from, to: t, kind: 'then', label: 'then', count: 1 });
      if (e) edges.push({ from, to: e, kind: 'else', label: 'else', count: 1 });
      return;
    }
    edges.push({ from, to: target(i, ''), kind: 'next', label: '', count: 1 });
    if (s.kind === 'prompt' || s.kind === 'plan') ask(from, s.model);
    if (s.kind === 'fan_out' && (s.as || 'prompt') === 'prompt') {
      for (const r of s.routes || []) ask(from, r.model);
      ask(from, s.default_model);
    }
  });
  for (const m of models) nodes.push({ id: `m:${m}`, kind: 'model', label: m, sub: '', state: '' });
  return { nodes, edges, updated: '' };
}

function stepSub(s) {
  const parts = [s.kind];
  switch (s.kind) {
    case 'prompt': case 'plan': parts.push(s.model); break;
    case 'fan_out': parts.push(s.from ? `from ${s.from}` : '', `as ${s.as || 'prompt'}`); break;
    case 'session': parts.push(s.agent, s.workspace || 'scratch'); break;
    case 'if': parts.push(`${s.on || ''} ${s.test || ''}`.trim()); break;
    case 'output': parts.push(s.to); break;
    default: break;
  }
  return parts.filter((p) => p && String(p).trim()).join(' · ');
}

// previewChip says which edges carry a caption in the preview: the
// branches and fan-out items, not every plain "next" arrow.
export const previewChip = (e) => e.kind === 'then' || e.kind === 'else' || e.kind === 'item';

// runEventFor: a store event that concerns run id (its flowrun event, the
// stream reconnecting, or a resync of the flows).
export function runEventFor(kind, payload, id) {
  if (kind === 'events-open') return true;
  if (kind === 'resync') return !!payload && payload.what === 'flows';
  if (kind === 'flowrun') return !!payload && (!id || payload.run === id);
  return false;
}
