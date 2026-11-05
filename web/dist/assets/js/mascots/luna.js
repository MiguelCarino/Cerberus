// The luna theme's mascot: Folio, a cheerful manila folder sitting on one
// round green hill under a blue sky, with a little XP-style taskbar along the
// bottom of the stage. A faithful port of Atrium's mascot_luna.py: the same
// (state, tick, tool, mood, scene, cols, rows) draws the same frame, cell for
// cell and colour for colour (golden tests: web/test/mascots-themes-b.test.mjs).
// Our own drawing: an homage to the era, no Microsoft picture copied.
//
//   sky      cell backgrounds, deep blue at the top fading into the stage card
//            near the hill; at night just the card, with stars and a moon.
//            Small clouds of ~ and ( drift across it
//   the hill ONE broad smooth dome: a dark green fill under an edge drawn in
//            _ . - ' by how high the slope reaches into each cell, grass tufts
//   taskbar  the bottom row: a blue strip, a green "start" segment, Folio's own
//            task button when there is room, the HH:MM in a lighter tray well
//   Folio    13 x 7 cells: a tab bump, the back of the folder, the front flap,
//            a face; it bobs one row, and its shadow lies on the hill
//   props    small XP windows to its right: C:\>_ for Bash, a page for Read, a
//            page and a pencil for Edit/Write, a search box for searches, a
//            globe and progress blocks for WebFetch, two mini folders for
//            Task/Agent, a to-do list, sparkles otherwise; the hourglass when
//            thinking, pages fluttering while writing, a balloon tip when done,
//            an error window on error.
// Moods change the idle face and fidget (accents only while working); scenes
// add weather, music, a controller, party hats, papel picado and a reminder.
//
// The taskbar clock is the real time: setClock(fn) swaps the clock (a function
// returning a Date) for tests.

import { Canvas, STATES, BaseMascot, blink, lcg, propKind, streamLines, fd, mod, int, makeTheme } from './common.js';
import { MOODS, word } from './mood.js';

export const PANEL = '#0d2350';

// the folder
const TAB = '#d9b44a';
const BODY = '#c9a23c';
const FLAP = '#a8842a';
const INSIDE = '#7a5c18';
const OUTLINE = '#6b5214';
const SHINE = '#f2dc8a';
const INK = '#1c1a14';
const CHEEK = '#ff9a8a';
const BODY_DIM = '#a89050';
const FLAP_DIM = '#8a7438';
const TAB_DIM = '#b09a58';
// the land and the sky
const SKY_TOP = '#163d94';
const SKY_TOP_DIM = '#112f72';
const HILL = '#1f5a24';
const HILL_EDGE = '#5fae4a';
const TUFT = '#4f9a3e';
const SHADOW = '#123a16';
const CLOUD = '#9fb8e0';
const CLOUD_GREY = '#6f86ac';
const CLOUD_STORM = '#566e94';
const STAR = '#a8c8f0';
const STAR_DIM = '#5a78a8';
const MOON = '#f3e3a8';
const SUN = '#ffd75e';
const RAIN = '#9fc4f0';
const SNOW = '#f2f8ff';
const FOG = '#5a7898';
const FLASH = '#25499a';
// the taskbar and the windows
const TASKBAR = '#1d4cb0';
const START = '#2b7f2b';
const TASK_BTN = '#143680';
const TRAY = '#0e66b0';
const WHITE = '#ffffff';
const TEXT = '#e4eefa';
const TEXT_DIM = '#9fb0d0';
const TITLE = '#2459c8';
const CLOSE = '#c8402c';
const WIN_BODY = '#10285a';
const CMD_BODY = '#000000';
const PAGE = '#b8b39c';
const PAGE_DIM = '#4e4a36';
const SEL = '#316ac5';
const BALLOON = '#cbc7a3';
const GOOD = '#14540f';
const BAD = '#ff6b5a';
const BAR = '#3fa83a';
const TROUGH = '#b8b39c';
const GLASS = '#9fb8e0';
const SAND = '#e3c04a';
const GLOW = '#f0a030';
const SWEAT = '#9fd0ff';
const NOTE = ['#ffd75e', '#a8e890'];
const CONFETTI = ['#ff8a7a', '#ffd75e', '#a8e890', '#9cc2ff', '#c8b8ff'];
const PICADO = ['#ff6fae', '#ffa94d', '#7fd46f', '#6fb6ff', '#c58bff', '#ffd84d'];
const OVERCAST = ['cloud', 'rain', 'storm', 'snow', 'fog'];

const OW = 13;
const OH = 7;
const FOLDER = [
  ' .----.      ',
  "/      '----.",
  '|           |',
  '|           |',
  '|           |',
  '|           |',
  "'-----------'",
];
const EYES = [3, 9];
const MOUTH = [4, 6];
const OPEN = ['|\\         /|', "| `-------' |"];
const DROOP = "|`-._   _.-'|";

const COMP_W = OW + 2 + 17;
const WORDS = ['the', 'file', 'is', 'ready', 'and', 'all', 'set', 'your', 'notes', 'ok', 'to',
  'folder', 'now', 'done', 'a', 'it', 'saved', 'so', 'here'];
const CMDS = ['dir', 'ver', 'cls', 'tree', 'ping', 'type'];
const TODO = ['Back up', 'Update', 'Tidy up'];

const MOOD_FACE = {
  happy: ['^', '^', 'w', '~'],
  excited: ['*', '*', 'D', '~'],
  shy: ['>', '<', '~', '/'],
  curious: ['o', 'O', 'o', ''],
  focused: ['o', 'o', '-', ''],
  worried: ['o', 'o', '~', ''],
  sad: ['.', '.', 'n', ''],
  sleepy: ['-', '-', '.', ''],
};
const BROWS = { focused: ['\\', '/'], worried: ['/', '\\'], sad: ['/', '\\'] };
const FIDGETS = {
  calm: [['shuffle', 2], ['peek', 2], ['balloon', 2], ['wave', 1], ['rest', 2]],
  happy: [['wave', 3], ['balloon', 2], ['shuffle', 1]],
  excited: [['wave', 3], ['balloon', 2], ['peek', 2]],
  shy: [['peek', 3], ['rest', 3]],
  curious: [['peek', 3], ['balloon', 2]],
  focused: [['shuffle', 3], ['rest', 2]],
  worried: [['shuffle', 2], ['rest', 2]],
  sad: [['rest', 1]],
  sleepy: [['yawn', 3], ['rest', 2]],
};

