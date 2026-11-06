// Evals (E1): pure helpers for the Evals section in Usage and the run page
// (#/evals/runs/<run>). GET /api/evals lists the evals with their last
// run; GET /api/evals/runs/{run} is {run, reports, summary}. No DOM here,
// so node tests cover it.

// The matrix marks of a criterion result.
export const MARKS = { pass: '✓', fail: '✗', skipped: '–', error: '!' };
const WORDS = { pass: 'passed', fail: 'failed', skipped: 'skipped', error: 'error', pending: 'not judged yet' };

// cellMark is the mark of a state ('·' when not judged yet).
export const cellMark = (state) => MARKS[state] || '·';

// cellTone maps a result state to a tone class.
export function cellTone(state) {
  switch (state) {
    case 'pass': return 'ok';
    case 'fail': return 'err';
    case 'error': return 'warn';
    default: return 'muted';
  }
}

// cellText says a state in words (for titles and screen readers).
export const cellText = (state) => WORDS[state] || WORDS.pending;

// effectiveGates mirrors evals.EffectiveGates: the eval's gates, or the
// implicit "main" gate.
export function effectiveGates(ev) {
  const gates = (ev && ev.gates) || [];
  if (!gates.length) return [{ id: 'main', name: 'Main', max_iterations: 1 }];
  return gates.map((g) => ({ ...g, max_iterations: Math.max(1, g.max_iterations || 1) }));
}

// gateOf mirrors evals.GateOf.
export function gateOf(ev, c) {
  return (c && c.gate) || effectiveGates(ev)[0].id;
}

// gateColumns groups the criteria by gate, in gate order, keeping the
// definition's order inside a gate: [{gate, name, criteria: [id]}].
export function gateColumns(ev) {
  const crits = (ev && ev.criteria) || [];
  return effectiveGates(ev).map((g) => ({
    gate: g.id,
    name: g.name || g.id,
    criteria: crits.filter((c) => gateOf(ev, c) === g.id).map((c) => c.id),
  })).filter((g) => g.criteria.length);
}

// matrixRows is one row per session slot of a run: its label, session,
// slot state, and a cell per criterion ({state, result}) from its report
// (null when it has none yet). Rows follow the run's slot order.
export function matrixRows(detail) {
  const run = (detail && detail.run) || {};
  const reports = (detail && detail.reports) || [];
  const bySession = new Map(reports.map((r) => [r.session, r]));
  const repeats = Math.max(1, (run.snapshot && run.snapshot.repeat) || 1);
  return (run.sessions || []).map((s) => {
    const rep = s.session ? bySession.get(s.session) || null : null;
    const cells = {};
    for (const x of (rep && rep.results) || []) cells[x.criterion] = { state: x.state, result: x };
    return {
      key: `${s.setup}#${s.repeat}`,
      label: repeats > 1 ? `${s.setup} #${s.repeat}` : s.setup,
      setup: s.setup,
      session: s.session || '',
      state: s.state,
      live: s.live || '',
      error: s.error || '',
      report: rep,
      cells,
    };
  });
}

// runTone maps a run state to a tone.
export function runTone(state) {
  switch (state) {
    case 'done': return 'ok';
    case 'failed': return 'err';
    case 'canceled': return 'warn';
    default: return 'info';
  }
}

// slotTone maps a session slot state to a tone.
export function slotTone(state) {
  switch (state) {
    case 'judged': return 'ok';
    case 'error': return 'err';
    case 'canceled': return 'warn';
    default: return 'info';
  }
}

export const isFinalRun = (state) => state === 'done' || state === 'canceled' || state === 'failed';
export const canCancel = (state) => state === 'running';
export const canRejudge = (run) => !!run && isFinalRun(run.state) && (run.sessions || []).some((s) => s.session);

// fmtWall shows milliseconds as "12.3 s", "4 min 5 s" or "–".
export function fmtWall(ms) {
  const n = Number(ms) || 0;
  if (n <= 0) return '–';
  if (n < 60000) return `${(n / 1000).toFixed(n < 10000 ? 1 : 0)} s`;
  const m = Math.floor(n / 60000);
  const s = Math.round((n % 60000) / 1000);
  return s ? `${m} min ${s} s` : `${m} min`;
}

// usageText is "1,234 tokens · $0.0123 · 1.5 Wh" (parts left out when 0).
export function usageText(u) {
  if (!u) return '';
  const parts = [];
  if (u.tokens) parts.push(`${Number(u.tokens).toLocaleString('en-US')} tokens`);
  if (u.cost_usd) parts.push(`$${Number(u.cost_usd).toFixed(4)}`);
  if (u.energy_wh) parts.push(`${Number(u.energy_wh).toFixed(u.energy_wh < 10 ? 2 : 1)} Wh`);
  return parts.join(' · ');
}

// runTotals sums the reports: session and judge usage, and the longest
// wall time.
export function runTotals(detail) {
  const out = { session: { tokens: 0, cost_usd: 0, energy_wh: 0 }, judge: { tokens: 0, cost_usd: 0, energy_wh: 0 }, wallMs: 0 };
  for (const r of (detail && detail.reports) || []) {
    for (const k of ['tokens', 'cost_usd', 'energy_wh']) {
      out.session[k] += Number(r.session_usage && r.session_usage[k]) || 0;
    }
    out.wallMs = Math.max(out.wallMs, Number(r.wall_ms) || 0);
  }
  const j = (detail && detail.run && detail.run.judge_usage) || {};
  for (const k of ['tokens', 'cost_usd', 'energy_wh']) out.judge[k] = Number(j[k]) || 0;
  return out;
}

