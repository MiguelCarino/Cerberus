// models.js: pure helpers for the Models view. No DOM.

// deleteModelMessage words the confirm for deleting a local model from disk.
export function deleteModelMessage(m) {
  const size = m && m.SizeMiB ? (m.SizeMiB >= 1024 ? `${(m.SizeMiB / 1024).toFixed(1)} GB` : `${m.SizeMiB} MB`) : '';
  const how = {
    llamacpp: 'Its .gguf file is removed from your models folder.',
    lmstudio: "Its files are removed from LM Studio's models folder.",
    ollama: 'Ollama removes it.',
  }[m && m.Backend] || 'Its files are removed.';
  const parts = [how];
  if (size) parts.push(`That frees about ${size}.`);
  if (m && m.Loaded) parts.push('It is unloaded first.');
  parts.push('To use it again you have to download it again.');
  return { title: `Delete ${(m && m.ID) || 'model'}`, message: parts.join(' ') };
}

// tokensK is a token count, short: 950, 12k, 1.5k.
function tokensK(n) {
  const v = Math.max(0, Math.round(Number(n) || 0));
  if (v < 1000) return String(v);
  return v < 10000 ? `${(v / 1000).toFixed(1).replace(/\.0$/, '')}k` : `${Math.round(v / 1000)}k`;
}

// prefillText is a request reading its prompt (step web-api), from
// llama.cpp's progress: "reading prompt 12k/48k". '' when there is none or
// it is done.
export function prefillText(p) {
  if (!p || p.done || p.Done) return '';
  const total = Number(p.total ?? p.Total) || 0;
  const done = Number(p.processed ?? p.Processed) || 0;
  if (total <= 0 || done >= total) return '';
  return `reading prompt ${tokensK(done)}/${tokensK(total)}`;
}

// noProgressNote says why a busy LM Studio or Ollama model shows no
// prompt reading: only llama.cpp reports it.
export function noProgressNote(m) {
  if (!m || !(m.InFlight > 0) || m.Prefill) return '';
  return { lmstudio: 'LM Studio gives no prompt progress', ollama: 'Ollama gives no prompt progress' }[m.Backend] || '';
}

// mergeLive applies a "models" event (the live part of each model, step
// web-api) to the list from /api/models. It returns the new list and
// whether an id was unknown (then the page refetches the list). The event
// lists every local model but only the cloud models with work going, so a
// cloud model missing from it is idle now: its live fields are cleared.
export function mergeLive(models, live, now = Date.now()) {
  const byId = new Map((live || []).map((x) => [x.id, x]));
  let unknown = false;
  const known = new Set((models || []).map((m) => m.ID));
  for (const id of byId.keys()) if (!known.has(id)) unknown = true;
  const out = (models || []).map((m) => {
    const x = byId.get(m.ID);
    if (!x) {
      if (m.Local || !(m.InFlight || m.Waiting || m.BusySince || m.WaitSince || m.Prefill)) return m;
      return { ...m, InFlight: 0, Waiting: 0, BusySince: '', WaitSince: '', BusyForS: 0, LongestWaitS: 0, Prefill: null };
    }
    const since = (t) => { const v = t ? Date.parse(t) : NaN; return Number.isFinite(v) ? Math.max(0, (now - v) / 1000) : 0; };
    return {
      ...m, Loaded: !!x.loaded, InFlight: x.in_flight || 0, MaxParallel: x.max_parallel || 0, Waiting: x.waiting || 0,
      LongestWaitS: since(x.wait_since), BusyForS: since(x.busy_since), BusySince: x.busy_since || '',
      WaitSince: x.wait_since || '', Stalls: x.stalls || 0, LastUsed: x.last_used || m.LastUsed,
      LastFailure: x.last_failure || null, Prefill: x.in_flight ? m.Prefill : null,
    };
  });
  return { models: out, unknown };
}

// withSince stamps when each busy model's oldest request started and its
// oldest queued one began waiting, from /api/status's ages, so the times
// can tick on the page without asking again.
export function withSince(models, now = Date.now()) {
  const at = (s) => (s > 0 ? new Date(now - s * 1000).toISOString() : '');
  return (models || []).map((m) => ({ ...m, BusySince: at(m.BusyForS), WaitSince: at(m.LongestWaitS) }));
}

// tickBusy recomputes the ages from the stamps (withSince, mergeLive).
export function tickBusy(models, now = Date.now()) {
  const since = (t) => { const v = t ? Date.parse(t) : NaN; return Number.isFinite(v) ? Math.max(0, (now - v) / 1000) : 0; };
  return (models || []).map((m) => (m.BusySince || m.WaitSince
    ? { ...m, BusyForS: since(m.BusySince), LongestWaitS: since(m.WaitSince) } : m));
}

// reserveText is the VRAM reserve in effect (GET /api/gpu vram_reserve):
// "Railguard line 97% · reserve 1505 MiB". '' when unknown.
export function reserveText(r) {
  if (!r || !r.source) return '';
  switch (r.source) {
    case 'railguard': return `Railguard line ${r.line_pct}% · reserve ${r.mib} MiB`;
    case 'fixed': return `VRAM reserve ${r.mib} MiB (set in Settings)`;
    case 'default': return `VRAM reserve ${r.mib} MiB (no Railguard)`;
    case 'off': return 'VRAM reserve off';
    default: return '';
  }
}

// busyText is a model's load line (gateway phase G5): how long the oldest
// request has been answering, the queue and its oldest wait, and stalls.
export function busyText(m) {
  const secs = (x) => (x >= 90 ? `${Math.round(x / 60)} min` : `${Math.round(x)} s`);
  const out = [];
  const reading = prefillText(m.Prefill);
  if (reading) out.push(reading);
  if (m.BusyForS >= 60) out.push(`busy for ${secs(m.BusyForS)}`);
  if (m.Waiting > 0) out.push(`${m.Waiting} queued${m.LongestWaitS >= 1 ? `, oldest ${secs(m.LongestWaitS)}` : ''}`);
  if (m.Stalls > 0) out.push(`${m.Stalls} stall${m.Stalls === 1 ? '' : 's'} since start`);
  const note = noProgressNote(m);
  if (note && m.BusyForS >= 60) out.push(note);
  return out.join(' · ');
}

// failureText is a model's last engine death (ModelInfo.LastFailure), shown
// for a day: "Ended by Railguard: VRAM 97% full (3 min ago)". '' when none.
export function failureText(m, now = Date.now()) {
  const f = m && m.LastFailure;
  if (!f || !f.Text) return '';
  const t = Date.parse(f.Time);
  const age = Number.isFinite(t) ? Math.max(0, (now - t) / 1000) : NaN;
  if (age > 86400) return '';
  const ago = !Number.isFinite(age) ? '' : age < 90 ? `${Math.round(age)} s ago`
    : age < 5400 ? `${Math.round(age / 60)} min ago` : `${Math.round(age / 3600)} h ago`;
  const text = f.Text.charAt(0).toUpperCase() + f.Text.slice(1);
  return ago ? `${text} (${ago})` : text;
}