export const MOOD_BUBBLES = {
  happy: 'Neat as a new folder. Ready for the next job!',
  excited: "Ooh, a new project! I've got a fresh folder ready!",
  shy: "Oh! I didn't see you there… hello ☺",
  curious: "What's in there? Let's take a look!",
  focused: "Sorting things out. I'm listening.",
  worried: 'Hmm, something looks out of order…',
  sad: 'My papers are all crumpled today…',
  sleepy: 'Folio is idle… zz',
};
// mood.js topics in its words, a balloon tip's voice ({0} the topic's arguments).
export const TOPIC_LINES = {
  thanks: 'Any time! I\'ll file that one under "nice".',
  worried: "Hmm, that doesn't look right. Shall we check?",
  frustrated: "Stuck again? Let's sort it out afresh.",
  relieved: 'Phew! It works now.',
  determined: "This is a big one. I'll keep the papers in order.",
  proud: 'All done, and neatly filed!',
  sad: "Oh dear. I'll sit quietly in the drawer for a bit…",
  happy: 'Hooray!',
  curious: 'Interesting! Tell me more?',
  rain: "It's raining outside. Good day to stay in and tidy up.",
  storm: 'A storm is coming. Save your work!',
  snow: "Snow on the hill! I'll keep your files cosy ❄",
  fog: "Can't see the hill for the fog. Careful as you go.",
  sun: 'Lovely sunny day on the hill ☀',
  hot: "It's {0} outside. Stay cool!",
  cold: '{0}! Chilly enough to curl my corners.',
  gpu_hot: "Graphics card at {0}°C. Don't toast my edges!",
  cpu_hot: 'Processor at {0}°C! Time for a breather.',
  music: 'Nice tune! ♪',
  game: 'Play first, paperwork later!',
  event: 'Psst: {0} comes up in {1} min.',
  event_now: "Now's the moment: {0}.",
  birthday: "It's your big day, {0}! Party hats on 🎈",
  holiday: "It's {0}. The in-tray can wait!",
  go_home: '{0} already. Close the drawers, go home!',
  bedtime: '{0}. Even the files need their sleep. Shut down?',
  morning: "Morning! I've sorted today's papers.",
  lunch: 'Time for a lunch break?',
  weekend: 'No in-tray on a weekend. Go play outside!',
  friday: 'Friday afternoon! Nearly time to log off ☺',
  hello: ['Hi there! What shall we file today?',
    'Hello! Fresh folder, ready to go.',
    'Welcome! Pick a task, any task.'],
  welcome_back: 'Welcome back! I kept your desktop tidy.',
  wake: ['Oh! Sorry, my screensaver kicked in.',
    "Huh? I'm up! Just defragmenting…"],
  long_prompt: ["That's a thick stack of papers!",
    'Whoa, this needs a bigger folder!'],
  code: ['Code! Let me get the right folder.',
    "Ooh, source files. I'll sort them."],
  image: ['A picture! Straight into My Pictures.',
    'Ooh, a picture! Nice thumbnail.'],
  aborted: ['Cancelled. Nothing was lost!',
    'Stopped. Undo is my middle name.'],
  tests_pass: ['All tests passed! ✓',
    'Green checkmarks all round!',
    'Tests OK. Filed under "works".'],
  tests_fail: ["Some tests failed. Let's take a look.",
    "Tests failed… I'll pull the report."],
  commit: ['Committed! Safely filed away.',
    'Saved to history ✓',
    'Shipped! Stamped and filed.'],
  fast: ['That was quick!',
    'Done in a click!',
    'Speedy! Like a fresh install.'],
  busy: ['So many windows open! Busy busy…',
    'Lots of tasks running. Hang on!'],
  model: ['Say hello to {0}!',
    'New helper on the job: {0}.'],
  poke: ['Hi! Need a hand?',
    'Hee, that tickles my tab!',
    'You clicked me! Shall we work?',
    'Double-click to open… just kidding!',
    "Yes? I'm all ears. Well, all tabs.",
    "Psst! I'm a folder, not a button ☺"],
  poke_spam: ['Hey! My papers are going everywhere!',
    "Easy! You'll scatter my files!",
    "Okay, okay! I'm wide awake!"],
  pet: ["Aww, you're making my corners curl ☺",
    "That's nice… I feel so organised.",
    'Hehe, a gold star for me?',
    "D'aww. Best desktop ever."],
  rg_memory: "{0} was hogging the memory -- I've put it away.",
  rg_vram: '{0} filled the graphics memory -- filed it away.',
  rg_game_vram: '{0} is using nearly all the graphics memory. Lower the textures?',
  rg_panic: 'Done! {0} is closed.',
  rg_stuck: "Memory's nearly full and I can't find which file to close...",
  rg_agents: "All the agents' jobs are closed. Your sessions are safe.",
};
export const EMOTE_COLOURS = {
  accent: '#9cc2ff', warn: SUN, bad: BAD, good: '#a8e890', love: '#ffa0c8', dim: TEXT_DIM,
};

// ---------- helpers ----------

// pyRound is Python's round() to an int: halves go to the even neighbour.
function pyRound(x) {
  return Math.abs(x % 1) === 0.5 ? 2 * Math.round(x / 2) : Math.round(x);
}

const hex2 = (n) => n.toString(16).padStart(2, '0');

