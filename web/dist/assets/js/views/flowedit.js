// Flow editor (#/flows/new, #/flows/new/<template>, #/flows/<id>): name,
// description, input hint, the trigger (Manual, or Schedule with a
// friendly picker and the raw schedule / cron field), limits (collapsed)
// and the steps as an ordered list of cards, each a short form for its
// kind with model and agent pickers (add, duplicate, move, delete). A
// preview graph beside the form (below it on phones) is drawn with
// flowgraph.js from POST /api/flows/validate, which also gives the
// problems shown on the fields. Test run runs the draft once and shows
// each step's output as it arrives (flowrun events); Save checks the
// version (409: changed elsewhere). Unsaved edits survive a visit to
// another view in this tab (restored with a notice). Text is set as text.

import { api, apiResult } from '../api.js';
import { h, clear, openDialog, confirmDialog, toast, errText, pill, keepFocus } from '../dom.js';
import { store } from '../store.js';
import { debounce, relTime } from '../util.js';
import { loadChoices, modelLabel } from './form.js';
import { askInput } from './flows.js';
import { copyText, downloadText } from './chatdom.js';
import {
  SCHEDULE_MODES, DAY_WORDS, WEEK_ORDER, parseSchedule, buildSchedule, triggerText, STEP_KINDS, kindLabel,
  stepFields, newStep, duplicateStep, moveStep, removeStep, renameStepRefs, placeholdersFor, cleanFlow,
  blankFlow, fromTemplate, LIMIT_FIELDS, indexProblems, problemsAt, problemSummary, problemText, draftGraph,
  previewChip, cronFor, limitsText, stepSummary, clip, runTone, stateText, usageText, stepUsage, outputText, stepRows, isFinalRun, canCancelRun, runEventFor, selectDefault, setStepField,
  runLog, runLogName,
} from '../flows.js';
import { layoutGraph, fitText, FONT, isCircle, circleOf } from '../flowgraph.js';

const NS = 'http://www.w3.org/2000/svg';
const enc = encodeURIComponent;
const TEST_MS = 800; // at most one test-run fetch per this
const copy = (v) => JSON.parse(JSON.stringify(v));

// Unsaved edits kept across a visit elsewhere: {key, draft, base}.
let stash = null;
let lastTestInput = '';

