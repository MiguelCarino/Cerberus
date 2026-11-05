// Flow run page (#/flows/runs/<run>): the run's live graph (the Flow
// view's panel from views/flow.js over GET /api/flows/runs/{run}/flow),
// its state, tokens, cost and energy, Approve / Reject while it waits,
// Cancel while it goes, links to the sessions it started and each step's
// input and output (collapsible, with copy; fan-out items inside their
// step). Kept live from the store's flowrun events for this run, one
// fetch at most every second and none while the page is hidden.

import { api } from '../api.js';
import { h, clear, toast, errText, pill, confirmDialog, keepFocus } from '../dom.js';
import { store } from '../store.js';
import { relTime } from '../util.js';
import { flowPanel } from './flow.js';
import { copyText, downloadText } from './chatdom.js';
import { askInput } from './flows.js';
import {
  runTone, stateText, usageText, stepUsage, outputText, stepRows, sessionLinks, isFinalRun, canCancelRun,
  fmtDuration, runDuration, isZeroTime, kindLabel, runEventFor, clip, rowKey, itemKey, runLog, runLogName, retriedText,
} from '../flows.js';

const FETCH_MS = 1000;
const enc = encodeURIComponent;
const OUT_MAX = 20000; // characters shown before "Copy" is the way to get it all

