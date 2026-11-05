// The lofi theme's mascot: Miso, a sleepy ginger cat on a cushion in a cosy
// room at night: a rainy window on the left, a little desk with a lamp on the
// right, a record player on the floor. A faithful port of Atrium's
// mascot_lofi.py: the same (state, tick, tool, mood, scene, cols, rows) draws
// the same frame, cell for cell and colour for colour (golden tests:
// web/test/mascots-themes-a.test.mjs).
//
//   room    the back wall, a darker floor row with boards.
//   window  13 x 7 cells: plum frame and muntins, the sky inside painted as
//           cell backgrounds (dusk, night, pale day, grey when overcast), a
//           skyline with lit windows, and the weather on the glass.
//   Miso    18 x 6..9 cells, a tabby loaf on a lilac cushion: she breathes
//           idle, sits up at work and reaches a paw over to the desk.
//   desk    a lamp, a shelf (books and a plant, or the alarm clock), and the
//           work: a tiny CRT for Bash, an open book for Read, a notebook for
//           Edit/Write, a magnifier for the searches, a radio for WebFetch,
//           two kittens peeking for Task/Agent, sticky notes for TodoWrite, a
//           ball of yarn otherwise; a cassette when thinking, a notebook over
//           a keyboard when writing, a mug when done (tipped on an error).
//   floor   the record player (it spins in the music scene and when done), a
//           kitten or a fan beside it; a crate and a plant on a wide stage.
//   mood    her whole face and pose idle, accents at work; an idle fidget
//           (tail flick, ear twitch, look round, yawn, washing a paw).
//   scene   day, night, sun (and a beam), cloud, rain, storm, snow, fog, hot
//           (she sprawls), cold (a blanket), music, game, party, festive
//           string lights, an alarm clock (mood.js SCENES).

import { BaseMascot, Canvas, STATES, blink, lcg, propKind, streamLines, fd, mod, int, makeTheme }
  from './common.js';
import { MOODS, word } from './mood.js';

export const PANEL = '#1c1830';

const WALL = '#241e3a';
const FLOOR_MARK = '#2e2648';
const FRAME = '#3a2f55';
const BEAM = '#2e2440';
const DESK = '#3a2a3a';
const DESK_EDGE = '#7a6070';
const SHELF = '#33263a';
const LAMP_GLOW = '#4a3628';
const SCREEN = '#141128';
const CUSHION = '#3a2f55';
const BLANKET = '#33284a';
const BLANKET_MARK = '#7d7392';
const FUR = '#e8a470';
const STRIPE = '#b8744c';
const EYE = '#a6d6a0';
const NOSE = '#f4a3b0';
const BLUSH = '#f58a98';
const WHISKER = '#bfaebf';
const KITTEN = ['#c3b8d0', '#e8c49a'];
const TEXT = '#e8dccb';
const TEXT_HI = '#f3e9dc';
const DIM = '#bfaebf';
const HAZE = '#a091aa';
const PEACH = '#f2a477';
const PEACH_LT = '#f6b48c';
const LILAC = '#b9a3ea';
const RAIN = '#8fbfe0';
const TEAL = '#84cbc8';
const SAGE = '#a6d6a0';
const HONEY = '#e8d77a';
const BERRY = '#f58a98';
const YARN = '#f4a3b0';
const LEAF = '#86b088';
const SNOW = '#eae4f2';
const STAR = '#d6c8ff';
const STAR_DIM = '#6a6090';
const MOON = '#f2d49a';
const SUN = '#f0b080';
const CLOUD = '#8a80a6';
const FLASH = '#8a80a6';
const LIT = ['#f2b88a', '#e8c49a'];
const BUILDING = '#1a1428';
const NOTE_BG = ['#4a4030', '#4a3040', '#30404a'];
const SLEEVES = ['#f2a477', '#b9a3ea', '#8fbfe0', '#a6d6a0', '#f58a98', '#e8d77a'];
const BULBS = ['#f2a477', '#e8d77a', '#b9a3ea', '#8fbfe0', '#84cbc8', '#f4a3b0'];
const CONFETTI = ['#f2a477', '#b9a3ea', '#8fbfe0', '#e8d77a', '#f4a3b0'];
export const EMOTE_COLOURS = { accent: PEACH, warn: HONEY, bad: BERRY, good: SAGE, love: YARN, dim: HAZE };
const SKY = {
  dusk: ['#2a2348', '#33284f', '#3e2d56', '#4e3458', '#5e3c5a'],
  night: ['#120f24', '#151229', '#18142e', '#1c1733', '#211a38'],
  day: ['#4a3d64', '#544469', '#5e4a6c', '#6e526e', '#7e5a6c'],
  sun: ['#4e4064', '#644a68', '#7a5468', '#8e5e66', '#9a6866'],
  grey: ['#2e2a40', '#332e45', '#38324a', '#3d364e', '#423a52'],
  grey_night: ['#18162a', '#1b182e', '#1e1b32', '#211d35', '#241f38'],
};
const FOG_BG = '#3e3854';
const OVERCAST = ['cloud', 'rain', 'storm', 'snow', 'fog'];

const WW = 13;
const WH = 7;
const CW = 18;
const DW = 15;
const COMP_W = WW + 1 + CW + 1 + DW;

const WORDS = ['the', 'rain', 'is', 'soft', 'and', 'warm', 'tea', 'on', 'a', 'slow',
  'night', 'so', 'it', 'works', 'now', 'all', 'good', 'here', 'page', 'done'];
const SCRIBBLES = ['~~', '~', '~~~', '-~', '~-~', '~~-'];
const CMDS = ['ls', 'cd ~', 'make', 'git st', 'cat', 'ping'];

