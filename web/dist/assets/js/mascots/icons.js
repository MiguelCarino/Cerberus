// The mascot, small enough for a notification or a PWA icon: one 16 x 16 pixel
// sprite per mascot theme (the ASCII stages are 48+ columns wide, unreadable at
// 48 px), so a theme whose mascot does not shrink gets a little cousin instead:
// Orbit's probe is a sputnik. A port of Atrium's mascot_icons.py: the same
// sprites cell for cell (node test: web/test/mascots-themes-b.test.mjs against
// the Python's tables), drawn the same way.
//
//   pixels(theme, mood)            GRID x GRID cells, each a "#rrggbb" or null
//   drawIcon(ctx, theme, mood, s)  fills a 2D context, s px per cell
//   iconDataURL(theme, mood, opts) a PNG data URL (cached), or null without a
//                                  canvas (opts.canvas: a canvas to draw on)
//
// MOODS put a sign over the top-right corner, the way the chat stage puts one
// over the mascot's head: calm none, alert a red "!", relieved a green tick.
// The sprite sits at the bottom left of an 18 x 18 grid, the sign's corner free
// above it; each cell is SCALE px with no smoothing (108 px), crisp when a
// notification shrinks it to 48. Atrium's badge (the notifying app's own icon
// in the corner) is a desktop icon-theme lookup and is not ported.
//
// Pure apart from iconDataURL (which needs a DOM canvas): node --test imports it.

export const GRID = 18;
export const SCALE = 6;
export const MOODS = ['calm', 'alert', 'relieved'];
export const DEFAULT = 'moe';

// ---------- building sprites ----------
// Rows are built from runs so every row is provably 16 cells: "." is clear,
// every other letter a colour of that sprite's palette.

function rows(...runs) {
  const out = runs.map((r) => r.join(''));
  const bad = out.map((r, i) => (r.length !== 16 ? i : -1)).filter((i) => i >= 0);
  if (out.length !== 16 || bad.length) throw new Error(`sprite rows ${bad} are not 16 cells`);
  return out;
}

const blank = () => Array.from({ length: 16 }, () => new Array(16).fill('.'));
const rep = (s, n) => s.repeat(n);

// disc is a filled circle on a 16 x 16 grid: fill(x, y) picks each inner cell.
function disc(cx, cy, r, outline, fill, grid = null) {
  const g = grid ? grid.map((row) => [...row]) : blank();
  for (let y = 0; y < 16; y++) {
    for (let x = 0; x < 16; x++) {
      const d = ((x + 0.5 - cx) ** 2 + (y + 0.5 - cy) ** 2) ** 0.5;
      if (d <= r - 1.0) g[y][x] = fill(x, y);
      else if (d <= r) g[y][x] = outline;
    }
  }
  return g;
}

// line draws a Bresenham line of c from (x0, y0) to (x1, y1), clipped.
function line(grid, x0, y0, x1, y1, c) {
  const dx = Math.abs(x1 - x0);
  const dy = -Math.abs(y1 - y0);
  const sx = x0 < x1 ? 1 : -1;
  const sy = y0 < y1 ? 1 : -1;
  let err = dx + dy;
  for (;;) {
    if (x0 >= 0 && x0 < 16 && y0 >= 0 && y0 < 16) grid[y0][x0] = c;
    if (x0 === x1 && y0 === y1) return;
    const e2 = 2 * err;
    if (e2 >= dy) {
      err += dy;
      x0 += sx;
    }
    if (e2 <= dx) {
      err += dx;
      y0 += sy;
    }
  }
}

function put(grid, cells, c) {
  for (const [x, y] of cells) grid[y][x] = c;
  return grid;
}

// outline rings every cell of the letters in inside with c (4 neighbours), so
// a thin shape stays visible on a light notification too.
function outline(grid, inside, c) {
  const ring = [];
  for (let y = 0; y < 16; y++) {
    for (let x = 0; x < 16; x++) {
      if (!inside.includes(grid[y][x])) continue;
      for (const [dx, dy] of [[1, 0], [-1, 0], [0, 1], [0, -1]]) ring.push([x + dx, y + dy]);
    }
  }
  for (const [x, y] of ring) {
    if (x >= 0 && x < 16 && y >= 0 && y < 16 && grid[y][x] === '.') grid[y][x] = c;
  }
  return grid;
}

const join = (g) => g.map((r) => r.join(''));