// gatesText is "2/3 gates" for a report, with the first failing gate.
export function gatesText(rep) {
  if (!rep) return '';
  const t = `${rep.gates_passed || 0}/${rep.gates_total || 0} gates`;
  return rep.first_failing_gate ? `${t} · stopped at ${rep.first_failing_gate}` : t;
}

// lastRunText is the list's last-run line.
export function lastRunText(lr) {
  if (!lr) return 'never run';
  return `${lr.state} · ${lr.judged || 0}/${lr.sessions || 0} judged`;
}

// parseEvalJSON reads the definition editor's text: {ok, eval} or
// {ok: false, error}.
export function parseEvalJSON(text) {
  let v;
  try { v = JSON.parse(String(text || '')); } catch (e) { return { ok: false, error: `Not valid JSON: ${e.message}` }; }
  if (!v || typeof v !== 'object' || Array.isArray(v)) return { ok: false, error: 'The definition is a JSON object.' };
  return { ok: true, eval: v };
}

// editableJSON is a definition as the editor shows it: without the stored
// fields the server keeps (owner, update time), version kept for the save.
export function editableJSON(ev) {
  if (!ev) return '';
  const { owner, owner_name: ownerName, updated, ...rest } = ev;
  void owner; void ownerName; void updated;
  return JSON.stringify(rest, null, 2);
}

// A starting definition for "New eval".
export const TEMPLATE = {
  name: 'Health endpoint',
  task: 'Add a GET /health endpoint to the HTTP server that answers 200 with the JSON {"ok": true}, and a test for it.',
  workspace: 'scratch',
  gates: [{ id: 'build', name: 'Build', max_iterations: 1 }, { id: 'test', name: 'Test', max_iterations: 1 }],
  criteria: [
    { id: 'has_server', prompt: 'There is an HTTP server entry point', gate: 'build' },
    { id: 'health_route', prompt: 'GET /health returns 200 with the JSON {"ok": true}', gate: 'build', depends_on: ['has_server'] },
    { id: 'tests_pass', prompt: "The project's tests run and none fail", gate: 'test', evidence: ['checks', 'files'] },
  ],
  setups: [{ label: 'default', agent: 'cerberus' }],
  repeat: 1,
  judge_model: '',
  persona: '',
};

// evalRunEventFor: does a store event concern run id (its evalrun event,
// or an evals resync)?
export function evalRunEventFor(kind, payload, id) {
  if (kind === 'resync') return !!payload && payload.what === 'evals';
  if (kind === 'evalrun') return !!payload && (!id || payload.run === id);
  return false;
}

// problemsText lists validation problems, errors first.
export function problemsText(ps) {
  const list = (ps || []).slice().sort((a, b) => (a.severity === b.severity ? 0 : a.severity === 'error' ? -1 : 1));
  return list.map((p) => `${p.severity}: ${p.field ? `${p.field}: ` : ''}${p.message}`).join('\n');
}

// ---------- E2: iterations and feedback ----------

// iterationsText names the gates that took more than one iteration:
// "build: 2 iterations" ('' when every gate took one).
export function iterationsText(rep) {
  const many = ((rep && rep.gates) || []).filter((g) => (g.iterations || 0) > 1);
  return many.map((g) => `${g.gate}: ${g.iterations} iterations`).join(' · ');
}

// cellHistory is the earlier iterations of one criterion in a report,
// oldest first (the matrix shows the latest).
export function cellHistory(rep, criterion) {
  return ((rep && rep.history) || []).filter((x) => x.criterion === criterion);
}

// feedbackLabel is one feedback message's heading: its gate, the
// iteration it followed, its persona, and whether it reached the session.
export function feedbackLabel(fb) {
  if (!fb) return '';
  const parts = [`${fb.gate || 'gate'} · after iteration ${fb.iteration || 1}`];
  parts.push(fb.persona ? `persona ${fb.persona}` : 'reviewer');
  if (fb.fallback) parts.push('plain text (the model did not write it)');
  parts.push(fb.sent ? 'sent' : 'not sent');
  return parts.join(' · ');
}

// trajectoryPath is the API path of a session's trajectory (E4): ATIF JSON,
// or with format 'jsonl' its raw request records. A report carries the
// same path as `trajectory`.
export function trajectoryPath(session, format = 'atif') {
  const f = format === 'jsonl' ? 'jsonl' : 'atif';
  return `/api/sessions/${encodeURIComponent(session)}/trajectory?format=${f}`;
}

// trajectoryFile is the name a downloaded trajectory is saved under.
export function trajectoryFile(session, format = 'atif') {
  const safe = String(session || 'session').replace(/[^A-Za-z0-9._-]/g, '_').slice(0, 128) || 'session';
  return format === 'jsonl' ? `cerberus-${safe}.records.jsonl` : `cerberus-${safe}.atif.json`;
}