// mix is the colour f of the way from hex a to hex b.
export function mix(a, b, f) {
  let out = '#';
  for (const i of [1, 3, 5]) {
    const x = parseInt(a.slice(i, i + 2), 16);
    const y = parseInt(b.slice(i, i + 2), 16);
    out += hex2(int(pyRound(x + (y - x) * f)));
  }
  return out;
}

let clock = () => new Date();

// setClock swaps the clock the taskbar reads (a function returning a Date);
// null puts the real one back.
export function setClock(fn) {
  clock = fn || (() => new Date());
}

function hhmm() {
  const d = clock();
  return ` ${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')} `;
}

const has = (scene, tags) => tags.some((t) => scene.has(t));

// ---------- the mascot ----------

class Mascot extends BaseMascot {
  constructor(cols = 64, rows = 18) {
    super(cols, rows);
    this.cols = Math.max(int(cols), COMP_W + 2);
    this.rows = Math.max(int(rows), OH + 7);
    this.cx = fd(this.cols - COMP_W, 2);
    this.bottom = this.rows - 2;
    const peak = this.rows >= 18 ? 4.5 : 3.6;
    const hc = this.cx + OW / 2.0;
    const half = Math.max(this.cols * 0.62, 30.0);
    this.h = [];
    for (let x = 0; x < this.cols; x++) {
      const u = Math.min(1.0, Math.abs(x + 0.5 - hc) / half);
      this.h.push(0.9 + (peak - 0.9) * (1 - u * u) + 0.25 * Math.sin(x * 0.17 + 1.3));
    }
    this.ground = this.bottom - int(this.h[int(hc)]);
    this.tufts = [];
    for (let x = 0; x < this.cols; x++) this.tufts.push(lcg(89, x)());
    this.stream = streamLines(WORDS, 12, 67);
    this.clouds = [];
    for (let i = 0; i < Math.max(2, fd(this.cols, 28)); i++) {
      const r = lcg(79, i, this.cols);
      this.clouds.push([r() * this.cols, int(r() * Math.max(2, fd(this.ground, 3))), 0.02 + r() * 0.03]);
    }
    this.grey = [];
    for (let i = 0; i < Math.max(2, fd(this.cols, 24)); i++) {
      const r = lcg(83, i, this.cols);
      this.grey.push([r() * this.cols, int(r() * Math.max(2, fd(this.ground, 3))), 0.03 + r() * 0.03]);
    }
    this.stars = [];
    for (let i = 0; i < fd(this.cols * this.rows, 40); i++) {
      const r = lcg(97, i);
      this.stars.push([int(r() * this.cols), int(r() * Math.max(2, this.ground - 3)),
        20 + int(r() * 30), int(r() * 50)]);
    }
  }

  bubble(state, tool = null, mood = null) {
    if (state === 'idle' && Object.hasOwn(MOOD_BUBBLES, mood || '')) return MOOD_BUBBLES[mood];
    return super.bubble(state, tool);
  }

  edge(x) {
    return this.bottom - int(this.h[x]);
  }

  // ---------- composition ----------

  render(state, tick, tool = null, mood = null, scene = new Set()) {
    if (!STATES.includes(state)) state = 'idle';
    if (!MOODS.includes(mood)) mood = 'calm';
    if (mood === 'sleepy' && ['thinking', 'tool', 'writing'].includes(state)) mood = 'focused';
    tick = int(tick);
    const cv = new Canvas(this.cols, this.rows);
    const kind = state === 'tool' ? propKind(tool) : null;
    const idle = state === 'idle';
    const fidget = idle ? this.fidget(tick, mood) : null;
    this.sky(cv, state, tick, scene);
    if (scene.has('festive')) this.banner(cv, tick);
    this.hill(cv, tick, scene);
    if (scene.has('fog')) this.fog(cv, tick);
    if (scene.has('snow')) this.snow(cv, tick);
    if (scene.has('rain') || scene.has('storm')) this.weatherRain(cv, tick, scene.has('storm'));
    if (scene.has('hot')) this.shimmer(cv, tick);
    if (scene.has('party')) this.confetti(cv, tick);
    this.taskbar(cv, tick, scene);
    let hover;
    if (state === 'done') hover = [0, 1, 2, 2, 1, 0, 0, 0][mod(tick, 8)];
    else if (idle && mood === 'excited' && mod(fd(tick, 30), 2)) hover = [0, 1, 2, 2, 1, 0][mod(tick, 6)];
    else if (state === 'error' || (idle && (mood === 'sad' || mood === 'sleepy'))) hover = 0;
    else if (scene.has('music')) hover = mod(fd(tick, 4), 2);
    else hover = Math.sin(tick / 7.0) > 0 ? 1 : 0;
    const top = this.ground - OH - hover;
    const ox = this.cx;
    this.shadow(cv, ox, hover);
    if (kind === 'helpers') this.helpers(cv, tick);
    this.head = [Math.min(ox + OW - 3, this.cols - 3), Math.max(0, top)];
    this.folder(cv, state, tick, ox, top, mood, scene, fidget);
    this.moodMarks(cv, state, tick, ox, top, mood, scene, fidget);
    if (scene.has('party')) this.hat(cv, tick, ox, top);
    const px = this.cx + OW + 2;
    const py = this.ground - 8;
    if (state === 'thinking') this.hourglass(cv, tick, px + 4, py);
    else if (state === 'writing') {
      this.pages(cv, tick, ox, top, px, py);
      this.doc(cv, tick, px, py, true, true);
    } else if (state === 'done') this.check(cv, tick, px, py);
    else if (state === 'error') this.error(cv, tick, px, py);
    else if (kind && kind !== 'helpers') this[`prop_${kind}`](cv, tick, px, py);
    else if (kind === 'helpers') { /* drawn above */ } else if (scene.has('game')) this.controller(cv, tick, px, py);
    return cv;
  }

