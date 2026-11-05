// Batch view: one fleet task as a live grid of repo × state chips (each a
// link to its session), with Accept all passed and Discard all.

import { api, post } from '../api.js';
import { h, clear, openDialog, confirmDialog, toast, errText, pill } from '../dom.js';
import { store } from '../store.js';
import { stateTone, relTime, repoName } from '../util.js';
import { batchSessions, stateCounts, countChips, batchProgress, resultSummary } from '../fleet.js';

const bid = (id) => `/api/batches/${encodeURIComponent(id)}`;

export function mount(root, { id }) {
  const titleEl = h('div', { class: 'ss-title', text: 'Loading…' });
  const metaEl = h('div', { class: 'ss-meta' });
  const progEl = h('span', { class: 'fleet-prog' });
  const taskEl = h('p', { class: 'batch-task' });
  const countsEl = h('div', { class: 'count-chips' });
  const acceptBtn = h('button', { class: 'btn btn-primary', type: 'button', disabled: true, onclick: () => acceptAll() }, 'Accept all passed');
  const discardBtn = h('button', { class: 'btn btn-danger', type: 'button', disabled: true, onclick: () => discardAll() }, 'Discard all');
  const grid = h('div', { class: 'batch-grid' });
  const resultsEl = h('div', { class: 'batch-results' });

  root.append(h('section', { class: 'view view-batch' },
    h('div', { class: 'ss-head' },
      h('a', { class: 'btn btn-ghost ss-back', href: '#/sessions', 'aria-label': 'Back to sessions' }, '←'),
      h('div', { class: 'ss-headtext' }, titleEl, metaEl), progEl),
    h('div', { class: 'view-pad batch-body' },
      taskEl, countsEl,
      h('div', { class: 'batch-actions' }, acceptBtn, discardBtn),
      resultsEl, grid)));

  let batch = null;
  let fetched = [];
  let busy = false;
  let gone = false;

  const sessions = () => batchSessions(batch ? batch.Sessions : [], fetched, (sid) => store.session(sid));

  function render() {
    if (!batch) return;
    const list = sessions();
    const counts = list.length ? stateCounts(list) : (batch.Counts || {});
    const p = batchProgress(counts);
    titleEl.textContent = batch.Title || batch.Task || batch.ID;
    metaEl.textContent = [`${p.total} repo${p.total === 1 ? '' : 's'}`, batch.Agent + (batch.Model ? ` · ${batch.Model}` : ''), relTime(batch.Created)]
      .filter(Boolean).join(' · ');
    progEl.textContent = `${p.finished}/${p.total} done`;
    taskEl.textContent = batch.Task || '';
    clear(countsEl).append(...countChips(counts).map((c) => pill(`${c.n} ${c.state}`, stateTone(c.state))));
    acceptBtn.disabled = busy || p.passed === 0;
    acceptBtn.textContent = p.passed ? `Accept ${p.passed} passed` : 'Accept all passed';
    const open = list.some((s) => s.State !== 'accepted' && s.State !== 'discarded');
    discardBtn.disabled = busy || !open;
    clear(grid).append(...list.map(cell));
  }

  async function load() {
    try {
      const out = await api(bid(id));
      if (gone) return;
      batch = (out && (out.batch || out.Batch)) || null;
      fetched = (out && (out.sessions || out.Sessions)) || [];
      if (!batch) throw new Error('Fleet task not found');
      render();
    } catch (e) {
      if (gone) return;
      titleEl.textContent = 'Fleet task';
      clear(grid).append(h('div', { class: 'alert err', text: errText(e) }));
    }
  }

  async function acceptAll() {
    const res = await openDialog((close) => {
      const msg = h('textarea', { class: 'textarea', rows: '3', value: batch.Title || '' });
      const push = h('input', { type: 'checkbox' });
      return {
        title: 'Accept all passed',
        sub: 'Each passed session: squash, fast-forward main',
        body: h('div', { class: 'task-form' },
          h('label', { class: 'field' }, h('span', { class: 'field-label', text: 'Commit message' }), msg),
          h('label', { class: 'check' }, push, 'Push to origin/main'),
          h('p', { class: 'field-hint', text: 'Sessions that did not pass are skipped and listed.' })),
        foot: [
          h('button', { class: 'btn btn-ghost', type: 'button', onclick: () => close(null) }, 'Cancel'),
          h('button', { class: 'btn btn-primary', type: 'button', onclick: () => close({ message: msg.value.trim(), push: push.checked }) }, 'Accept'),
        ],
      };
    });
    if (res) run('accept', res);
  }

  async function discardAll() {
    const ok = await confirmDialog({
      title: 'Discard fleet task', ok: 'Discard all', danger: true,
      message: 'This stops every session of this fleet task and deletes their worktrees. Accepted commits stay on main.',
    });
    if (ok) run('discard', {});
  }

  async function run(verb, body) {
    busy = true;
    render();
    try {
      const out = await post(`${bid(id)}/${verb}`, body);
      const results = Array.isArray(out) ? out : (out && (out.results || out.Results)) || [];
      showResults(results);
      toast(resultSummary(results), results.some((r) => r.Result === 'failed') ? 'warn' : 'ok');
    } catch (e) {
      toast(errText(e), 'err');
    }
    busy = false;
    load();
  }

  function showResults(results) {
    clear(resultsEl);
    if (!results.length) return;
    resultsEl.append(h('details', { class: 'checks', open: true },
      h('summary', { text: `Last run: ${resultSummary(results)}` }),
      h('ul', { class: 'batch-res' }, results.map((r) => h('li', {},
        pill(r.Result, r.Result === 'accepted' ? 'ok' : r.Result === 'failed' ? 'err' : 'muted'), ' ',
        h('b', { text: repoName(r.Repo) || r.SessionID }),
        r.Commit ? h('code', { text: ` ${String(r.Commit).slice(0, 10)}` }) : null,
        r.Detail ? h('span', { class: 'batch-res-detail', text: ` ${r.Detail}` }) : null)))));
  }

  const ids = () => new Set(batch ? batch.Sessions : []);
  const unsub = store.subscribe((kind, payload) => {
    if (kind === 'sessions' || (kind === 'session' && payload && ids().has(payload.ID))) render();
  });
  const onVisible = () => load();
  window.addEventListener('cerberus:visible', onVisible);
  load();

  return () => {
    gone = true;
    unsub();
    window.removeEventListener('cerberus:visible', onVisible);
  };
}

function cell(s) {
  const tone = stateTone(s.State);
  return h('a', { class: `batch-cell tone-${tone}`, href: `#/s/${encodeURIComponent(s.ID)}`, title: s.Error || s.WaitingReason || s.State },
    h('span', { class: 'batch-repo', text: repoName(s.Repo || s.Dir) || s.ID }),
    s.State === 'waiting' ? h('span', { class: 'badge-wait', text: 'Waiting' }) : pill(s.State, tone),
    s.Attempt > 1 ? h('span', { class: 'batch-att', text: `attempt ${s.Attempt}` }) : null);
}
