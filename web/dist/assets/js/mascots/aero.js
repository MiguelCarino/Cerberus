// The aero theme's mascot: a glossy glass orb buddy floating over rolling green
// hills, under a Windows 7 sky with an aurora. A faithful port of Atrium's
// mascot_aero.py: the same (state, tick, tool, mood, scene, cols, rows) draws
// the same frame, cell for cell and colour for colour (golden tests:
// web/test/mascots-themes-a.test.mjs).
//
//   sky     an aurora: two ribbons of ~ along a slow wave over a glow of cell
//           backgrounds whose hue slides along them, and cotton clouds.
//   hills   two rolling ranges along the bottom, painted as cell backgrounds
//           under an edge of slope glyphs, grass tufts on the near one.
//   the orb 15 x 8 cells of Aero glass: lighter in its top half than its
//           bottom, a shine at its top left, a friendly face; it bobs and its
//           shadow on the hill grows and shrinks.
//   props   small glass windows to its right: cmd for Bash, a document for
//           Read, the document and a pencil for Edit/Write, the search box for
//           the searches, a globe and the green progress bar for WebFetch, two
//           little orbs for Task/Agent, a checklist for TodoWrite, rising
//           bubbles otherwise; thinking is the busy ring, done a green check,
//           an error a red tint and the amber warning triangle.
//   mood    the whole face idle, accents at work; an idle fidget (look, blown
//           bubble, gloss glint, Messenger nudge, yawn).
//   scene   day sky and sun flare, night stars and moon, clouds, rain, snow,
//           fog, heat, frost, music, a controller (game), party hat, papel
//           picado, an alarm clock gadget (mood.js SCENES).

import { BaseMascot, Canvas, STATES, blink, lcg, propKind, streamLines, fd, mod, fmod, int, makeTheme }
  from './common.js';
import { MOODS, word } from './mood.js';

export const PANEL = '#12325a';

const SKY = '#8cc8ff';
const MIST = '#c4d8ee';
const SHINE = '#eef6ff';
const GLASS_HI = '#3f86cf';
const GLASS_LO = '#22599c';
const GLASS_RED_HI = '#b04a5a';
const GLASS_RED_LO = '#7c2a3a';
const GREEN = '#8fe07a';
const GREEN_DARK = '#2f7a3a';
const FAR_HILL = '#1d5660';
const FAR_EDGE = '#5fa8b0';
const AMBER = '#ffc46b';
const BAD = '#ff6b78';
const WIN_BAR = '#2f68a8';
const WIN_BODY = '#0f2848';
const CMD_BODY = '#070c14';
const TEXT = '#e4eefa';
const TEXT_DIM = '#8fa8c4';
const SEL = '#2f6db5';
const BAR_GREEN = '#3a9a3a';
const BAR_SHINE = '#7fd46f';
const AURORA = ['#6cf0c8', '#8fe07a', '#7fd0ff', '#b99cff', '#7fd0ff', '#8fe07a'];
const AURORA_GLOW = ['#123c54', '#13424a', '#15395e', '#1c3460', '#15395e', '#13424a'];
const AURORA_DIM = '#1a3550';
const DAY_SKY = ['#1d5490', '#1b4d86', '#19467c', '#173f72'];
const DAY_GLOW = ['#1f5a96', '#21609c', '#235f9a', '#1f5a96'];
const GLASS_DIM_HI = '#3d6a9a';
const GLASS_DIM_LO = '#284b74';
const BLUSH = '#ff9cc0';
const SUN = '#ffe08a';
const MOON = '#f3e3a8';
const STAR = '#c4d8ee';
const STAR_DIM = '#5f7fa4';
const CLOUD_GREY = '#7f97b3';
const CLOUD_STORM = '#5f7894';
const RAIN = '#8cc8ff';
const SNOW = '#f2f8ff';
const FOG = '#5a7898';
const NOTE = ['#ffe08a', '#8fe07a'];
const CONFETTI = ['#ff6fae', '#ffc46b', '#8fe07a', '#7fd0ff', '#b99cff'];
const PICADO = ['#ff6fae', '#ffa94d', '#7fd46f', '#6fb6ff', '#c58bff', '#ffd84d'];
const OVERCAST = ['cloud', 'rain', 'storm', 'snow', 'fog'];

const OW = 15;
const OH = 8;
const ORB = [
  '    _.---._    ',
  "  .'       '.  ",
  ' /           \\ ',
  '|             |',
  '|             |',
  ' \\           / ',
  "  '._     _.'  ",
  "     '---'     ",
];
const EYES = [4, 10];
const MOUTH = [4, 7];

const COMP_W = OW + 2 + 17;
const WORDS = ['the', 'sky', 'is', 'clear', 'and', 'all', 'set', 'your', 'file', 'ok', 'to',
  'glass', 'now', 'done', 'a', 'it', 'works', 'so'];
const CMDS = ['dir', 'ver', 'ping', 'cls', 'tree', 'ipconfig'];
const TODO = ['Back up', 'Update', 'Tidy up', 'Share'];

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
  calm: [['look', 2], ['bubble', 2], ['glint', 2], ['nudge', 1], ['rest', 2]],
  happy: [['bubble', 3], ['glint', 2], ['nudge', 1]],
  excited: [['nudge', 3], ['bubble', 2], ['glint', 2]],
  shy: [['look', 2], ['rest', 3]],
  curious: [['look', 4], ['bubble', 1]],
  focused: [['rest', 3], ['glint', 1]],
  worried: [['look', 3], ['rest', 2]],
  sad: [['rest', 1]],
  sleepy: [['yawn', 3], ['rest', 2]],
};

