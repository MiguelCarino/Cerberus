// The cyberpunk theme's mascot: a neon android with a visor, in front of a
// rainy night skyline. A faithful port of Atrium's mascot_cyberpunk.py: the
// same (state, tick, tool, mood, scene, cols, rows) draws the same frame, cell
// for cell and colour for colour (golden tests:
// web/test/mascots-themes-a.test.mjs).
//
//   city     a skyline along the bottom painted as cell backgrounds, windows
//            lit in cyan, magenta and yellow and now and then flickering, a
//            neon sign, thin slanted rain, a hover car; a scan line sweeps
//            down the picture, brightening what it passes.
//   android  21 x 11 cells: antenna, a helmet with a visor (a strip of dark
//            cells the eyes are drawn on, the scanner while it thinks), a
//            mouth grille, a chest core pulsing cyan to magenta.
//   props    a holo panel to its right: a shell for Bash, a data shard for
//            Read, code being rewritten for Edit/Write, a reticle over the data
//            for the searches, a wireframe globe for WebFetch, the gig list
//            for TodoWrite, ICE-breaker sparks otherwise; two drones for
//            Task/Agent; thinking fills the neural-net bars, done says JOB DONE.
//   glitch   torn rows and magenta ghosts every few seconds; on error most
//            frames, the visor red Xs and the panel SIGNAL LOST.
//   mood     the visor is the face (glyphs and colour per mood), with the
//            grille, the core and the holo's status line; an idle fidget.
//   scene    smog or outrun sun, moon, smog banks, a downpour and a holo
//            umbrella, lightning, snow, fog, heat, a neon scarf, headphones,
//            invaders (game), party hat, neon papel picado, an ALARM sign.

import { BaseMascot, Canvas, STATES, blink, lcg, propKind, streamLines, fd, mod, int, makeTheme }
  from './common.js';
import { ACTIVE, MOODS, word } from './mood.js';

export const PANEL = '#07090f';

const CYAN = '#00f0ff';
const CYAN_DIM = '#0a8f9c';
const MAGENTA = '#ff2a6d';
const MAGENTA_DIM = '#8f1a44';
const YELLOW = '#f9f002';
const VIOLET = '#b026ff';
const GREEN = '#39ff14';
const STEEL = '#c8d3e6';
const STEEL_DIM = '#5a6478';
const BRIGHT = '#e6fcff';
const RAIN = '#1d5f6e';
const BUILDING = ['#0d1424', '#111a30', '#0b1020'];
const VISOR = '#1e0a22';
const HOLO = '#04161e';
const PINK = '#ff6ec7';

export const EMOTE_COLOURS = {
  accent: CYAN, warn: YELLOW, bad: MAGENTA, good: GREEN, love: PINK, dim: CYAN_DIM,
};

const W = 21;
const H = 11;
const DROID = [
  '         _|_         ',
  "      .-'---'-.      ",
  '     /  _____  \\     ',
  '    |  [     ]  |    ',
  '    |   \\___/   |    ',
  "     \\  '---'  /     ",
  "  .---'-.___.-'---.  ",
  ' /  |  [     ]  |  \\ ',
  '|_  |  [_____]  |  _|',
  '  \\_|   |   |   |_/  ',
  '    |___|   |___|    ',
];
const VISOR_X = 8;
const VISOR_Y = 3;
const VISOR_W = 5;
const CORE = [7, 10];

const PW = 16;
const PH = 8;
const COMP_W = W + 1 + PW;
const DRONE = [' _-_ ', '=(o)=', " ' ' "];

const CODE = ['jack', 'in', 'ice', '0xff', 'run', 'net', 'chrome', 'if', 'ok', 'go',
  'sys', '{', '}', '=>', 'fn', 'ret', 'edge', 'neon'];
const CMDS = ['nmap -sV', 'ssh v@c', 'make', 'ls -la', './ice', 'ping 0'];
const GIGS = ['jack in', 'find data', 'break ICE', 'get paid'];

const MOOD_FACE = {
  calm: ['o   o', CYAN, null, ['SYS:ONLINE', CYAN]],
  happy: ['^   ^', GREEN, '_v_', ['MOOD:PREEM', GREEN]],
  excited: ['*   *', YELLOW, '=-=', ['MOOD:NOVA!!', YELLOW]],
  shy: ['>   <', MAGENTA, '...', ['VISOR:HOT', MAGENTA]],
  curious: ['o   O', CYAN, '_o_', ['QUERY?', YELLOW]],
  focused: ['\\o o/', BRIGHT, '---', ['TGT:LOCKED', BRIGHT]],
  worried: ['/o o\\', YELLOW, '~~~', ['! WARNING', YELLOW]],
  sad: ['T   T', VIOLET, '-^-', ['SIGNAL:LOW', VIOLET]],
  sleepy: ['-   -', CYAN_DIM, null, ['LOW POWER', CYAN_DIM]],
};
const WORK_FACE = {
  happy: ' ^  ^', excited: ' *  *', shy: ' >  <', curious: ' o  O',
  focused: '\\o o/', worried: '/o o\\', sad: ' T  T',
};
const FIDGETS = {
  calm: [['scan', 1]],
  happy: [['wave', 3], ['scan', 1], ['ping', 1]],
  excited: [['wave', 2], ['ping', 2], ['twitch', 1]],
  shy: [['scan', 1], ['rest', 3]],
  curious: [['scan', 3], ['ping', 2]],
  focused: [['scan', 2], ['rest', 3]],
  worried: [['scan', 2], ['twitch', 2], ['rest', 1]],
  sad: [['rest', 3], ['twitch', 1]],
  sleepy: [['dip', 2], ['rest', 2]],
};
const CONFETTI = [CYAN, MAGENTA, YELLOW, GREEN, VIOLET];

const UMBRELLA = ['    .-~~~~~~~~~-.    ',
  '   /_/_/_/|\\_\\_\\_\\   '];