const EARS = {
  up: '/\\_,,,_/\\',
  twitch: "/'_,,,_/\\",
  back: "'\\_,,,_/'",
  flat: '_-._,,,_.-_',
};
const MOOD_FACE = {
  calm: ['ᵕ', 'ᵕ', 'ᴥ'],
  happy: ['^', '^', 'ᴥ'],
  excited: ['O', 'O', 'ᴥ'],
  shy: ['o', 'o', 'ᴥ'],
  curious: ['o', 'O', 'ᴥ'],
  focused: ['-', '-', 'ᴥ'],
  worried: ['o', 'o', '~'],
  sad: [';', ';', 'ᴥ'],
  sleepy: ['-', '-', 'ᴥ'],
};
const FIDGETS = {
  calm: [['tail', 3], ['ear', 2], ['look', 1], ['wash', 1], ['rest', 2]],
  happy: [['tail', 3], ['wash', 2], ['ear', 1]],
  excited: [['tail', 3], ['look', 2], ['ear', 2]],
  shy: [['ear', 2], ['rest', 3]],
  curious: [['look', 4], ['ear', 2]],
  focused: [['rest', 3], ['ear', 1]],
  worried: [['look', 3], ['ear', 2]],
  sad: [['rest', 2], ['tail', 1]],
  sleepy: [['yawn', 2], ['rest', 3], ['ear', 1]],
};

export const MOOD_BUBBLES = {
  calm: "mrrp… lo-fi beats and you. what's up?",
  happy: "purr~ everything's cosy. what's next?",
  excited: "mrrp! ooh, let's make something!",
  shy: 'oh… you saw me nap. hi ☺',
  curious: "hm? what's that? tell me more…",
  focused: 'headphones on. ready when you are.',
  worried: 'mrow… something feel off?',
  sad: 'rainy kind of day… stay a while?',
  sleepy: 'zz… five more minutes…',
};
export const TOPIC_LINES = {
  thanks: 'purr~ anytime.',
  worried: "mrow… that doesn't look right.",
  frustrated: 'again? deep breath, one more go…',
  relieved: 'phew. back to the beats ~',
  determined: "long one. i'll keep the tea warm…",
  proud: 'all done, and nicely too ~',
  sad: 'sorry… *ears down*',
  happy: 'purr~',
  curious: 'ooh, interesting…',
  rain: 'rain on the window… perfect study weather.',
  storm: "a storm… let's stay in where it's warm.",
  snow: 'snow outside. blanket time ❄',
  fog: 'foggy out. slow day, slow tea.',
  sun: 'a warm sunbeam… i call dibs.',
  hot: "it's {0}… i'm melting into the cushion.",
  cold: 'brr, {0} out. under the blanket we go.',
  gpu_hot: "the GPU's toasty: {0}°C. warm enough to nap on.",
  cpu_hot: "the CPU's at {0}°C… give it a breather?",
  music: "ooh, this one's a vibe ♫",
  game: "game time? i'll watch ~",
  event: 'mrrp, {0} in {1} min.',
  event_now: '{0} is starting now!',
  birthday: 'happy birthday, {0}! ♫',
  holiday: "it's {0} today ~",
  go_home: "it's {0}. time to log off and rest.",
  bedtime: "it's {0}… the lamp says bedtime.",
  morning: 'mrrp… good morning. coffee first.',
  lunch: 'lunch? i heard fish.',
  weekend: "it's the weekend. nap o'clock ~",
  friday: 'friday afternoon… the week winds down ~',
  hello: ['mrrp, hi ~ what are we making?', "oh, hello. the tea's on ~",
    'hey you. cosy session?'],
  welcome_back: "you're back ~ i kept the cushion warm.",
  wake: "mrr…? oh, i'm up. mostly ~",
  long_prompt: ["that's a long one… i'll get comfy.", 'lots to read ~ more tea, then.'],
  code: 'code ~ let me pad over and look.',
  image: 'ooh, a picture. let me see ~',
  aborted: 'okay, stopping ~',
  tests_pass: ['tests are green ~ purr.', 'all passing. tea break?',
    'green lights ~ nice and calm.'],
  tests_fail: ["tests failed… we'll fix it ~", 'red ones… one more listen.'],
  commit: ['committed ~ off it goes.', 'shipped. a quiet little win ~',
    'pushed ~ time to stretch.'],
  fast: ['quick one ~', 'done before the kettle boiled.', 'that was fast ~ back to napping.'],
  busy: ['busy paws today…', "so many tools… i'll keep up ~"],
  model: 'a new voice on the radio: {0} ~',
  poke: ['mrrp?', 'hm? oh, hi ~', '*tail flick*', 'you rang? ~',
    'boop ~ right back at you.', 'mrow, i was almost asleep…'],
  poke_spam: ['mrow! paws off, please ~', "okay okay, i'm awake! ~",
    '*hiss*… kidding. but stop it ~'],
  pet: ['purrrr~', '*kneads the cushion* ~', 'mm, right behind the ears ~',
    'slow blink ~ that means i like you.'],
  rg_memory: '{0} ate all the memory… I nudged it off the shelf.',
  rg_vram: '{0} hogged the GPU. pawed it away ~',
  rg_game_vram: '{0} is filling the GPU… maybe lower the textures?',
  rg_panic: 'mrrp. {0} is gone. back to the beats ~',
  rg_stuck: "mrow… memory's almost gone and I can't find who did it.",
  rg_agents: "all the agents' jobs stopped. the sessions are still napping safely.",
};

const has = (set, ...tags) => tags.some((t) => set.has(t));

// pyRound is Python's round(x) to an int: halves go to the even neighbour.
function pyRound(x) {
  const r = Math.round(x);
  return Math.abs(x % 1) === 0.5 ? 2 * Math.round(x / 2) : r;
}

