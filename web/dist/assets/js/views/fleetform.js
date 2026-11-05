// New fleet task: one instruction, one session per repo. A repo finder
// (has-file and name filters over /api/repos/find) feeds a checklist; the
// batch is created with POST /api/batches and opened in the batch view.

import { api, post } from '../api.js';
import { h, clear, openDialog, toast, errText } from '../dom.js';
import { expandTask } from '../fleet.js';
import { field, loadChoices, agentPickers, textWithMic } from './form.js';
import { micControl } from './mic.js';

export async function newFleetTask() {
  let choices;
  try {
    choices = await loadChoices();
  } catch (e) {
    toast(errText(e), 'err');
    return;
  }
  const batch = await openDialog((close) => fleetForm({ ...choices, close }));
  if (batch && batch.ID) location.hash = `#/b/${encodeURIComponent(batch.ID)}`;
}

function fleetForm({ agents, models, defaultAgent, close }) {
  // ---------- repo finder ----------
  let matches = [];
  const selected = new Set();
  const has = h('input', { class: 'input', placeholder: 'e.g. carino-navbar.js', autocapitalize: 'off', spellcheck: 'false' });
  const name = h('input', { class: 'input', placeholder: 'e.g. Carino-*', autocapitalize: 'off', spellcheck: 'false' });
  const findBtn = h('button', { class: 'btn', type: 'button', onclick: () => find() }, 'Find repos');
  const all = h('input', { type: 'checkbox', onchange: () => { setAll(all.checked); } });
  const countEl = h('span', { class: 'rf-count' });
  const listEl = h('div', { class: 'rf-list', role: 'group', 'aria-label': 'Repos' });
  const finder = h('div', { class: 'rf' },
    h('div', { class: 'form-row' }, field('Has file', has), field('Name', name)),
    h('div', { class: 'rf-bar' }, h('label', { class: 'check' }, all, 'Select all'), countEl, findBtn),
    listEl);
  // The list always reflects the filters as typed: a change re-runs the
  // search after a pause, and Start stays disabled until it has (so typing a
  // filter and going straight to the task never starts the old selection).
  let searched = null; // the query of the list shown; null while none
  let seq = 0;
  let debounce = 0;
  const query = () => {
    const q = new URLSearchParams();
    if (has.value.trim()) q.set('has', has.value.trim());
    if (name.value.trim()) q.set('name', name.value.trim());
    return q.toString();
  };
  const stale = () => searched !== query();
  for (const inp of [has, name]) {
    inp.addEventListener('keydown', (e) => { if (e.key === 'Enter') { e.preventDefault(); find(); } });
    inp.addEventListener('input', () => {
      clearTimeout(debounce);
      debounce = setTimeout(find, 400);
      syncCount();
    });
  }

  async function find() {
    clearTimeout(debounce);
    const q = query();
    const mine = ++seq;
    findBtn.disabled = true;
    clear(listEl).append(h('div', { class: 'fp-empty', text: 'Searching…' }));
    let out;
    try {
      out = await api(`/api/repos/find${q ? '?' + q : ''}`);
    } catch (e) {
      if (mine !== seq) return;
      matches = [];
      selected.clear();
      searched = q; // shown as an error, not as "searching"
      clear(listEl).append(h('div', { class: 'alert err', text: errText(e) }));
      findBtn.disabled = false;
      syncCount();
      return;
    }
    if (mine !== seq) return; // a newer search is on its way
    matches = (Array.isArray(out) ? out : []).sort((a, b) => a.Name.localeCompare(b.Name));
    searched = q;
    // A filtered search starts with every clean repo ticked (dirty ones may
    // fail to accept later, so they are opt-in). The unfiltered list shown
    // when the dialog opens ticks nothing: the CLI refuses to run without a
    // filter, and the web must not be easier to aim at every repo.
    selected.clear();
    if (q) for (const m of matches) if (!m.Dirty) selected.add(m.Path);
    findBtn.disabled = false;
    renderList();
  }

  function setAll(on) {
    selected.clear();
    if (on) for (const m of matches) selected.add(m.Path);
    renderList();
  }

  function renderList() {
    clear(listEl);
    if (!matches.length) listEl.append(h('div', { class: 'fp-empty', text: 'No repos match.' }));
    for (const m of matches) {
      const box = h('input', { type: 'checkbox', onchange: () => {
        if (box.checked) selected.add(m.Path); else selected.delete(m.Path);
        syncCount();
        syncPreview();
      } });
      box.checked = selected.has(m.Path);
      listEl.append(h('label', { class: 'rf-row' }, box,
        h('span', { class: 'rf-name', text: m.Name }),
        m.Dirty ? h('span', { class: 'rf-dirty', title: 'Uncommitted changes: accepting may fail', text: 'dirty' }) : null,
        h('span', { class: 'rf-path', text: m.Path })));
    }
    syncCount();
    syncPreview();
  }

  function syncCount() {
    countEl.textContent = stale() ? 'Filters changed: searching…' : `${selected.size} of ${matches.length} selected`;
    all.checked = matches.length > 0 && selected.size === matches.length;
    all.indeterminate = selected.size > 0 && selected.size < matches.length;
    submit.disabled = busy || selected.size === 0 || stale();
    submit.textContent = selected.size ? `Start ${selected.size} session${selected.size === 1 ? '' : 's'}` : 'Start';
  }

  // ---------- task ----------
  const task = h('textarea', { class: 'textarea', rows: '5', placeholder: 'What should the agent do in each repo?' });
  const mic = micControl(task);
  const preview = h('p', { class: 'field-hint rf-preview' });
  const syncPreview = () => {
    const first = matches.find((m) => selected.has(m.Path));
    const uses = /\{(repo|name)\}/.test(task.value);
    preview.textContent = first && uses ? `${first.Name}: ${expandTask(task.value, first)}` : '';
  };
  task.addEventListener('input', syncPreview);
  const title = h('input', { class: 'input', placeholder: 'Optional short title' });
  const pick = agentPickers({ agents, models, defaultAgent });
  const attempts = h('input', { class: 'input', type: 'number', min: '1', max: '10', placeholder: 'Default', inputmode: 'numeric' });
  let busy = false;
  const submit = h('button', { class: 'btn btn-primary', type: 'submit', disabled: true }, 'Start');

  const form = h('form', { class: 'task-form', onsubmit: async (e) => {
    e.preventDefault();
    if (stale()) { find(); toast('The repo list was out of date: check it, then start', 'warn'); return; }
    if (!selected.size) { toast('Pick at least one repo', 'warn'); return; }
    if (!task.value.trim()) { toast('Describe the task', 'warn'); return; }
    busy = true;
    syncCount();
    const spec = {
      Repos: matches.filter((m) => selected.has(m.Path)).map((m) => m.Path),
      Task: task.value.trim(), Title: title.value.trim(),
      Agent: pick.agent(), Model: pick.model(), EscalateTo: pick.escalate(),
      MaxAttempts: Math.max(0, parseInt(attempts.value, 10) || 0),
    };
    try {
      const out = await post('/api/batches', spec);
      const batch = (out && (out.batch || out.Batch)) || out;
      toast(`Fleet task started: ${spec.Repos.length} session${spec.Repos.length === 1 ? '' : 's'}`, 'ok');
      close(batch || {});
    } catch (err) {
      toast(errText(err), 'err');
      busy = false;
      syncCount();
    }
  } },
  h('div', { class: 'field' }, h('span', { class: 'field-label', text: 'Repos' }), finder),
  textWithMic('Task', task, mic),
  h('p', { class: 'field-hint', text: 'Use {repo} for the repo path and {name} for its folder name.' }),
  preview,
  field('Title', title),
  h('div', { class: 'form-row' }, pick.agentField, pick.modelField),
  pick.hint,
  h('div', { class: 'form-row' }, pick.escalateField, field('Max attempts', attempts)),
  h('div', { class: 'form-actions' },
    h('button', { class: 'btn btn-ghost', type: 'button', onclick: () => close(null) }, 'Cancel'), submit));

  find();
  return { title: 'New fleet task', sub: 'One session per repo, each in its own worktree', body: form };
}