export const MOOD_BUBBLES = {
  happy: '[SYS] mood.exe: preem. Ready when you are, choom.',
  excited: "Overclocked and buzzing! What's the gig, choom?",
  shy: "Visor's running hot... d-don't stare, choom.",
  curious: "Query pending... what's next on the grid?",
  focused: 'Target locked. Feed me the next job.',
  worried: "[WARN] Something's off in the net...",
  sad: "Signal's weak. Like tears in rain...",
  sleepy: 'Low power mode... zz... ping me if you need me.',
};
export const TOPIC_LINES = {
  thanks: "No sweat, choom. That's what the chrome's for.",
  worried: "[WARN] That didn't go nova...",
  frustrated: 'Delta again?! Rerouting... one more run.',
  relieved: "Signal's back. All green, choom.",
  determined: 'Long run. Holding the line.',
  proud: "Gig's done. Preem work, if I say so myself.",
  sad: 'Sorry, choom... my bad.',
  happy: 'Nova! mood.exe maxed out.',
  curious: 'Hmm. Running a query on that...',
  rain: 'Rain on the neon. Grab an umbrella, choom.',
  storm: "Storm's frying the grid. Stay inside.",
  snow: 'Snow on the chrome. Static from the sky.',
  fog: "Smog's thick tonight. Visibility: delta.",
  sun: "Sun's out. Even the neon looks washed out.",
  hot: '[SYS] thermal warning: {0} outside.',
  cold: "Coolant's freezing: {0} outside.",
  gpu_hot: '[SYS] GPU thermal warning: {0}°C',
  cpu_hot: '[SYS] CPU thermal warning: {0}°C',
  music: 'Preem track. Turning up the bass.',
  game: "Game time. Let's frag some ICE.",
  event: '[REMINDER] {0} in {1} min, choom.',
  event_now: '[ALERT] {0} is starting. Jack in.',
  birthday: "It's {0}'s birthday. Nova!",
  holiday: "Today's {0}. Neon's on the house.",
  go_home: "{0}. Shift's over, choom -- jack out, go home.",
  bedtime: '{0}. Low power mode recommended: go to bed.',
  morning: 'Morning, choom. Systems booting.',
  lunch: 'Lunch break? Even androids recharge.',
  weekend: 'Weekend. No gigs, just neon.',
  friday: "Friday night. The neon's calling, choom.",
  hello: ["Online. What's the gig, choom?", '[SYS] link up. Ready to run.',
    "Hey, choom. Jack in, let's work."],
  welcome_back: 'Welcome back, choom. Kept the neon warm.',
  wake: "[SYS] exiting low power... I'm up!",
  long_prompt: 'Big data dump. Buffering, choom...',
  code: 'Code-chrome incoming. Scanning it.',
  image: ['Optics engaged. Nice pic, choom.', 'Running it through the optics...'],
  aborted: '[ABORT] Jacking out. Run killed.',
  tests_pass: ['Tests green. Preem.', '[OK] All checks passed. Nova!',
    "ICE cleared: the suite's green."],
  tests_fail: ['[FAIL] Tests flatlined, choom.', 'Red on the board. Rerouting...'],
  commit: ['Pushed to the net. Shipped.', 'Commit burned to shard. Preem.',
    '[SYS] payload delivered.'],
  fast: ['Quick hack. Too easy.', 'In and out. Zero trace.',
    'Done before the ICE woke up.'],
  busy: 'Running a dozen daemons at once...',
  model: '[SYS] new wetware loaded: {0}',
  poke: ['Hey! Watch the chrome, choom.', '[PING] ...pong.', "Easy, the visor's sensitive.",
    "Need something? I'm all sensors.", 'Tap registered. Hi, choom.',
    'Careful. I bite in binary.'],
  poke_spam: ['[ALERT] Input flood! Back off, choom!', 'Poke me again and I call MaxTac.'],
  pet: ['...preem. Keep that up, choom.', '[SYS] affection.exe: running.',
    "Visor's going pink... d-don't stop."],
  rg_memory: '[ICE] {0} was eating the RAM. Flatlined it, choom.',
  rg_vram: '[ICE] {0} jacked the VRAM. Zeroed.',
  rg_game_vram: '[WARN] {0} is maxing the VRAM. Dial the textures down, choom.',
  rg_panic: 'Panic button hit. {0} is history.',
  rg_stuck: "[WARN] RAM's redlining and I can't find the culprit...",
  rg_agents: "Every agent job's been cut. Sessions still jacked in.",
};

const has = (set, ...tags) => tags.some((t) => set.has(t));
const hex2 = (v) => v.toString(16).toUpperCase().padStart(2, '0');

export class Mascot extends BaseMascot {
  constructor(cols = 64, rows = 18) {
    super(cols, rows);
    this.cols = Math.max(int(cols), COMP_W + 2);
    this.rows = Math.max(int(rows), H + 2);
    this.cx = fd(this.cols - COMP_W, 2);
    this.top = this.rows - 1 - H;
    this.stream = streamLines(CODE, PW - 3, 67);
    this.city = [];
    let x = 0;
    let i = 0;
    while (x < this.cols) {
      const r = lcg(113, i, this.cols);
      const w = 3 + int(r() * 6);
      const h = 3 + int(r() * Math.max(3, fd(this.rows, 2) - 1));
      this.city.push([x, w, h, i]);
      x += w + (r() < 0.3 ? 1 : 0);
      i += 1;
    }
    this.sign = this.city.length > 2 ? this.city[fd(this.city.length * 3, 4)] : null;
  }

  bubble(state, tool = null, mood = null) {
    if (state === 'idle' && Object.hasOwn(MOOD_BUBBLES, mood || '')) return MOOD_BUBBLES[mood];
    return super.bubble(state, tool, mood);
  }

  // ---------- composition ----------

