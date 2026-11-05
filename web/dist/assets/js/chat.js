// Pure helpers for the LLM chat view (no DOM, so node --test runs them):
// the mascot stage's state machine and frame timing, the stage grid fit,
// image scaling maths, permission-card arming, chat events → mood
// reactions, stored messages → transcript items, the status and end lines,
// tool summaries, the model picker's groups, input history, find, corner
// cycling and a small SSE parser for the quick answer.

// ---------- stage: state machine and timing ----------

export const DONE_TO_IDLE_S = 15; // "done" / "error" held this long, then "idle"
export const FAST_MS = 100; // 10 ticks/s while something happens
export const SLOW_MS = 400; // 2.5/s after QUIET_S of nothing
export const QUIET_S = 10;
export const SLEEP_S = 120; // then the stage stops until the next event

// StageMachine follows a chat's events to the mascot's state: idle,
// thinking, tool (with the tool's name for the prop), writing, done, error.
// now is in seconds (any monotonic clock).
export class StageMachine {
  constructor(now = 0) {
    this.state = 'idle';
    this.tool = null;
    this.since = now;
    this.lastActivity = now;
    this.running = false;
  }

  set(state, tool, now) {
    if (state !== this.state || tool !== this.tool) {
      this.state = state;
      this.tool = tool;
      this.since = now;
    }
    this.lastActivity = now;
  }

  // event takes a chat stream event ({type, ...}); every event counts as
  // activity (the stage wakes). Returns false for a non-event.
  event(ev, now) {
    if (!ev || !ev.type) return false;
    switch (ev.type) {
      case 'user':
      case 'start':
      case 'reasoning':
        this.running = true;
        this.set('thinking', null, now);
        break;
      case 'text':
        this.running = true;
        this.set('writing', null, now);
        break;
      case 'tool':
        this.running = true;
        this.set('tool', ev.canon || ev.name || 'tool', now);
        break;
      case 'tool_done':
        this.set('thinking', null, now);
        break;
      case 'done':
        this.running = false;
        if (ev.ok) this.set('done', null, now);
        else if (ev.stopped) this.set('idle', null, now);
        else this.set('error', null, now);
        break;
      case 'error':
        this.running = false;
        this.set('error', null, now);
        break;
      case 'state':
        if (ev.busy === false) this.running = false;
        this.lastActivity = now;
        break;
      default:
        this.lastActivity = now;
    }
    return true;
  }

  // wake marks activity (a poke, a menu change) without a state change.
  wake(now) { this.lastActivity = now; }

  // tick moves done/error back to idle after DONE_TO_IDLE_S. Returns the
  // state to draw.
  tick(now) {
    if ((this.state === 'done' || this.state === 'error') && now - this.since > DONE_TO_IDLE_S) {
      this.state = 'idle';
      this.tool = null;
      this.since = now;
    }
    return this.state;
  }

  // interval is the delay to the next frame in ms, or 0 to stop: hidden
  // tabs stop, a run or recent activity draws at 10/s, a quiet stage at
  // 2.5/s, and after SLEEP_S of nothing the stage sleeps.
  interval(now, hidden = false) {
    if (hidden) return 0;
    const quiet = now - this.lastActivity;
    if (this.running || quiet < QUIET_S) return FAST_MS;
    if (quiet < SLEEP_S) return SLOW_MS;
    // A done/error pose still has to fall back to idle.
    if (this.state === 'done' || this.state === 'error') return SLOW_MS;
    return 0;
  }
}

// ---------- stage: the grid ----------

export const ART_MIN = [48, 14];
export const ART_MAX = [160, 24];
export const ART_LIMIT = [480, 120];
export const ART_SCALE_MIN = 0.6;
export const ART_SCALE_MAX = 2.0;

