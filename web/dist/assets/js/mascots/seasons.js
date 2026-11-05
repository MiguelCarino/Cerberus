// The seasons theme's mascot: Sorrel, a little fox kit sitting at the foot of
// a small tree on a strip of ground, with an almanac strip along the bottom of
// the stage. The tree, the ground and the sky turn with the season. A faithful
// port of Atrium's mascot_seasons.py: the same (state, tick, tool, mood, scene,
// cols, rows) in the same season and at the same time draws the same frame,
// cell for cell and colour for colour (golden tests:
// web/test/mascots-themes-b.test.mjs). Our own drawing: no fox from any film,
// game or brand.
//
//   the sky  cell backgrounds, the season's floor at the top fading into the
//            stage card at the ground; stars and the real moon's phase by
//            night, a small sun in the sun scene, cloud bands when overcast
//   the tree spring blossom with petals drifting, a full summer crown with
//            fireflies by night, an amber autumn crown with falling leaves
//            piling at the root, bare winter branches with snow on them
//   strip    the almanac: AUTUMN - 24 OCT, the moon's phase as one cell
//            (. new, ) waxing, O full, ( waning), the state word (resting,
//            musing, foraging, writing, found it, oops), the HH:MM when there
//            is room
//   Sorrel   15 x 5 cells: ears, a round face, a cream muzzle, paws, a big
//            tail (it wags while writing); a scarf in winter and the cold; a
//            ball with its tail over its nose when sleepy
//   props    field-notes cards: a notebook for Bash, a pressed-leaf book for
//            Read, field notes for Edit/Write, a leaf pile for Grep/Glob, a
//            paper kite for WebSearch/WebFetch, sparrows for Task/Agent, seed
//            packets for TodoWrite, fireflies otherwise.
//
// The season: setSeason(s) pins one (spring, summer, autumn, winter; null
// unpins); otherwise the month of the clock in the hemisphere setHemisphere()
// names (north by default), as Atrium's theme.season counts them
// (meteorological: whole months). EMOTE_COLOURS is updated in place when the
// season changes, as in Atrium. setClock(fn) swaps the clock (a function
// returning a Date) the strip, the date, the moon and the season read.

import { Canvas, STATES, BaseMascot, blink, lcg, propKind, streamLines, fd, mod, int, makeTheme } from './common.js';
import { MOODS, word } from './mood.js';

export const PANEL = '#101418';
export const SEASONS = ['spring', 'summer', 'autumn', 'winter'];
export const DEFAULT_SEASON = 'autumn';

// Each season's palette (Atrium's themes/seasons/season/<s>/palette.css s-*
// names) plus the stage's own colours for the tree, the ground and the drift.
export const PALETTES = {
  spring: {
    floor: '#0c110e', surface: '#131b16', card: '#141c17', raised: '#1c2820', rule: '#2c3d32',
    rule_hi: '#46604f', text: '#eee2e8', dim: '#a9b8ad', readout: '#f6edf1', ink: '#1a0c12',
    accent: '#f4a8d0', good: '#7fd66a', warn: '#f2c14e', bad: '#ff6a45', bad_fg: '#ff8a6e',
    hue1: '#a8c8f0', hue2: '#c5b3f2', hue3: '#f2c4a0', epoch: '#f1e8c8',
    crown_bg: ['#3a1f30', '#4a2a3e'], crown_fg: ['#f4a8d0', '#d887b0', '#f6c8de'],
    crown_glyphs: '*o.*o', trunk: '#5a4636', ground_bg: '#14261a', ground_fg: ['#3c6e40', '#5a8a4a'],
    flower: '#f4a8d0', drift: ['#f4a8d0', '#e8b8d0'], drift_glyphs: ".*'",
  },
  summer: {
    floor: '#06110c', surface: '#0b1a13', card: '#0c1b14', raised: '#12261c', rule: '#21402f',
    rule_hi: '#335c45', text: '#e8efda', dim: '#a2b8a2', readout: '#f3f6ea', ink: '#05140f',
    accent: '#5ccfd6', good: '#a3d65c', warn: '#ff9f45', bad: '#ff5c80', bad_fg: '#ff86a0',
    hue1: '#9fc2ff', hue2: '#ecd27a', hue3: '#d8d0f0', epoch: '#f2dc90',
    crown_bg: ['#0f2a18', '#143420'], crown_fg: ['#3e8a4a', '#5aa05a', '#2e6e3a'],
    crown_glyphs: '"\',%"', trunk: '#5a4630', ground_bg: '#0e2416', ground_fg: ['#3a7a3e', '#4e8e44'],
    flower: '#ecd27a', drift: ['#e6f08a', '#c8d860'], drift_glyphs: '.*',
  },
  autumn: {
    floor: '#110b07', surface: '#1b120c', card: '#1c130d', raised: '#281a11', rule: '#45301f',
    rule_hi: '#64462c', text: '#f1e3d0', dim: '#bba78f', readout: '#f8efe3', ink: '#1c0d04',
    accent: '#ee9a62', good: '#86d0a0', warn: '#f0d85c', bad: '#e9506e', bad_fg: '#f47a90',
    hue1: '#a9bfd8', hue2: '#c9a2d8', hue3: '#c8b8a0', epoch: '#f0dfb8',
    crown_bg: ['#3a1a0c', '#4a2410'], crown_fg: ['#ee9a62', '#d0703a', '#e8b048', '#b8502a'],
    crown_glyphs: "*,'*~", trunk: '#4a3426', ground_bg: '#22140c', ground_fg: ['#a85a2a', '#c8782e', '#8a3e1e'],
    flower: '#e8b048', drift: ['#ee9a62', '#d0703a', '#e8b048'], drift_glyphs: ",'~",
  },
  winter: {
    floor: '#070b11', surface: '#0e141e', card: '#0f1520', raised: '#172030', rule: '#2a384e',
    rule_hi: '#415678', text: '#e4ebf4', dim: '#9caabd', readout: '#f1f5fa', ink: '#06111c',
    accent: '#8ccaf2', good: '#6fd8a2', warn: '#f1c45c', bad: '#f26a7c', bad_fg: '#ff8a98',
    hue1: '#c4ccd8', hue2: '#b9a8f0', hue3: '#e6cdb8', epoch: '#eee6c8',
    crown_bg: null, crown_fg: null, crown_glyphs: '', trunk: '#5a5a66', ground_bg: '#141c2a',
    ground_fg: ['#b8c8dc', '#8a9ab4'], flower: '#c4ccd8', drift: ['#dce4f0', '#b8c8dc'], drift_glyphs: '*.',
  },
};
// Sorrel, the same in every season
const FUR = '#b8582a';
const FUR_DIM = '#8a4426';
const OUTLINE = '#e09a62';
const MUZZLE = '#e8d8c0';
const INK = '#1a0c06';
const CHEEK = '#f08a8a';
const SCARF = '#5a3a8a';
const SCARF_FG = '#b9a8f0';
const SWEAT = '#9fd0ff';
const STAR = '#c9d3e0';
const STAR_DIM = '#5a6b86';
const MOON = '#e8e4d0';
const SUN = '#ffe08a';
const FLASH = '#e8edf5';
const RAIN = '#8ab4e0';
const SNOW = '#e4ecf6';
const FOG = '#4a5666';
const CLOUD = '#5a6878';
const NOTEBOOK = '#d8ccb4';
const CONFETTI = ['#f08a8a', '#ffe08a', '#7fd66a', '#8ab4e0', '#c5b3f2'];
const PICADO = ['#ff6fae', '#ffa94d', '#7fd46f', '#6fb6ff', '#c58bff', '#ffd84d'];
const OVERCAST = ['cloud', 'rain', 'storm', 'snow', 'fog'];

