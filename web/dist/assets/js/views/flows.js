// Flows view (#/flows): the saved flows as recipes with their runs below
// them, the way an image lists its containers. A card is the flow (name,
// trigger in words, next run, steps, owner, an enabled switch, Edit and
// Run) and its runs, newest first: state (or "Waiting for you"), the
// input on one line, when, usage, Open, Stop while it goes and Run again
// with the same input. Five show; "Show all N runs" reads the rest of
// that flow's runs. Runs of flows no longer listed (deleted) sit in a
// group at the bottom. With no flows yet it offers the built-in
// templates. Kept live from the store's flowrun events (one reload at most
// every 1.5 s, none while the page is hidden); Stop and Run read it again
// at once. The editor is views/flowedit.js, a run's page views/flowrun.js;
// the pure helpers are ../flows.js and ../flowruns.js.

import { api, apiResult } from '../api.js';
import { h, clear, openDialog, confirmDialog, toast, errText, pill, keepFocus } from '../dom.js';
import { store } from '../store.js';
import { relTime } from '../util.js';
import { nextText, runTone, stateText, usageText, runEventFor } from '../flows.js';
import {
  PREVIEW, groupRuns, runTotal, shownRuns, toggleText, needsFetch, runActions, inputLine, lastInput, runWhen,
} from '../flowruns.js';
import { ownerLabel } from '../users.js';

const RELOAD_MS = 1500;
const ALL_RUNS = 500; // the API's most runs in one list
const enc = encodeURIComponent;

