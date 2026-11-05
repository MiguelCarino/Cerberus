// Bench runs (gateway phase G6): pure helpers for the Bench section in
// Usage. GET /api/bench lists saved runs and the one going now; POST starts
// one (admins).

const median = (xs) => {
  const v = xs.filter((x) => x > 0).sort((a, b) => a - b);
  if (!v.length) return 0;
  const m = Math.floor(v.length / 2);
  return v.length % 2 ? v[m] : (v[m - 1] + v[m]) / 2;
};

// runSummary reduces a run to one row: decode tok/s and TTFT over the text
// kinds (medians of the kinds' medians), prefill tok/s over the prefill
// kinds, tokens per joule, and errors.
export function runSummary(run) {
  const kinds = (run && run.kinds) || [];
  const text = kinds.filter((k) => !String(k.kind).startsWith('prefill'));
  const pre = kinds.filter((k) => String(k.kind).startsWith('prefill'));
  return {
    id: run.id,
    model: (run.options && run.options.model) || '',
    label: (run.options && run.options.label) || '',
    state: run.state || '',
    time: run.time,
    decode: median(text.map((k) => k.median && k.median.decode_tps)),
    ttft: median(text.map((k) => k.median && k.median.ttft_ms)),
    prefill: median(pre.map((k) => k.median && k.median.prefill_tps)),
    tokJ: median(kinds.map((k) => k.median && k.median.tok_per_j)),
    errors: kinds.reduce((n, k) => n + (k.errors || 0), 0),
    draft: !!(run.config && run.config.draft),
  };
}

// fmtRate shows tok/s, or a dash.
export const fmtRate = (v) => (v > 0 ? `${v >= 100 ? Math.round(v) : v.toFixed(1)} tok/s` : '–');

// benchBody is the POST body for the start form.
export function benchBody({ model, label, repeat, prefill, force }) {
  const out = { model: String(model || '').trim() };
  if (label && String(label).trim()) out.label = String(label).trim();
  const n = Number(repeat);
  if (Number.isFinite(n) && n >= 1 && n <= 20) out.repeat = Math.round(n);
  if (prefill) out.prefill = true;
  if (force) out.force = true;
  return out;
}