// stageGrid is Atrium's on_stage_size: the grid (cols × rows) that fills a
// stage of availW × availH px, and the font scale to draw it at, given the
// cell size at scale 1. Below ART_MIN the font shrinks (to 0.6), with room
// for more than ART_MAX's rows it grows (to 2.0); either way the grid fills
// the stage, up to ART_LIMIT. Scales go in steps of 0.05.
export function stageGrid(availW, availH, cellW, cellH) {
  const w = Math.max(1, availW);
  const hgt = Math.max(1, availH);
  const cw = cellW > 0 ? cellW : 8;
  const lh = cellH > 0 ? cellH : 16;
  const fitC = w / cw;
  const fitR = hgt / lh;
  const [minC, minR] = ART_MIN;
  const [, maxR] = ART_MAX;
  let scale = Math.min(1, fitC / minC, fitR / minR);
  if (scale >= 1) scale = Math.max(1, Math.min(fitR / maxR, fitC / minC, ART_SCALE_MAX));
  scale = round2(Math.max(ART_SCALE_MIN, Math.floor(scale * 20 + 1e-9) / 20));
  // The linear estimate can overshoot by rounding: step down while ART_MIN
  // does not fit.
  while (scale > ART_SCALE_MIN && (minC * cw * scale > w || minR * lh * scale > hgt)) {
    scale = round2(Math.max(ART_SCALE_MIN, scale - 0.05));
  }
  const cols = Math.max(minC, Math.min(ART_LIMIT[0], Math.floor(w / (cw * scale) + 1e-9)));
  const rows = Math.max(minR, Math.min(ART_LIMIT[1], Math.floor(hgt / (lh * scale) + 1e-9)));
  return { cols, rows, scale };
}

const round2 = (x) => Math.round(x * 100) / 100;

// ---------- stage: spans → runs for the DOM ----------

// mergeRuns joins neighbouring runs of the same colours (theme spans are
// merged already; the emote overlay can split one). runs: [text, fg, bg?].
export function mergeRuns(runs) {
  const out = [];
  for (const r of runs || []) {
    const [text, fg = null, bg = null] = r;
    if (!text) continue;
    const last = out[out.length - 1];
    if (last && last[1] === fg && last[2] === bg) last[0] += text;
    else out.push([text, fg, bg]);
  }
  return out;
}

// ---------- images ----------

export const IMAGE_EDGE = 1568; // the longest side the models look at
export const IMAGE_BYTES = 3_500_000; // a PNG above this goes as JPEG
export const JPEG_QUALITY = 0.88;

// scaleDims fits w × h inside IMAGE_EDGE on its longest side, keeping the
// aspect ratio (never upscales).
export function scaleDims(w, h, edge = IMAGE_EDGE) {
  const W = Math.max(1, Math.round(w || 1));
  const H = Math.max(1, Math.round(h || 1));
  const long = Math.max(W, H);
  if (long <= edge) return { w: W, h: H, scaled: false };
  const k = edge / long;
  return { w: Math.max(1, Math.round(W * k)), h: Math.max(1, Math.round(H * k)), scaled: true };
}

// imageEncoding picks the format for an encoded PNG of pngBytes: the PNG,
// or JPEG q88 when it is over IMAGE_BYTES.
export function imageEncoding(pngBytes) {
  return pngBytes > IMAGE_BYTES ? { type: 'image/jpeg', quality: JPEG_QUALITY } : { type: 'image/png' };
}

// dataURLBytes is the decoded size of a base64 data: URL.
export function dataURLBytes(url) {
  const i = String(url || '').indexOf(',');
  if (i < 0) return 0;
  const b64 = url.slice(i + 1);
  const pad = b64.endsWith('==') ? 2 : b64.endsWith('=') ? 1 : 0;
  return Math.max(0, Math.floor((b64.length * 3) / 4) - pad);
}

export const MAX_IMAGES = 8;

// ---------- permission cards ----------

export const ARM_MS = 600; // buttons do nothing this long after the card shows
export const CONFIRM_MIN_MS = 400; // a run command's second press: not sooner
export const CONFIRM_MAX_MS = 5000; // and not later than this

// isCommandAsk: a card for running a command needs two presses to allow.
export function isCommandAsk(req) {
  const t = String((req && (req.tool || req.display)) || '');
  return t === 'Bash' || t === 'run_command' || t === 'BashOutput';
}