// Users and roles: admins can list every person's flows and runs (?all=1);
// kept across visits, like the cards a person opened.
let everyone = false;
const expanded = new Set();

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
  const listEl = h('div', { class: 'flw-list' }, h('p', { class: 't-sec', text: 'Loading…' }));
  const goneEl = h('div', { class: 'flw-gone', hidden: true });
  const noteEl = h('div', { class: 'flw-note', hidden: true });
  root.append(h('section', { class: 'view view-pad view-flows' },
    h('div', { class: 'view-head' },
      h('div', {},
        h('h1', { class: 'view-title', text: 'Flows' }),
        h('p', { class: 'view-sub', text: 'Saved recipes: a trigger and steps that hand work to models and agents. Each flow lists its runs.' })),
      h('div', { class: 'head-actions' }, scope, h('a', { class: 'btn btn-primary', href: '#/flows/new' }, '+ New flow'))),
    noteEl,
    listEl,
    goneEl));

  let gone = false;
  let timer = 0;
  let last = 0;
  let loading = false;
  let again = false;
  let templates = null;
  let flowsOff = false;
  let flowList = [];
  let grouped = { byFlow: new Map(), orphans: [] };
  const busy = new Set(); // run ids with a Stop on its way

  const allQ = () => (allUsers && everyone ? '&all=1' : '');

  async function load() {
    if (loading) { again = true; return; }
    loading = true;
    again = false;
    last = Date.now();
    try {
      const [list, runs] = await Promise.all([
        apiResult(allUsers && everyone ? '/api/flows?all=1' : '/api/flows'),
        api(`/api/flows/runs?per_flow=${PREVIEW}${allQ()}`).catch(() => []),
        api('/api/status').then((st) => { flowsOff = st && st.flows_enabled === false; }).catch(() => {}),
      ]);
      if (gone) return;
      flowList = list.ok ? list.body || [] : [];
      grouped = groupRuns(flowList, runs || []);
      // Opened cards: the rest of that flow's runs.
      const groups = [...grouped.byFlow.entries(), ...grouped.orphans.map((g) => [g.flow, g.runs])];
      await Promise.all(groups.filter(([id, rs]) => needsFetch(rs, expanded.has(id))).map(async ([id]) => {
        const more = await api(`/api/flows/runs?flow=${enc(id)}&limit=${ALL_RUNS}${allQ()}`).catch(() => null);
        if (!more || gone) return;
        const total = more.length;
        const rs = more.map((r) => ({ ...r, flow_runs: Math.max(total, r.flow_runs || 0) }));
        if (grouped.byFlow.has(id)) grouped.byFlow.set(id, rs);
        else for (const g of grouped.orphans) if (g.flow === id) g.runs = rs;
      }));
      if (gone) return;
      if (!list.ok) {
        showNote(list.status === 503 ? 'Flows are not available on this host right now.' : list.message, 'warn');
      } else if (flowsOff) {
        showNote('Flows are turned off in Settings (flows.enabled): runs, by hand or on a schedule, are refused.', 'warn');
      } else {
        showNote('', '');
      }
      await render();
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

  // now reads the list again at once (after Stop, Run, Run again).
  function now() {
    clearTimeout(timer);
    timer = 0;
    load();
  }

  function showNote(text, tone) {
    noteEl.hidden = !text;
    noteEl.className = `flw-note alert ${tone}`;
    noteEl.textContent = text;
  }

  async function render() {
    if (!flowList.length && !templates) templates = await api('/api/flows/templates').catch(() => []);
    if (gone) return;
    keepFocus(root, () => {
      if (!flowList.length) clear(listEl).append(emptyState(templates || []));
      else clear(listEl).append(...flowList.map((f) => flowCard(f)));
      renderGone();
    });
  }

  function renderGone() {
    const { orphans } = grouped;
    goneEl.hidden = !orphans.length;
    clear(goneEl);
    if (!orphans.length) return;
    goneEl.append(
      h('h2', { class: 'group-title', text: 'Runs of deleted flows' }),
      h('p', { class: 't-sec flw-gone-sub', text: 'Their flows were deleted (or moved to another person); the runs stay.' }),
      ...orphans.map((g) => h('section', { class: 'flw-row flw-row-gone', 'aria-label': `Runs of ${g.name}` },
        h('div', { class: 'flw-main' },
          h('span', { class: 'flw-name flw-name-gone', text: g.name }),
          h('div', { class: 'flw-meta' }, h('span', { class: 'flw-tag', text: 'deleted' }), h('span', { text: g.flow }))),
        runsBox(g.flow, g.name, g.runs, null))));
  }

  function flowCard(f) {
    const runs = grouped.byFlow.get(f.id) || [];
    const lr = f.last_run;
    const next = f.enabled && f.trigger && f.trigger.kind === 'schedule' ? nextText(f.next) : '';
    const owner = ownerLabel(f, me);
    const meta = h('div', { class: 'flw-meta' },
      h('span', { class: 'flw-trigger', text: f.trigger_text || 'Manual' }),
      next ? h('span', { text: `Next ${next}` }) : null,
      f.trigger && f.trigger.kind === 'schedule' && !f.enabled ? h('span', { text: 'Schedule paused' }) : null,
      `${(f.steps || []).length} ${(f.steps || []).length === 1 ? 'step' : 'steps'}`,
      owner ? h('span', { class: 'owner-chip', title: 'Owner', text: owner }) : null);
    const skip = f.last_skip ? h('p', { class: 'flw-skip', text: `Skipped ${relTime(f.last_skip.time)}: ${f.last_skip.reason}` }) : null;
    // Missed or late scheduled runs: shown as text (a title is out of reach
    // on phones and for keyboards).
    const note = lr && lr.note ? h('p', { class: 'flw-lastnote', text: `Last run: ${lr.note}` }) : null;
    const sw = h('input', {
      type: 'checkbox', checked: !!f.enabled, 'aria-label': `${f.name}: enabled`, 'data-fkey': `on:${f.id}`,
      onchange: (e) => setEnabled(f, e.target.checked, e.target),
    });
    return h('article', { class: 'flw-row', 'aria-label': f.name || f.id },
      h('div', { class: 'flw-head' },
        h('div', { class: 'flw-main' },
          h('a', { class: 'flw-name', href: `#/flows/${enc(f.id)}`, 'data-fkey': `name:${f.id}`, text: f.name || f.id }),
          f.description ? h('p', { class: 'flw-desc', text: f.description }) : null,
          meta, note, skip),
        h('div', { class: 'flw-actions' },
          h('label', { class: 'flw-switch', title: f.enabled ? 'Enabled: the schedule runs' : 'Disabled: only Run starts it' },
            h('span', { class: 'switch' }, sw, h('span', { class: 'track' })),
            h('span', { class: 'flw-switch-text', text: f.enabled ? 'On' : 'Off' })),
          h('a', { class: 'btn btn-sm', href: `#/flows/${enc(f.id)}`, 'data-fkey': `edit:${f.id}`, 'aria-label': `Edit ${f.name || f.id}` }, 'Edit'),
          h('button', {
            class: 'btn btn-sm btn-primary', type: 'button', 'data-fkey': `run:${f.id}`, 'aria-label': `Run ${f.name || f.id}`,
            onclick: () => runFlow(f, lastInput(runs)),
          }, '▶ Run'))),
      runsBox(f.id, f.name || f.id, runs, f));
  }

  // runsBox is a flow's runs (f is null for a deleted flow).
  function runsBox(id, name, runs, f) {
    const total = runTotal(runs);
    const open = expanded.has(id);
    const box = h('div', { class: 'flw-runs-box' });
    if (!runs.length) {
      box.append(h('p', { class: 'flw-none t-sm', text: 'No runs yet. ▶ Run starts one.' }));
      return box;
    }
    box.append(h('ol', { class: 'flw-runs', 'aria-label': `Runs of ${name}` },
      shownRuns(runs, open).map((r) => runItem(r, f))));
    const more = toggleText(total, open);
    if (more) {
      box.append(h('button', {
        class: 'btn btn-sm btn-ghost flw-more', type: 'button', 'aria-expanded': String(open), 'data-fkey': `more:${id}`,
        onclick: () => {
          if (expanded.has(id)) expanded.delete(id); else expanded.add(id);
          render();
          if (needsFetch(runs, expanded.has(id))) now();
        },
      }, more));
    }
    return box;
  }

  function runItem(r, f) {
    const href = `#/flows/runs/${enc(r.id)}`;
    const line = inputLine(r.input);
    const when = runWhen(r);
    const usage = usageText(r);
    const acts = runActions(r, { flowExists: !!f });
    const owner = ownerLabel(r, me);
    const tone = r.asks ? 'warn' : runTone(r.state);
    const queued = r.state === 'queued';
    return h('li', { class: `flw-run tone-${tone}` },
      h('div', { class: 'flw-run-top' },
        r.asks ? waitingPill(r.asks) : pill(stateText(r.state), runTone(r.state)),
        h('a', {
          class: `flw-run-input${line ? '' : ' is-empty'}`, href, 'data-fkey': `open:${r.id}`,
          title: r.input || 'No input', text: line || 'No input',
        })),
      h('div', { class: 'flw-run-meta' },
        r.test ? h('span', { class: 'flw-tag', text: 'test' }) : r.trigger === 'schedule' ? h('span', { class: 'flw-tag', text: 'scheduled' }) : null,
        owner && owner !== (f ? ownerLabel(f, me) : '') ? h('span', { class: 'owner-chip', text: owner }) : null,
        h('span', { title: localTime(when.start), text: `${queued ? 'Queued' : 'Started'} ${relTime(when.start)}` }),
        when.end
          ? h('span', { title: localTime(when.end), text: `ended ${relTime(when.end)}${when.took ? ` · took ${when.took}` : ''}` })
          : when.took ? h('span', { text: `going for ${when.took}` }) : null,
        usage ? h('span', { text: usage }) : null,
        r.reason ? h('span', { class: 'flw-run-reason', text: r.reason }) : null,
        r.note ? h('span', { class: 'flw-run-note', text: r.note }) : null),
      h('div', { class: 'flw-run-acts' },
        h('a', { class: 'btn btn-sm btn-ghost', href, 'data-fkey': `page:${r.id}`, 'aria-label': `Open the run of ${relTime(when.start)}` }, 'Open'),
        acts.stop ? h('button', {
          class: 'btn btn-sm btn-ghost flw-stop', type: 'button', 'data-fkey': `stop:${r.id}`, disabled: busy.has(r.id),
          onclick: () => stopRun(r),
        }, 'Stop') : null,
        acts.again ? h('button', {
          class: 'btn btn-sm btn-ghost', type: 'button', 'data-fkey': `again:${r.id}`,
          title: line ? `Run again with: ${line}` : 'Run again', onclick: () => runAgain(r, f),
        }, 'Run again') : null));
  }

  async function stopRun(r) {
    if (!(await confirmDialog({ title: 'Stop this run?', message: 'The current step stops (sessions it started are stopped too) and the run ends as canceled.', ok: 'Stop run', danger: true }))) return;
    busy.add(r.id);
    try {
      await api(`/api/flows/runs/${enc(r.id)}/cancel`, { method: 'POST', body: {} });
      toast(`Stopped the run of ${r.flow_name || r.flow}`, 'ok');
    } catch (e) {
      toast(errText(e), 'err');
    } finally {
      busy.delete(r.id);
      now();
    }
  }

  // runAgain starts the run's flow with the run's input (asked first, with
  // that input in the box, when the flow takes one or the run had one).
  async function runAgain(r, f) {
    let input = r.input || '';
    if (((f && f.input_hint) || '').trim() || input) {
      const got = await askInput({ title: `Run ${f.name || f.id} again`, hint: f.input_hint, value: input, ok: 'Run' });
      if (got === undefined) return;
      input = got;
    }
    await start(f, input);
  }

  // runFlow starts a flow from its card, asking for the input first when
  // the flow has an input hint (the last run's input in the box).
  async function runFlow(f, prev) {
    let input = '';
    if ((f.input_hint || '').trim()) {
      const got = await askInput({ title: `Run ${f.name || f.id}`, hint: f.input_hint, value: prev, ok: 'Run' });
      if (got === undefined) return;
      input = got;
    }
    await start(f, input);
  }

  // start runs a flow and keeps the list: the new run shows first under it.
  async function start(f, input) {
    try {
      await api(`/api/flows/${enc(f.id)}/run`, { method: 'POST', body: { input } });
      toast(`Started ${f.name || f.id}: its run is listed under it`, 'ok');
    } catch (e) {
      toast(errText(e), 'err');
    } finally {
      now();
    }
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

const localTime = (iso) => { const t = Date.parse(iso); return t > 0 ? new Date(t).toLocaleString() : ''; };

// waitingPill marks a run whose sessions wait on N approvals: the run page
// answers them (flowrun.js).
function waitingPill(n) {
  return h('span', { class: 'pill warn', title: 'Open the run to answer', text: n > 1 ? `Waiting for you (${n})` : 'Waiting for you' });
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
