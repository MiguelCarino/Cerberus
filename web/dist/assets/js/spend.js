// Pure helpers for the Usage view's spend, budgets and subscription plan
// gauges. No DOM, so node --test imports them directly.

// fmtUSD renders a dollar amount: cents below $100, whole dollars above,
// and "<$0.01" for a tiny non-zero spend so it never reads as free.
export function fmtUSD(n) {
  if (n == null || !Number.isFinite(n) || n <= 0) return '$0.00';
  if (n < 0.01) return '<$0.01';
  if (n >= 100) return '$' + Math.round(n).toLocaleString('en');
  return '$' + n.toFixed(2);
}

// fmtUSDExact renders a configured amount (a budget cap, or the spend set
// against one): like fmtUSD, but a sub-cent value keeps two significant
// digits ("$0.0012", "$0.000001"), matching the gateway's 429 message, so
// a tiny test cap stays readable.
export function fmtUSDExact(n) {
  if (n == null || !Number.isFinite(n) || n <= 0 || n >= 0.01) return fmtUSD(n);
  const r = Number(n.toPrecision(2));
  const digits = Math.min(20, Math.max(2, 1 - Math.floor(Math.log10(r))));
  return '$' + r.toFixed(digits).replace(/0+$/, '');
}

// fmtTokens renders a token count compactly: 950, 12.3k, 4.56M.
export function fmtTokens(n) {
  if (n == null || !Number.isFinite(n) || n <= 0) return '0';
  if (n >= 1e9) return `${(n / 1e9).toFixed(2)}B`;
  if (n >= 1e6) return `${(n / 1e6).toFixed(n >= 1e8 ? 0 : n >= 1e7 ? 1 : 2)}M`;
  if (n >= 1e3) return `${(n / 1e3).toFixed(n >= 1e5 ? 0 : 1)}k`;
  return String(Math.round(n));
}

// spendRows merges spend.today and spend.month ({provider: usd} each) into
// rows sorted by month spend, plus totals.
export function spendRows(spend) {
  const today = (spend && spend.today) || {};
  const month = (spend && spend.month) || {};
  const names = new Set([...Object.keys(today), ...Object.keys(month)]);
  const rows = [...names].map((provider) => ({ provider, today: +today[provider] || 0, month: +month[provider] || 0 }))
    .sort((a, b) => b.month - a.month || b.today - a.today || a.provider.localeCompare(b.provider));
  const total = rows.reduce((t, r) => ({ today: t.today + r.today, month: t.month + r.month }), { today: 0, month: 0 });
  return { rows, total };
}

// budgetState is ok | soft | hard for a spend against its caps (0 = no cap).
export function budgetState(spent, soft, hard) {
  if (hard > 0 && spent >= hard) return 'hard';
  if (soft > 0 && spent >= soft) return 'soft';
  return 'ok';
}

// Branding status token for each budget state.
export const BUDGET_TONE = { ok: 'ok', soft: 'warn', hard: 'err' };

// budgetBar normalises one budget from /api/usage (config.Budget has
// lower-case JSON tags; either spelling is accepted) into what a bar needs:
// fill and soft-marker percentages against the larger cap, state and tone.
export function budgetBar(b) {
  b = b || {};
  const provider = b.provider ?? b.Provider ?? '*';
  const period = b.period ?? b.Period ?? 'month';
  const soft = +(b.soft_usd ?? b.SoftUSD) || 0;
  const hard = +(b.hard_usd ?? b.HardUSD) || 0;
  const spent = +(b.spent ?? b.Spent) || 0;
  const given = b.state ?? b.State;
  const state = given === 'ok' || given === 'soft' || given === 'hard' ? given : budgetState(spent, soft, hard);
  const cap = Math.max(hard, soft);
  const pct = cap > 0 ? clampPct((spent / cap) * 100) : 0;
  const softPct = hard > 0 && soft > 0 && soft < hard ? clampPct((soft / hard) * 100) : null;
  return { provider: provider === '*' ? 'All cloud providers' : provider, period, soft, hard, spent, cap, pct, softPct, state, tone: BUDGET_TONE[state] };
}

const clampPct = (p) => (Number.isFinite(p) ? Math.max(0, Math.min(100, p)) : 0);

// planGauge is one subscription gauge: used tokens against the user's own
// estimate. A limit of 0 means no estimate: no percentage, a hint instead.
export function planGauge(used, limit) {
  used = Math.max(0, +used || 0);
  limit = Math.max(0, +limit || 0);
  if (!limit) return { pct: null, tone: 'muted', used, limit, label: `${fmtTokens(used)} tokens`, hint: 'set estimates in config' };
  const raw = (used / limit) * 100;
  const tone = raw >= 90 ? 'err' : raw >= 70 ? 'warn' : 'ok';
  return { pct: clampPct(raw), raw: Math.round(raw), tone, used, limit, label: `${fmtTokens(used)} of ~${fmtTokens(limit)}`, hint: 'your estimate' };
}

// resetText says when the 5 h window resets, relative to now. Go's zero
// time (no activity) and past times give "".
export function resetText(iso, now = Date.now()) {
  const t = Date.parse(iso || '');
  if (!t || String(iso).startsWith('0001-') || t <= now) return '';
  const min = Math.ceil((t - now) / 60000);
  const span = min >= 60 ? `${Math.floor(min / 60)} h ${min % 60} min` : `${min} min`;
  return `resets in ${span}`;
}

// planErrTone is the alert tone for a subscription card's error: an agent
// that has not written any transcripts yet (a fresh install) is a note, not
// a failure.
export function planErrTone(err) {
  return /^no .*transcripts\b/i.test(String(err || '')) ? 'info' : 'err';
}
