// The orbit theme's mascot: Pip, a little probe in low orbit -- a dish on top,
// a gold-foil body with a face, a solar panel either side, a status LED that
// blinks -- floating over the curve of a planet, with a console's telemetry
// strip along the bottom of the stage. A faithful port of Atrium's
// mascot_orbit.py: the same (state, tick, tool, mood, scene, cols, rows) draws
// the same frame, cell for cell and colour for colour (golden tests:
// web/test/mascots-themes-b.test.mjs). Our own drawing: no agency's
// spacecraft, logo or patch.
//
//   space    cell backgrounds, the console floor at the top fading into the
//            stage card at the limb; stars twinkle in it
//   the limb ONE wide arc of planet: an ocean fill with land drifting by (the
//            planet turns a cell every eight ticks) under an edge in _ . - '
//            in the telemetry teal; by night darker, with amber city lights
//   strip    the bottom row: a T+ mission clock (the tick as MM:SS), a status
//            word (STBY, PROC, EXEC, DNLK, GO, NO-GO), signal bars when there
//            is room, the HH:MM at the right
//   Pip      19 x 6 cells: the dish (it sweeps while thinking), the LED, the
//            body with eyes and a mouth, solar panels on struts; it bobs
//   props    console cards to its right: TERMINAL, READ, EDIT, SCAN, DOWNLINK,
//            RELAY (two drones), CHECKLIST, a comet otherwise; an oscilloscope
//            on a COMPUTE card when thinking, a DOWNLINK card filling while
//            writing, ALL SYSTEMS GO when done, MASTER CAUTION on error.
//
// The strip's clock is the real time: setClock(fn) swaps the clock (a function
// returning a Date) for tests.

import { Canvas, STATES, BaseMascot, blink, lcg, propKind, streamLines, fd, mod, int, makeTheme } from './common.js';
import { MOODS, word } from './mood.js';

export const PANEL = '#0e1626';

// the console
const BASE = '#070b14';
const RAISED = '#16223a';
const RULE_HI = '#3d5478';
const HEAD = '#1c2a45';
const TEXT = '#dbe4f0';
const TEXT_DIM = '#8a9bb4';
const ACCENT = '#5cc8db';
const ACCENT_DIM = '#3a8796';
const ON_ACCENT = '#04121a';
const GOOD = '#4fd18b';
const WARN = '#f2b13c';
const BAD = '#f0565a';
const BAD_DIM = '#9a3a40';
// Pip
const FOIL = '#c8a24a';
const FOIL_DIM = '#9c8648';
const OUTLINE = '#8a9bb4';
const DISH = '#dbe4f0';
const INK = '#1a1408';
const CHEEK = '#f08a8a';
const CELL = '#1d3f7a';
const CELL_DIM = '#18305a';
const CELL_LINE = '#4a76b8';
const GLINT = '#6aa7ff';
const LED_OFF = '#1f4a33';
// the planet and the sky
const OCEAN = '#0f2c4c';
const OCEAN_DIM = '#0d2440';
const OCEAN_NIGHT = '#0a1424';
const LAND = '#1f3d33';
const LAND_NIGHT = '#101c1c';
const LIMB_NIGHT = '#3d5478';
const CITY = '#f2b13c';
const CLOUD = '#b7c4d6';
const CLOUD_GREY = '#7d8ba0';
const STAR = '#c9d3e0';
const STAR_DIM = '#5a6b86';
const SUN = '#ffe08a';
const SNOW = '#eef4fb';
const FOG = '#3d5478';
const FLASH = '#e8edf5';
const SWEAT = '#9fd0ff';
const NOTE = ['#5cc8db', '#4fd18b'];
const CONFETTI = ['#f08a8a', '#ffe08a', '#4fd18b', '#6aa7ff', '#c792ea'];
const PICADO = ['#ff6fae', '#ffa94d', '#7fd46f', '#6fb6ff', '#c58bff', '#ffd84d'];
const OVERCAST = ['cloud', 'rain', 'storm', 'snow', 'fog'];

const PW = 19;
const PH = 6;
const DISH_ROW = '\\_____/';
const PROBE = [
  null,
  '         |         ',
  '      .-----.      ',
  '     =|     |=     ',
  '      |     |      ',
  "      '-----'      ",
];
const PANEL_ROWS = ['+---+', '|:::|', '|:::|', '+---+'];
const PANEL_X = [0, 14];
const EYES = [8, 10];
const MOUTH = [4, 9];
const LED = [9, 0];

const COMP_W = PW + 2 + 17;
const WORDS = ['copy', 'that', 'all', 'go', 'signal', 'is', 'clean', 'and', 'the', 'data',
  'looks', 'good', 'on', 'track', 'nominal', 'a', 'it', 'so', 'ready', 'now'];
const CMDS = ['ping', 'status', 'uplink', 'ls', 'sync', 'diag'];
const TODO = ['Pre-flight', 'Uplink', 'Verify'];
const FILES = ['telemetry', 'flightplan', 'orbit.css', 'notes.txt'];

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
  calm: [['sweep', 2], ['ping', 2], ['glint', 2], ['wave', 1], ['rest', 2]],
  happy: [['wave', 3], ['ping', 2], ['glint', 1]],
  excited: [['wave', 3], ['ping', 2], ['sweep', 2]],
  shy: [['sweep', 2], ['rest', 3]],
  curious: [['sweep', 3], ['ping', 2]],
  focused: [['glint', 3], ['rest', 2]],
  worried: [['sweep', 2], ['rest', 2]],
  sad: [['rest', 1]],
  sleepy: [['yawn', 3], ['rest', 2]],
};