export function mount(root, r) {
  const id = r.run;
  const open = new Set(); // step ids whose details are open (kept across refreshes)
  const openItems = new Set(); // itemKey of fan-out items whose details are open
  let touched = false; // the person opened or closed one: stop auto-opening
  // Live refreshes patch the page in place: rows, actions and session
  // links whose content did not change are kept, so focus, open details
  // and selected text survive (a rebuilt part gives focus back by data-fkey).
  const rowEls = new Map(); // step id → {key, li, det}
  let rowOrder = null;
  let actionsKey = null;
  let sessKey = null;
  const titleEl = h('h1', { class: 'view-title', text: 'Run' });
  const subEl = h('p', { class: 'view-sub' });
  const actionsEl = h('div', { class: 'fr-actions' });
  const factsEl = h('div', { class: 'fr-facts' });
  const noticeEl = h('div', { class: 'fr-notice', 'aria-live': 'polite' });
  const stepsEl = h('ol', { class: 'fr-steps' });
  // Focus inside the steps (a Tab, a click) stops the auto-opening too: a
  // step must not fold away under the person reading or using it.
  stepsEl.addEventListener('focusin', () => { touched = true; });
  const sessEl = h('div', { class: 'fr-sessions' });
  const backEl = h('a', { class: 'fe-back', href: '#/flows' }, '← Flows');
  const panel = flowPanel(`/api/flows/runs/${enc(id)}/flow`, { legend: 'run' });
  root.append(h('section', { class: 'view view-pad view-flowrun' },
    backEl,
    h('div', { class: 'view-head' }, h('div', {}, titleEl, subEl), actionsEl),
    noticeEl, factsEl,
    h('div', { class: 'fr-graph' }, panel.el),
    sessEl,
    h('h2', { class: 'group-title', text: 'Steps' }),
    stepsEl));

  let gone = false;
  let data = null;
  let timer = 0;
  let last = 0;
  let busy = false;
  let again = false;

  async function load() {
    if (busy) { again = true; return; }
    busy = true;
    again = false;
    last = Date.now();
    try {
      const d = await api(`/api/flows/runs/${enc(id)}`);
      if (gone) return;
      data = d;
      render();
    } catch (e) {
      if (gone) return;
      if (!data) {
        clear(stepsEl);
        noticeEl.replaceChildren(h('div', { class: 'empty' },
          h('p', { class: 'empty-title', text: 'Run not found' }),
          h('p', { class: 't-sec', text: `${errText(e)}. Runs are listed on the Flows page.` }),
          h('a', { class: 'btn', href: '#/flows' }, 'Back to Flows')));
      }
    } finally {
      busy = false;
      if (again && !gone) schedule();
    }
  }

  function schedule() {
    if (timer || gone) return;
    if (document.hidden) { again = true; return; }
    timer = setTimeout(() => { timer = 0; load(); }, Math.max(0, last + FETCH_MS - Date.now()));
  }

  function render() {
    const run = data.run;
    const steps = data.steps || [];
    titleEl.textContent = run.flow_name || run.flow;
    backEl.href = run.test ? '#/flows' : `#/flows/${enc(run.flow)}`;
    backEl.textContent = run.test ? '← Flows' : `← ${run.flow_name || run.flow}`;
    subEl.replaceChildren(
      pill(stateText(run.state), runTone(run.state)),
      h('span', { class: 'fr-sub-text', text: [
        run.test ? 'Test run' : run.trigger === 'schedule' ? 'Scheduled run' : 'Run',
        `started ${relTime(isZeroTime(run.started) ? run.created : run.started)}`,
        run.flow_version ? `version ${run.flow_version}` : '',
        run.actor ? `by ${run.actor}` : '',
      ].filter(Boolean).join(' · ') }));
    renderActions(run);
    renderNotice(run);
    renderFacts(run);
    renderSessions(run, steps);
    renderSteps(run, steps, data.live || []);
  }

  function renderActions(run) {
    const key = `${run.state}|${!!run.test}`;
    if (key === actionsKey) return;
    actionsKey = key;
    keepFocus(actionsEl, () => {
      clear(actionsEl);
      if (run.state === 'waiting') {
        actionsEl.append(
          h('button', { class: 'btn btn-primary', type: 'button', 'data-fkey': 'approve', onclick: () => decide(true) }, '✓ Approve'),
          h('button', { class: 'btn btn-danger', type: 'button', 'data-fkey': 'reject', onclick: () => decide(false) }, '✕ Reject'));
      }
      if (canCancelRun(run.state)) {
        actionsEl.append(h('button', { class: 'btn btn-ghost', type: 'button', 'data-fkey': 'cancel', onclick: () => cancel() }, 'Cancel run'));
      }
      if (isFinalRun(run.state) && !run.test) {
        actionsEl.append(h('button', { class: 'btn', type: 'button', 'data-fkey': 'again', onclick: () => again2(run) }, '▶ Run again'));
      }
      // The whole run as text, as it is at the click (data is kept fresh).
      actionsEl.append(
        h('button', { class: 'btn btn-ghost', type: 'button', 'data-fkey': 'copylog', title: 'Every step\'s input and output as text',
          onclick: (e) => { if (data) copyText(runLog(data.run, data.steps || [], data.live || []), e.currentTarget); } }, '⧉ Copy log'),
        h('button', { class: 'btn btn-ghost', type: 'button', 'data-fkey': 'getlog',
          onclick: () => { if (data) downloadText(runLogName(data.run), runLog(data.run, data.steps || [], data.live || [])); } }, '↓ Log'));
    });
  }

  function renderNotice(run) {
    clear(noticeEl);
    if (run.state === 'waiting' && run.approval) {
      noticeEl.append(h('div', { class: 'alert warn fr-approval' },
        h('strong', { text: 'Waiting for your approval' }),
        h('p', { text: run.approval.message || 'Approve to continue, or reject to end the run.' }),
        run.approval.asked && !isZeroTime(run.approval.asked) ? h('span', { class: 't-sm', text: `asked ${relTime(run.approval.asked)}` }) : null));
    } else if (run.approval && !isZeroTime(run.approval.decided)) {
      noticeEl.append(h('p', { class: 'fr-decided t-sm', text: `${run.approval.ok ? 'Approved' : 'Rejected'} by ${run.approval.by || 'someone'} ${relTime(run.approval.decided)}${run.approval.note ? `: ${run.approval.note}` : ''}` }));
    }
    if (run.reason) noticeEl.append(h('div', { class: `alert ${run.state === 'limited' || run.state === 'canceled' ? 'warn' : 'err'}`, text: run.reason }));
    if (run.note) noticeEl.append(h('p', { class: 't-sm fr-note', text: run.note }));
  }

  function renderFacts(run) {
    const dur = runDuration(run);
    const facts = [
      ['Input', run.input ? clip(run.input, 160) : 'none'],
      ['Steps run', String(run.steps_run || 0)],
      ['Time', dur ? `${fmtDuration(dur)}${isFinalRun(run.state) ? '' : ' so far'}` : run.state === 'queued' ? 'queued' : '–'],
      ['Tokens · cost · energy', usageText(run) || 'nothing used yet'],
      ['Models', (run.models || []).join(', ') || '–'],
    ];
    factsEl.replaceChildren(...facts.map(([k, v]) => h('div', { class: 'fr-fact' },
      h('span', { class: 'fr-fact-k', text: k }), h('span', { class: 'fr-fact-v', text: v, title: k === 'Input' ? run.input : null }))));
  }

  function renderSessions(run, steps) {
    const list = sessionLinks(run, steps);
    const key = list.join('\n');
    if (key === sessKey) return;
    sessKey = key;
    sessEl.hidden = !list.length;
    keepFocus(sessEl, () => {
      clear(sessEl);
      if (!list.length) return;
      sessEl.append(h('span', { class: 'fr-fact-k', text: 'Sessions' }),
        ...list.map((s) => h('a', { class: 'chip-tag fr-sess', href: `#/s/${enc(s)}`, 'data-fkey': `sess:${s}` }, `${s} →`)));
    });
  }

  function renderSteps(run, steps, live) {
    const rows = stepRows(run, steps, live);
    // Open the step that is running or waiting, and the last one finished,
    // until the person opens or closes one themselves.
    if (!touched) {
      open.clear();
      const cur = rows.find((x) => x.state === 'running' || x.state === 'waiting');
      const done = rows.filter((x) => x.rec);
      if (cur) open.add(cur.step.id);
      if (done.length) open.add(done[done.length - 1].step.id);
    }
    const order = rows.map((x) => x.step.id).join('\n');
    if (order !== rowOrder) { // a new run shape: build every row
      rowOrder = order;
      rowEls.clear();
      clear(stepsEl);
    }
    rows.forEach((row, i) => {
      const key = rowKey(row, i);
      const had = rowEls.get(row.step.id);
      if (had && had.key === key) {
        if (had.det.open !== open.has(row.step.id)) had.det.open = open.has(row.step.id);
        return;
      }
      const { li, det } = stepItem(row, i);
      if (had) keepFocus(stepsEl, () => had.li.replaceWith(li));
      else stepsEl.append(li);
      rowEls.set(row.step.id, { key, li, det });
    });
  }

  function stepItem({ step, rec, state, waiting, live }, i) {
    const det = h('details', { class: `fr-step tone-${runTone(state)}`, open: open.has(step.id) });
    det.addEventListener('toggle', () => {
      if (det.open === open.has(step.id)) return; // set by a refresh, not by the person
      touched = true;
      if (det.open) open.add(step.id); else open.delete(step.id);
    });
    det.append(h('summary', { 'data-fkey': `step:${step.id}` },
      h('span', { class: 'fr-step-n', text: String(i + 1) }),
      h('span', { class: 'fr-step-name' },
        h('span', { text: step.name || step.id }),
        h('span', { class: 'fr-step-kind', text: kindLabel(step.kind) })),
      h('span', { class: 'fr-step-line', text: rec ? stepUsage(rec) : waiting ? `waiting for the model: ${waiting}` : '' }),
      pill(stateText(state), runTone(state))));
    const body = h('div', { class: 'fr-step-body' });
    if (!rec) {
      if (waiting) body.append(h('p', { class: 'fr-waiting t-sm', text: `Waiting for the model: ${waiting}` }));
      body.append(h('p', { class: 't-sm', text: state === 'running' ? 'Running now: its input and output show when it ends.' : state === 'waiting' ? 'Waiting for your approval.' : state === 'skipped' ? 'This step did not run.' : 'Not run yet.' }));
      if ((live || []).length) body.append(itemsList(step.id, live));
    } else {
      if (rec.retries > 0 && rec.retry_error) body.append(h('p', { class: 't-sm fr-retried', text: `${retriedText(rec.retries)} after: ${rec.retry_error}` }));
      if (rec.error) body.append(h('p', { class: 'fe-msg err', text: rec.error }));
      if (rec.jump) body.append(h('p', { class: 't-sm', text: `Went to ${rec.jump}` }));
      if (rec.session) body.append(h('a', { class: 'fe-link', href: `#/s/${enc(rec.session)}`, 'data-fkey': `step:${step.id}:sess` }, `Open session ${rec.session} →`));
      if (rec.file) body.append(h('p', { class: 't-sm', text: `Wrote ${rec.file} in the run folder` }));
      if (rec.input) body.append(block('Input', rec.input, `step:${step.id}:in`));
      const out = outputText(rec.output);
      if (out) body.append(block('Output', out, `step:${step.id}:out`));
      if ((rec.items || []).length) body.append(itemsList(step.id, rec.items));
    }
    det.append(body);
    return { li: h('li', {}, det), det };
  }

  function itemsList(stepId, items) {
    return h('div', { class: 'fr-items' },
      h('span', { class: 'fr-fact-k', text: `Items (${items.length})` }),
      ...items.map((it) => {
        const k = itemKey(stepId, it.index);
        const d = h('details', { class: `fr-item tone-${runTone(it.state)}`, open: openItems.has(k) },
          h('summary', { 'data-fkey': `item:${k}` },
            h('span', { class: 'fr-step-name' }, h('span', { text: it.title || `Item ${it.index + 1}` }), it.kind ? h('span', { class: 'fr-step-kind', text: it.kind }) : null),
            h('span', { class: 'fr-step-line', text: it.waiting ? `waiting for the model: ${it.waiting}` : stepUsage(it) }),
            pill(stateText(it.state), runTone(it.state))));
        d.addEventListener('toggle', () => {
          if (d.open === openItems.has(k)) return; // as built, not the person
          touched = true;
          if (d.open) openItems.add(k); else openItems.delete(k);
        });
        const b = h('div', { class: 'fr-step-body' });
        if (it.waiting) b.append(h('p', { class: 'fr-waiting t-sm', text: `Waiting for the model: ${it.waiting}` }));
        if (it.retries > 0 && it.retry_error) b.append(h('p', { class: 't-sm fr-retried', text: `${retriedText(it.retries)} after: ${it.retry_error}` }));
        if (it.error) b.append(h('p', { class: 'fe-msg err', text: it.error }));
        if (it.session) b.append(h('a', { class: 'fe-link', href: `#/s/${enc(it.session)}`, 'data-fkey': `item:${k}:sess` }, `Open session ${it.session} →`));
        if (it.input) b.append(block('Input', it.input, `item:${k}:in`));
        const out = outputText(it.output);
        if (out) b.append(block('Output', out, `item:${k}:out`));
        d.append(b);
        return d;
      }));
  }

  async function decide(ok) {
    let note = '';
    if (!ok) {
      const got = await askInput({ title: 'Reject this run?', hint: 'Why (optional; kept with the run)', ok: 'Reject' });
      if (got === undefined) return;
      note = got;
    }
    try {
      const run = await api(`/api/flows/runs/${enc(id)}/approve`, { method: 'POST', body: { ok, note } });
      if (data) { data.run = { ...data.run, ...run, snapshot: data.run.snapshot }; render(); }
      toast(ok ? 'Approved: the run goes on' : 'Rejected: the run ends', ok ? 'ok' : 'warn');
      load();
    } catch (e) { toast(errText(e), 'err'); load(); }
  }

  async function cancel() {
    if (!(await confirmDialog({ title: 'Cancel this run?', message: 'The current step stops (sessions it started are stopped too) and the run ends as canceled.', ok: 'Cancel run', danger: true }))) return;
    try {
      await api(`/api/flows/runs/${enc(id)}/cancel`, { method: 'POST', body: {} });
      load();
    } catch (e) { toast(errText(e), 'err'); load(); }
  }

  async function again2(run) {
    const snap = run.snapshot || {};
    let input = run.input || '';
    if ((snap.input_hint || '').trim() || input) {
      const got = await askInput({ title: `Run ${run.flow_name || run.flow} again`, hint: snap.input_hint, value: input, ok: 'Run' });
      if (got === undefined) return;
      input = got;
    }
    try {
      const next = await api(`/api/flows/${enc(run.flow)}/run`, { method: 'POST', body: { input } });
      location.hash = `#/flows/runs/${enc(next.id)}`;
    } catch (e) { toast(errText(e), 'err'); }
  }

  const unsub = store.subscribe((kind, payload) => {
    if (!runEventFor(kind, payload, id)) return;
    schedule();
    panel.refresh();
  });
  const onVisible = () => { if (again) schedule(); };
  window.addEventListener('cerberus:visible', onVisible);
  requestAnimationFrame(() => { if (!gone) panel.open(); });
  load();

  return () => {
    gone = true;
    clearTimeout(timer);
    unsub();
    panel.destroy();
    window.removeEventListener('cerberus:visible', onVisible);
  };
}

// block is a labelled, copyable text (an input or an output).
function block(label, text, fkey = '') {
  const shown = text.length > OUT_MAX ? `${text.slice(0, OUT_MAX)}\n… (${text.length - OUT_MAX} more characters: Copy takes all)` : text;
  return h('div', { class: 'fr-block' },
    h('div', { class: 'fr-block-head' },
      h('span', { class: 'fr-fact-k', text: label }),
      h('button', { class: 'btn btn-sm btn-ghost', type: 'button', 'aria-label': `Copy the ${label.toLowerCase()}`, 'data-fkey': fkey ? `${fkey}:copy` : null, onclick: (e) => copyText(text, e.currentTarget) }, '⧉ copy')),
    h('pre', { class: 'fe-pre', text: shown }));
}
