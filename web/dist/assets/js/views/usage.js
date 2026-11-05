// Usage view: cloud spend per provider (today and this month), budget bars,
// subscription plan gauges, and the recent gateway requests. A table on
// wide screens; each request row stacks into a card on phones (CSS, via
// data-label). Opening the view marks subscription tracking active
// (POST /api/usage/watch), renewed while the view stays open and visible.

import { api, post } from '../api.js';
import { h, clear, toast, errText } from '../dom.js';
import { fmtNum, fmtInt, fmtMs, fmtEnergy, relTime } from '../util.js';
import { fmtUSD, fmtUSDExact, fmtTokens, spendRows, budgetBar, planGauge, resetText, planErrTone } from '../spend.js';
import { leaderboardSection } from './leaderboard.js';
import { recordsSection } from './records.js';
import { benchSection } from './bench.js';
import { delegationText, finetuneText, fmtTokens as fmtTok, fmtWh, fmtSaved } from '../delegation.js';
import { store } from '../store.js';

// The server keeps subs active for 10 min per watch; renew a little sooner.
const WATCH_MS = 9 * 60 * 1000;

const COLS = [
  ['When', (r) => relTime(r.Time)],
  ['Model', (r) => r.Model],
  ['Client', (r) => r.Client || (r.SessionID ? `session ${r.SessionID}` : '–')],
  ['Tokens', (r) => `${fmtInt(r.PromptTokens)} → ${fmtInt(r.CompletionTokens)}`],
  ['TTFT', (r) => fmtMs(r.TTFTMs)],
  ['Time', (r) => fmtMs(r.DurationMs)],
  ['tok/s', (r) => fmtNum(r.TokPerSec)],
  ['Energy', (r) => (r.Local ? fmtEnergy(r.EnergyJ) : 'cloud')],
  ['Cost', (r) => (r.Local ? '–' : r.CostUSD ? fmtUSD(r.CostUSD) : '–')],
  ['tok/J', (r) => fmtNum(r.TokPerJ, 2)],
  ['Status', (r) => String(r.Status || '–')],
];

export function mount(root) {
  const spendEl = h('div', { class: 'spend-grid' });
  const budgetsEl = h('div', { class: 'budget-list' });
  const plansEl = h('div', { class: 'plan-grid' });
  const wrap = h('div', { class: 'usage-wrap' }, h('p', { class: 't-sec', text: 'Loading…' }));
  const board = leaderboardSection();
  const recs = store.can('audit') ? recordsSection() : null; // admins: what each client sends (G3)
  const benches = benchSection(); // G6
  const delegEl = h('div', { class: 'deleg' });
  root.append(h('section', { class: 'view view-pad' },
    h('div', { class: 'view-head' },
      h('div', {}, h('h1', { class: 'view-title', text: 'Usage' }), h('p', { class: 'view-sub', text: 'Cloud spend, subscription use, the leaderboard and the last 100 gateway requests.' })),
      h('button', { class: 'btn', type: 'button', onclick: () => load() }, 'Refresh')),
    h('h2', { class: 'group-title', text: 'Spend' }), spendEl,
    budgetsEl,
    plansEl,
    board.el,
    benches.el,
    h('h2', { class: 'group-title', text: 'Delegated to local models' }), delegEl,
    h('h2', { class: 'group-title', text: 'Recent requests' }), wrap,
    recs ? recs.el : null));

  let gone = false;
  let timer = 0;
  let gen = 0; // the newest watch call

  async function load() {
    try {
      const u = await api('/api/usage?limit=100');
      if (gone) return;
      renderSpend(spendEl, u && u.spend);
      renderBudgets(budgetsEl, (u && u.budgets) || []);
      renderPlans(plansEl, (u && u.plans) || []);
      renderRecent(wrap, (u && u.recent) || []);
      board.load();
      if (recs) recs.load();
      benches.load();
      loadDelegation();
    } catch (e) {
      if (!gone) toast(errText(e), 'err');
    }
  }

  // loadDelegation: what agents handed to local models (30 days) and the
  // training examples collected so far.
  async function loadDelegation() {
    const [d, ft] = await Promise.all([
      api('/api/delegation?period=30d').catch(() => null),
      store.can('finetune') ? api('/api/finetune').catch(() => null) : null,
    ]);
    if (!gone) renderDelegation(delegEl, d, ft);
  }

  // watch renews subscription tracking, then reloads so the gauges show
  // what the tracker picked up. Paused while the tab is hidden.
  // Only the newest call schedules (gen), so a hide/show while the POST
  // is in flight never leaves a second loop running.
  async function watch() {
    clearTimeout(timer);
    timer = 0;
    if (gone || document.hidden) return;
    const mine = ++gen;
    await post('/api/usage/watch').catch(() => {});
    if (gone || mine !== gen) return;
    load();
    clearTimeout(timer);
    if (!document.hidden) timer = setTimeout(watch, WATCH_MS);
  }

  const onVisible = () => watch();
  const onHidden = () => { if (document.hidden) { clearTimeout(timer); timer = 0; } };
  window.addEventListener('cerberus:visible', onVisible);
  document.addEventListener('visibilitychange', onHidden);
  watch();
  return () => {
    gone = true;
    board.destroy();
    if (recs) recs.destroy();
    benches.destroy();
    clearTimeout(timer);
    window.removeEventListener('cerberus:visible', onVisible);
    document.removeEventListener('visibilitychange', onHidden);
  };
}

