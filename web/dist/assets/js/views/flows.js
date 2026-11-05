// Flows view (#/flows): the saved flows (name, trigger in words, next
// run, last run, an enabled switch and Run) and the recent runs below.
// With no flows yet it offers the built-in templates to start from. Kept
// live from the store's flowrun events (one reload at most every 1.5 s,
// none while the page is hidden). The editor is views/flowedit.js, a
// run's page views/flowrun.js; the pure helpers are ../flows.js.

import { api, apiResult } from '../api.js';
import { h, clear, openDialog, toast, errText, pill } from '../dom.js';
import { store } from '../store.js';
import { relTime } from '../util.js';
import { nextText, runTone, stateText, usageText, isZeroTime, runEventFor } from '../flows.js';
import { ownerLabel } from '../users.js';

const RELOAD_MS = 1500;
const enc = encodeURIComponent;

// Users and roles: admins can list every person's flows and runs (?all=1);
// kept across visits.
let everyone = false;

export function mount(root) {
  const me = store.me();
  const allUsers = !!(me && me.hasUsers && store.can('users'));
  const scope = allUsers ? h('div', { class: 'fchips', role: 'group', 'aria-label': 'Whose flows' },
    [['mine', 'Mine'], ['all', 'All users']].map(([k, label]) => h('button', {
      class: 'fchip', type: 'button', 'data-scope': k, 'aria-pressed': String((k === 'all') === everyone),
      onclick: () => {
        everyone = k === 'all';
        for (const b of scope.querySelectorAll('[data-scope]')) b.setAttribute('aria-pressed', String(b.dataset.scope === k));
        load();
      },
    }, label))) : null;
  const listEl = h('div', { class: 'flw-list', 'aria-live': 'polite' }, h('p', { class: 't-sec', text: 'Loading…' }));
  const runsEl = h('div', { class: 'flw-runs' });
  const noteEl = h('div', { class: 'flw-note', hidden: true });
  root.append(h('section', { class: 'view view-pad view-flows' },
    h('div', { class: 'view-head' },
      h('div', {},
        h('h1', { class: 'view-title', text: 'Flows' }),
        h('p', { class: 'view-sub', text: 'Saved recipes: a trigger and steps that hand work to models and agents.' })),
      h('div', { class: 'head-actions' }, scope, h('a', { class: 'btn btn-primary', href: '#/flows/new' }, '+ New flow'))),
    noteEl,
    listEl,
    h('h2', { class: 'group-title', text: 'Recent runs' }),
    runsEl));

  let gone = false;
  let timer = 0;
  let last = 0;
  let loading = false;
  let again = false;
  let templates = null;
  let flowsOff = false;

  async function load() {
    if (loading) { again = true; return; }
    loading = true;
    again = false;
    last = Date.now();
    try {
      const [list, runs] = await Promise.all([
        apiResult(allUsers && everyone ? '/api/flows?all=1' : '/api/flows'),
        api(allUsers && everyone ? '/api/flows/runs?limit=20&all=1' : '/api/flows/runs?limit=20').catch(() => []),
        api('/api/status').then((st) => { flowsOff = st && st.flows_enabled === false; }).catch(() => {}),
      ]);
      if (gone) return;
      if (!list.ok) {
        showNote(list.status === 503 ? 'Flows are not available on this host right now.' : list.message, 'warn');
        renderFlows([]);
      } else if (flowsOff) {
        showNote('Flows are turned off in Settings (flows.enabled): runs, by hand or on a schedule, are refused.', 'warn');
        await renderFlows(list.body || []);
      } else {
        showNote('', '');
        await renderFlows(list.body || []);
      }
      renderRuns(runs || []);
    } catch (e) {
      if (!gone) toast(errText(e), 'err');
    } finally {
      loading = false;
      if (again && !gone) schedule();
    }
  }

  function schedule() {
    if (gone || timer) return;
    if (document.hidden) { again = true; return; }
    timer = setTimeout(() => { timer = 0; if (!gone && !document.hidden) load(); else again = true; },
      Math.max(0, last + RELOAD_MS - Date.now()));
  }

  function showNote(text, tone) {
    noteEl.hidden = !text;
    noteEl.className = `flw-note alert ${tone}`;
    noteEl.textContent = text;
  }

  async function renderFlows(list) {
    if (!list.length) {
      if (!templates) templates = await api('/api/flows/templates').catch(() => []);
      if (gone) return;
      clear(listEl).append(emptyState(templates || []));
      return;
    }
    clear(listEl).append(...list.map((f) => flowRow(f)));
  }

  function flowRow(f) {
    const lr = f.last_run;
    const next = f.enabled && f.trigger && f.trigger.kind === 'schedule' ? nextText(f.next) : '';
    const meta = h('div', { class: 'flw-meta' },
      h('span', { class: 'flw-trigger', text: f.trigger_text || 'Manual' }),
      next ? h('span', { text: `Next ${next}` }) : null,
      f.trigger && f.trigger.kind === 'schedule' && !f.enabled ? h('span', { text: 'Schedule paused' }) : null,
      `${(f.steps || []).length} ${(f.steps || []).length === 1 ? 'step' : 'steps'}`,
      ownerLabel(f, me) ? h('span', { class: 'owner-chip', title: 'Owner', text: ownerLabel(f, me) }) : null);
    const lastEl = lr
      ? h('a', { class: 'flw-last', href: `#/flows/runs/${enc(lr.id)}` },
        pill(stateText(lr.state), runTone(lr.state)), h('span', { class: 't-sm', text: relTime(isZeroTime(lr.ended) ? lr.created : lr.ended) }))
      : h('span', { class: 'flw-last t-sm', text: 'Never run' });
    const skip = f.last_skip ? h('p', { class: 'flw-skip', text: `Skipped ${relTime(f.last_skip.time)}: ${f.last_skip.reason}` }) : null;
    // Missed or late scheduled runs: shown as text (a title is out of reach
    // on phones and for keyboards).
    const note = lr && lr.note ? h('p', { class: 'flw-lastnote', text: `Last run: ${lr.note}` }) : null;
    const sw = h('input', {
      type: 'checkbox', checked: !!f.enabled, 'aria-label': `${f.name}: enabled`,
      onchange: (e) => setEnabled(f, e.target.checked, e.target),
    });
    return h('article', { class: 'flw-row' },
      h('div', { class: 'flw-main' },
        h('a', { class: 'flw-name', href: `#/flows/${enc(f.id)}`, text: f.name || f.id }),
        f.description ? h('p', { class: 'flw-desc', text: f.description }) : null,
        meta, note, skip),
      h('div', { class: 'flw-side' },
        lastEl,
        h('div', { class: 'flw-actions' },
          h('label', { class: 'flw-switch', title: f.enabled ? 'Enabled: the schedule runs' : 'Disabled: only Run starts it' },
            h('span', { class: 'switch' }, sw, h('span', { class: 'track' })),
            h('span', { class: 'flw-switch-text', text: f.enabled ? 'On' : 'Off' })),
          h('button', { class: 'btn btn-sm', type: 'button', onclick: () => runFlow(f) }, '▶ Run'))));
  }

  // setEnabled saves the flow with enabled flipped (a PUT of the whole
  // flow at the version listed; 409 when it changed meanwhile).
  async function setEnabled(f, on, input) {
    const { trigger_text: _t, next: _n, last_run: _l, last_skip: _s, ...flow } = f;
    input.disabled = true;
    try {
      const r = await apiResult(`/api/flows/${enc(f.id)}`, { method: 'PUT', body: { ...flow, enabled: on } });
      if (r.status === 409) toast('This flow changed elsewhere; the list is reloaded.', 'warn');
      else if (!r.ok) { toast(r.message, 'err'); input.checked = !on; }
      else toast(on ? `${f.name} is on` : `${f.name} is off`, 'ok');
    } catch (e) {
      toast(errText(e), 'err');
      input.checked = !on;
    } finally {
      input.disabled = false;
      load();
    }
  }

  function renderRuns(runs) {
    clear(runsEl);
    if (!runs.length) {
      runsEl.append(h('p', { class: 't-sec flw-none', text: 'No runs yet. Press Run on a flow, or open one and try a Test run.' }));
      return;
    }
    runsEl.append(...runs.map((r) => runRow(r)));
  }

  const onEvent = (kind, payload) => { if (runEventFor(kind, payload, '')) schedule(); };
  const unsub = store.subscribe(onEvent);
  const onVisible = () => { if (again) schedule(); };
  window.addEventListener('cerberus:visible', onVisible);
  load();
  return () => {
    gone = true;
    clearTimeout(timer);
    unsub();
    window.removeEventListener('cerberus:visible', onVisible);
  };
}

