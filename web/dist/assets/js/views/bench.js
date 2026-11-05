// Bench section in Usage (gateway phase G6): saved bench runs, one row each
// (decode, time to first token, prefill and tokens per joule), the run going
// now, and for admins a form to start one. No timers: it reloads with the
// rest of Usage (Refresh), and after a start.

import { api, post } from '../api.js';
import { h, clear, toast, errText } from '../dom.js';
import { fmtMs, relTime } from '../util.js';
import { runSummary, fmtRate, benchBody } from '../bench.js';
import { store } from '../store.js';

// benchSection returns {el, load(), destroy()}.
export function benchSection() {
  let gone = false;
  let seq = 0;
  const body = h('div', { class: 'bench-body' }, h('p', { class: 't-sec', text: 'Loading…' }));
  const form = store.can('models.manage') ? startForm() : null;
  const el = h('div', { class: 'bench' },
    h('h2', { class: 'group-title', text: 'Bench' }),
    h('p', { class: 't-sec bench-about', text: 'The same prompts every time, at temperature 0 with thinking off, so runs compare like for like. Running one loads the model on the GPU.' }),
    form ? form.el : null, body);

  async function load() {
    const mine = ++seq;
    let r;
    try {
      r = await api('/api/bench');
    } catch (e) {
      if (!gone && mine === seq) clear(body).append(h('p', { class: 't-sec', text: errText(e) }));
      return;
    }
    if (gone || mine !== seq) return;
    render(r || {});
  }

  function render(r) {
    clear(body);
    if (r.running) {
      body.append(h('p', { class: 'alert info', text: `Running ${r.running.options.model}: ${r.running.progress || 'starting'}. Refresh to follow it.` }));
    }
    const runs = (r.runs || []).map(runSummary);
    if (!runs.length) {
      body.append(h('p', { class: 't-sec', text: 'No runs yet. Start one here or with: cerberus bench --model ID' }));
      return;
    }
    body.append(h('table', { class: 'data usage-table bench-table' },
      h('thead', {}, h('tr', {}, ['When', 'Model', 'Label', 'Decode', 'First token', 'Prefill', 'tok/J', 'State'].map((t) => h('th', { text: t })))),
      h('tbody', {}, runs.map((s) => h('tr', { class: s.state === 'failed' || s.errors ? 'row-err' : '', title: s.id },
        h('td', { 'data-label': 'When', text: relTime(s.time) }),
        h('td', { 'data-label': 'Model', class: 'mono', text: s.model + (s.draft ? ' + draft' : '') }),
        h('td', { 'data-label': 'Label', text: s.label || '–' }),
        h('td', { 'data-label': 'Decode', class: 'num', text: fmtRate(s.decode) }),
        h('td', { 'data-label': 'First token', class: 'num', text: s.ttft > 0 ? fmtMs(s.ttft) : '–' }),
        h('td', { 'data-label': 'Prefill', class: 'num', text: fmtRate(s.prefill) }),
        h('td', { 'data-label': 'tok/J', class: 'num', text: s.tokJ > 0 ? s.tokJ.toFixed(2) : '–' }),
        h('td', { 'data-label': 'State', text: s.errors ? `${s.state} · ${s.errors} errors` : s.state }))))),
      h('p', { class: 't-sec', text: 'Compare two runs kind by kind: cerberus bench compare A B (the ids are in each row\'s tooltip).' }));
  }

  function startForm() {
    const st = store.status();
    const models = ((st && st.models) || []).filter((m) => m.Local).map((m) => m.ID);
    const model = h('select', { class: 'input', 'aria-label': 'Model' }, models.map((id) => h('option', { value: id, text: id })));
    const label = h('input', { class: 'input', type: 'text', placeholder: 'Label (e.g. power cap 300 W)', 'aria-label': 'Label', maxlength: '80' });
    const prefill = h('input', { type: 'checkbox' });
    const btn = h('button', { class: 'btn', type: 'button', disabled: !models.length, onclick: async () => {
      btn.disabled = true;
      try {
        const r = await post('/api/bench', benchBody({ model: model.value, label: label.value, prefill: prefill.checked }));
        toast(`Bench started: ${r && r.id}`);
        load();
      } catch (e) {
        toast(errText(e), 'err');
      } finally {
        btn.disabled = false;
      }
    } }, 'Run bench');
    return { el: h('div', { class: 'bench-form' }, model, label,
      h('label', { class: 'bench-check' }, prefill, h('span', { text: 'Long prompts too' })), btn) };
  }

  return { el, load, destroy: () => { gone = true; } };
}