const SW = 15;
const SH = 5;
const SORREL = [
  ' /\\     /\\     ',
  '/   ---   \\    ',
  '|         |    ',
  ' \\  .v.  /     ',
  '  (_)---(_)    ',
];
const EARS_FLAT = ' _\\     /_     ';
const EARS_FLICK = ' /\\     |\\     ';
const TAIL = [
  [[2, 12, ' _ '], [3, 11, '( )'], [4, 11, "-'"]],
  [[2, 11, '_  '], [3, 10, ' (\\ '], [4, 11, "-' "]],
];
const CURLED = [
  '               ',
  '               ',
  "  .-'''''-.    ",
  ' (  -  - ~~)   ',
  "  '-.___.-'    ",
];
const EYES = [3, 7];
const SNOUT = [3, 5];
const HEAD_FILL = [[1, 1, 10], [2, 1, 10], [3, 2, 9]];

const TW = 13;
const CROWN = [
  '    #####    ',
  '  #########  ',
  ' ########### ',
  '#############',
  ' ########### ',
  '   #######   ',
];
const BRANCHES = [
  '  \\  \\ / /Y  ',
  ' Y \\  \\|/ /  ',
  '  \\ \\ Y|/ /  ',
  '   \\ \\ |/    ',
  '    \\ \\|/    ',
  '      ||     ',
];
const TRUNK = ['     |:|     ', '     |:|     ', '     |:|     ', '   _/   \\_   '];
const TREE_H = CROWN.length + TRUNK.length;

const CARD_W = 16;
const COMP_W = TW + 1 + SW + 2 + CARD_W;
const WORDS = ['the', 'path', 'bends', 'here', 'a', 'leaf', 'on', 'wind', 'and', 'we',
  'follow', 'tracks', 'by', 'the', 'brook', 'soft', 'moss', 'noted', 'seed', 'sun'];
const CMDS = ['ls', 'dig', 'sniff', 'grep', 'make', 'tree'];
const TODO = ['Sow', 'Water', 'Wait'];
const MONTHS = ['JAN', 'FEB', 'MAR', 'APR', 'MAY', 'JUN', 'JUL', 'AUG', 'SEP', 'OCT', 'NOV', 'DEC'];

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
  calm: [['ear', 2], ['swish', 2], ['sniff', 2], ['pounce', 1], ['rest', 2]],
  happy: [['pounce', 3], ['swish', 2], ['ear', 1]],
  excited: [['pounce', 3], ['swish', 2], ['ear', 2]],
  shy: [['ear', 2], ['rest', 3]],
  curious: [['sniff', 3], ['ear', 2]],
  focused: [['ear', 2], ['rest', 3]],
  worried: [['ear', 2], ['swish', 1], ['rest', 2]],
  sad: [['rest', 1]],
  sleepy: [['yawn', 3], ['rest', 2]],
};

export const MOOD_BUBBLES = {
  happy: 'Bright day in the woods! Where to next?',
  excited: "Ooh, a new trail? Let's go, let's go!",
  shy: "Oh! I didn't hear you on the path… hi ☺",
  curious: "Fresh tracks! Let's see where they lead.",
  focused: "Nose down. I'm following the trail.",
  worried: 'Hmm, the wind just changed…',
  sad: "A grey sort of day. I'll sit by the roots a while.",
  sleepy: 'Curled up in the leaves… zz',
};
// mood.js topics in its words: a gentle young naturalist with a pocket almanac.
export const TOPIC_LINES = {
  thanks: "Aw, thank you. That's going in my field notes.",
  worried: 'Hmm, the tracks look odd here. Want to double-check?',
  frustrated: "Still tangled? Let's back up a few steps and try the other path.",
  relieved: 'Phew, there it is! Sunlight through the branches.',
  determined: "Long trail ahead. I'll keep pace with you.",
  proud: 'We made it to the top! Look at that view.',
  sad: "It's okay. Even the trees rest in winter.",
  happy: "Hooray! My tail won't stop wagging.",
  curious: 'Ooh, a question. Let me check my almanac.',
  rain: 'Rain on the leaves. Good weather for staying in the den.',
  storm: "Thunder! Let's tuck in and save our work.",
  snow: "Snow! Everything's gone quiet and soft ❄",
  fog: "Fog in the hollow. Let's go slowly and stay on the path.",
  sun: 'Sun on my fur. Perfect for a stretch ☀',
  hot: '{0} out there. Find some shade, okay?',
  cold: '{0}! Tuck your paws in.',
  gpu_hot: "GPU at {0}°C. It's getting warm under the fur!",
  cpu_hot: 'CPU at {0}°C! Let it catch its breath?',
  music: 'Is that a song? My ears are dancing ♪',
  game: 'Play time! The almanac can wait.',
  event: 'Heads up: {0} in {1} min.',
  event_now: '{0} is starting now. Off you go!',
  birthday: 'Happy birthday, {0}! I found you a pinecone.',
  holiday: '{0} today. A good day for a long walk!',
  go_home: "{0} already. The sun's going down, time to head home.",
  bedtime: '{0}. Even the owls are yawning. Bed?',
  morning: "Morning! Dew on everything. What's first?",
  lunch: "Lunch? I'd share my berries.",
  weekend: 'Weekend! The trails are all yours.',
  friday: 'Friday afternoon. Almost time to wander.',
  hello: ['Sorrel here! What are we exploring today?',
    'Hi! I kept the path clear for you.',
    "Oh, you're back. Pull up a log and tell me."],
  welcome_back: 'Welcome back! I saved your spot by the roots.',
  wake: ['Huh? Oh! I was just resting my eyes.',
    'Mm… up, up. Leaves out of my ears!'],
  long_prompt: ["That's a long trail of words! Reading…",
    'Big bundle! Let me unpack it.'],
  code: ['Code! Let me sniff it over.',
    'Tracks in the source. Following them.'],
  image: ['A picture! Let me have a look.',
    'Ooh, an image. Pinning it in my notes.'],
  aborted: ['Stopping! Back to the den.',
    "Okay, okay, I'll put it down."],
  tests_pass: ['All green, like new leaves ✓',
    'Tests pass! Bright and clear.'],
  tests_fail: ["A few weeds in the garden. Let's pull them.",
    "Tests failed. We'll find the burr."],
  commit: ["Planted! It'll grow from here.",
    'Committed. Another ring in the tree ✓'],
  fast: ['Quick as a hare!',
    'In and out, like a squirrel.'],
  busy: ['So many trails at once…',
    'Busy, busy, like a beaver.'],
  model: ['A new guide on the trail: {0}.',
    '{0} is leading now. Hello!'],
  poke: ['Hey! That tickles.',
    'Boop! Right on the nose.',
    "Yes? I'm listening, ears up.",
    'Careful, I was napping on that leaf!',
    "Sorrel here. What's up?",
    'Eep! You found me.'],
  poke_spam: ["Okay, okay! I'm awake!",
    "My tail's in a knot!",
    'Too many boops! Hiding in the leaves!'],
  pet: ['Mmm… warm as a summer stone.',
    "That's the good spot behind the ears ☺",
    "Purr… wait, foxes don't purr. Still nice.",
    'Best friend in the whole forest.'],
  rg_memory: '{0} was hoarding all the memory, like acorns. I chased it off.',
  rg_vram: "{0} filled the GPU's den. Shooed it out.",
  rg_game_vram: '{0} is using almost all the GPU memory. Lower the textures?',
  rg_panic: 'Got it! {0} has scampered off.',
  rg_stuck: "Memory's running thin and I can't find the tracks...",
  rg_agents: 'Every agent job is stopped. The sessions are safe in the burrow.',
};
// A few topics in each season's own words; the rest come from TOPIC_LINES.
export const SEASON_LINES = {
  spring: {
    hello: 'Hi! The cherry trees are out. What shall we grow today?',
    morning: 'Morning! The birds started before us.',
    weekend: "Weekend! The meadow's full of flowers.",
    rain: 'A spring shower. Everything smells green.',
    snow: 'Snow in spring? The blossoms are shivering ❄',
    sun: 'Warm sun on new leaves ☀',
    hot: "{0} already? Spring's in a hurry.",
    cold: '{0}. A late frost, mind the buds.',
    pet: 'Mmm… soft as new moss ☺',
  },
  summer: {
    hello: "Hey! Long bright days. What's the plan?",
    morning: "Morning! Let's get going before the heat.",
    weekend: 'Weekend! Go find a lake and some shade.',
    rain: "Thunder rumbling. Let's sit this one out.",
    snow: 'Snow in summer?! Quick, look ❄',
    sun: 'Big summer sun. Water and shade ☀',
    hot: '{0}! Paws in the stream, okay?',
    cold: '{0}? A cool summer night, nice.',
    pet: 'Mmm… like lying in warm grass ☺',
  },
  autumn: {
    hello: "Hi! Crunchy leaves everywhere. Let's get cosy and work.",
    morning: 'Morning! Mist in the hollow and apples in the grass.',
    weekend: 'Weekend! Perfect for a walk through the leaves.',
    rain: 'A soft drizzle. The leaves are glowing.',
    snow: 'First snow on the leaves ❄',
    sun: 'Golden sun through the branches ☀',
    hot: '{0} in autumn? The trees are confused.',
    cold: '{0}. Sweater weather!',
    pet: 'Mmm… like a nest of dry leaves ☺',
  },
  winter: {
    hello: 'Brr, hi! I saved you a warm spot by the fire.',
    morning: 'Morning! Frost on every branch.',
    weekend: 'Weekend! Cocoa and a blanket?',
    rain: 'Sleet. Not quite rain, not quite snow.',
    snow: 'Fresh snow! Want to make tracks? ❄',
    sun: 'Low winter sun. It sparkles on the snow ☀',
    hot: '{0} in winter? Strange days.',
    cold: '{0}! Scarf on, paws tucked.',
    pet: 'Mmm… warmer already ☺',
  },
};