// ---------- the sprites ----------

function moe() {
  // the chibi cat-eared girl: pink hair, big eyes, a sailor collar
  return [{ o: '#2a1630', h: '#f4a6c8', s: '#ffe0cc', w: '#ffffff', k: '#3a1a40', b: '#ff8fab', m: '#c0405a', c: '#5a7bd8' }, rows(
    ['..', 'o', rep('.', 10), 'o', '..'],
    ['.', 'oho', rep('.', 8), 'oho', '.'],
    ['.', 'ohho', rep('.', 6), 'ohho', '.'],
    ['.', 'ohhh', rep('o', 6), 'hhho', '.'],
    ['o', rep('h', 14), 'o'],
    ['o', rep('h', 14), 'o'],
    ['o', 'hhhh', 's', 'hhhh', 's', 'hhhh', 'o'],
    ['o', 'hh', rep('s', 10), 'hh', 'o'],
    ['o', 'hs', 's', 'wk', rep('s', 4), 'wk', 's', 'sh', 'o'],
    ['o', 'hs', 's', 'kk', rep('s', 4), 'kk', 's', 'sh', 'o'],
    ['o', 'hs', 'b', rep('s', 8), 'b', 'sh', 'o'],
    ['.', 'o', 'hs', 'sss', 'mm', 'sss', 'sh', 'o', '.'],
    ['.', 'o', 'h', rep('s', 10), 'h', 'o', '.'],
    ['..', 'o', rep('s', 10), 'o', '..'],
    ['...', rep('o', 10), '...'],
    ['....', 'o', rep('c', 6), 'o', '....'])];
}

function matrix() {
  // the hooded hacker: a dark hood, two green eyes in the shadow
  return [{ o: '#020a04', h: '#1d4426', f: '#050d07', g: '#5dff7e', d: '#1e8f3a' }, rows(
    ['.....', rep('o', 6), '.....'],
    ['...', 'oo', rep('h', 6), 'oo', '...'],
    ['..', 'o', rep('h', 10), 'o', '..'],
    ['.', 'o', rep('h', 12), 'o', '.'],
    ['.', 'o', 'hhh', rep('f', 6), 'hhh', 'o', '.'],
    ['o', 'hhh', rep('f', 8), 'hhh', 'o'],
    ['o', 'hh', rep('f', 10), 'hh', 'o'],
    ['o', 'hh', 'ff', 'gg', 'ff', 'gg', 'ff', 'hh', 'o'],
    ['o', 'hh', rep('f', 10), 'hh', 'o'],
    ['o', 'hh', 'fff', 'dddd', 'fff', 'hh', 'o'],
    ['o', 'hhh', rep('f', 8), 'hhh', 'o'],
    ['o', 'hhhh', rep('f', 6), 'hhhh', 'o'],
    ['o', rep('h', 14), 'o'],
    ['o', 'hhhhh', 'dddd', 'hhhhh', 'o'],
    ['o', rep('h', 14), 'o'],
    [rep('o', 16)])];
}

function win95() {
  // the paperclip assistant: a bent wire, two big eyes with raised brows
  const g = blank();
  for (const [x0, y0, x1, y1] of [[4, 7, 4, 14], [4, 15, 10, 15], [11, 14, 11, 9],
    [6, 9, 6, 12], [6, 13, 9, 13], [9, 12, 9, 6], [10, 6, 10, 6]]) line(g, x0, y0, x1, y1, 'p');
  put(g, [[5, 15], [11, 15], [5, 6], [6, 5], [7, 5], [8, 5]], 'p');
  put(g, [[4, 8], [4, 9], [4, 10], [6, 10], [6, 11]], 'q');
  outline(g, 'pq', 'o');
  const eyes = rows(
    ['.', 'kkk', '.', 'kkk', '........'],
    [rep('.', 16)],
    ['o', 'ww', 'o', 'ww', 'o', rep('.', 9)],
    ['o', 'wk', 'o', 'wk', 'o', rep('.', 9)],
    ['o', 'ww', 'o', 'ww', 'o', rep('.', 9)],
    ['.', 'oo', '.', 'oo', rep('.', 10)],
    ...Array.from({ length: 10 }, () => [rep('.', 16)]));
  eyes.forEach((row, y) => {
    Array.from(row).forEach((ch, x) => {
      // Python's g[y][x + 3] on a list: past the end it raises, but every
      // eye cell is at x <= 7, so the writes stay inside.
      if (ch !== '.') g[y][x + 3] = ch;
    });
  });
  return [{ o: '#2b2f3a', p: '#9ea4b6', q: '#eef0f6', w: '#ffffff', k: '#101018' }, join(g)];
}