  render(state, tick, tool = null, mood = null, scene = new Set()) {
    if (!STATES.includes(state)) state = 'idle';
    if (!MOODS.includes(mood)) mood = 'calm';
    if (mood === 'sleepy' && ACTIVE.includes(state)) mood = 'focused';
    tick = int(tick);
    const cv = new Canvas(this.cols, this.rows);
    const kind = state === 'tool' ? propKind(tool) : null;
    const idle = state === 'idle';
    const wet = has(scene, 'rain', 'storm');
    const sky = scene.has('festive') ? 3 : 0;

    let dy = 0;
    if (state === 'done') dy = [0, 1, 1, 0, 0, 0][mod(tick, 6)];
    else if (idle && mood === 'excited' && mod(fd(tick, 30), 2)) dy = [0, 1, 1, 0, 0, 0][mod(tick, 6)];
    else if (idle && scene.has('music') && !['sad', 'sleepy'].includes(mood)) dy = mod(fd(tick, 4), 2);
    const top = this.top - Math.min(dy, this.top - 1);
    this.head = [this.cx + 15, Math.max(top + 1, sky)];

    this.sky(cv, tick, scene, sky);
    this.skyline(cv, tick, scene.has('day'));
    if (scene.has('storm')) this.lightning(cv, tick, sky);
    if (scene.has('fog')) this.fog(cv, tick);
    this.car(cv, tick, sky);
    if (scene.has('snow')) this.snow(cv, tick);
    const party = scene.has('party');
    const umbrella = wet && top - (party ? 3 : 2) >= 0;
    const dry = umbrella ? this.dry(top, party) : null;
    if (wet || !has(scene, 'sun', 'snow')) this.rain(cv, tick, dry);
    if (scene.has('festive')) this.banner(cv, tick);
    if (scene.has('hot')) this.shimmer(cv, tick);
    if (kind === 'helpers') this.drones(cv, tick);
    if (scene.has('clock')) this.alarm(cv, tick);
    this.droid(cv, state, tick, kind, top, mood, scene);
    this.holo(cv, state, tick, kind, mood, scene);

    if (umbrella) this.umbrella(cv, top, party);
    if (party) this.hat(cv, tick, top, !umbrella);
    if (wet && state !== 'error') this.downpour(cv, tick, dry, scene.has('storm'));
    if (scene.has('party')) this.confetti(cv, tick);
    this.moodMarks(cv, state, tick, top, mood, scene);
    this.scanline(cv, tick);
    if (state === 'error' || [0, 1].includes(mod(tick, 47))
      || (idle && this.fidget(tick, mood) === 'twitch' && mod(tick, 3) === 0)) {
      this.glitch(cv, tick, state === 'error');
    }
    return cv;
  }

  fidget(tick, mood) {
    if (mod(tick, 90) <= 80) return null;
    const table = FIDGETS[mood] || FIDGETS.calm;
    let pick = lcg(43, fd(tick, 90))() * table.reduce((a, [, w]) => a + w, 0);
    for (const [name, weight] of table) {
      pick -= weight;
      if (pick < 0) return name === 'rest' ? null : name;
    }
    return null;
  }

  // ---------- the city ----------

  skyline(cv, tick, day = false) {
    const bottom = this.rows - 1;
    for (const [x, w, h, i] of this.city) {
      const bg = BUILDING[i % BUILDING.length];
      const top = bottom - h + 1;
      cv.fill(x, top, w, h, bg, ' ', null, false);
      const r = lcg(127, i);
      for (let yy = top + 1; yy < bottom + 1; yy += 2) {
        for (let xx = x + 1; xx < x + w - 1; xx += 2) {
          const v = r();
          if (v < (day ? 0.82 : 0.45)) continue;
          const colour = [CYAN_DIM, MAGENTA_DIM, '#8f8a10', STEEL_DIM][int(v * 97) % 4];
          if (v > 0.93 && mod(fd(tick, 3) + xx, 7) === 0) continue;
          cv.cell(xx, yy, '.', colour, bg);
        }
      }
      cv.cell(x, top, w > 3 ? '_' : '.', STEEL_DIM, bg);
      if (h > 5 && i % 3 === 0) {
        cv.cell(x + fd(w, 2), top - 1, "'", mod(fd(tick, 8) + i, 3) ? MAGENTA : MAGENTA_DIM);
      }
    }
    if (this.sign) {
      const [x, w, h, i] = this.sign;
      const top = bottom - h + 1;
      const label = w >= 4 ? 'OPEN' : '24';
      const on = ![0, 3].includes(mod(fd(tick, 2), 23));
      cv.put(x + fd(w - label.length, 2), top + 1, label, on ? MAGENTA : MAGENTA_DIM, false,
        BUILDING[i % BUILDING.length]);
    }
  }

  car(cv, tick, sky = 0) {
    const period = 160;
    const t = mod(tick, period);
    if (t > this.cols + 8) return;
    const odd = mod(fd(tick, period), 2);
    const y = sky + (odd ? 1 : 2);
    const x = odd === 0 ? t - 6 : this.cols - t;
    cv.put(x, y, '<=o=>', STEEL);
    cv.cell(x + 2, y, 'o', CYAN);
    cv.cell(odd === 0 ? x - 1 : x + 5, y, '~', MAGENTA_DIM);
  }

  rain(cv, tick, dry = null) {
    const [xa, xb, yDry] = dry || [0, -1, 0];
    const n = fd(this.cols * this.rows, 40);
    for (let j = 0; j < n; j++) {
      const r = lcg(131, j);
      const speed = 0.8 + r() * 0.5;
      const x0 = int(r() * (this.cols + this.rows));
      const span = this.rows + 2;
      const y = mod(int(r() * span + tick * speed), span);
      const x = x0 - fd(y, 2);
      if (y >= yDry && xa <= x && x <= xb) continue;
      if (x >= 0 && x < this.cols && y >= 0 && y < this.rows && cv.get(x, y) === ' ') {
        cv.cell(x, y, j % 3 ? '/' : ',', RAIN, cv.bg[y][x]);
      }
    }
  }

  scanline(cv, tick) {
    const period = this.rows + 30;
    const y = mod(tick, period);
    if (y >= this.rows) return;
    for (let x = 0; x < this.cols; x++) {
      if (cv.get(x, y) !== ' ') cv.recolour(x, y, BRIGHT);
    }
  }

  // ---------- the scene ----------

  sky(cv, tick, scene, y) {
    const overcast = has(scene, 'cloud', 'rain', 'storm', 'snow', 'fog');
    if (scene.has('sun')) {
      ['  .-~-.  ', ' /=====\\ ', '(-------)'].forEach((r, k) => cv.put(2, y + k, r, [YELLOW, YELLOW, MAGENTA][k]));
      const band = mod(fd(tick, 6), 3);
      cv.put(4, y + 1, band ? '=====' : '-----', band === 1 ? MAGENTA : YELLOW);
    } else if (scene.has('day') && !overcast) {
      cv.put(2, y, '.-.', STEEL_DIM);
      cv.put(2, y + 1, '( )', STEEL_DIM);
      cv.put(2, y + 2, "'-'", STEEL_DIM);
    } else if (scene.has('night') && !overcast) {
      const x = this.cols - 6;
      cv.put(x, y, ' _', STEEL);
      cv.put(x, y + 1, '( ', STEEL);
      cv.put(x, y + 2, ' `-', STEEL);
    }
    if (overcast) this.clouds(cv, tick, y);
  }