// A season's reaction-sign colours.
function contrastColours(p) {
  return { accent: p.accent, warn: p.warn, bad: p.bad_fg, good: p.good, love: '#ff9cc8', dim: '#a9b4bf' };
}

// The reaction sign over its head, UPDATED IN PLACE when the season changes.
export const EMOTE_COLOURS = contrastColours(PALETTES[DEFAULT_SEASON]);
let emoteSeason = DEFAULT_SEASON;

function setEmoteSeason(season) {
  if (Object.hasOwn(PALETTES, season) && emoteSeason !== season) {
    Object.assign(EMOTE_COLOURS, contrastColours(PALETTES[season]));
    emoteSeason = season;
  }
}

// ---------- the clock, the season, the moon ----------

let clock = () => new Date();
let pin = null;
let hemisphere = 'north';

// setClock swaps the clock (a function returning a Date); null puts the real
// one back.
export function setClock(fn) {
  clock = fn || (() => new Date());
}

// setSeason pins a season (null, or anything not in SEASONS, unpins).
export function setSeason(s) {
  pin = SEASONS.includes(s) ? s : null;
}

// setHemisphere: "south" turns the seasons six months on; anything else is north.
export function setHemisphere(h) {
  hemisphere = h === 'south' ? 'south' : 'north';
}

// Month (1-12) -> season, northern hemisphere: meteorological, whole months.
const NORTH = [null, 'winter', 'winter', 'spring', 'spring', 'spring', 'summer', 'summer', 'summer',
  'autumn', 'autumn', 'autumn', 'winter'];

// seasonFor is the season of month 1-12 in a hemisphere.
export function seasonFor(month, hemi = 'north') {
  return hemi === 'south' ? NORTH[mod(month + 5, 12) + 1] : NORTH[month];
}

// currentSeason is the season drawn now: the pin, else the clock's month.
export function currentSeason() {
  return pin || seasonFor(clock().getMonth() + 1, hemisphere) || DEFAULT_SEASON;
}

const MOON_EPOCH = Date.UTC(2000, 0, 6, 18, 14); // a new moon
const SYNODIC = 29.530588853; // days, new moon to new moon

// moonGlyph is the moon's phase as one cell: "." new, ")" waxing, "O" full,
// "(" waning (as seen from the north; a hint, not an ephemeris).
export function moonGlyph(when = clock()) {
  const age = mod((when.getTime() - MOON_EPOCH) / 1000 / 86400.0, SYNODIC);
  if (age < 1.85 || age >= SYNODIC - 1.85) return '.';
  if (age < SYNODIC / 2 - 1.85) return ')';
  if (age < SYNODIC / 2 + 1.85) return 'O';
  return '(';
}

// almanacDate is the strip's date: AUTUMN - 24 OCT.
export function almanacDate(season, today = clock()) {
  return `${season.toUpperCase()} - ${today.getDate()} ${MONTHS[today.getMonth()]}`;
}

function hhmm() {
  const d = clock();
  return ` ${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')} `;
}

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

const has = (scene, tags) => tags.some((t) => scene.has(t));

export const STATUS = {
  idle: 'resting', thinking: 'musing', tool: 'foraging', writing: 'writing', done: 'found it', error: 'oops',
};

// ---------- the mascot ----------

class Mascot extends BaseMascot {
  constructor(cols = 64, rows = 18) {
    super(cols, rows);
    this.cols = Math.max(int(cols), COMP_W + 1);
    this.rows = Math.max(int(rows), TREE_H + 4);
    this.season = DEFAULT_SEASON;
    this.ground = this.rows - 2;
    const cx = fd(this.cols - COMP_W, 2);
    this.tx = this.cols < 90 ? cx : Math.max(1, fd(this.cols, 3) - TW - 4);
    this.sx = Math.max(this.tx + TW + 1, cx + TW + 1);
    this.px = this.sx + SW + 2;
    this.stream = streamLines(WORDS, 12, 59);
    this.stars = [];
    for (let i = 0; i < fd(this.cols * this.rows, 30); i++) {
      const r = lcg(227, i);
      this.stars.push([int(r() * this.cols), int(r() * Math.max(2, this.ground - 3)),
        20 + int(r() * 30), int(r() * 50)]);
    }
    this.crown = [];
    CROWN.forEach((row, y) => {
      Array.from(row).forEach((c, x) => {
        if (c === '#') this.crown.push([x, y, lcg(229, x, y)()]);
      });
    });
    this.groundNoise = [];
    for (let x = 0; x < this.cols; x++) this.groundNoise.push(lcg(233, x)());
  }