// permPress decides what a press on a card's button does. card: {shownAt,
// command, pending: {decision, at} | null}; decision: once | session |
// always | deny. Returns {action, card}: 'ignore' (not armed yet, or a
// second press too soon), 'confirm' (first press on Allow for a command:
// press again), or 'answer'. "Always" is picked from a menu, a second step
// already, so it never needs a second press.
export function permPress(card, decision, now) {
  const c = { ...card };
  if (now - c.shownAt < ARM_MS) return { action: 'ignore', card: c };
  if (decision === 'deny' || decision === 'always' || !c.command) return { action: 'answer', card: { ...c, pending: null } };
  const p = c.pending;
  if (p && p.decision === decision) {
    const dt = now - p.at;
    if (dt < CONFIRM_MIN_MS) return { action: 'ignore', card: c };
    if (dt <= CONFIRM_MAX_MS) return { action: 'answer', card: { ...c, pending: null } };
  }
  return { action: 'confirm', card: { ...c, pending: { decision, at: now } } };
}

// answeredLine is the one line an answered card collapses to.
export function answeredLine(ev, req) {
  const what = askHeadline(req) || (req && req.tool) || 'the tool';
  const ruleWord = ev.rule && /^your saved permission /.test(ev.rule.text || '') ? 'saved permission' : 'session rule';
  const by = ev.by && ev.by !== 'user' ? ` (${ev.by === 'rule' ? ruleWord : ev.by})` : '';
  if (ev.decision === 'deny') return `✗ denied · ${what}${ev.note ? ` · “${ev.note}”` : ''}${by}`;
  if (ev.decision === 'always') return `✓ always allowed (saved) · ${what}${ev.rule && ev.rule.text ? ` · ${ev.rule.text}` : ''}`;
  if (ev.decision === 'session') return `✓ allowed for this session · ${what}${ev.rule && ev.rule.text ? ` · ${ev.rule.text}` : ''}`;
  return `✓ allowed once · ${what}${by}`;
}

// askHeadline is the card's main line: the command, path, URL or query.
export function askHeadline(req) {
  if (!req) return '';
  return toolSummary(req.tool, req.input || {}) || req.display || '';
}

// askDetails are the extra lines a card shows: an edit's changed lines (at
// most 10), a write's size, a plan.
export function askDetails(req) {
  const inp = (req && req.input) || {};
  const out = [];
  if (typeof inp.old_string === 'string' || typeof inp.new_string === 'string') {
    const del = String(inp.old_string || '').split('\n').filter((l, i, a) => l || i < a.length - 1).map((l) => ['del', l]);
    const add = String(inp.new_string || '').split('\n').filter((l, i, a) => l || i < a.length - 1).map((l) => ['add', l]);
    const lines = [...del, ...add];
    out.push(...lines.slice(0, 10));
    if (lines.length > 10) out.push(['more', `… ${lines.length - 10} more lines`]);
  } else if (typeof inp.content === 'string') {
    const n = inp.content.split('\n').length;
    out.push(['info', `${n} line${n === 1 ? '' : 's'} · ${inp.content.length} chars`]);
  } else if (typeof inp.plan === 'string') {
    out.push(...inp.plan.split('\n').slice(0, 10).map((l) => ['info', l]));
  }
  return out;
}

// ---------- events → mood reactions ----------

// moodEvents maps a chat event to the Mood.react calls it makes: a list of
// [event, kw]. ctx keeps what later events need: {tools: Map(id → {tool,
// command}), reply: the reply text so far}.
export function moodEvents(ev, ctx) {
  if (!ev) return [];
  const tools = ctx.tools || (ctx.tools = new Map());
  switch (ev.type) {
    case 'user':
      ctx.reply = '';
      return [['prompt', { text: ev.text || '', images: ev.images || 0 }]];
    case 'text':
      ctx.reply = (ctx.reply || '') + (ev.text || '');
      return [];
    case 'tool': {
      const tool = ev.canon || ev.name || '';
      const command = (ev.input && (ev.input.command || ev.input.cmd)) || null;
      tools.set(ev.id, { tool, command });
      return [['tool_start', { tool }]];
    }
    case 'tool_done': {
      const t = tools.get(ev.id) || { tool: null, command: null };
      tools.delete(ev.id);
      return [[ev.is_error ? 'tool_error' : 'tool_ok', { tool: t.tool, command: t.command, output: ev.output || '' }]];
    }
    case 'ask':
      return [['permission', {}]];
    case 'answered':
      if (ev.by !== 'user') return [];
      return [[ev.decision === 'deny' ? 'permission_denied' : 'permission_allowed', {}]];
    case 'done': {
      const out = [];
      if (ctx.reply) out.push(['reply', { text: ctx.reply }]);
      ctx.reply = '';
      if (ev.stopped) out.push(['stopped', {}]);
      else if (ev.ok) out.push(['result_ok', { duration: typeof ev.seconds === 'number' ? ev.seconds : null }]);
      else out.push(['result_error', {}]);
      return out;
    }
    case 'error':
      return [['result_error', {}]];
    default:
      return [];
  }
}