export function mount(root, r = {}) {
  const isNew = r.id === 'new';
  let flowId = isNew ? '' : r.id;
  let base = null; // the saved flow (null for a new one)
  let draft = null;
  let dirty = false;
  let idx = indexProblems([]);
  let graph = null;
  let choices = { models: [], agents: [], defaultAgent: '' };
  let repos = [];
  let templates = [];
  let gone = false;
  let saving = false;
  let valSeq = 0;
  const collapsed = new Set();
  let slots = []; // {owner, step, field, known, el}: where problems show
  let owner = 'form'; // which render pass is adding slots

  // ---------- skeleton ----------
  const titleEl = h('h1', { class: 'view-title fe-title', text: isNew ? 'New flow' : 'Flow' });
  const statusEl = h('p', { class: 'view-sub fe-status', role: 'status' });
  const delBtn = h('button', { class: 'btn btn-ghost btn-danger', type: 'button', hidden: isNew, onclick: () => del() }, 'Delete');
  const testBtn = h('button', { class: 'btn', type: 'button', onclick: () => testRun() }, '▶ Test run');
  const saveBtn = h('button', { class: 'btn btn-primary', type: 'button', onclick: () => save() }, 'Save');
  const noticeEl = h('div', { class: 'fe-notice', hidden: true });
  const tplEl = h('div', { class: 'fe-tpls', hidden: !isNew });
  const formEl = h('div', { class: 'fe-form' });
  const stepsEl = h('div', { class: 'fe-steps' });
  const svgBox = h('div', { class: 'fe-graph', role: 'group', 'aria-label': 'Preview of the flow' });
  const sumEl = h('p', { class: 'fe-sum', role: 'status' });
  const probList = h('ul', { class: 'fe-problems' });
  const testEl = h('div', { class: 'fe-test', hidden: true, 'aria-live': 'polite' });
  const side = h('aside', { class: 'fe-side', 'aria-label': 'Preview and test run' },
    h('div', { class: 'fe-card fe-preview' },
      h('div', { class: 'fe-card-head' }, h('h2', { class: 'fe-h', text: 'Preview' }), sumEl),
      svgBox, probList),
    testEl);
  root.append(h('section', { class: 'view view-pad view-flowedit' },
    h('a', { class: 'fe-back', href: '#/flows' }, '← Flows'),
    h('div', { class: 'view-head fe-head' },
      h('div', { class: 'fe-head-text' }, titleEl, statusEl),
      h('div', { class: 'fe-head-actions' }, delBtn, testBtn, saveBtn)),
    noticeEl, tplEl,
    h('div', { class: 'fe-grid' }, h('div', { class: 'fe-main' }, formEl, stepsEl), side)));

  const onUnload = (e) => { if (dirty) { e.preventDefault(); e.returnValue = ''; } };
  window.addEventListener('beforeunload', onUnload);
  const onKey = (e) => {
    if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 's') { e.preventDefault(); save(); }
  };
  document.addEventListener('keydown', onKey);

  // ---------- load ----------
  async function init() {
    const key = flowId || 'new';
    const [view, ch, rp, tp] = await Promise.all([
      isNew ? Promise.resolve(null) : apiResult(`/api/flows/${enc(flowId)}`),
      loadChoices().catch(() => choices),
      api('/api/repos').catch(() => []),
      isNew ? api('/api/flows/templates').catch(() => []) : Promise.resolve([]),
    ]);
    if (gone) return;
    choices = ch || choices;
    repos = rp || [];
    templates = tp || [];
    if (view && !view.ok) {
      clear(formEl).append(h('div', { class: 'empty' },
        h('p', { class: 'empty-title', text: view.status === 404 ? 'No such flow' : 'Could not load the flow' }),
        h('p', { class: 't-sec', text: view.status === 404 ? 'It may have been deleted. Go back to Flows to pick another, or make a new one.' : view.message }),
        h('a', { class: 'btn', href: '#/flows' }, 'Back to Flows')));
      testBtn.disabled = true; saveBtn.disabled = true; delBtn.hidden = true;
      return;
    }
    if (view) {
      base = view.body.flow;
      draft = copy(base);
      idx = indexProblems(view.body.problems);
    } else {
      const t = r.template && templates.find((x) => x.key === r.template);
      draft = t ? fromTemplate(t) : blankFlow(defaults());
    }
    if (stash && stash.key === key && !r.template && (!base || stash.base === base.version)) {
      draft = stash.draft;
      dirty = true;
      notice('Your unsaved changes are back.', 'Discard them', () => {
        draft = base ? copy(base) : blankFlow(defaults());
        dirty = false; stash = null; renderAll(); validate();
      });
    }
    stash = null;
    renderTemplates();
    renderAll();
    validate();
  }

  const defaults = () => {
    const avail = choices.agents.find((a) => a.available && a.name === choices.defaultAgent) || choices.agents.find((a) => a.available);
    return { model: 'default', agent: avail ? avail.name : '' };
  };

  function notice(text, action, fn) {
    clear(noticeEl);
    noticeEl.hidden = !text;
    if (!text) return;
    noticeEl.append(h('span', { text }), action ? h('button', { class: 'btn btn-sm btn-ghost', type: 'button', onclick: () => { fn(); notice(''); } }, action) : null);
  }

  // ---------- header ----------
  function renderHead() {
    titleEl.textContent = (draft.name || '').trim() || (flowId ? flowId : 'New flow');
    const parts = [];
    if (base) parts.push(`Version ${base.version}`, `saved ${relTime(base.updated)}`);
    else parts.push('Not saved yet');
    if (dirty) parts.push('unsaved changes');
    statusEl.textContent = parts.join(' · ');
    statusEl.classList.toggle('fe-dirty', dirty);
    delBtn.hidden = !flowId;
  }

  function changed({ steps = false, head = true } = {}) {
    dirty = true;
    if (head) renderHead();
    if (steps) renderSteps();
    validateSoon();
    drawGraph(draftGraphOrServer());
  }

  // ---------- templates (new flows) ----------
  function renderTemplates() {
    clear(tplEl);
    tplEl.hidden = !!flowId;
    if (tplEl.hidden) return;
    const pick = async (t) => {
      if (dirty && !(await confirmDialog({ title: 'Replace this draft?', message: 'Starting from a template replaces what you have written here.', ok: 'Replace' }))) return;
      draft = t ? fromTemplate(t) : blankFlow(defaults());
      dirty = false;
      collapsed.clear();
      renderAll();
      validate();
    };
    tplEl.append(h('span', { class: 'fe-tpls-label', text: 'Start from' }),
      h('button', { class: 'fe-tpl', type: 'button', onclick: () => pick(null), title: 'One prompt step to build on' }, 'Blank'),
      ...templates.map((t) => h('button', { class: 'fe-tpl', type: 'button', title: t.description, onclick: () => pick(t) }, t.name)));
  }

  // ---------- the form ----------
  function renderAll() {
    renderHead();
    renderForm();
    renderSteps();
    drawGraph(draftGraphOrServer());
  }

  function renderForm() {
    clear(formEl);
    owner = 'form';
    slots = slots.filter((sl) => sl.owner !== 'form');
    const nameIn = h('input', { class: 'input', value: draft.name || '', maxlength: 120, placeholder: 'Morning briefing', 'aria-label': 'Name', 'data-field': 'name' });
    nameIn.addEventListener('input', () => { draft.name = nameIn.value; changed(); });
    const descIn = h('textarea', { class: 'textarea fe-desc', rows: 2, value: draft.description || '', placeholder: 'What it is for (optional)', 'aria-label': 'Description', 'data-field': 'description' });
    descIn.addEventListener('input', () => { draft.description = descIn.value; changed(); });
    const hintIn = h('input', { class: 'input', value: draft.input_hint || '', placeholder: 'A question · Describe the website · (empty: no input)', 'aria-label': 'Input hint', 'data-field': 'input_hint' });
    hintIn.addEventListener('input', () => { draft.input_hint = hintIn.value; changed(); });
    formEl.append(h('div', { class: 'fe-card' },
      fieldBox('Name', nameIn, '', 'name'),
      fieldBox('Description', descIn, '', 'description'),
      fieldBox('Input hint', hintIn, 'Run asks for input when this is set; a scheduled run uses it as the input.', 'input_hint')),
    triggerCard(), limitsCard());
  }

  function fieldBox(label, control, hint, field, step = '') {
    const prob = h('div', { class: 'fe-prob' });
    addSlot({ step, field, el: prob });
    return h('label', { class: 'field fe-field' },
      h('span', { class: 'field-label', text: label }), control,
      hint ? h('span', { class: 'fe-hint', text: hint }) : null, prob);
  }

  function triggerCard() {
    const t = draft.trigger || (draft.trigger = { kind: 'manual' });
    const isSched = t.kind === 'schedule';
    const seg = h('div', { class: 'fe-seg', role: 'radiogroup', 'aria-label': 'Trigger' },
      ['manual', 'schedule'].map((k) => h('button', {
        class: `toggle-btn${t.kind === k || (!t.kind && k === 'manual') ? ' active' : ''}`, type: 'button', role: 'radio',
        'aria-checked': String(t.kind === k || (!t.kind && k === 'manual')),
        onclick: () => {
          if (t.kind === k) return;
          t.kind = k;
          if (k === 'schedule' && !t.schedule) t.schedule = 'weekdays 08:00';
          changed();
          renderForm();
        },
      }, k === 'manual' ? 'Manual' : 'Schedule')));
    const words = h('p', { class: 'fe-words', text: triggerText(t) });
    const card = h('div', { class: 'fe-card' },
      h('div', { class: 'fe-card-head' }, h('h2', { class: 'fe-h', text: 'Trigger' }), words), seg);
    if (isSched) {
      const m = parseSchedule(t.schedule);
      const raw = h('input', { class: 'input mono', value: t.schedule || '', placeholder: '0 8 * * 1-5', 'aria-label': 'Schedule (cron or friendly)', spellcheck: 'false', autocapitalize: 'off', 'data-field': 'trigger.schedule' });
      const picker = h('div', { class: 'fe-picker' });
      const set = (next) => {
        t.schedule = buildSchedule(next);
        raw.value = t.schedule;
        words.textContent = triggerText(t);
        changed();
      };
      const drawPicker = (mm) => {
        clear(picker);
        const mode = h('select', { class: 'select', 'aria-label': 'Repeat' },
          SCHEDULE_MODES.map(([v, l]) => h('option', { value: v, selected: v === mm.mode, text: l })));
        mode.addEventListener('change', () => {
          const next = { ...mm, mode: mode.value };
          if (next.mode === 'cron' && !next.cron) next.cron = cronFor(mm);
          set(next); drawPicker(next);
        });
        picker.append(mode);
        if (mm.mode === 'every') {
          const n = h('input', { class: 'input fe-num', type: 'number', min: 1, max: 1440, value: String(mm.n), 'aria-label': 'Every how many' });
          const unit = h('select', { class: 'select', 'aria-label': 'Unit' },
            h('option', { value: 'm', selected: mm.unit === 'm', text: 'minutes' }), h('option', { value: 'h', selected: mm.unit === 'h', text: 'hours' }));
          const upd = () => { mm.n = Number(n.value) || 1; mm.unit = unit.value; set(mm); };
          n.addEventListener('input', upd); unit.addEventListener('change', upd);
          picker.append(n, unit);
        } else if (mm.mode !== 'cron') {
          if (mm.mode === 'days') {
            picker.append(h('div', { class: 'fe-days', role: 'group', 'aria-label': 'Days' }, WEEK_ORDER.map((d) => {
              const on = mm.days.includes(d);
              return h('button', {
                class: `toggle-btn fe-day${on ? ' active' : ''}`, type: 'button', 'aria-pressed': String(on),
                onclick: () => { mm.days = on ? mm.days.filter((x) => x !== d) : [...mm.days, d]; set(mm); drawPicker(mm); },
              }, DAY_WORDS[d]);
            })));
          }
          const at = h('input', { class: 'input fe-time', type: 'time', value: mm.at, 'aria-label': 'At' });
          at.addEventListener('change', () => { if (at.value) { mm.at = at.value; set(mm); } });
          picker.append(h('span', { class: 'fe-at', text: 'at' }), at);
        }
      };
      drawPicker(m);
      raw.addEventListener('input', () => {
        t.schedule = raw.value;
        words.textContent = triggerText(t);
        const mm = parseSchedule(raw.value);
        if (mm.ok) drawPicker(mm);
        changed();
      });
      const en = h('input', { type: 'checkbox', checked: !!draft.enabled, 'aria-label': 'Enabled' });
      en.addEventListener('change', () => { draft.enabled = en.checked; changed(); });
      card.append(picker,
        fieldBox('Schedule', raw, 'Friendly (every 6h, daily 08:00, weekdays 08:00, mon,thu 19:30) or a cron line: minute hour day month weekday. Local time.', 'trigger.schedule'),
        h('label', { class: 'fe-switch' }, h('span', { class: 'switch' }, en, h('span', { class: 'track' })),
          h('span', { text: 'Enabled: run on this schedule' })));
    } else {
      card.append(h('p', { class: 'fe-hint', text: 'Runs when you press Run (here or in the list), from the CLI (cerberus flow run) or the API.' }));
    }
    const prob = h('div', { class: 'fe-prob' });
    addSlot({ step: '', field: 'trigger.kind', el: prob });
    card.append(prob);
    return card;
  }

  function limitsCard() {
    const lim = draft.limits || (draft.limits = {});
    const d = h('details', { class: 'fe-card fe-limits' },
      h('summary', {}, h('span', { class: 'fe-h', text: 'Limits' }), h('span', { class: 'fe-hint', text: limitsText(lim) })));
    const grid = h('div', { class: 'fe-limit-grid' });
    for (const [k, label, def] of LIMIT_FIELDS) {
      const inp = h('input', { class: 'input', type: 'number', min: 0, step: k === 'max_cost_usd' ? '0.01' : '1', inputmode: k === 'max_cost_usd' ? 'decimal' : 'numeric', value: lim[k] ? String(lim[k]) : '', placeholder: String(def), 'aria-label': label, 'data-field': `limits.${k}` });
      inp.addEventListener('input', () => {
        const v = Number(inp.value);
        if (v > 0) lim[k] = v; else delete lim[k];
        d.querySelector('summary .fe-hint').textContent = limitsText(lim);
        changed();
      });
      grid.append(fieldBox(label, inp, '', `limits.${k}`));
    }
    d.append(grid, h('p', { class: 'fe-hint', text: 'Empty means the default. A run that passes a limit stops as "limited" with the reason.' }));
    return d;
  }

  // ---------- steps ----------
  function renderSteps(focus = '') {
    // Keep the focused control focused across the rebuild (Tab out of an
    // id field rebuilds the cards).
    const act = document.activeElement;
    if (!focus && act && stepsEl.contains(act) && act.dataset && act.dataset.focus) focus = act.dataset.focus;
    clear(stepsEl);
    owner = 'steps';
    slots = slots.filter((sl) => sl.owner !== 'steps');
    const steps = draft.steps || (draft.steps = []);
    stepsEl.append(h('div', { class: 'fe-steps-head' },
      h('h2', { class: 'group-title', text: `Steps (${steps.length})` }),
      steps.length > 1 ? h('button', { class: 'btn btn-sm btn-ghost', type: 'button', onclick: () => { if (collapsed.size) collapsed.clear(); else steps.forEach((s) => collapsed.add(s)); renderSteps(); } }, collapsed.size ? 'Expand all' : 'Collapse all') : null));
    const stepsProb = h('div', { class: 'fe-prob' });
    addSlot({ step: '', field: 'steps', el: stepsProb });
    stepsEl.append(stepsProb);
    if (!steps.length) {
      stepsEl.append(h('div', { class: 'empty fe-steps-empty' },
        h('p', { class: 'empty-title', text: 'No steps' }),
        h('p', { class: 't-sec', text: 'Add a first step: a Prompt asks a model, a Plan splits work, an Agent session does it.' })));
    }
    steps.forEach((s, i) => stepsEl.append(stepCard(s, i, steps)));
    stepsEl.append(h('button', { class: 'btn fe-add', type: 'button', onclick: () => addStep() }, '+ Add step'));
    for (const sl of slots) paintSlot(sl);
    if (focus) {
      let el = stepsEl.querySelector(`[data-focus="${focus}"]`);
      // A button that is now disabled (moved to an end): its card's number.
      if (el && el.disabled) el = stepsEl.querySelector(`[data-focus="${focus.split('|')[0]}|toggle"]`);
      if (el) el.focus();
    }
  }

  function stepCard(s, i, steps) {
    const fields = stepFields(s, steps);
    const known = ['id', 'kind', 'name', ...fields.map((f) => f.key)];
    const isCol = collapsed.has(s);
    const upd = (opts) => changed(opts);
    const nameIn = h('input', { class: 'input fe-step-name', value: s.name || '', placeholder: kindLabel(s.kind), 'aria-label': `Step ${i + 1} name`, maxlength: 80 });
    nameIn.addEventListener('input', () => { s.name = nameIn.value; upd(); });
    const idIn = h('input', { class: 'input mono fe-step-id', value: s.id, 'aria-label': `Step ${i + 1} id`, title: `Step id: later steps read its output as {steps.${s.id}.output}`, spellcheck: 'false', autocapitalize: 'off', maxlength: 32, 'data-focus': `${i}|id` });
    idIn.addEventListener('change', () => {
      const next = idIn.value.trim();
      if (!next || next === s.id) { idIn.value = s.id; return; }
      const wasCol = collapsed.has(s);
      draft.steps = renameStepRefs(draft.steps, s.id, next);
      if (wasCol) collapsed.add(draft.steps[i]);
      // After the focus has moved on (Tab), so the rebuild keeps it there.
      setTimeout(() => { if (!gone) changed({ steps: true }); }, 0);
    });
    const btn = (label, title, fn, disabled = false) => h('button', { class: 'btn btn-sm btn-ghost fe-icon', type: 'button', title, 'aria-label': `${title} (step ${i + 1})`, disabled, onclick: fn, 'data-focus': `${i}|${title}` }, label);
    const head = h('div', { class: 'fe-step-head' },
      h('button', {
        class: 'fe-step-toggle', type: 'button', 'aria-expanded': String(!isCol), 'aria-label': `${isCol ? 'Expand' : 'Collapse'} step ${i + 1}`,
        onclick: () => { if (isCol) collapsed.delete(s); else collapsed.add(s); renderSteps(`${i}|toggle`); }, 'data-focus': `${i}|toggle`,
      }, h('span', { class: 'fe-step-n', text: String(i + 1) })),
      h('span', { class: `fe-kind fe-kind-${s.kind}`, text: kindLabel(s.kind) }),
      nameIn, idIn,
      h('div', { class: 'fe-step-tools' },
        btn('↑', 'Move up', () => { draft.steps = moveStep(draft.steps, i, -1); changed(); renderSteps(String(i - 1) + '|Move up'); }, i === 0),
        btn('↓', 'Move down', () => { draft.steps = moveStep(draft.steps, i, 1); changed(); renderSteps(String(i + 1) + '|Move down'); }, i === steps.length - 1),
        btn('⧉', 'Duplicate', () => { draft.steps = duplicateStep(draft.steps, i); changed({ steps: true }); }),
        btn('✕', 'Delete', async () => {
          if (!(await confirmDialog({ title: `Delete step ${s.id}?`, message: 'Steps that point at it show a problem until you change them.', ok: 'Delete', danger: true }))) return;
          draft.steps = removeStep(draft.steps, i);
          changed({ steps: true });
        })));
    const hasErr = problemsAt(idx, s.id, '', []).some((p) => p.severity !== 'warning');
    const card = h('article', { class: `fe-step${isCol ? ' is-collapsed' : ''}${hasErr ? ' has-error' : ''}`, id: `fe-step-${i}`, 'data-step': s.id }, head);
    const own = h('div', { class: 'fe-prob' });
    addSlot({ step: s.id, field: '', known, el: own });
    card.append(own);
    if (isCol) {
      card.append(h('p', { class: 'fe-step-sum', text: stepSummary(s) }));
      return card;
    }
    const body = h('div', { class: 'fe-step-body' });
    for (const f of fields) {
      if (f.show === false) continue;
      body.append(fieldFor(s, i, f, steps));
    }
    card.append(body);
    return card;
  }

  function fieldFor(s, i, f, steps) {
    const focusKey = `${i}|${f.key}`;
    const set = (v, opts = {}) => { s[f.key] = v; changed(opts); };
    let control;
    switch (f.type) {
      case 'template': {
        const ta = h('textarea', { class: 'textarea fe-tpl-text', rows: Math.min(10, Math.max(f.key === 'system' ? 2 : 3, String(s[f.key] || '').split('\n').length + 1)), value: s[f.key] || '', placeholder: f.placeholder || '', 'aria-label': f.label, 'data-focus': focusKey, spellcheck: 'false' });
        ta.addEventListener('input', () => set(ta.value));
        const chips = h('div', { class: 'fe-chips', 'aria-label': 'Insert a placeholder' },
          placeholdersFor(steps, i, !!f.inItem).map((p) => h('button', {
            class: 'fe-chip', type: 'button', title: `Insert ${p}`,
            onclick: () => { insertAt(ta, p); set(ta.value); },
          }, p)));
        return fieldBlock(f, h('div', { class: 'fe-tpl-box' }, ta, chips), s.id);
      }
      case 'text':
        control = h('input', { class: 'input', value: s[f.key] || '', placeholder: f.placeholder || '', 'aria-label': f.label, 'data-focus': focusKey, spellcheck: 'false' });
        control.addEventListener('input', () => set(control.value));
        break;
      case 'number':
        control = h('input', { class: 'input fe-num', type: 'number', min: f.min || 0, max: f.max || null, inputmode: 'numeric', value: s[f.key] ? String(s[f.key]) : '', placeholder: f.placeholder || '', 'aria-label': f.label, 'data-focus': focusKey });
        control.addEventListener('input', () => set(Number(control.value) || 0));
        break;
      case 'bool': {
        const cb = h('input', { type: 'checkbox', checked: !!s[f.key], 'data-focus': focusKey });
        // A JSON prompt may feed a fan-out: other cards' "Parts from" change.
        cb.addEventListener('change', () => { set(cb.checked, { steps: true }); refocus(focusKey); });
        const prob = h('div', { class: 'fe-prob' });
        addSlot({ step: s.id, field: f.key, el: prob });
        return h('div', { class: 'fe-field' }, h('label', { class: 'check fe-check' }, cb, h('span', { text: f.label })), prob);
      }
      case 'select': case 'step': {
        const opts = [...(f.options || [])];
        const cur = s[f.key] || '';
        if (f.type === 'step' && !opts.some(([v]) => v === '')) opts.unshift(['', 'Pick a step']);
        const val = selectDefault(s, f.key);
        // A state test's value is a select with no empty option: keep the
        // step holding what it shows ("passed"), or it saves "".
        if (f.key === 'value' && val !== cur) s.value = val;
        if (val && !opts.some(([v]) => v === val)) opts.push([val, `${val} (not found)`]);
        control = h('select', { class: 'select', 'aria-label': f.label, 'data-focus': focusKey },
          opts.map(([v, l]) => h('option', { value: v, selected: v === val, text: l })));
        // Some selects change which fields show (as, workspace, test, to).
        const reshape = ['as', 'workspace', 'test', 'to'].includes(f.key);
        control.addEventListener('change', () => {
          setStepField(s, f.key, control.value);
          changed({ steps: reshape });
          if (reshape) refocus(focusKey);
        });
        break;
      }
      case 'model': control = modelSelect(s[f.key] || '', (v) => set(v), f.label, focusKey); break;
      case 'agent': control = agentSelect(s[f.key] || '', (v) => set(v), f.label, focusKey); break;
      case 'repo': control = repoSelect(s[f.key] || '', (v) => set(v), f.label, focusKey); break;
      case 'routes': return fieldBlock(f, routesEditor(s, i), s.id);
      default: control = h('span', {});
    }
    return fieldBlock(f, control, s.id);
  }

  function fieldBlock(f, control, step) {
    const prob = h('div', { class: 'fe-prob' });
    addSlot({ step, field: f.key, el: prob });
    const isGroup = f.type === 'routes' || f.type === 'template';
    return h(isGroup ? 'div' : 'label', { class: `field fe-field fe-f-${f.type}` },
      h('span', { class: 'field-label', text: f.label }), control,
      f.hint ? h('span', { class: 'fe-hint', text: f.hint }) : null, prob);
  }

  function refocus(key) {
    requestAnimationFrame(() => { const el = stepsEl.querySelector(`[data-focus="${key}"]`); if (el) el.focus(); });
  }

  function modelSelect(cur, onChange, label, focusKey) {
    const list = choices.models || [];
    const opts = [['default', 'Default model'], ...list.map((m) => [m.ID, modelLabel(m)])];
    if (cur && !opts.some(([v]) => v === cur)) opts.push([cur, `${cur} · not installed`]);
    const sel = h('select', { class: 'select', 'aria-label': label, 'data-focus': focusKey },
      cur ? null : h('option', { value: '', selected: true, text: 'Pick a model' }),
      opts.map(([v, l]) => h('option', { value: v, selected: v === cur, text: l })));
    sel.addEventListener('change', () => onChange(sel.value));
    return sel;
  }

  function agentSelect(cur, onChange, label, focusKey) {
    const list = choices.agents || [];
    const opts = list.map((a) => [a.name, a.available ? a.name : `${a.name} (not installed)`]);
    if (cur && !opts.some(([v]) => v === cur)) opts.push([cur, `${cur} (not configured)`]);
    const sel = h('select', { class: 'select', 'aria-label': label, 'data-focus': focusKey },
      cur ? null : h('option', { value: '', selected: true, text: 'Pick an agent' }),
      opts.map(([v, l]) => h('option', { value: v, selected: v === cur, text: l })));
    sel.addEventListener('change', () => onChange(sel.value));
    return sel;
  }

  function repoSelect(cur, onChange, label, focusKey) {
    const opts = repos.map((x) => [x.path, x.name]);
    if (cur && !opts.some(([v]) => v === cur)) opts.push([cur, cur]);
    if (!opts.length) {
      const inp = h('input', { class: 'input mono', value: cur, placeholder: '/home/me/src/project', 'aria-label': label, 'data-focus': focusKey });
      inp.addEventListener('input', () => onChange(inp.value));
      return inp;
    }
    const sel = h('select', { class: 'select', 'aria-label': label, 'data-focus': focusKey },
      cur ? null : h('option', { value: '', selected: true, text: 'Pick a repository' }),
      opts.map(([v, l]) => h('option', { value: v, selected: v === cur, text: l })));
    sel.addEventListener('change', () => onChange(sel.value));
    return sel;
  }

  function routesEditor(s, i) {
    const routes = s.routes || (s.routes = []);
    const box = h('div', { class: 'fe-routes' });
    routes.forEach((rt, n) => {
      const kind = h('input', { class: 'input mono', value: rt.kind || '', placeholder: 'html', 'aria-label': `Route ${n + 1} kind`, spellcheck: 'false', autocapitalize: 'off' });
      kind.addEventListener('input', () => { rt.kind = kind.value; changed(); });
      const model = modelSelect(rt.model || '', (v) => { rt.model = v; changed(); }, `Route ${n + 1} model`, `${i}|route${n}`);
      box.append(h('div', { class: 'fe-route' }, kind, h('span', { class: 'fe-arrow', 'aria-hidden': 'true', text: '→' }), model,
        h('button', { class: 'btn btn-sm btn-ghost fe-icon', type: 'button', 'aria-label': `Remove route ${n + 1}`, title: 'Remove', onclick: () => { routes.splice(n, 1); changed({ steps: true }); } }, '✕')));
    });
    box.append(h('button', { class: 'btn btn-sm', type: 'button', 'data-focus': `${i}|addroute`, onclick: () => { routes.push({ kind: '', model: 'default' }); changed({ steps: true }); refocus(`${i}|addroute`); } }, '+ Route'));
    return box;
  }

  function addStep() {
    openDialog((close) => ({
      title: 'Add a step',
      sub: 'Steps run in order; an If can jump ahead.',
      body: h('div', { class: 'more-list fe-kinds' }, STEP_KINDS.map((k) => h('button', {
        class: 'more-item fe-kind-item', type: 'button',
        onclick: () => close(k.kind),
      }, h('span', { class: 'more-label', text: k.label }), h('span', { class: 'more-sub', text: k.blurb })))),
    })).then((kind) => {
      if (!kind || gone) return;
      draft.steps = [...draft.steps, newStep(kind, draft.steps, defaults())];
      changed({ steps: true });
      const n = draft.steps.length - 1;
      requestAnimationFrame(() => {
        const card = document.getElementById(`fe-step-${n}`);
        if (card) { card.scrollIntoView({ block: 'nearest', behavior: 'smooth' }); const f = card.querySelector('.fe-step-body [data-focus]'); if (f) f.focus({ preventScroll: true }); }
      });
    });
  }

  // ---------- problems ----------
  function addSlot(sl) {
    sl.owner = owner;
    slots.push(sl);
    paintSlot(sl);
  }

  function paintSlot(sl) {
    if (!sl.el) return;
    const list = problemsAt(idx, sl.step, sl.field, sl.known || []);
    clear(sl.el).append(...list.map((p) => h('p', { class: `fe-msg ${p.severity === 'warning' ? 'warn' : 'err'}`, text: p.message })));
  }

  function paintProblems() {
    for (const sl of slots) paintSlot(sl);
    for (const card of stepsEl.querySelectorAll('.fe-step')) {
      const id = card.dataset.step;
      card.classList.toggle('has-error', ((idx.by.get(id)) || []).some((p) => p.severity !== 'warning'));
    }
    sumEl.textContent = problemSummary(idx);
    sumEl.className = `fe-sum ${idx.errors ? 'err' : idx.warnings ? 'warn' : 'ok'}`;
    clear(probList);
    for (const p of idx.all.slice(0, 12)) {
      probList.append(h('li', {}, h('button', {
        class: `fe-msg-link ${p.severity === 'warning' ? 'warn' : 'err'}`, type: 'button',
        onclick: () => jumpTo(p.step, p.field),
      }, problemText(p))));
    }
    if (idx.all.length > 12) probList.append(h('li', { class: 'fe-hint', text: `and ${idx.all.length - 12} more` }));
  }

  function jumpTo(step, field) {
    let el = null;
    if (step) {
      const i = draft.steps.findIndex((s) => s.id === step);
      if (i >= 0) {
        if (collapsed.has(draft.steps[i])) { collapsed.delete(draft.steps[i]); renderSteps(); }
        el = stepsEl.querySelector(`[data-focus="${i}|${(field || '').split('.')[0]}"]`) || document.getElementById(`fe-step-${i}`);
      }
    } else {
      el = formEl.querySelector(`[data-field="${field}"]`) || formEl.querySelector('[data-field="name"]');
      const det = el && el.closest('details');
      if (det) det.open = true;
    }
    if (!el) return;
    el.scrollIntoView({ block: 'center', behavior: 'smooth' });
    if (el.focus) el.focus({ preventScroll: true });
  }

  // ---------- validation and the preview ----------
  const validateSoon = debounce(() => validate(), 450);

  async function validate() {
    if (!draft) return;
    const mine = ++valSeq;
    try {
      const res = await apiResult('/api/flows/validate', { method: 'POST', body: cleanFlow(draft) });
      if (gone || mine !== valSeq) return;
      if (res.ok && res.body) {
        idx = indexProblems(res.body.problems);
        graph = res.body.graph || null;
      } else if (res.status === 400 && res.body && res.body.problems) {
        idx = indexProblems(res.body.problems);
        graph = null;
      } else {
        graph = null;
      }
    } catch {
      if (gone || mine !== valSeq) return;
      graph = null;
    }
    paintProblems();
    drawGraph(draftGraphOrServer());
  }

  // The server's graph when it matches the draft's steps, else the
  // draft's own (instant while typing).
  function draftGraphOrServer() {
    const local = draftGraph(draft);
    if (!graph) return local;
    const ids = (g) => g.nodes.map((n) => n.id).join('|');
    return ids(graph) === ids(local) ? graph : local;
  }

  function drawGraph(g) {
    clear(svgBox);
    if (!g || !g.nodes.length) return;
    const laid = layoutGraph(g, 'tb');
    const pad = 12;
    const W = Math.max(1, laid.width + pad * 2);
    const H = Math.max(1, laid.height + pad * 2);
    const svg = sv('svg', { class: 'fe-svg', viewBox: `${-pad} ${-pad} ${W} ${H}`, width: W, height: H, role: 'img', 'aria-label': `${laid.nodes.filter((n) => n.kind === 'step').length} steps` });
    const edges = sv('g', { class: 'fl-edges' });
    const chips = sv('g', { class: 'fl-chips' });
    const nodes = sv('g', { class: 'fl-nodes' });
    for (const e of laid.edges) {
      edges.append(sv('g', { class: `fl-edge fl-edge-${e.kind} fl-${e.style} fe-edge-${e.kind}` },
        sv('path', { class: 'fl-line', d: e.geo.d }), sv('polygon', { class: 'fl-arrow', points: e.geo.arrow })));
      if (!previewChip(e) || !e.text) continue;
      const { w, h: ch } = e.chip;
      chips.append(sv('g', { class: `fl-chip fe-chip-${e.kind}`, transform: `translate(${e.geo.mid.x - w / 2},${e.geo.mid.y - ch / 2})` },
        sv('rect', { width: w, height: ch, rx: ch / 2 }),
        sv('text', { x: w / 2, y: ch / 2, 'text-anchor': 'middle', 'dominant-baseline': 'central', text: e.text })));
    }
    for (const n of laid.nodes) nodes.append(previewNode(n, laid.dir));
    svg.append(edges, nodes, chips);
    svgBox.append(svg);
  }

  function previewNode(n, dir) {
    const stepId = n.kind === 'step' ? n.id.slice(3) : '';
    const bad = stepId && ((idx.by.get(stepId)) || []).some((p) => p.severity !== 'warning');
    const g = sv('g', {
      class: `fl-node fl-node-${n.kind}${stepId ? ' fl-nav' : ''}${bad ? ' fe-bad' : ''}`,
      transform: `translate(${n.x},${n.y})`,
      tabindex: stepId ? 0 : null, role: stepId ? 'button' : null,
      'aria-label': stepId ? `Step ${n.label}: go to its card` : null,
    }, sv('title', { text: [n.label, n.sub].filter(Boolean).join('\n') }));
    if (isCircle(n)) {
      const c = circleOf({ ...n, x: 0, y: 0 }, dir);
      g.append(sv('circle', { class: 'fl-circle fl-tone-muted', cx: c.cx, cy: c.cy, r: c.r }),
        sv('text', { class: 'fe-model-glyph', x: c.cx, y: c.cy, 'text-anchor': 'middle', 'dominant-baseline': 'central', text: 'M' }));
      const tx = c.r * 2 + 10;
      const lab = fitText(n.label, n.w - tx - 4, FONT.label);
      g.append(sv('text', { class: 'fl-label', x: tx, y: c.cy + 4, text: lab.text }));
    } else {
      g.append(sv('rect', { class: 'fl-card', width: n.w, height: n.h, rx: 12 }));
      const lab = fitText(n.label, n.w - 24, FONT.label);
      const sub = fitText(n.sub, n.w - 24, FONT.sub, true);
      g.append(sv('text', { class: 'fl-label', x: 12, y: sub.text ? 30 : 42, text: lab.text }));
      if (sub.text) g.append(sv('text', { class: 'fl-sub', x: 12, y: 50, text: sub.text }));
    }
    if (stepId) {
      const go = () => jumpTo(stepId, '');
      g.addEventListener('click', go);
      g.addEventListener('keydown', (e) => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); go(); } });
    }
    return g;
  }

  // ---------- save ----------
  async function save(force = null) {
    if (saving || !draft) return;
    saving = true;
    saveBtn.disabled = true;
    try {
      const body = cleanFlow(draft);
      if (force) body.version = force;
      else if (base) body.version = base.version;
      const res = flowId
        ? await apiResult(`/api/flows/${enc(flowId)}`, { method: 'PUT', body })
        : await apiResult('/api/flows', { method: 'POST', body });
      if (gone) return;
      if (res.status === 409 && res.body && res.body.flow) { saving = false; saveBtn.disabled = false; conflict(res.body.flow); return; }
      if (res.status === 400 && res.body && res.body.problems) {
        idx = indexProblems(res.body.problems);
        paintProblems();
        toast(`Not saved: ${problemSummary(idx)}`, 'err');
        return;
      }
      if (!res.ok) { toast(`Not saved: ${res.message}`, 'err'); return; }
      const view = res.body;
      base = view.flow;
      const keepCol = draft.steps.map((s) => collapsed.has(s));
      draft = copy(base);
      collapsed.clear();
      draft.steps.forEach((s, i) => { if (keepCol[i]) collapsed.add(s); });
      dirty = false;
      if (!flowId) {
        flowId = base.id;
        history.replaceState(null, '', `#/flows/${enc(flowId)}`);
        renderTemplates();
      }
      idx = indexProblems(view.problems);
      renderAll();
      paintProblems();
      toast(`Saved ${base.name} (version ${base.version})`, 'ok');
    } catch (e) {
      if (!gone) toast(errText(e), 'err');
    } finally {
      saving = false;
      saveBtn.disabled = false;
    }
  }

  async function conflict(theirs) {
    const choice = await openDialog((close) => ({
      title: 'Changed elsewhere',
      sub: `${theirs.name || theirs.id}: now version ${theirs.version}`,
      body: h('p', { class: 'dlg-msg', text: `This flow was saved somewhere else (version ${theirs.version}, ${relTime(theirs.updated)}) after you opened it. Load that version and lose your changes here, or save yours over it.` }),
      foot: [
        h('button', { class: 'btn btn-ghost', type: 'button', onclick: () => close('') }, 'Keep editing'),
        h('button', { class: 'btn', type: 'button', onclick: () => close('load') }, 'Load theirs'),
        h('button', { class: 'btn btn-primary', type: 'button', onclick: () => close('mine') }, 'Save mine over it'),
      ],
    }));
    if (gone) return;
    if (choice === 'load') {
      base = theirs; draft = copy(theirs); dirty = false; collapsed.clear();
      renderAll(); validate();
    } else if (choice === 'mine') {
      base = { ...base, version: theirs.version, updated: theirs.updated };
      save(theirs.version);
    }
  }

  async function del() {
    if (!flowId) return;
    const ok = await confirmDialog({ title: `Delete ${draft.name || flowId}?`, message: 'The flow and its kept versions are removed; its runs stay in the list.', ok: 'Delete', danger: true });
    if (!ok) return;
    try {
      await api(`/api/flows/${enc(flowId)}`, { method: 'DELETE' });
      dirty = false;
      toast('Flow deleted', 'ok');
      location.hash = '#/flows';
    } catch (e) {
      toast(errText(e), 'err');
    }
  }

  // ---------- test run ----------
  let test = null; // {run, steps}
  let testTimer = 0;
  let testLast = 0;
  let testBusy = false;
  let testAgain = false;

  async function testRun() {
    if (!draft) return;
    const input = await askInput({ title: 'Test run', hint: draft.input_hint, value: lastTestInput, ok: 'Run the draft' });
    if (input === undefined || gone) return;
    lastTestInput = input;
    testBtn.disabled = true;
    try {
      const res = await apiResult('/api/flows/test', { method: 'POST', body: { flow: cleanFlow(draft), input } });
      if (gone) return;
      if (res.status === 400 && res.body && res.body.problems) {
        idx = indexProblems(res.body.problems);
        paintProblems();
        toast(`Fix these first: ${problemSummary(idx)}`, 'err');
        return;
      }
      if (!res.ok) { toast(res.message, 'err'); return; }
      test = { run: res.body, steps: [] };
      renderTest();
      testEl.scrollIntoView({ block: 'nearest', behavior: 'smooth' });
      fetchTest();
    } catch (e) {
      if (!gone) toast(errText(e), 'err');
    } finally {
      testBtn.disabled = false;
    }
  }

  async function fetchTest() {
    if (!test) return;
    if (testBusy) { testAgain = true; return; }
    testBusy = true;
    testAgain = false;
    testLast = Date.now();
    try {
      const d = await api(`/api/flows/runs/${enc(test.run.id)}`);
      if (gone || !test || d.run.id !== test.run.id) return;
      test = { run: d.run, steps: d.steps || [] };
      renderTest();
    } catch { /* the next event retries */ } finally {
      testBusy = false;
      if (testAgain) scheduleTest();
    }
  }

  function scheduleTest() {
    if (testTimer || gone) return;
    if (document.hidden) { testAgain = true; return; }
    testTimer = setTimeout(() => { testTimer = 0; fetchTest(); }, Math.max(0, testLast + TEST_MS - Date.now()));
  }

  const unsub = store.subscribe((kind, payload) => {
    if (test && runEventFor(kind, payload, test.run.id)) scheduleTest();
  });
  const onVisible = () => { if (testAgain) scheduleTest(); };
  window.addEventListener('cerberus:visible', onVisible);

  // The test panel is rebuilt only when what it shows changed, and gives
  // keyboard focus back (data-fkey) when it is.
  let testKey = null;
  function renderTest() {
    const key = test ? JSON.stringify([test.run.id, test.run.state, test.run.step, test.run.pc, test.run.reason, test.run.approval,
      test.run.tokens, test.run.cost_usd, test.run.energy_wh, test.steps]) : '';
    if (key === testKey) return;
    testKey = key;
    keepFocus(testEl, buildTest);
  }

  function buildTest() {
    clear(testEl);
    testEl.hidden = !test;
    if (!test) return;
    const run = test.run;
    const usage = usageText(run);
    const actions = h('div', { class: 'fe-test-actions' },
      run.state === 'waiting' ? [
        h('button', { class: 'btn btn-sm btn-primary', type: 'button', 'data-fkey': 'approve', onclick: () => decide(true) }, 'Approve'),
        h('button', { class: 'btn btn-sm btn-danger', type: 'button', 'data-fkey': 'reject', onclick: () => decide(false) }, 'Reject'),
      ] : null,
      canCancelRun(run.state) ? h('button', { class: 'btn btn-sm btn-ghost', type: 'button', 'data-fkey': 'cancel', onclick: () => cancelTest() }, 'Cancel') : null,
      h('a', { class: 'btn btn-sm', href: `#/flows/runs/${enc(run.id)}`, 'data-fkey': 'page' }, 'Run page'),
      h('button', { class: 'btn btn-sm btn-ghost', type: 'button', 'data-fkey': 'copylog', onclick: (e) => copyText(runLog(test.run, test.steps || []), e.currentTarget) }, '⧉ Copy log'),
      h('button', { class: 'btn btn-sm btn-ghost', type: 'button', 'data-fkey': 'getlog', 'aria-label': 'Download the log', onclick: () => downloadText(runLogName(test.run), runLog(test.run, test.steps || [])) }, '↓ Log'),
      isFinalRun(run.state) ? h('button', { class: 'btn btn-sm btn-ghost', type: 'button', 'aria-label': 'Close the test run', 'data-fkey': 'close', onclick: () => { test = null; renderTest(); } }, '✕') : null);
    testEl.append(h('div', { class: 'fe-card' },
      h('div', { class: 'fe-card-head' },
        h('h2', { class: 'fe-h', text: 'Test run' }),
        pill(stateText(run.state), runTone(run.state))),
      h('p', { class: 'fe-hint', text: [run.input ? `Input: ${clip(run.input, 120)}` : 'No input', usage].filter(Boolean).join(' · ') }),
      run.reason ? h('p', { class: `fe-msg ${run.state === 'limited' ? 'warn' : 'err'}`, text: run.reason }) : null,
      run.approval && run.state === 'waiting' ? h('p', { class: 'fe-msg warn', text: run.approval.message || 'Waiting for your approval' }) : null,
      actions,
      h('ol', { class: 'fe-test-steps' }, stepRows(run, test.steps).map((row) => testRow(row)))));
  }

  function testRow({ step, rec, state }) {
    const text = rec ? outputText(rec.output) : '';
    const line = rec ? stepUsage(rec) : '';
    const outEl = text ? h('div', { class: 'fe-out' },
      h('pre', { class: 'fe-pre', text: clip(text, 4000) }),
      h('button', { class: 'btn btn-sm btn-ghost fe-copy', type: 'button', 'data-fkey': `copy:${step.id}`, onclick: (e) => copyText(text, e.currentTarget) }, '⧉ copy')) : null;
    return h('li', { class: `fe-test-step tone-${runTone(state)}` },
      h('div', { class: 'fe-test-head' },
        h('span', { class: 'fe-test-name', text: step.name || step.id }),
        pill(stateText(state), runTone(state))),
      line ? h('p', { class: 'fe-hint', text: line }) : null,
      rec && rec.error ? h('p', { class: 'fe-msg err', text: rec.error }) : null,
      rec && rec.session ? h('a', { class: 'fe-link', href: `#/s/${enc(rec.session)}`, 'data-fkey': `sess:${step.id}` }, `Session ${rec.session} →`) : null,
      outEl);
  }

  async function decide(ok) {
    if (!test) return;
    try {
      const r = await api(`/api/flows/runs/${enc(test.run.id)}/approve`, { method: 'POST', body: { ok, note: '' } });
      test.run = r;
      renderTest();
      fetchTest();
    } catch (e) { toast(errText(e), 'err'); }
  }

  async function cancelTest() {
    if (!test) return;
    try {
      const r = await api(`/api/flows/runs/${enc(test.run.id)}/cancel`, { method: 'POST', body: {} });
      test.run = r;
      renderTest();
      fetchTest();
    } catch (e) { toast(errText(e), 'err'); }
  }

  init().catch((e) => { if (!gone) toast(errText(e), 'err'); });

  return () => {
    gone = true;
    if (dirty && draft) stash = { key: flowId || 'new', draft, base: base ? base.version : 0 };
    validateSoon.cancel();
    clearTimeout(testTimer);
    unsub();
    window.removeEventListener('cerberus:visible', onVisible);
    window.removeEventListener('beforeunload', onUnload);
    document.removeEventListener('keydown', onKey);
  };
}

// ---------- small helpers ----------

function sv(tag, attrs = {}, ...children) {
  const el = document.createElementNS(NS, tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (v == null || v === false) continue;
    if (k === 'text') el.textContent = v;
    else el.setAttribute(k, String(v));
  }
  for (const c of children.flat()) if (c) el.append(c);
  return el;
}

function insertAt(ta, text) {
  const s = ta.selectionStart ?? ta.value.length;
  const e = ta.selectionEnd ?? ta.value.length;
  ta.value = ta.value.slice(0, s) + text + ta.value.slice(e);
  const p = s + text.length;
  ta.focus();
  ta.setSelectionRange(p, p);
}