  bubble(state, tool = null, mood = null) {
    if (state === 'idle' && Object.hasOwn(MOOD_BUBBLES, mood || '')) return MOOD_BUBBLES[mood];
    return super.bubble(state, tool);
  }

  seasonNow() {
    const s = currentSeason();
    this.season = s;
    setEmoteSeason(s);
    return s;
  }

  seasonIs(s) {
    return this.season === s;
  }

  // ---------- composition ----------

  render(state, tick, tool = null, mood = null, scene = new Set()) {
    if (!STATES.includes(state)) state = 'idle';
    if (!MOODS.includes(mood)) mood = 'calm';
    if (mood === 'sleepy' && ['thinking', 'tool', 'writing'].includes(state)) mood = 'focused';
    tick = int(tick);
    const season = this.seasonNow();
    const p = PALETTES[season];
    const cv = new Canvas(this.cols, this.rows);
    let kind = state === 'tool' ? propKind(tool) : null;
    if (state === 'tool' && (tool === 'WebSearch' || tool === 'WebFetch')) kind = 'kite';
    const idle = state === 'idle';
    const fidget = idle ? this.fidget(tick, mood) : null;
    const asleep = idle && mood === 'sleepy';
    const cold = season === 'winter' || scene.has('cold');
    this.sky(cv, p, state, tick, scene);
    if (scene.has('festive')) this.banner(cv, tick);
    this.tree(cv, p, season, tick, scene);
    this.groundRow(cv, p, season, tick, scene);
    this.weather(cv, p, season, tick, scene);
    if (scene.has('party')) this.confetti(cv, tick);
    this.strip(cv, p, season, state, tick, scene);
    let hop;
    if (state === 'done') hop = [0, 1, 2, 2, 1, 0, 0, 0][mod(tick, 8)];
    else if (idle && mood === 'excited' && mod(fd(tick, 30), 2)) hop = [0, 1, 2, 1, 0, 0][mod(tick, 6)];
    else if (fidget === 'pounce') hop = [0, 1, 2, 1, 0, 0][Math.min(5, fd(mod(tick, 90) - 72, 2))];
    else if (scene.has('music') && !asleep) hop = mod(fd(tick, 4), 2);
    else hop = 0;
    const top = this.ground - SH - hop;
    const ox = this.sx;
    this.head = [Math.min(ox + 10, this.cols - 3), Math.max(0, top - 1)];
    if (kind === 'helpers') this.sparrows(cv, p, tick);
    this.sorrel(cv, p, state, tick, ox, top, mood, scene, fidget, asleep, cold);
    this.marks(cv, p, state, tick, ox, top, mood, scene, fidget, asleep, cold);
    if (scene.has('party') && !asleep) this.hat(cv, tick, ox, top);
    const px = this.px;
    const py = this.ground - 8;
    if (state === 'thinking') this.musing(cv, p, tick, px, py);
    else if (state === 'writing') {
      this.notebook(cv, p, tick, ox, top);
      this.doc(cv, p, tick, px, py, true, 'FIELD NOTES');
    } else if (state === 'done') this.found(cv, p, tick, px, py, ox);
    else if (state === 'error') this.oops(cv, p, tick, px, py);
    else if (kind && kind !== 'helpers') this[`prop_${kind}`](cv, p, tick, px, py);
    else if (scene.has('game')) this.controller(cv, p, tick, px, py);
    return cv;
  }

  fidget(tick, mood) {
    if (mod(tick, 90) < 72) return null;
    const table = FIDGETS[mood] || FIDGETS.calm;
    let pick = lcg(239, fd(tick, 90))() * table.reduce((s, [, w]) => s + w, 0);
    for (const [name, weight] of table) {
      pick -= weight;
      if (pick < 0) return name === 'rest' ? null : name;
    }
    return null;
  }

  // ---------- the stage ----------

  sky(cv, p, state, tick, scene) {
    const overcast = has(scene, OVERCAST);
    const night = scene.has('night');
    const span = Math.max(1, this.ground);
    for (let y = 0; y < this.rows; y++) {
      const bg = mix(p.floor, PANEL, Math.min(1.0, y / span));
      for (let x = 0; x < this.cols; x++) cv.recolour(x, y, null, bg);
    }
    const y0 = scene.has('festive') ? 2 : 0;
    const sun = !night && !overcast && state !== 'error' && scene.has('sun');
    if (night || !overcast) {
      for (const [x, y, period, ph] of this.stars) {
        if (overcast && mod(x + y, 3)) continue;
        if (!night && mod(x * 7 + y, 3)) continue;
        if (y < y0 || (sun && x >= this.cols - 8 && y < y0 + 4)) continue;
        const t = mod(tick + ph, period);
        cv.cell(x, y, t < 5 ? '.+*+.'[t] : '.', t < 5 ? STAR : STAR_DIM);
      }
    }
    if (night && !overcast) cv.cell(this.cols - 4, y0 + 1, moonGlyph(clock()), MOON);
    if (sun) {
      const x = this.cols - 7;
      const rays = ['\\ | /', '  |  ', '\\   /'][mod(fd(tick, 8), 3)];
      cv.put(x, y0, rays, SUN);
      cv.put(x, y0 + 1, '-(O)-', SUN);
      cv.put(x, y0 + 2, rays[0] === '\\' ? '/ | \\' : '  |  ', SUN);
    }
    if (overcast) {
      const heavy = scene.has('rain') || scene.has('storm');
      for (let i = 0; i < Math.max(2, fd(this.cols, 16)); i++) {
        const r = lcg(241, i, this.cols);
        const w = 5 + int(r() * 7);
        const y = y0 + int(r() * 3);
        const x = mod(int(r() * this.cols + tick * (0.05 + r() * 0.04)), this.cols + w) - w;
        for (let c = 0; c < w; c++) {
          if (cv.get(x + c, y) === ' ') cv.cell(x + c, y, (c + i) % 3 ? '~' : '-', heavy ? FOG : CLOUD);
        }
      }
    }
  }

  tree(cv, p, season, tick, scene) {
    const x0 = this.tx;
    const base = this.ground - 1;
    const ty = base - TREE_H + 1;
    TRUNK.forEach((row, i) => {
      const y = ty + CROWN.length + i;
      Array.from(row).forEach((ch, c) => {
        if (ch !== ' ') cv.cell(x0 + c, y, ch, p.trunk, ch === ':' ? '#1e140e' : null);
      });
    });
    if (season === 'winter') {
      BRANCHES.forEach((row, i) => {
        Array.from(row).forEach((ch, c) => {
          if (ch !== ' ') {
            cv.cell(x0 + c, ty + i, ch, p.trunk);
            if (i > 0 && BRANCHES[i - 1][c] === ' ' && (c + i) % 2 === 0) cv.cell(x0 + c, ty + i - 1, '.', SNOW);
          }
        });
      });
      return;
    }
    const sway = mod(fd(tick, 24), 2) && scene.has('storm') ? 1 : 0;
    for (const [c, r, noise] of this.crown) {
      const x = x0 + c + (r < 2 ? sway : 0);
      const y = ty + r;
      const bg = p.crown_bg[int(noise * 7) % 2];
      const glyphs = p.crown_glyphs;
      if (noise < 0.55) {
        let g = glyphs[int(noise * 97) % glyphs.length];
        const fg = p.crown_fg[int(noise * 31) % p.crown_fg.length];
        if ((season === 'spring' || season === 'autumn') && mod(fd(tick, 6) + c * 3 + r, 23) === 0) g = '*';
        cv.cell(x, y, g, fg, bg);
      } else {
        cv.cell(x, y, ' ', null, bg);
      }
    }
  }

