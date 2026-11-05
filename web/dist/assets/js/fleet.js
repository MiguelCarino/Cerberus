// Pure helpers for fleet tasks (one instruction, one session per repo): state
// counts, the task template preview and accept/discard result tallies. No
// DOM, so node --test imports them directly.

import { repoName } from './util.js';

// batchSessions returns a batch's sessions in its own order, preferring the
// live copy from the store (kept current by /api/events) over the one the
// batch endpoint returned.
export function batchSessions(ids, fetched, live) {
  const byId = new Map((fetched || []).map((s) => [s.ID, s]));
  return (ids || []).map((id) => (live && live(id)) || byId.get(id)).filter(Boolean);
}

// stateCounts tallies sessions by state.
export function stateCounts(list) {
  const out = {};
  for (const s of list || []) out[s.State] = (out[s.State] || 0) + 1;
  return out;
}

// Display order for count chips; unknown states follow alphabetically.
const ORDER = ['waiting', 'starting', 'running', 'checking', 'retrying', 'escalated', 'queued', 'passed', 'failed', 'error', 'stopped', 'accepted', 'discarded'];

// countChips orders {state: n} for display, dropping zeros.
export function countChips(counts) {
  const rank = (s) => { const i = ORDER.indexOf(s); return i < 0 ? ORDER.length : i; };
  return Object.entries(counts || {}).filter(([, n]) => n > 0)
    .sort(([a], [b]) => rank(a) - rank(b) || a.localeCompare(b))
    .map(([state, n]) => ({ state, n }));
}

// batchProgress sums counts: total, finished (no longer running) and passed
// (what "Accept all passed" would take).
export function batchProgress(counts) {
  let total = 0;
  let finished = 0;
  for (const [s, n] of Object.entries(counts || {})) {
    total += n;
    if (['passed', 'failed', 'error', 'stopped', 'accepted', 'discarded'].includes(s)) finished += n;
  }
  return { total, finished, passed: (counts && counts.passed) || 0 };
}

// expandTask previews a task template for one repo, as the server expands
// it: {repo} = the repo's path, {name} = its folder name.
export function expandTask(template, repo) {
  const path = (repo && (repo.Path || repo.path)) || '';
  const name = (repo && (repo.Name || repo.name)) || repoName(path);
  return String(template || '').replaceAll('{repo}', path).replaceAll('{name}', name);
}

// resultCounts tallies BatchResult.Result values.
export function resultCounts(results) {
  const out = { accepted: 0, skipped: 0, failed: 0, discarded: 0 };
  for (const r of results || []) out[r.Result] = (out[r.Result] || 0) + 1;
  return out;
}

// resultSummary is a one-line toast for accept/discard results.
export function resultSummary(results) {
  const c = resultCounts(results);
  const parts = Object.entries(c).filter(([, n]) => n > 0).map(([k, n]) => `${n} ${k}`);
  return parts.length ? parts.join(', ') : 'Nothing to do';
}