  clouds(cv, tick, y0) {
    const span = this.cols + 16;
    for (let i = 0; i < Math.max(2, fd(this.cols, 28)); i++) {
      const r = lcg(47, i);
      const y = y0 + (this.rows >= 16 ? int(r() * 2) : 0);
      const x = mod(int(r() * span + tick / (9 + i * 3)), span) - 14;
      cv.put(x, y, '  .-~~-.   ', STEEL_DIM);
      cv.put(x, y + 1, ' (  ~~  )-.', STEEL_DIM);
      cv.put(x, y + 2, '(_________)', STEEL_DIM);
    }
  }

  lightning(cv, tick, y0) {
    const cyc = fd(tick, 37);
    const t = mod(tick, 37);
    if (t > 3) return;
    const x = int(lcg(53, cyc)() * (this.cols - 4)) + 1;
    const colour = t % 2 === 0 ? YELLOW : BRIGHT;
    [[0, '\\'], [1, '\\'], [1, '/'], [1, '\\'], [2, '\\']].forEach(([dx, ch], k) => {
      cv.cell(x + dx, y0 + 1 + k, ch, colour);
    });
    if (t === 0) {
      for (let yy = 0; yy < fd(this.rows, 2); yy++) {
        for (let xx = 0; xx < this.cols; xx++) cv.recolour(xx, yy, BRIGHT);
      }
    }
  }

  fog(cv, tick) {
    [fd(this.rows, 3), fd(this.rows, 2) + 1, this.rows - 3].forEach((y, k) => {
      const off = mod(fd(tick, 6 + k * 2) + k * 7, 12);
      for (let x = 0; x < this.cols; x++) {
        if (mod(x + off, 12) < 6 && mod(x + off, 12) !== 2) cv.cell(x, y, '~-'[(x + k) % 2], STEEL_DIM);
      }
    });
  }

  snow(cv, tick) {
    const span = this.rows + 2;
    const n = Math.max(6, fd(this.cols * this.rows, 60));
    for (let i = 0; i < n; i++) {
      const r = lcg(59, i);
      const speed = 0.05 + r() * 0.06;
      const pos = r() * span + tick * speed;
      const fall = int(fd(pos, span));
      const y = int(pos - fall * span) - 1;
      const x = int(lcg(61, i, fall)() * this.cols + [0, 1, 1, 0, 0, -1, -1, 0][mod(fd(tick, 5) + i, 8)]);
      const c = cv.get(x, y);
      if (c === ' ' || c === '.') cv.cell(x, y, '*.+'[i % 3], i % 2 ? BRIGHT : STEEL);
    }
  }

  banner(cv, tick) {
    for (let x = 0; x < this.cols; x++) cv.cell(x, 0, '~', STEEL_DIM);
    let n = 0;
    for (let x = 1; x < this.cols - 2; x += 5, n++) {
      const colour = CONFETTI[n % CONFETTI.length];
      const cut = ['o*o', '*o*', 'x+x', '+x+'][mod(n + fd(tick, 6), 4)];
      cv.put(x, 1, cut, PANEL, false, colour);
      cv.put(x, 2, mod(fd(tick, 4) + n, 2) ? 'VvV' : 'vVv', colour);
    }
  }

  shimmer(cv, tick) {
    for (const y of [this.rows - 2, this.rows - 4]) {
      for (let x = 0; x < this.cols; x++) {
        if (lcg(67, x, y, fd(tick, 3))() < 0.1 && cv.get(x, y) === ' ') {
          cv.cell(x, y, '~', [YELLOW, MAGENTA_DIM][x % 2], cv.bg[y][x]);
        }
      }
    }
  }

  alarm(cv, tick) {
    const x = this.cx - 9;
    const y = this.rows - 4;
    if (x < 2) return;
    const ring = mod(fd(tick, 2), 10) < 5;
    const shake = ring ? mod(tick, 2) : 0;
    cv.sprite(x + shake, y, '.-----.', MAGENTA, HOLO);
    cv.sprite(x + shake, y + 1, '|     |', MAGENTA, HOLO);
    cv.put(x + shake + 1, y + 1, ring || mod(tick, 4) ? 'ALARM' : '     ', YELLOW, false, HOLO);
    cv.sprite(x + shake, y + 2, "'-----'", MAGENTA, HOLO);
    if (ring) {
      cv.put(x - 2, y + 1, '((', YELLOW);
      cv.put(x + 8, y + 1, '))', YELLOW);
    }
  }

  hat(cv, tick, top, star = true) {
    const x = this.cx;
    if (star) cv.cell(x + 10, top - 2, '*+'[mod(fd(tick, 4), 2)], YELLOW);
    cv.sprite(x + 9, top - 1, '/:\\', MAGENTA);
    cv.cell(x + 10, top - 1, ':', YELLOW);
    cv.sprite(x + 8, top, '/_:_\\', CYAN);
    cv.cell(x + 10, top, ':', YELLOW);
  }

  dry(top, party = false) {
    return [this.cx + 3, this.cx + 17, top - (party ? 2 : 1)];
  }

  umbrella(cv, top, party = false) {
    const x = this.cx;
    const y = top - (party ? 4 : 3);
    cv.sprite(x, y, UMBRELLA[0], CYAN);
    cv.sprite(x, y + 1, UMBRELLA[1], CYAN);
    cv.cell(x + 4, y, '.', MAGENTA);
    cv.cell(x + 16, y, '.', MAGENTA);
    cv.cell(x + 10, y + 1, '|', STEEL);
    cv.cell(x + 10, top - (party ? 2 : 1), '|', STEEL);
  }

