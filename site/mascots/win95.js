// The win95 theme's mascot: a paperclip assistant with eyebrows, in a grey
// "Office Assistant" window on the teal desktop. A faithful port of Atrium's
// mascot_win95.py: the same (state, tick, tool, mood, scene, cols, rows) draws
// the same frame, cell for cell and colour for colour (golden tests:
// web/test/mascots-themes-a.test.mjs).
//
//   desktop  Windows 95 teal on every cell, My Computer and the Recycle Bin
//            down the left when there is room, the taskbar along the bottom:
//            Start, the assistant's button, the clock (the real local time:
//            setClock swaps the clock, for tests).
//   window   36 x 13 cells of face grey with a navy title bar and a shadow
//            edge: the bevel, as far as a cell grid can draw one.
//   the clip 13 x 11 cells on the window's left: eyes whose pupils look about
//            under the famous eyebrows, on a two-loop wire.
//   props    on the window's right: an MS-DOS Prompt for Bash, Notepad for
//            Read, Notepad and a pencil for Edit/Write, Find for the searches,
//            the copy dialog for WebFetch, two small clips for Task/Agent, a
//            checklist for TodoWrite, the hourglass otherwise; thinking lights
//            a bulb, done is an OK box, an error the illegal-operation box.
//   mood     his eyes, brows and pose (idle), the brows, sweat and blush at
//            work; an idle fidget (wiggle, look, wink, paper plane, yawn).
//   scene    weather only on teal cells, never on the window; a Weather
//            Channel ticker scrolls every tag along the window's bottom row.
//
// The 16-colour palette only, every grey dimmed (no white).

import { BaseMascot, Canvas, STATES, blink, lcg, propKind, streamLines, fd, mod, fmod, int, makeTheme }
  from './common.js';
import { ACTIVE, MOODS, word } from './mood.js';

export const PANEL = '#4a4a4a';

const TEAL = '#008080';
const NAVY = '#000080';
const GREY = '#acacac';
const HI = '#e4e4e4';
const SHADOW = '#727272';
const BLACK = '#000000';
const FIELD = '#cccccc';
const DIM = '#3a3a3a';
const YELLOW = '#d4c000';
const MAROON = '#800000';
const RED = '#e00000';
const GREEN = '#007a00';
const BLUE = '#1084d0';
const PICADO = [RED, YELLOW, HI, BLUE, GREEN];
const PICADO_IN = [RED, NAVY, GREEN, BLUE, MAROON];

const DW = 36;
const DH = 13;

const WIRE = [
  "   .-' '-.   ",
  '   |  _  |   ',
  '   | | | |   ',
  '   | | | |   ',
  '   | | | |   ',
  "   | '-' |   ",
  '   |     |   ',
  "   '-----'   ",
];
const SLUMP = 6;
const EYES_X = [2, 7];
const BROWS = {
  rest: ['~~', '~~'],
  up: ['__', '__'],
  think: ['_/', '~~'],
  sad: ['\\_', '_/'],
  glad: ['^^', '^^'],
  worry: ['_/', '\\_'],
  quirk: ['__', '~~'],
};
const MOOD_BROWS = {
  calm: ['rest', false, false],
  happy: ['rest', true, true],
  excited: ['up', true, true],
  shy: ['rest', false, false],
  curious: ['quirk', true, false],
  focused: ['sad', false, false],
  worried: ['worry', true, true],
  sad: ['worry', false, false],
  sleepy: ['up', false, false],
};
const WORK_BROWS = { focused: 'sad', worried: 'worry', sad: 'worry', excited: 'up' };
const FIDGETS = {
  calm: [['wiggle', 2], ['look', 2], ['plane', 1], ['wink', 1], ['rest', 2]],
  happy: [['wiggle', 2], ['wink', 2], ['plane', 2]],
  excited: [['wiggle', 3], ['plane', 2], ['wink', 1]],
  shy: [['look', 2], ['wink', 1], ['rest', 2]],
  curious: [['look', 3], ['wiggle', 2], ['plane', 1]],
  focused: [['rest', 3], ['look', 1]],
  worried: [['look', 3], ['rest', 1]],
  sad: [['rest', 2], ['look', 1]],
  sleepy: [['yawn', 3], ['rest', 2]],
};
const TICKER = [['sun', 'Sunny'], ['cloud', 'Cloudy'], ['rain', 'Rain'],
  ['storm', 'Severe storm warning'], ['snow', 'Snow'], ['fog', 'Dense fog'],
  ['hot', 'Heat advisory'], ['cold', 'Freeze warning'],
  ['music', 'Now playing: CD Player'], ['game', 'Solitaire running'],
  ['party', 'party.exe'], ['festive', 'Feliz dia!'],
  ['clock', 'Scheduled Task due soon']];