// ---------- stored messages → transcript items ----------

// contentText reads an OpenAI message's content (a string or parts) into
// {text, images}; a stored image is the text part "[image]".
export function contentText(content) {
  if (typeof content === 'string') return { text: content, images: 0 };
  if (!Array.isArray(content)) return { text: '', images: 0 };
  let images = 0;
  const text = [];
  for (const p of content) {
    if (!p || typeof p !== 'object') continue;
    if (p.type === 'image_url' || p.type === 'image') images++;
    else if (p.type === 'text' && p.text === '[image]') images++;
    else if (p.type === 'text' && typeof p.text === 'string') text.push(p.text);
  }
  return { text: text.join('\n'), images };
}

function parseArgs(raw) {
  if (raw && typeof raw === 'object') return raw;
  try { const v = JSON.parse(raw || '{}'); return v && typeof v === 'object' ? v : {}; } catch { return {}; }
}

// CANON is the built-in agent's tool names as the live events show them
// (agent.Canon), so a reloaded transcript names tools the same way.
export const CANON = { run_command: 'Bash', write_file: 'Write', edit_file: 'Edit', read_file: 'Read', list_dir: 'LS', search: 'Grep' };

// A stored tool result that reports a failure (the engine's error, denial
// and refusal texts).
const FAILED_RESULT = /^(error|denied|refused|the user denied|blocked)\b/i;

// messagesToItems turns a chat's stored messages into transcript items:
// {kind: 'user', text, images}, {kind: 'assistant', text, reasoning},
// {kind: 'tool', id, name, input, output, isError, done}. System messages
// are skipped; a tool result joins its call.
export function messagesToItems(messages) {
  const items = [];
  const byId = new Map();
  for (const m of messages || []) {
    if (!m || m.role === 'system') continue;
    if (m.role === 'user') {
      const c = contentText(m.content);
      items.push({ kind: 'user', text: c.text, images: c.images });
    } else if (m.role === 'assistant') {
      const c = contentText(m.content);
      const reasoning = typeof m.reasoning_content === 'string' ? m.reasoning_content : (typeof m.reasoning === 'string' ? m.reasoning : '');
      if (c.text.trim() || reasoning) items.push({ kind: 'assistant', text: c.text, reasoning });
      for (const tc of m.tool_calls || []) {
        const fn = tc.function || {};
        const it = { kind: 'tool', id: tc.id || '', name: CANON[fn.name] || fn.name || 'tool', input: parseArgs(fn.arguments), output: '', isError: false, done: false };
        items.push(it);
        if (tc.id) byId.set(tc.id, it);
      }
    } else if (m.role === 'tool') {
      const it = byId.get(m.tool_call_id);
      const out = contentText(m.content).text;
      if (it) { it.output = out; it.done = true; it.isError = FAILED_RESULT.test(out.trim()); }
    }
  }
  for (const it of items) if (it.kind === 'tool') it.done = true;
  return items;
}

// beforeLastUser is messages up to (not including) the last user message:
// a busy chat's running message comes back from the event log instead.
export function beforeLastUser(messages) {
  const list = messages || [];
  for (let i = list.length - 1; i >= 0; i--) if (list[i] && list[i].role === 'user') return list.slice(0, i);
  return list.slice();
}

// replayFrom picks, from a busy chat's backlog of events (oldest first),
// where the running message starts: its last "user" event (0 when there is
// none in the log).
export function replayFrom(events) {
  for (let i = (events || []).length - 1; i >= 0; i--) if (events[i] && events[i].type === 'user') return i;
  return 0;
}