function renderDelegation(el, d, ft) {
  clear(el);
  const total = d && d.total;
  const line = delegationText(total, d && d.reference);
  el.append(h('p', { class: 'deleg-total', text: line || 'No jobs handed to local models in the last 30 days. Agents can use cerberus_models and cerberus_ask when delegation is on (Settings → Agents).' }));
  if (line && d.by_model && d.by_model.length) {
    const rows = d.by_model.slice(0, 6).map((r) => h('tr', {},
      h('td', { class: 'mono', text: r.key }),
      h('td', { class: 'num', text: String(r.requests) }),
      h('td', { class: 'num', text: fmtTok(r.prompt_tokens + r.completion_tokens) }),
      h('td', { class: 'num', text: r.energy_j > 0 ? fmtWh(r.energy_j) : '–' }),
      h('td', { class: 'num', text: d.reference ? (fmtSaved(r.saved_usd) || '–') : '–' })));
    el.append(h('div', { class: 'deleg-scroll' }, h('table', { class: 'data deleg-table' },
      h('thead', {}, h('tr', {}, ...['Local model', 'Jobs', 'Tokens', 'Energy', 'Saved'].map((t) => h('th', { text: t })))),
      h('tbody', {}, ...rows))));
  }
  if (ft) {
    const usable = (ft.stats && ft.stats.usable) || 0;
    el.append(h('div', { class: 'deleg-ft' },
      h('p', { class: 't-sec', text: finetuneText(ft) }),
      h('a', { class: 'btn btn-sm', href: '#/settings/finetune' }, 'Review in Settings → Fine-tuning'),
      usable ? h('a', { class: 'btn btn-sm', href: '/api/finetune/export', download: '' }, `Download ${usable} rows (JSONL)`) : null));
  }
}

function renderSpend(el, spend) {
  clear(el);
  const { rows, total } = spendRows(spend);
  el.append(spendCard('Today', total.today, 'all providers'), spendCard('This month', total.month, 'all providers'));
  if (!rows.length) {
    el.append(h('p', { class: 't-sec spend-none', text: 'No cloud spend recorded. Local models cost energy, not money (see the Energy column).' }));
    return;
  }
  el.append(h('table', { class: 'data spend-table' },
    h('thead', {}, h('tr', {}, h('th', { text: 'Provider' }), h('th', { text: 'Today' }), h('th', { text: 'Month' }))),
    h('tbody', {}, rows.map((r) => h('tr', {},
      h('td', { text: r.provider }), h('td', { class: 'num', text: fmtUSD(r.today) }), h('td', { class: 'num', text: fmtUSD(r.month) }))))));
}

function spendCard(label, usd, sub) {
  return h('div', { class: 'spend-card' },
    h('span', { class: 'stat-lbl', text: label }),
    h('span', { class: 'spend-val', text: fmtUSD(usd) }),
    h('span', { class: 'stat-sub', text: sub }));
}