  fidget(tick, mood) {
    if (mod(tick, 90) < 72) return null;
    const table = FIDGETS[mood] || FIDGETS.calm;
    let pick = lcg(109, fd(tick, 90))() * table.reduce((s, [, w]) => s + w, 0);
    for (const [name, weight] of table) {
      pick -= weight;
      if (pick < 0) return name === 'rest' ? null : name;
    }
    return null;
  }

  // ---------- the sky and the land ----------

  sky(cv, state, tick, scene) {
    const overcast = has(scene, OVERCAST);
    const clear = !overcast && state !== 'error';
    const night = scene.has('night');
    const flash = scene.has('storm') && mod(tick, 47) < 2;
    if (!night || flash) {
      const top = flash ? FLASH : (overcast ? SKY_TOP_DIM : SKY_TOP);
      const span = Math.max(1, this.ground);
      for (let y = 0; y < this.bottom + 1; y++) {
        const bg = mix(top, PANEL, Math.min(1.0, y / span));
        for (let x = 0; x < this.cols; x++) cv.recolour(x, y, null, bg);
      }
    }
    const y0 = scene.has('festive') ? 2 : 0;
    if (night) {
      const moon = clear && this.cols >= 50;
      for (const [x, y, period, ph] of this.stars) {
        if (overcast && mod(x + y, 3)) continue;
        if (moon && x >= this.cols - 6 && y < y0 + 3) continue;
        if (cv.get(x, y) !== ' ') continue;
        const t = mod(tick + ph, period);
        cv.cell(x, y, t < 5 ? '.+*+.'[t] : '.', t < 5 ? STAR : STAR_DIM);
      }
      if (moon) {
        cv.put(this.cols - 4, y0, '.-', MOON);
        cv.put(this.cols - 5, y0 + 1, '(', MOON);
        cv.put(this.cols - 4, y0 + 2, '`-', MOON);
      }
    } else if (clear && (scene.has('day') || scene.has('sun')) && this.cols >= 48) {
      this.sun(cv, tick, y0);
    }
    this.cloudLayer(cv, tick, scene);
    if (scene.has('storm')) this.lightning(cv, tick);
  }

  cloudLayer(cv, tick, scene) {
    const overcast = has(scene, OVERCAST);
    const grey = scene.has('storm') ? CLOUD_STORM : CLOUD_GREY;
    const y0 = scene.has('festive') ? 3 : 1;
    if (overcast) {
      for (const [x0, yy, speed] of this.grey) {
        const x = mod(int(x0 + tick * speed), this.cols + 14) - 10;
        const y = yy + y0;
        cv.put(x + 2, y, '(~~~)  (~~)', grey);
        cv.put(x, y + 1, '(~~~~~~~~~~~~)', grey);
      }
    }
    const colour = !overcast ? CLOUD : CLOUD_GREY;
    for (const [x0, yy, speed] of this.clouds) {
      const x = mod(int(x0 + tick * speed), this.cols + 10) - 7;
      const y = yy + y0;
      cv.put(x + 1, y, '(~~)', colour);
      cv.put(x, y + 1, '(~~~~~)', colour);
    }
  }

  hill(cv, tick, scene) {
    const snow = scene.has('snow');
    for (let x = 0; x < this.cols; x++) {
      const h = this.h[x];
      const top = this.bottom - int(h);
      for (let y = top + 1; y < this.bottom + 1; y++) cv.cell(x, y, ' ', null, HILL);
      const frac = h - int(h);
      const glyph = "_.-'"[Math.min(3, int(frac * 4))];
      cv.cell(x, top, glyph, snow ? SNOW : HILL_EDGE);
    }
    const wind = mod(fd(tick, 6), 2);
    this.tufts.forEach((v, x) => {
      if (v < 0.22) {
        const y = this.edge(x) + 1;
        if (y <= this.bottom) {
          if (snow) cv.cell(x, y, v < 0.1 ? '.' : 'o', SNOW, HILL);
          else cv.cell(x, y, v < 0.11 ? ",'"[mod(x + wind, 2)] : '"', TUFT, HILL);
        }
      }
    });
  }

  shadow(cv, ox, hover) {
    const w = 11 - hover * 2;
    for (let c = 0; c < w; c++) {
      const x = ox + fd(OW - w, 2) + c;
      if (x >= 0 && x < this.cols) cv.cell(x, this.edge(x), '_', SHADOW, HILL);
    }
  }

  taskbar(cv, tick, scene) {
    const y = this.rows - 1;
    cv.fill(0, y, this.cols, 1, TASKBAR, ' ', null, false);
    cv.put(0, y, ' start ', WHITE, false, START);
    cv.cell(7, y, ')', '#1d5a1d', TASKBAR);
    if (this.cols >= 60) cv.put(9, y, ' Folio ', TEXT, false, TASK_BTN);
    const clk = hhmm();
    const ring = scene.has('clock') && mod(fd(tick, 5), 2);
    const cx = this.cols - clk.length;
    cv.put(cx, y, clk, ring ? INK : WHITE, false, ring ? GLOW : TRAY);
    const icons = scene.has('music') ? [['d', NOTE[0]]] : [];
    if (scene.has('clock')) icons.push(['!', ring ? GLOW : WHITE]);
    const tx = cx - 2 * icons.length;
    cv.fill(tx, y, 2 * icons.length, 1, TRAY, ' ', null, false);
    icons.forEach(([c, colour], i) => cv.cell(tx + 1 + 2 * i, y, c, colour, TRAY));
    if (scene.has('clock') && mod(fd(tick, 10), 3)) {
      const text = ' (!) Reminder ';
      const bx = Math.max(0, this.cols - text.length - 1);
      cv.put(bx, y - 2, text, INK, false, BALLOON);
      cv.cell(bx + 2, y - 2, '!', '#8e1c16', BALLOON);
      cv.cell(this.cols - 4, y - 1, 'V', BALLOON);
    }
  }

  // ---------- the scene ----------