  downpour(cv, tick, dry, heavy = false) {
    const [xa, xb, yDry] = dry || [0, -1, 0];
    const span = this.rows + 2;
    const n = fd(this.cols * this.rows, heavy ? 12 : 20);
    for (let j = 0; j < n; j++) {
      const r = lcg(73, j);
      const speed = (heavy ? 1.2 : 0.9) + r() * 0.4;
      const x0 = int(r() * (this.cols + this.rows));
      const y = mod(int(r() * span + tick * speed), span);
      const x = x0 - fd(y, 2);
      if (y >= yDry && xa <= x && x <= xb) continue;
      if (x >= 0 && x < this.cols && y >= 0 && y < this.rows && !cv.occ[y][x] && cv.get(x, y) === ' ') {
        cv.cell(x, y, j % 4 ? '/' : "'", CYAN_DIM, cv.bg[y][x]);
      }
    }
    const y = this.rows - 1;
    for (let x = 0; x < this.cols; x++) {
      if (lcg(79, x, fd(tick, 2))() < (heavy ? 0.1 : 0.05) && cv.get(x, y) === ' '
        && !cv.occ[y][x] && !(xa <= x && x <= xb)) {
        cv.cell(x, y, 'o.'[mod(tick, 2)], CYAN_DIM, cv.bg[y][x]);
      }
    }
  }

  confetti(cv, tick) {
    const span = this.rows + 2;
    const n = fd(this.cols * this.rows, 70);
    for (let i = 0; i < n; i++) {
      const r = lcg(83, i);
      const y = mod(int(r() * span + tick * (0.3 + r() * 0.2)), span) - 1;
      const x = int(r() * this.cols) + [0, 1, 0, -1][mod(fd(tick, 4) + i, 4)];
      if (x >= 0 && x < this.cols && y >= 0 && y < this.rows && !cv.occ[y][x] && cv.get(x, y) === ' ') {
        cv.cell(x, y, "*,'."[i % 4], CONFETTI[i % CONFETTI.length]);
      }
    }
  }

  // ---------- the android ----------

  droid(cv, state, tick, kind, top, mood, scene) {
    const x0 = this.cx;
    const idle = state === 'idle';
    const fidget = idle ? this.fidget(tick, mood) : null;
    const low = ['sad', 'sleepy'].includes(mood) && !['done', 'error'].includes(state);
    DROID.forEach((r, i) => cv.sprite(x0, top + i, r, [6, 7, 8].includes(i) ? STEEL_DIM : STEEL));
    for (const c of [6, 7, 13, 14]) cv.recolour(x0 + c, top + 1, low ? CYAN_DIM : CYAN);
    for (const c of [2, 3, 4, 5, 15, 16, 17, 18]) cv.recolour(x0 + c, top + 6, low ? MAGENTA_DIM : MAGENTA);
    let tip = mod(fd(tick, 5), 2) ? '*' : '+';
    let tipCol = MAGENTA;
    if (state === 'error') {
      tipCol = YELLOW;
    } else if (mood === 'sad' && state !== 'done') {
      cv.cell(x0 + 10, top, '\\', STEEL);
      tip = '.';
      tipCol = MAGENTA_DIM;
    } else if (mood === 'sleepy') {
      tipCol = MAGENTA_DIM;
    } else if (mood === 'excited') {
      tip = '*';
      tipCol = CONFETTI[mod(tick, 3)];
    } else if (mood === 'curious' && idle) {
      tip = '?';
      tipCol = YELLOW;
    }
    if (scene.has('clock') && mod(fd(tick, 3), 2) && state !== 'error') {
      tip = '!';
      tipCol = YELLOW;
    }
    cv.cell(x0 + (mood === 'sad' && !['done', 'error'].includes(state) ? 11 : 10), top - 1, tip, tipCol);
    if (fidget === 'ping') {
      const t = mod(tick, 90) - 81;
      cv.put(x0 + 8 - fd(t, 3), top - 1, '(', CYAN_DIM);
      cv.put(x0 + 12 + fd(t, 3), top - 1, ')', CYAN_DIM);
    }
    const tap = ['tool', 'writing'].includes(state) || (idle && (scene.has('game') || mood === 'worried'));
    if (tap && mod(fd(tick, 3), 2)) {
      cv.put(x0, top + 8, '|=', CYAN);
      cv.put(x0 + 19, top + 8, '=|', CYAN);
    }
    if (state === 'done' || (idle && mood === 'excited' && mod(fd(tick, 30), 2) && mod(fd(tick, 6), 2))) {
      cv.put(x0, top + 7, '\\o', CYAN);
      cv.put(x0 + 19, top + 7, 'o/', CYAN);
    } else if (fidget === 'wave') {
      cv.put(x0 + 19, top + 7, mod(fd(tick, 2), 2) ? 'o/' : 'o_', CYAN);
    }
    let core = mod(fd(tick, 6), 2) ? CYAN : MAGENTA;
    if (state === 'error') core = mod(fd(tick, 2), 2) ? MAGENTA : MAGENTA_DIM;
    else if (mood === 'excited') core = CONFETTI[mod(fd(tick, 2), 3)];
    else if (low) core = mod(fd(tick, 12), 2) ? CYAN_DIM : MAGENTA_DIM;
    cv.put(x0 + 8, top + 7, '     ', STEEL, false, VISOR);
    cv.cell(x0 + CORE[1], top + CORE[0], ['done', 'tool'].includes(state) ? '@' : 'o', core, VISOR);
    if (scene.has('cold')) {
      Array.from('=~=~=~=').forEach((c, k) => cv.cell(x0 + 7 + k, top + 6, c, [MAGENTA, CYAN][k % 2]));
    }
    if (scene.has('music')) {
      for (const k of [3, 4]) {
        cv.cell(x0 + 3, top + k, '[', MAGENTA);
        cv.cell(x0 + 17, top + k, ']', MAGENTA);
      }
    }
    this.visor(cv, state, tick, x0 + VISOR_X, top + VISOR_Y, mood, fidget, scene.has('game') && idle);
    let grille = idle ? MOOD_FACE[mood][2] : null;
    if (idle && mood === 'sleepy' && mod(tick, 70) < 10) grille = '_O_';
    if (grille) cv.put(x0 + 9, top + 4, grille, MOOD_FACE[mood][1]);
    else if (['writing', 'done'].includes(state) && mod(fd(tick, 2), 2)) cv.put(x0 + 9, top + 4, '___', CYAN);
    if (mood === 'shy' && state !== 'error') {
      cv.put(x0 + 5, top + 4, '//', MAGENTA);
      cv.put(x0 + 14, top + 4, '//', MAGENTA);
    }
  }

