// What every theme's mascot shares: the cell grid, the noise, the tool -> prop
// mapping, blinking, the reaction sign over the head, and the glue that turns a
// theme's Mascot class into the module API (makeTheme). A faithful port of
// Atrium's mascot_common.py (and of mascots.calm_tick / emote_markup): the same
// (state, tool, tick, cols, rows, mood, scene) gives the same frame text, and the
// same colour runs as its Pango markup. Pure: no DOM, so node --test imports it.
//
// Python arithmetic is kept on purpose: floor division and modulo round towards
// minus infinity (fd, mod, fmod), int() truncates (Math.trunc), and the LCG is
// exact 32-bit maths, so a petal falls on the same cell in both languages.

import { WIDE, COMBINING } from './eaw.js';

export const STATES = ['idle', 'thinking', 'tool', 'writing', 'done', 'error'];
export const TOOLS = ['Bash', 'Read', 'Edit', 'Write', 'Grep', 'Glob', 'WebSearch', 'WebFetch',
  'Task', 'Agent', 'TodoWrite', 'mcp__x__y', 'Unknown', null];
export const COMBOS = [
  ...STATES.filter((s) => s !== 'tool').map((s) => [s, null]),
  ...TOOLS.map((t) => ['tool', t]),
];

// ---------- Python arithmetic ----------

// fd is Python's a // b for ints and floats alike (CPython's float_floor_div:
// exact even where a / b would round up to the next integer).
export function fd(a, b) {
  if (Number.isInteger(a) && Number.isInteger(b)) return Math.floor(a / b);
  let m = a % b;
  let div = (a - m) / b;
  if (m && (b < 0) !== (m < 0)) div -= 1;
  if (!div) return 0;
  let f = Math.floor(div);
  if (div - f > 0.5) f += 1;
  return f;
}

// mod is Python's a % b for ints (the sign of b).
export function mod(a, b) {
  const m = a % b;
  return m && (m < 0) !== (b < 0) ? m + b : m;
}

// fmod is Python's a % b for floats (same rule; JS % is the exact C fmod).
export const fmod = mod;

// int is Python's int(x) for a float: towards zero.
export const int = Math.trunc;

// ---------- noise ----------

// lcg is a 32-bit LCG folded over the seed integers; returns a draw function
// yielding floats in [0, 1). Same seed, same sequence, always (and the same as
// mascot_common.lcg).
export function lcg(...seed) {
  let s = 2166136261;
  for (const v of seed) s = Math.imul((s ^ (Math.trunc(v) >>> 0)) >>> 0, 16777619) >>> 0;
  let state = s || 1;
  const draw = () => {
    state = (Math.imul(state, 1664525) + 1013904223) >>> 0;
    return (state >>> 8) / 16777216;
  };
  draw();
  return draw;
}

// ---------- widths ----------

function inRanges(table, cp) {
  let lo = 0;
  let hi = table.length / 2 - 1;
  while (lo <= hi) {
    const mid = (lo + hi) >> 1;
    if (cp < table[mid * 2]) hi = mid - 1;
    else if (cp > table[mid * 2 + 1]) lo = mid + 1;
    else return true;
  }
  return false;
}

// dw is the display width of one character in a CJK monospace font
// (ambiguous counts as 2, combining marks as 0).
export function dw(ch) {
  const cp = ch.codePointAt(0);
  if (cp >= 0x20 && cp < 0x7f) return 1;
  if (inRanges(COMBINING, cp)) return 0;
  return inRanges(WIDE, cp) ? 2 : 1;
}

export function displayWidth(s) {
  let n = 0;
  for (const c of s) n += dw(c);
  return n;
}

// ---------- tools ----------