  groundRow(cv, p, season, tick, scene) {
    const y = this.ground;
    const snow = season === 'winter' || scene.has('snow');
    for (let x = 0; x < this.cols; x++) {
      const n = this.groundNoise[x];
      const bg = snow ? '#1a2230' : p.ground_bg;
      let ch;
      let fg;
      if (snow) {
        ch = n < 0.5 ? '_' : (n < 0.8 ? '~' : '.');
        fg = season === 'winter' ? p.ground_fg[0] : SNOW;
      } else {
        ch = [',', "'", '"', '.'][int(n * 4)];
        fg = p.ground_fg[int(n * 13) % p.ground_fg.length];
        if (season === 'spring' && n > 0.86) {
          ch = '*';
          fg = p.flower;
        } else if (season === 'summer' && n > 0.92) {
          ch = '*';
          fg = p.flower;
        }
      }
      cv.cell(x, y, ch, fg, bg);
    }
    if (season === 'autumn' && !scene.has('snow')) {
      for (let c = 0; c < TW; c++) {
        const x = this.tx + c;
        if (c >= 2 && c <= 10) {
          const n = this.groundNoise[mod(x, this.cols)];
          const leaf = c === 4 || c === 8;
          cv.cell(x, leaf ? y - 1 : y, ",'~"[int(n * 3)],
            p.ground_fg[(c + int(n * 5)) % p.ground_fg.length], leaf ? null : p.ground_bg);
        }
      }
    }
  }

  weather(cv, p, season, tick, scene) {
    const night = scene.has('night');
    const top = scene.has('festive') ? 2 : 0;
    const span = Math.max(2, this.ground - top);
    let precip = null;
    if (scene.has('snow')) precip = 'snow';
    else if (scene.has('storm')) precip = 'storm';
    else if (scene.has('rain')) precip = { spring: 'shower', summer: 'storm', autumn: 'drizzle', winter: 'sleet' }[season];
    if (precip === null) {
      if (!(season === 'summer' && !night)) {
        const n = { spring: 6, summer: 7, autumn: 7, winter: 10 }[season];
        for (let i = 0; i < Math.max(3, fd(this.cols * n, 64)); i++) {
          const r = lcg(251, i);
          if (season === 'summer') {
            const x = mod(int(r() * this.cols + Math.sin(tick / 9.0 + i) * 2), this.cols);
            const y = this.ground - 1 - int(r() * Math.min(6, span - 1));
            const period = 14 + int(r() * 12);
            if (mod(tick + int(r() * period), period) < fd(period, 2) && cv.get(x, y) === ' ') {
              cv.cell(x, y, mod(tick + i, 5) === 0 ? '*' : '.', p.drift[i % 2]);
            }
            continue;
          }
          const pos = r() * span + tick * (0.08 + r() * 0.06);
          const fall = int(fd(pos, span));
          const y = top + int(pos - fall * span);
          const x = mod(int(lcg(253, i, fall)() * this.cols + Math.sin(tick / 7.0 + i) * 1.5), this.cols);
          if (y < this.ground && cv.get(x, y) === ' ') {
            const g = p.drift_glyphs[(i + fall) % p.drift_glyphs.length];
            cv.cell(x, y, g, p.drift[(i + fall) % p.drift.length]);
          }
        }
      }
    } else {
      const dense = { snow: 10, storm: 16, shower: 7, drizzle: 6, sleet: 10 }[precip];
      for (let i = 0; i < Math.max(4, fd(this.cols * dense, 64)); i++) {
        const r = lcg(257, i);
        const speed = precip === 'snow' ? 0.12 : (precip === 'storm' ? 0.5 : 0.35);
        const pos = r() * span + tick * (speed + r() * 0.1);
        const fall = int(fd(pos, span));
        const y = top + int(pos - fall * span);
        const drift = precip === 'storm' ? fd(tick, 3) : 0;
        const x = mod(int(lcg(259, i, fall)() * this.cols + drift * 0.3), this.cols);
        if (y >= this.ground || cv.get(x, y) !== ' ') continue;
        if (precip === 'snow') cv.cell(x, y, '*.'[i % 2], SNOW);
        else if (precip === 'storm') cv.cell(x, y, '/', RAIN);
        else if (precip === 'shower') cv.cell(x, y, "'", p.hue1);
        else if (precip === 'drizzle') cv.cell(x, y, '.', p.hue1);
        else cv.cell(x, y, ',*'[i % 2], i % 2 === 0 ? RAIN : SNOW);
      }
      if (precip === 'storm' || scene.has('storm')) {
        const cyc = fd(tick, 37);
        const t = mod(tick, 37);
        if (t < 3) {
          const x = int(lcg(263, cyc)() * (this.cols - 4)) + 2;
          Array.from('\\/\\').forEach((ch, k) => cv.cell(x + (k % 2), top + 1 + k, ch, FLASH));
        }
      }
    }
    if (scene.has('fog')) {
      for (let k = 0; k < 2; k++) {
        const off = mod(fd(tick, 7 + k * 3) + k * 5, 10);
        const y = this.ground - 2 - k * 3;
        for (let x = 0; x < this.cols; x++) {
          if ((x + off) % 10 < 5 && cv.get(x, y) === ' ') cv.cell(x, y, '~', FOG);
        }
      }
    }
    if (scene.has('cold')) {
      for (let x = 0; x < this.cols; x += 5) cv.cell(x + mod(fd(tick, 20), 2), this.ground, '*', SNOW, null);
    }
  }

  strip(cv, p, season, state, tick, scene) {
    const y = this.rows - 1;
    cv.fill(0, y, this.cols, 1, p.raised, ' ', null, false);
    cv.recolour(0, y, null, p.accent);
    const now = clock();
    const date = ` ${almanacDate(season, now)} `;
    cv.put(1, y, date, p.hue2, false, p.rule);
    let x = 1 + date.length + 1;
    cv.cell(x, y, moonGlyph(now), MOON, p.raised);
    x += 2;
    const colour = { done: p.good, error: p.bad_fg }[state] || (state === 'idle' ? p.dim : p.accent);
    cv.put(x, y, STATUS[state] || 'resting', colour, false, p.raised);
    const clk = hhmm();
    const ring = scene.has('clock') && mod(fd(tick, 5), 2);
    const cx = this.cols - clk.length;
    if (cx > x + 10) {
      cv.put(cx, y, clk, ring ? p.ink : p.text, false, ring ? p.warn : p.rule);
      if (scene.has('music')) cv.cell(cx - 2, y, 'd', p.accent, p.raised);
    }
    if (scene.has('clock') && mod(fd(tick, 10), 3)) {
      const text = ' (!) REMINDER ';
      const bx = Math.max(0, this.cols - text.length - 1);
      cv.put(bx, y - 2, text, p.ink, false, p.warn);
      cv.cell(this.cols - 4, y - 1, 'V', p.warn);
    }
  }

