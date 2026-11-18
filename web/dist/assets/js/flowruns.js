// Pure helpers for the Flows list (views/flows.js), where a flow is the
// recipe and its runs sit below it (Docker's image and its containers):
// grouping the runs list by flow, which runs a card shows, the toggle's
// words, the buttons a run offers, a run's input on one line and when it
// ran. No DOM and no network (web/test/flowruns.test.mjs). The runs come
// from GET /api/flows/runs?per_flow=N (internal/api/flows.go), each with
// flow_runs: how many runs its flow has in the caller's scope.

import { canCancelRun, isZeroTime, fmtDuration, runDuration } from './flows.js';

// PREVIEW is how many runs a flow card shows before "Show all N runs".
export const PREVIEW = 5;

// groupRuns splits a newest-first runs list by flow: byFlow maps each
// listed flow's id to its runs (order kept); orphans are the runs of flows
// not in the list (deleted, or moved to another person), one group per
// flow id, the group with the newest run first.
export function groupRuns(flowList = [], runs = []) {
  const known = new Set(flowList.map((f) => f.id));
  const byFlow = new Map(flowList.map((f) => [f.id, []]));
  const gone = new Map();
  for (const r of runs || []) {
    if (!r || !r.flow) continue;
    if (known.has(r.flow)) { byFlow.get(r.flow).push(r); continue; }
    if (!gone.has(r.flow)) gone.set(r.flow, { flow: r.flow, name: r.flow_name || r.flow, runs: [] });
    gone.get(r.flow).runs.push(r);
  }
  return { byFlow, orphans: [...gone.values()] };
}

// runTotal is how many runs a flow has: the server's count (flow_runs)
// when it gave one, else the runs in hand, whichever is larger.
export function runTotal(runs = []) {
  const counted = runs.reduce((m, r) => Math.max(m, Number(r && r.flow_runs) || 0), 0);
  return Math.max(counted, runs.length);
}

// shownRuns is what a card lists: the newest PREVIEW unless expanded.
export function shownRuns(runs = [], expanded = false, preview = PREVIEW) {
  return expanded ? runs : runs.slice(0, preview);
}

// toggleText: "Show all 12 runs" / "Show fewer", or "" when everything
// already fits.
export function toggleText(total, expanded, preview = PREVIEW) {
  if (total <= preview) return '';
  return expanded ? 'Show fewer' : `Show all ${total} runs`;
}

// needsFetch: an expanded card whose runs are fewer than its total reads
// the rest (GET /api/flows/runs?flow=<id>).
export const needsFetch = (runs = [], expanded = false) => expanded && runs.length < runTotal(runs);

// runActions: Stop while the run can still be stopped (queued, running,
// waiting); Run again when its flow still exists and the run was not a
// test of an unsaved draft.
export function runActions(run, { flowExists = true } = {}) {
  return {
    stop: !!run && canCancelRun(run.state),
    again: !!run && flowExists && !run.test,
  };
}

// inputLine is a run's input on one line (whitespace folded), "" when it
// had none.
export const inputLine = (input) => String(input ?? '').replace(/\s+/g, ' ').trim();

// lastInput is the input of a flow's newest run that had one ("" when
// none did): Run offers it so the same prompt is one click away.
export function lastInput(runs = []) {
  for (const r of runs) if (r && !r.test && String(r.input ?? '').trim()) return r.input;
  return '';
}

// runWhen says when a run started and ended: {start: ISO, end: ISO or "",
// took: "2 min 05 s" or ""}. took counts running time, so a run still
// going reads as "for 3 min".
export function runWhen(run, now = Date.now()) {
  if (!run) return { start: '', end: '', took: '' };
  const ms = runDuration(run, now);
  return {
    start: run.created || '',
    end: isZeroTime(run.ended) ? '' : run.ended,
    took: ms > 0 ? fmtDuration(ms) : '',
  };
}