  sun(cv, tick, y = 0) {
    const x = this.cols - 7;
    const rays = ['\\ | /', '  |  ', '\\   /'][mod(fd(tick, 8), 3)];
    cv.put(x, y, rays, SUN);
    cv.put(x, y + 1, '-(O)-', SUN);
    cv.put(x, y + 2, rays[0] === '\\' ? '/ | \\' : '  |  ', SUN);
  }

  lightning(cv, tick) {
    const cyc = fd(tick, 47);
    const t = mod(tick, 47);
    if (t > 3) return;
    const x = int(lcg(113, cyc)() * (this.cols - 4)) + 1;
    [[1, '/'], [0, '/'], [0, '\\'], [1, '/']].forEach(([dx, ch], k) => {
      cv.cell(x + dx + (k > 1 ? 1 : 0), 1 + k, ch, t % 2 === 0 ? SUN : SNOW);
    });
  }

  banner(cv, tick) {
    for (let x = 0; x < this.cols; x++) {
      cv.cell(x, 0, '-', TEXT_DIM);
      const k = x % 4;
      if (k < 3) {
        const flutter = mod(fd(tick, 4) + fd(x, 4), 2);
        cv.cell(x, 1, ['o.o', '.o.'][flutter][k], PANEL, PICADO[fd(x, 4) % PICADO.length]);
      }
    }
  }

  fog(cv, tick) {
    [fd(this.rows, 3), fd(this.rows, 2) + 1, this.rows - 3].forEach((y, k) => {
      const off = mod(fd(tick, 6 + k * 2) + k * 7, 12);
      for (let x = 0; x < this.cols; x++) {
        if ((x + off) % 12 < 5 && (x + off) % 12 !== 2) cv.cell(x, y, '~', FOG);
      }
    });
  }

  snow(cv, tick) {
    const span = this.rows + 2;
    for (let i = 0; i < Math.max(6, fd(this.cols * this.rows, 60)); i++) {
      const r = lcg(127, i);
      const speed = 0.05 + r() * 0.06;
      const pos = r() * span + tick * speed;
      const fall = int(fd(pos, span));
      const y = int(pos - fall * span) - 1;
      const x = int(lcg(131, i, fall)() * this.cols + [0, 1, 1, 0, 0, -1, -1, 0][mod(fd(tick, 5) + i, 8)]);
      if (y < this.rows - 1 && cv.get(x, y) === ' ') cv.cell(x, y, '*.+'[i % 3], SNOW);
    }
  }

  weatherRain(cv, tick, heavy = false) {
    const span = this.rows + 2;
    for (let i = 0; i < fd(this.cols * this.rows, heavy ? 14 : 24); i++) {
      const r = lcg(137, i);
      const speed = (heavy ? 0.7 : 0.5) + r() * 0.3;
      const y = int(mod(r() * span + tick * speed, span)) - 1;
      const x = mod(int(r() * this.cols - y * 0.5), this.cols);
      if (y < this.rows - 1 && cv.get(x, y) === ' ') cv.cell(x, y, '/', RAIN);
    }
  }

  shimmer(cv, tick) {
    for (let x = 0; x < this.cols; x++) {
      if (lcg(139, x, fd(tick, 3))() < 0.08) {
        const y = this.edge(x) - 1 - (x % 2);
        if (cv.get(x, y) === ' ') cv.cell(x, y, '~', GLOW);
      }
    }
  }

  confetti(cv, tick) {
    const span = this.rows + 2;
    for (let i = 0; i < Math.max(4, fd(this.cols * this.rows, 90)); i++) {
      const r = lcg(149, i);
      const pos = r() * span + tick * (0.12 + r() * 0.1);
      const fall = int(fd(pos, span));
      const y = int(pos - fall * span) - 1;
      const x = int(lcg(151, i, fall)() * this.cols) + mod(fd(tick, 4) + i, 2);
      if (y < this.rows - 1 && cv.get(x, y) === ' ') {
        cv.cell(x, y, ",'.*"[mod(fd(tick, 3) + i, 4)], CONFETTI[i % CONFETTI.length]);
      }
    }
  }

  hat(cv, tick, ox, top) {
    cv.cell(ox + 3, top - 3, '*+'[mod(fd(tick, 4), 2)], SUN);
    cv.put(ox + 2, top - 2, '/:\\', CONFETTI[0]);
    cv.put(ox + 1, top - 1, '/_:_\\', CONFETTI[3]);
    cv.cell(ox + 3, top - 2, ':', SUN);
    cv.cell(ox + 3, top - 1, ':', SUN);
  }

  controller(cv, tick, x, y) {
    cv.put(x + 1, y + 2, '  .-----.  ', CLOUD);
    cv.put(x + 1, y + 3, ' / +  o o\\ ', CLOUD);
    cv.put(x + 1, y + 4, '(   .-.   )', CLOUD);
    cv.put(x + 1, y + 5, " `-'   `-' ", CLOUD);
    cv.cell(x + 4, y + 3, '+', TEXT);
    cv.cell(x + 7, y + 3, 'o', NOTE[1]);
    cv.cell(x + 9, y + 3, 'o', [SUN, BAD, NOTE[1], RAIN][mod(fd(tick, 3), 4)]);
    cv.put(x + 2, y + 6, ' Game on! ', WHITE, false, mod(fd(tick, 6), 2) ? START : TASK_BTN);
  }

  // ---------- Folio ----------