export class Mascot extends BaseMascot {
  constructor(cols = 64, rows = 18) {
    super(cols, rows);
    this.cols = Math.max(int(cols), COMP_W);
    this.rows = Math.max(int(rows), 14);
    const g = Math.max(0, Math.min(4, fd(this.cols - COMP_W, 8)));
    const comp = COMP_W + 2 * g;
    this.cx = fd(this.cols - comp, 2);
    this.right = this.cx + comp;
    this.wx = this.cx;
    this.kx = this.wx + WW + 1 + g;
    this.dx = this.kx + CW + 1 + g;
    this.floor = this.rows - 2;
    this.wy = Math.max(1, this.floor - 11);
    this.py = this.floor - 8;
    this.stream = streamLines(WORDS, 8, 67);
    this.scribble = streamLines(SCRIBBLES, 6, 71);
    this.city = Array.from({ length: WW }, (_, i) => int(lcg(173, i)() * 3));
    this.lit = Array.from({ length: WW * 2 }, (_, i) => lcg(179, i)());
    this.stars = Array.from({ length: 7 }, (_, i) => [1 + int(lcg(181, i)() * (WW - 2)), 1 + int(lcg(183, i)() * 4),
      20 + int(lcg(187, i)() * 30), int(lcg(191, i)() * 50)]);
    this.boards = Array.from({ length: this.cols }, (_, x) => lcg(193, x)());
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
    const spinning = scene.has('music') || state === 'done';

    this.room(cv);
    this.window(cv, tick, mood, scene);
    if (scene.has('sun') && !has(scene, ...OVERCAST)) this.beam(cv);
    if (scene.has('festive')) this.lights(cv, tick);
    if (this.cx >= 8) this.decor(cv);
    if (this.floor >= 15) this.poster(cv);
    this.shelf(cv, tick, scene.has('clock'));
    this.desk(cv, scene.has('night') || !scene.has('day'));
    this.recordPlayer(cv, tick, spinning);
    if (kind === 'helpers') this.kittens(cv, tick);
    else if (scene.has('hot')) this.fan(cv, tick);
    const top = this.miso(cv, state, tick, kind, mood, scene, fidget);
    if (state === 'thinking') this.cassette(cv, tick);
    else if (state === 'writing') this.notebook(cv, tick);
    else if (state === 'done') this.mug(cv, tick);
    else if (state === 'error') this.mug(cv, tick, true);
    else if (kind && kind !== 'helpers') PROPS[kind].call(this, cv, tick);
    if (['tool', 'writing'].includes(state) && !['helpers', 'globe', 'wand'].includes(kind) && top !== null) {
      this.reach(cv, tick, top + 3);
    }
    if (spinning) this.notes(cv, tick);
    if (scene.has('party')) this.confetti(cv, tick);
    return cv;
  }

  fidget(tick, mood) {
    if (mod(tick, 90) < 72) return null;
    const table = FIDGETS[mood] || FIDGETS.calm;
    let pick = lcg(199, fd(tick, 90))() * table.reduce((a, [, w]) => a + w, 0);
    for (const [name, weight] of table) {
      pick -= weight;
      if (pick < 0) return name === 'rest' ? null : name;
    }
    return null;
  }

  // ---------- the room ----------

  room(cv) {
    cv.fill(0, 0, this.cols, this.floor + 1, WALL, ' ', null, false);
    const y = this.floor + 1;
    for (let x = 0; x < this.cols; x++) {
      const v = this.boards[x];
      if (v < 0.12) cv.cell(x, y, v < 0.06 ? '_' : '.', FLOOR_MARK);
    }
    for (let yy = y + 1; yy < this.rows; yy++) {
      for (let x = 0; x < this.cols; x++) {
        if (this.boards[(x * 7 + yy) % this.cols] < 0.08) cv.cell(x, yy, '.', FLOOR_MARK);
      }
    }
  }

  beam(cv) {
    for (let y = this.wy + WH; y < this.floor + 1; y++) {
      for (let k = 0; k < 3; k++) {
        const x = this.wx + 6 + (y - this.wy - WH) * 2 + k;
        if (x >= 0 && x < this.cols && cv.bg[y][x] === WALL) cv.recolour(x, y, null, BEAM);
      }
    }
  }

  lights(cv, tick) {
    for (let x = 0; x < this.cols; x++) {
      if (x % 3 === 1) {
        const on = mod(fd(x, 3) + fd(tick, 6), 4) !== 0;
        cv.cell(x, 0, on ? 'o' : '∙', on ? BULBS[fd(x, 3) % BULBS.length] : HAZE);
      } else {
        cv.cell(x, 0, '-', FRAME);
      }
    }
  }

  decor(cv) {
    const x = this.cx - 8;
    let y = this.floor - 2;
    for (let i = 0; i < 5; i++) cv.cell(x + 1 + i, y, '▐', SLEEVES[i % SLEEVES.length]);
    cv.sprite(x, y + 1, '|=====|', DESK_EDGE);
    cv.sprite(x, y + 2, '|_____|', DESK_EDGE);
    const px = this.right + 2;
    y = this.floor - 3;
    if (px + 5 <= this.cols) {
      cv.sprite(px, y, ',\\|/,', LEAF);
      cv.sprite(px, y + 1, ' \\|/ ', LEAF);
      cv.sprite(px, y + 2, '.---.', STRIPE);
      cv.sprite(px, y + 3, '|___|', STRIPE);
    }
  }

  poster(cv) {
    const x = this.kx + 4;
    const y = this.floor - 14;
    cv.sprite(x, y, '.-------.', FRAME);
    cv.sprite(x, y + 1, '|       |', FRAME);
    cv.sprite(x, y + 2, "'-------'", FRAME);
    cv.cell(x + 2, y + 1, '☾', LILAC);
    cv.cell(x + 4, y + 1, '♫', PEACH_LT);
    cv.cell(x + 6, y + 1, '✦', HONEY);
  }

  // ---------- the window ----------

