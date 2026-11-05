// Leaderboard section in Usage (day phase 6): ranks sessions, agents,
// models or repos by cost, tokens, energy or wall time over a period, from
// GET /api/leaderboard. Bars for the eye (hidden from screen readers) and
// the same numbers as a table under them. The choice is remembered per
// browser. No timers: it reloads with the rest of Usage.

import { apiResult } from '../api.js';
import { h, clear } from '../dom.js';
import { PERIODS, BYS, METRICS, normChoice, leaderboardQuery, barRows, totalText, emptyText } from '../leaderboard.js';

const KEY = 'cerberus.leaderboard';

function loadChoice() {
  try { return normChoice(JSON.parse(localStorage.getItem(KEY)) || {}); } catch { return normChoice({}); }
}
function saveChoice(c) {
  try { localStorage.setItem(KEY, JSON.stringify(c)); } catch { /* private mode */ }
}

// leaderboardSection returns {el, load(), destroy()}.
export function leaderboardSection() {
  let choice = loadChoice();
  let seq = 0;
  let gone = false;
  const groups = {};

  const chips = (name, label, list) => {
    const g = h('div', { class: 'fchips lb-chips', role: 'group', 'aria-label': label },
      h('span', { class: 'fchip-lbl', text: label }),
      list.map(([k, text]) => h('button', {
        class: 'fchip', type: 'button', 'data-k': k, 'aria-pressed': String(choice[name] === k),
        onclick: () => { choice = { ...choice, [name]: k }; saveChoice(choice); syncChips(); load(); },
      }, text)));
    groups[name] = g;
    return g;
  };
  const syncChips = () => {
    for (const [name, g] of Object.entries(groups)) {
      for (const b of g.querySelectorAll('button')) b.setAttribute('aria-pressed', String(b.dataset.k === choice[name]));
    }
  };

  const totalEl = h('span', { class: 'lb-total' });
  const bars = h('ol', { class: 'lb-bars', 'aria-hidden': 'true' });
  const table = h('div', { class: 'lb-table' });
  const el = h('section', { class: 'lb', 'aria-label': 'Leaderboard' },
    h('div', { class: 'lb-head' }, h('h2', { class: 'group-title', text: 'Leaderboard' }), totalEl),
    h('div', { class: 'lb-controls' },
      chips('period', 'Period', PERIODS), chips('by', 'Group', BYS), chips('metric', 'Metric', METRICS)),
    bars, table);

  async function load() {
    const my = ++seq;
    const res = await apiResult(leaderboardQuery(choice)).catch((e) => ({ ok: false, status: 0, message: String(e.message || e) }));
    if (gone || my !== seq) return;
    if (res.status === 404 || res.status === 503) { el.hidden = true; return; } // a daemon without the leaderboard
    el.hidden = false;
    if (!res.ok) {
      clear(bars);
      clear(table).append(h('div', { class: 'alert err', text: res.message }));
      return;
    }
    render(res.body || {});
  }

  function render(board) {
    const rows = barRows(board);
    const metricLabel = (METRICS.find(([k]) => k === board.metric) || [, ''])[1];
    const byLabel = (BYS.find(([k]) => k === board.by) || [, ''])[1];
    totalEl.textContent = rows.length ? `Total ${totalText(board)}` : '';
    clear(bars);
    clear(table);
    if (!rows.length) {
      table.append(h('p', { class: 't-sec', text: emptyText(board) }));
      return;
    }
    bars.append(...rows.map((r) => h('li', { class: 'lb-row' },
      h('span', { class: 'lb-rank', text: String(r.rank) }),
      h('div', { class: 'lb-main' },
        h('div', { class: 'lb-line' },
          r.href ? h('a', { class: 'lb-label', href: r.href, tabindex: '-1', text: r.label }) : h('span', { class: 'lb-label', text: r.label }),
          h('span', { class: 'lb-val', text: r.main })),
        h('div', { class: 'lb-bar' }, h('span', { style: `width:${Math.max(r.pct, r.value > 0 ? 1 : 0).toFixed(1)}%` })),
        r.sub ? h('div', { class: 'lb-sub', text: r.sub }) : null))));
    table.append(h('details', {},
      h('summary', { text: 'Show as a table' }),
      h('table', { class: 'data lb-data' },
        h('caption', { class: 'sr-only', text: `${byLabel} ranked by ${metricLabel.toLowerCase()}` }),
        h('thead', {}, h('tr', {}, h('th', { scope: 'col', text: '#' }), h('th', { scope: 'col', text: byLabel }),
          h('th', { scope: 'col', text: metricLabel }), h('th', { scope: 'col', text: 'Details' }))),
        h('tbody', {}, rows.map((r) => h('tr', {},
          h('td', { class: 'num', text: String(r.rank) }),
          h('th', { scope: 'row' }, r.href ? h('a', { href: r.href, text: r.label }) : r.label),
          h('td', { class: 'num', text: r.main }),
          h('td', { text: r.sub })))))));
  }

  return { el, load, destroy() { gone = true; } };
}