  visor(cv, state, tick, x, y, mood, fidget, game) {
    let cells;
    let colour = mood === 'calm' ? CYAN : MOOD_FACE[mood][1];
    if (state === 'thinking') {
      const p = mod(fd(tick, 2), VISOR_W * 2 - 2);
      const pos = p < VISOR_W ? p : VISOR_W * 2 - 2 - p;
      cells = [];
      for (let k = 0; k < VISOR_W; k++) {
        const d = Math.abs(k - pos);
        cells.push(d === 0 ? 'o' : (d === 1 ? '=' : '-'));
      }
      colour = mood === 'calm' ? MAGENTA : colour;
    } else if (state === 'error') {
      cells = Array.from('x   x');
      colour = MAGENTA;
    } else if (state === 'done') {
      cells = Array.from({ excited: '*   *', shy: '>   <' }[mood] ?? '^   ^');
    } else if (state === 'writing') {
      cells = [];
      for (let k = 0; k < VISOR_W; k++) cells.push('=-_~'[int(lcg(137, k, fd(tick, 2))() * 4)]);
    } else if (state === 'tool' || game) {
      cells = Array.from(WORK_FACE[mood] ?? ' o  o');
    } else {
      cells = Array.from(MOOD_FACE[mood][0]);
      if (mood === 'sleepy' && mod(fd(tick, 25), 5) === 0) cells = Array.from('o   o');
      if (fidget === 'scan') {
        const p = mod(tick, 90) - 81;
        cells = new Array(VISOR_W).fill('-');
        cells[Math.min(VISOR_W - 1, fd(p, 2))] = 'o';
      } else if (fidget === 'dip') {
        cells = mod(tick, 3) ? Array.from('_   _') : new Array(VISOR_W).fill(' ');
      }
    }
    if (blink(tick, 59) && ['idle', 'tool'].includes(state)) {
      cells = cells.map((c) => ('oO*^'.includes(c) ? '-' : c));
    }
    cells.forEach((c, k) => cv.cell(x + k, y, c, colour, VISOR));
  }

  moodMarks(cv, state, tick, top, mood, scene) {
    const x0 = this.cx;
    const idle = state === 'idle';
    if (idle && mood === 'sleepy') {
      const t = mod(tick, 36);
      for (let j = 0; j < 3; j++) {
        if (fd(t, 9) > j) cv.cell(x0 + 18 + j, top + 2 - j, 'zzZ'[j], j < 2 ? CYAN_DIM : CYAN);
      }
    } else if (idle && mood === 'curious' && mod(fd(tick, 5), 4)) {
      cv.cell(x0 + 15, top, '?', YELLOW);
    } else if (idle && mood === 'excited' && mod(fd(tick, 3), 4) < 2) {
      cv.cell(x0 + 2, top + 1, '!', YELLOW);
      cv.cell(x0 + 18, top + 1, '!', YELLOW);
    } else if (idle && mood === 'focused' && mod(fd(tick, 10), 3)) {
      cv.cell(x0 + 2, top + 3, '>', CYAN_DIM);
      cv.cell(x0 + 18, top + 3, '<', CYAN_DIM);
    } else if (['shy', 'happy'].includes(mood) && idle) {
      const t = mod(tick, mood === 'shy' ? 30 : 60);
      if (t < 12) cv.put(x0 + 17, top + 3 - fd(t, 4), '<3', mood === 'shy' ? MAGENTA : GREEN);
    }
    if (mood === 'sad' && !['done', 'error'].includes(state) && mod(tick, 40) < 16) {
      const t = fd(mod(tick, 40), 4);
      cv.cell(x0 + 7, top + 4 + (t % 2), ",'"[t % 2], CYAN_DIM);
      if (state === 'idle') cv.cell(x0 + 13, top + 4 + ((t + 1) % 2), ",'"[(t + 1) % 2], CYAN_DIM);
    }
    if ((mood === 'worried' && state !== 'done') || scene.has('hot')) {
      const t = mod(tick, 16);
      if (t < 10) cv.cell(x0 + 17, top + 2 + (t >= 5 ? 1 : 0), t < 5 ? "'" : ',', CYAN);
    }
    if (scene.has('music') && state !== 'error') {
      for (let j = 0; j < 2; j++) {
        const t = mod(tick + j * 12, 24);
        if (t < 20) cv.cell(x0 - 1 - fd(t, 6) - j * 2, top + 5 - fd(t, 5), 'db'[j], [YELLOW, MAGENTA][j]);
      }
    }
    if (scene.has('cold') && idle) {
      const t = mod(tick, 40);
      if (t < 12) cv.cell(x0 + 17 + fd(t, 4), top + 4 - fd(t, 6), '.oO'[Math.min(2, fd(t, 4))], BRIGHT);
    }
  }

  // ---------- the holo panel ----------

