// Eval run page (#/evals/runs/<run>, E1/E2): the run's state, its setups
// ranked (fewer gate iterations rank higher), and the matrix: one row per session (setup and repeat), the
// criteria as columns grouped by gate, ✓ ✗ – ! per cell. A cell opens the
// judge's rationale and evidence. The matrix scrolls inside its own box,
// so the page fits a 360 px phone. Kept live from evalrun events for this
// run, one fetch a second at most, none while the page is hidden.

import { api, post } from '../api.js';
import { h, clear, toast, errText, pill, confirmDialog, openDialog } from '../dom.js';
import { store } from '../store.js';
import { relTime } from '../util.js';
import {
  gateColumns, matrixRows, cellMark, cellTone, cellText, runTone, slotTone, isFinalRun, canCancel, canRejudge,
  fmtWall, usageText, runTotals, gatesText, evalRunEventFor, iterationsText, cellHistory, feedbackLabel,
  trajectoryPath, trajectoryFile,
} from '../evals.js';

const FETCH_MS = 1000;
const enc = encodeURIComponent;
const isZero = (t) => !t || String(t).startsWith('0001-');

export function mount(root, r) {
  const id = r.run;
  const titleEl = h('h1', { class: 'view-title', text: 'Eval run' });
  const subEl = h('p', { class: 'view-sub' });
  const actionsEl = h('div', { class: 'fr-actions' });
  const noticeEl = h('div', { 'aria-live': 'polite' });
  const factsEl = h('div', { class: 'fr-facts' });
  const rankEl = h('div', { class: 'ev-rank-wrap' });
  const matrixEl = h('div', { class: 'ev-matrix-wrap', tabindex: '0', role: 'region', 'aria-label': 'Results by setup and criterion' });
  root.append(h('section', { class: 'view view-pad view-evalrun' },
    h('a', { class: 'fe-back', href: '#/usage' }, '← Usage'),
    h('div', { class: 'view-head' }, h('div', {}, titleEl, subEl), actionsEl),
    noticeEl, factsEl,
    h('h2', { class: 'group-title', text: 'Ranking' }), rankEl,
    h('h2', { class: 'group-title', text: 'Results' }),
    h('p', { class: 't-sm ev-legend', text: '✓ pass · ✗ fail · – skipped · ! error · · not judged yet. A cell shows the judge\'s reasons.' }),
    matrixEl));

  let gone = false;
  let data = null;
  let timer = 0;
  let last = 0;
  let busy = false;
  let again = false;
  let actionsKey = '';

  async function load() {
    if (busy) { again = true; return; }
    busy = true;
    again = false;
    last = Date.now();
    try {
      const d = await api(`/api/evals/runs/${enc(id)}`);
      if (gone) return;
      data = d;
      render();
    } catch (e) {
      if (gone) return;
      if (!data) {
        noticeEl.replaceChildren(h('div', { class: 'empty' },
          h('p', { class: 'empty-title', text: 'Run not found' }),
          h('p', { class: 't-sec', text: `${errText(e)}. Evals and their runs are in Usage.` }),
          h('a', { class: 'btn', href: '#/usage' }, 'Back to Usage')));
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
    titleEl.textContent = run.eval_name || run.eval;
    subEl.replaceChildren(pill(run.state, runTone(run.state)),
      h('span', { class: 'fr-sub-text', text: [
        `started ${relTime(isZero(run.started) ? run.created : run.started)}`,
        run.eval_version ? `version ${run.eval_version}` : '',
        run.actor ? `by ${run.actor}` : '',
        run.setup_filter ? `setup ${run.setup_filter} only` : '',
      ].filter(Boolean).join(' · ') }));
    renderActions(run);
    noticeEl.replaceChildren(run.reason ? h('div', { class: `alert ${run.state === 'canceled' ? 'warn' : run.state === 'failed' ? 'err' : 'info'}`, text: run.reason }) : '');
    renderFacts(run);
    renderRank();
    renderMatrix(run);
  }

  function renderActions(run) {
    const key = `${run.state}|${canRejudge(run)}`;
    if (key === actionsKey) return;
    actionsKey = key;
    clear(actionsEl);
    if (canCancel(run.state)) actionsEl.append(h('button', { class: 'btn btn-ghost', type: 'button', onclick: cancel }, 'Cancel run'));
    if (canRejudge(run)) {
      actionsEl.append(h('button', { class: 'btn', type: 'button', title: 'Judge the same sessions again with the judge model of now', onclick: rejudge }, '↻ Rejudge'));
    }
  }

  function renderFacts(run) {
    const t = runTotals(data);
    const judged = (run.sessions || []).filter((s) => s.state === 'judged').length;
    const facts = [
      ['Judge', `${run.judge_model || '–'}${run.judge_prompt ? ` · prompt ${run.judge_prompt}` : ''}`],
      ['Sessions', `${judged}/${(run.sessions || []).length} judged · ${run.max_parallel || 1} at once`],
      ['Longest session', fmtWall(t.wallMs)],
      ['Sessions used', usageText(t.session) || '–'],
      ['Judge used', usageText(t.judge) || '–'],
    ];
    if (run.base) facts.push(['Base commit', run.base.slice(0, 12)]);
    factsEl.replaceChildren(...facts.map(([k, v]) => h('div', { class: 'fr-fact' },
      h('span', { class: 'fr-fact-k', text: k }), h('span', { class: 'fr-fact-v', text: v }))));
  }

  function renderRank() {
    const ranks = data.summary || [];
    clear(rankEl);
    if (!ranks.length) return;
    rankEl.append(h('table', { class: 'data usage-table ev-rank' },
      h('thead', {}, h('tr', {}, ['#', 'Setup', 'Gates', 'Criteria', 'Iterations', 'Time', 'Session use', 'Setup hash'].map((x) => h('th', { text: x })))),
      h('tbody', {}, ranks.map((k) => h('tr', {},
        h('td', { 'data-label': '#', text: k.reports ? String(k.rank) : '–' }),
        h('td', { 'data-label': 'Setup', class: 'mono', text: k.setup }),
        h('td', { 'data-label': 'Gates', class: 'num', text: k.reports ? `${k.gates_passed}/${k.gates_total}` : '–' }),
        h('td', { 'data-label': 'Criteria', class: 'num', text: k.reports ? `${k.criteria_passed}/${k.criteria_total}` : '–' }),
        h('td', { 'data-label': 'Iterations', class: 'num', title: 'Iterations the passed gates took (fewer ranks higher)', text: k.reports ? String(k.iterations || 0) : '–' }),
        h('td', { 'data-label': 'Time', class: 'num', text: fmtWall(k.wall_ms) }),
        h('td', { 'data-label': 'Session use', text: usageText(k.session_usage) || '–' }),
        h('td', { 'data-label': 'Setup hash', class: 'mono', text: (k.setup_hashes || []).join(' ') || '–' }))))));
  }

  function renderMatrix(run) {
    const ev = run.snapshot || {};
    const cols = gateColumns(ev);
    const rows = matrixRows(data);
    const prompts = new Map((ev.criteria || []).map((c) => [c.id, c]));
    // Keep the scroll position across live refreshes.
    const sx = matrixEl.scrollLeft;
    const sy = matrixEl.scrollTop;
    clear(matrixEl);
    const table = h('table', { class: 'ev-matrix' },
      h('thead', {},
        h('tr', {}, h('th', { class: 'ev-corner', rowspan: '2', scope: 'col', text: 'Setup' }),
          cols.map((g) => h('th', { class: 'ev-gate', colspan: String(g.criteria.length), scope: 'colgroup', text: g.name })),
          h('th', { class: 'ev-gates-col', rowspan: '2', scope: 'col', text: 'Gates' })),
        h('tr', {}, cols.flatMap((g) => g.criteria.map((c) => h('th', { class: 'ev-crit', scope: 'col', title: (prompts.get(c) || {}).prompt || c },
          h('span', { text: c })))))),
      h('tbody', {}, rows.map((row) => h('tr', {},
        h('th', { class: 'ev-setup', scope: 'row' },
          h('span', { class: 'ev-setup-name', text: row.label }),
          h('span', { class: 'ev-setup-state' }, pill(row.state, slotTone(row.state))),
          row.session ? h('a', { class: 'fe-link ev-sess', href: `#/s/${enc(row.session)}`, title: row.live ? `session ${row.live}` : '' }, `${row.session} →`) : null,
          row.session ? trajButton(row, 'ev-sess ev-traj') : null),
        cols.flatMap((g) => g.criteria.map((c) => {
          const cell = row.cells[c];
          const st = cell ? cell.state : '';
          return h('td', { class: `ev-cell tone-${cellTone(st)}` },
            h('button', {
              class: 'ev-mark', type: 'button', disabled: !cell,
              'aria-label': `${row.label}, ${c}: ${cellText(st)}`, title: cellText(st),
              onclick: () => showCell(row, c, cell, prompts.get(c)),
            }, cellMark(st)));
        })),
        h('td', { class: 'ev-gates-col t-sm' }, gatesCell(row))))));
    matrixEl.append(table);
    matrixEl.scrollLeft = sx;
    matrixEl.scrollTop = sy;
  }

  // trajButton downloads the row's session trajectory (E4, ATIF JSON). A
  // 409 (records off or swept, none for the session) is a toast, not a file.
  function trajButton(row, cls) {
    return h('button', {
      class: `fe-link ${cls}`, type: 'button',
      title: 'Download the session\'s model calls as an ATIF trajectory (from the request records)',
      onclick: () => downloadTrajectory(row),
    }, 'Trajectory ↓');
  }

  async function downloadTrajectory(row) {
    const path = (row.report && row.report.trajectory) || trajectoryPath(row.session);
    try {
      const text = await api(path, { text: true });
      const url = URL.createObjectURL(new Blob([text], { type: 'application/json' }));
      const a = h('a', { href: url, download: trajectoryFile(row.session), hidden: true });
      document.body.append(a);
      a.click();
      a.remove();
      setTimeout(() => URL.revokeObjectURL(url), 1000);
    } catch (e) {
      toast(errText(e), 'err');
    }
  }

  // gatesCell is a row's Gates column: gates passed, the gates that took
  // more than one iteration, and a button for the feedback sent (E2).
  function gatesCell(row) {
    const rep = row.report;
    if (!rep) return [h('span', { text: row.error || (row.state === 'judging' ? 'judging…' : row.state) })];
    const out = [h('span', { text: gatesText(rep) })];
    const its = iterationsText(rep);
    if (its) out.push(h('span', { class: 'ev-iters', text: its }));
    const fbs = rep.feedback || [];
    if (fbs.length) {
      out.push(h('button', { class: 'btn btn-sm ev-fb-btn', type: 'button', onclick: () => showFeedback(row, fbs) },
        fbs.length === 1 ? 'Feedback' : `Feedback (${fbs.length})`));
    }
    return out;
  }

  function showFeedback(row, fbs) {
    openDialog((close) => ({
      title: `Feedback · ${row.label}`,
      sub: 'What the judge sent the session between iterations (also in its feed, "Eval feedback:")',
      body: h('div', { class: 'ev-cell-detail' }, fbs.map((fb) => h('div', { class: 'ev-fb' },
        h('span', { class: 'fr-fact-k', text: feedbackLabel(fb) }),
        (fb.criteria || []).length ? h('p', { class: 't-sm mono', text: `Failed: ${fb.criteria.join(', ')}` }) : null,
        h('p', { class: 'ev-fb-text', text: fb.text || '–' }),
        fb.error ? h('p', { class: 't-sm ev-fb-err', text: fb.error }) : null)),
      row.session ? h('a', { class: 'fe-link', href: `#/s/${enc(row.session)}`, onclick: () => close() }, `Open session ${row.session} →`) : null,
      row.session ? trajButton(row, 'ev-traj') : null),
      foot: [h('button', { class: 'btn', type: 'button', onclick: () => close() }, 'Close')],
    }));
  }

  function showCell(row, critId, cell, crit) {
    const x = cell.result || {};
    const earlier = cellHistory(row.report, critId);
    openDialog((close) => ({
      title: `${critId} · ${cellText(x.state)}`,
      sub: `${row.label} · gate ${x.gate || '–'}${x.iteration > 1 ? ` · iteration ${x.iteration}` : ''}`,
      body: h('div', { class: 'ev-cell-detail' },
        crit && crit.prompt ? h('p', { class: 'ev-prompt', text: crit.prompt }) : null,
        h('span', { class: 'fr-fact-k', text: 'Rationale' }),
        h('p', { class: 'ev-rationale', text: x.rationale || '–' }),
        (x.evidence || []).length ? h('span', { class: 'fr-fact-k', text: 'Evidence' }) : null,
        (x.evidence || []).length ? h('ul', { class: 'ev-evidence' }, x.evidence.map((e) => h('li', { class: 'mono', text: e }))) : null,
        x.model ? h('p', { class: 't-sm', text: `Judged by ${x.model}${x.tokens ? ` · ${x.tokens} tokens` : ''}` }) : null,
        earlier.length ? h('span', { class: 'fr-fact-k', text: 'Earlier iterations' }) : null,
        earlier.length ? h('ul', { class: 'ev-evidence' }, earlier.map((e) => h('li', {},
          `${e.iteration}: ${cellText(e.state)}${e.rationale ? ` · ${e.rationale}` : ''}`))) : null,
        row.session ? h('a', { class: 'fe-link', href: `#/s/${enc(row.session)}`, onclick: () => close() }, `Open session ${row.session} →`) : null,
        row.session ? trajButton(row, 'ev-traj') : null),
      foot: [h('button', { class: 'btn', type: 'button', onclick: () => close() }, 'Close')],
    }));
  }

  async function cancel() {
    if (!(await confirmDialog({ title: 'Cancel this run?', message: 'Sessions not started yet will not start; running ones are stopped. Reports already written stay.', ok: 'Cancel run', danger: true }))) return;
    try { await post(`/api/evals/runs/${enc(id)}/cancel`, {}); load(); } catch (e) { toast(errText(e), 'err'); load(); }
  }

  async function rejudge() {
    if (!(await confirmDialog({ title: 'Judge this run again?', message: 'The same sessions are judged again with the judge model configured now; their reports are replaced. Nothing runs again.', ok: 'Rejudge' }))) return;
    try { await post(`/api/evals/runs/${enc(id)}/rejudge`, {}); toast('Judging again', 'ok'); load(); } catch (e) { toast(errText(e), 'err'); load(); }
  }

  const unsub = store.subscribe((kind, payload) => {
    if (!evalRunEventFor(kind, payload, id)) return;
    schedule();
  });
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