// streamVerdict checks a stream's first "hello" {epoch, base, next}
// against what the page holds: its epoch (the daemon's, "" before the
// first) and want, the next index it asked for. "reload" when the daemon
// restarted (another epoch, or a log behind the page) or the log no longer
// holds the asked index (a gap: events were trimmed while away).
export function streamVerdict(hello, { epoch = '', want = 0 } = {}) {
  if (!hello || typeof hello !== 'object') return 'ok';
  if (epoch && hello.epoch && hello.epoch !== epoch) return 'reload';
  if (typeof hello.next === 'number' && hello.next < want) return 'reload';
  if (typeof hello.base === 'number' && hello.base > want) return 'reload';
  return 'ok';
}

// replayPlan says how a chat view opens: a busy chat replays its running
// message from run_from when the log still holds it (replay: true, the
// stored messages before the last user one shown first); otherwise, idle
// or with the run's start trimmed away, every stored message is shown and
// the stream listens from next.
export function replayPlan(view) {
  const v = view || {};
  const busy = !!(v.status && v.status.busy);
  const next = v.next || 0;
  const base = v.base || 0;
  const runFrom = Number.isInteger(v.run_from) ? v.run_from : 0;
  if (busy && runFrom >= base && runFrom >= 0 && next > runFrom) return { replay: true, from: runFrom, until: next };
  return { replay: false, from: next, until: 0 };
}

// preRunQueue is the queued texts a replay does not show: the queue holds
// the messages waiting now, oldest first, and the replay shows only those
// queued after the running message started (its "queued" events). The
// rest were queued earlier, so they come first.
export function preRunQueue(queue, replayed) {
  const q = Array.isArray(queue) ? queue : [];
  const shown = (replayed || []).filter((e) => e && e.type === 'queued').length;
  return q.slice(0, Math.max(0, q.length - shown));
}

// ---------- lines ----------

export const MODES = [
  ['ask', 'Ask me', 'Every tool that changes something asks first'],
  ['auto', 'Auto', 'Reads and safe commands run; the rest asks'],
  ['acceptEdits', 'Accept edits', 'File edits run; commands still ask'],
  ['plan', 'Plan', 'Read-only tools; the model writes a plan'],
];

export function modeLabel(mode) {
  const m = MODES.find(([k]) => k === mode);
  return m ? m[1] : (mode || 'Ask me');
}

// fmtTokensShort: 950, 12.3k, 128k, 1.2M.
export function fmtTokensShort(n) {
  const v = Number(n) || 0;
  if (v < 1000) return String(Math.round(v));
  if (v < 100_000) return `${(v / 1000).toFixed(1).replace(/\.0$/, '')}k`;
  if (v < 1_000_000) return `${Math.round(v / 1000)}k`;
  return `${(v / 1_000_000).toFixed(1).replace(/\.0$/, '')}M`;
}

export function fmtSeconds(s) {
  const v = Number(s) || 0;
  if (v < 60) return `${v.toFixed(1)} s`;
  const m = Math.floor(v / 60);
  const sec = Math.round(v % 60);
  return m < 60 ? `${m} min ${sec} s` : `${Math.floor(m / 60)} h ${m % 60} min`;
}

export function fmtCost(usd) {
  const v = Number(usd) || 0;
  if (v <= 0) return '';
  return v < 0.01 ? `$${v.toFixed(4)}` : `$${v.toFixed(2)}`;
}