  folder(cv, state, tick, x0, top, mood, scene, fidget) {
    const idle = state === 'idle';
    const tired = state === 'error' || (idle && (mood === 'sad' || mood === 'sleepy'));
    const [tab, body, flap] = tired ? [TAB_DIM, BODY_DIM, FLAP_DIM] : [TAB, BODY, FLAP];
    const t = mod(tick, 90) - 72;
    const opened = state === 'writing' || (fidget === 'peek' && t >= 4 && t < 15);
    FOLDER.forEach((r, i) => cv.sprite(x0, top + i, r, OUTLINE));
    for (let c = 1; c < 7; c++) cv.recolour(x0 + c, top + 1, null, tab);
    for (const i of [2, 3]) for (let c = 1; c < OW - 1; c++) cv.recolour(x0 + c, top + i, null, body);
    for (const i of [4, 5]) for (let c = 1; c < OW - 1; c++) cv.recolour(x0 + c, top + i, null, opened ? INSIDE : flap);
    if (opened) {
      OPEN.forEach((r, i) => {
        Array.from(r.slice(1, -1)).forEach((ch, k) => {
          if (ch !== ' ') cv.cell(x0 + k + 1, top + 4 + i, ch, OUTLINE, INSIDE);
        });
      });
    } else if (tired || (idle && mood === 'worried')) {
      Array.from(DROOP.slice(1, -1)).forEach((ch, k) => {
        if (ch !== ' ') cv.cell(x0 + k + 1, top + 5, ch, OUTLINE, flap);
      });
    }
    if (fidget === 'shuffle' || state === 'tool' || state === 'writing') {
      const off = fidget === 'shuffle' ? mod(fd(tick, 3), 2) : 0;
      cv.put(x0 + 8 + off, top, '[==]'.slice(0, 4 - off), PAGE_DIM, false, PAGE);
    }
    cv.cell(x0 + 10, top + 2, "'", SHINE, body);
    if (scene.has('cold')) {
      for (const [c, i] of [[1, 5], [11, 5]]) cv.cell(x0 + c, top + i, '*', SNOW, flap);
    }
    // face
    const eye = state === 'done' ? '^' : 'o';
    let gaze = state === 'tool' || state === 'writing' ? 1 : 0;
    if (state === 'thinking') gaze = [0, 1, 0, -1][mod(fd(tick, 12), 4)];
    let mouth = { done: 'D', error: '~', thinking: 'o' }[state] || 'v';
    if (state === 'writing') mouth = 'o-o-v-'[mod(fd(tick, 2), 6)];
    let left = eye;
    let right = eye;
    let cheek = state === 'done' || state === 'idle' ? '~' : '';
    if (idle && Object.hasOwn(MOOD_FACE, mood)) {
      [left, right, mouth, cheek] = MOOD_FACE[mood];
      if (mood === 'shy') gaze = -1;
      else if (mood === 'curious') gaze = 1;
      else if (mood === 'worried') gaze = [0, -1, 0, 1][mod(fd(tick, 8), 4)];
    } else if (mood === 'shy' && state !== 'error') {
      cheek = '/';
    } else if ((mood === 'happy' || mood === 'excited') && state !== 'error') {
      cheek = '~';
      if (mood === 'excited' && state !== 'done' && mod(tick, 20) < 3) {
        left = '*';
        right = '*';
      }
    }
    if (state === 'error') {
      left = 'x';
      right = 'x';
    }
    if (fidget === 'peek') gaze = t < 9 ? -1 : 1;
    else if (fidget === 'yawn') {
      left = '-';
      right = '-';
      mouth = t >= 3 && t < 14 ? 'O' : 'o';
    } else if (fidget === 'balloon') mouth = 'o';
    const shut = blink(tick, 47) && state !== 'done' && state !== 'error';
    [left, right].forEach((ch, k) => cv.cell(x0 + EYES[k] + gaze, top + 3, shut ? '-' : ch, INK, body));
    const mbg = opened ? INSIDE : flap;
    cv.cell(x0 + MOUTH[1] + gaze, top + MOUTH[0], mouth, opened ? SHINE : INK, mbg);
    if (Object.hasOwn(BROWS, mood) && state !== 'done') {
      BROWS[mood].forEach((ch, k) => cv.cell(x0 + EYES[k] + gaze, top + 2, ch, OUTLINE, body));
    }
    if (state === 'error' || ((mood === 'worried' || scene.has('hot')) && state !== 'done')) {
      const s = mod(fd(tick, 5), 4);
      if (s < 3) cv.cell(x0 + OW, top + 1 + s, "',."[s], SWEAT);
    }
    if (cheek && !opened) {
      cv.cell(x0 + 2, top + 4, cheek, CHEEK, mbg);
      cv.cell(x0 + 10, top + 4, cheek, CHEEK, mbg);
    }
    if (mood === 'sad' && idle) {
      const s = mod(fd(tick, 6), 3);
      if (s < 2) cv.cell(x0 + EYES[0] + gaze, top + 4 + s, ",'"[s], SWEAT, mbg);
    }
  }

  moodMarks(cv, state, tick, ox, top, mood, scene, fidget) {
    const idle = state === 'idle';
    const t = mod(tick, 90) - 72;
    if (idle && mood === 'sleepy') {
      const s = mod(tick, 36);
      for (let j = 0; j < 3; j++) {
        if (fd(s, 9) > j) cv.cell(ox + OW + 1 + j, top + 1 - j, 'zzZ'[j], j < 2 ? TEXT_DIM : TEXT);
      }
    } else if (idle && mood === 'curious' && mod(fd(tick, 5), 4)) {
      cv.cell(ox + OW + 1, top, '?', SUN);
    } else if (idle && mood === 'excited' && mod(fd(tick, 3), 4) < 2) {
      cv.cell(ox - 1, top, '!', SUN);
      cv.cell(ox + OW + 1, top, '!', SUN);
    } else if (mood === 'excited' && ['thinking', 'tool', 'writing', 'done'].includes(state)) {
      if (mod(fd(tick, 3), 4) === 0) cv.cell(ox + OW + 1, top, '+', SUN);
    }
    if (fidget === 'balloon' && t >= 2) {
      cv.put(ox - 1, top - 2, ' Hi! ', INK, false, BALLOON);
      cv.cell(ox + 1, top - 1, 'V', BALLOON);
    } else if (fidget === 'wave') {
      const arm = '\\/'[mod(fd(tick, 3), 2)];
      cv.cell(ox + OW, top + 3, 'o', BODY);
      cv.cell(ox + OW + 1, top + 2, arm, BODY);
    }
    if (scene.has('music')) {
      for (let j = 0; j < 2; j++) {
        const s = mod(tick + j * 12, 24);
        if (s < 20) cv.cell(ox - 2 - fd(s, 6) - j * 2, top + 5 - fd(s, 5), 'db'[j], NOTE[j]);
      }
    }
    if (scene.has('cold') && idle) {
      const s = mod(tick, 40);
      if (s < 12) cv.cell(ox + OW + 1 + fd(s, 4), top + 4 - fd(s, 6), '.oO'[Math.min(2, fd(s, 4))], SNOW);
    }
  }