// propKind maps a tool name to the prop the mascot holds: bash book pad lens
// globe helpers todo wand.
export function propKind(tool) {
  const t = (tool || '').trim();
  if (t.startsWith('mcp__')) return 'wand';
  if (t === 'Bash') return 'bash';
  if (t === 'Read' || t === 'NotebookRead') return 'book';
  if (['Edit', 'Write', 'MultiEdit', 'NotebookEdit'].includes(t)) return 'pad';
  if (['Grep', 'Glob', 'WebSearch', 'ToolSearch'].includes(t)) return 'lens';
  if (t === 'WebFetch') return 'globe';
  if (t === 'Task' || t === 'Agent') return 'helpers';
  if (t === 'TodoWrite') return 'todo';
  return 'wand';
}

// ---------- the grid ----------

// Canvas is cols x rows cells, each a single-column character, a colour (null
// for a blank cell) and a background (null: the panel shows through).
// Out-of-range writes are clipped silently. Strings are walked by code point,
// as Python walks them.
export class Canvas {
  constructor(cols, rows) {
    this.cols = cols;
    this.rows = rows;
    this.ch = Array.from({ length: rows }, () => new Array(cols).fill(' '));
    this.fg = Array.from({ length: rows }, () => new Array(cols).fill(null));
    this.bg = Array.from({ length: rows }, () => new Array(cols).fill(null));
    this.occ = Array.from({ length: rows }, () => new Array(cols).fill(false));
  }

  cell(x, y, c, colour, bg = null) {
    if (x >= 0 && x < this.cols && y >= 0 && y < this.rows) {
      if (dw(c) !== 1) c = '?';
      this.ch[y][x] = c;
      this.fg[y][x] = c !== ' ' ? (colour ?? null) : null;
      if (bg != null) this.bg[y][x] = bg;
    }
  }

  get(x, y) {
    if (x >= 0 && x < this.cols && y >= 0 && y < this.rows) return this.ch[y][x];
    return null;
  }

  // put writes s at (x, y). Spaces are transparent unless solid (or bg).
  put(x, y, s, colour, solid = false, bg = null) {
    let i = 0;
    for (const c of s) {
      if (!(c === ' ' && !solid && bg == null)) this.cell(x + i, y, c, colour, bg);
      i++;
    }
  }

  // sprite writes s with its outer spaces transparent and inner ones opaque:
  // a sprite row that hides what is behind it between its edges.
  sprite(x, y, s, colour, bg = null) {
    const cs = Array.from(s);
    let a = 0;
    while (a < cs.length && cs[a] === ' ') a++;
    let b = cs.length;
    while (b > 0 && cs[b - 1] === ' ') b--;
    if (a >= b) return;
    this.put(x + a, y, cs.slice(a, b).join(''), colour, true, bg);
    if (y >= 0 && y < this.rows) {
      for (let c = Math.max(0, x + a); c < Math.min(this.cols, x + b); c++) this.occ[y][c] = true;
    }
  }

  // fill paints a solid block of background (blank cells unless ch).
  fill(x, y, w, h, bg, ch = ' ', colour = null, occupy = true) {
    for (let yy = y; yy < y + h; yy++) {
      for (let xx = x; xx < x + w; xx++) {
        if (xx >= 0 && xx < this.cols && yy >= 0 && yy < this.rows) {
          this.ch[yy][xx] = ch;
          this.fg[yy][xx] = ch !== ' ' ? colour : null;
          this.bg[yy][xx] = bg;
          if (occupy) this.occ[yy][xx] = true;
        }
      }
    }
  }

  recolour(x, y, colour = null, bg = null) {
    if (x >= 0 && x < this.cols && y >= 0 && y < this.rows) {
      if (colour != null && this.ch[y][x] !== ' ') this.fg[y][x] = colour;
      if (bg != null) this.bg[y][x] = bg;
    }
  }

  // shiftRow rotates row y by dx cells (a glitch); width never changes.
  shiftRow(y, dx) {
    if (!(y >= 0 && y < this.rows) || !dx) return;
    dx = mod(dx, this.cols);
    for (const grid of [this.ch, this.fg, this.bg, this.occ]) {
      const row = grid[y];
      grid[y] = row.slice(this.cols - dx).concat(row.slice(0, this.cols - dx));
    }
  }

  lines() {
    return this.ch.map((r) => r.join(''));
  }

