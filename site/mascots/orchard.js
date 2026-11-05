// The orchard theme's mascot: Hum, a round little honeybee who tends the
// orchard -- a blossom tree on the left, a straw hive on the right, a strip of
// meadow along the bottom. A faithful port of Atrium's mascot_orchard.py: the
// same (state, tick, tool, mood, scene, cols, rows) draws the same frame, cell
// for cell and colour for colour (golden tests:
// web/test/mascots-themes-b.test.mjs). Our own character: a bee, never a
// fruit, nothing borrowed.
//
//   stage  the light panel, a faint sky tint in the top two rows, a meadow row
//          (grass tufts on a soft green) over a row of soil
//   tree   12 cells on the left: a rounded canopy with pink blossoms, a trunk
//   Hum    13 x 8 cells: two antennae, a round amber head and body with dark
//          stripes, a face, a pollen satchel (@) at the hip carrying a grain
//          of the tool he uses, and two pale wings that flutter with his mood
//   hive   a straw skep in honey stripes; the work on a little card over it
//          (a terminal, a book, a note, a magnifier, a globe, worker bees, a
//          honeycomb checklist, pollen sparkles); thinking he flies from one
//          blossom to the other, writing a leaf-scroll fills, done a jar of
//          honey, an error a spilt jar and a "!"
// Moods set the face, the antennae and the wing beat; scenes add weather,
// music notes, a handheld, a party hat, festive lights and an alarm clock.
//
// The stage is LIGHT, so every element has a dark outline; orchard-dark.js
// binds the DARK palette below to the same drawing.

import { Canvas, STATES, BaseMascot, blink, lcg, propKind, streamLines, fd, mod, int, makeTheme } from './common.js';
import { MOODS, word } from './mood.js';

export const PANEL = '#c4c4ca';

// Colour roles, light appearance: dark glyphs on light grounds.
export const LIGHT = {
  panel: PANEL,
  sky: ['#bfc5cf', '#c2c6cd'], sky_night: ['#b3b7c6', '#babdc8'],
  line: '#1d1d1f', dim: '#4f4f54',
  body: '#e6a800', stripe_bg: '#1d1d1f', stripe: '#e6a800', eye: '#1d1d1f',
  wing_bg: '#dfe6f0', wing: '#33415a', satchel: '#7a4a00',
  blush: '#e69aa8', sweat: '#1f4f9a',
  leaf_bg: '#a2ba93', leaf: '#2a542c', blossom: '#a3245f', trunk: '#55361c',
  grass_bg: '#aab89c', grass: '#2a542c', soil_bg: '#b2a794', soil: '#55452f',
  hive_bg: '#d2a04a', hive: '#4f300c', door_bg: '#3a2408',
  card_bg: '#dadade', card: '#1d1d1f', card_dim: '#4f4f54',
  term_bg: '#1e1e1e', term: '#e5e5e5', term_ok: '#32d74b',
  honey_bg: '#e6a800', honey: '#4f300c',
  accent: '#0058c4', good: '#248a3d', warn: '#b25000', bad: '#d70015',
  love: '#d30f45',
  sun: '#8a5600', moon: '#5e5018', star: '#43437a',
  cloud_bg: '#d3d6dc', cloud: '#4f5666', rain: '#1f4f9a', snow: '#3f4f6e',
  snow_bg: '#e3e7ee', fog: '#6e7480', flash: '#8a6a00',
  scarf_bg: '#b02a3a', scarf: '#ffffff', hat_bg: '#0062cc', hat: '#ffffff',
  notes: ['#6a2a9a', '#0058c4'],
  confetti: ['#d70015', '#0058c4', '#248a3d', '#8944ab', '#b25000'],
  bulbs: ['#d70015', '#b25000', '#248a3d', '#0058c4', '#8944ab'],
  game_bg: '#3a3a3c', game: '#e5e5ea',
};
// Dark appearance (orchard-dark.js): light lines on #232325.
export const DARK = {
  panel: '#232325',
  sky: ['#1d2130', '#202229'], sky_night: ['#151722', '#1a1b23'],
  line: '#d8d8dc', dim: '#a1a1a6',
  body: '#e6a800', stripe_bg: '#1d1d1f', stripe: '#e6a800', eye: '#1d1d1f',
  wing_bg: '#3a4658', wing: '#d4e2f6', satchel: '#ffcf6a',
  blush: '#a04a60', sweat: '#7ab0ff',
  leaf_bg: '#2c472e', leaf: '#93c48c', blossom: '#ff8fc0', trunk: '#c49470',
  grass_bg: '#26342a', grass: '#8fc08a', soil_bg: '#2e2a26', soil: '#968670',
  hive_bg: '#6a4a1a', hive: '#f0c070', door_bg: '#1a1206',
  card_bg: '#3a3a3c', card: '#ebebf0', card_dim: '#a1a1a6',
  term_bg: '#121212', term: '#e5e5e5', term_ok: '#32d74b',
  honey_bg: '#e6a800', honey: '#3a2408',
  accent: '#409cff', good: '#30d158', warn: '#ff9f0a', bad: '#ff453a',
  love: '#ff375f',
  sun: '#ffd060', moon: '#f2e2a0', star: '#e0dcff',
  cloud_bg: '#34363c', cloud: '#b0b4be', rain: '#7ab0ff', snow: '#f2f2f7',
  snow_bg: '#4a4e58', fog: '#7c808a', flash: '#ffe066',
  scarf_bg: '#c03a4a', scarf: '#ffffff', hat_bg: '#0071e3', hat: '#ffffff',
  notes: ['#d070ff', '#409cff'],
  confetti: ['#ff453a', '#409cff', '#30d158', '#bf5af2', '#ff9f0a'],
  bulbs: ['#ff453a', '#ff9f0a', '#30d158', '#409cff', '#bf5af2'],
  game_bg: '#0b0b0c', game: '#e5e5ea',
};
export const EMOTE_COLOURS = {
  accent: '#0058c4', warn: '#b25000', bad: '#d70015', good: '#248a3d', love: '#d30f45', dim: '#4f4f54',
};
const OVERCAST = ['cloud', 'rain', 'storm', 'snow', 'fog'];