// runRow is one run in a list: state, flow, when, usage.
export function runRow(r) {
  const when = relTime(isZeroTime(r.ended) ? r.created : r.ended);
  const usage = usageText(r);
  return h('a', { class: `flw-run tone-${runTone(r.state)}`, href: `#/flows/runs/${enc(r.id)}` },
    pill(stateText(r.state), runTone(r.state)),
    h('span', { class: 'flw-run-name', text: r.flow_name || r.flow }),
    h('span', { class: 'flw-run-meta' },
      r.test ? h('span', { class: 'flw-tag', text: 'test' }) : r.trigger === 'schedule' ? h('span', { class: 'flw-tag', text: 'scheduled' }) : null,
      ownerLabel(r, store.me()) ? h('span', { class: 'owner-chip', text: ownerLabel(r, store.me()) }) : null,
      h('span', { text: when }),
      usage ? h('span', { text: usage }) : null,
      r.reason ? h('span', { class: 'flw-run-reason', text: r.reason }) : null,
      r.note ? h('span', { class: 'flw-run-note', text: r.note }) : null));
}

function emptyState(templates) {
  return h('div', { class: 'empty flw-empty' },
    h('p', { class: 'empty-title', text: 'No flows yet' }),
    h('p', { class: 't-sec', text: 'A flow is a trigger plus steps: ask a model, plan, fan out to local models, run an agent, wait for your approval, send the result. Start from a template or a blank flow.' }),
    h('div', { class: 'flw-templates' },
      templates.map((t) => h('a', { class: 'flw-tpl', href: `#/flows/new/${enc(t.key)}` },
        h('span', { class: 'flw-tpl-name', text: t.name }),
        h('span', { class: 'flw-tpl-desc', text: t.description }))),
      h('a', { class: 'flw-tpl', href: '#/flows/new' },
        h('span', { class: 'flw-tpl-name', text: 'Blank flow' }),
        h('span', { class: 'flw-tpl-desc', text: 'One prompt step to build on.' }))));
}