function renderBudgets(el, budgets) {
  clear(el);
  if (!budgets.length) return;
  el.append(h('h2', { class: 'group-title', text: 'Budgets' }));
  for (const raw of budgets) {
    const b = budgetBar(raw);
    // Caps are exact configured values, not estimates: sub-cent ones keep
    // their digits (as in the gateway's 429 message).
    const caps = [b.soft ? `soft ${fmtUSDExact(b.soft)}` : '', b.hard ? `hard ${fmtUSDExact(b.hard)}` : ''].filter(Boolean).join(' · ');
    const state = b.state === 'hard' ? 'cap reached: requests refused' : b.state === 'soft' ? 'over the soft limit' : 'within budget';
    el.append(h('div', { class: `budget tone-${b.tone}` },
      h('div', { class: 'budget-head' },
        h('span', { class: 'budget-name', text: `${b.provider} · per ${b.period}` }),
        h('span', { class: 'budget-amt', text: `${fmtUSDExact(b.spent)}${b.cap ? ` of ${fmtUSDExact(b.cap)}` : ''}` })),
      h('div', { class: 'meter', role: 'meter', 'aria-valuemin': '0', 'aria-valuemax': '100', 'aria-valuenow': String(Math.round(b.pct)), 'aria-label': `${b.provider} budget` },
        h('span', { class: 'meter-fill', style: `width:${b.pct}%` }),
        b.softPct != null ? h('span', { class: 'meter-mark', style: `left:${b.softPct}%`, title: 'soft limit' }) : null),
      h('div', { class: 'budget-foot' }, h('span', { class: 'budget-state', text: state }), h('span', { text: caps }))));
  }
}

function renderPlans(el, plans) {
  clear(el);
  if (!plans.length) return;
  el.append(h('h2', { class: 'group-title plan-title', text: 'Subscriptions' }));
  for (const p of plans) {
    const win = planGauge(p.Window5hTokens, p.Window5hLimit);
    const week = planGauge(p.WeeklyTokens, p.WeeklyLimit);
    const reset = resetText(p.Window5hResetAt);
    const facts = [
      p.Sessions ? `${p.Sessions} session${p.Sessions === 1 ? '' : 's'}` : '',
      p.LastActivity ? `last active ${relTime(p.LastActivity)}` : '',
      p.CacheReadTokens || p.WeeklyCacheReadTokens ? `cache reads ${fmtTokens(p.CacheReadTokens)} (5 h) · ${fmtTokens(p.WeeklyCacheReadTokens)} (week), not counted` : '',
    ].filter((x) => x && !x.endsWith(' –'));
    el.append(h('div', { class: 'plan-card' },
      h('div', { class: 'gpu-head' }, h('span', { class: 'gpu-name', text: p.Name }), p.Kind ? h('span', { class: 'chip-tag', text: p.Kind }) : null),
      p.Err ? h('div', { class: 'alert ' + planErrTone(p.Err), text: p.Err }) : null,
      gauge('5 h window', win, reset || 'no activity in this window'),
      gauge('Weekly (rolling 7 days)', week, ''),
      facts.length ? h('p', { class: 'plan-facts', text: facts.join(' · ') }) : null));
  }
}

function gauge(label, g, sub) {
  return h('div', { class: `gauge tone-${g.tone}` },
    h('div', { class: 'budget-head' },
      h('span', { class: 'stat-lbl', text: label }),
      h('span', { class: 'budget-amt', text: g.pct == null ? g.label : `${g.label} · ${g.raw}%` })),
    h('div', { class: 'meter', role: 'meter', 'aria-label': label, 'aria-valuemin': '0', 'aria-valuemax': '100', 'aria-valuenow': String(Math.round(g.pct || 0)) },
      h('span', { class: 'meter-fill', style: `width:${g.pct || 0}%` })),
    h('div', { class: 'budget-foot' }, h('span', { text: sub }), h('span', { class: g.pct == null ? 'gauge-hint unset' : 'gauge-hint', text: g.hint })));
}

function renderRecent(wrap, list) {
  const recent = [...list].sort((a, b) => (Date.parse(b.Time) || 0) - (Date.parse(a.Time) || 0));
  if (!recent.length) { clear(wrap).append(h('p', { class: 't-sec', text: 'No requests yet.' })); return; }
  clear(wrap).append(h('table', { class: 'data usage-table' },
    h('thead', {}, h('tr', {}, COLS.map(([l]) => h('th', { text: l })))),
    h('tbody', {}, recent.map(row))));
}

function row(r) {
  const bad = r.Status >= 400 || r.Error;
  return h('tr', { class: bad ? 'row-err' : '', title: r.Error || '' },
    COLS.map(([label, fn]) => h('td', { 'data-label': label, text: fn(r) })));
}