const TW = 12;
const HW = 22;
const RW = 12;
const COMP_W = TW + 1 + HW + 1 + RW;
const BW = 13;
const BH = 8;

const WORDS = ['nectar', 'bloom', 'pollen', 'honey', 'comb', 'buzz', 'petal', 'sun', 'hive',
  'sweet', 'dew', 'leaf', 'row', 'tree', 'warm', 'done', 'neat', 'tidy'];
const CMDS = ['>_ ls', '> make', '> git', '> cat', '> ping', '> cd ~'];

const MOOD_FACE = {
  calm: ['ᵕ', 'ᵕ', 'w'],
  happy: ['^', '^', 'w'],
  excited: ['O', 'O', 'D'],
  shy: ['o', 'o', '~'],
  curious: ['o', 'O', 'o'],
  focused: ['-', '-', '_'],
  worried: ['o', 'o', '~'],
  sad: [';', ';', 'n'],
  sleepy: ['-', '-', '_'],
};
const WORK_FACE = {
  thinking: ['o', 'o', '-'], tool: ['o', 'o', '_'], writing: ['-', '-', '_'],
  done: ['^', '^', 'w'], error: ['x', 'x', 'o'],
};
const WING_BEAT = {
  calm: 3, happy: 2, excited: 1, shy: 3, curious: 2, focused: 2, worried: 1, sad: 5, sleepy: 0,
};
const ANTENNAE = {
  calm: 'up', happy: 'up', excited: 'perk', shy: 'up', curious: 'perk',
  focused: 'up', worried: 'droop', sad: 'droop', sleepy: 'droop',
};
const FIDGETS = {
  calm: [['twitch', 3], ['loop', 1], ['look', 2], ['rest', 2]],
  happy: [['loop', 3], ['twitch', 2], ['look', 1]],
  excited: [['loop', 4], ['twitch', 2]],
  shy: [['look', 2], ['rest', 3]],
  curious: [['look', 4], ['twitch', 2]],
  focused: [['rest', 3], ['twitch', 1]],
  worried: [['look', 3], ['twitch', 2]],
  sad: [['rest', 3], ['twitch', 1]],
  sleepy: [['rest', 4]],
};
// The grain of the work he carries in his satchel, per prop kind.
const CARRY = { bash: '>', book: '=', pad: '/', lens: 'o', globe: '@', helpers: '8', todo: '+', wand: '*' };
const BODY_ROWS = [
  ['   .-~~~-.   ', null],
  ['  (e     e)  ', 'head'],
  ['  (   m   )  ', 'head'],
  ['  (=======)  ', 'stripe'],
  ['  (       )@ ', 'body'],
  ['   `=====´   ', 'stripe_end'],
];
const CANOPY = [' .-~~~~-.  ', '(  ✿   ✿ ) ', '( ✿  ✿   ✿)', '(✿   ✿  ✿ )', " `-.__.-'  "];
const HIVE = ['  _...._  ', ' /======\\ ', '|========|', '|===()===|', "'--------'"];