// statusParts is the status line under the input: model (and the next
// message's model when one is picked), context used / total (%), elapsed,
// cost, mode, queue and waiting cards.
export function statusParts(st, { nextModel = '', nextMode = '', ctxOf = null } = {}) {
  const s = st || {};
  const parts = [];
  if (s.waiting) parts.push(`🔒 ${s.waiting} waiting`);
  let model = s.model || '';
  if (nextModel && nextModel !== model) model = model ? `${model} → ${nextModel}` : nextModel;
  if (model) parts.push(model);
  const total = s.context || (ctxOf ? ctxOf(nextModel || s.model) : 0) || 0;
  if (s.used || total) {
    const pct = total ? ` (${Math.min(999, Math.round((100 * (s.used || 0)) / total))}%)` : '';
    parts.push(`ctx ${fmtTokensShort(s.used || 0)}${total ? ' / ' + fmtTokensShort(total) : ''}${pct}`);
  }
  if (s.elapsed) parts.push(`⏱ ${fmtSeconds(s.elapsed)}`);
  const cost = fmtCost(s.cost);
  if (cost) parts.push(cost);
  let mode = modeLabel(s.mode);
  if (nextMode && nextMode !== s.mode) mode = `${s.mode ? modeLabel(s.mode) + ' → ' : ''}${modeLabel(nextMode)}`;
  parts.push(mode);
  if (s.queue) parts.push(`${s.queue} queued`);
  return parts;
}

// endLine is the line a run ends with: "✓ done · 3 steps · 4.2 s · model".
export function endLine(done, model = '') {
  const d = done || {};
  if (d.stopped) return { tone: 'muted', text: `■ stopped${d.seconds ? ' · ' + fmtSeconds(d.seconds) : ''}` };
  if (!d.ok) return { tone: 'err', text: `✗ failed${d.error ? ': ' + d.error : ''}` };
  const parts = ['✓ done'];
  if (d.steps) parts.push(`${d.steps} step${d.steps === 1 ? '' : 's'}`);
  if (typeof d.seconds === 'number') parts.push(fmtSeconds(d.seconds));
  if (model) parts.push(model);
  const cost = fmtCost(d.cost);
  if (cost) parts.push(cost);
  return { tone: 'ok', text: parts.join(' · ') };
}

// thinkingLabel is the thinking fold's summary: "thinking · N lines".
export function thinkingLabel(text) {
  const n = String(text || '').trim() ? String(text).trim().split('\n').length : 0;
  return `thinking · ${n} line${n === 1 ? '' : 's'}`;
}

export const THINKING_KEEP = 20000;

// keepTail keeps the last n characters of s.
export const keepTail = (s, n = THINKING_KEEP) => (s.length > n ? s.slice(s.length - n) : s);

// shortPath shows a path under the home folder as ~/….
export function shortPath(p, home = '') {
  const s = String(p || '');
  if (home && (s === home || s.startsWith(home.endsWith('/') ? home : home + '/'))) return '~' + s.slice(home.replace(/\/$/, '').length);
  return s;
}

const clip = (s, n = 160) => { const t = String(s ?? '').replace(/\s+/g, ' ').trim(); return t.length > n ? t.slice(0, n - 1) + '…' : t; };

// toolSummary is the one line after a tool's name: what it works on
// (Atrium's tool_summary). name may be a native name or the canonical one.
export function toolSummary(name, data, home = '') {
  if (!data || typeof data !== 'object') return '';
  const path = () => shortPath(data.file_path || data.path || data.notebook_path || '', home);
  switch (name) {
    case 'Bash': case 'run_command': return clip(data.command || '');
    case 'Read': case 'Edit': case 'Write': case 'MultiEdit': case 'NotebookEdit': case 'LS':
    case 'read_file': case 'write_file': case 'edit_file': case 'list_dir':
      return clip(path());
    case 'Grep': case 'search': {
      const where = data.path ? shortPath(data.path, home) : '';
      let what = data.pattern ? `/${data.pattern}/` : '';
      if (data.glob) what += (what ? '  ' : '') + data.glob;
      return clip(what + (where ? `  ${where}` : ''));
    }
    case 'Glob': return clip((data.pattern || '') + (data.path ? `  ${shortPath(data.path, home)}` : ''));
    case 'WebSearch': return clip(data.query || '');
    case 'WebFetch': return clip(data.url || '');
    case 'Task': case 'Agent': return clip(data.description || data.subagent_type || '');
    case 'TodoWrite': {
      const todos = Array.isArray(data.todos) ? data.todos : [];
      return `${todos.filter((t) => t && t.status === 'completed').length}/${todos.length} todos`;
    }
    case 'Skill': return clip(data.skill || '');
    default:
      for (const v of Object.values(data)) if (typeof v === 'string' && v) return clip(v);
      return '';
  }
}