function aero() {
  // the glossy orb buddy: a blue sphere, a white gloss, a smile
  const fill = (x, y) => {
    if (((x + 0.5 - 6.2) / 3.6) ** 2 + ((y + 0.5 - 4.3) / 1.8) ** 2 < 1) return 'w';
    if (y < 6) return 'a';
    return y < 11 ? 'b' : 'c';
  };
  const g = disc(8, 8, 7.6, 'o', fill);
  put(g, [[5, 8], [5, 9], [10, 8], [10, 9]], 'k');
  put(g, [[5, 11], [6, 12], [7, 12], [8, 12], [9, 12], [10, 11]], 'k');
  return [{ o: '#0a2a5a', a: '#8fdcff', b: '#3aa0f0', c: '#1c6cc8', w: '#f4fbff', k: '#0b2550' }, join(g)];
}

function cyberpunk() {
  // the neon android: a steel head, a magenta visor with cyan eyes
  return [{ o: '#0a0b12', m: '#4a5070', v: '#ff2fb4', c: '#3ff0ff', g: '#14161e', n: '#23263a', a: '#3ff0ff' }, rows(
    ['....', rep('o', 7), 'a', '....'],
    ['...', 'o', rep('m', 7), 'o', 'a', '...'],
    ['..', 'o', rep('m', 10), 'o', '..'],
    ['.', 'o', rep('m', 12), 'o', '.'],
    ['.', 'o', rep('m', 12), 'o', '.'],
    ['o', rep('v', 14), 'o'],
    ['o', 'v', 'ccc', rep('v', 6), 'ccc', 'v', 'o'],
    ['o', rep('v', 14), 'o'],
    ['.', 'o', rep('m', 12), 'o', '.'],
    ['.', 'o', 'mmm', rep('g', 6), 'mmm', 'o', '.'],
    ['.', 'o', rep('m', 12), 'o', '.'],
    ['..', 'o', rep('m', 10), 'o', '..'],
    ['...', rep('o', 10), '...'],
    ['.....', 'o', 'nnnn', 'o', '.....'],
    ['...', 'o', rep('m', 8), 'o', '...'],
    ['..', 'o', rep('m', 10), 'o', '..'])];
}

function lofi() {
  // the sleepy ginger cat: striped head, closed eyes, a white muzzle
  return [{ o: '#3a2010', r: '#e8893a', d: '#c0661f', i: '#f5a0a8', k: '#3a2010', w: '#fff3e6', n: '#f07890' }, rows(
    ['.', 'o', rep('.', 12), 'o', '.'],
    ['oio', rep('.', 10), 'oio'],
    ['orio', rep('.', 8), 'oiro'],
    ['o', 'rrr', rep('o', 8), 'rrr', 'o'],
    ['o', rep('r', 5), 'd', 'rr', 'd', rep('r', 5), 'o'],
    ['o', rep('r', 14), 'o'],
    ['o', 'rrr', 'd', rep('r', 6), 'd', 'rrr', 'o'],
    ['o', rep('r', 14), 'o'],
    ['o', 'rr', 'kkk', rep('r', 4), 'kkk', 'rr', 'o'],
    ['o', rep('r', 14), 'o'],
    ['o', 'rrr', rep('w', 8), 'rrr', 'o'],
    ['o', 'rr', 'wwww', 'nn', 'wwww', 'rr', 'o'],
    ['o', 'rr', 'www', 'k', 'ww', 'k', 'www', 'rr', 'o'],
    ['.', 'o', 'r', rep('w', 10), 'r', 'o', '.'],
    ['..', 'o', rep('r', 10), 'o', '..'],
    ['...', rep('o', 10), '...'])];
}