export const MOOD_BUBBLES = {
  calm: "bzz… the orchard's quiet. what shall we tend?",
  happy: "bzz! everything's in bloom. what's next?",
  excited: "bzzz! a new flower! let's go!",
  shy: 'oh… you caught me napping on a petal.',
  curious: "hm? what's that one? tell me more…",
  focused: 'wings steady. ready when you are.',
  worried: "bzz… something's not right in the rows?",
  sad: 'a grey sort of day… stay a while?',
  sleepy: 'zz… one more minute in the blossom…',
};
// mood.js topics in his words: gentle, tidy, a gardener's voice.
export const TOPIC_LINES = {
  thanks: 'bzz~ my pleasure.',
  worried: "hm… that row doesn't look right.",
  frustrated: "again? deep breath, we'll replant it…",
  relieved: 'phew. back to the blossoms ~',
  determined: "a long row. I'll keep at it.",
  proud: 'all tended, and neatly too ~',
  sad: 'sorry… *antennae down*',
  happy: 'bzz~',
  curious: 'ooh, a new flower…',
  rain: 'rain on the orchard… good for the roots.',
  storm: 'a storm… everyone back to the hive.',
  snow: "snow on the hive. we'll stay in and keep warm.",
  fog: "foggy rows today. I'll fly slowly.",
  sun: 'sunshine! the blossoms are wide open.',
  hot: "it's {0}… the hive's fanning its wings.",
  cold: 'brr, {0} out. scarf on.',
  gpu_hot: "the GPU's at {0}°C. warm as a summer hive.",
  cpu_hot: "the CPU's at {0}°C… let it rest a moment?",
  music: "a tune! I'll buzz along ~",
  game: "game time? I'll keep the flowers company.",
  event: 'bzz, {0} in {1} min.',
  event_now: '{0} is starting now!',
  birthday: 'happy birthday, {0}! fresh honey for you.',
  holiday: "it's {0} today ~",
  go_home: "it's {0}. the orchard can wait till tomorrow.",
  bedtime: "it's {0}… even bees sleep.",
  morning: "good morning! the dew's still on the petals.",
  lunch: 'lunch? I know where the nectar is.',
  weekend: "it's the weekend. a slow day in the garden ~",
  friday: 'friday afternoon… the rows are nearly done ~',
  hello: ['bzz, hello ~ what shall we grow?', "oh, hi! the orchard's ready.",
    'hello you. a tidy session?'],
  welcome_back: "you're back ~ I kept the blossoms open.",
  wake: "bzz…? oh, I'm up. mostly ~",
  long_prompt: ["that's a long row… I'll settle in.", 'lots to read ~ more nectar, then.'],
  code: 'code ~ let me take a closer look.',
  image: 'ooh, a picture. let me see ~',
  aborted: 'okay, stopping ~',
  tests_pass: ['tests are green ~ like new leaves.', 'all passing. honey break?',
    'green rows ~ nice and tidy.'],
  tests_fail: ["a few tests wilted… we'll tend them ~", "red ones… let's look again."],
  commit: ['committed ~ off to the hive.', 'shipped. a sweet little win ~',
    'pushed ~ time to stretch the wings.'],
  fast: ['quick one ~', 'done before the dew dried.', 'that was fast ~ bzz.'],
  busy: ['busy as a bee today…', "so many tools… I'll keep up ~"],
  model: 'a new voice in the orchard: {0} ~',
  poke: ['bzz?', 'hm? oh, hi ~', '*wing flutter*', 'you called? ~',
    'boop ~ right back at you.', 'bzz, I was almost dozing…'],
  poke_spam: ["bzz! careful, I'm small ~", "okay okay, I'm awake! ~",
    '*buzzes in a circle*… please stop ~'],
  pet: ['bzzz~', '*happy wiggle* ~', 'mm, right between the wings ~',
    '*a slow little twirl* ~'],
  rg_memory: 'the hive was crowded: {0} was hoarding the honey (memory), so I moved it out.',
  rg_vram: "{0} filled the GPU's comb. I cleared it out ~",
  rg_game_vram: "{0} is filling the GPU's comb… lower the textures?",
  rg_panic: 'done: {0} is gone. back to the blossoms ~',
  rg_stuck: "the memory's nearly full and I can't find who took it…",
  rg_agents: "all the agents' jobs stopped. the sessions are safe in the hive.",
};

// pyRound is Python's round() to an int: halves go to the even neighbour.
function pyRound(x) {
  return Math.abs(x % 1) === 0.5 ? 2 * Math.round(x / 2) : Math.round(x);
}

const has = (scene, tags) => tags.some((t) => scene.has(t));

// Mascot draws with this.constructor.PAL (LIGHT here; orchard-dark.js binds
// DARK in a subclass).
class Mascot extends BaseMascot {
  constructor(cols = 64, rows = 18) {
    super(cols, rows);
    this.p = this.constructor.PAL;
    this.cols = Math.max(int(cols), COMP_W);
    this.rows = Math.max(int(rows), 14);
    const g = Math.max(0, Math.min(4, fd(this.cols - COMP_W, 8)));
    const comp = COMP_W + 2 * g;
    this.cx = fd(this.cols - comp, 2);
    this.tx = this.cx;
    this.hx = this.tx + TW + 1 + g;
    this.rx = this.hx + HW + 1 + g;
    this.ground = this.rows - 2;
    this.baseY = Math.max(1, this.ground - BH - 1);
    this.head = [this.hx + 10, Math.max(0, this.baseY - 1)];
    this.stream = streamLines(WORDS, 8, 97);
    this.grass = [];
    for (let x = 0; x < this.cols; x++) this.grass.push(lcg(311, x)());
    this.stars = [];
    for (let i = 0; i < Math.max(6, fd(this.cols, 9)); i++) {
      this.stars.push([int(lcg(313, i)() * this.cols), int(lcg(317, i)() * 3),
        20 + int(lcg(331, i)() * 30), int(lcg(337, i)() * 50)]);
    }
    this.cardX = this.rx;
    this.cardY = 0;
  }

