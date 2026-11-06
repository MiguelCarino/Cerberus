// Evals section in Usage (E1), below Bench: the evals this person may run
// (all of them for admins), each with its last run, and a JSON editor in a
// dialog to make or change one. Runs open their own page
// (#/evals/runs/<run>). No timers: it reloads with the rest of Usage
// (Refresh), after a save and on evalrun events.

import { api, post } from '../api.js';
import { h, clear, toast, errText, openDialog, confirmDialog, pill } from '../dom.js';
import { relTime } from '../util.js';
import { store } from '../store.js';
import { lastRunText, runTone, parseEvalJSON, editableJSON, TEMPLATE, problemsText, evalRunEventFor } from '../evals.js';

const enc = encodeURIComponent;

// evalsSection returns {el, load(), destroy()}.
export function evalsSection() {
  let gone = false;
  let seq = 0;
  const admin = store.can('models.manage');
  const canMake = admin || store.can('sessions.write');
  const body = h('div', { class: 'ev-body' }, h('p', { class: 't-sec', text: 'Loading…' }));
  const head = h('div', { class: 'ev-head' },
    h('h2', { class: 'group-title', text: 'Evals' }),
    canMake ? h('button', { class: 'btn btn-sm', type: 'button', onclick: () => edit(null) }, '+ New eval') : null);
  const el = h('div', { class: 'ev' }, head,
    h('p', { class: 't-sec ev-about', text: 'A task, criteria in gates and the agent setups to try. Each setup gets a session; when it ends a judge model reads its workspace and decides every criterion. The judge never runs anything.' }),
    body);

  async function load() {
    const mine = ++seq;
    let list;
    try {
      list = await api(admin ? '/api/evals?all=1' : '/api/evals');
    } catch (e) {
      if (!gone && mine === seq) clear(body).append(h('p', { class: 't-sec', text: errText(e) }));
      return;
    }
    if (gone || mine !== seq) return;
    render(list || []);
  }

  function render(list) {
    clear(body);
    if (!list.length) {
      body.append(h('p', { class: 't-sec', text: canMake ? 'No evals yet. Make one with + New eval, or: cerberus eval import FILE' : 'No evals yet.' }));
      return;
    }
    body.append(h('ul', { class: 'ev-list' }, list.map((ev) => {
      const lr = ev.last_run;
      return h('li', { class: 'ev-row' },
        h('div', { class: 'ev-row-main' },
          h('span', { class: 'ev-name', text: ev.name || ev.id }),
          h('span', { class: 't-sm ev-meta', text: [
            ev.id, `${(ev.criteria || []).length} criteria`, `${(ev.setups || []).length} setups`,
            ev.repeat > 1 ? `× ${ev.repeat}` : '', ev.owner_name ? `by ${ev.owner_name}` : '',
          ].filter(Boolean).join(' · ') })),
        lr ? h('a', { class: 'ev-last', href: `#/evals/runs/${enc(lr.id)}`, title: lr.id },
          pill(lr.state, runTone(lr.state)), h('span', { class: 't-sm', text: `${lastRunText(lr)} · ${relTime(lr.created)}` }))
          : h('span', { class: 't-sm ev-last', text: 'never run' }),
        h('div', { class: 'ev-actions' },
          h('button', { class: 'btn btn-sm btn-primary', type: 'button', onclick: (e) => start(ev, e.currentTarget) }, '▶ Run'),
          h('button', { class: 'btn btn-sm btn-ghost', type: 'button', onclick: () => edit(ev.id) }, 'Edit')));
    })));
  }

  async function start(ev, btn) {
    const n = (ev.setups || []).length * Math.max(1, ev.repeat || 1);
    if (!(await confirmDialog({ title: `Run ${ev.name || ev.id}?`, message: `This starts ${n} agent session${n === 1 ? '' : 's'} (at most evals.max_parallel at once), then the judge model reads each one's work.`, ok: 'Run' }))) return;
    btn.disabled = true;
    try {
      const run = await post(`/api/evals/${enc(ev.id)}/run`, {});
      location.hash = `#/evals/runs/${enc(run.id)}`;
    } catch (e) {
      toast(errText(e), 'err');
    } finally {
      btn.disabled = false;
    }
  }

  async function edit(id) {
    let view = null;
    if (id) {
      try { view = await api(`/api/evals/${enc(id)}`); } catch (e) { toast(errText(e), 'err'); return; }
    }
    const saved = await openDialog((close) => {
      const area = h('textarea', { class: 'input ev-json', spellcheck: 'false', 'aria-label': 'Eval definition (JSON)', rows: '18' });
      area.value = view ? editableJSON(view.eval) : JSON.stringify(TEMPLATE, null, 2);
      const msg = h('pre', { class: 'ev-problems', 'aria-live': 'polite' });
      if (view && (view.problems || []).length) msg.textContent = problemsText(view.problems);
      const read = () => {
        const p = parseEvalJSON(area.value);
        if (!p.ok) { msg.textContent = p.error; return null; }
        return p.eval;
      };
      const check = async () => {
        const ev = read();
        if (!ev) return;
        try {
          const r = await post('/api/evals/validate', ev);
          msg.textContent = (r.problems || []).length ? problemsText(r.problems) : `No problems. Judging order: ${(r.order || []).map((g) => g.join(' → ')).join(' | ')}`;
        } catch (e) { msg.textContent = errText(e); }
      };
      const save = async () => {
        const ev = read();
        if (!ev) return;
        try {
          const r = id
            ? await api(`/api/evals/${enc(id)}`, { method: 'PUT', body: { ...ev, id } })
            : await post('/api/evals', ev);
          toast(`Saved ${r.eval.id} (version ${r.eval.version})`, 'ok');
          close(true);
        } catch (e) {
          const ps = e && e.body && e.body.problems;
          msg.textContent = ps && ps.length ? problemsText(ps) : errText(e);
          if (e && e.status === 409) msg.textContent += '\nIt changed elsewhere: close and open it again to see the new version.';
        }
      };
      const del = async () => {
        if (!(await confirmDialog({ title: `Delete ${id}?`, message: 'The definition goes; its runs and reports stay.', ok: 'Delete', danger: true }))) return;
        try { await api(`/api/evals/${enc(id)}`, { method: 'DELETE' }); toast(`Deleted ${id}`, 'ok'); close(true); } catch (e) { msg.textContent = errText(e); }
      };
      return {
        title: id ? `Edit ${id}` : 'New eval',
        sub: view ? `version ${view.eval.version} · judge ${view.judge_model || 'not set (evals.judge_model)'}` : 'Gates run in order; a criterion may depend on others in its gate or an earlier one.',
        wide: true,
        body: h('div', { class: 'ev-edit' }, area, msg),
        foot: [
          id ? h('button', { class: 'btn btn-danger btn-ghost', type: 'button', onclick: del }, 'Delete') : null,
          h('button', { class: 'btn btn-ghost', type: 'button', onclick: check }, 'Check'),
          h('button', { class: 'btn btn-primary', type: 'button', onclick: save }, 'Save'),
        ],
      };
    });
    if (saved && !gone) load();
  }

  const unsub = store.subscribe((kind, payload) => {
    if (!evalRunEventFor(kind, payload, '')) return;
    if (kind === 'resync' || payload.state !== 'running' || !payload.session) load();
  });

  return { el, load, destroy: () => { gone = true; unsub(); } };
}