  window(cv, tick, mood, scene) {
    const x0 = this.wx;
    const y0 = this.wy;
    const overcast = has(scene, ...OVERCAST);
    const night = scene.has('night');
    let sky;
    if (overcast) sky = SKY[night ? 'grey_night' : 'grey'];
    else if (night) sky = SKY.night;
    else if (scene.has('sun')) sky = SKY.sun;
    else if (scene.has('day')) sky = SKY.day;
    else sky = SKY.dusk;
    const flash = scene.has('storm') && mod(tick, 47) < 2;
    const fog = scene.has('fog');
    for (let iy = 1; iy < WH - 1; iy++) {
      for (let ix = 1; ix < WW - 1; ix++) {
        cv.cell(x0 + ix, y0 + iy, ' ', null, flash ? FLASH : (fog ? FOG_BG : sky[iy - 1]));
      }
    }
    for (let ix = 1; ix < WW - 1; ix++) {
      const h = this.city[ix];
      for (let k = 0; k < h; k++) {
        const y = y0 + WH - 2 - k;
        cv.recolour(x0 + ix, y, null, BUILDING);
        const lit = this.lit[ix * 2 + k];
        if (lit < 0.35 && !scene.has('day') && !flash) {
          const on = mod(fd(tick, 40) + ix, 7) !== 0;
          if (on) cv.cell(x0 + ix, y, '∙', LIT[k % 2], BUILDING);
        }
      }
    }
    if (night && !overcast && !flash) {
      for (const [ix, iy, period, ph] of this.stars) {
        if (this.city[ix] >= WH - 1 - iy) continue;
        if (cv.get(x0 + ix, y0 + iy) !== ' ') continue;
        const t = mod(tick + ph, period);
        cv.cell(x0 + ix, y0 + iy, t < 3 ? '+' : '.', t < 3 ? STAR : STAR_DIM);
      }
      cv.cell(x0 + 9, y0 + 1, '☾', MOON);
    } else if (!overcast && !flash && has(scene, 'sun', 'day')) {
      cv.cell(x0 + 3, y0 + 1, scene.has('sun') ? 'O' : 'o', SUN);
    }
    if ((overcast && !fog) || scene.has('cloud')) this.clouds(cv, tick, x0, y0, scene.has('storm'));
    const rain = has(scene, 'rain', 'storm');
    if (rain || mood === 'sad') {
      const heavy = scene.has('storm') || (rain && mood === 'sad');
      this.streaks(cv, tick, x0, y0, heavy ? 12 : 6);
    }
    if (scene.has('snow')) this.flakes(cv, tick, x0, y0);
    if (fog) {
      for (const iy of [2, 4]) {
        const off = mod(fd(tick, 8) + iy * 3, 6);
        for (let ix = 1; ix < WW - 1; ix++) {
          if ((ix + off) % 6 < 4) cv.cell(x0 + ix, y0 + iy, '~', HAZE);
        }
      }
    }
    if (flash) {
      [[8, '/'], [7, '/'], [8, '\\'], [7, '/']].forEach(([dx, ch], k) => {
        if (k < WH - 3) cv.cell(x0 + dx, y0 + 1 + k, ch, TEXT_HI);
      });
    }
    cv.put(x0, y0, `.${'-'.repeat(WW - 2)}.`, FRAME);
    for (let iy = 1; iy < WH - 1; iy++) {
      cv.cell(x0, y0 + iy, '|', FRAME);
      cv.cell(x0 + WW - 1, y0 + iy, '|', FRAME);
      cv.cell(x0 + fd(WW, 2), y0 + iy, iy === 3 ? '+' : '|', FRAME);
    }
    for (let ix = 1; ix < WW - 1; ix++) {
      if (ix !== fd(WW, 2)) cv.cell(x0 + ix, y0 + 3, '-', FRAME);
    }
    cv.put(x0, y0 + WH - 1, `'${'='.repeat(WW - 2)}'`, scene.has('snow') ? SNOW : FRAME);
  }

  clouds(cv, tick, x0, y0, storm) {
    [[1, 0.05], [2, 0.035]].forEach(([row, speed], i) => {
      const x = mod(int(lcg(197, i)() * 13 + tick * speed), WW + 4) - 3;
      Array.from('(~~)').forEach((ch, k) => {
        const ix = x + k;
        if (ix >= 1 && ix < WW - 1) cv.cell(x0 + ix, y0 + row, ch, storm ? HAZE : CLOUD);
      });
    });
  }

  streaks(cv, tick, x0, y0, n) {
    for (let i = 0; i < n; i++) {
      const r = lcg(211, i);
      const pos = r() * 20 + tick * (0.3 + r() * 0.2);
      const iy = 1 + mod(int(pos), WH - 2);
      const ix = 1 + mod(int(r() * 11) - fd(int(pos), 2), WW - 2);
      if (ix === fd(WW, 2) || iy === 3) continue;
      cv.cell(x0 + ix, y0 + iy, i % 3 ? "'" : ',', RAIN);
    }
  }

  flakes(cv, tick, x0, y0) {
    for (let i = 0; i < 6; i++) {
      const r = lcg(223, i);
      const pos = r() * 20 + tick * (0.08 + r() * 0.05);
      const iy = 1 + mod(int(pos), WH - 2);
      const ix = 1 + mod(int(r() * 11) + mod(fd(int(pos), 3), 2), WW - 2);
      if (ix === fd(WW, 2) || iy === 3) continue;
      cv.cell(x0 + ix, y0 + iy, '*.'[i % 2], SNOW);
    }
  }

  // ---------- the desk, the shelf, the floor ----------

  desk(cv, lamp = true) {
    const x = this.dx;
    const top = this.floor - 3;
    cv.fill(x, top, DW, 1, DESK, '=', DESK_EDGE);
    for (let y = top + 1; y < this.floor + 1; y++) {
      cv.cell(x + 1, y, '|', DESK_EDGE);
      cv.cell(x + DW - 2, y, '|', DESK_EDGE);
    }
    cv.put(x + 4, top + 1, '[_o_]', HAZE);
    const lx = x + 10;
    const ly = top - 4;
    const shade = lamp ? PEACH : HAZE;
    cv.sprite(lx, ly, ' ___ ', shade);
    cv.sprite(lx, ly + 1, '/___\\', shade);
    cv.sprite(lx, ly + 2, '  |  ', DIM);
    cv.sprite(lx, ly + 3, ' _|_ ', DIM);
    if (lamp) {
      for (const c of [1, 2, 3]) cv.recolour(lx + c, ly + 2, null, LAMP_GLOW);
      for (let c = 0; c < 5; c++) cv.recolour(lx + c, top, null, LAMP_GLOW);
    }
  }