export const MOOD_BUBBLES = {
  happy: "Green board, all stations! What's next?",
  excited: "New mission? Tanks full, I'm GO!",
  shy: "Oh! You weren't on my scope… hi ☺",
  curious: "Unknown signal. Let's take a closer look!",
  focused: 'Locked on. Listening.',
  worried: 'Telemetry looks a little off…',
  sad: 'Low power today. Running on reserves…',
  sleepy: 'Pip in safe mode… zz',
};
// mood.js topics in its words, a flight controller's voice.
export const TOPIC_LINES = {
  thanks: 'Copy, thanks! Logging that one as a win.',
  worried: 'Reading something off-nominal. Run a check?',
  frustrated: 'Still no lock? Reset, new approach, go again.',
  relieved: 'And we have signal! Back to nominal.',
  determined: "Long burn ahead. I'll hold the telemetry steady.",
  proud: 'Mission complete. Clean landing!',
  sad: 'Copy… drifting quietly on the night side for a bit.',
  happy: 'Woo! Green lights!',
  curious: 'Interesting data. Send more?',
  rain: 'Rain over your sector. Good day to stay docked.',
  storm: 'Storm cells on the ground track. Save your work!',
  snow: 'Snow down there! Keep warm, crew ❄',
  fog: 'Low visibility on the ground. Go easy.',
  sun: 'Full sun on the panels. Power is great ☀',
  hot: '{0} outside. Watch your thermals!',
  cold: '{0}! Heaters on, crew.',
  gpu_hot: 'GPU at {0}°C. Thermal limit approaching!',
  cpu_hot: 'CPU at {0}°C! Throttle back a bit?',
  music: 'Good tunes on the loop ♪',
  game: 'Sim time! The checklist can wait.',
  event: 'Heads up: {0} in {1} min.',
  event_now: 'Mark: {0}, now.',
  birthday: 'Happy launch day, {0}! 🎈',
  holiday: "{0} today. Console's on standby!",
  go_home: '{0} already. End of shift, head home!',
  bedtime: '{0}. Crew rest period. Power down?',
  morning: "Morning! Today's flight plan is loaded.",
  lunch: 'Time to refuel? Lunch break!',
  weekend: 'Weekend: no launches on the board. Go enjoy it!',
  friday: 'Friday PM. Final orbit of the week!',
  hello: ['Pip online. Ready for tasking.',
    "Comms check: hi! What's the mission?",
    'Good to hear you, ground. Go ahead.'],
  welcome_back: 'Welcome back, ground! Held orbit for you.',
  wake: ['Exiting safe mode… systems coming up!',
    "Huh? Signal reacquired. I'm up!"],
  long_prompt: ['Big uplink! Buffering…',
    "That's a full payload. Copy all."],
  code: ['Code uplink received. Parsing.',
    'Source on the wire. Checking it.'],
  image: ['Imagery received! Nice frame.',
    'Incoming image. Downlinking pixels.'],
  aborted: ['Abort, abort! Burn cut.',
    'Copy, aborting. All stop.'],
  tests_pass: ['Tests are GO! Green board.',
    'All checks nominal ✓',
    'Test sequence complete. GO!'],
  tests_fail: ['Tests NO-GO. Pulling telemetry.',
    'Test failure. Holding the count.'],
  commit: ['Commit confirmed. Payload away!',
    'Pushed! Clean separation.',
    'Logged to the flight record ✓'],
  fast: ['Fast burn! Nice.',
    'In and out. Textbook.',
    'Quick hop. Nominal!'],
  busy: ['Lots of traffic on the loop…',
    'Busy pass! Juggling the uplinks.'],
  model: ['New flight computer: {0}.',
    'Handover to {0}. Copy.'],
  poke: ['Ping received! Pong.',
    'Hey! You nudged my orbit.',
    'Comms check? Loud and clear!',
    'Pip here. Standing by.',
    "Careful, that's a thruster!",
    'Roger, ground. Need something?'],
  poke_spam: ["Easy! You're knocking me out of orbit!",
    'Stop, stop! Attitude control failing!',
    'Mayday! Too many pings!'],
  pet: ['Aww… panels at full power ☺',
    'Warm fuzzies on all sensors.',
    "That's a GO for cuddles.",
    'Best ground crew in the fleet.'],
  rg_memory: '{0} was draining memory. Jettisoned it -- back to nominal.',
  rg_vram: '{0} saturated the GPU memory. Cut it loose.',
  rg_game_vram: '{0} is at the VRAM limit. Holding, not aborting.',
  rg_panic: 'Abort confirmed: {0} is down.',
  rg_stuck: 'Memory critical, no target acquired...',
  rg_agents: 'All agent burns terminated. Crew sessions safe.',
};
export const EMOTE_COLOURS = {
  accent: ACCENT, warn: WARN, bad: BAD, good: GOOD, love: '#ff8fc0', dim: TEXT_DIM,
};
// The strip's status word per state.
export const STATUS = {
  idle: ['STBY', TEXT_DIM], thinking: ['PROC', ACCENT], tool: ['EXEC', ACCENT],
  writing: ['DNLK', ACCENT], done: ['GO', GOOD], error: ['NO-GO', BAD],
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

// setClock swaps the clock the strip reads (a function returning a Date);
// null puts the real one back.
export function setClock(fn) {
  clock = fn || (() => new Date());
}

function hhmm() {
  const d = clock();
  return ` ${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')} `;
}

const pad2 = (n) => String(n).padStart(2, '0');
const has = (scene, tags) => tags.some((t) => scene.has(t));

// ---------- the mascot ----------

class Mascot extends BaseMascot {
  constructor(cols = 64, rows = 18) {
    super(cols, rows);
    this.cols = Math.max(int(cols), COMP_W + 2);
    this.rows = Math.max(int(rows), PH + 8);
    this.cx = fd(this.cols - COMP_W, 2);
    this.bottom = this.rows - 2;
    const peak = this.rows >= 18 ? 3.6 : 2.8;
    const mid = this.cols / 2.0;
    const radius = Math.max(this.cols * 1.6, 80.0);
    this.h = [];
    for (let x = 0; x < this.cols; x++) {
      const dx = x + 0.5 - mid;
      const sag = (radius - Math.sqrt(Math.max(0.0, radius * radius - dx * dx))) * 0.5;
      this.h.push(Math.max(0.6, peak - sag) + 0.12 * Math.sin(x * 0.13 + 0.7));
    }
    const hc = this.cx + PW / 2.0;
    this.ground = this.bottom - int(this.h[int(hc)]);
    this.span = this.cols * 3;
    this.land = [];
    this.city = [];
    for (let x = 0; x < this.span + 4; x++) {
      this.land.push(lcg(181, fd(x, 4))() * 0.6 + lcg(181, fd(x, 4) + 1)() * 0.4);
    }
    for (let x = 0; x < this.span + 4; x++) this.city.push(lcg(191, x)());
    this.stream = streamLines(WORDS, 12, 73);
    this.stars = [];
    for (let i = 0; i < fd(this.cols * this.rows, 28); i++) {
      const r = lcg(167, i);
      this.stars.push([int(r() * this.cols), int(r() * Math.max(2, this.ground - 1)),
        20 + int(r() * 30), int(r() * 50)]);
    }
    this.bands = [];
    for (let i = 0; i < Math.max(2, fd(this.cols, 18)); i++) {
      const r = lcg(173, i, this.cols);
      this.bands.push([r() * this.cols, r(), 3 + int(r() * 6), 0.04 + r() * 0.04]);
    }
  }

  bubble(state, tool = null, mood = null) {
    if (state === 'idle' && Object.hasOwn(MOOD_BUBBLES, mood || '')) return MOOD_BUBBLES[mood];
    return super.bubble(state, tool);
  }

  edge(x) {
    return this.bottom - int(this.h[Math.max(0, Math.min(this.cols - 1, x))]);
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
    this.space(cv, state, tick, scene);
    if (scene.has('festive')) this.banner(cv, tick);
    this.planet(cv, tick, scene);
    if (scene.has('party')) this.confetti(cv, tick);
    this.strip(cv, state, tick, scene);
    let hover;
    if (state === 'done') hover = [0, 1, 2, 2, 1, 0, 0, 0][mod(tick, 8)];
    else if (idle && mood === 'excited' && mod(fd(tick, 30), 2)) hover = [0, 1, 2, 2, 1, 0][mod(tick, 6)];
    else if (state === 'error' || (idle && (mood === 'sad' || mood === 'sleepy'))) hover = 0;
    else if (scene.has('music')) hover = mod(fd(tick, 4), 2);
    else hover = Math.sin(tick / 7.0) > 0 ? 1 : 0;
    const top = this.ground - PH - 1 - hover;
    const ox = this.cx;
    if (kind === 'helpers') this.helpers(cv, tick);
    this.head = [Math.min(ox + 14, this.cols - 3), Math.max(0, top)];
    this.probe(cv, state, tick, ox, top, mood, scene, fidget);
    this.moodMarks(cv, state, tick, ox, top, mood, scene, fidget);
    if (scene.has('party')) this.hat(cv, tick, ox, top);
    const px = this.cx + PW + 2;
    const py = this.ground - 8;
    if (state === 'thinking') {
      this.rings(cv, tick, ox, top);
      this.scope(cv, tick, px, py);
    } else if (state === 'writing') {
      this.signal(cv, tick, ox, top, px, py);
      this.doc(cv, tick, px, py, true, 'DOWNLINK');
    } else if (state === 'done') this.go(cv, tick, px, py);
    else if (state === 'error') this.caution(cv, tick, px, py);
    else if (kind && kind !== 'helpers') this[`prop_${kind}`](cv, tick, px, py);
    else if (kind === 'helpers') { /* drawn above */ } else if (scene.has('game')) this.controller(cv, tick, px, py);
    return cv;
  }

  fidget(tick, mood) {
    if (mod(tick, 90) < 72) return null;
    const table = FIDGETS[mood] || FIDGETS.calm;
    let pick = lcg(199, fd(tick, 90))() * table.reduce((s, [, w]) => s + w, 0);
    for (const [name, weight] of table) {
      pick -= weight;
      if (pick < 0) return name === 'rest' ? null : name;
    }
    return null;
  }

  // ---------- space and the planet ----------

  space(cv, state, tick, scene) {
    const overcast = has(scene, OVERCAST);
    const night = scene.has('night');
    const span = Math.max(1, this.ground);
    for (let y = 0; y < this.bottom + 1; y++) {
      const bg = mix(BASE, PANEL, Math.min(1.0, y / span));
      for (let x = 0; x < this.cols; x++) cv.recolour(x, y, null, bg);
    }
    const y0 = scene.has('festive') ? 2 : 0;
    const sun = !night && !overcast && state !== 'error' && (scene.has('day') || scene.has('sun'))
      && this.cols >= 48;
    for (const [x, y, period, ph] of this.stars) {
      if (overcast && mod(x + y, 3)) continue;
      if (!night && mod(x * 7 + y, 2)) continue;
      if (sun && x >= this.cols - 8 && y < y0 + 4) continue;
      if (y < y0 || cv.get(x, y) !== ' ') continue;
      const t = mod(tick + ph, period);
      cv.cell(x, y, t < 5 ? '.+*+.'[t] : '.', t < 5 ? STAR : STAR_DIM);
    }
    if (sun) this.sun(cv, tick, y0);
  }

  planet(cv, tick, scene) {
    const night = scene.has('night');
    const overcast = has(scene, OVERCAST);
    const ocean = night ? OCEAN_NIGHT : (overcast ? OCEAN_DIM : OCEAN);
    const land = night ? LAND_NIGHT : LAND;
    const turn = fd(tick, 8);
    for (let x = 0; x < this.cols; x++) {
      const h = this.h[x];
      const top = this.bottom - int(h);
      const k = mod(x + turn, this.span);
      for (let y = top + 1; y < this.bottom + 1; y++) {
        const ground = this.land[k + mod(y, 2)] > 0.62;
        cv.cell(x, y, ' ', null, ground ? land : ocean);
        if (night && ground && this.city[mod(k + y, this.span)] < 0.18) cv.cell(x, y, '.', CITY, land);
      }
      const frac = h - int(h);
      const glyph = "_.-'"[Math.min(3, int(frac * 4))];
      const colour = scene.has('snow') ? SNOW : (night ? LIMB_NIGHT : ACCENT);
      cv.cell(x, top, glyph, colour);
    }
    if (scene.has('cloud') || scene.has('rain') || scene.has('storm')) {
      this.bandsOn(cv, tick, scene.has('rain') || scene.has('storm'), scene.has('storm'));
    }
    if (scene.has('fog')) this.haze(cv, tick);
  }

  bandsOn(cv, tick, heavy = false, storm = false) {
    const colour = heavy ? CLOUD_GREY : CLOUD;
    for (const [x0, yf, w, speed] of this.bands) {
      const x = mod(int(x0 + tick * speed), this.cols + w) - w;
      for (let c = 0; c < w * (heavy ? 2 : 1); c++) {
        const xx = x + c;
        if (!(xx >= 0 && xx < this.cols)) continue;
        const top = this.edge(xx);
        const depth = this.bottom - top;
        if (depth < 1) continue;
        const y = top + 1 + int(yf * depth);
        if (y <= this.bottom) cv.cell(xx, y, mod(c + fd(tick, 6), 3) ? '~' : '-', colour);
      }
    }
    if (storm) {
      const cyc = fd(tick, 37);
      const t = mod(tick, 37);
      if (t < 3) {
        const x = int(lcg(193, cyc)() * (this.cols - 2)) + 1;
        const y = Math.min(this.bottom, this.edge(x) + 1);
        cv.cell(x, y, t !== 1 ? '*' : '+', FLASH);
      }
    }
  }

  haze(cv, tick) {
    for (let k = 0; k < 2; k++) {
      const off = mod(fd(tick, 7 + k * 3) + k * 5, 10);
      for (let x = 0; x < this.cols; x++) {
        const y = this.edge(x) - k;
        if ((x + off) % 10 < 4 && cv.get(x, y) === ' ') cv.cell(x, y, '~', FOG);
      }
    }
  }

  strip(cv, state, tick, scene) {
    const y = this.rows - 1;
    cv.fill(0, y, this.cols, 1, RAISED, ' ', null, false);
    cv.recolour(0, y, null, ACCENT);
    const secs = mod(fd(tick, 10), 3600);
    cv.put(1, y, ` T+${pad2(fd(secs, 60))}:${pad2(mod(secs, 60))} `, TEXT, false, RAISED);
    const [word_, colour] = STATUS[state] || STATUS.idle;
    cv.put(11, y, ` ${word_} `, colour, false, HEAD);
    const clk = hhmm();
    const ring = scene.has('clock') && mod(fd(tick, 5), 2);
    const cx = this.cols - clk.length;
    cv.put(cx, y, clk, ring ? ON_ACCENT : TEXT, false, ring ? WARN : HEAD);
    let x = cx - 2;
    if (scene.has('music')) {
      cv.cell(x, y, 'd', NOTE[0], RAISED);
      x -= 2;
    }
    if (this.cols >= 60) {
      const n = state === 'error' ? 1 : (state === 'idle' ? 3 : 2 + mod(fd(tick, 4), 2));
      for (let i = 0; i < 3; i++) {
        cv.cell(x - 3 + i, y, '|', i < n ? (state !== 'error' ? GOOD : BAD) : RULE_HI, RAISED);
      }
      cv.put(x - 8, y, 'SIG', TEXT_DIM, false, RAISED);
    }
    if (scene.has('clock') && mod(fd(tick, 10), 3)) {
      const text = ' (!) REMINDER ';
      const bx = Math.max(0, this.cols - text.length - 1);
      cv.put(bx, y - 2, text, ON_ACCENT, false, WARN);
      cv.cell(this.cols - 4, y - 1, 'V', WARN);
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

  confetti(cv, tick) {
    const span = this.rows + 2;
    for (let i = 0; i < Math.max(4, fd(this.cols * this.rows, 90)); i++) {
      const r = lcg(197, i);
      const pos = r() * span + tick * (0.10 + r() * 0.08);
      const fall = int(fd(pos, span));
      const y = int(pos - fall * span) - 1;
      const x = int(lcg(199, i, fall)() * this.cols) + mod(fd(tick, 4) + i, 2);
      if (y < this.rows - 1 && cv.get(x, y) === ' ') {
        cv.cell(x, y, ",'.*"[mod(fd(tick, 3) + i, 4)], CONFETTI[i % CONFETTI.length]);
      }
    }
  }

  hat(cv, tick, ox, top) {
    const x = ox + 9;
    cv.cell(x, top - 3, '*+'[mod(fd(tick, 4), 2)], SUN);
    cv.put(x - 1, top - 2, '/:\\', CONFETTI[0]);
    cv.put(x - 2, top - 1, '/_:_\\', CONFETTI[3]);
    cv.cell(x, top - 2, ':', SUN);
    cv.cell(x, top - 1, ':', SUN);
  }

  controller(cv, tick, x, y) {
    this.card(cv, x, y + 1, 16, 6, 'SIM', 'GO');
    cv.put(x + 3, y + 3, '.-----.', TEXT_DIM, false, RAISED);
    cv.put(x + 2, y + 4, '( +  oo )', TEXT_DIM, false, RAISED);
    cv.put(x + 3, y + 5, "'-' '-'", TEXT_DIM, false, RAISED);
    cv.cell(x + 4, y + 4, '+', TEXT, RAISED);
    cv.cell(x + 7, y + 4, 'o', GOOD, RAISED);
    cv.cell(x + 8, y + 4, 'o', [ACCENT, BAD, GOOD, WARN][mod(fd(tick, 3), 4)], RAISED);
  }

  // ---------- Pip ----------

  probe(cv, state, tick, x0, top, mood, scene, fidget) {
    const idle = state === 'idle';
    const tired = state === 'error' || (idle && (mood === 'sad' || mood === 'sleepy'));
    const foil = tired ? FOIL_DIM : FOIL;
    const cell = tired ? CELL_DIM : CELL;
    const t = mod(tick, 90) - 72;
    const sag = tired || (idle && mood === 'worried') ? 1 : 0;
    PANEL_X.forEach((px, side) => {
      let dy = sag;
      if (fidget === 'wave' && side === 1) dy = -mod(fd(tick, 3), 2);
      PANEL_ROWS.forEach((r, i) => cv.sprite(x0 + px, top + 2 + dy + i, r, OUTLINE));
      for (const i of [1, 2]) for (let c = 1; c < 4; c++) cv.recolour(x0 + px + c, top + 2 + dy + i, CELL_LINE, cell);
      let g = null;
      if (fidget === 'glint') g = fd(t, 2) - side * 4;
      else if (!tired && mod(tick, 120) < 8) g = mod(tick, 120) - side * 4;
      if (g !== null && g >= 0 && g < 4) {
        for (const i of [1, 2]) cv.recolour(x0 + px + 1 + Math.min(2, g), top + 2 + dy + i, null, GLINT);
      }
      if (scene.has('cold')) cv.cell(x0 + px + (side === 0 ? 1 : 3), top + 2 + dy + 2, '*', SNOW, cell);
    });
    PROBE.forEach((r, i) => {
      if (r !== null) cv.sprite(x0, top + i, r, OUTLINE);
    });
    for (const i of [3, 4]) for (let c = 7; c < 12; c++) cv.recolour(x0 + c, top + i, null, foil);
    let gazeD = 0;
    if (state === 'thinking') gazeD = [0, 1, 0, -1][mod(fd(tick, 10), 4)];
    else if (fidget === 'sweep') gazeD = t < 9 ? -1 : 1;
    cv.put(x0 + 6 + gazeD, top, DISH_ROW, DISH, true);
    let led;
    if (state === 'error') led = mod(fd(tick, 3), 2) ? BAD : BAD_DIM;
    else if (idle && mood === 'sleepy') led = mod(tick, 40) < 4 ? WARN : LED_OFF;
    else if (['thinking', 'tool', 'writing', 'done'].includes(state)) led = GOOD;
    else led = mod(tick, 16) < 3 ? GOOD : LED_OFF;
    cv.cell(x0 + LED[0] + gazeD, top + LED[1], 'o', led);
    // face
    const eye = state === 'done' ? '^' : 'o';
    let gaze = state === 'tool' || state === 'writing' ? 1 : 0;
    if (state === 'thinking') gaze = gazeD;
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
    if (fidget === 'sweep') gaze = gazeD;
    else if (fidget === 'yawn') {
      left = '-';
      right = '-';
      mouth = t >= 3 && t < 14 ? 'O' : 'o';
    } else if (fidget === 'ping') mouth = 'o';
    const shut = blink(tick, 47) && state !== 'done' && state !== 'error';
    [left, right].forEach((ch, k) => cv.cell(x0 + EYES[k] + gaze, top + 3, shut ? '-' : ch, INK, foil));
    cv.cell(x0 + MOUTH[1] + gaze, top + MOUTH[0], mouth, INK, foil);
    if (Object.hasOwn(BROWS, mood) && state !== 'done') {
      BROWS[mood].forEach((ch, k) => cv.cell(x0 + EYES[k] + gaze, top + 2, ch, OUTLINE));
    }
    if (cheek) {
      cv.cell(x0 + 7, top + 4, cheek, CHEEK, foil);
      cv.cell(x0 + 11, top + 4, cheek, CHEEK, foil);
    }
    if (state === 'error' || ((mood === 'worried' || scene.has('hot')) && state !== 'done')) {
      const s = mod(fd(tick, 5), 4);
      if (s < 3) cv.cell(x0 + 13, top + s, "',."[s], SWEAT);
    }
    if (mood === 'sad' && idle) {
      const s = mod(fd(tick, 6), 3);
      if (s < 2) cv.cell(x0 + EYES[0] + gaze, top + 4 + s, ",'"[s], SWEAT, s === 0 ? foil : null);
    }
  }

  moodMarks(cv, state, tick, ox, top, mood, scene, fidget) {
    const idle = state === 'idle';
    const t = mod(tick, 90) - 72;
    if (idle && mood === 'sleepy') {
      const s = mod(tick, 36);
      for (let j = 0; j < 3; j++) {
        if (fd(s, 9) > j) cv.cell(ox + 14 + j, top - j, 'zzZ'[j], j < 2 ? TEXT_DIM : TEXT);
      }
    } else if (idle && mood === 'curious' && mod(fd(tick, 5), 4)) {
      cv.cell(ox + 14, top, '?', ACCENT);
    } else if (idle && mood === 'excited' && mod(fd(tick, 3), 4) < 2) {
      cv.cell(ox + 4, top, '!', WARN);
      cv.cell(ox + 14, top, '!', WARN);
    } else if (mood === 'excited' && ['thinking', 'tool', 'writing', 'done'].includes(state)) {
      if (mod(fd(tick, 3), 4) === 0) cv.cell(ox + 14, top, '+', SUN);
    }
    if (fidget === 'ping' && t >= 2) {
      const s = fd(t - 2, 3);
      if (s < 3) {
        const w = 3 + 2 * s;
        cv.put(ox + 9 - fd(w, 2), top - 1 - s, '-'.repeat(w), s < 2 ? ACCENT : ACCENT_DIM);
      }
      if (t >= 8) cv.put(ox + 12, top - 2, 'PING', TEXT_DIM);
    }
    if (scene.has('music')) {
      for (let j = 0; j < 2; j++) {
        const s = mod(tick + j * 12, 24);
        if (s < 20) cv.cell(ox - 1 - fd(s, 6) - j * 2, top + 5 - fd(s, 5), 'db'[j], NOTE[j]);
      }
    }
    if (scene.has('hot')) {
      PANEL_X.forEach((px, side) => {
        if (lcg(211, side, fd(tick, 3))() < 0.6) cv.cell(ox + px + 1 + mod(fd(tick, 3) + side, 3), top + 1, '~', WARN);
      });
    }
  }

  // ---------- things beside it ----------

  card(cv, x, y, w, h, title, status = null, statusColour = GOOD, body = RAISED) {
    cv.fill(x, y, w, h, body);
    cv.fill(x, y, w, 1, HEAD);
    cv.recolour(x, y, null, ACCENT);
    cv.put(x + 2, y, title.slice(0, Math.max(0, w - 3)), TEXT_DIM, false, HEAD);
    if (status && title.length + status.length + 4 <= w) {
      cv.put(x + w - status.length - 1, y, status, statusColour, false, HEAD);
    }
  }

  rings(cv, tick, ox, top) {
    const s = mod(fd(tick, 3), 4);
    if (s < 3) {
      const w = 3 + 2 * s;
      cv.put(ox + 9 - fd(w, 2), top - 1 - s, '-'.repeat(w), s < 2 ? ACCENT : ACCENT_DIM);
    }
  }

  scope(cv, tick, x, y) {
    this.card(cv, x, y, 16, 7, 'COMPUTE', 'PROC', ACCENT);
    for (let c = 0; c < 14; c++) {
      const v = Math.sin((c + tick * 0.6) / 2.0) * 1.6 + Math.sin((c * 3 + tick) / 5.0) * 0.4;
      const r = Math.max(-2, Math.min(2, int(pyRound(v))));
      const nxt = Math.sin((c + 1 + tick * 0.6) / 2.0) * 1.6;
      const ch = Math.abs(nxt - v) < 0.4 ? '-' : (nxt > v ? '/' : '\\');
      cv.cell(x + 1 + c, y + 3 - r, ch, ACCENT, RAISED);
    }
    cv.put(x + 1, y + 6, ('PROC' + '.'.repeat(mod(fd(tick, 4), 4))).padEnd(14), TEXT_DIM, false, RAISED);
  }

  doc(cv, tick, x, y, stream = false, title = 'READ', status = null) {
    this.card(cv, x, y, 16, 7, title, status, ACCENT);
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
      cv.put(x + 2, y + 1 + k, text, k === 4 ? TEXT : TEXT_DIM, false, RAISED);
    }
    if (stream && mod(fd(tick, 3), 2)) cv.cell(x + 2 + Math.min(col, 11), y + 5, '_', ACCENT, RAISED);
  }

  signal(cv, tick, ox, top, px, py) {
    const x0 = ox + 13;
    const y0 = top;
    const x1 = px;
    const y1 = py + 2;
    for (let k = 0; k < 3; k++) {
      const s = mod(tick + k * 6, 18) / 18.0;
      const x = int(pyRound(x0 + (x1 - x0) * s));
      const y = int(pyRound(y0 + (y1 - y0) * s - 2.0 * Math.sin(s * Math.PI)));
      if (cv.get(x, y) === ' ') cv.cell(x, y, k === 0 ? '*' : '.', ACCENT);
    }
  }

  go(cv, tick, x, y) {
    this.card(cv, x, y + 1, 16, 5, 'STATUS', 'GO');
    cv.put(x + 1, y + 3, 'v/', GOOD, false, RAISED);
    cv.put(x + 4, y + 3, 'ALL SYSTEMS', TEXT, false, RAISED);
    cv.put(x + 4, y + 4, 'GO', GOOD, false, RAISED);
    cv.put(x + 7, y + 4, 'NOMINAL', TEXT_DIM, false, RAISED);
    const r = lcg(203, fd(tick, 3));
    for (let k = 0; k < 4; k++) {
      const sx = x + int(r() * 16);
      const sy = y + int(r() * 7);
      if (cv.get(sx, sy) === ' ') cv.cell(sx, sy, '+*.'[int(r() * 3)], STAR);
    }
  }

  caution(cv, tick, x, y) {
    const on = mod(fd(tick, 4), 2);
    cv.fill(x, y + 1, 16, 5, RAISED);
    cv.fill(x, y + 1, 16, 1, on ? WARN : HEAD);
    cv.put(x + 1, y + 1, 'MASTER CAUTION', on ? ON_ACCENT : WARN, false, on ? WARN : HEAD);
    cv.put(x + 1, y + 3, '(x)', on ? BAD : BAD_DIM, false, RAISED);
    cv.put(x + 5, y + 3, 'NO-GO', BAD, false, RAISED);
    cv.put(x + 5, y + 4, 'Recycle?', TEXT_DIM, false, RAISED);
  }

  sparkles(cv, tick, x, y, n = 5) {
    for (let j = 0; j < n; j++) {
      const r = lcg(207, j);
      const period = 24 + int(r() * 16);
      const t = mod(tick + int(r() * period), period);
      const sx = x + int(r() * 12);
      const sy = y - int(r() * 5);
      if (t < fd(period * 2, 3) && cv.get(sx, sy) === ' ') {
        cv.cell(sx, sy, '*+.'[mod(fd(t, 4) + j, 3)], [SUN, STAR, ACCENT][j % 3]);
      }
    }
  }

  prop_bash(cv, tick, x, y) {
    this.card(cv, x, y, 16, 7, 'TERMINAL', 'EXEC', ACCENT, BASE);
    const cyc = fd(tick, 30);
    const cmd = CMDS[mod(cyc, CMDS.length)];
    const n = Math.min(cmd.length, fd(mod(tick, 30), 2));
    for (let k = 0; k < 3; k++) {
      const prev = CMDS[mod(cyc - 3 + k, CMDS.length)];
      cv.put(x + 1, y + 1 + k, ('> ' + prev).slice(0, 14), TEXT_DIM, false, BASE);
    }
    cv.put(x + 1, y + 5, ('> ' + cmd.slice(0, n)).slice(0, 14), TEXT, false, BASE);
    if (mod(fd(tick, 3), 2)) cv.cell(x + 3 + n, y + 5, '_', ACCENT, BASE);
  }

  prop_book(cv, tick, x, y) {
    this.doc(cv, tick, x, y, false, 'READ');
    const hot = mod(fd(tick, 3), 5);
    for (let c = 0; c < 14; c++) cv.recolour(x + 1 + c, y + 1 + hot, TEXT, RULE_HI);
  }

  prop_pad(cv, tick, x, y) {
    this.doc(cv, tick, x, y, true, 'EDIT', 'UPLK');
  }

  prop_lens(cv, tick, x, y) {
    this.card(cv, x, y, 16, 7, 'SCAN', 'EXEC', ACCENT);
    cv.fill(x + 1, y + 1, 14, 1, BASE);
    const q = 'files';
    const n = Math.min(q.length, fd(mod(tick, 40), 3));
    cv.put(x + 2, y + 1, '>' + q.slice(0, n), TEXT, false, BASE);
    const hit = mod(fd(tick, 5), 4);
    FILES.forEach((f, k) => {
      cv.put(x + 3, y + 2 + k, f.padEnd(12), k === hit ? TEXT : TEXT_DIM, false, k === hit ? RULE_HI : RAISED);
    });
    cv.put(x + 1, y + 2 + hit, '>', ACCENT, false, RAISED);
  }

  prop_globe(cv, tick, x, y) {
    this.card(cv, x, y, 16, 7, 'DOWNLINK', 'AOS');
    cv.put(x + 1, y + 2, ['(@)', '(o)', '(@)', '(O)'][mod(fd(tick, 4), 4)], ACCENT, false, RAISED);
    cv.put(x + 5, y + 2, 'page.html', TEXT, false, RAISED);
    const pct = mod(tick * 2, 101);
    cv.put(x + 5, y + 3, `${pct}%`.padEnd(5), TEXT_DIM, false, RAISED);
    const bx = x + 1;
    const by = y + 5;
    const bw = 14;
    cv.fill(bx, by, bw, 1, BASE);
    const filled = int(pct / 100.0 * bw);
    for (let c = 0; c < bw; c++) {
      cv.cell(bx + c, by, c < filled ? '=' : '-', c < filled ? ACCENT : RULE_HI, BASE);
    }
  }

  helpers(cv, tick) {
    [Math.max(0, this.cx - 7), Math.min(this.cols - 6, this.cx + COMP_W + 1)].forEach((x, n) => {
      const y = this.ground - 5 - (Math.sin(tick / 5.0 + n * 2) > 0 ? 1 : 0);
      cv.put(x, y, '  _  ', DISH);
      cv.put(x, y + 1, '=[o]=', OUTLINE);
      cv.cell(x + 2, y + 1, 'o', INK, FOIL);
      cv.cell(x + 2, y, '_', mod(fd(tick, 4) + n, 3) === 0 ? GOOD : DISH);
    });
    const px = this.cx + PW + 2;
    const py = this.ground - 8;
    this.card(cv, px, py, 16, 5, 'RELAY', 'AOS');
    const k = mod(fd(tick, 8), 3);
    cv.put(px + 1, py + 2, ('handover' + '.'.repeat(k)).padEnd(11), TEXT, false, RAISED);
    cv.put(px + 1, py + 3, '2 drones', TEXT_DIM, false, RAISED);
  }

  prop_todo(cv, tick, x, y) {
    this.card(cv, x, y, 16, 5, 'CHECKLIST');
    const done = 1 + mod(fd(tick, 12), TODO.length);
    TODO.forEach((item, k) => {
      const mark = k < done ? '[x]' : '[ ]';
      cv.put(x + 1, y + 1 + k, mark, k < done ? GOOD : TEXT_DIM, false, RAISED);
      cv.put(x + 5, y + 1 + k, item, k < done ? TEXT_DIM : TEXT, false, RAISED);
    });
  }

  prop_wand(cv, tick, x, y) {
    const s = mod(tick, 30);
    const cx = x + Math.min(13, fd(s, 2));
    const cy = y + 5 - Math.min(4, fd(s, 6));
    cv.put(cx - 3, cy, '~~~', ACCENT_DIM);
    cv.cell(cx, cy, '*', SUN);
    this.sparkles(cv, tick, x + 2, y + 5, 6);
  }
}
Mascot.BUBBLES = {
  idle: 'Pip here. All systems nominal. Standing by.',
  thinking: 'Crunching the numbers…',
  writing: 'Downlinking your answer…',
  done: 'Copy that. All systems GO ✓',
  error: 'Off-nominal. Recycle and try again?',
};
Mascot.TOOL_BUBBLES = {
  Bash: 'Executing command sequence…',
  Read: 'Pulling the file up on screen…',
  Edit: 'Uplinking the changes…',
  MultiEdit: 'Uplinking the changes…',
  Write: 'Uplinking the changes…',
  NotebookEdit: 'Uplinking the changes…',
  Grep: 'Scanning the files…',
  Glob: 'Scanning the files…',
  WebSearch: 'Sweeping the net for a signal…',
  WebFetch: 'Receiving downlink…',
  Task: 'Calling in the relay drones…',
  Agent: 'Calling in the relay drones…',
  TodoWrite: 'Updating the checklist…',
};
Mascot.OTHER = 'Bringing %s online…';
Mascot.moods = true;

export { Mascot };

const theme = makeTheme({
  meta: { id: 'orbit', name: 'Orbit', panel: PANEL },
  Mascot,
  say: (topic) => word(TOPIC_LINES, topic),
  emoteColours: EMOTE_COLOURS,
});

export const { meta, lines, spans, render, bubble, say, emoteColours } = theme;