function luna() {
  // the cheerful manila folder: a tab, a front flap with a face
  return [{ o: '#7a5418', t: '#e0b050', f: '#f6d27a', k: '#4a3010', w: '#ffffff', b: '#f0907a' }, rows(
    [rep('.', 16)],
    ['..', rep('o', 5), rep('.', 9)],
    ['.', 'o', rep('t', 5), 'o', rep('.', 8)],
    ['o', rep('t', 6), rep('o', 9)],
    ['o', rep('t', 14), 'o'],
    [rep('o', 16)],
    ['o', rep('f', 14), 'o'],
    ['o', rep('f', 14), 'o'],
    ['o', 'fff', 'wk', 'ffff', 'wk', 'fff', 'o'],
    ['o', 'fff', 'kk', 'ffff', 'kk', 'fff', 'o'],
    ['o', 'f', 'b', rep('f', 10), 'b', 'f', 'o'],
    ['o', rep('f', 5), 'k', 'ff', 'k', rep('f', 5), 'o'],
    ['o', rep('f', 6), 'kk', rep('f', 6), 'o'],
    ['o', rep('f', 14), 'o'],
    ['o', rep('f', 14), 'o'],
    [rep('o', 16)])];
}

function orbit() {
  // the probe, shrunk to a sputnik: a silver ball, four swept-back antennas,
  // a red beacon -- and a face, it is still the mascot
  const g = blank();
  for (const [x0, y0, x1, y1] of [[6, 7, 0, 15], [7, 8, 3, 15], [5, 6, 0, 11], [8, 9, 7, 15]]) {
    line(g, x0, y0, x1, y1, 'a');
  }
  const fill = (x, y) => {
    if ((x + 0.5 - 8.6) ** 2 + (y + 0.5 - 3.6) ** 2 < 2.2) return 'w';
    return x + y < 18 ? 's' : 'm';
  };
  const d = disc(10, 5.5, 5.2, 'o', fill, g);
  put(d, [[8, 5], [8, 6], [12, 5], [12, 6]], 'k');
  put(d, [[9, 8], [10, 8], [11, 8]], 'k');
  put(d, [[10, 0]], 'l');
  return [{ o: '#1d2433', s: '#d8dde8', m: '#9aa3b8', w: '#ffffff', a: '#a8b0c4', k: '#1d2433', l: '#ff4d4d' }, join(d)];
}

function seasons() {
  // the fox kit: tall ears, an orange face, a white muzzle
  return [{ o: '#5a2a0c', r: '#f08a2c', w: '#fff4e8', k: '#2a1408', e: '#4a2410' }, rows(
    ['.', 'o', rep('.', 12), 'o', '.'],
    ['.', 'oo', rep('.', 10), 'oo', '.'],
    ['.', 'oeo', rep('.', 8), 'oeo', '.'],
    ['.', 'oro', rep('.', 8), 'oro', '.'],
    ['.', 'orro', rep('.', 6), 'orro', '.'],
    ['o', 'rrr', rep('o', 8), 'rrr', 'o'],
    ['o', rep('r', 14), 'o'],
    ['o', rep('r', 14), 'o'],
    ['o', 'rr', 'wk', rep('r', 6), 'wk', 'rr', 'o'],
    ['o', 'rr', 'kk', rep('r', 6), 'kk', 'rr', 'o'],
    ['o', 'ww', rep('r', 10), 'ww', 'o'],
    ['.', 'o', 'www', 'rr', 'ww', 'rr', 'www', 'o', '.'],
    ['..', 'o', 'wwww', 'kk', 'wwww', 'o', '..'],
    ['...', 'o', rep('w', 8), 'o', '...'],
    ['....', 'o', rep('w', 6), 'o', '....'],
    ['.....', rep('o', 6), '.....'])];
}

function orchard() {
  // Hum, the honeybee: antennae, pale wings, a round amber body in dark
  // stripes, two eyes and a smile (orchard and orchard-dark alike)
  return [{ k: '#1d1d1f', y: '#f5b400', w: '#e8f1ff', e: '#ffffff' }, rows(
    ['....', 'k', '......', 'k', '....'],
    ['.....', 'k', '....', 'k', '.....'],
    ['......', 'kkkk', '......'],
    ['.', 'ww', '..', 'k', 'yyyy', 'k', '..', 'ww', '.'],
    ['wwww', 'k', 'yyyyyy', 'k', 'wwww'],
    ['www', 'k', 'y', 'ek', 'yy', 'ek', 'y', 'k', 'www'],
    ['.', 'ww', 'k', 'yyyyyyyy', 'k', 'ww', '.'],
    ['..', 'k', 'yyy', 'k', 'yy', 'k', 'yyy', 'k', '..'],
    ['..', 'k', 'yyyy', 'kk', 'yyyy', 'k', '..'],
    ['..', rep('k', 12), '..'],
    ['..', 'k', rep('y', 10), 'k', '..'],
    ['..', rep('k', 12), '..'],
    ['...', 'k', rep('y', 8), 'k', '...'],
    ['....', rep('k', 8), '....'],
    ['.....', 'k', 'yyyy', 'k', '.....'],
    ['......', 'kkkk', '......'])];
}