  shelf(cv, tick, clock = false) {
    const x = this.dx;
    const y = this.floor - 9;
    cv.fill(x, y, DW, 1, SHELF, '_', FRAME);
    [SLEEVES[1], SLEEVES[3], SLEEVES[0], SLEEVES[2]].forEach((c, i) => cv.cell(x + 1 + i, y - 1, '▐', c));
    cv.cell(x + 2, y - 2, '▐', SLEEVES[5]);
    cv.cell(x + 3, y - 2, '▐', SLEEVES[4]);
    if (!clock) {
      cv.sprite(x + 11, y - 2, '\\|/', LEAF);
      cv.sprite(x + 11, y - 1, '(_)', STRIPE);
    } else {
      const ring = mod(fd(tick, 2), 10) < 5;
      const shake = ring ? mod(tick, 2) : 0;
      const cx = x + 6 + shake;
      cv.sprite(cx, y - 2, 'o.-.o', DIM);
      cv.cell(cx, y - 2, 'o', PEACH);
      cv.cell(cx + 4, y - 2, 'o', PEACH);
      cv.sprite(cx, y - 1, '(   )', DIM);
      cv.cell(cx + 2, y - 1, '|/-\\'[mod(fd(tick, 3), 4)], TEXT_HI);
      if (ring) {
        cv.cell(cx - 1, y - 2, '(', PEACH_LT);
        cv.cell(cx + 5, y - 2, ')', PEACH_LT);
      }
    }
  }

  recordPlayer(cv, tick, spinning) {
    const x = this.wx;
    const y = this.floor - 2;
    cv.sprite(x, y, ' _____ ', DIM);
    cv.sprite(x, y + 1, '|     |', DIM);
    cv.sprite(x, y + 2, "'-----'", DIM);
    const centre = spinning ? '-\\|/'[mod(fd(tick, 2), 4)] : '-';
    cv.cell(x + 1, y + 1, '(', TEXT);
    cv.cell(x + 2, y + 1, centre, PEACH);
    cv.cell(x + 3, y + 1, ')', TEXT);
    if (spinning) cv.cell(x + 4, y + 1, '/', LILAC);
    else cv.cell(x + 5, y + 1, "'", LILAC);
  }

  fan(cv, tick) {
    const x = this.wx + 8;
    const y = this.floor - 3;
    cv.sprite(x, y, ' .-. ', DIM);
    cv.sprite(x, y + 1, '(   )', DIM);
    cv.cell(x + 2, y + 1, '+x'[mod(fd(tick, 2), 2)], RAIN);
    cv.sprite(x, y + 2, " `|' ", DIM);
    cv.sprite(x, y + 3, ' _|_ ', DIM);
  }

  kittens(cv, tick) {
    [this.wx + 8, this.dx + 5].forEach((x, n) => {
      const up = Math.sin(tick / 6.0 + n * 2.1) > -0.3;
      const y = this.floor - (up ? 1 : 0);
      cv.sprite(x, y, '/\\_/\\', KITTEN[n]);
      if (up) {
        const eye = blink(tick + n * 17, 41) ? '-' : 'o';
        cv.sprite(x, y + 1, '(   )', KITTEN[n]);
        cv.cell(x + 1, y + 1, eye, EYE);
        cv.cell(x + 2, y + 1, '.', NOSE);
        cv.cell(x + 3, y + 1, eye, EYE);
      }
    });
    if (mod(fd(tick, 10), 3) === 0) cv.put(this.wx + 8, this.floor - 2, 'mew', HAZE);
  }

  notes(cv, tick) {
    const x0 = this.wx + 3;
    const y0 = this.floor - 3;
    for (let j = 0; j < 2; j++) {
      const s = mod(tick + j * 13, 26);
      if (s < 15) {
        const y = y0 - fd(s, 5);
        const x = x0 + j * 2 + (mod(fd(s, 4), 2) ? 1 : 0);
        if (cv.get(x, y) === ' ') cv.cell(x, y, '♫', [PEACH_LT, LILAC][j]);
      }
    }
  }

  confetti(cv, tick) {
    const span = this.floor;
    const n = Math.max(4, fd(this.cols * this.rows, 140));
    for (let i = 0; i < n; i++) {
      const r = lcg(227, i);
      const pos = r() * span + tick * (0.1 + r() * 0.08);
      const fall = int(fd(pos, span));
      const y = int(pos - fall * span);
      const x = int(lcg(229, i, fall)() * this.cols);
      if (cv.get(x, y) === ' ') cv.cell(x, y, ",'∙"[mod(fd(tick, 3) + i, 3)], CONFETTI[i % CONFETTI.length]);
    }
  }

  // ---------- Miso ----------