export const MOOD_BUBBLES = {
  happy: "It looks like you're having a great day! Want to keep it that way?",
  excited: "It looks like you're about to do something AMAZING! Can I help?!",
  shy: "Oh! I didn't see you there. Um... would you like help?",
  curious: "It looks like you're up to something. What are we making?",
  focused: "Focus mode on. I'll stay out of your way. Mostly.",
  worried: 'It looks like something might go wrong. Did you remember to save?',
  sad: 'It looks like nobody needs an assistant today...',
  sleepy: "It looks like you're tired. Starting screen saver... zzz",
};
export const TOPIC_LINES = {
  thanks: "It looks like you're saying thanks. You're welcome! Click OK.",
  worried: 'It looks like something went wrong. Would you like help?',
  frustrated: 'Abort, Retry, Ignore? ...Retry. One more time!',
  relieved: 'Phew! The problem has been resolved. Click OK to continue.',
  determined: 'This may take several minutes. Please do not turn off your computer.',
  proud: 'Task completed successfully. Nice work!',
  sad: 'The AI has performed an illegal operation and is very sorry.',
  happy: "It looks like you're happy! Shall I run party.exe?",
  curious: "It looks like you're asking a question. Searching Help Topics...",
  rain: 'Weather Channel: rain today. Take an umbrella.',
  storm: 'Severe storm warning! Save your work and stay inside.',
  snow: "It looks like it's snowing! Would you like to build a snowman?",
  fog: 'Weather Channel: dense fog. Visibility: 640 x 480.',
  sun: 'Weather Channel: sunny today. Go outside -- Solitaire will wait.',
  hot: 'Heat advisory: {0} outside. Even the fan is sweating.',
  cold: 'Brr, {0} outside. Sit closer to the CRT.',
  gpu_hot: 'Warning: the GPU has reached {0}°C. Not a fatal exception... yet.',
  cpu_hot: 'Warning: the CPU is at {0}°C. Is the fan plugged in?',
  music: "It looks like you're listening to music. Starting CD Player...",
  game: "It looks like you're playing a game. I won't tell your boss.",
  event: 'Scheduled Task: {0} in {1} min.',
  event_now: 'Reminder: {0} is starting now! Snooze is not an option.',
  birthday: "It looks like it's {0}'s birthday! Running party.exe...",
  holiday: 'Today is {0}! Feliz dia! Loading papel_picado.bmp...',
  go_home: "It's {0}. It looks like you're done for the day. Shut Down?",
  bedtime: "It's {0}. It is now safe to turn off your computer.",
  morning: 'Good morning! Windows is starting up...',
  lunch: "It looks like it's lunch time. Minimize all windows?",
  weekend: "It looks like it's the weekend! Solitaire, anyone?",
  friday: "It looks like it's Friday afternoon. Log off?",
  hello: ["Hi! It looks like you're starting something.",
    'Welcome to Windows 95! Need help?',
    "Hello! I'm your Office Assistant."],
  welcome_back: ['Welcome back! Your desktop has been restored.',
    "It looks like you're back! Resume?"],
  wake: ["Huh? Resuming from Standby... I'm up!",
    "Screen saver off! I wasn't sleeping."],
  long_prompt: ["It looks like you're writing a novel!",
    "Whoa. That's longer than AUTOEXEC.BAT."],
  code: ["It looks like you're writing code. Help?",
    'Code! Opening it in EDIT.COM...'],
  image: ['It looks like a picture. Opening Paint...',
    'Ooh, a bitmap! In 256 colors?'],
  aborted: ['Operation cancelled. Click OK.',
    'Ctrl+Alt+Del! Task ended.'],
  tests_pass: ['All tests passed. Click OK to continue.',
    'ScanDisk found no errors!',
    'Tests passed! Shall I run party.exe?'],
  tests_fail: ['It looks like a test failed. Retry?',
    'Tests failed. Abort, Retry, Fix?'],
  commit: ['Your files have been saved to C:\\.',
    'It looks like you shipped it! Click OK.',
    'Backup complete. Copied to floppy!'],
  fast: ['Done before the hourglass even turned!',
    'That was fast! Faster than a 486.',
    'Finished in record time. Click OK.'],
  busy: ["It looks like you're very busy. Need help?",
    "Multitasking! Don't press Ctrl+Alt+Del."],
  model: ['It looks like you installed {0}!',
    'New driver loaded: {0}.'],
  poke: ['It looks like you clicked me! Need help?',
    "Hey! I'm a paperclip, not a button.",
    '*wiggles eyebrows* Yes?',
    'Hi! Would you like a tip?',
    'Did you mean to double-click?',
    'Boing! Still here. Still helpful.'],
  poke_spam: ["It looks like you're clicking a lot. Stop?",
    "Stop! You'll bend my wire!",
    'Too many clicks! Not responding...'],
  pet: ['Aww. Nobody ever pets the paperclip!',
    "Nicer than 'Hide Assistant'. Thanks!",
    'Office Assistant is now happy.',
    '*blushes in 16 colors*'],
  rg_memory: "It looks like {0} was using all the memory. I've closed it for you!",
  rg_vram: 'It looks like {0} filled the graphics memory. It has been terminated.',
  rg_game_vram: 'It looks like {0} is using nearly all the graphics memory. Lower the details?',
  rg_panic: '{0} has performed an illegal operation and will be shut down.',
  rg_stuck: "Out of memory. Close some programs and try again. (I can't find which!)",
  rg_agents: 'All agent tasks have been ended. Your sessions are safe. Click OK.',
};
export const EMOTE_COLOURS = {
  accent: NAVY, warn: '#6a5000', bad: MAROON, good: '#006000', love: '#800080', dim: DIM,
};

const WORDS = ['Dear', 'Sir', 'the', 'and', 'model', 'file', 'is', 'ok', 'to', 'please',
  'find', 'all', 'yours', 'report', 'a', 'of', 'done', 'help'];
const DOS = ['dir', 'cd \\', 'mem', 'edit', 'ver', 'scandisk'];
const TODO = ['Backup', 'Defrag', 'Reply', 'Save'];

const has = (set, ...tags) => tags.some((t) => set.has(t));

// pyRound is Python's round(x) to an int: halves go to the even neighbour.
function pyRound(x) {
  const r = Math.round(x);
  return Math.abs(x % 1) === 0.5 ? 2 * Math.round(x / 2) : r;
}

// center is Python's str.center(width) (the odd space goes where CPython puts it).
function center(s, width) {
  const marg = width - s.length;
  if (marg <= 0) return s;
  const left = fd(marg, 2) + (marg & width & 1);
  return ' '.repeat(left) + s + ' '.repeat(marg - left);
}

// The taskbar clock's source: the real local time unless a test swaps it.
let clock = () => new Date();

// setClock swaps the clock the taskbar shows (a function returning a Date);
// null restores the real one.
export function setClock(fn) {
  clock = fn || (() => new Date());
}

