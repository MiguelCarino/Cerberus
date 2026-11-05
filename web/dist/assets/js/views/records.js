// Records section in Usage (gateway phase G3): the gateway's request
// records for a day, filtered by client, with a summary per client, the
// tools called, and each line opening in a dialog (its text, when the
// server keeps text, is read through an audited call). Admins only. No
// timers: it reloads with the rest of Usage.

import { api } from '../api.js';
import { h, clear, openDialog, toast, errText } from '../dom.js';
import { fmtInt, fmtMs, relTime } from '../util.js';
import { recordsQuery, recordURL, summarize, toolCounts, switches, cacheShare, guardText, FINISH_LABEL, OFF_TEXT } from '../records.js';
import { store } from '../store.js';

// recordsSection returns {el, load(), destroy()}.
export function recordsSection() {
  let seq = 0;
  let gone = false;
  let filter = { day: '', client: '' };
  const head = h('div', { class: 'rec-head' });
  const body = h('div', { class: 'rec-body' }, h('p', { class: 't-sec', text: 'Loading…' }));
  const el = h('div', { class: 'rec' }, h('h2', { class: 'group-title', text: 'Request records' }), head, body);

  async function load() {
    const mine = ++seq;
    let r;
    try {
      r = await api(recordsQuery(filter));
    } catch (e) {
      if (!gone && mine === seq) clear(body).append(h('p', { class: 't-sec', text: errText(e) }));
      return;
    }
    if (gone || mine !== seq) return;
    render(r || {});
  }

  function render(r) {
    clear(head);
    clear(body);
    const stats = r.stats || {};
    const st = store.status();
    const guard = guardText(st && st.agent_guard);
    if (guard) head.append(h('p', { class: 't-sec rec-note', text: guard }));
    if (!stats.on) { body.append(h('p', { class: 't-sec', text: OFF_TEXT })); return; }
    const days = r.days || [];
    const lines = r.lines || [];
    const sum = summarize(lines);
    head.append(
      h('p', { class: 't-sec rec-note', text: `${fmtInt(stats.written)} written since start${stats.dropped ? ` · ${fmtInt(stats.dropped)} dropped` : ''} · text: ${stats.text || 'none'}` }),
      days.length ? h('label', { class: 'rec-day' }, h('span', { class: 'fchip-lbl', text: 'Day' }),
        h('select', { class: 'input', onchange: (e) => { filter = { ...filter, day: e.target.value }; load(); } },
          days.map((d) => h('option', { value: d, text: d, selected: d === (filter.day || days[0]) })))) : null,
      sum.clients.length > 1 || filter.client ? h('div', { class: 'fchips', role: 'group', 'aria-label': 'Client' },
        h('span', { class: 'fchip-lbl', text: 'Client' }),
        [['', 'All'], ...sum.clients.map((c) => [c.client, c.client])].map(([k, t]) => h('button', {
          class: 'fchip', type: 'button', 'aria-pressed': String(filter.client === k),
          onclick: () => { filter = { ...filter, client: k }; load(); },
        }, t))) : null);
    if (!lines.length) { body.append(h('p', { class: 't-sec', text: 'No requests recorded on this day.' })); return; }

    body.append(h('table', { class: 'data rec-sum' },
      h('thead', {}, h('tr', {}, ['Client', 'Requests', 'Avg prompt', 'From cache', 'Tool calls', 'Tool errors'].map((t) => h('th', { text: t })))),
      h('tbody', {}, sum.clients.map((c) => h('tr', {},
        h('td', { 'data-label': 'Client', class: 'mono', text: c.client }),
        h('td', { 'data-label': 'Requests', class: 'num', text: fmtInt(c.requests) }),
        h('td', { 'data-label': 'Avg prompt', class: 'num', text: `${fmtInt(c.avgPrompt)} tok` }),
        h('td', { 'data-label': 'From cache', class: 'num', text: c.cachePct == null ? '–' : `${c.cachePct}%` }),
        h('td', { 'data-label': 'Tool calls', class: 'num', text: fmtInt(c.calls) }),
        h('td', { 'data-label': 'Tool errors', class: 'num', text: c.errorPct == null ? '–' : `${c.resultErrors} of ${c.results} (${c.errorPct}%)` }))))));

    const tools = toolCounts(lines).slice(0, 12);
    if (tools.length) {
      body.append(h('p', { class: 'rec-tools' }, h('span', { class: 'fchip-lbl', text: 'Tools called' }),
        tools.map((t) => h('span', { class: 'chip-tag', text: `${t.name} ${t.n}` }))));
    }
    const ends = Object.entries(sum.finishes).sort((a, b) => b[1] - a[1]);
    body.append(h('p', { class: 't-sec rec-ends', text: 'Ended: ' + ends.map(([f, n]) => `${FINISH_LABEL[f] || f} ${n}`).join(' · ') }));

    body.append(h('table', { class: 'data usage-table rec-lines' },
      h('thead', {}, h('tr', {}, ['When', 'Client', 'API', 'Model', 'Prompt → out', 'Cache', 'Tools', 'Ended', 'Time'].map((t) => h('th', { text: t })))),
      h('tbody', {}, lines.map((l) => {
        const share = cacheShare(l);
        const bad = l.status >= 400 || l.finish === 'error';
        return h('tr', { class: `rec-row${bad ? ' row-err' : ''}`, tabindex: '0', role: 'button', 'aria-label': `Open the request at ${l.time}`,
          onclick: () => openLine(l), onkeydown: (e) => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); openLine(l); } } },
        h('td', { 'data-label': 'When', text: relTime(l.time) }),
        h('td', { 'data-label': 'Client', class: 'mono', text: l.client || '–' }),
        h('td', { 'data-label': 'API', text: l.api || '–' }),
        h('td', { 'data-label': 'Model', class: 'mono', text: l.model || '–' }),
        h('td', { 'data-label': 'Prompt → out', class: 'num', text: `${fmtInt(l.prompt_tokens)} → ${fmtInt(l.completion_tokens)}` }),
        h('td', { 'data-label': 'Cache', class: 'num', text: share == null ? '–' : `${share}%` }),
        h('td', { 'data-label': 'Tools', class: 'num', text: `${l.tools || 0} offered · ${(l.calls || []).length} called` }),
        h('td', { 'data-label': 'Ended', text: FINISH_LABEL[l.finish] || l.finish || '–' }),
        h('td', { 'data-label': 'Time', class: 'num', text: fmtMs(l.duration_ms) }));
      }))));
  }

  async function openLine(l) {
    const url = recordURL(l.ref);
    if (!url) return;
    let full = l;
    try {
      full = await api(url);
    } catch (e) {
      toast(errText(e), 'err');
      return;
    }
    const facts = [
      ['Client', full.client], ['API', `${full.api}${full.stream ? ' (streamed)' : ''}`], ['Model', full.model],
      ['Session', full.session || '–'], ['Asked', switches(full)],
      ['Messages', `${full.messages} · ${full.tools} tools offered`],
      ['Tokens', `${fmtInt(full.prompt_tokens)} prompt (${fmtInt(full.cached_tokens)} cached) → ${fmtInt(full.completion_tokens)}${full.reasoning_tokens ? ` (${fmtInt(full.reasoning_tokens)} thinking)` : ''}`],
      ['Tool results', full.results ? `${full.results} sent back · ${full.matched || 0} answer this gateway's calls · ${full.result_errors || 0} marked errors` : '–'],
      ['Calls in reply', (full.calls || []).map((c) => c.name).join(', ') || '–'],
      ['Ended', `${FINISH_LABEL[full.finish] || full.finish} · HTTP ${full.status} · first token ${fmtMs(full.ttft_ms)} · ${fmtMs(full.duration_ms)}`],
      full.error ? ['Error', full.error] : null,
      full.guard ? ['Guard', `${full.guard.mode}: ${full.guard.flagged} flagged${(full.guard.held || []).length ? ` · held ${full.guard.held.join(', ')}` : ''}`] : null,
    ].filter(Boolean);
    const t = full.text;
    openDialog(() => ({
      title: 'Request record', sub: new Date(full.time).toLocaleString(), wide: true,
      body: h('div', { class: 'rec-detail' },
        h('dl', { class: 'rec-facts' }, facts.map(([k, v]) => [h('dt', { text: k }), h('dd', { text: v })])),
        t ? [
          t.system ? [h('h3', { class: 'rec-h', text: 'System prompt' }), h('pre', { class: 'rec-pre', text: t.system })] : null,
          t.messages ? [h('h3', { class: 'rec-h', text: 'Conversation' }), h('pre', { class: 'rec-pre', text: t.messages })] : null,
          t.reasoning ? [h('h3', { class: 'rec-h', text: 'Thinking' }), h('pre', { class: 'rec-pre', text: t.reasoning })] : null,
          t.reply ? [h('h3', { class: 'rec-h', text: 'Reply' }), h('pre', { class: 'rec-pre', text: t.reply })] : null,
        ] : h('p', { class: 't-sec', text: 'No text kept (records keep counts only).' })),
    }));
  }

  return { el, load, destroy: () => { gone = true; } };
}