  bubble(state, tool = null, mood = null) {
    if (state === 'idle' && Object.hasOwn(MOOD_BUBBLES, mood || '')) return MOOD_BUBBLES[mood];
    return super.bubble(state, tool);
  }

  // ---------- composition ----------

  render(state, tick, tool = null, mood = null, scene = new Set()) {
    if (!STATES.includes(state)) state = 'idle';
    if (!MOODS.includes(mood)) mood = 'calm';
    if (mood === 'sleepy' && ['thinking', 'tool', 'writing'].includes(state)) mood = 'focused';
    tick = int(tick);
    const cv = new Canvas(this.cols, this.rows);
    const kind = state === 'tool' ? propKind(tool) : null;
    const fidget = state === 'idle' ? this.fidget(tick, mood) : null;
    this.sky(cv, scene);
    this.weatherSky(cv, tick, scene);
    this.meadow(cv, tick, scene);
    this.tree(cv, scene);
    this.hive(cv, tick, scene);
    if (scene.has('festive')) this.lights(cv, tick);
    if (scene.has('clock')) this.alarm(cv, tick);
    this.blossoms(cv, tick);
    if (state === 'writing') this.scroll(cv, tick);
    else if (state === 'done') this.jar(cv, tick);
    else if (state === 'error') this.jar(cv, tick, true);
    else if (kind === 'helpers') this.workers(cv, tick);
    else if (kind) this[`prop_${kind}`](cv, tick);
    this.hum(cv, state, tick, kind, mood, scene, fidget);
    if (scene.has('music') || state === 'done') this.notes(cv, tick);
    if (scene.has('party')) this.confetti(cv, tick);
    if (scene.has('rain') || scene.has('storm') || scene.has('snow')) this.falling(cv, tick, scene);
    return cv;
  }

  fidget(tick, mood) {
    if (mod(tick, 90) < 76) return null;
    const table = FIDGETS[mood] || FIDGETS.calm;
    let pick = lcg(347, fd(tick, 90))() * table.reduce((s, [, w]) => s + w, 0);
    for (const [name, weight] of table) {
      pick -= weight;
      if (pick < 0) return name === 'rest' ? null : name;
    }
    return null;
  }

  // ---------- the stage ----------

  sky(cv, scene) {
    const p = this.p;
    const tint = scene.has('night') ? p.sky_night : p.sky;
    tint.forEach((bg, y) => cv.fill(0, y, this.cols, 1, bg, ' ', null, false));
    if (scene.has('fog')) {
      for (let y = 2; y < this.ground - 1; y += 3) {
        for (let x = 0; x < this.cols; x++) if ((x + y * 3) % 7 < 3) cv.cell(x, y, '~', p.fog);
      }
    }
  }

  weatherSky(cv, tick, scene) {
    const p = this.p;
    const right = this.cols - 3;
    if (scene.has('night')) {
      for (const [x, y, period, phase] of this.stars) {
        if (mod(tick + phase, period) < period - 3 && cv.get(x, y) === ' ') {
          cv.cell(x, y, (x + y) % 3 ? '✦' : '⋆', p.star);
        }
      }
      cv.put(right - 1, 0, '(', p.moon);
      cv.put(right - 1, 1, '(', p.moon);
    } else if ((scene.has('day') || scene.has('sun')) && !has(scene, OVERCAST)) {
      cv.cell(right, 0, '☼', p.sun);
      if (scene.has('sun')) {
        cv.put(right - 2, 0, '-', p.sun);
        cv.cell(right - 1, 1, '/', p.sun);
        cv.cell(right, 1, '|', p.sun);
        cv.cell(right + 1, 1, '\\', p.sun);
      }
    }
    if (scene.has('cloud') || scene.has('rain') || scene.has('storm')) {
      [this.cx + 4, this.cx + fd(this.cols, 2), this.cols - 14].forEach((x0, i) => {
        const x = mod(x0 + fd(tick, 12 + 4 * i), Math.max(1, this.cols - 6));
        cv.put(x, i % 2, '(~~~)', p.cloud, false, p.cloud_bg);
      });
    }
    if (scene.has('storm') && mod(tick, 47) < 3) {
      const x = this.hx + fd(HW, 2) + mod(fd(tick, 47), 7) - 3;
      Array.from('/\\/').forEach((ch, i) => cv.cell(x + (i === 1 ? 1 : 0), 2 + i, ch, p.flash));
    }
  }

  falling(cv, tick, scene) {
    const p = this.p;
    const snow = scene.has('snow');
    const n = Math.max(6, fd(this.cols * this.rows, snow ? 60 : 40));
    for (let i = 0; i < n; i++) {
      const r = lcg(353, i);
      const x0 = int(r() * this.cols);
      const speed = 1 + int(r() * 2);
      const y = mod(int(r() * this.ground) + fd(tick, snow ? 3 : 1) * speed, Math.max(1, this.ground));
      const x = x0 + (snow ? mod(fd(tick, 6) + i, 3) - 1 : 0);
      if (cv.get(x, y) === ' ') cv.cell(x, y, snow ? '*' : (i % 2 ? "'" : ','), snow ? p.snow : p.rain);
    }
  }

