// delegation.js: words for work agents handed to local models
// (GET /api/delegation, /api/sessions/{id}/delegation) and for the
// fine-tuning examples (GET /api/finetune). Pure: no DOM.

// fmtTokens: 950 → "950", 12 345 → "12.3k", 2 100 000 → "2.1M".
export function fmtTokens(n) {
  const v = Number(n) || 0;
  if (v >= 1e6) return `${(v / 1e6).toFixed(v >= 1e7 ? 0 : 1)}M`;
  if (v >= 1e3) return `${(v / 1e3).toFixed(v >= 1e4 ? 0 : 1)}k`;
  return String(Math.round(v));
}

// fmtWh: joules as Wh (or kWh).
export function fmtWh(j) {
  const wh = (Number(j) || 0) / 3600;
  if (wh >= 1000) return `${(wh / 1000).toFixed(2)} kWh`;
  if (wh >= 10) return `${wh.toFixed(0)} Wh`;
  if (wh >= 0.1) return `${wh.toFixed(1)} Wh`;
  return wh > 0 ? '<0.1 Wh' : '0 Wh';
}

// fmtSaved: "$0.04", "<$0.01".
export function fmtSaved(usd) {
  const v = Number(usd) || 0;
  if (v <= 0) return '';
  if (v < 0.01) return '<$0.01';
  return `$${v < 100 ? v.toFixed(2) : Math.round(v)}`;
}

// delegationText is one line for a summary row (or the total): what was
// handed off, its tokens, energy, and what it saved at the reference price.
// "" when nothing was delegated.
export function delegationText(row, reference = '') {
  const r = row || {};
  const n = r.requests || 0;
  if (!n) return '';
  const parts = [`${n} ${n === 1 ? 'job' : 'jobs'} to local models`,
    `${fmtTokens((r.prompt_tokens || 0) + (r.completion_tokens || 0))} tokens`];
  if (r.energy_j > 0) parts.push(fmtWh(r.energy_j));
  const saved = fmtSaved(r.saved_usd);
  if (saved && reference) {
    const ref = String(reference).split('/').pop();
    parts.push(saved.startsWith('<') ? `under ${saved.slice(1)} saved vs ${ref}` : `~${saved} saved vs ${ref}`);
  }
  return parts.join(' · ');
}

// finetuneText describes what is collected for training.
export function finetuneText(data) {
  const d = data || {};
  const st = d.stats || {};
  const mode = d.capture || 'off';
  if (mode === 'off' && !st.examples) return 'Not collecting. Turn it on in Settings → Fine-tuning.';
  const what = mode === 'cloud' ? 'delegated jobs and cloud answers' : mode === 'delegated' ? 'delegated jobs and their ratings' : 'nothing new (off)';
  const n = st.examples || 0;
  return `Collecting ${what}. ${n} ${n === 1 ? 'example' : 'examples'}, ${st.rated || 0} rated, ${st.usable || 0} ready to train on.`;
}