  // ---------- things beside it ----------

  window(cv, x, y, w, h, title, body = WIN_BODY) {
    cv.fill(x, y, w, h, body);
    cv.fill(x, y, w, 1, TITLE);
    cv.put(x + 1, y, title.slice(0, Math.max(0, w - 6)), WHITE, false, TITLE);
    cv.cell(x + w - 4, y, '-', WHITE, TITLE);
    cv.cell(x + w - 2, y, 'x', WHITE, CLOSE);
  }

  hourglass(cv, tick, x, y) {
    const frame = ['._____.', '\\     /', ' \\   / ', '  ) (  ', ' /   \\ ', '/     \\', "'-----'"];
    frame.forEach((r, i) => cv.put(x, y + i, r, GLASS));
    const topCells = [[1, 1], [1, 5], [1, 2], [1, 4], [1, 3], [2, 2], [2, 4], [2, 3]];
    const bottomCells = [[5, 3], [5, 2], [5, 4], [5, 1], [5, 5], [4, 3], [4, 2], [4, 4]];
    const t = mod(tick, 40);
    const n = Math.min(8, fd(t, 4));
    topCells.forEach(([r, c], k) => {
      if (k >= n) cv.cell(x + c, y + r, ':', SAND);
    });
    bottomCells.forEach(([r, c], k) => {
      if (k < n) cv.cell(x + c, y + r, '.', SAND);
    });
    if (n < 8 && mod(tick, 2)) cv.cell(x + 3, y + 3, ':', SAND);
    cv.put(x - 3, y + 7, ('Please wait' + '.'.repeat(mod(fd(tick, 4), 4))).padEnd(14), TEXT_DIM, true);
  }

  doc(cv, tick, x, y, stream = false, pencil = false, title = 'Notes') {
    this.window(cv, x, y, 16, 7, title, PAGE);
    const lines = this.stream;
    let li;
    let col;
    if (stream) {
      const total = mod(tick, lines.length * 12);
      li = fd(total, 12);
      col = mod(total, 12);
    } else {
      li = fd(tick, 6);
      col = 12;
    }
    for (let k = 0; k < 5; k++) {
      const j = li - 4 + k;
      if (j < 0) continue;
      const text = j < li ? lines[mod(j, lines.length)] : lines[mod(j, lines.length)].slice(0, col);
      cv.put(x + 2, y + 1 + k, text, k === 4 ? INK : PAGE_DIM, false, PAGE);
    }
    if (pencil) {
      const px = x + 2 + Math.min(col, 11);
      cv.put(px, y + 4, '/', '#a04400', false, PAGE);
      cv.put(px + 1, y + 3, '/', SAND, false, PAGE);
    }
  }

  pages(cv, tick, ox, top, px, py) {
    const x0 = ox + OW - 1;
    const y0 = top + 4;
    const x1 = px;
    const y1 = py + 3;
    for (let k = 0; k < 2; k++) {
      const s = mod(tick + k * 9, 18) / 18.0;
      const x = int(pyRound(x0 + (x1 - x0) * s));
      const y = int(pyRound(y0 + (y1 - y0) * s - 2.5 * Math.sin(s * Math.PI)));
      cv.cell(x, y, '=', PAGE_DIM, PAGE);
    }
  }

  check(cv, tick, x, y) {
    cv.fill(x, y + 2, 15, 3, BALLOON);
    cv.put(x + 1, y + 2, '(i) Folio', '#0f3c96', false, BALLOON);
    cv.put(x + 1, y + 3, 'v/', GOOD, false, BALLOON);
    cv.put(x + 4, y + 3, 'All filed!', INK, false, BALLOON);
    cv.put(x + 13, y + 2, 'x', PAGE_DIM, false, BALLOON);
    cv.cell(x + 1, y + 5, '/', BALLOON);
    const r = lcg(103, fd(tick, 3));
    for (let k = 0; k < 4; k++) {
      const sx = x + int(r() * 15);
      const sy = y + int(r() * 7);
      if (cv.get(sx, sy) === ' ') cv.cell(sx, sy, '+*.'[int(r() * 3)], SHINE);
    }
  }

  error(cv, tick, x, y) {
    this.window(cv, x, y + 1, 16, 5, 'Error');
    const on = mod(fd(tick, 4), 2);
    cv.put(x + 1, y + 2, '(x)', on ? BAD : '#b04a40', false, WIN_BODY);
    cv.put(x + 5, y + 2, 'Oops!', TEXT, false, WIN_BODY);
    cv.put(x + 5, y + 3, 'Try again?', TEXT_DIM, false, WIN_BODY);
    cv.put(x + 6, y + 5, '[ OK ]', INK, false, PAGE);
  }

  sparkles(cv, tick, x, y, n = 5) {
    for (let j = 0; j < n; j++) {
      const r = lcg(107, j);
      const period = 24 + int(r() * 16);
      const t = mod(tick + int(r() * period), period);
      const sx = x + int(r() * 12);
      const sy = y - int(r() * 5);
      if (t < fd(period * 2, 3)) cv.cell(sx, sy, '*+.'[mod(fd(t, 4) + j, 3)], [SUN, SHINE, CLOUD][j % 3]);
    }
  }