  meadow(cv, tick, scene) {
    const p = this.p;
    const y = this.ground;
    cv.fill(0, y, this.cols, 1, p.grass_bg, ' ', null, false);
    for (let x = 0; x < this.cols; x++) {
      const v = this.grass[x];
      if (v < 0.35) {
        const sway = mod(fd(tick, 9) + x, 5) === 0;
        cv.cell(x, y, v < 0.2 ? (sway ? '/' : ',') : (v < 0.28 ? "'" : '"'), p.grass);
      }
    }
    for (let yy = y + 1; yy < this.rows; yy++) {
      cv.fill(0, yy, this.cols, 1, p.soil_bg, ' ', null, false);
      for (let x = 0; x < this.cols; x++) {
        if (this.grass[(x * 7) % this.cols] < 0.12) cv.cell(x, yy, '.', p.soil);
      }
    }
    if (scene.has('hot')) {
      for (let x = 0; x < this.cols; x++) if (mod(x + fd(tick, 3), 6) === 0) cv.cell(x, y - 1, '~', p.dim);
    }
  }

  tree(cv, scene) {
    const p = this.p;
    const x = this.tx;
    const base = this.ground - 1;
    const top = Math.max(2, base - 8);
    CANOPY.forEach((line, i) => {
      const row = Array.from(line);
      const yy = top + i;
      let first = 0;
      while (first < row.length && row[first] === ' ') first++;
      let last = row.length;
      while (last > 0 && row[last - 1] === ' ') last--;
      for (let j = first; j < last; j++) {
        const ch = row[j];
        const bg = i > 0 && i < CANOPY.length - 1 ? p.leaf_bg : null;
        if (ch === '✿') cv.cell(x + j, yy, ch, p.blossom, p.leaf_bg);
        else if (ch === ' ') {
          if (bg) cv.cell(x + j, yy, ' ', null, bg);
        } else cv.cell(x + j, yy, ch, p.leaf, bg);
      }
    });
    if (scene.has('snow')) cv.put(x + 1, top - 1, '_.---._', p.snow, false, p.snow_bg);
    for (let yy = top + CANOPY.length; yy < base + 1; yy++) cv.put(x + 4, yy, '||', p.trunk);
    cv.put(x + 3, base, '/||\\', p.trunk);
  }

  hive(cv, tick, scene) {
    const p = this.p;
    const x = this.rx + 1;
    const base = this.ground - 1;
    const top = base - HIVE.length + 1;
    HIVE.forEach((row, i) => {
      const yy = top + i;
      Array.from(row).forEach((ch, j) => {
        if (ch === ' ') return;
        if (ch === '=') cv.cell(x + j, yy, (j + i) % 2 ? '=' : '-', p.hive, p.hive_bg);
        else if (ch === '(' || ch === ')') cv.cell(x + j, yy, ch, p.hive, p.door_bg);
        else cv.cell(x + j, yy, ch, p.hive);
      });
    });
    if (scene.has('snow')) cv.put(x + 2, top - 1, '_.--._', p.snow, false, p.snow_bg);
    this.cardX = this.rx;
    this.cardY = Math.max(0, top - 6);
  }

  blossoms(cv, tick) {
    const p = this.p;
    const y = this.ground - 1;
    for (const x of [this.hx + 1, this.hx + HW - 2]) {
      cv.cell(x, y - 1, '✿', p.blossom);
      cv.cell(x, y, '|', p.grass);
      cv.cell(x - 1, y, mod(fd(tick, 20), 2) ? '\\' : ',', p.grass);
    }
  }

  // ---------- Hum ----------