  holo(cv, state, tick, kind, mood, scene) {
    const x = this.cx + W + 1;
    const y = this.top + 1;
    const frame = state === 'error' ? MAGENTA : CYAN_DIM;
    cv.fill(x, y, PW, PH, HOLO);
    cv.put(x, y, `+${'-'.repeat(PW - 2)}+`, frame, false, HOLO);
    cv.put(x, y + PH - 1, `+${'-'.repeat(PW - 2)}+`, frame, false, HOLO);
    for (let k = 1; k < PH - 1; k++) {
      cv.cell(x, y + k, '|', frame, HOLO);
      cv.cell(x + PW - 1, y + k, '|', frame, HOLO);
    }
    if (scene.has('clock')) cv.put(x + 2, y, '[!ALARM]', mod(fd(tick, 3), 2) ? YELLOW : MAGENTA, false, HOLO);
    const tx = x + 2;
    const ty = y + 1;
    const tw = PW - 4;
    const line = (k, text, colour = STEEL) => cv.put(tx, ty + k, text.slice(0, tw), colour, false, HOLO);

    if (state === 'thinking') {
      line(0, 'NEURAL//NET', MAGENTA);
      for (let k = 0; k < 3; k++) {
        const n = int((lcg(139, k, fd(tick, 3))() * 0.5 + 0.5) * (tw - 2));
        line(2 + k, '|'.repeat(n), [CYAN, VIOLET, MAGENTA][k]);
      }
      line(5, `${'-\\|/'[mod(tick, 4)]} sync`, CYAN_DIM);
    } else if (state === 'writing') {
      const ls = this.stream;
      const total = mod(tick, ls.length * tw);
      const li = fd(total, tw);
      const col = mod(total, tw);
      for (let k = 0; k < 6; k++) {
        const j = li - 5 + k;
        if (j < 0) continue;
        line(k, j < li ? ls[j] : ls[j].slice(0, col), k === 5 ? CYAN : STEEL_DIM);
      }
      if (mod(fd(tick, 3), 2)) cv.cell(tx + Math.min(col, tw - 1), ty + 5, '_', MAGENTA, HOLO);
    } else if (state === 'done') {
      line(0, '> gig://exit 0', CYAN_DIM);
      const on = mod(fd(tick, 3), 4) !== 0;
      line(2, '  JOB DONE', on ? YELLOW : CYAN);
      line(4, '+500 eddies', GREEN);
    } else if (state === 'error') {
      if (mod(fd(tick, 2), 3)) {
        line(1, 'ERR::', MAGENTA);
        line(2, 'SIGNAL LOST', MAGENTA);
      }
      const r = lcg(149, tick);
      let noise = '';
      for (let i = 0; i < tw; i++) noise += '#%@&*!'[int(r() * 6)];
      line(4, noise, MAGENTA_DIM);
      line(5, `rebooting${'.'.repeat(mod(tick, 4))}`, STEEL_DIM);
    } else if (state === 'tool') {
      HOLOS[kind].call(this, line, cv, tx, ty, tw, tick);
    } else if (scene.has('game')) {
      HOLOS.game.call(this, line, cv, tx, ty, tw, tick);
    } else {
      const [status, colour] = MOOD_FACE[mood][3];
      line(0, status, colour);
      line(2, mod(fd(tick, 5), 2) ? '> _' : '>', STEEL);
      if (scene.has('music')) {
        const r = lcg(173, fd(tick, 2));
        line(4, 'eq   ', MAGENTA_DIM);
        for (let k = 0; k < tw - 5; k++) cv.cell(tx + 5 + k, ty + 4, ' .:|'[int(r() * 4)], [MAGENTA, CYAN][k % 2], HOLO);
      } else {
        line(4, `net  ${'|'.repeat(2 + mod(fd(tick, 7), 6))}`, MAGENTA_DIM);
      }
      if (scene.has('hot') && mod(fd(tick, 4), 2)) line(5, 'THERMAL WARN', YELLOW);
      else if (scene.has('cold')) line(5, 'tmp  low *', CYAN);
      else if (mood === 'sleepy') line(5, `pwr  ${'|'.repeat(1 + mod(fd(tick, 20), 2))}`, MAGENTA_DIM);
      else if (mood === 'excited') line(5, `pwr  ${'|'.repeat(7)}${'+'.repeat(mod(fd(tick, 3), 2))}`, YELLOW);
      else line(5, `pwr  ${'|'.repeat(7)}`, CYAN_DIM);
    }
  }

  drones(cv, tick) {
    [Math.max(0, this.cx - 6), Math.min(this.cols - 5, this.cx + COMP_W + 1)].forEach((x, n) => {
      const y = this.top + 2 + (mod(fd(tick, 4) + n, 2) ? 1 : 0);
      const rotor = [' _-_ ', ' -_- '][mod(tick + n, 2)];
      cv.sprite(x, y, rotor, STEEL_DIM);
      cv.sprite(x, y + 1, DRONE[1], STEEL);
      cv.cell(x + 2, y + 1, 'o', mod(fd(tick, 3) + n, 2) ? CYAN : MAGENTA);
      cv.sprite(x, y + 2, DRONE[2], CYAN_DIM);
    });
  }

  // ---------- glitch ----------

  glitch(cv, tick, heavy) {
    if (heavy && mod(tick, 6) === 0) return;
    const r = lcg(167, tick);
    const rows = 2 + int(r() * (heavy ? 4 : 2));
    for (let i = 0; i < rows; i++) {
      const y = int(r() * this.rows);
      cv.shiftRow(y, int(r() * 7) - 3);
    }
    const n = fd(this.cols, heavy ? 3 : 6);
    for (let i = 0; i < n; i++) {
      const x = int(r() * (this.cols - 1));
      const y = int(r() * this.rows);
      const c = cv.get(x, y);
      if (c !== ' ' && c !== null && cv.get(x + 1, y) === ' ') cv.cell(x + 1, y, c, r() < 0.5 ? MAGENTA : CYAN);
    }
  }
}
Mascot.moods = true;
Mascot.BUBBLES = {
  idle: 'Systems online. Awaiting input, choom.',
  thinking: 'Running the neural net…',
  writing: 'Uploading thoughts to the grid…',
  done: "Job's done. Preem work.",
  error: 'ERR::SIGNAL LOST. Rebooting…',
};
Mascot.TOOL_BUBBLES = {
  Bash: 'Jacking into the shell…',
  Read: 'Scanning a data shard…',
  Edit: 'Rewriting the code-chrome…',
  MultiEdit: 'Rewriting the code-chrome…',
  Write: 'Burning a new shard…',
  NotebookEdit: 'Patching the notebook…',
  Grep: 'Tracing the pattern through the net…',
  Glob: 'Sweeping the file grid…',
  WebSearch: 'Pinging the net…',
  WebFetch: 'Pulling data off the net…',
  Task: 'Deploying drones…',
  Agent: 'Deploying drones…',
  TodoWrite: 'Updating the gig list…',
};
Mascot.OTHER = 'Running %s.exe…';