  miso(cv, state, tick, kind, mood, scene, fidget) {
    const idle = state === 'idle';
    const x = this.kx;
    const p = this.floor - 1;
    cv.fill(x, this.floor, CW - 1, 1, CUSHION, '~', LILAC);
    cv.cell(x, this.floor, '(', LILAC, CUSHION);
    cv.cell(x + CW - 2, this.floor, ')', LILAC, CUSHION);
    if (idle && scene.has('hot')) {
      this.sprawl(cv, tick, x, p, mood, fidget);
      this.head = [x + 7, p - 3];
      return null;
    }
    let n;
    if (idle) {
      if (mood === 'sad') n = 0;
      else if (mood === 'sleepy') n = mod(fd(tick, 30), 2);
      else n = 1 + mod(fd(tick, 20), 2);
    } else if (state === 'done') {
      n = [1, 2].includes(mod(fd(tick, 10), 4)) ? 3 : 2;
    } else {
      n = 2;
    }
    const top = p - n - 4;
    const stretch = state === 'done' && n === 3;
    const puffed = state === 'error';
    let tilt = 0;
    if (state === 'thinking') tilt = mod(fd(tick, 15), 2);
    else if (idle && mood === 'curious') tilt = 1;
    let ears = 'up';
    if (puffed || (idle && mood === 'sad')) ears = 'flat';
    else if (mood === 'worried' && state !== 'done') ears = 'back';
    else if (fidget === 'ear' || (scene.has('music') && mod(fd(tick, 5), 4) === 0)) ears = 'twitch';
    const e = EARS[ears];
    const ex = x + 8 + tilt - fd(e.length, 2);
    cv.sprite(ex, top, e, FUR);
    Array.from(e).forEach((ch, i) => {
      if (ch === ',') cv.recolour(ex + i, top, STRIPE);
    });
    cv.sprite(x + 3 + tilt, top + 1, '/         \\', FUR);
    cv.sprite(x + 2 + tilt, top + 2, '(           )', FUR);
    cv.sprite(x + 2, top + 3, puffed ? "{`--.___.--'}" : "/`--.___.--'\\", FUR);
    for (let k = 0; k < n; k++) {
      const y = top + 4 + k;
      if (puffed) {
        cv.sprite(x + 1, y, '{  ^^^   ^^^  }', FUR);
      } else {
        cv.sprite(x + 1, y, '(             )', FUR);
        for (const c of [5, 6]) cv.cell(x + c, y, ')', STRIPE);
        for (const c of [10, 11]) cv.cell(x + c, y, '(', STRIPE);
      }
    }
    cv.sprite(x, p, puffed ? '{^^,,^^^^^^^,,^^}' : '(__,,_______,,__)', FUR);
    let tail;
    if (puffed) tail = [[17, p, '}'], [17, p - 1, '{'], [17, p - 2, '}']];
    else if (mood === 'excited' && state !== 'error') tail = [[16, p - 1, '|'], [17, p - 2, ')'], [17, p - 3, '(']];
    else if (fidget === 'tail' || (idle && mood === 'happy' && mod(fd(tick, 8), 3) === 0)) {
      tail = [[17, p, ')'], [17, p - 1, ')'], [17, p - 2, "'"]];
    } else if (idle && ['sleepy', 'sad'].includes(mood)) tail = [[17, p, '~']];
    else tail = [[17, p - 1, '('], [17, p, ')']];
    for (const [dx, y, ch] of tail) cv.cell(x + dx, y, ch, FUR);
    if (stretch) {
      for (const [dx, dy, ch] of [[1, 2, '\\'], [0, 1, '\\'], [0, 0, ','], [15, 2, '/'], [16, 1, '/'], [16, 0, ',']]) {
        cv.cell(x + dx, top + dy, ch, FUR);
      }
    }
    if (idle && scene.has('cold')) {
      for (let y = top + 3; y < p + 1; y++) {
        for (let c = 1; c < 16; c++) {
          if (cv.get(x + c, y) !== null) cv.cell(x + c, y, (c + y) % 2 ? 'x' : '+', BLANKET_MARK, BLANKET);
        }
      }
    }
    this.face(cv, state, tick, kind, mood, scene, fidget, x + tilt, top);
    if (idle && scene.has('game') && mood !== 'sleepy') {
      cv.put(x + 6, p, '[+', RAIN, false, SCREEN);
      cv.cell(x + 8, p, '▐', [TEAL, PEACH, LILAC][mod(fd(tick, 4), 3)], SCREEN);
      cv.put(x + 9, p, 'o]', RAIN, false, SCREEN);
    }
    if (mood === 'focused') this.headphones(cv, x + tilt, top);
    if (scene.has('party')) {
      cv.cell(x + 8 + tilt, top - 2, '*', HONEY);
      cv.cell(x + 7 + tilt, top - 1, '/', BERRY);
      cv.cell(x + 8 + tilt, top - 1, ':', HONEY);
      cv.cell(x + 9 + tilt, top - 1, '\\', BERRY);
    }
    this.marks(cv, state, tick, mood, scene, fidget, x + tilt, top);
    this.head = [x + tilt + 15, Math.max(0, top - 1)];
    return top;
  }

  face(cv, state, tick, kind, mood, scene, fidget, x, top) {
    const idle = state === 'idle';
    let [left, right, mouth] = MOOD_FACE.calm;
    if (idle) {
      [left, right, mouth] = MOOD_FACE[mood] || MOOD_FACE.calm;
    } else if (state === 'done') {
      [left, right, mouth] = ['^', '^', 'ᴥ'];
    } else if (state === 'error') {
      [left, right, mouth] = ['O', 'O', '~'];
    } else if (mood === 'focused') {
      left = '-';
      right = '-';
    } else {
      left = 'o';
      right = 'o';
      if (kind === 'lens') right = 'O';
    }
    let gaze = 0;
    if (['tool', 'writing'].includes(state)) gaze = 1;
    const t = mod(tick, 90) - 72;
    if (fidget === 'look') {
      left = 'o';
      right = 'o';
      gaze = t < 9 ? -1 : 1;
    } else if (fidget === 'yawn') {
      mouth = t >= 3 && t < 14 ? 'O' : 'o';
    }
    const shut = blink(tick, 47) && ['o', 'O'].includes(left) && !['done', 'error'].includes(state);
    for (const [ex, ch] of [[6, left], [10, right]]) cv.cell(x + ex + gaze, top + 1, shut ? '-' : ch, EYE);
    cv.cell(x + 8, top + 2, mouth, NOSE);
    cv.cell(x + 5, top + 2, '=', WHISKER);
    cv.cell(x + 11, top + 2, '=', WHISKER);
    if (mood === 'shy' && state !== 'error') {
      cv.cell(x + 4, top + 2, '/', BLUSH);
      cv.cell(x + 12, top + 2, '/', BLUSH);
      if (idle) cv.put(x + 5, top + 1, '(,,)', FUR);
    } else if (mood === 'happy' && state !== 'error') {
      cv.cell(x + 4, top + 2, '~', BLUSH);
      cv.cell(x + 12, top + 2, '~', BLUSH);
    }
    if (fidget === 'wash' && mod(t, 6) < 4) cv.put(x + 9, top + 2, '(,)', FUR);
  }

  headphones(cv, x, top) {
    cv.put(x + 4, top - 1, '.-------.', LILAC);
    cv.cell(x + 3, top + 1, '[', LILAC);
    cv.cell(x + 13, top + 1, ']', LILAC);
  }

  reach(cv, tick, y) {
    const x0 = this.kx + 15;
    const tap = mod(fd(tick, 3), 2);
    for (let x = x0; x < this.dx + 1; x++) {
      if (cv.get(x, y) === ' ') cv.cell(x, y, '_', FUR);
    }
    cv.cell(this.dx + 1 + tap, y, ',', FUR);
  }

