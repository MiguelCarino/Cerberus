// Pure helpers for the "Get models" sheet (day phase 6): reading what the
// user typed (a Hugging Face URL, an Ollama name, an LM Studio query),
// merging download progress from the event stream, and the progress text.
// No DOM, so node --test imports them directly.

import { fmtBytes, fmtBps } from './monitor.js';

export const ENGINE_LABEL = { lmstudio: 'LM Studio', ollama: 'Ollama', llamacpp: 'llama.cpp' };

export const engineLabel = (e) => ENGINE_LABEL[e] || e || 'auto';

const FINAL = new Set(['done', 'failed', 'canceled']);

// isFinal reports whether a download has stopped for good.
export const isFinal = (it) => FINAL.has(it && it.state);

// classifyRef reads the search box: a Hugging Face URL (or "hf.co/…")
// goes to llama.cpp, "name:tag" or any name typed with the "ollama "
// prefix goes to Ollama, "lms " to LM Studio, and anything else (a bare
// name such as llama3.2) is left to the server: LM Studio when lms is
// installed, else Ollama. Returns {engine, ref, kind, error}; engine ""
// lets the server guess. kind is hf | ollama | lmstudio | auto | empty.
export function classifyRef(input) {
  const raw = String(input || '').trim();
  if (!raw) return { engine: '', ref: '', kind: 'empty', error: '' };
  const prefixed = /^(ollama|lms|lmstudio|hf|llamacpp)[:\s]\s*(.+)$/i.exec(raw);
  if (prefixed) {
    const p = prefixed[1].toLowerCase();
    const rest = prefixed[2].trim();
    if (p === 'ollama') return { engine: 'ollama', ref: rest, kind: 'ollama', error: validOllama(rest) };
    if (p === 'hf' || p === 'llamacpp') return classifyRef(/^https?:\/\//i.test(rest) ? rest : `https://huggingface.co/${rest}`);
    return { engine: 'lmstudio', ref: rest, kind: 'lmstudio', error: '' };
  }
  if (/^(https?:\/\/)?(www\.)?(huggingface\.co|hf\.co)\//i.test(raw)) {
    const url = /^https?:\/\//i.test(raw) ? raw : `https://${raw}`;
    let u;
    try { u = new URL(url); } catch { return { engine: 'llamacpp', ref: raw, kind: 'hf', error: 'Not a valid URL' }; }
    if (u.protocol !== 'https:') return { engine: 'llamacpp', ref: url, kind: 'hf', error: 'Hugging Face links must use https' };
    if (/\.gguf(\?|$)/i.test(u.pathname)) return { engine: 'llamacpp', ref: u.toString(), kind: 'hf', error: '' };
    // A repo page (huggingface.co/<owner>/<repo>, maybe /tree/…): LM Studio
    // downloads a whole repo, picking the build that fits this machine.
    const segs = u.pathname.split('/').filter(Boolean);
    if (segs.length >= 2 && (segs.length === 2 || segs[2] === 'tree') && segs.slice(0, 2).every((s) => /^[A-Za-z0-9][A-Za-z0-9._-]*$/.test(s))) {
      return { engine: 'lmstudio', ref: `https://huggingface.co/${segs[0]}/${segs[1]}`, kind: 'hfrepo', error: '' };
    }
    return { engine: 'llamacpp', ref: u.toString(), kind: 'hf', error: 'Paste a repo link, or the link of one .gguf file (Files tab → the file → copy link)' };
  }
  if (/^https?:\/\//i.test(raw)) return { engine: '', ref: raw, kind: 'hf', error: 'Only Hugging Face links can be downloaded' };
  // "name:tag" is how Ollama names models (llama3.2:3b, qwen2.5-coder:7b).
  if (/^[a-z0-9][a-z0-9._-]*(\/[a-z0-9._-]+)?:[A-Za-z0-9._-]+$/.test(raw)) {
    return { engine: 'ollama', ref: raw, kind: 'ollama', error: '' };
  }
  return { engine: '', ref: raw, kind: 'auto', error: '' };
}

function validOllama(name) {
  return /^[a-z0-9][a-z0-9._-]*(\/[a-z0-9._-]+)?(:[A-Za-z0-9._-]+)?$/.test(name) ? '' : 'Ollama names look like llama3.2:3b';
}

// normDownloads turns GET /api/downloads (a list, or {items, engines})
// into {items, engines}; engines maps an engine to whether it can
// download ({} when the server does not say).
export function normDownloads(reply) {
  const list = Array.isArray(reply) ? reply : (reply && (reply.items || reply.downloads)) || [];
  const engines = (reply && !Array.isArray(reply) && reply.engines) || {};
  return { items: sortDownloads(list.filter((x) => x && x.id)), engines };
}

const ORDER = { running: 0, verifying: 0, queued: 1 };

// sortDownloads lists active ones first, then queued (oldest first), then
// finished ones newest first.
export function sortDownloads(list) {
  const t = (x, k) => Date.parse(x[k]) || 0;
  return [...list].sort((a, b) => {
    const ra = ORDER[a.state] ?? 2;
    const rb = ORDER[b.state] ?? 2;
    if (ra !== rb) return ra - rb;
    if (ra < 2) return t(a, 'created') - t(b, 'created');
    return (t(b, 'ended') || t(b, 'created')) - (t(a, 'ended') || t(a, 'created'));
  });
}

// mergeDownload applies one item from the event stream (replacing the
// one with its id).
export function mergeDownload(list, item) {
  if (!item || !item.id) return list;
  return sortDownloads([item, ...list.filter((x) => x.id !== item.id)]);
}

// fmtETA renders seconds left ("45 s", "3 min", "1 h 05 min"); "" when
// unknown (negative).
export function fmtETA(sec) {
  if (!Number.isFinite(sec) || sec < 0) return '';
  if (sec < 60) return `${Math.max(1, Math.round(sec))} s`;
  if (sec < 3600) return `${Math.round(sec / 60)} min`;
  const h = Math.floor(sec / 3600);
  return `${h} h ${String(Math.round((sec % 3600) / 60)).padStart(2, '0')} min`;
}

// progressPct is 0..100, or null when the size is unknown and the engine
// gives no percent.
export function progressPct(it) {
  if (!it) return null;
  if (it.state === 'done') return 100;
  if (it.total > 0) return Math.min(100, Math.max(0, (it.bytes / it.total) * 100));
  if (it.percent > 0) return Math.min(100, it.percent);
  return null;
}

// progressText is the one-line state of a download.
export function progressText(it) {
  if (!it) return '';
  switch (it.state) {
    case 'queued': return 'Queued';
    case 'verifying': return 'Checking SHA-256…';
    case 'done': return ['Done', it.total ? fmtBytes(it.total) : '', it.verified ? 'SHA-256 verified' : '', 'not loaded'].filter(Boolean).join(' · ');
    case 'failed': return `Failed: ${it.error || 'unknown error'}`;
    case 'canceled': return 'Canceled';
    default: {
      const pct = progressPct(it);
      const size = it.total > 0 ? `${fmtBytes(it.bytes)} of ${fmtBytes(it.total)}` : it.bytes > 0 ? fmtBytes(it.bytes) : '';
      return [pct == null ? '' : `${Math.floor(pct)}%`, size, it.rate > 0 ? fmtBps(it.rate) : '', fmtETA(it.eta_sec) ? `${fmtETA(it.eta_sec)} left` : '',
        it.status || ''].filter(Boolean).join(' · ') || 'Starting…';
    }
  }
}