export const MOOD_BUBBLES = {
  happy: "Everything is in sync ✓ What's next?",
  excited: "Wow! Let's make something amazing today!",
  shy: 'Oh! You nudged me… hi there ☺',
  curious: "Ooh, what's that? Tell me more!",
  focused: 'Status: Busy. Ready when you are.',
  worried: "Hmm… let's run a quick check-up.",
  sad: 'Feeling a little cloudy today…',
  sleepy: 'Status: Away… zz',
};
export const TOPIC_LINES = {
  thanks: "You're welcome! Glad I could help ✓",
  worried: "Uh-oh… let's troubleshoot that.",
  frustrated: "Still not working? Let's try once more!",
  relieved: 'Phew! Problem solved ✓',
  determined: 'Big job! Hang tight, still in progress…',
  proud: 'All done! Everything is up to date ✓',
  sad: 'Sorry about that…',
  happy: 'Awesome! ✓',
  curious: 'Ooh, interesting…',
  rain: 'The weather gadget says rain. Grab an umbrella!',
  storm: 'Storm warning! Better stay inside.',
  snow: "It's snowing! ❄",
  fog: 'Foggy out there. Take it slow!',
  sun: 'Clear skies and sunshine today ☀',
  hot: 'Toasty out there: {0}. Stay cool!',
  cold: 'Brr, {0} outside. Bundle up!',
  gpu_hot: 'Your GPU is running warm: {0}°C',
  cpu_hot: 'Your CPU is running warm: {0}°C',
  music: 'Now Playing… great song! ♪',
  game: 'Game on! Have fun!',
  event: 'Reminder: {0} in {1} min',
  event_now: 'Reminder: {0} is starting now!',
  birthday: 'Happy birthday, {0}! 🎂',
  holiday: 'Today is {0}! 🎉',
  go_home: "It's {0}. Time to sign out and head home!",
  bedtime: "It's {0}… time to shut down for the night.",
  morning: 'Good morning! Your desktop is ready ☀',
  lunch: "Lunch break? You've earned it!",
  weekend: "It's the weekend! Enjoy ✓",
  friday: 'Friday afternoon! Weekend sync in progress ✓',
  hello: ['Hi there! Ready when you are ✓',
    'Hello! Fresh desktop, clear skies ☀',
    "Welcome! Let's make something shiny"],
  welcome_back: ['Welcome back! All right where you left it ✓',
    "You're back! Resuming your session…"],
  wake: ["Oh! Waking from sleep mode… I'm up!",
    'Resuming… hi! I was only dimmed'],
  long_prompt: ['Big one! Loading all of it…',
    "Whoa, that's a lot. Reading it all!"],
  code: ['Code! Let me take a clear look',
    'Ooh, code. Opening it in glass view'],
  image: ['A picture! Opening Photo Viewer…',
    'Ooh, nice image. Let me see!'],
  aborted: ['Stopped! Ready for the next one ✓',
    'Cancelled. No harm done!'],
  tests_pass: ['All tests passed ✓ Looking good!',
    'Green across the board ✓',
    'Tests: all clear skies ☀'],
  tests_fail: ["Some tests failed… let's take a look",
    "Tests didn't pass. We'll fix it!"],
  commit: ['Changes saved and synced ✓',
    'Committed! Your work is backed up ✓',
    'Shipped! Off it floats to the cloud ☁'],
  fast: ['Done in a blink ✓',
    'That was quick! ✓',
    'Speedy! Already finished ✓'],
  busy: ['Lots of tasks running… on it!',
    'Busy busy! Multitasking like a pro'],
  model: ['Switched to {0} ✓',
    'Now running on {0}. Shiny!'],
  poke: ['Hi! That tickles the glass ☺',
    'Boop! Still shiny ✓',
    'Hey there! Need anything?',
    'Ooh, a click! Hello ✓',
    '*wobbles* Wheee!',
    'You found me! Hi ☺'],
  poke_spam: ["Whoa, easy! You'll smudge the glass",
    'Too many clicks! Glass overheating…',
    "Hey! I'm getting fingerprints!"],
  pet: ['Aww… polishing my glow ✓',
    "Hehe, that's nice ☺",
    'So smooth… thank you!',
    'Mmm, extra gloss ✓'],
  rg_memory: '{0} was using too much memory, so I closed it ✓',
  rg_vram: '{0} filled the graphics memory, so I closed it ✓',
  rg_game_vram: '{0} is using almost all graphics memory. Lower its textures?',
  rg_panic: 'Done! {0} has been closed ✓',
  rg_stuck: "Memory is running low… I couldn't find what to close.",
  rg_agents: 'All agent tasks stopped. Your sessions are fine ✓',
};
export const EMOTE_COLOURS = {
  accent: '#7fd0ff', warn: AMBER, bad: '#ffa0a8', good: GREEN, love: BLUSH, dim: '#a8bcd4',
};

const has = (set, ...tags) => tags.some((t) => set.has(t));

// pyRound is Python's round(x) to an int: halves go to the even neighbour.
function pyRound(x) {
  const r = Math.round(x);
  return Math.abs(x % 1) === 0.5 ? 2 * Math.round(x / 2) : r;
}

// edges gives the first and one-past-last non-space index of s (Python's
// len(s) - len(s.lstrip(" ")) and len(s.rstrip(" "))).
function edges(s) {
  let a = 0;
  while (a < s.length && s[a] === ' ') a++;
  let b = s.length;
  while (b > 0 && s[b - 1] === ' ') b--;
  return [a, b];
}

// hill is the height above the bottom row of a hill range at column x.
function hill(x, freq, amp, seed) {
  const r = lcg(seed);
  const a = r() * 6.28;
  const b = r() * 6.28;
  return amp * (1 + 0.6 * Math.sin(x * freq + a) + 0.4 * Math.sin(x * freq * 2.3 + b));
}