// excerpt is the first lines of a tool's output for the chip (14 lines).
export function excerpt(out, lines = 14, chars = 4000) {
  const s = String(out || '');
  const ls = s.split('\n');
  let t = ls.slice(0, lines).join('\n');
  if (t.length > chars) t = t.slice(0, chars);
  const more = ls.length > lines || s.length > t.length;
  return { text: t, more };
}

export const SPINNER = ['◐', '◓', '◑', '◒'];

// ---------- models ----------

const BACKEND_NAMES = { llamacpp: 'llama.cpp', lmstudio: 'LM Studio', ollama: 'Ollama', openai: 'API' };

// modelGroups sorts the picker's data (GET /api/chats/models) into
// sections: Local · <backend>, API · <provider>, Claude · Claude Code.
// Each item: {id, label, loaded, isDefault, local, ctx}.
export function modelGroups(data) {
  const d = data || {};
  const groups = new Map();
  const add = (key, label, item) => {
    if (!groups.has(key)) groups.set(key, { key, label, items: [] });
    groups.get(key).items.push(item);
  };
  for (const m of d.models || []) {
    const id = m.ID || m.id;
    if (!id) continue;
    const backend = m.Backend || m.backend || '';
    const local = Boolean(m.Local ?? m.local);
    const item = { id, label: id, loaded: Boolean(m.Loaded ?? m.loaded), isDefault: id === d.default, local, ctx: m.Ctx || m.ctx || 0 };
    if (local) add(`local:${backend}`, `Local · ${BACKEND_NAMES[backend] || backend || 'models'}`, item);
    else {
      const prov = id.includes('/') ? id.split('/')[0] : (BACKEND_NAMES[backend] || backend || 'server');
      add(`api:${prov}`, `API · ${prov}`, item);
    }
  }
  const out = [...groups.values()];
  out.sort((a, b) => (a.key.startsWith('local') ? 0 : 1) - (b.key.startsWith('local') ? 0 : 1) || a.label.localeCompare(b.label));
  for (const g of out) g.items.sort((a, b) => (b.loaded - a.loaded) || a.id.localeCompare(b.id));
  const cl = d.claude || {};
  if (cl.available && Array.isArray(cl.models) && cl.models.length) {
    out.push({ key: 'claude', label: 'Claude · Claude Code', items: cl.models.map((id) => ({ id, label: id === 'claude' ? 'claude (its default)' : id, loaded: false, isDefault: id === d.default, local: false, ctx: 0 })) });
  }
  return out;
}