  // spans is markup() without the markup: one array per row of runs
  // [text, colour] -- or [text, colour, background] when a background is set
  // -- merged exactly as mascot_common.Canvas.markup merges them (a blank cell
  // joins any run with its own background). colour is null for a run with
  // nothing to colour.
  spans() {
    const out = [];
    for (let y = 0; y < this.rows; y++) {
      const chars = this.ch[y];
      const cols = this.fg[y];
      const bgs = this.bg[y];
      const parts = [];
      let run = [];
      let runCol = null;
      let runBg = null;
      const push = () => parts.push(runBg == null ? [run.join(''), runCol] : [run.join(''), runCol, runBg]);
      for (let x = 0; x < this.cols; x++) {
        const c = chars[x];
        const col = cols[x];
        const bg = bgs[x];
        if ((c === ' ' && bg === runBg) || (col === runCol && bg === runBg)) {
          run.push(c);
          continue;
        }
        if (run.length) push();
        run = [c];
        runCol = c !== ' ' ? col : null;
        runBg = bg;
      }
      if (run.length) push();
      out.push(parts);
    }
    return out;
  }
}

// streamLines draws words from `words`, wrapped to `width`: the text a mascot
// streams onto its scroll / screen while the model writes.
export function streamLines(words, width, seed = 31, count = 400) {
  const r = lcg(seed);
  const textLines = [];
  let cur = '';
  for (let i = 0; i < count; i++) {
    const w = words[int(r() * words.length)];
    if (cur.length + w.length + (cur ? 1 : 0) > width) {
      textLines.push(cur);
      cur = w;
    } else {
      cur = cur ? `${cur} ${w}` : w;
    }
  }
  textLines.push(cur);
  return textLines.map((l) => l.padEnd(width));
}

// blink is true on the frames the eyes are shut: every few seconds, and a
// double blink now and then.
export function blink(tick, period = 53) {
  const a = mod(tick, period);
  const b = mod(tick, period * 3);
  return a === 0 || a === 1 || b === 5 || b === 6;
}

// pyfmt is Python's "..." % arg for one %s.
export function pyfmt(fmt, arg) {
  let done = false;
  return fmt.replace(/%[s%]/g, (m) => {
    if (m === '%%') return '%';
    if (done) return m;
    done = true;
    return String(arg);
  });
}

// sceneSet takes a scene as a Set, an array or "tag,tag" and returns a Set.
export function sceneSet(scene) {
  if (!scene) return new Set();
  if (scene instanceof Set) return scene;
  if (typeof scene === 'string') return new Set(scene.split(',').filter(Boolean));
  return new Set(scene);
}

// ---------- the reaction sign ----------

// Fallback colours for a mascot without EMOTE_COLOURS of its own.
export const EMOTE_FALLBACK = {
  accent: '#7fd4ff', warn: '#f2c14e', bad: '#ff6b6b', good: '#6ee08a', love: '#ff8fb8', dim: '#9aa3b2',
};

// drawEmote draws a View emote -- [text, tone, age in seconds] -- with its
// first character at anchor [x, y], the cell just above the right side of the
// mascot's head. It pops in (a dot for a quarter second), floats up one row
// after 1.5 s and blinks out in its last second. steady (reduce motion): the
// whole sign at the anchor from first frame to last.
export function drawEmote(cv, emote, anchor, colours = null, steady = false) {
  if (!emote || !anchor) return;
  let text = emote[0];
  const tone = emote[1];
  const age = Number(emote[2]);
  const colour = (colours || {})[tone] || EMOTE_FALLBACK[tone] || EMOTE_FALLBACK.accent;
  if (!steady) {
    if (age < 0.25) text = '.';
    else if (age > 7.5 && int(age * 4) % 2) return;
  }
  let [x, y] = anchor;
  if (age >= 1.5 && !steady) y -= 1;
  x = Math.max(0, Math.min(x, cv.cols - Array.from(text).length));
  y = Math.max(0, Math.min(y, cv.rows - 1));
  cv.put(x, y, text, colour);
}