  hum(cv, state, tick, kind, mood, scene, fidget) {
    const p = this.p;
    let y = this.baseY;
    if (mood !== 'sleepy' || state !== 'idle') {
      const beat = scene.has('music') ? 4 : 8;
      y += mod(fd(tick, beat), 2) ? 1 : 0;
    }
    let x = this.hx + fd(HW - BW, 2);
    if (state === 'thinking') {
      const span = HW - BW;
      const t = mod(tick, 60) / 60.0;
      x = this.hx + int(pyRound(span * (0.5 - 0.5 * Math.cos(2 * Math.PI * t))));
    } else if (['tool', 'writing', 'done', 'error'].includes(state)) {
      x = this.hx + HW - BW;
    } else if (fidget === 'loop') {
      x += [-1, 0, 1, 0][mod(fd(tick, 3), 4)];
      y += y > 1 ? [0, -1, 0, 1][mod(fd(tick, 3), 4)] : 0;
    }
    y = Math.max(1, Math.min(y, this.ground - BH));

    let face = MOOD_FACE[mood] || MOOD_FACE.calm;
    const work = state !== 'idle';
    if (work) face = WORK_FACE[state];
    if (state === 'idle' && mood !== 'sleepy' && blink(tick)) face = ['-', '-', face[2]];
    if (fidget === 'look' && mood !== 'sleepy') {
      face = mod(fd(tick, 4), 2) ? [face[0], face[1], face[2]] : [face[1], face[0], face[2]];
    }

    let antennae = !work ? (ANTENNAE[mood] || 'up') : (state === 'error' ? 'droop' : 'up');
    if (fidget === 'twitch' && mod(fd(tick, 2), 2)) antennae = 'perk';
    const a = p.line;
    if (antennae === 'droop') {
      cv.put(x + 2, y + 1, '~', a);
      cv.put(x + 10, y + 1, '~', a);
      cv.put(x + 3, y + 1, '\\', a);
      cv.put(x + 9, y + 1, '/', a);
    } else {
      const tip = antennae === 'perk' ? '*' : 'o';
      cv.cell(x + 2, y, tip, a);
      cv.cell(x + 10, y, tip, a);
      cv.cell(x + 3, y + 1, '\\', a);
      cv.cell(x + 9, y + 1, '/', a);
    }
    BODY_ROWS.forEach(([row, role], i) => {
      const yy = y + 2 + i;
      Array.from(row).forEach((ch, j) => {
        const xx = x + j;
        const inner = j > 2 && j < 10;
        if (role === null) {
          if (ch !== ' ') cv.cell(xx, yy, ch, a);
          return;
        }
        if (role === 'stripe_end') {
          if (j === 3) cv.cell(xx, yy, '`', a);
          else if (j > 3 && j < 9) cv.cell(xx, yy, '=', p.stripe, p.stripe_bg);
          else if (j === 9) cv.cell(xx, yy, "'", a);
          return;
        }
        if (ch === '(' || ch === ')') cv.cell(xx, yy, ch, a);
        else if (ch === '@') cv.cell(xx, yy, kind ? (CARRY[kind] || '@') : '@', p.satchel);
        else if (inner && role === 'stripe') cv.cell(xx, yy, '=', p.stripe, p.stripe_bg);
        else if (inner) {
          let glyph = ' ';
          if (ch === 'e') glyph = j < 6 ? face[0] : face[1];
          else if (ch === 'm') glyph = face[2];
          cv.cell(xx, yy, glyph, p.eye, p.body);
          if (glyph === ' ') cv.fill(xx, yy, 1, 1, p.body);
        }
      });
    });
    const ey = y + 3;
    cv.cell(x + 3, ey, ' ', null, p.body);
    cv.cell(x + 9, ey, ' ', null, p.body);
    cv.cell(x + 4, ey, face[0], p.eye, p.body);
    cv.cell(x + 8, ey, face[1], p.eye, p.body);
    if (mood === 'shy' && state === 'idle') {
      cv.cell(x + 3, y + 4, ' ', null, p.blush);
      cv.cell(x + 9, y + 4, ' ', null, p.blush);
    }
    let speed = !work ? (WING_BEAT[mood] ?? 3) : (state === 'thinking' || state === 'tool' ? 1 : 2);
    if (state === 'error') speed = 0;
    const up = speed && mod(fd(tick, speed), 2) === 0;
    let wy = y + (up ? 2 : 3);
    if (!speed) wy = y + 4;
    for (let i = 0; i < 2; i++) {
      cv.cell(x, wy + i, '(', p.wing, p.wing_bg);
      cv.cell(x + 1, wy + i, i === 0 ? '~' : '_', p.wing, p.wing_bg);
      cv.cell(x + 11, wy + i, i === 0 ? '~' : '_', p.wing, p.wing_bg);
      cv.cell(x + 12, wy + i, ')', p.wing, p.wing_bg);
    }
    this.head = [Math.min(x + 11, this.cols - 1), Math.max(0, y - 1)];
    if (mood === 'sleepy' && state === 'idle') {
      const z = mod(fd(tick, 8), 3);
      cv.put(x + 11, Math.max(0, y + 1 - z), z < 2 ? 'z' : 'Z', p.dim);
    }
    if (mood === 'curious' && state === 'idle') cv.cell(x + 11, y, '?', p.accent);
    if (mood === 'excited' && mod(fd(tick, 3), 2)) cv.cell(x + 12, y + 1, '✦', p.warn);
    if (mood === 'worried' || scene.has('hot') || state === 'error') cv.cell(x + 10, y + 2, "'", p.sweat);
    if (state === 'thinking') cv.put(x + 11, y + 1, '.'.repeat(1 + mod(fd(tick, 5), 3)), p.dim);
    if (state === 'error') cv.cell(x + 12, y + 1, '!', p.bad);
    if (scene.has('cold')) {
      cv.put(x + 3, y + 5, '~~~~~~~', p.scarf, false, p.scarf_bg);
      if (mod(fd(tick, 6), 3) === 0) cv.cell(x + 1, y + 4, 'o', p.dim);
    }
    if (scene.has('party')) {
      cv.cell(x + 5, y + 1, '/', p.hat, p.hat_bg);
      cv.cell(x + 6, y, '*', p.warn);
      cv.cell(x + 6, y + 1, '^', p.hat, p.hat_bg);
      cv.cell(x + 7, y + 1, '\\', p.hat, p.hat_bg);
    }
    if (scene.has('game') && state === 'idle') cv.put(x + 4, y + 8, '[=o]', p.game, false, p.game_bg);
  }

  // ---------- the work, on a card over the hive ----------