export class Mascot extends BaseMascot {
  constructor(cols = 64, rows = 18) {
    super(cols, rows);
    this.cols = Math.max(int(cols), COMP_W + 2);
    this.rows = Math.max(int(rows), OH + 6);
    this.cx = fd(this.cols - COMP_W, 2);
    this.ground = this.rows - 3;
    this.hn = Array.from({ length: this.cols }, (_, x) => int(hill(x, 0.21, 1.3, 7)));
    this.hf = Array.from({ length: this.cols }, (_, x) => int(hill(x, 0.13, 1.6, 3) + 1.4));
    this.tufts = Array.from({ length: this.cols }, (_, x) => lcg(89, x)());
    this.stream = streamLines(WORDS, 12, 61);
    this.clouds = [];
    for (let i = 0; i < Math.max(2, fd(this.cols, 30)); i++) {
      const r = lcg(79, i, this.cols);
      const x = r() * this.cols;
      const y = int(r() * Math.max(2, fd(this.rows, 3)));
      this.clouds.push([x, y, 0.02 + r() * 0.03]);
    }
    this.grey = [];
    for (let i = 0; i < Math.max(2, fd(this.cols, 26)); i++) {
      const r = lcg(83, i, this.cols);
      const x = r() * this.cols;
      const y = int(r() * Math.max(2, fd(this.rows, 3))) - 1;
      this.grey.push([x, y, 0.03 + r() * 0.03]);
    }
    this.stars = [];
    for (let i = 0; i < fd(this.cols * this.rows, 40); i++) {
      const r = lcg(97, i);
      const x = int(r() * this.cols);
      const y = int(r() * Math.max(2, this.ground - 6));
      const period = 20 + int(r() * 30);
      this.stars.push([x, y, period, int(r() * 50)]);
    }
  }

  bubble(state, tool = null, mood = null) {
    if (state === 'idle' && Object.hasOwn(MOOD_BUBBLES, mood || '')) return MOOD_BUBBLES[mood];
    return super.bubble(state, tool, mood);
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
    this.hills(cv, tick, scene);
    if (scene.has('fog')) this.fog(cv, tick);
    if (scene.has('snow')) this.snow(cv, tick);
    if (has(scene, 'rain', 'storm')) this.weatherRain(cv, tick, scene.has('storm'));
    if (scene.has('hot')) this.shimmer(cv, tick);
    if (scene.has('party')) this.confetti(cv, tick);
    let hover;
    if (state === 'done') hover = [0, 1, 2, 2, 1, 0, 0, 0][mod(tick, 8)];
    else if (idle && mood === 'excited' && mod(fd(tick, 30), 2)) hover = [0, 1, 2, 2, 1, 0][mod(tick, 6)];
    else if (idle && ['sad', 'sleepy'].includes(mood)) hover = 0;
    else if (scene.has('music') && state !== 'error') hover = mod(fd(tick, 4), 2);
    else hover = Math.sin(tick / 7.0) > 0 ? 1 : 0;
    const top = this.ground - OH - hover;
    let ox = this.cx;
    if (fidget === 'nudge' && mod(tick, 90) < 80) ox += [0, 1, 0, -1][mod(tick, 4)];
    this.shadow(cv, ox, hover);
    if (kind === 'helpers') this.helpers(cv, tick);
    if (scene.has('clock')) this.alarm(cv, tick, kind === 'helpers');
    this.orb(cv, state, tick, ox, top, mood, scene, fidget);
    this.head = [ox + 12, Math.max(0, top)];
    this.moodMarks(cv, state, tick, ox, top, mood, scene, fidget);
    if (scene.has('party')) this.hat(cv, tick, ox, top);
    const px = this.cx + OW + 2;
    const py = this.ground - 8;
    if (state === 'thinking') this.ring(cv, tick, px + 4, py + 2);
    else if (state === 'writing') this.doc(cv, tick, px, py, true, true);
    else if (state === 'done') this.check(cv, tick, px, py);
    else if (state === 'error') this.warning(cv, tick, px, py);
    else if (kind && kind !== 'helpers') PROPS[kind].call(this, cv, tick, px, py);
    else if (kind === 'helpers') { /* the helpers drew their own window */ }
    else if (scene.has('game')) this.controller(cv, tick, px, py);
    else this.bubbles(cv, tick, px + 2, py + 6, 2);
    return cv;
  }

  fidget(tick, mood) {
    if (mod(tick, 90) < 72) return null;
    const table = FIDGETS[mood] || FIDGETS.calm;
    let pick = lcg(109, fd(tick, 90))() * table.reduce((a, [, w]) => a + w, 0);
    for (const [name, weight] of table) {
      pick -= weight;
      if (pick < 0) return name === 'rest' ? null : name;
    }
    return null;
  }

  // ---------- the sky and the land ----------

  sky(cv, state, tick, scene) {
    const overcast = has(scene, ...OVERCAST);
    const clear = !overcast && state !== 'error';
    if (scene.has('day') && !overcast) {
      DAY_SKY.forEach((bg, y) => {
        for (let x = 0; x < this.cols; x++) cv.recolour(x, y, null, bg);
      });
    }
    this.aurora(cv, state, tick, scene);
    const y0 = scene.has('festive') ? 2 : 0;
    if (scene.has('night')) {
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
    } else if (clear && has(scene, 'day', 'sun') && this.cols >= 48) {
      this.sun(cv, tick, y0);
    }
    this.cloudLayer(cv, tick, scene);
    if (scene.has('storm')) this.lightning(cv, tick);
  }