// runFlow starts a saved flow, asking for the input first when the flow
// has an input hint, and opens the run's page.
export async function runFlow(f) {
  let input = '';
  if ((f.input_hint || '').trim()) {
    const got = await askInput({ title: `Run ${f.name || f.id}`, hint: f.input_hint, ok: 'Run' });
    if (got === undefined) return;
    input = got;
  }
  try {
    const run = await api(`/api/flows/${enc(f.id)}/run`, { method: 'POST', body: { input } });
    location.hash = `#/flows/runs/${enc(run.id)}`;
  } catch (e) {
    toast(errText(e), 'err');
  }
}

// askInput asks for a run's input; resolves to the text, or undefined
// when cancelled. Ctrl/⌘+Enter runs.
export function askInput({ title, hint = '', value = '', ok = 'Run' }) {
  return openDialog((close) => {
    const ta = h('textarea', { class: 'textarea flw-input', rows: 4, placeholder: hint || 'Input (optional)', 'aria-label': 'Input', value });
    ta.addEventListener('keydown', (e) => { if (e.key === 'Enter' && (e.ctrlKey || e.metaKey)) { e.preventDefault(); close(ta.value); } });
    requestAnimationFrame(() => ta.focus());
    return {
      title,
      sub: hint ? `Input: ${hint}` : 'Input is optional',
      body: h('div', { class: 'flw-input-box' }, ta),
      foot: [
        h('button', { class: 'btn btn-ghost', type: 'button', onclick: () => close(undefined) }, 'Cancel'),
        h('button', { class: 'btn btn-primary', type: 'button', onclick: () => close(ta.value) }, ok),
      ],
    };
  });
}