// Reduce motion slows the whole stage to a third of its speed (mascots.py).
export const CALM_SLOW = 3;

export function calmTick(tick, calm = true) {
  return calm ? Math.floor(Math.trunc(tick) / CALM_SLOW) : Math.trunc(tick);
}

// ---------- the module API ----------

// BaseMascot is the public half every non-moe mascot shares (a port of
// mascot_common.BaseMascot): bubble() from its BUBBLES / TOOL_BUBBLES tables
// (OTHER takes the tool's name: an MCP tool's last part, or the raw name), and
// lines / spans over its render(). A subclass sets the static tables and
// implements render(state, tick, tool) returning a Canvas. mood and scene are
// taken and ignored unless the subclass sets moods = true (and draws them).
export class BaseMascot {
  constructor(cols, rows) {
    this.cols = cols;
    this.rows = rows;
    this.head = null;
  }

  render() {
    throw new Error('render not implemented');
  }

  bubble(state, tool = null) {
    const k = this.constructor;
    const B = k.BUBBLES || {};
    const T = k.TOOL_BUBBLES || {};
    const other = k.OTHER || '%s';
    if (state === 'tool') {
      if (tool && tool.startsWith('mcp__')) return pyfmt(other, tool.split('__').at(-1));
      return Object.hasOwn(T, tool || '') ? T[tool || ''] : pyfmt(other, tool || 'tool');
    }
    return B[state] ?? B.idle ?? '...';
  }
}
BaseMascot.moods = false;

// makeTheme turns a Mascot class into the module API every theme exports:
//
//   meta                                      {id, name, panel, moods}
//   lines(state, tick, tool, mood, scene, cols, rows)  -> rows strings
//   spans(...same)                            -> rows of [text, colour(, bg)] runs
//   render(...same)                           -> {canvas, head} (head: the
//                                                emote anchor [x, y] or null)
//   bubble(state, tool, mood)                 -> the speech line
//   say(topic)                                -> a mood topic in its voice
//   emoteColours                              -> tone -> colour
//
// One mascot instance is kept per size (each precomputes its stars, grass and
// stream text for its size), the last few sizes only.
export function makeTheme({ meta, Mascot, say, emoteColours = null }) {
  const cache = new Map();
  const get = (cols, rows) => {
    const key = `${cols}x${rows}`;
    let m = cache.get(key);
    if (!m) {
      m = new Mascot(cols, rows);
      cache.set(key, m);
      if (cache.size > 4) cache.delete(cache.keys().next().value);
    }
    return m;
  };
  const render = (state, tick, tool = null, mood = null, scene = null, cols = 64, rows = 18) => {
    const m = get(Math.trunc(cols), Math.trunc(rows));
    const canvas = m.render(state, tick, tool || null, mood || null, sceneSet(scene));
    return { canvas, head: m.head || null };
  };
  return {
    meta: { moods: Boolean(Mascot.moods), ...meta },
    emoteColours: emoteColours || EMOTE_FALLBACK,
    render,
    lines: (...a) => render(...a).canvas.lines(),
    spans: (...a) => render(...a).canvas.spans(),
    bubble: (state, tool = null, mood = null) => get(64, 18).bubble(state, tool || null, mood || null),
    say,
  };
}

// frameSpans is mascots.emote_markup for the web: a theme module's spans with
// a reaction's emote over the head (mascots that draw moods only), and calm
// (reduce motion) drawing the stage at calmTick and the emote steady.
//   opts: {state, tick, tool, mood, scene, cols, rows, emote, calm}
export function frameSpans(theme, opts) {
  const tick = calmTick(opts.tick || 0, Boolean(opts.calm));
  const args = [opts.state, tick, opts.tool, opts.mood, opts.scene, opts.cols, opts.rows];
  const drawn = theme.render(...args);
  if (!drawn) return theme.spans(...args);
  const { canvas, head } = drawn;
  if (opts.emote && theme.meta.moods && head) {
    drawEmote(canvas, opts.emote, head, theme.emoteColours, Boolean(opts.calm));
  }
  return canvas.spans();
}