  aurora(cv, state, tick, scene) {
    const bands = this.rows >= 17 ? [[1, 0.16, 0.0], [3, 0.10, 2.0]] : [[0, 0.16, 0.0], [2, 0.10, 2.0]];
    const overcast = has(scene, ...OVERCAST);
    const dim = state === 'error' || overcast;
    const day = scene.has('day') && !dim;
    let dense = 0.55;
    if (day) dense = 0.18;
    else if (overcast && state !== 'error') dense = 0.3;
    else if (scene.has('night') && !dim) dense = 0.7;
    bands.forEach(([row, freq, ph], n) => {
      for (let x = 0; x < this.cols; x++) {
        const y = row + int(pyRound(Math.sin(x * freq + ph + tick * 0.04) * 1.2));
        const k = mod(fd(x, 4) + fd(tick, 5) + n * 2, AURORA_GLOW.length);
        const glow = dim ? AURORA_DIM : (day ? DAY_GLOW[k % DAY_GLOW.length] : AURORA_GLOW[k]);
        if (y >= 0 && y < this.rows) cv.recolour(x, y, null, glow);
        if (lcg(101, x, row)() < dense && y >= 0 && y < this.rows) {
          const colour = dim ? TEXT_DIM : (day ? MIST : AURORA[mod(fd(x, 3) + fd(tick, 4) + row, AURORA.length)]);
          cv.cell(x, y, mod(x + fd(tick, 6), 5) ? '~' : '-', colour);
        }
      }
    });
  }

  cloudLayer(cv, tick, scene) {
    const overcast = has(scene, ...OVERCAST);
    const grey = scene.has('storm') ? CLOUD_STORM : CLOUD_GREY;
    if (overcast) {
      for (const [x0, y0, speed] of this.grey) {
        const x = mod(int(x0 + tick * speed), this.cols + 16) - 12;
        const y = y0 + 2;
        cv.sprite(x + 2, y, '.--.  .-.', grey);
        cv.sprite(x, y + 1, "(`    )'  `-.", grey);
        cv.sprite(x, y + 2, '(___________)', grey);
      }
    }
    const colour = !overcast || scene.has('day') ? MIST : CLOUD_GREY;
    for (const [x0, y0, speed] of this.clouds) {
      const x = mod(int(x0 + tick * speed), this.cols + 12) - 8;
      const y = y0 + 2;
      cv.sprite(x + 1, y, '.--.', colour);
      cv.sprite(x, y + 1, '(    )_', colour);
      cv.sprite(x + 1, y + 2, "`----'", colour);
    }
  }

  hills(cv, tick, scene) {
    const bottom = this.rows - 1;
    const snow = scene.has('snow');
    for (const [heights, fill, edge] of [[this.hf, FAR_HILL, FAR_EDGE], [this.hn, GREEN_DARK, GREEN]]) {
      for (let x = 0; x < this.cols; x++) {
        const h = heights[x];
        const top = bottom - h;
        for (let y = Math.max(0, top + 1); y < this.rows; y++) cv.cell(x, y, ' ', null, fill);
        const left = x ? heights[x - 1] : h;
        const right = x + 1 < this.cols ? heights[x + 1] : h;
        let glyph;
        if (left < h && h <= right) glyph = '/';
        else if (right < h && h <= left) glyph = '\\';
        else if (left < h && h > right) glyph = '.';
        else glyph = '_';
        cv.cell(x, top, glyph, snow ? SNOW : edge);
      }
    }
    const wind = mod(fd(tick, 6), 2);
    this.tufts.forEach((v, x) => {
      if (v < 0.2) {
        const y = bottom - this.hn[x] + 1;
        if (y < this.rows) {
          if (snow) cv.cell(x, y, v < 0.1 ? '.' : 'o', SNOW, GREEN_DARK);
          else cv.cell(x, y, v < 0.1 ? ",'"[(x + wind) % 2] : '"', BAR_SHINE, GREEN_DARK);
        }
      }
    });
  }

  shadow(cv, ox, hover) {
    const w = 9 - hover * 2;
    for (let c = 0; c < w; c++) {
      const x = ox + fd(OW - w, 2) + c;
      if (x >= 0 && x < this.cols) cv.cell(x, this.rows - 1 - this.hn[x], '_', '#173d20');
    }
  }

  // ---------- the scene ----------

  sun(cv, tick, y = 0) {
    const rays = ['\\ | /', '  |  ', '\\   /'][mod(fd(tick, 8), 3)];
    cv.put(1, y, rays, SUN);
    cv.put(1, y + 1, '-(O)-', SUN);
    cv.put(1, y + 2, rays[0] === '\\' ? '/ | \\' : '  |  ', SUN);
    [[8, 3, 'o', MIST], [11, 4, '.', SHINE], [14, 5, 'O', '#7fd0ff']].forEach(([dx, dy, ch, colour], k) => {
      if (mod(fd(tick, 6) + k, 5) && cv.get(dx, y + dy) === ' ') cv.cell(dx, y + dy, ch, colour);
    });
  }

