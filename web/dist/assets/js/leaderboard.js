// Pure helpers for the Leaderboard section in Usage (day phase 6): the
// period/by/metric choices, the query, and the bar rows with each metric's
// formatting. No DOM, so node --test imports them directly.

import { fmtUSD, fmtTokens } from './spend.js';
import { fmtKWh, fmtMoney } from './monitor.js';

export const PERIODS = [['today', 'Today'], ['7d', '7 days'], ['30d', '30 days'], ['month', 'This month']];
export const BYS = [['session', 'Sessions'], ['agent', 'Agents'], ['model', 'Models'], ['repo', 'Repos']];
export const METRICS = [['cost', 'Cost'], ['tokens', 'Tokens'], ['energy', 'Energy'], ['time', 'Wall time']];

const pick = (list, v, dflt) => (list.some(([k]) => k === v) ? v : dflt);

// normChoice keeps a stored choice only when it is still valid.
export function normChoice(c = {}) {
  return {
    period: pick(PERIODS, c.period, '7d'),
    by: pick(BYS, c.by, 'model'),
    metric: pick(METRICS, c.metric, 'cost'),
  };
}

// leaderboardQuery is the GET /api/leaderboard path for a choice.
export function leaderboardQuery(c, limit = 20) {
  const n = normChoice(c);
  return `/api/leaderboard?period=${n.period}&by=${n.by}&metric=${n.metric}&limit=${limit}`;
}

// fmtWall renders seconds of wall time ("45 s", "12 min", "3.4 h").
export function fmtWall(sec) {
  if (!Number.isFinite(sec) || sec <= 0) return '0 s';
  if (sec < 60) return `${Math.round(sec)} s`;
  if (sec < 3600) return `${Math.round(sec / 60)} min`;
  return `${(sec / 3600).toFixed(sec < 36000 ? 1 : 0)} h`;
}

// metricText renders a row's ranked metric, plus a secondary line (the
// other currency or the inputs behind it).
export function metricText(row, metric, board = {}) {
  switch (metric) {
    case 'cost': {
      const cur = board.cost_currency || 'USD';
      const main = cur === 'USD' ? fmtUSD(row.cost_usd ?? row.value) : fmtMoney(row.value, cur);
      return { main, sub: cur !== 'USD' && row.cost_usd ? `${fmtUSD(row.cost_usd)} USD` : `${row.requests || 0} req` };
    }
    case 'tokens':
      return { main: fmtTokens(row.tokens ?? row.value), sub: `${fmtTokens(row.prompt_tokens)} in · ${fmtTokens(row.completion_tokens)} out` };
    case 'energy':
      return { main: fmtKWh(row.energy_kwh ?? row.value), sub: row.energy_cost > 0 ? fmtMoney(row.energy_cost, board.currency || 'MXN') : '' };
    case 'time':
      return { main: fmtWall(row.wall_sec ?? row.value), sub: row.sessions ? `${row.sessions} session${row.sessions === 1 ? '' : 's'}` : '' };
    default:
      return { main: String(row.value ?? ''), sub: '' };
  }
}

// barRows turns a board into [{key, label, value, pct, main, sub, rank}],
// largest first, pct relative to the largest value (0..100).
export function barRows(board) {
  const metric = board && board.metric;
  const rows = ((board && board.rows) || []).filter((r) => r && Number.isFinite(+r.value));
  const sorted = [...rows].sort((a, b) => b.value - a.value);
  const max = sorted.length ? Math.max(0, sorted[0].value) : 0;
  return sorted.map((r, i) => {
    const t = metricText(r, metric, board);
    return {
      key: r.key || r.label || String(i),
      label: r.label || r.key || '–',
      value: +r.value,
      pct: max > 0 ? Math.max(0, Math.min(100, (r.value / max) * 100)) : 0,
      main: t.main,
      sub: t.sub,
      rank: i + 1,
      href: rowLink(board.by, r),
    };
  });
}

// rowLink is the in-app route for a row (a session's page), or "".
export function rowLink(by, row) {
  if (by === 'session' && row && row.key && !row.key.startsWith('(')) return `#/s/${encodeURIComponent(row.key)}`;
  return '';
}

// totalText renders the board's total for its metric.
export function totalText(board) {
  if (!board || !board.total) return '';
  return metricText(board.total, board.metric, board).main;
}

// emptyText is the note for a board with no rows. Requests that ranked as
// zero (local models cost energy, not money) are not "nothing recorded".
export function emptyText(board) {
  const n = (board && board.total && board.total.requests) || 0;
  if (!n) return 'Nothing recorded in this period.';
  const req = n + (n === 1 ? ' request' : ' requests');
  if (board.metric === 'cost') return req + ' in this period, none with a cloud cost: local models cost energy, not money. Rank by Energy or Tokens instead.';
  return req + ' in this period, none with a value for this metric.';
}
