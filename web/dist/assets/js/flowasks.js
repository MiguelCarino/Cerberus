// Approvals that hold a flow run (GET /api/flows/runs/{run} → asks): a
// session the run started (a session step, a fan-out item run as a
// session, or a child either of them spawned) waiting at the agent's tool
// approval (kind "tool") or a spawned child waiting to be let start (kind
// "spawn"). Pure helpers for the run page; the buttons are the session
// list's own (views/approval.js, ctl.js approveSession), so an answer goes
// through the same session routes, checks and audit.

// normAsks keeps the well-formed asks of a run reply, oldest first as sent.
export function normAsks(list) {
  if (!Array.isArray(list)) return [];
  return list.filter((a) => a && typeof a.session === 'string' && a.session && (a.kind === 'tool' || a.kind === 'spawn'))
    .map((a) => ({
      session: a.session,
      name: typeof a.name === 'string' && a.name ? a.name : a.session,
      kind: a.kind,
      step: typeof a.step === 'string' ? a.step : '',
      item: Number.isInteger(a.item) ? a.item : null,
      via: typeof a.via === 'string' ? a.via : '',
      ask: typeof a.ask === 'string' ? a.ask : '',
      seq: Number.isInteger(a.seq) ? a.seq : 0,
      always: Array.isArray(a.always) ? a.always : [],
      since: typeof a.since === 'string' ? a.since : '',
    }));
}

// askSession is the session the approval buttons answer (views/approval.js
// reads these fields of a session from the live list).
export function askSession(a) {
  if (!a || a.kind !== 'tool') return null;
  return { ID: a.session, Name: a.name, State: 'waiting', WaitingReason: 'approval', WaitingAsk: a.ask, AskSeq: a.seq, WaitingAlways: a.always };
}

// askKey identifies one wait (a new prompt in the same session is a new key).
export const askKey = (a) => (a ? `${a.kind}/${a.session}/${a.seq}/${a.ask}` : '');

// asksKey identifies a list of asks (redraw only when it changes).
export const asksKey = (asks) => (asks || []).map(askKey).join('\n');

// asksFor are the asks of one step (and, with item, of one fan-out item).
export function asksFor(asks, step, item = null) {
  return (asks || []).filter((a) => a.step === step && (item === null || a.item === item));
}

// asksElsewhere are the asks the page cannot place on a step.
export function asksElsewhere(asks, stepIds) {
  const ids = new Set(stepIds || []);
  return (asks || []).filter((a) => !a.step || !ids.has(a.step));
}

// askWhat says what the session wants, in a line.
export function askWhat(a) {
  if (!a) return '';
  if (a.kind === 'spawn') return `${a.name} asks to start`;
  return a.ask || 'An approval (open the session to see what for)';
}

// askWhere says which part of the run asks: the step's name, the item's
// title, and "a session it started" for a child.
export function askWhere(a, steps = [], items = []) {
  if (!a) return '';
  const st = (steps || []).find((s) => s && s.id === a.step);
  const parts = [];
  if (st) parts.push(st.name || st.id);
  else if (a.step) parts.push(a.step);
  if (a.item !== null && a.item !== undefined) {
    const it = (items || []).find((x) => x && x.index === a.item);
    parts.push((it && it.title) || `item ${a.item + 1}`);
  }
  if (a.via) parts.push('a session it started');
  return parts.join(' · ');
}

// askLine is a step or item summary line while it waits.
export function askLine(list) {
  const n = (list || []).length;
  if (!n) return '';
  if (n > 1) return `waiting for your approval (${n})`;
  return `waiting for your approval: ${askWhat(list[0])}`;
}