  lightning(cv, tick) {
    const cyc = fd(tick, 47);
    const t = mod(tick, 47);
    if (t > 3) return;
    const x = int(lcg(113, cyc)() * (this.cols - 4)) + 1;
    [[1, '/'], [0, '/'], [0, '\\'], [1, '/']].forEach(([dx, ch], k) => {
      cv.cell(x + dx + (k > 1 ? 1 : 0), 1 + k, ch, t % 2 === 0 ? SUN : SHINE);
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
        if (mod(x + off, 12) < 5 && mod(x + off, 12) !== 2) cv.cell(x, y, '~', FOG);
      }
    });
  }

  snow(cv, tick) {
    const span = this.rows + 2;
    const n = Math.max(6, fd(this.cols * this.rows, 60));
    for (let i = 0; i < n; i++) {
      const r = lcg(127, i);
      const speed = 0.05 + r() * 0.06;
      const pos = r() * span + tick * speed;
      const fall = int(fd(pos, span));
      const y = int(pos - fall * span) - 1;
      const x = int(lcg(131, i, fall)() * this.cols + [0, 1, 1, 0, 0, -1, -1, 0][mod(fd(tick, 5) + i, 8)]);
      if (cv.get(x, y) === ' ') cv.cell(x, y, '*.+'[i % 3], SNOW);
    }
  }

  weatherRain(cv, tick, heavy = false) {
    const span = this.rows + 2;
    const n = fd(this.cols * this.rows, heavy ? 14 : 24);
    for (let i = 0; i < n; i++) {
      const r = lcg(137, i);
      const speed = (heavy ? 0.7 : 0.5) + r() * 0.3;
      const y = int(fmod(r() * span + tick * speed, span)) - 1;
      const x = int(r() * this.cols);
      if (cv.get(x, y) === ' ') cv.cell(x, y, i % 3 ? "'" : ',', RAIN);
    }
  }

  shimmer(cv, tick) {
    for (let x = 0; x < this.cols; x++) {
      if (lcg(139, x, fd(tick, 3))() < 0.08) {
        const y = this.ground - 1 - (x % 2);
        if (cv.get(x, y) === ' ') cv.cell(x, y, '~', AMBER);
      }
    }
  }

  confetti(cv, tick) {
    const span = this.rows + 2;
    const n = Math.max(4, fd(this.cols * this.rows, 90));
    for (let i = 0; i < n; i++) {
      const r = lcg(149, i);
      const pos = r() * span + tick * (0.12 + r() * 0.1);
      const fall = int(fd(pos, span));
      const y = int(pos - fall * span) - 1;
      const x = int(lcg(151, i, fall)() * this.cols) + mod(fd(tick, 4) + i, 2);
      if (cv.get(x, y) === ' ') cv.cell(x, y, ",'.*"[mod(fd(tick, 3) + i, 4)], CONFETTI[i % CONFETTI.length]);
    }
  }

  alarm(cv, tick, high = false) {
    const x = Math.max(0, this.cx - 8);
    const y = this.ground - (high ? 9 : 3);
    const ring = mod(fd(tick, 2), 10) < 5;
    const shake = ring ? mod(tick, 2) : 0;
    cv.sprite(x + shake, y, 'o.-.o', SKY);
    cv.cell(x + shake, y, 'o', AMBER);
    cv.cell(x + shake + 4, y, 'o', AMBER);
    cv.sprite(x + shake, y + 1, '(   )', SKY);
    for (const c of [1, 2, 3]) cv.recolour(x + shake + c, y + 1, null, GLASS_LO);
    cv.cell(x + shake + 2, y + 1, '|/-\\'[mod(fd(tick, 3), 4)], SHINE, GLASS_LO);
    cv.sprite(x + shake, y + 2, "'-^-'", SKY);
    if (ring) {
      cv.cell(x + 6, y - 1, '!', AMBER);
      cv.cell(x - 1, y, '(', AMBER);
      cv.cell(x + 6, y, ')', AMBER);
    }
  }

  hat(cv, tick, ox, top) {
    cv.cell(ox + 7, top - 3, '*+'[mod(fd(tick, 4), 2)], AMBER);
    cv.sprite(ox + 6, top - 2, '/:\\', CONFETTI[0]);
    cv.sprite(ox + 5, top - 1, '/_:_\\', CONFETTI[3]);
    cv.cell(ox + 7, top - 2, ':', AMBER);
    cv.cell(ox + 7, top - 1, ':', AMBER);
  }

  controller(cv, tick, x, y) {
    cv.sprite(x + 1, y + 2, '  .-----.  ', MIST);
    cv.sprite(x + 1, y + 3, ' / +  @ o\\ ', MIST);
    cv.sprite(x + 1, y + 4, '(   .-.   )', MIST);
    cv.sprite(x + 1, y + 5, " `-'   `-' ", MIST);
    cv.cell(x + 4, y + 3, '+', TEXT);
    cv.cell(x + 7, y + 3, '@', GREEN);
    cv.cell(x + 9, y + 3, 'o', [AMBER, BAD, GREEN, SKY][mod(fd(tick, 3), 4)]);
    cv.put(x + 2, y + 6, 'Game on!', mod(fd(tick, 6), 2) ? GREEN : BAR_SHINE, true);
  }

  // ---------- the orb ----------

  orb(cv, state, tick, x0, top, mood, scene, fidget) {
    const idle = state === 'idle';
    let hi;
    let lo;
    if (state === 'error') {
      hi = GLASS_RED_HI;
      lo = GLASS_RED_LO;
    } else if (idle && ['sad', 'sleepy'].includes(mood)) {
      hi = GLASS_DIM_HI;
      lo = GLASS_DIM_LO;
    } else {
      hi = GLASS_HI;
      lo = GLASS_LO;
    }
    ORB.forEach((r, i) => {
      cv.sprite(x0, top + i, r, state === 'error' ? BAD : SKY);
      if (i === 0 || i === 7) return;
      const [a, b] = edges(r);
      for (let c = a + 1; c < b - 1; c++) cv.recolour(x0 + c, top + i, null, i <= 3 ? hi : lo);
    });
    if (fidget === 'glint') {
      const t = mod(tick, 90) - 72;
      for (let i = 1; i < 7; i++) {
        const [a, b] = edges(ORB[i]);
        for (const c of [t - 3 + i, t - 2 + i]) {
          if (a + 1 <= c && c < b - 1) cv.recolour(x0 + c, top + i, null, '#6aa6e0');
        }
      }
    }
    cv.cell(x0 + 3, top + 2, '(', SHINE, hi);
    cv.cell(x0 + 4, top + 1, '_', SHINE, hi);
    cv.cell(x0 + 11, top + 5, ')', MIST, lo);
    if (has(scene, 'rain', 'storm')) {
      const t = mod(fd(tick, 3), 6);
      if (t < 4) cv.cell(x0 + 12, top + 2 + t, '.', SHINE, t < 2 ? hi : lo);
    }
    if (scene.has('cold')) {
      for (const [c, i, ch] of [[2, 5, '*'], [3, 6, '.'], [12, 5, '*'], [11, 6, '.']]) {
        cv.cell(x0 + c, top + i, ch, SNOW, lo);
      }
    }
    const eye = state === 'done' ? '^' : 'o';
    let gaze = ['tool', 'writing'].includes(state) ? 1 : 0;
    if (state === 'thinking') gaze = [0, 1, 0, -1][mod(fd(tick, 12), 4)];
    let mouth = { done: 'D', error: '~', thinking: 'o' }[state] ?? 'v';
    if (state === 'writing') mouth = 'o-o-v-'[mod(fd(tick, 2), 6)];
    let left = eye;
    let right = eye;
    let cheek = ['done', 'idle'].includes(state) ? '~' : '';
    if (idle && Object.hasOwn(MOOD_FACE, mood)) {
      [left, right, mouth, cheek] = MOOD_FACE[mood];
      if (mood === 'shy') gaze = -1;
      else if (mood === 'curious') gaze = 1;
      else if (mood === 'worried') gaze = [0, -1, 0, 1][mod(fd(tick, 8), 4)];
    } else if (mood === 'shy' && state !== 'error') {
      cheek = '/';
    } else if (['happy', 'excited'].includes(mood) && state !== 'error') {
      cheek = '~';
      if (mood === 'excited' && state !== 'done' && mod(tick, 20) < 3) {
        left = '*';
        right = '*';
      }
    }
    const t = mod(tick, 90) - 72;
    if (fidget === 'look') {
      gaze = t < 9 ? -1 : 1;
    } else if (fidget === 'yawn') {
      left = '-';
      right = '-';
      mouth = t >= 3 && t < 14 ? 'O' : 'o';
    } else if (fidget === 'bubble') {
      mouth = 'o';
    }
    const shut = blink(tick, 49) && !['done', 'error'].includes(state);
    const lit = mood === 'excited' && state !== 'error' ? AMBER : SHINE;
    [left, right].forEach((ch, k) => cv.cell(x0 + EYES[k] + gaze, top + 3, shut ? '-' : ch, lit, hi));
    cv.cell(x0 + MOUTH[1] + gaze, top + MOUTH[0], mouth, SHINE, lo);
    if (Object.hasOwn(BROWS, mood) && state !== 'done') {
      BROWS[mood].forEach((ch, k) => cv.cell(x0 + EYES[k] + gaze, top + 2, ch, MIST, hi));
    }
    if (state === 'error') {
      if (mod(fd(tick, 4), 3)) cv.cell(x0 + 12, top + 2, "'", SKY, hi);
    } else if ((mood === 'worried' || scene.has('hot')) && state !== 'done') {
      const s = mod(fd(tick, 5), 4);
      if (s < 3) cv.cell(x0 + 12, top + 2 + (s > 1 ? 1 : 0), "',"[s > 1 ? 1 : 0], SKY, hi);
    }
    if (cheek) {
      const colour = cheek === '/' ? BLUSH : '#9fc4f5';
      cv.cell(x0 + 2, top + 4, cheek, colour, lo);
      cv.cell(x0 + 12, top + 4, cheek, colour, lo);
    }
    if (mood === 'sad' && ['idle', 'error'].includes(state)) {
      const s = mod(fd(tick, 6), 3);
      if (s < 2) cv.cell(x0 + EYES[0] + gaze, top + 4 + s, ",'"[s], RAIN, lo);
    }
  }

  moodMarks(cv, state, tick, ox, top, mood, scene, fidget) {
    const idle = state === 'idle';
    const t = mod(tick, 90) - 72;
    if (idle && mood === 'sleepy') {
      const s = mod(tick, 36);
      for (let j = 0; j < 3; j++) {
        if (fd(s, 9) > j) cv.cell(ox + 13 + j, top + 1 - j, 'zzZ'[j], j < 2 ? MIST : SHINE);
      }
    } else if (idle && mood === 'curious' && mod(fd(tick, 5), 4)) {
      cv.cell(ox + 13, top, '?', AMBER);
    } else if (idle && mood === 'excited' && mod(fd(tick, 3), 4) < 2) {
      cv.cell(ox + 1, top, '!', AMBER);
      cv.cell(ox + 13, top, '!', AMBER);
    } else if (mood === 'excited' && ['thinking', 'tool', 'writing', 'done'].includes(state)) {
      if (mod(fd(tick, 3), 4) === 0) cv.cell(ox + 13, top, '+', AMBER);
    }
    if (fidget === 'bubble') {
      cv.cell(ox - 1 - fd(t, 6), top + 4 - fd(t, 3), t < 9 ? 'o' : 'O', MIST);
    } else if (fidget === 'nudge' && t < 8) {
      cv.cell(ox - 2, top + 3, '(', MIST);
      cv.cell(ox + OW + 1, top + 3, ')', MIST);
    }
    if (scene.has('music') && state !== 'error') {
      for (let j = 0; j < 2; j++) {
        const s = mod(tick + j * 12, 24);
        if (s < 20) cv.cell(ox - 2 - fd(s, 6) - j * 2, top + 5 - fd(s, 5), 'db'[j], NOTE[j]);
      }
    }
    if (scene.has('cold') && idle) {
      const s = mod(tick, 40);
      if (s < 12) cv.cell(ox + 14 + fd(s, 4), top + 4 - fd(s, 6), '.oO'[Math.min(2, fd(s, 4))], SNOW);
    }
  }

  // ---------- things beside it ----------

  window(cv, x, y, w, h, title, body = WIN_BODY) {
    cv.fill(x, y, w, h, body);
    cv.fill(x, y, w, 1, WIN_BAR);
    cv.put(x + 1, y, title.slice(0, w - 5), TEXT, false, WIN_BAR);
    cv.put(x + w - 2, y, 'x', TEXT, false, '#c8404c');
  }

  ring(cv, tick, x, y) {
    const pts = [[3, 0], [5, 0], [6, 1], [5, 2], [3, 2], [1, 2], [0, 1], [1, 0]];
    const head = mod(tick, 8);
    pts.forEach(([dx, dy], k) => {
      const age = mod(head - k, 8);
      const colour = age === 0 ? SHINE : (age < 3 ? SKY : (age < 5 ? '#4a7fb8' : null));
      if (colour) cv.cell(x + dx, y + dy, age === 0 ? 'o' : '.', colour);
    });
    cv.put(x - 2, y + 4, `Working${'.'.repeat(mod(fd(tick, 4), 4))}`.padEnd(10), TEXT_DIM, true);
  }

  doc(cv, tick, x, y, stream = false, pencil = false) {
    this.window(cv, x, y, 16, 7, 'Document');
    const ls = this.stream;
    let li;
    let col;
    if (stream) {
      const total = mod(tick, ls.length * 12);
      li = fd(total, 12);
      col = mod(total, 12);
    } else {
      li = fd(tick, 6);
      col = 12;
    }
    for (let k = 0; k < 5; k++) {
      const j = li - 4 + k;
      if (j < 0) continue;
      const text = j < li ? ls[mod(j, ls.length)] : ls[mod(j, ls.length)].slice(0, col);
      cv.put(x + 2, y + 1 + k, text, k === 4 ? TEXT : TEXT_DIM, false, WIN_BODY);
    }
    if (pencil) {
      const px = x + 2 + Math.min(col, 11);
      cv.put(px, y + 4, '/', AMBER, false, WIN_BODY);
      cv.put(px + 1, y + 3, '/', MIST, false, WIN_BODY);
      if (mod(fd(tick, 3), 2)) cv.cell(px, y + 5, '_', SHINE, WIN_BODY);
    }
  }

  check(cv, tick, x, y) {
    cv.put(x + 3, y + 1, '.---.', GREEN);
    cv.put(x + 2, y + 2, '/     \\', GREEN);
    cv.put(x + 2, y + 3, '|   / |', GREEN);
    cv.put(x + 2, y + 4, '| \\/  |', GREEN);
    cv.put(x + 2, y + 5, '\\     /', GREEN);
    cv.put(x + 3, y + 6, "'---'", GREEN);
    const r = lcg(103, fd(tick, 3));
    for (let i = 0; i < 4; i++) {
      const sx = x + int(r() * 14);
      const sy = y + int(r() * 7);
      if (cv.get(sx, sy) === ' ') cv.cell(sx, sy, '+*.'[int(r() * 3)], SHINE);
    }
  }

  warning(cv, tick, x, y) {
    const on = mod(fd(tick, 4), 2);
    const colour = on ? AMBER : '#b08040';
    cv.put(x + 4, y + 1, '/\\', colour);
    cv.put(x + 3, y + 2, '/  \\', colour);
    cv.put(x + 2, y + 3, '/ !! \\', colour);
    cv.put(x + 1, y + 4, '/______\\', colour);
    cv.put(x, y + 6, 'Try again?', TEXT_DIM, true);
  }

  bubbles(cv, tick, x, y, n = 4) {
    for (let j = 0; j < n; j++) {
      const r = lcg(107, j);
      const period = 30 + int(r() * 20);
      const t = mod(tick + int(r() * period), period);
      const bx = x + int(r() * 12) + (mod(fd(t, 5), 2) ? 1 : 0);
      const by = y - fd(t, 4);
      if (by < 0) continue;
      cv.cell(bx, by, t < fd(period, 2) ? 'o' : 'O', j % 2 ? MIST : SKY);
    }
  }

  helpers(cv, tick) {
    [Math.max(0, this.cx - 8), Math.min(this.cols - 7, this.cx + COMP_W + 1)].forEach((x, n) => {
      const y = this.ground - 5 - (Math.sin(tick / 5.0 + n * 2) > 0 ? 1 : 0);
      const hi = ['#3f9a5a', '#7a5ab8'][n];
      cv.sprite(x, y, ' .-. ', SKY);
      cv.sprite(x, y + 1, '(o o)', SHINE, hi);
      cv.sprite(x, y + 2, " `-' ", SKY);
    });
    const px = this.cx + OW + 2;
    const py = this.ground - 8;
    this.window(cv, px, py, 16, 5, 'Helpers');
    const k = mod(fd(tick, 8), 3);
    cv.put(px + 1, py + 2, `asking${'.'.repeat(k)}`.padEnd(10), TEXT, false, WIN_BODY);
    cv.put(px + 1, py + 3, '2 friends', TEXT_DIM, false, WIN_BODY);
  }
}
Mascot.moods = true;
Mascot.BUBBLES = {
  idle: 'Hi there! What shall we do today?',
  thinking: 'Working on it…',
  writing: 'Writing your answer…',
  done: "All done. Everything's up to date.",
  error: "Something went wrong. Let's try that again.",
};
Mascot.TOOL_BUBBLES = {
  Bash: 'Running a command…',
  Read: 'Opening the file…',
  Edit: 'Making changes…',
  MultiEdit: 'Making changes…',
  Write: 'Saving your file…',
  NotebookEdit: 'Updating the notebook…',
  Grep: 'Searching…',
  Glob: 'Looking through your files…',
  WebSearch: 'Searching the web…',
  WebFetch: 'Downloading…',
  Task: 'Asking my friends to help…',
  Agent: 'Asking my friends to help…',
  TodoWrite: 'Updating your tasks…',
};
Mascot.OTHER = 'Working with %s…';