export class Mascot extends BaseMascot {
  constructor(cols = 64, rows = 18) {
    super(cols, rows);
    this.cols = Math.max(int(cols), DW + 2);
    this.rows = Math.max(int(rows), DH + 1);
    this.wx = fd(this.cols - DW, 2);
    this.wy = Math.max(0, fd(this.rows - 1 - DH, 2));
    this.icons = this.wx >= 9;
    this.stream = streamLines(WORDS, 12, 57);
    this.sx = Math.max(this.wx + DW, this.cols - 9);
    this.sy = 0;
    this.stars = [];
    const n = Math.max(4, fd(this.cols * this.rows, 70));
    for (let i = 0; i < n; i++) {
      const r = lcg(13, i, this.cols, this.rows);
      const period = 20 + int(r() * 30);
      const x = int(r() * this.cols);
      const y = int(r() * Math.max(3, fd(this.rows * 2, 3)));
      this.stars.push([x, y, period, int(r() * period)]);
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
    if (mood === 'sleepy' && ACTIVE.includes(state)) mood = 'focused';
    tick = int(tick);
    const cv = new Canvas(this.cols, this.rows);
    const kind = state === 'tool' ? propKind(tool) : null;
    cv.fill(0, 0, this.cols, this.rows, TEAL, ' ', null, false);
    if (this.icons) this.drawIcons(cv, tick);
    this.taskbar(cv, tick, scene);
    this.window(cv, state, tick);
    if (scene.size) this.outside(cv, state, tick, scene);
    let dy = 0;
    if (state === 'done') dy = [0, 1, 1, 0, 0, 0, 0, 0][mod(tick, 8)];
    else if (state === 'idle' && mood === 'excited' && mod(fd(tick, 30), 2)) dy = [0, 1, 1, 0, 0, 0][mod(tick, 6)];
    const [x0, top] = this.clip(cv, state, tick, kind, dy, mood, scene);
    this.moodMarks(cv, state, tick, x0, top, mood, scene);
    this.head = [x0 + 10, Math.max(this.wy + 2, top + 1)];
    const px = this.wx + 16;
    const py = this.wy + 2;
    if (state === 'thinking') this.bulb(cv, tick, px, py);
    else if (state === 'writing') this.page(cv, tick, px, py, true, true);
    else if (state === 'done') this.msgbox(cv, tick, px, py, true);
    else if (state === 'error') this.msgbox(cv, tick, px, py, false);
    else if (kind) PROPS[kind].call(this, cv, tick, px, py);
    else if (scene.has('game')) this.solitaire(cv, tick, px, py);
    else if (mood === 'sleepy') this.screensaver(cv, tick, px, py);
    else this.idleTip(cv, tick, px, py);
    if (state === 'idle' && this.fidget(tick, mood) === 'plane') this.plane(cv, tick, x0);
    this.ticker(cv, tick, scene);
    return cv;
  }

  // ---------- the desktop ----------

  drawIcons(cv) {
    const x = Math.max(1, fd(this.wx - 6, 2));
    const y = 1;
    cv.put(x, y, '.--.', HI);
    cv.put(x, y + 1, '|__|', HI);
    cv.put(x, y + 2, '/__\\', GREY);
    cv.put(x, y + 3, 'MyPC', HI);
    if (this.rows >= 18) {
      cv.put(x, y + 5, ' __ ', GREY);
      cv.put(x, y + 6, '|::|', GREY);
      cv.put(x, y + 7, '|__|', GREY);
      cv.put(x, y + 8, 'Bin', HI);
    }
  }

  taskbar(cv, tick, scene) {
    const y = this.rows - 1;
    cv.fill(0, y, this.cols, 1, GREY, ' ', null, false);
    cv.put(0, y, '[Start]', BLACK, false, GREY);
    cv.cell(1, y, 'S', NAVY, GREY);
    if (this.cols >= 60) cv.put(8, y, '[Office Assistant]', BLACK, false, FIELD);
    const now = clock();
    const h = now.getHours();
    const text = ` ${mod(h - 1, 12) + 1}:${String(now.getMinutes()).padStart(2, '0')} ${h < 12 ? 'AM' : 'PM'} `;
    const ring = scene.has('clock') && mod(fd(tick, 5), 2);
    const cx = this.cols - text.length;
    cv.put(cx, y, text, BLACK, false, ring ? YELLOW : FIELD);
    const icons = scene.has('music') ? [['d', NAVY]] : [];
    if (scene.has('clock')) icons.push(['!', ring ? RED : MAROON]);
    const tx = cx - 2 * icons.length;
    cv.fill(tx, y, 2 * icons.length, 1, FIELD, ' ', null, false);
    icons.forEach(([c, colour], i) => cv.cell(tx + 1 + 2 * i, y, c, colour, FIELD));
  }

  window(cv, state, tick) {
    const x = this.wx;
    const y = this.wy;
    cv.fill(x, y, DW, DH, GREY);
    cv.fill(x + DW - 1, y + 1, 1, DH - 1, SHADOW);
    cv.fill(x + 1, y + DH - 1, DW - 1, 1, SHADOW);
    const active = state !== 'idle' || mod(fd(tick, 100), 2) === 0;
    cv.fill(x, y, DW - 1, 1, active ? NAVY : SHADOW);
    cv.put(x + 1, y, 'Office Assistant', HI, false, active ? NAVY : SHADOW);
    cv.put(x + DW - 6, y, '_', BLACK, false, GREY);
    cv.put(x + DW - 4, y, 'x', BLACK, false, GREY);
  }

  // ---------- outside: the scene on the desktop ----------

  // desk writes s on bare desktop only: never over the window, an icon, the
  // taskbar or what is already out there. solid claims the gaps between s's
  // ends too, so nothing falls through it.
  desk(cv, x, y, s, colour, solid = false) {
    if (!(y >= 0 && y < this.rows - 1)) return;
    let a = 0;
    while (a < s.length && s[a] === ' ') a++;
    let b = s.length;
    while (b > 0 && s[b - 1] === ' ') b--;
    for (let i = a; i < b; i++) {
      const xx = x + i;
      if (xx >= 0 && xx < this.cols && !cv.occ[y][xx] && cv.ch[y][xx] === ' ') {
        if (s[i] !== ' ') cv.cell(xx, y, s[i], colour);
        if (solid) cv.occ[y][xx] = true;
      }
    }
  }

  outside(cv, state, tick, scene) {
    this.sy = scene.has('festive') ? 2 : 0;
    const overcast = has(scene, 'cloud', 'rain', 'storm', 'snow', 'fog');
    if (scene.has('festive')) this.picado(cv, tick);
    if (scene.has('clock')) this.alarm(cv, tick);
    if (overcast) this.clouds(cv, tick, scene);
    else if (scene.has('sun') || scene.has('day')) this.sun(cv, tick, scene.has('sun'));
    else if (scene.has('night')) {
      [' .-', '(  ', ' `-'].forEach((row, k) => this.desk(cv, this.sx + 2, this.sy + k, row, YELLOW, true));
    }
    if (scene.has('storm')) this.lightning(cv, tick);
    if (scene.has('fog')) this.fog(cv, tick);
    if (scene.has('snow')) this.snowdrift(cv);
    if (scene.has('cold')) this.icicles(cv, tick, scene);
    if (scene.has('hot')) this.shimmer(cv, tick);
    if (scene.has('night')) {
      for (const [x, y, period, ph] of this.stars) {
        if (overcast && mod(x + y, 3)) continue;
        const t = mod(tick + ph, period);
        this.desk(cv, x, y, t < 5 ? '.+*+.'[t] : '.', t < 5 ? YELLOW : GREY);
      }
    }
    if (has(scene, 'rain', 'storm')) {
      const heavy = scene.has('storm');
      this.fall(cv, tick, 71, fd(this.cols * this.rows, heavy ? 20 : 32),
        heavy ? 0.7 : 0.5, heavy ? "',/" : "','", FIELD);
    }
    if (scene.has('snow')) this.fall(cv, tick, 59, fd(this.cols * this.rows, 50), 0.06, '*.+', HI, true);
    if (scene.has('party')) this.fall(cv, tick, 79, fd(this.cols * this.rows, 45), 0.15, "*'.o", PICADO, true);
  }

  fall(cv, tick, seed, n, speed, glyphs, colour, drift = false) {
    const span = this.rows + 1;
    for (let i = 0; i < n; i++) {
      const r = lcg(seed, i);
      const v = speed * (0.7 + r() * 0.6);
      const pos = r() * span + tick * v;
      const f = int(fd(pos, span));
      const y = int(pos - f * span) - 1;
      let x = int(lcg(seed + 1, i, f)() * this.cols);
      if (drift) x += [0, 1, 1, 0, 0, -1, -1, 0][mod(fd(tick, 5) + i, 8)];
      const c = typeof colour === 'string' ? colour : colour[i % colour.length];
      this.desk(cv, x, y, glyphs[i % glyphs.length], c);
    }
  }

  sun(cv, tick, rays = false) {
    const x = this.sx + 1;
    const y = this.sy;
    if (rays) {
      const top = ['\\ | /', '  |  ', '\\   /'][mod(fd(tick, 8), 3)];
      this.desk(cv, x, y, top, YELLOW);
      this.desk(cv, x, y + 1, '-(O)-', YELLOW, true);
      this.desk(cv, x, y + 2, top[0] === '\\' ? '/ | \\' : '  |  ', YELLOW);
    } else {
      this.desk(cv, x, y, ' .-. ', YELLOW, true);
      this.desk(cv, x, y + 1, '(   )', YELLOW, true);
      this.desk(cv, x, y + 2, " '-' ", YELLOW, true);
    }
  }

  clouds(cv, tick, scene) {
    const dark = has(scene, 'storm', 'night');
    const flash = scene.has('storm') && mod(tick, 47) < 4 && mod(tick, 2) === 0;
    const colour = flash ? HI : (scene.has('storm') ? SHADOW : (dark ? GREY : HI));
    this.desk(cv, this.sx, this.sy, '  .--. ', colour, true);
    this.desk(cv, this.sx, this.sy + 1, '(____)', colour, true);
    const span = this.cols + 12;
    for (let i = 0; i < Math.max(1, fd(this.cols, 30)); i++) {
      const r = lcg(47, i);
      const y = (this.rows >= 18 ? int(r() * 3) : 0) + this.sy;
      const x = mod(int(r() * span + tick / (9 + i * 3)), span) - 10;
      this.desk(cv, x, y, '  .--.   ', colour, true);
      this.desk(cv, x, y + 1, '(___)__) ', colour, true);
    }
  }

  lightning(cv, tick) {
    const cyc = fd(tick, 47);
    const t = mod(tick, 47);
    if (t > 3) return;
    const colour = t % 2 === 0 ? YELLOW : HI;
    const bolt = [[1, '/'], [0, '/'], [1, '\\'], [2, '/']];
    for (const x of [this.sx + 3, int(lcg(53, cyc)() * (this.cols - 4)) + 1]) {
      bolt.forEach(([dx, ch], k) => this.desk(cv, x + dx, this.sy + 2 + k, ch, colour));
    }
  }

  fog(cv, tick) {
    [fd(this.rows, 3), fd(this.rows, 2) + 1, this.rows - 3].forEach((y, k) => {
      const off = mod(fd(tick, 6 + k * 2) + k * 7, 12);
      for (let x = 0; x < this.cols; x++) {
        if (mod(x + off, 12) < 6 && mod(x + off, 12) !== 2) this.desk(cv, x, y, '~', GREY);
      }
    });
  }

  snowdrift(cv) {
    const drift = '.-~~-._.-~-.__.-~~-._.-~-.__.-~~-.__';
    if (this.wy >= 1) this.desk(cv, this.wx, this.wy - 1, drift.slice(0, DW), HI);
    for (let x = 0; x < this.cols; x++) this.desk(cv, x, this.rows - 2, '_.-'[fd((x * 7) % 5, 2)], HI);
  }

  icicles(cv, tick, scene) {
    const y = this.wy + DH;
    if (y < this.rows - 1) {
      for (let x = this.wx + 2; x < this.wx + DW; x += 3) {
        this.desk(cv, x, y, x % 2 ? 'V' : 'v', HI);
        if (mod(fd(tick, 7) + x, 9) === 0) this.desk(cv, x, y + 1, "'", HI);
      }
    } else if (!scene.has('festive')) {
      for (let x = this.wx + 17; x < this.wx + DW - 2; x += 3) cv.cell(x, this.wy + 1, x % 2 ? 'V' : 'v', BLUE, GREY);
    }
  }

  shimmer(cv, tick) {
    for (const y of [this.rows - 2, this.rows - 3]) {
      for (let x = 0; x < this.cols; x++) {
        if (lcg(67, x, y, fd(tick, 3))() < 0.12) this.desk(cv, x, y, '~', YELLOW);
      }
    }
  }

  picado(cv, tick) {
    for (let x = 0; x < this.cols; x++) {
      this.desk(cv, x, 0, '-', GREY);
      if (x % 3 === 1) this.desk(cv, x, 1, 'Vv'[mod(fd(tick, 4) + fd(x, 3), 2)], PICADO[fd(x, 3) % PICADO.length]);
    }
    if (this.wy < 2) {
      for (let x = this.wx + 17; x < this.wx + DW - 2; x += 2) {
        cv.cell(x, this.wy + 1, 'Vv'[mod(fd(tick, 4) + fd(x, 2), 2)], PICADO_IN[fd(x, 2) % PICADO_IN.length], GREY);
      }
    }
  }

  alarm(cv, tick) {
    const x = Math.max(this.wx + DW, this.cols - 8) + 1;
    const y = this.rows - 5;
    const ring = mod(fd(tick, 2), 10) < 5;
    const shake = ring ? mod(tick, 2) : 0;
    this.desk(cv, x + shake, y, 'o.-.o', HI, true);
    this.desk(cv, x + shake, y + 1, '( | )', HI, true);
    cv.cell(x + shake + 2, y + 1, '|/-\\'[mod(fd(tick, 3), 4)], RED);
    this.desk(cv, x + shake, y + 2, "'-^-'", HI, true);
    if (this.cols - x >= 6) this.desk(cv, x, y + 3, 'Alarm', HI, true);
    if (ring) {
      this.desk(cv, x + 2, y - 1, '!', YELLOW);
      this.desk(cv, x - 1, y, '(', YELLOW);
      this.desk(cv, x + 6, y, ')', YELLOW);
    }
  }

  ticker(cv, tick, scene) {
    const items = TICKER.filter(([tag]) => scene.has(tag)).map(([, text]) => text);
    if (!items.length) return;
    const when = scene.has('night') ? 'Tonight: ' : (scene.has('day') ? 'Today: ' : '');
    const text = when + items.join(' * ');
    const x = this.wx + 16;
    const y = this.wy + DH - 2;
    const w = 18;
    let shown;
    if (text.length <= w) {
      shown = center(text, w);
    } else {
      const loop = `${text}  *  `;
      const off = mod(fd(tick, 2), loop.length);
      shown = (loop.slice(off) + loop).slice(0, w);
    }
    cv.put(x, y, shown, YELLOW, false, NAVY);
  }

  // ---------- the clip ----------

  fidget(tick, mood) {
    if (mod(tick, 90) < 72) return null;
    const table = FIDGETS[mood] || FIDGETS.calm;
    let pick = lcg(43, fd(tick, 90))() * table.reduce((a, [, w]) => a + w, 0);
    for (const [name, weight] of table) {
      pick -= weight;
      if (pick < 0) return name === 'rest' ? null : name;
    }
    return null;
  }

  clip(cv, state, tick, kind, dy, mood, scene) {
    const idle = state === 'idle';
    const fidget = idle ? this.fidget(tick, mood) : null;
    const slump = idle && ['sad', 'sleepy'].includes(mood);
    const dx = scene.has('cold') && state !== 'error' && mod(tick, 30) < 6 ? mod(tick, 2) : 0;
    const x0 = this.wx + 2 + dx;
    const top = this.wy + 1 - dy + (slump ? 1 : 0);
    const wire = slump ? WIRE.slice(0, SLUMP).concat(WIRE.slice(SLUMP + 1)) : WIRE;
    wire.forEach((r, i) => cv.put(x0, top + 3 + i, r, BLACK));
    for (let i = 1; i < wire.length - 1; i++) cv.recolour(x0 + 9, top + 3 + i, DIM);
    let brow;
    let raised;
    if (state === 'thinking') {
      const key = WORK_BROWS[mood] ?? 'think';
      brow = BROWS[key];
      raised = [key === 'think' || key === 'up', key === 'up'];
    } else if (state === 'error') {
      brow = BROWS.sad;
      raised = [false, false];
    } else if (state === 'done') {
      brow = BROWS.glad;
      raised = [false, false];
    } else if (idle) {
      const [key, ra, rb] = MOOD_BROWS[mood];
      brow = BROWS[key];
      raised = [ra, rb];
      if ((fidget === 'wiggle' && mod(fd(tick, 3), 2)) || fidget === 'yawn') {
        brow = BROWS.up;
        raised = [true, true];
      } else if (scene.has('music') && !['sad', 'sleepy'].includes(mood) && mod(fd(tick, 5), 2)) {
        raised = [true, true];
      }
    } else {
      const key = WORK_BROWS[mood] ?? 'rest';
      const up = key === 'up' || (key === 'rest' && mod(fd(tick, 25), 4) === 0);
      brow = BROWS[key];
      raised = [up, key === 'up'];
    }
    if (scene.has('party')) raised = [false, false];
    EYES_X.forEach((ex, k) => {
      const row = Math.max(this.wy + 1, top + (raised[k] ? 0 : 1));
      cv.put(x0 + ex + 1, row, brow[k], BLACK);
    });
    let look;
    if (['tool', 'writing'].includes(state)) look = 1;
    else if (state === 'thinking') look = mod(fd(tick, 15), 2);
    else if (idle && fidget === 'look') look = mod(fd(tick, 4), 2);
    else if (idle && mood === 'worried') look = mod(fd(tick, 3), 2);
    else if (idle && ['curious', 'focused'].includes(mood)) look = 1;
    else if (idle && ['sad', 'sleepy'].includes(mood)) look = 0;
    else if (idle) look = [0, 0, 1, 1, 0, 0, 0, 1][mod(fd(tick, 12), 8)];
    else look = 0;
    let shut = blink(tick, 47) && !['done', 'error'].includes(state);
    if (idle && (fidget === 'yawn' || (mood === 'sleepy' && mod(tick, 40) < 30))) shut = true;
    EYES_X.forEach((ex, e) => {
      cv.cell(x0 + ex, top + 2, '(', BLACK);
      cv.cell(x0 + ex + 3, top + 2, ')', BLACK);
      const pair = this.eye(state, mood, look, e, tick);
      const closed = shut || (fidget === 'wink' && e === 1);
      for (let k = 0; k < 2; k++) {
        const c = closed ? '-' : pair[k];
        cv.cell(x0 + ex + 1 + k, top + 2, c, BLACK, closed ? GREY : HI);
      }
    });
    if (fidget === 'yawn') cv.cell(x0 + 6, top + 3, 'O', BLACK);
    return [x0, top];
  }

  eye(state, mood, look, e) {
    if (state === 'error') return 'x ';
    if (state === 'done') return '^ ';
    let pupil = 'o';
    if (state === 'idle') {
      if (mood === 'happy') return '^^';
      if (mood === 'shy') return e === 0 ? ' >' : '< ';
      if (mood === 'sleepy') return '__';
      pupil = { excited: '*', sad: '.', curious: 'oO'[e] }[mood] ?? 'o';
    } else if (mood === 'excited') {
      pupil = '*';
    } else if (mood === 'sad') {
      pupil = '.';
    }
    return look === 0 ? `${pupil} ` : ` ${pupil}`;
  }

  moodMarks(cv, state, tick, x0, top, mood, scene) {
    const idle = state === 'idle';
    const work = ACTIVE.includes(state);
    const y1 = this.wy + 1;
    if ((mood === 'worried' && state !== 'done') || scene.has('hot')) {
      const t = mod(tick, 12);
      cv.cell(x0 + 11, Math.max(y1, top + 1) + fd(t, 4), "'',"[fd(t, 4)], BLUE);
    }
    if (mood === 'sad' && ['idle', 'error'].includes(state)) {
      const t = mod(tick, 15);
      cv.cell(x0 + 10, top + 3 + fd(t, 5), ",'."[fd(t, 5)], BLUE);
    }
    if (mood === 'shy' && state !== 'error') {
      cv.put(x0 + 1, top + 3, '//', RED);
      cv.put(x0 + 10, top + 3, '//', RED);
    }
    if (mood === 'curious' && (idle || (work && mod(fd(tick, 20), 3) === 0)) && mod(fd(tick, 5), 4)) {
      cv.cell(x0 + 12, Math.max(y1, top), '?', NAVY);
    }
    if (mood === 'excited' && (idle || state === 'done' || mod(fd(tick, 20), 3) === 0)
      && mod(fd(tick, 3), 4) < 2) {
      cv.cell(x0, Math.max(y1, top + 1), '!', RED);
      cv.cell(x0 + 12, Math.max(y1, top + 1), '!', RED);
    }
    if (idle && mood === 'sleepy') {
      const t = mod(tick, 36);
      for (let j = 0; j < 3; j++) {
        if (fd(t, 9) > j) cv.cell(x0 + 11 + j, Math.max(y1, top + 1 - j), 'zzZ'[j], NAVY);
      }
    }
    if (scene.has('music') && state !== 'error') {
      for (let j = 0; j < 2; j++) {
        const t = mod(tick + j * 12, 24);
        if (t < 20) cv.cell(x0 - 1 + j, top + 9 - fd(t, 4), 'db'[j], NAVY);
      }
    }
    if (scene.has('cold')) {
      cv.put(x0 + 2, top + 4, '=========', MAROON);
      cv.cell(x0 + 10, top + 5, '#', MAROON);
      cv.cell(x0 + 10, top + 6, mod(fd(tick, 6), 2) ? '#' : '%', MAROON);
    }
    if (scene.has('party')) {
      cv.put(x0 + 5, top, '/ \\', top > this.wy ? MAROON : HI);
      cv.cell(x0 + 6, top, '*', top > this.wy ? RED : YELLOW);
    }
  }

  // ---------- things beside him ----------

  frame(cv, x, y, w, h, title, body = FIELD, bar = NAVY) {
    cv.fill(x, y, w, h, body);
    cv.fill(x, y, w, 1, bar);
    cv.put(x + 1, y, title.slice(0, w - 2), HI, false, bar);
    cv.fill(x + w, y + 1, 1, h, SHADOW);
    cv.fill(x + 1, y + h, w, 1, SHADOW);
  }

  idleTip(cv, tick, x, y) {
    cv.put(x, y, 'Tip of the Day', NAVY, false, GREY);
    const tips = [['Press Enter', 'to send.'], ['Shift+Enter', 'adds a line.'],
      ['Ctrl+T opens', 'a terminal.'], ['Esc closes', 'this window.']];
    const [a, b] = tips[mod(fd(tick, 80), tips.length)];
    cv.put(x, y + 2, a, BLACK, false, GREY);
    cv.put(x, y + 3, b, BLACK, false, GREY);
    this.button(cv, x + 3, y + 6, 'Next >', mod(tick, 80) > 70);
  }

  screensaver(cv, tick, x, y) {
    this.frame(cv, x, y, 17, 7, 'Screen Saver', BLACK);
    const cx = x + 8;
    const cy = y + 3.5;
    for (let i = 0; i < 12; i++) {
      const r = lcg(89, i);
      const a = r() * 2 * Math.PI;
      const d = fmod(r() + tick * (0.02 + r() * 0.03), 1.0);
      const sx = int(pyRound(cx + Math.cos(a) * d * 9));
      const sy = int(pyRound(cy + Math.sin(a) * d * 3.4));
      if (x <= sx && sx < x + 17 && y + 1 <= sy && sy < y + 7) {
        cv.cell(sx, sy, '.+*'[Math.min(2, int(d * 3))], d > 0.5 ? HI : SHADOW, BLACK);
      }
    }
    cv.put(x, y + 8, 'Starfield  zZ', NAVY, false, GREY);
  }

  solitaire(cv, tick, x, y) {
    this.frame(cv, x, y, 18, 8, 'Solitaire', GREEN);
    const t = mod(tick, 120);
    const up = Math.min(4, fd(t, 15));
    cv.put(x + 1, y + 1, '[#]', HI, false, NAVY);
    for (let k = 0; k < 4; k++) {
      const fx = x + 6 + k * 3;
      if (k < up) cv.put(fx, y + 1, k < 3 ? 'A23'[k % 3] : 'K', k % 2 ? RED : BLACK, false, HI);
      else cv.cell(fx, y + 1, ':', HI, GREEN);
    }
    for (let k = 0; k < (t < 75 ? 4 : 0); k++) {
      for (let j = 0; j < Math.max(0, 4 - k - (up > k ? 1 : 0)); j++) {
        cv.put(x + 1 + k * 4, y + 3 + j, ` ${'KQJ9'[j]} `, (j + k) % 2 ? RED : BLACK, false, HI);
        cv.cell(x + 1 + k * 4, y + 3 + j, '|', SHADOW, HI);
      }
    }
    if (t >= 75) {
      const bounce = [0, 1, 2, 3, 4, 5, 4, 3, 2, 3, 4, 5, 4, 5];
      for (let s = 0; s < Math.min(bounce.length, fd(t - 75, 3) + 1); s++) {
        cv.cell(x + 15 - s, y + 1 + bounce[s], 'K', BLACK, HI);
      }
    }
  }

  plane(cv, tick, x0) {
    const t = mod(tick, 90) - 72;
    const x = x0 + 12 + t;
    if (x < this.wx + DW - 3) cv.put(x, this.wy + 1, '=>', NAVY, false, GREY);
  }

  button(cv, x, y, label, pressed = false) {
    const w = label.length + 2;
    cv.fill(x, y, w, 1, !pressed ? FIELD : SHADOW);
    cv.put(x + 1, y, label, BLACK, false, !pressed ? FIELD : SHADOW);
    cv.fill(x + w, y, 1, 1, !pressed ? SHADOW : GREY);
  }

  bulb(cv, tick, x, y) {
    const lit = mod(fd(tick, 6), 4) !== 0;
    const glow = lit ? YELLOW : SHADOW;
    cv.put(x + 4, y, '.-.', glow, false, GREY);
    cv.put(x + 3, y + 1, "( ' )", glow, false, GREY);
    cv.put(x + 4, y + 2, ')=(', DIM, false, GREY);
    cv.put(x + 4, y + 3, "'-'", DIM, false, GREY);
    if (lit) {
      for (const [dx, dy, c] of [[-1, 0, '\\'], [7, 0, '/'], [-2, 1, '-'], [8, 1, '-']]) {
        cv.cell(x + 4 + dx, y + dy, c, YELLOW, GREY);
      }
    }
    cv.put(x, y + 5, `Please wait${'.'.repeat(mod(fd(tick, 4), 4))}`, BLACK, false, GREY);
    this.hourglass(cv, tick, x + 14, y + 1);
  }

  hourglass(cv, tick, x, y) {
    const rows = mod(fd(tick, 8), 2) ? ['___', '\\:/', '/.\\', '---'] : ['___', '\\./', '/:\\', '---'];
    rows.forEach((r, i) => cv.put(x, y + i, r, BLACK, false, GREY));
  }

  page(cv, tick, x, y, pencil = false, stream = false) {
    this.frame(cv, x, y, 16, 8, 'Notepad');
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
    for (let k = 0; k < 6; k++) {
      const j = li - 5 + k;
      if (j < 0) continue;
      const text = j < li ? ls[mod(j, ls.length)] : ls[mod(j, ls.length)].slice(0, col);
      cv.put(x + 1, y + 1 + k, text, BLACK, false, FIELD);
    }
    if (pencil) {
      const px = x + 1 + Math.min(col, 11);
      cv.put(px, y + 5, '/', YELLOW, false, FIELD);
      cv.put(px + 1, y + 4, '/', MAROON, false, FIELD);
      if (mod(fd(tick, 3), 2)) cv.cell(px, y + 6, '_', BLACK, FIELD);
    }
  }

  msgbox(cv, tick, x, y, ok) {
    this.frame(cv, x, y, 18, 8, ok ? 'LLM chat' : 'Error', GREY, NAVY);
    if (ok) {
      cv.put(x + 1, y + 2, '(i)', HI, false, NAVY);
      cv.put(x + 5, y + 2, 'Completed', BLACK, false, GREY);
      cv.put(x + 5, y + 3, 'successfully', BLACK, false, GREY);
      this.button(cv, x + 5, y + 6, ' OK ', mod(tick, 30) > 26);
    } else {
      const flash = mod(fd(tick, 4), 2);
      cv.put(x + 1, y + 2, '(X)', HI, false, flash ? RED : MAROON);
      cv.put(x + 5, y + 2, 'Illegal', BLACK, false, GREY);
      cv.put(x + 5, y + 3, 'operation', BLACK, false, GREY);
      this.button(cv, x + 2, y + 6, 'Close', false);
      this.button(cv, x + 9, y + 6, 'Debug', false);
    }
  }
}
Mascot.moods = true;
Mascot.BUBBLES = {
  idle: "It looks like you're writing a prompt. Would you like help?",
  thinking: 'Please wait while the AI thinks…',
  writing: "It looks like you're writing a letter. The AI is typing…",
  done: 'Task completed successfully. Click OK to continue.',
  error: 'This program has performed an illegal operation.',
};
Mascot.TOOL_BUBBLES = {
  Bash: 'Starting MS-DOS Prompt…',
  Read: 'Opening document…',
  Edit: "It looks like you're editing a file. Need help?",
  MultiEdit: "It looks like you're editing a file. Need help?",
  Write: 'Saving to C:\\…',
  NotebookEdit: 'Editing the notebook…',
  Grep: 'Find: Files or Folders…',
  Glob: 'Searching My Computer…',
  WebSearch: 'Connecting to the Information Superhighway…',
  WebFetch: 'Downloading… 28.8 kbps',
  Task: 'Starting the Wizard: calling helpers…',
  Agent: 'Starting the Wizard: calling helpers…',
  TodoWrite: 'Updating your To Do list…',
};
Mascot.OTHER = 'Running %s.exe…';

// The prop beside him per prop kind (mascot_win95._prop_<kind>).
const PROPS = {
  bash(cv, tick, x, y) {
    this.frame(cv, x, y, 16, 7, 'MS-DOS Prompt', BLACK);
    const cyc = fd(tick, 30);
    const cmd = DOS[mod(cyc, DOS.length)];
    const n = Math.min(cmd.length, fd(mod(tick, 30), 2));
    for (let k = 0; k < 3; k++) {
      const prev = DOS[mod(cyc - 3 + k, DOS.length)];
      cv.put(x + 1, y + 1 + k, `C:\\>${prev}`.slice(0, 14), SHADOW, false, BLACK);
    }
    cv.put(x + 1, y + 5, `C:\\>${cmd.slice(0, n)}`.slice(0, 14), GREY, false, BLACK);
    if (mod(fd(tick, 3), 2)) cv.cell(x + 5 + n, y + 5, '_', GREY, BLACK);
  },
  book(cv, tick, x, y) {
    this.page(cv, tick, x, y);
    const hot = mod(fd(tick, 3), 6);
    for (let c = 0; c < 14; c++) cv.recolour(x + 1 + c, y + 1 + hot, HI, NAVY);
  },
  pad(cv, tick, x, y) {
    this.page(cv, tick, x, y, true, true);
  },
  lens(cv, tick, x, y) {
    this.frame(cv, x, y, 16, 8, 'Find: All Files', GREY);
    cv.put(x + 1, y + 1, 'Named:', BLACK, false, GREY);
    cv.put(x + 8, y + 1, '*.py   ', BLACK, false, FIELD);
    const hit = mod(fd(tick, 4), 4);
    ['theme.py', 'panel.py', 'front.py', 'clip.py'].forEach((f, k) => {
      const sel = k === hit;
      cv.put(x + 1, y + 3 + k, f.padEnd(14), sel ? HI : BLACK, false, sel ? NAVY : FIELD);
    });
    const mx = x + 10 + mod(fd(tick, 2), 4);
    cv.put(mx, y + 3 + hit, 'O-', MAROON, false, NAVY);
  },
  globe(cv, tick, x, y) {
    this.frame(cv, x, y, 17, 8, 'Copying...', GREY);
    cv.put(x + 1, y + 2, '[__]', BLACK, false, YELLOW);
    cv.put(x + 12, y + 2, '[__]', BLACK, false, YELLOW);
    const t = mod(tick, 10);
    const arc = [0, 1, 1, 2, 2, 2, 2, 1, 1, 0];
    cv.put(x + 4 + fd(t * 7, 10), y + 3 - arc[t] + 1, '#', HI, false, GREY);
    const pct = mod(tick * 2, 101);
    const n = int(pct / 100 * 15);
    cv.fill(x + 1, y + 5, 15, 1, FIELD);
    cv.fill(x + 1, y + 5, n, 1, NAVY);
    cv.put(x + 1, y + 6, `${pct}% of 28.8k`.slice(0, 15), BLACK, false, GREY);
  },
  helpers(cv, tick, x, y) {
    for (let n = 0; n < 2; n++) {
      const bx = x + n * 8;
      const hop = mod(fd(tick, 4) + n, 2);
      cv.put(bx + 1, y + 1 - hop, '~~', BLACK, false, GREY);
      cv.put(bx, y + 2 - hop, '(oo)', BLACK, false, GREY);
      cv.cell(bx + 1 + mod(fd(tick, 10) + n, 2), y + 2 - hop, 'o', BLACK, HI);
      cv.put(bx, y + 3 - hop, '|.-|', BLACK, false, GREY);
      cv.put(bx, y + 4 - hop, '||||', BLACK, false, GREY);
      cv.put(bx, y + 5 - hop, "|'-'", BLACK, false, GREY);
      cv.put(bx, y + 6 - hop, "'--'", BLACK, false, GREY);
    }
    if (y + 8 < this.wy + DH - 1) cv.put(x, y + 8, 'Wizard 1 of 2', NAVY, false, GREY);
  },
  todo(cv, tick, x, y) {
    this.frame(cv, x, y, 16, 7, 'To Do', FIELD);
    const done = mod(fd(tick, 12), TODO.length + 1);
    TODO.forEach((item, k) => {
      cv.put(x + 1, y + 1 + k, k < done ? '[x]' : '[ ]', k < done ? GREEN : BLACK, false, FIELD);
      cv.put(x + 5, y + 1 + k, item, k >= done ? BLACK : DIM, false, FIELD);
      if (k === done && mod(fd(tick, 3), 2)) cv.put(x + 1, y + 1 + k, '[>]', NAVY, false, FIELD);
    });
  },
  wand(cv, tick, x, y) {
    this.hourglass(cv, tick, x + 6, y + 1);
    cv.put(x + 1, y + 6, `Working${'.'.repeat(mod(fd(tick, 4), 4))}`, BLACK, false, GREY);
    const r = lcg(83, fd(tick, 5));
    for (let j = 0; j < 3; j++) {
      const sx = x + int(r() * 15);
      const sy = y + int(r() * 5);
      if (cv.get(sx, sy) === ' ') cv.cell(sx, sy, '*', NAVY, GREY);
    }
  },
};

const theme = makeTheme({
  meta: { id: 'win95', name: 'Windows 95', panel: PANEL },
  Mascot,
  say: (topic) => word(TOPIC_LINES, topic),
  emoteColours: EMOTE_COLOURS,
});

export const { meta, lines, spans, render, bubble, say, emoteColours } = theme;