  sprawl(cv, tick, x, p, mood) {
    cv.sprite(x, p - 2, '  /\\_/\\           ', FUR);
    cv.sprite(x, p - 1, ' (     )`-------. ', FUR);
    cv.sprite(x, p, '(,,_ _,,)______,)~', FUR);
    const sleepy = ['sleepy', 'calm', 'sad'].includes(mood);
    const eye = sleepy || blink(tick, 47) ? '-' : 'o';
    cv.cell(x + 3, p - 1, eye, EYE);
    cv.cell(x + 5, p - 1, eye, EYE);
    cv.cell(x + 4, p, 'ᴥ', NOSE);
    const s = mod(fd(tick, 5), 4);
    if (s < 3) cv.cell(x, p - 2 + (s > 1 ? 1 : 0), "',"[s > 1 ? 1 : 0], RAIN);
    if (sleepy) this.zs(cv, tick, x + 9, p - 2, mood === 'sleepy');
  }

  zs(cv, tick, x, y, big = false) {
    if (big) {
      const s = mod(tick, 36);
      for (let j = 0; j < 3; j++) {
        if (fd(s, 9) > j) cv.cell(x + j, y - j, 'zzZ'[j], j < 2 ? DIM : TEXT_HI);
      }
    } else {
      const s = mod(tick, 60);
      if (s < 40) cv.cell(x + fd(s, 20), y - fd(s, 14), 'z', HAZE);
    }
  }

  marks(cv, state, tick, mood, scene, fidget, x, top) {
    const idle = state === 'idle';
    if (idle && mood === 'sleepy') this.zs(cv, tick, x + 14, top, true);
    else if (idle && mood === 'calm' && fidget === null) this.zs(cv, tick, x + 14, top);
    if (mood === 'curious' && (idle || mod(fd(tick, 20), 3) === 0) && mod(fd(tick, 5), 4)) {
      cv.cell(x + 15, top - 1, '?', PEACH_LT);
    }
    if (mood === 'excited' && state !== 'error') {
      const on = mod(fd(tick, 3), 4);
      if (on < 2 || idle) {
        cv.cell(x + 1, top, on % 2 ? '✦' : '✧', HONEY);
        if (idle) cv.cell(x + 15, top - 1, on % 2 ? '✧' : '✦', HONEY);
      }
    }
    if ((mood === 'worried' || scene.has('hot')) && state !== 'done') {
      const s = mod(fd(tick, 5), 4);
      const d = s > 1 ? 1 : 0;
      if (s < 3 && cv.get(x + 15, top + 1 + d) === ' ') cv.cell(x + 15, top + 1 + d, "',"[d], RAIN);
    }
    if (mood === 'happy' && state !== 'error' && mod(fd(tick, 6), 3)) {
      for (const px of [this.kx, this.kx + 16]) {
        if (cv.get(px, top + 3) === ' ') cv.cell(px, top + 3, '~', BLUSH);
      }
    }
    if (state === 'thinking') {
      Array.from('∙◦o').forEach((ch, j) => {
        if (mod(fd(tick, 6), 4) > j) cv.cell(x + 14 + j, top - j, ch, DIM);
      });
    } else if (state === 'error' && mod(fd(tick, 4), 3)) {
      cv.cell(this.kx, top, '!', BERRY);
      cv.cell(this.kx + 16, top, '!', BERRY);
    } else if (state === 'done') {
      const r = lcg(233, fd(tick, 4));
      for (let i = 0; i < 3; i++) {
        const sx = this.kx + int(r() * CW);
        const sy = top - 3 + int(r() * 3);
        if (cv.get(sx, sy) === ' ') cv.cell(sx, sy, '✧', PEACH_LT);
      }
    }
    if (scene.has('cold') && ['idle', 'done'].includes(state)) {
      const s = mod(tick, 40);
      if (s < 12) cv.cell(x + 15 + fd(s, 4), top + 2 - fd(s, 6), '.oO'[Math.min(2, fd(s, 4))], SNOW);
    }
  }

  // ---------- the work on the desk ----------

  cassette(cv, tick) {
    const x = this.dx;
    const y = this.py;
    cv.sprite(x, y + 2, '.--------.', DIM);
    cv.sprite(x, y + 3, '|(o)==(o)|', DIM);
    cv.sprite(x, y + 4, "'--------'", DIM);
    const reel = 'oO0O'[mod(fd(tick, 3), 4)];
    cv.cell(x + 2, y + 3, reel, PEACH);
    cv.cell(x + 7, y + 3, reel, PEACH);
    cv.cell(x + 4, y + 3, '=', LILAC);
    cv.cell(x + 5, y + 3, '=', LILAC);
    cv.put(x + 2, y + 2, 'side A', HAZE);
  }

  notebook(cv, tick) {
    const x = this.dx;
    const y = this.py;
    cv.sprite(x, y, '.--------.', DIM);
    cv.sprite(x, y + 1, '|        |', DIM);
    cv.sprite(x, y + 2, '|        |', DIM);
    cv.sprite(x, y + 3, "'--------'", DIM);
    const ls = this.stream;
    const total = mod(tick, ls.length * 8);
    const li = fd(total, 8);
    const col = mod(total, 8);
    for (let k = 0; k < 2; k++) {
      const j = li - 1 + k;
      if (j < 0) continue;
      const text = k === 0 ? ls[mod(j, ls.length)] : ls[mod(j, ls.length)].slice(0, col);
      cv.put(x + 1, y + 1 + k, text, k ? TEXT : DIM);
    }
    cv.put(x, y + 4, '[::::::::]', HAZE);
    const key = 1 + int(lcg(239, fd(tick, 2))() * 8);
    cv.cell(x + key, y + 4, ':', TEXT_HI, FRAME);
  }

  mug(cv, tick, tipped = false) {
    const x = this.dx + 4;
    const y = this.py + 4;
    if (tipped) {
      cv.put(x - 1, y, '(=]', DIM);
      cv.put(x + 2, y, '~~,.', STRIPE);
      return;
    }
    cv.put(x, y, '[_])', PEACH_LT);
    for (let k = 0; k < 2; k++) {
      const s = mod(fd(tick, 4) + k, 3);
      if (s < 2) cv.cell(x + 1 + ((s + k) % 2), y - 1 - k, k ? '∿' : '~', HAZE);
    }
  }
}
Mascot.moods = true;
Mascot.BUBBLES = {
  idle: "mrrp… lo-fi beats and you. what's up?",
  thinking: 'hmm… let me think on that, slowly',
  writing: 'scribbling it down…',
  done: 'all done ~ time for tea',
  error: "mrow?! that didn't work. try again?",
};
Mascot.TOOL_BUBBLES = {
  Bash: 'tapping the terminal…',
  Read: 'reading, paws on the page…',
  Edit: 'a little edit…',
  MultiEdit: 'a little edit…',
  Write: 'writing it down…',
  NotebookEdit: 'tidying the notebook…',
  Grep: 'sniffing around…',
  Glob: 'pawing through files…',
  WebSearch: 'tuning the radio…',
  WebFetch: 'fetching it…',
  Task: 'calling the kittens…',
  Agent: 'calling the kittens…',
  TodoWrite: 'sticky notes…',
};
Mascot.OTHER = 'batting at %s…';