// The glass window beside it per prop kind (mascot_aero._prop_<kind>);
// helpers draws its own in Mascot.helpers.
const PROPS = {
  bash(cv, tick, x, y) {
    this.window(cv, x, y, 16, 7, 'cmd', CMD_BODY);
    const cyc = fd(tick, 30);
    const cmd = CMDS[mod(cyc, CMDS.length)];
    const n = Math.min(cmd.length, fd(mod(tick, 30), 2));
    for (let k = 0; k < 3; k++) {
      const prev = CMDS[mod(cyc - 3 + k, CMDS.length)];
      cv.put(x + 1, y + 1 + k, `C:\\>${prev}`.slice(0, 14), TEXT_DIM, false, CMD_BODY);
    }
    cv.put(x + 1, y + 5, `C:\\>${cmd.slice(0, n)}`.slice(0, 14), TEXT, false, CMD_BODY);
    if (mod(fd(tick, 3), 2)) cv.cell(x + 5 + n, y + 5, '_', TEXT, CMD_BODY);
  },
  book(cv, tick, x, y) {
    this.doc(cv, tick, x, y);
    const hot = mod(fd(tick, 3), 5);
    for (let c = 0; c < 14; c++) cv.recolour(x + 1 + c, y + 1 + hot, SHINE, SEL);
  },
  pad(cv, tick, x, y) {
    this.doc(cv, tick, x, y, true, true);
  },
  lens(cv, tick, x, y) {
    this.window(cv, x, y, 16, 7, 'Search');
    cv.fill(x + 1, y + 1, 14, 1, '#1c4070');
    const q = 'files';
    const n = Math.min(q.length, fd(mod(tick, 40), 3));
    cv.put(x + 2, y + 1, q.slice(0, n), TEXT, false, '#1c4070');
    cv.put(x + 13, y + 1, 'O', SKY, false, '#1c4070');
    const hit = mod(fd(tick, 5), 4);
    ['theme.py', 'aero.css', 'notes.txt', 'photo.png'].forEach((f, k) => {
      cv.put(x + 2, y + 2 + k, f.padEnd(12), k === hit ? SHINE : TEXT_DIM, false, k === hit ? SEL : WIN_BODY);
    });
  },
  globe(cv, tick, x, y) {
    const land = ['~/\\_  ~~ /\\  ', '_/  \\__  ~ _/', '  ~~  \\_/  ~ '];
    const off = mod(fd(tick, 3), land[0].length);
    cv.put(x + 3, y, '.-""-.', SKY);
    cv.put(x + 2, y + 1, '/      \\', SKY);
    cv.put(x + 2, y + 2, '|      |', SKY);
    cv.put(x + 2, y + 3, '\\      /', SKY);
    cv.put(x + 3, y + 4, '`-..-\'', SKY);
    for (let k = 0; k < 3; k++) cv.put(x + 3, y + 1 + k, (land[k] + land[k]).slice(off, off + 6), GREEN);
    const a = tick * 0.4;
    const ex = x + 6 + int(pyRound(Math.cos(a) * 6));
    const ey = y + 2 + int(pyRound(Math.sin(a) * 1.2));
    cv.cell(ex, ey, '*', AMBER);
    const bx = x;
    const by = y + 6;
    const bw = 15;
    const pct = mod(tick * 2, 101);
    const n = Math.max(1, int(pct / 100 * bw));
    cv.fill(bx, by, bw, 1, '#1a3048');
    cv.fill(bx, by, n, 1, BAR_GREEN);
    const sweep = bx + mod(tick, bw + 4) - 2;
    for (const c of [sweep, sweep + 1]) {
      if (bx <= c && c < bx + n) cv.recolour(c, by, null, BAR_SHINE);
    }
  },
  todo(cv, tick, x, y) {
    this.window(cv, x, y, 16, 6, 'Tasks');
    const done = mod(fd(tick, 12), TODO.length + 1);
    TODO.forEach((item, k) => {
      cv.put(x + 1, y + 1 + k, k < done ? '[v]' : '[ ]', k < done ? GREEN : TEXT_DIM, false, WIN_BODY);
      cv.put(x + 5, y + 1 + k, item, k < done ? TEXT_DIM : TEXT, false, WIN_BODY);
    });
  },
  wand(cv, tick, x, y) {
    cv.put(x + 1, y + 6, '*', AMBER);
    cv.put(x + 2, y + 5, '/', MIST);
    this.bubbles(cv, tick, x + 2, y + 5, 5);
  },
};

const theme = makeTheme({
  meta: { id: 'aero', name: 'Aero', panel: PANEL },
  Mascot,
  say: (topic) => word(TOPIC_LINES, topic),
  emoteColours: EMOTE_COLOURS,
});

export const { meta, lines, spans, render, bubble, say, emoteColours } = theme;