  banner(cv, tick) {
    for (let x = 0; x < this.cols; x++) {
      cv.cell(x, 0, '-', '#a9b4bf');
      const k = x % 4;
      if (k < 3) {
        const flutter = mod(fd(tick, 4) + fd(x, 4), 2);
        cv.cell(x, 1, ['o.o', '.o.'][flutter][k], PANEL, PICADO[fd(x, 4) % PICADO.length]);
      }
    }
  }

  confetti(cv, tick) {
    const span = this.rows + 2;
    for (let i = 0; i < Math.max(4, fd(this.cols * this.rows, 90)); i++) {
      const r = lcg(269, i);
      const pos = r() * span + tick * (0.10 + r() * 0.08);
      const fall = int(fd(pos, span));
      const y = int(pos - fall * span) - 1;
      const x = int(lcg(271, i, fall)() * this.cols) + mod(fd(tick, 4) + i, 2);
      if (y < this.rows - 1 && cv.get(x, y) === ' ') {
        cv.cell(x, y, ",'.*"[mod(fd(tick, 3) + i, 4)], CONFETTI[i % CONFETTI.length]);
      }
    }
  }

  // ---------- Sorrel ----------

  sorrel(cv, p, state, tick, x0, top, mood, scene, fidget, asleep, cold) {
    const idle = state === 'idle';
    const tired = state === 'error' || (idle && mood === 'sad');
    const fur = tired || asleep ? FUR_DIM : FUR;
    const t = mod(tick, 90) - 72;
    if (asleep) {
      CURLED.forEach((row, i) => cv.sprite(x0, top + i, row, OUTLINE));
      for (let x = x0 + 3; x < x0 + 10; x++) cv.recolour(x, top + 3, null, fur);
      for (const x of [x0 + 8, x0 + 9]) cv.recolour(x, top + 3, MUZZLE, fur);
      for (const x of [x0 + 5, x0 + 8]) {
        if (cv.get(x, top + 3) === '-') cv.recolour(x, top + 3, INK, fur);
      }
      if (cold) cv.put(x0 + 3, top + 4, '~~~', SCARF_FG, false, SCARF);
      return;
    }
    const rows = [...SORREL];
    if (state === 'error' || (idle && mood === 'sad')) rows[0] = EARS_FLAT;
    else if (fidget === 'ear' && mod(fd(t, 3), 2)) rows[0] = EARS_FLICK;
    rows.forEach((row, i) => cv.sprite(x0, top + i, row, OUTLINE));
    for (const [r, a, b] of HEAD_FILL) for (let x = x0 + a; x < x0 + b; x++) cv.recolour(x, top + r, null, fur);
    for (let x = x0 + 4; x < x0 + 7; x++) cv.recolour(x, top + 3, INK, MUZZLE);
    let frame = 0;
    if (state === 'writing' || (state === 'done' && mod(tick, 4) < 2)) frame = mod(fd(tick, 2), 2);
    else if (fidget === 'swish') frame = mod(fd(t, 3), 2);
    for (const [r, c, text] of TAIL[frame]) cv.sprite(x0 + c, top + r, text, OUTLINE);
    for (const [r, c, text] of TAIL[frame]) {
      Array.from(text).forEach((ch, k) => {
        if (ch === '(') {
          cv.recolour(x0 + c + k + 1, top + r, null, fur);
          cv.cell(x0 + c + k + 1, top + r, ' ', null, r === 3 ? MUZZLE : fur);
        }
      });
    }
    // the face
    const eye = state === 'done' ? '^' : 'o';
    let gaze = state === 'tool' || state === 'writing' ? 1 : 0;
    if (state === 'thinking') gaze = [0, 1, 0, -1][mod(fd(tick, 10), 4)];
    let snout = { done: 'D', error: '~', thinking: 'o' }[state] || 'v';
    if (state === 'writing') snout = 'vov-'[mod(fd(tick, 2), 4)];
    let left = eye;
    let right = eye;
    let cheek = state === 'done' || state === 'idle' ? '~' : '';
    if (idle && Object.hasOwn(MOOD_FACE, mood)) {
      [left, right, snout, cheek] = MOOD_FACE[mood];
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
    if (fidget === 'yawn') {
      left = '-';
      right = '-';
      snout = t >= 3 && t < 14 ? 'O' : 'o';
    } else if (fidget === 'sniff') {
      snout = mod(fd(t, 2), 2) ? 'o' : 'v';
    }
    const shut = blink(tick, 47) && state !== 'done' && state !== 'error';
    [left, right].forEach((ch, k) => cv.cell(x0 + EYES[k] + gaze, top + 2, shut ? '-' : ch, INK, fur));
    cv.cell(x0 + SNOUT[1], top + SNOUT[0], snout, INK, MUZZLE);
    if (Object.hasOwn(BROWS, mood) && state !== 'done') {
      BROWS[mood].forEach((ch, k) => cv.cell(x0 + EYES[k] + gaze, top + 1, ch, OUTLINE, fur));
    }
    if (cheek) {
      cv.cell(x0 + 2, top + 3, cheek, CHEEK, fur);
      cv.cell(x0 + 8, top + 3, cheek, CHEEK, fur);
    }
    if (cold) {
      cv.put(x0 + 5, top + 4, '~~~', SCARF_FG, false, SCARF);
      cv.cell(x0 + 1, top + 4, '{', SCARF_FG, null);
    }
  }

  marks(cv, p, state, tick, ox, top, mood, scene, fidget, asleep, cold) {
    const idle = state === 'idle';
    const t = mod(tick, 90) - 72;
    if (asleep) {
      const s = mod(tick, 36);
      for (let j = 0; j < 3; j++) {
        if (fd(s, 9) > j) cv.cell(ox + 11 + j, top + 1 - j, 'zzZ'[j], j < 2 ? p.dim : p.text);
      }
    } else if (idle && mood === 'curious' && mod(fd(tick, 5), 4)) {
      cv.cell(ox + 11, top, '?', p.accent);
    } else if (idle && mood === 'excited' && mod(fd(tick, 3), 4) < 2) {
      cv.cell(ox, top, '!', p.warn);
      cv.cell(ox + 11, top, '!', p.warn);
    } else if (mood === 'excited' && ['thinking', 'tool', 'writing', 'done'].includes(state)) {
      if (mod(fd(tick, 3), 4) === 0) cv.cell(ox + 11, top, '+', SUN);
    }
    if (state === 'thinking') {
      const leaf = this.seasonIs('winter') ? PALETTES.summer.crown_fg[1] : p.drift[0];
      for (let j = 0; j < 3; j++) {
        const s = mod(fd(tick, 3) + j * 3, 9);
        if (s < 7) {
          cv.cell(ox + 10 + j + fd(s, 3), top - 1 - fd(s, 2), ".o,'"[(j + s) % 4], j ? leaf : p.hue2);
        }
      }
    }
    if (state === 'error') {
      cv.put(ox + 2, top - 3, '.--.', CLOUD);
      cv.put(ox + 1, top - 2, '(____)', CLOUD);
      for (let k = 0; k < 3; k++) {
        if (mod(fd(tick, 2) + k, 3) === 0) cv.cell(ox + 2 + k * 2, top - 1, "'", RAIN);
      }
    }
    if (cold && !asleep && state !== 'error') {
      const s = mod(tick, 24);
      if (s < 12) cv.cell(ox + 3 - fd(s, 4), top + 2 - fd(s, 4), '.o'[fd(s, 6) % 2], p.dim);
    }
    if (state === 'error' || ((mood === 'worried' || scene.has('hot')) && state !== 'done' && !asleep)) {
      const s = mod(fd(tick, 5), 4);
      if (s < 3) cv.cell(ox + 11, top + 1 + s, "',."[s], SWEAT);
    }
    if (idle && mood === 'sad') {
      const s = mod(fd(tick, 6), 3);
      if (s < 2) cv.cell(ox + EYES[0], top + 3 + s, ",'"[s], SWEAT, s === 0 ? FUR_DIM : null);
    }
    if (fidget === 'sniff') cv.cell(ox - 1, top + 4, ',', p.drift[0]);
    if (fidget === 'pounce' && t < 12) {
      const g = this.seasonIs('winter') ? '*' : '.';
      cv.cell(ox + 6, top - 2 + fd(t, 6), g, p.drift[0]);
    }
    if (scene.has('music')) {
      for (let j = 0; j < 2; j++) {
        const s = mod(tick + j * 12, 24);
        if (s < 20) cv.cell(ox - 1 - fd(s, 6) - j * 2, top + 4 - fd(s, 5), 'db'[j], [p.accent, p.hue2][j]);
      }
    }
    if (scene.has('hot')) {
      for (let k = 0; k < 3; k++) {
        if (lcg(277, k, fd(tick, 3))() < 0.6) {
          const x = ox + 2 + k * 4 + mod(fd(tick, 3), 2);
          if (cv.get(x, this.ground - 1) === ' ') cv.cell(x, this.ground - 1, '~', p.warn);
        }
      }
    }
  }

  hat(cv, tick, ox, top) {
    const x = ox + 5;
    cv.cell(x, top - 3, '*+'[mod(fd(tick, 4), 2)], SUN);
    cv.put(x - 1, top - 2, '/:\\', CONFETTI[0]);
    cv.put(x - 2, top - 1, '/_:_\\', CONFETTI[3]);
    cv.cell(x, top - 2, ':', SUN);
    cv.cell(x, top - 1, ':', SUN);
  }

  // ---------- things beside it ----------

  card(cv, p, x, y, w, h, title, status = null, statusColour = null, body = null) {
    body = body || p.raised;
    cv.fill(x, y, w, h, body);
    cv.fill(x, y, w, 1, p.rule);
    cv.recolour(x, y, null, p.accent);
    cv.put(x + 2, y, title.slice(0, Math.max(0, w - 3)), p.hue2, false, p.rule);
    if (status && title.length + status.length + 4 <= w) {
      cv.put(x + w - status.length - 1, y, status, statusColour || p.good, false, p.rule);
    }
  }

  musing(cv, p, tick, x, y) {
    this.card(cv, p, x, y + 1, CARD_W, 5, 'ALMANAC');
    cv.put(x + 2, y + 3, '-\\|/'[mod(fd(tick, 3), 4)], p.accent, false, p.raised);
    cv.put(x + 4, y + 3, 'wind & leaf', p.dim, false, p.raised);
    cv.put(x + 2, y + 4, ('hmm' + '.'.repeat(mod(fd(tick, 4), 4))).padEnd(12), p.text, false, p.raised);
  }

  doc(cv, p, tick, x, y, stream = false, title = 'READ', status = null) {
    this.card(cv, p, x, y, CARD_W, 7, title, status, p.accent);
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
      cv.put(x + 2, y + 1 + k, text, k === 4 ? p.text : p.dim, false, p.raised);
    }
    if (stream && mod(fd(tick, 3), 2)) cv.cell(x + 2 + Math.min(col, 11), y + 5, '_', p.accent, p.raised);
  }