  card(cv, w = 10, h = 4) {
    const p = this.p;
    const x = this.cardX + 1;
    const y = this.cardY;
    cv.fill(x, y, w, h, p.card_bg);
    cv.put(x, y, '.' + '-'.repeat(w - 2) + '.', p.card_dim, false, p.card_bg);
    cv.put(x, y + h - 1, "'" + '-'.repeat(w - 2) + "'", p.card_dim, false, p.card_bg);
    for (let yy = y + 1; yy < y + h - 1; yy++) {
      cv.cell(x, yy, '|', p.card_dim, p.card_bg);
      cv.cell(x + w - 1, yy, '|', p.card_dim, p.card_bg);
    }
    return [x, y];
  }

  prop_bash(cv, tick) {
    const p = this.p;
    const x = this.cardX + 1;
    const y = this.cardY;
    cv.fill(x, y, 10, 4, p.term_bg);
    cv.put(x + 1, y, 'o o o', p.card_dim, false, p.term_bg);
    const cmd = CMDS[mod(fd(tick, 25), CMDS.length)];
    const n = Math.min(cmd.length, fd(mod(tick, 25), 2) + 1);
    cv.put(x + 1, y + 1, cmd.slice(0, n).padEnd(8).slice(0, 8), p.term, false, p.term_bg);
    cv.put(x + 1, y + 2, '>', p.term_ok, false, p.term_bg);
    if (mod(fd(tick, 4), 2)) cv.cell(x + 2, y + 2, '_', p.term, p.term_bg);
  }

  prop_book(cv, tick) {
    const p = this.p;
    const [x, y] = this.card(cv);
    const page = mod(fd(tick, 12), 3);
    for (let i = 0; i < 2; i++) {
      cv.put(x + 1, y + 1 + i, ['~~~ ', '~~  ', '~ ~ '][(page + i) % 3], p.card, false, p.card_bg);
      cv.put(x + 5, y + 1 + i, ['~~~', ' ~~', '~~ '][(page + i + 1) % 3], p.card, false, p.card_bg);
    }
    cv.cell(x + 4, y + 1, '|', p.card_dim, p.card_bg);
    cv.cell(x + 4, y + 2, '|', p.card_dim, p.card_bg);
  }

  prop_pad(cv, tick) {
    const p = this.p;
    const [x, y] = this.card(cv);
    const n = mod(fd(tick, 3), 14);
    const line1 = '~~~~~~'.slice(0, Math.min(6, n));
    const line2 = '~~~~'.slice(0, Math.max(0, Math.min(4, n - 6)));
    cv.put(x + 1, y + 1, line1.padEnd(7), p.card, false, p.card_bg);
    cv.put(x + 1, y + 2, line2.padEnd(7), p.card, false, p.card_bg);
    const px = x + 1 + (n < 6 ? line1.length : line2.length);
    const py = n < 6 ? y + 1 : y + 2;
    cv.cell(px, py, '/', p.warn, p.card_bg);
  }

  prop_lens(cv, tick) {
    const p = this.p;
    const [x, y] = this.card(cv);
    cv.put(x + 1, y + 2, '✿ ✿ ✿ ✿', p.blossom, false, p.card_bg);
    const lx = x + 1 + mod(fd(tick, 4), 6);
    cv.put(lx, y + 1, 'O-', p.card, false, p.card_bg);
  }

  prop_globe(cv, tick) {
    const p = this.p;
    const [x, y] = this.card(cv);
    cv.put(x + 3, y + 1, ['(@)', '(#)', '(%)'][mod(fd(tick, 5), 3)], p.accent, false, p.card_bg);
    const waves = ')'.repeat(1 + mod(fd(tick, 4), 3));
    cv.put(x + 6, y + 1, waves.padEnd(3), p.card_dim, false, p.card_bg);
    cv.put(x + 1, y + 2, '........'.slice(0, 1 + mod(fd(tick, 2), 8)).padEnd(8), p.good, false, p.card_bg);
  }

  prop_todo(cv, tick) {
    const p = this.p;
    const [x, y] = this.card(cv, 11, 5);
    const done = mod(fd(tick, 15), 4);
    ['nectar', 'pollen', 'honey'].forEach((item, i) => {
      const mark = i < done ? 'x' : ' ';
      cv.put(x + 1, y + 1 + i, `[${mark}]${item}`.slice(0, 9), p.card, false, p.card_bg);
      if (mark === 'x') cv.cell(x + 2, y + 1 + i, 'x', p.good, p.card_bg);
    });
  }

  prop_wand(cv, tick) {
    const p = this.p;
    const x = this.cardX + 1;
    const y = this.cardY;
    for (let i = 0; i < 6; i++) {
      const r = lcg(359, i, fd(tick, 6));
      const cx = x + int(r() * 10);
      const cy = y + int(r() * 4);
      cv.cell(cx, cy, '✦⋆*'[i % 3], [p.warn, p.accent, p.blossom][i % 3]);
    }
  }