// The holo panel's contents per prop kind (mascot_cyberpunk._holo_<kind>),
// plus game (idle with a game running).
const HOLOS = {
  game(line, cv, tx, ty, tw, tick) {
    line(0, `SCORE ${String(mod(tick * 10, 10000)).padStart(4, '0')}`, YELLOW);
    const sway = [0, 1, 2, 1][mod(fd(tick, 6), 4)];
    for (let k = 0; k < 3; k++) {
      cv.put(tx + sway + k * 4, ty + 1, mod(fd(tick, 3), 2) ? '/o\\' : '\\o/', [GREEN, MAGENTA, CYAN][k], false, HOLO);
    }
    let ship = mod(fd(tick, 2), tw - 2);
    ship = mod(fd(fd(tick, 2), tw - 2), 2) === 0 ? ship : tw - 3 - ship;
    const shot = 4 - mod(tick, 4);
    cv.cell(tx + ship + 1, ty + shot, '|', YELLOW, HOLO);
    line(5, ' '.repeat(tw), STEEL);
    cv.put(tx + ship, ty + 5, '_A_', CYAN, false, HOLO);
  },
  bash(line, cv, tx, ty, tw, tick) {
    const cyc = fd(tick, 30);
    const cmd = CMDS[mod(cyc, CMDS.length)];
    const n = Math.min(cmd.length, fd(mod(tick, 30), 2));
    line(0, 'SHELL', MAGENTA);
    for (let k = 0; k < 3; k++) line(1 + k, `> ${CMDS[mod(cyc - 3 + k, CMDS.length)]}`, STEEL_DIM);
    line(5, `> ${cmd.slice(0, n)}`, CYAN);
    if (mod(fd(tick, 3), 2)) cv.cell(tx + 2 + n, ty + 5, '_', MAGENTA, HOLO);
  },
  book(line, cv, tx, ty, tw, tick) {
    line(0, '[#]== SHARD', YELLOW);
    const off = fd(tick, 3);
    for (let k = 1; k < 6; k++) {
      const r = lcg(151, off + k);
      const hexs = [0, 1, 2, 3].map(() => hex2(int(r() * 256))).join(' ');
      line(k, hexs, k === 1 + mod(fd(tick, 2), 5) ? CYAN : STEEL_DIM);
    }
  },
  pad(line, cv, tx, ty, tw, tick) {
    line(0, 'REWRITE', MAGENTA);
    const t = mod(tick, 60);
    const n = fd(t, 12);
    const part = mod(t, 12);
    const edits = [['-', 'old_chrome'], ['+', 'new_chrome'], ['-', 'if ice:'], ['+', 'if ok:'], ['+', ' jack()']];
    for (let k = 0; k < Math.min(5, n + 1); k++) {
      const [sign, text] = edits[k];
      const shown = k < n ? text : text.slice(0, part);
      line(1 + k, `${sign} ${shown}`, sign === '+' ? GREEN : MAGENTA_DIM);
    }
  },
  lens(line, cv, tx, ty, tw, tick) {
    line(0, 'TRACE', MAGENTA);
    for (let k = 1; k < 6; k++) {
      const r = lcg(157, k, fd(tick, 40));
      let s = '';
      for (let i = 0; i < tw; i++) s += 'abcdef0123:.'[int(r() * 12)];
      line(k, s, STEEL_DIM);
    }
    const rx = tx + mod(fd(tick, 2), tw - 4);
    const ry = ty + 1 + mod(fd(tick, 8), 4);
    cv.put(rx, ry, '[+]', mod(fd(tick, 2), 2) ? YELLOW : CYAN, false, HOLO);
    if (mod(fd(tick, 8), 4) === 3) line(5, 'MATCH', GREEN);
  },
  globe(line, cv, tx, ty, tw, tick) {
    line(0, 'NET//PULL', MAGENTA);
    const meridian = mod(fd(tick, 2), 4);
    [' .-:-. ', '( :|: )', '(-:+:-)', '( :|: )', " '-:-' "].forEach((r, k) => {
      const s = meridian === 1 || meridian === 3 ? r.replaceAll(':', '|') : r;
      cv.put(tx, ty + 1 + k, s, k % 2 ? CYAN_DIM : CYAN, false, HOLO);
    });
    const pct = mod(tick * 3, 101);
    cv.put(tx + 8, ty + 2, `${String(pct).padStart(3)}%`, STEEL, false, HOLO);
    for (let j = 0; j < 3; j++) {
      const px = tx + 7 + mod(tick + j * 3, 5);
      cv.cell(px, ty + 4, '<', j === 0 ? MAGENTA : MAGENTA_DIM, HOLO);
    }
  },
  helpers(line, cv, tx, ty, tw, tick) {
    line(0, 'DRONES', MAGENTA);
    const n = 1 + mod(fd(tick, 10), 2);
    for (let k = 0; k < n; k++) line(2 + k, `drone_${k + 1}  [up]`, CYAN);
    line(5, `link ${'='.repeat(2 + mod(tick, 6))}`, CYAN_DIM);
  },
  todo(line, cv, tx, ty, tw, tick) {
    line(0, 'GIGS', MAGENTA);
    const done = mod(fd(tick, 12), GIGS.length + 1);
    GIGS.forEach((gig, k) => {
      const box = k < done ? '[x]' : (k === done && mod(fd(tick, 3), 2) ? '[>]' : '[ ]');
      line(1 + k, `${box} ${gig}`, k < done ? STEEL_DIM : STEEL);
      if (k < done) cv.put(tx, ty + 1 + k, box, GREEN, false, HOLO);
    });
  },
  wand(line, cv, tx, ty, tw, tick) {
    line(0, 'ICE BREAKER', MAGENTA);
    const r = lcg(163, fd(tick, 2));
    for (let k = 1; k < 5; k++) {
      const off = int(r() * 3);
      let bolt = '';
      for (let c = 0; c < tw - off; c++) bolt += '\\/'[mod(c + k + tick, 2)];
      cv.put(tx + off, ty + k, bolt, [YELLOW, MAGENTA, VIOLET, CYAN][k - 1], false, HOLO);
    }
    line(5, '>> breach', CYAN);
  },
};

const theme = makeTheme({
  meta: { id: 'cyberpunk', name: 'Cyberpunk', panel: PANEL },
  Mascot,
  say: (topic) => word(TOPIC_LINES, topic),
  emoteColours: EMOTE_COLOURS,
});

export const { meta, lines, spans, render, bubble, say, emoteColours } = theme;