  notebook(cv, p, tick, ox, top) {
    const x = ox - 4;
    const y = top + 3;
    cv.put(x, y + 1, '[==]', INK, false, NOTEBOOK);
    cv.cell(x + 1 + mod(fd(tick, 2), 2), y, '/', p.trunk);
  }

  found(cv, p, tick, x, y, ox) {
    this.card(cv, p, x, y + 1, CARD_W, 5, 'FOUND IT');
    cv.put(x + 1, y + 3, 'v/', p.good, false, p.raised);
    cv.put(x + 4, y + 3, 'all written', p.text, false, p.raised);
    cv.put(x + 4, y + 4, 'down', p.dim, false, p.raised);
    cv.put(ox - 3, this.ground - 1, 'v/', p.good);
    const r = lcg(281, fd(tick, 3));
    for (let k = 0; k < 4; k++) {
      const sx = x + int(r() * CARD_W);
      const sy = y + int(r() * 7);
      if (cv.get(sx, sy) === ' ') cv.cell(sx, sy, '+*.'[int(r() * 3)], STAR);
    }
  }

  oops(cv, p, tick, x, y) {
    const on = mod(fd(tick, 4), 2);
    cv.fill(x, y + 1, CARD_W, 5, p.raised);
    cv.fill(x, y + 1, CARD_W, 1, on ? p.warn : p.rule);
    cv.put(x + 1, y + 1, 'OOPS', on ? p.ink : p.warn, false, on ? p.warn : p.rule);
    cv.put(x + 1, y + 3, '(x)', p.bad_fg, false, p.raised);
    cv.put(x + 5, y + 3, 'brambles', p.bad_fg, false, p.raised);
    cv.put(x + 5, y + 4, 'try again?', p.dim, false, p.raised);
  }

  controller(cv, p, tick, x, y) {
    this.card(cv, p, x, y + 1, CARD_W, 6, 'PLAY');
    cv.put(x + 3, y + 3, '.-----.', p.dim, false, p.raised);
    cv.put(x + 2, y + 4, '( +  oo )', p.dim, false, p.raised);
    cv.put(x + 3, y + 5, "'-' '-'", p.dim, false, p.raised);
    cv.cell(x + 4, y + 4, '+', p.text, p.raised);
    cv.cell(x + 7, y + 4, 'o', p.good, p.raised);
    cv.cell(x + 8, y + 4, 'o', [p.accent, p.bad_fg, p.good, p.warn][mod(fd(tick, 3), 4)], p.raised);
  }

  sparkles(cv, p, tick, x, y, n = 5) {
    for (let j = 0; j < n; j++) {
      const r = lcg(283, j);
      const period = 24 + int(r() * 16);
      const t = mod(tick + int(r() * period), period);
      const sx = x + int(r() * 12);
      const sy = y - int(r() * 5);
      if (t < fd(period * 2, 3) && cv.get(sx, sy) === ' ') {
        cv.cell(sx, sy, '*+.'[mod(fd(t, 4) + j, 3)], [p.drift[0], STAR, p.accent][j % 3]);
      }
    }
  }