const BUILDERS = {
  moe, matrix, win95, aero, cyberpunk, lofi, luna, orbit, seasons, orchard, 'orchard-dark': orchard,
};

// SPRITES: theme -> {palette, rows} (16 rows of 16 letters, "." clear).
export const SPRITES = Object.fromEntries(Object.entries(BUILDERS).map(([t, f]) => {
  const [palette, rws] = f();
  return [t, { palette, rows: rws }];
}));

// The mood signs, 7 x 7, over the top-right corner of the grid.
export const SIGNS = {
  alert: {
    palette: { r: '#f0565a', w: '#ffffff', o: '#3a0c10' },
    rows: ['.ooooo.', 'orrwrro', 'orrwrro', 'orrwrro', 'orrrrro', 'orrwrro', '.ooooo.'],
  },
  relieved: {
    palette: { g: '#3fbf7f', w: '#ffffff', o: '#0c3a22' },
    rows: ['.ooooo.', 'oggggwo', 'ogggwwo', 'owgwwgo', 'owwwggo', 'ogwgggo', '.ooooo.'],
  },
};

// sprite is a theme's {palette, rows}; an unknown theme gets moe's.
export function sprite(theme) {
  return SPRITES[theme] || SPRITES[DEFAULT];
}

// pixels is the icon as GRID rows of GRID cells: "#rrggbb", or null where it
// is clear. The sprite at the bottom left, a mood's sign at the top right.
export function pixels(theme, mood = 'calm') {
  const grid = Array.from({ length: GRID }, () => new Array(GRID).fill(null));
  const cells = (pal, rws, ox, oy) => {
    rws.forEach((row, y) => {
      Array.from(row).forEach((ch, x) => {
        if (ch !== '.' && Object.hasOwn(pal, ch)) grid[oy + y][ox + x] = pal[ch];
      });
    });
  };
  const s = sprite(theme);
  cells(s.palette, s.rows, 0, GRID - 16);
  if (Object.hasOwn(SIGNS, mood)) cells(SIGNS[mood].palette, SIGNS[mood].rows, GRID - 7, 0);
  return grid;
}

// drawIcon fills a 2D context (anything with fillStyle and fillRect) with the
// icon at scale px per cell, from (0, 0); it clears nothing.
export function drawIcon(ctx, theme, mood = 'calm', scale = SCALE) {
  pixels(theme, mood).forEach((row, y) => {
    row.forEach((colour, x) => {
      if (!colour) return;
      ctx.fillStyle = colour;
      ctx.fillRect(x * scale, y * scale, scale, scale);
    });
  });
}

const urls = new Map(); // "theme mood scale" -> data URL

// iconDataURL is the icon as a PNG data URL, GRID * scale px square, for a
// notification's icon or a PWA icon link. Cached per theme, mood and scale.
// opts.canvas is a canvas to draw on (sized here); without one a new one from
// document. null when there is no canvas at all (node, a worker).
export function iconDataURL(theme, mood = 'calm', opts = {}) {
  const t = Object.hasOwn(SPRITES, theme) ? theme : DEFAULT;
  const m = MOODS.includes(mood) ? mood : 'calm';
  const scale = Math.max(1, Math.trunc(opts.scale || SCALE));
  const key = `${t} ${m} ${scale}`;
  if (!opts.canvas && urls.has(key)) return urls.get(key);
  let canvas = opts.canvas || null;
  if (!canvas && typeof document !== 'undefined' && document.createElement) canvas = document.createElement('canvas');
  if (!canvas || typeof canvas.getContext !== 'function') return null;
  canvas.width = GRID * scale;
  canvas.height = GRID * scale;
  const ctx = canvas.getContext('2d');
  if (!ctx) return null;
  ctx.clearRect(0, 0, canvas.width, canvas.height);
  ctx.imageSmoothingEnabled = false;
  drawIcon(ctx, t, m, scale);
  const url = canvas.toDataURL('image/png');
  if (!opts.canvas) urls.set(key, url);
  return url;
}