  prop_bash(cv, tick, x, y) {
    this.window(cv, x, y, 16, 7, 'Command', CMD_BODY);
    const cyc = fd(tick, 30);
    const cmd = CMDS[mod(cyc, CMDS.length)];
    const n = Math.min(cmd.length, fd(mod(tick, 30), 2));
    for (let k = 0; k < 3; k++) {
      const prev = CMDS[mod(cyc - 3 + k, CMDS.length)];
      cv.put(x + 1, y + 1 + k, ('C:\\>' + prev).slice(0, 14), TEXT_DIM, false, CMD_BODY);
    }
    cv.put(x + 1, y + 5, ('C:\\>' + cmd.slice(0, n)).slice(0, 14), TEXT, false, CMD_BODY);
    if (mod(fd(tick, 3), 2)) cv.cell(x + 5 + n, y + 5, '_', TEXT, CMD_BODY);
  }

  prop_book(cv, tick, x, y) {
    this.doc(cv, tick, x, y, false, false, 'Reading');
    const hot = mod(fd(tick, 3), 5);
    for (let c = 0; c < 14; c++) cv.recolour(x + 1 + c, y + 1 + hot, WHITE, SEL);
  }

  prop_pad(cv, tick, x, y) {
    this.doc(cv, tick, x, y, true, true, 'Editing');
  }

  prop_lens(cv, tick, x, y) {
    this.window(cv, x, y, 16, 7, 'Search');
    cv.fill(x + 1, y + 1, 14, 1, PAGE);
    const q = 'files';
    const n = Math.min(q.length, fd(mod(tick, 40), 3));
    cv.put(x + 2, y + 1, q.slice(0, n), INK, false, PAGE);
    const hit = mod(fd(tick, 5), 4);
    ['notes.txt', 'luna.css', 'photo.png', 'todo.txt'].forEach((f, k) => {
      cv.put(x + 4, y + 2 + k, f.padEnd(11), k === hit ? WHITE : TEXT_DIM, false, k === hit ? SEL : WIN_BODY);
    });
    cv.put(x + 1, y + 2 + hit, 'O-', SUN, false, WIN_BODY);
  }

  prop_globe(cv, tick, x, y) {
    this.window(cv, x, y, 16, 7, 'Download');
    cv.put(x + 1, y + 2, ['(@)', '(o)', '(@)', '(O)'][mod(fd(tick, 4), 4)], RAIN, false, WIN_BODY);
    cv.put(x + 5, y + 2, 'page.html', TEXT, false, WIN_BODY);
    const pct = mod(tick * 2, 101);
    cv.put(x + 5, y + 3, `${pct}%`.padEnd(5), TEXT_DIM, false, WIN_BODY);
    const bx = x + 1;
    const by = y + 5;
    const bw = 14;
    cv.fill(bx, by, bw, 1, TROUGH);
    const n = int(pct / 100.0 * bw);
    for (let c = 0; c < n; c++) if (c % 3 !== 2) cv.recolour(bx + c, by, null, BAR);
  }

  helpers(cv, tick) {
    [Math.max(0, this.cx - 7), Math.min(this.cols - 6, this.cx + COMP_W + 1)].forEach((x, n) => {
      const y = this.ground - 4 - (Math.sin(tick / 5.0 + n * 2) > 0 ? 1 : 0);
      const body = [BODY, '#b8963a'][n];
      cv.put(x, y, ' ._  ', OUTLINE);
      cv.put(x, y + 1, '|o o|', INK, false, body);
      cv.cell(x, y + 1, '|', OUTLINE, body);
      cv.cell(x + 4, y + 1, '|', OUTLINE, body);
      cv.put(x, y + 2, "'---'", OUTLINE);
    });
    const px = this.cx + OW + 2;
    const py = this.ground - 8;
    this.window(cv, px, py, 16, 5, 'Helpers');
    const k = mod(fd(tick, 8), 3);
    cv.put(px + 1, py + 2, ('asking' + '.'.repeat(k)).padEnd(10), TEXT, false, WIN_BODY);
    cv.put(px + 1, py + 3, '2 folders', TEXT_DIM, false, WIN_BODY);
  }

  prop_todo(cv, tick, x, y) {
    this.window(cv, x, y, 16, 5, 'To Do');
    const done = 1 + mod(fd(tick, 12), TODO.length);
    TODO.forEach((item, k) => {
      const mark = k < done ? '[x]' : '[ ]';
      cv.put(x + 1, y + 1 + k, mark, k < done ? '#a8e890' : TEXT_DIM, false, WIN_BODY);
      cv.put(x + 5, y + 1 + k, item, k < done ? TEXT_DIM : TEXT, false, WIN_BODY);
    });
  }

  prop_wand(cv, tick, x, y) {
    cv.put(x + 1, y + 6, '*', SUN);
    cv.put(x + 2, y + 5, '/', CLOUD);
    this.sparkles(cv, tick, x + 2, y + 4, 6);
  }
}
Mascot.BUBBLES = {
  idle: "Hi, I'm Folio! What shall we work on?",
  thinking: 'Please wait…',
  writing: 'Writing it up…',
  done: "Done! Everything's filed ✓",
  error: "Hmm, that didn't work. Want to try again?",
};
Mascot.TOOL_BUBBLES = {
  Bash: 'Running a command…',
  Read: 'Opening the file…',
  Edit: 'Saving changes…',
  MultiEdit: 'Saving changes…',
  Write: 'Saving changes…',
  NotebookEdit: 'Saving changes…',
  Grep: 'Searching your files…',
  Glob: 'Searching your files…',
  WebSearch: 'Searching the web…',
  WebFetch: 'Downloading…',
  Task: 'Asking a few helpers…',
  Agent: 'Asking a few helpers…',
  TodoWrite: 'Updating your to-do list…',
};
Mascot.OTHER = 'Working with %s…';
Mascot.moods = true;

export { Mascot };

const theme = makeTheme({
  meta: { id: 'luna', name: 'Luna', panel: PANEL },
  Mascot,
  say: (topic) => word(TOPIC_LINES, topic),
  emoteColours: EMOTE_COLOURS,
});

export const { meta, lines, spans, render, bubble, say, emoteColours } = theme;
