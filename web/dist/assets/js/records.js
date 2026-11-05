// Request records (gateway phase G3): pure helpers for the Records section
// in Usage. GET /api/records lists a day's lines (no text); a line's ref
// opens it with its text (GET /api/records/<ref>, audited server side).

export const FINISH_LABEL = {
  stop: 'done', length: 'out of budget', length_in_thinking: 'out of budget while thinking',
  tool_calls: 'tool calls', disconnect: 'client left', error: 'error', held: 'held by the guard',
};

// recordsQuery builds the list URL for a filter.
export function recordsQuery(f = {}) {
  const q = new URLSearchParams();
  if (f.day) q.set('day', f.day);
  if (f.client) q.set('client', f.client);
  if (f.session) q.set('session', f.session);
  q.set('limit', String(f.limit || 200));
  return `/api/records?${q}`;
}

// recordURL is a line's detail URL; null for a ref that is not day/file/offset.
export function recordURL(ref) {
  const m = /^(\d{4}-\d{2}-\d{2})\/(\d+-\d+)\/(\d+)$/.exec(String(ref || ''));
  return m ? `/api/records/${m[1]}/${m[2]}/${m[3]}` : null;
}

const pct = (a, b) => (b > 0 ? Math.round((100 * a) / b) : null);

// cacheShare is the percentage of a line's prompt the engine's cache covered.
export function cacheShare(l) {
  return pct(l.cached_tokens || 0, l.prompt_tokens || 0);
}

// summarize groups lines by client: requests, the average prompt, the share
// of prompt tokens from the cache, tool calls, tool results sent back and how
// many of those the client marked as errors; plus how replies ended.
export function summarize(lines = []) {
  const by = new Map();
  const finishes = {};
  for (const l of lines) {
    const k = l.client || 'other';
    const s = by.get(k) || { client: k, requests: 0, prompt: 0, cached: 0, calls: 0, results: 0, resultErrors: 0 };
    s.requests++;
    s.prompt += l.prompt_tokens || 0;
    s.cached += l.cached_tokens || 0;
    s.calls += (l.calls || []).length;
    s.results += l.results || 0;
    s.resultErrors += l.result_errors || 0;
    by.set(k, s);
    const f = l.finish || 'unknown';
    finishes[f] = (finishes[f] || 0) + 1;
  }
  const clients = [...by.values()].map((s) => ({
    ...s,
    avgPrompt: s.requests ? Math.round(s.prompt / s.requests) : 0,
    cachePct: pct(s.cached, s.prompt),
    errorPct: pct(s.resultErrors, s.results),
  })).sort((a, b) => b.requests - a.requests);
  return { clients, finishes };
}

// toolCounts counts the tool calls in replies, by tool name, most first.
export function toolCounts(lines = []) {
  const m = new Map();
  for (const l of lines) for (const c of l.calls || []) m.set(c.name || '?', (m.get(c.name || '?') || 0) + 1);
  return [...m.entries()].map(([name, n]) => ({ name, n })).sort((a, b) => b.n - a.n || a.name.localeCompare(b.name));
}

// switches says what a request asked for, in a few words.
export function switches(l) {
  const out = [];
  if (l.thinking === false) out.push('thinking off');
  else if (l.thinking === true) out.push('thinking on');
  if (l.effort) out.push(`effort ${l.effort}`);
  out.push(l.sampled ? 'sampled' : 'greedy');
  if (l.schema) out.push('schema');
  if (l.images) out.push(`${l.images} image${l.images === 1 ? '' : 's'}`);
  if (l.budget) out.push(`budget ${l.budget}`);
  return out.join(' · ');
}

// offText is shown when the server keeps no records.
export const OFF_TEXT = 'Request records are off. Turn them on in Settings (Gateway → Request records) to see what each client sends: prompt sizes, cache hits, tools and how replies end.';

// guardText is the agent guard's line (gateway phase G2): its mode and what
// it did since start; "" when it is off or unknown.
export function guardText(g) {
  if (!g || !g.mode || g.mode === 'off') return '';
  const parts = [`${g.screened || 0} tool results read`, `${g.flagged || 0} flagged`];
  if (g.mode === 'hold') parts.push(`${g.held || 0} calls held`);
  if (g.timed_out) parts.push(`${g.timed_out} not in time`);
  if (g.errors) parts.push(`${g.errors} errors`);
  return `Agent guard (${g.mode}, ${g.model || 'no model'}): ${parts.join(' · ')} since start.`;
}