// The work on the desk per prop kind (mascot_lofi._prop_<kind>); helpers is
// the kittens, drawn on the floor.
const PROPS = {
  bash(cv, tick) {
    const x = this.dx;
    const y = this.py;
    cv.sprite(x, y, ' .------. ', DIM);
    cv.sprite(x, y + 1, ' |      | ', DIM);
    cv.sprite(x, y + 2, ' |      | ', DIM);
    cv.sprite(x, y + 3, " '------' ", DIM);
    cv.sprite(x, y + 4, '  /____\\  ', HAZE);
    cv.fill(x + 2, y + 1, 6, 2, SCREEN);
    const cyc = fd(tick, 24);
    const prev = CMDS[mod(cyc - 1, CMDS.length)];
    const cmd = CMDS[mod(cyc, CMDS.length)];
    const n = Math.min(cmd.length, fd(mod(tick, 24), 3));
    cv.put(x + 2, y + 1, `$${prev}`.slice(0, 6), HAZE, false, SCREEN);
    cv.put(x + 2, y + 2, `$${cmd.slice(0, n)}`.slice(0, 6), SAGE, false, SCREEN);
    if (mod(fd(tick, 4), 2) && n + 1 < 6) cv.cell(x + 3 + n, y + 2, '_', SAGE, SCREEN);
  },
  book(cv, tick) {
    const x = this.dx;
    const y = this.py;
    cv.sprite(x, y + 2, ' ___ ___ ', DIM);
    cv.sprite(x, y + 3, '|~~~|~~~|', DIM);
    cv.sprite(x, y + 4, '|___|___|', DIM);
    const hot = mod(fd(tick, 4), 6);
    cv.recolour(x + 1 + hot + (hot > 2 ? 1 : 0), y + 3, TEXT_HI);
    if (mod(tick, 60) < 6) cv.cell(x + 6, y + 2, '/', TEXT);
  },
  pad(cv, tick) {
    const x = this.dx;
    const y = this.py;
    cv.sprite(x, y + 1, '.------.', DIM);
    cv.sprite(x, y + 2, '|      |', DIM);
    cv.sprite(x, y + 3, '|      |', DIM);
    cv.sprite(x, y + 4, "'------'", DIM);
    const ls = this.scribble;
    const total = mod(tick, ls.length * 6);
    const li = fd(total, 6);
    const col = mod(total, 6);
    const prev = li ? ls[mod(li - 1, ls.length)] : '';
    cv.put(x + 1, y + 2, prev, HAZE);
    cv.put(x + 1, y + 3, ls[mod(li, ls.length)].slice(0, col), TEXT);
    cv.cell(x + 1 + Math.min(col, 6), y + 2, '/', PEACH_LT);
  },
  lens(cv, tick) {
    const x = this.dx;
    const y = this.py;
    cv.put(x, y + 4, '[~~ ~~~ ~]', DIM);
    const mx = x + int(pyRound((Math.sin(tick / 8.0) + 1) * 2.5));
    cv.sprite(mx + 1, y + 1, '.-.', PEACH_LT);
    cv.cell(mx, y + 2, '(', PEACH_LT);
    cv.cell(mx + 4, y + 2, ')', PEACH_LT);
    cv.put(mx + 1, y + 2, '~~~', TEXT_HI, false, FRAME);
    cv.sprite(mx + 1, y + 3, "`-'", PEACH_LT);
    cv.cell(mx + 4, y + 3, '\\', DIM);
  },
  globe(cv, tick) {
    const x = this.dx;
    const y = this.py;
    const wave = [' ∿ ', '(∿)', '((∿))'][mod(fd(tick, 4), 3)];
    cv.put(x + 3 - fd(wave.length, 2), y + 1, wave, LILAC);
    cv.cell(x + 3, y + 2, '\\', DIM);
    cv.sprite(x, y + 3, '.--------.', DIM);
    cv.sprite(x, y + 4, '|        |', DIM);
    cv.cell(x + 1, y + 4, 'o', PEACH);
    const level = mod(fd(tick, 2), 6);
    for (let c = 0; c < 5; c++) cv.cell(x + 3 + c, y + 4, ':', c < level ? SAGE : HAZE);
  },
  todo(cv, tick) {
    const x = this.dx;
    const y = this.py;
    const done = mod(fd(tick, 10), 7);
    for (let k = 0; k < 6; k++) {
      const nx = x + (k % 3) * 3;
      const ny = y + 1 + fd(k, 3) * 2;
      const bg = NOTE_BG[k % 3];
      cv.fill(nx, ny, 3, 2, bg);
      cv.put(nx, ny, '--', HAZE, false, bg);
      cv.cell(nx + 1, ny + 1, k < done ? 'v' : ' ', SAGE, bg);
    }
  },
  wand(cv, tick) {
    const x = this.dx;
    const y = this.py;
    const k = mod(fd(tick, 3), 8);
    for (let c = 0; c < k; c++) cv.cell(x + c, y + 4, '~', LILAC);
    cv.cell(x + k, y + 3, '.', YARN);
    cv.put(x + k, y + 4, `(${'@oO'[mod(tick, 3)]})`, YARN);
  },
};

const theme = makeTheme({
  meta: { id: 'lofi', name: 'Lo-fi', panel: PANEL },
  Mascot,
  say: (topic) => word(TOPIC_LINES, topic),
  emoteColours: EMOTE_COLOURS,
});

export const { meta, lines, spans, render, bubble, say, emoteColours } = theme;