  prop_bash(cv, p, tick, x, y) {
    this.card(cv, p, x, y, CARD_W, 7, 'NOTEBOOK', 'RUN', p.accent, p.floor);
    const cyc = fd(tick, 30);
    const cmd = CMDS[mod(cyc, CMDS.length)];
    const n = Math.min(cmd.length, fd(mod(tick, 30), 2));
    for (let k = 0; k < 3; k++) {
      const prev = CMDS[mod(cyc - 3 + k, CMDS.length)];
      cv.put(x + 1, y + 1 + k, ('> ' + prev).slice(0, 14), p.dim, false, p.floor);
    }
    cv.put(x + 1, y + 5, ('> ' + cmd.slice(0, n)).slice(0, 14), p.text, false, p.floor);
    if (mod(fd(tick, 3), 2)) cv.cell(x + 3 + n, y + 5, '_', p.accent, p.floor);
  }

  prop_book(cv, p, tick, x, y) {
    this.doc(cv, p, tick, x, y, false, 'LEAF BOOK');
    const hot = mod(fd(tick, 3), 5);
    for (let c = 0; c < 14; c++) cv.recolour(x + 1 + c, y + 1 + hot, p.text, p.rule_hi);
    cv.cell(x + CARD_W - 2, y + 1, '&', p.crown_fg ? p.crown_fg[0] : p.hue2, p.raised);
  }

  prop_pad(cv, p, tick, x, y) {
    this.doc(cv, p, tick, x, y, true, 'FIELD NOTES');
  }

  prop_lens(cv, p, tick, x, y) {
    this.card(cv, p, x, y, CARD_W, 7, 'LEAF PILE', 'SNIFF', p.accent);
    for (let r = 0; r < 4; r++) {
      for (let c = 0; c < 14; c++) {
        const n = lcg(287, r, c)();
        if (n < 0.55) {
          cv.cell(x + 1 + c, y + 2 + r, ",'~"[int(n * 5) % 3], p.drift[int(n * 11) % p.drift.length], p.raised);
        }
      }
    }
    const lx = x + 1 + mod(fd(tick, 2), 12);
    const ly = y + 2 + mod(fd(tick, 8), 3);
    cv.put(lx, ly, '(o)', p.text, false, p.rule_hi);
    cv.cell(lx + 3, ly + 1, '\\', p.dim, p.raised);
  }

  prop_kite(cv, p, tick, x, y) {
    this.card(cv, p, x, y + 2, CARD_W, 5, 'KITE POST', 'AIR', p.accent);
    const kx = x + 9 + (Math.sin(tick / 5.0) > 0 ? 1 : 0);
    const ky = Math.max(0, y - 2);
    cv.put(kx, ky, '/\\', p.accent);
    cv.put(kx, ky + 1, '\\/', p.accent);
    for (let k = 0; k < 2; k++) {
      cv.cell(kx - 1 - k, ky + 2 + k, mod(fd(tick, 3) + k, 2) ? '\\' : '.', p.dim);
    }
    cv.put(x + 2, y + 4, 'page.html', p.text, false, p.raised);
    const pct = mod(tick * 2, 101);
    const bw = 12;
    const filled = int(pct / 100.0 * bw);
    for (let c = 0; c < bw; c++) {
      cv.cell(x + 2 + c, y + 5, c < filled ? '=' : '-', c < filled ? p.accent : p.rule_hi, p.raised);
    }
  }

  sparrows(cv, p, tick) {
    [this.sx + 4, Math.min(this.cols - 3, this.px + CARD_W + 1)].forEach((x, n) => {
      const y = this.ground - 9 - (Math.sin(tick / 4.0 + n * 2) > 0 ? 1 : 0);
      const wing = mod(fd(tick, 2) + n, 2) ? 'v' : '^';
      cv.put(x, y, wing + 'o' + wing, p.dim);
      cv.cell(x + 1, y, 'o', p.hue3);
      cv.put(x, y + 1, '[=]', INK, false, NOTEBOOK);
    });
    const px = this.px;
    const py = this.ground - 8;
    this.card(cv, p, px, py, CARD_W, 5, 'MESSENGERS', null);
    const k = mod(fd(tick, 8), 3);
    cv.put(px + 1, py + 2, ('flying' + '.'.repeat(k)).padEnd(11), p.text, false, p.raised);
    cv.put(px + 1, py + 3, '2 sparrows', p.dim, false, p.raised);
  }

  prop_todo(cv, p, tick, x, y) {
    this.card(cv, p, x, y, CARD_W, 5, 'SEED PACKETS');
    const done = 1 + mod(fd(tick, 12), TODO.length);
    TODO.forEach((item, k) => {
      const mark = k < done ? '[x]' : '[ ]';
      cv.put(x + 1, y + 1 + k, mark, k < done ? p.good : p.dim, false, p.raised);
      cv.put(x + 5, y + 1 + k, item, k < done ? p.dim : p.text, false, p.raised);
    });
  }

  prop_wand(cv, p, tick, x, y) {
    for (let j = 0; j < 4; j++) {
      const a = tick / 6.0 + j * 1.57;
      const fx = x + 6 + int(pyRound(Math.cos(a) * 5));
      const fy = y + 4 + int(pyRound(Math.sin(a) * 2));
      if (cv.get(fx, fy) === ' ') cv.cell(fx, fy, j === mod(fd(tick, 4), 4) ? '*' : '.', '#e6f08a');
    }
    this.sparkles(cv, p, tick, x + 2, y + 6, 5);
  }
}
Mascot.BUBBLES = {
  idle: 'Sorrel here, under the old tree. What shall we look into?',
  thinking: 'Hmm, let me think it through…',
  writing: 'Writing it down in my field notes…',
  done: 'Found it! All written down ✓',
  error: 'Oops, a tangle in the brambles. Try again?',
};
Mascot.TOOL_BUBBLES = {
  Bash: 'Scribbling commands in my notebook…',
  Read: 'Leafing through the pages…',
  Edit: 'Writing the changes in…',
  MultiEdit: 'Writing the changes in…',
  Write: 'Writing the changes in…',
  NotebookEdit: 'Writing the changes in…',
  Grep: 'Sniffing through the leaf pile…',
  Glob: 'Sniffing through the leaf pile…',
  WebSearch: 'Flying my kite out for news…',
  WebFetch: "The kite's bringing a page back…",
  Task: 'Sending the sparrows with notes…',
  Agent: 'Sending the sparrows with notes…',
  TodoWrite: 'Sorting my seed packets…',
};
Mascot.OTHER = 'Trying out %s…';
Mascot.moods = true;

export { Mascot };

// say: a mood.js topic in its words, the season's own words first.
function sayTopic(topic) {
  const s = currentSeason();
  setEmoteSeason(s);
  const table = SEASON_LINES[s] || {};
  if (topic && topic.length && Object.hasOwn(table, topic[0])) return word(table, topic);
  return word(TOPIC_LINES, topic);
}

const theme = makeTheme({
  meta: { id: 'seasons', name: 'Seasons', panel: PANEL },
  Mascot,
  say: sayTopic,
  emoteColours: EMOTE_COLOURS,
});

export const { meta, lines, spans, render, bubble, say, emoteColours } = theme;