// modelProblem reads a failed run's error: is the model the problem (not
// loaded, unknown, no default)? null when it is something else. The chat
// then shows a card with fixes instead of a bare error line.
//   {kind: 'no-default'}            "default" names no model (engines.default_model unset)
//   {kind: 'not-loaded'}            the engine has no model in memory
//   {kind: 'unknown', model}        the gateway knows no such model
export function modelProblem(text) {
  const t = String(text || '');
  if (/unknown model: default\b|set engines\.default_model/i.test(t)) return { kind: 'no-default' };
  if (/no models? (are |is )?loaded|\bnot loaded\b|model\b.*\bnot found|model_not_found/i.test(t)) return { kind: 'not-loaded' };
  const m = t.match(/unknown model: ([^\s(]+)/i);
  if (m) return { kind: 'unknown', model: m[1] };
  return null;
}

// needsModelPick says whether a message sent now on model would fail for want
// of a model: the chat is on "default", no default model is set, and the
// gateway cannot guess one (it can when exactly one model is loaded, or there
// is exactly one model).
export function needsModelPick(data, model) {
  const d = data || {};
  if (model && model !== 'default') return false;
  if (d.default && d.default !== 'default') return false;
  const ms = Array.isArray(d.models) ? d.models : [];
  if (ms.length === 1) return false;
  const loaded = ms.filter((m) => Boolean(m.Local ?? m.local) && Boolean(m.Loaded ?? m.loaded)).length;
  return loaded !== 1;
}

// isClaudeModel mirrors chat.BackendOf.
export function isClaudeModel(id) {
  const low = String(id || '').trim().toLowerCase();
  return low === 'claude' || low.startsWith('claude:');
}

// switchesBackend: picking model starts a new conversation (Atrium's rule).
export function switchesBackend(current, next, hasMessages) {
  if (!next || !hasMessages) return false;
  return isClaudeModel(current) !== isClaudeModel(next);
}

// ---------- input history ----------

// historyStep moves through sent messages with Up (-1) and Down (+1). idx
// is the current position (history.length = the draft). Returns {idx,
// value}.
export function historyStep(history, idx, dir, draft) {
  const n = history.length;
  let i = Math.max(0, Math.min(n, idx + dir));
  if (n === 0) i = 0;
  return { idx: i, value: i === n ? draft : history[i] };
}

export function pushHistory(history, text, max = 100) {
  const t = String(text || '').trim();
  if (!t) return history;
  const out = history.filter((h) => h !== t);
  out.push(t);
  return out.slice(-max);
}

// ---------- find ----------

// findAll returns the start offsets of q in text (case-insensitive).
export function findAll(text, q) {
  const needle = String(q || '').toLowerCase();
  if (!needle) return [];
  const hay = String(text || '').toLowerCase();
  const out = [];
  let k = hay.indexOf(needle);
  while (k >= 0) { out.push(k); k = hay.indexOf(needle, k + needle.length); }
  return out;
}

// findStep moves the current match by dir with wrap-around; -1 when none.
export function findStep(i, n, dir) {
  if (n <= 0) return -1;
  if (i < 0) return dir < 0 ? n - 1 : 0;
  return (((i + dir) % n) + n) % n;
}

export const findLabel = (i, n) => (n ? `${i + 1} / ${n}` : '0 / 0');

// ---------- corner card ----------

export const CORNERS = ['top-right', 'bottom-right', 'bottom-left', 'top-left'];

export function nextCorner(c) {
  const i = CORNERS.indexOf(c);
  return CORNERS[(i + 1) % CORNERS.length];
}

// stagePrefs normalises the remembered stage state.
export function stagePrefs(raw) {
  const r = raw && typeof raw === 'object' ? raw : {};
  return { corner: Boolean(r.corner), where: CORNERS.includes(r.where) ? r.where : 'top-right' };
}

// ---------- quick answer: SSE over a fetch body ----------

// parseSSE splits buf into complete events and the unfinished rest.
// Events: {event, data, id}.
export function parseSSE(buf) {
  const events = [];
  const text = String(buf || '').replace(/\r\n?/g, '\n');
  const blocks = text.split('\n\n');
  const rest = blocks.pop();
  for (const b of blocks) {
    const ev = { event: 'message', data: '', id: '' };
    const data = [];
    let any = false;
    for (const line of b.split('\n')) {
      if (!line || line.startsWith(':')) continue;
      const k = line.indexOf(':');
      const field = k < 0 ? line : line.slice(0, k);
      let value = k < 0 ? '' : line.slice(k + 1);
      if (value.startsWith(' ')) value = value.slice(1);
      if (field === 'event') { ev.event = value; any = true; } else if (field === 'data') { data.push(value); any = true; } else if (field === 'id') ev.id = value;
    }
    if (!any) continue;
    ev.data = data.join('\n');
    events.push(ev);
  }
  return { events, rest };
}

// ---------- recent chats ----------

// agoText: "just now", "5 min ago", "3 h ago", "2 d ago" for Unix seconds.
export function agoText(unixSec, now = Date.now()) {
  const s = Math.max(0, now / 1000 - (Number(unixSec) || 0));
  if (s < 60) return 'just now';
  if (s < 3600) return `${Math.floor(s / 60)} min ago`;
  if (s < 86400) return `${Math.floor(s / 3600)} h ago`;
  return `${Math.floor(s / 86400)} d ago`;
}

export const DELETE_WINDOW_MS = 3000;

// deletePress: the first press on 🗑 arms it, a second within 3 s deletes.
export function deletePress(armedAt, now) {
  if (armedAt && now - armedAt <= DELETE_WINDOW_MS) return { delete: true, armedAt: 0 };
  return { delete: false, armedAt: now };
}

// chatIdOk mirrors chat.IsID.
export const chatIdOk = (id) => /^chat-\d{8}-\d{6}-[0-9a-f]{6}$/.test(String(id || ''));