  workers(cv, tick) {
    const p = this.p;
    const doorX = this.rx + 5;
    const doorY = this.ground - 2;
    for (let j = 0; j < 2; j++) {
      const s = mod(tick + j * 17, 34);
      const x = doorX + (j ? fd(s, 2) : -fd(s, 3));
      const y = doorY - 1 - fd(s, 6);
      if (y >= 0) {
        cv.put(x, y, j ? '~8>' : '<8~', p.line);
        cv.cell(x + 1, y, '8', p.stripe_bg, p.body);
      }
    }
  }

  scroll(cv, tick) {
    const p = this.p;
    const [x, y] = this.card(cv, 10, 4);
    const n = this.stream.length;
    const top = mod(fd(tick, 6), Math.max(1, n - 1));
    for (let i = 0; i < 2; i++) {
      cv.put(x + 1, y + 1 + i, this.stream[mod(top + i, n)].slice(0, 8), p.card, false, p.card_bg);
    }
  }

  jar(cv, tick, spilt = false) {
    const p = this.p;
    const x = this.cardX + 3;
    const y = this.cardY;
    if (spilt) {
      cv.put(x - 1, y + 2, '\\___/', p.line);
      cv.put(x - 1, y + 3, ' ~~~~~', p.honey, false, p.honey_bg);
      cv.cell(x + 5, y + 1, '!', p.bad);
      return;
    }
    cv.put(x, y, '[___]', p.line);
    cv.put(x, y + 1, '|', p.line);
    cv.put(x + 1, y + 1, '~~~', p.honey, false, p.honey_bg);
    cv.put(x + 4, y + 1, '|', p.line);
    cv.put(x, y + 2, '|', p.line);
    cv.put(x + 1, y + 2, '   ', null, false, p.honey_bg);
    cv.put(x + 4, y + 2, '|', p.line);
    cv.put(x, y + 3, "`---'", p.line);
    for (let i = 0; i < 3; i++) {
      if (mod(fd(tick, 4) + i, 3) === 0) cv.cell(x - 1 + i * 3, Math.max(0, y - 1), '✦', [p.warn, p.accent, p.good][i]);
    }
  }

  alarm(cv, tick) {
    const p = this.p;
    const x = this.rx + 4;
    const y = this.ground - 6;
    cv.put(x, y, '(:)', p.line, false, p.card_bg);
    if (mod(fd(tick, 3), 2)) {
      cv.cell(x - 1, y, '!', p.bad);
      cv.cell(x + 3, y, '!', p.bad);
    }
  }

  lights(cv, tick) {
    const p = this.p;
    for (let x = 0; x < this.cols; x++) {
      if (x % 4 === 1) cv.cell(x, 0, 'o', p.bulbs[mod(fd(x, 4) + fd(tick, 6), p.bulbs.length)]);
      else if (cv.get(x, 0) === ' ') cv.cell(x, 0, '-', p.dim);
    }
  }

  notes(cv, tick) {
    const p = this.p;
    const x0 = this.hx + 3;
    const y0 = this.baseY + 2;
    for (let j = 0; j < 2; j++) {
      const s = mod(tick + j * 11, 22);
      if (s < 14) {
        const y = y0 - fd(s, 5);
        const x = x0 + j * 13 + (mod(fd(s, 4), 2) ? 1 : 0);
        if (y >= 0 && cv.get(x, y) === ' ') cv.cell(x, y, '♫', p.notes[j]);
      }
    }
  }

  confetti(cv, tick) {
    const p = this.p;
    for (let i = 0; i < Math.max(4, fd(this.cols * this.rows, 140)); i++) {
      const r = lcg(367, i);
      const x = int(r() * this.cols);
      const y = mod(int(r() * this.ground) + fd(tick, 3), Math.max(1, this.ground));
      if (cv.get(x, y) === ' ') cv.cell(x, y, "*+'"[i % 3], p.confetti[i % p.confetti.length]);
    }
  }
}
Mascot.BUBBLES = {
  idle: "bzz… the orchard's quiet. what shall we tend?",
  thinking: 'hmm… flying it over, flower to flower',
  writing: 'writing it on a leaf…',
  done: 'all done ~ fresh honey',
  error: "bzz?! that didn't work. try again?",
};
Mascot.TOOL_BUBBLES = {
  Bash: 'tapping the little terminal…',
  Read: 'reading, a page at a time…',
  Edit: 'a small, tidy edit…',
  MultiEdit: 'a few tidy edits…',
  Write: 'writing it down…',
  NotebookEdit: 'tidying the notebook…',
  Grep: 'looking flower by flower…',
  Glob: 'flying the rows…',
  WebSearch: 'asking the other gardens…',
  WebFetch: 'fetching it from afar…',
  Task: 'sending the workers out…',
  Agent: 'sending the workers out…',
  TodoWrite: 'ticking the comb…',
};
Mascot.OTHER = 'gathering %s…';
Mascot.moods = true;
Mascot.PAL = LIGHT;

export { Mascot };

const theme = makeTheme({
  meta: { id: 'orchard', name: 'Orchard', panel: PANEL },
  Mascot,
  say: (topic) => word(TOPIC_LINES, topic),
  emoteColours: EMOTE_COLOURS,
});

export const { meta, lines, spans, render, bubble, say, emoteColours } = theme;
