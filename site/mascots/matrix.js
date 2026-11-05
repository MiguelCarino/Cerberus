// The matrix theme's mascot: a hooded hacker at a terminal, in falling green
// digital rain. A faithful port of Atrium's mascot_matrix.py: the same (state,
// tick, tool, mood, scene, cols, rows) draws the same frame, cell for cell and
// colour for colour (golden tests: web/test/mascots-themes-a.test.mjs).
//
//   hacker   21 x 11 cells, front view: a hood with only the eyes glowing in its
//            shadow, hands on a laptop whose lid faces us.
//   monitor  17 x 9 cells to his right, on the same desk: a shell for Bash, a
//            hex dump for Read, a diff for Edit/Write, a highlighted match for
//            the searches, a download bar for WebFetch, a checklist for
//            TodoWrite, the white rabbit for anything else; agents in shades
//            step in for Task/Agent. Thinking decrypts a word, writing streams
//            code, done says ACCESS GRANTED.
//   ambient  digital rain over the whole stage, behind the sprites (ASCII
//            glyphs only: half-width katakana are wide in a CJK mono font). On
//            error the picture glitches: rows tear, cells flip to red noise.
//   mood     eyes, brows and mouth in the hood's shadow plus what floats by it;
//            idle, the monitor types the mood's line and a fidget plays.
//   scene    sun or moon, clouds, blue slanted rain under an umbrella, snow,
//            fog, heat, scarf, headphones, snake (game), party hat, papel
//            picado, an alarm clock (mood.js SCENES).

import { BaseMascot, Canvas, STATES, blink, lcg, propKind, streamLines, fd, mod, fmod, int, makeTheme }
  from './common.js';
import { MOODS, word } from './mood.js';

export const PANEL = '#010803';

const HEAD = '#a8ffbc';
const RAIN = ['#00ff41', '#00d936', '#00a82a', '#007a1f', '#005214', '#00330c'];
const HOOD = '#a8bcae';
const HOOD_DIM = '#5f7566';
const EYE = '#3dff6e';
const EDGE = '#3ddc68';
const TEXT = '#00ff41';
const TEXT_DIM = '#00a82a';
const TEXT_FAINT = '#00661a';
const RED = '#ff3030';
const RED_DIM = '#a01818';
const AMBER = '#ffb000';
const WHITE = '#eef5ef';
const HILITE_BG = '#00b32c';
const INK = '#010803';
const SUN = '#ffb000';
const MOON = '#d8f5dc';
const CLOUD = '#5f7566';
const CLOUD_DIM = '#33443a';
const WET = '#7fb2ff';
const BROLLY = '#8fa6c8';
const SNOW = '#eef5ef';
const FOG = '#7d9585';
const HEAT = '#a07a1a';
const BLUSH = '#ff7fa0';
const SWEAT = '#7fd0ff';
const SCARF = ['#d04040', '#eef5ef'];
const MUSIC = '#5fd7ff';
const PILL = ['#ff3030', '#3d7dff'];
export const EMOTE_COLOURS = { accent: HEAD, warn: AMBER, bad: RED, good: TEXT, love: BLUSH, dim: HOOD_DIM };
const CONFETTI = ['#ff5fd7', '#ffb000', '#5fd7ff', '#00ff41', '#ff3030', '#eef5ef'];

const GLYPHS = '0123456789ABCDEFZ:.=*+-<>|"^~;!?#$%&';

const W = 21;
const H = 11;
const HACKER = [
  '      .-~~~~~-.      ',
  "    .'         '.    ",
  '   /   .-----.   \\   ',
  '  |   /       \\   |  ',
  '  |   |       |   |  ',
  '  |   \\       /   |  ',
  "   \\   '-._.-'   /   ",
  "  __'.         .'__  ",
  " /    '-------'    \\ ",
  '|   _.=========._   |',
  '|__|_____(@)_____|__|',
];
const EYE_Y = 4;
const EYE_L = 8;
const EYE_R = 12;
const MOUTH = [5, 10];

const MW = 17;
const MONITOR = [
  '.---------------.',
  '|               |',
  '|               |',
  '|               |',
  '|               |',
  '|               |',
  '|               |',
  "'-------.-------'",
  '      __|__      ',
];
const SCREEN_W = 13;
const SCREEN_H = 6;

const AGENT = [' ___ ', '(=_=)', '/|Y|\\', ' | | '];

const COMP_W = W + MW;

const CODE_WORDS = ['if', 'for', '0x1f', 'int', 'ret', 'neo', '{', '}', '=>', 'jmp', 'mov',
  'ax', 'nil', 'and', 'or', 'fn', 'let', 'zion', '101', 'xor', '[]'];
const CMDS = ['ls -la', 'make', 'git log', 'ps aux', './run', 'ssh zion'];
const DECRYPT = ['THINKING', 'DECODING', 'TRACE', 'KNOCK KNOCK'];
const DIFF = [['-', 'old = 1'], ['+', 'new = 2'], ['-', 'if bug:'], ['+', 'if ok:'],
  ['+', '  return 0'], ['-', 'panic()']];
const TODO = ['jack in', 'find neo', 'dodge', 'exit'];

const MOOD_FACE = {
  calm: ['o', 'o', '_'],
  happy: ['^', '^', 'v'],
  excited: ['*', '*', 'D'],
  shy: ['>', '<', '~'],
  curious: ['o', 'O', 'o'],
  focused: ['o', 'o', '-'],
  worried: ['o', 'o', '~'],
  sad: ['.', '.', 'n'],
  sleepy: ['-', '-', '.'],
};
const BROWS = { focused: ['\\', '/', 1], sleepy: ['_', '_', 0], worried: ['/', '\\', 1], sad: ['/', '\\', 1] };
const FIDGETS = {
  calm: [['look', 2], ['type', 2], ['mug', 1], ['cat', 1], ['rest', 2]],
  happy: [['type', 2], ['cat', 2], ['mug', 1], ['look', 1]],
  excited: [['type', 3], ['cat', 2], ['stretch', 1], ['pill', 1]],
  shy: [['look', 2], ['mug', 1], ['rest', 2]],
  curious: [['look', 2], ['pill', 2], ['cat', 2]],
  focused: [['type', 4], ['mug', 1]],
  worried: [['look', 3], ['type', 1], ['rest', 1]],
  sad: [['mug', 1], ['rest', 2]],
  sleepy: [['stretch', 2], ['mug', 2], ['rest', 2]],
};
const MOOD_SCREEN = {
  calm: 'wake up...',
  happy: 'hello, neo.',
  excited: 'i know kungfu',
  shy: '...hi.',
  curious: 'the matrix?',
  focused: 'tail -f zion',
  worried: 'agents near',
  sad: 'no spoon...',
  sleepy: 'zzz...',
};
const RAIN_PACE = { excited: 1.3, sad: 0.7, sleepy: 0.5 };

export const MOOD_BUBBLES = {
  happy: '> hello, neo. good to see you.',
  excited: "> i know kung fu. let's go!",
  shy: "> …you weren't supposed to see me.",
  curious: '> what is the matrix? follow the white rabbit…',
  focused: '> tail -f /dev/zion. ready.',
  worried: '> agents nearby… stay sharp.',
  sad: '> there is no spoon. or joy.',
  sleepy: '> zzz… wake me when the rabbit shows.',
};
export const TOPIC_LINES = {
  thanks: "> ack. you're welcome, operator.",
  worried: "> uh-oh… something's in the code.",
  frustrated: '> again?! déjà vu. one more run…',
  relieved: '> phew. exit 0.',
  determined: '> long run… hold the line.',
  proud: '> done. access granted.',
  sad: '> sorry… my bad.',
  happy: '> yes!',
  curious: '> hmm… how deep does the rabbit hole go?',
  rain: '> rain in the real world. take an umbrella.',
  storm: '> storm in the real world. stay inside.',
  snow: '> snow. the real world is cold.',
  fog: '> fog out there. visibility: 0.',
  sun: '> sunny out. shades on.',
  hot: "> it's hot… {0}. fans to max.",
  cold: '> brr, {0} outside.',
  gpu_hot: '> gpu at {0}°C. throttling…',
  cpu_hot: '> cpu at {0}°C. throttling…',
  music: '> nice track. the code has a beat.',
  game: '> loading the training program…',
  event: "> {0} in {1} min. the phone's ringing.",
  event_now: '> {0} now. answer the phone.',
  birthday: "> it's {0}'s birthday. happy rezday!",
  holiday: '> today is {0}. ¡viva!',
  go_home: '> {0}. log out, operator. go home.',
  bedtime: '> {0}… unplug. go to bed.',
  morning: '> wake up, neo… good morning.',
  lunch: '> lunch? even the oracle bakes cookies.',
  weekend: '> weekend. unplug from the matrix.',
  friday: '> friday. the weekend is a construct too.',
  hello: ["> hello, operator. what's the task?", "> knock knock. i'm listening.",
    '> connection open. ready.'],
  welcome_back: "> you're back. the matrix kept your seat.",
  wake: "> huh? i'm awake. i'm in.",
  long_prompt: '> long transmission. decoding…',
  code: ["> code. i don't even see it anymore.", '> source incoming. reading…'],
  image: '> an image. blonde, brunette, redhead…',
  aborted: '> ctrl-c. jacking out.',
  tests_pass: ['> tests green. there is no bug.', '> all pass. exit 0.',
    '> green across the board.'],
  tests_fail: ['> tests red. a glitch in the code.', "> fail. déjà vu… let's look."],
  commit: ['> committed to the mainframe.', "> pushed. it's in the system now.",
    '> shipped. zion will see it.'],
  fast: ['> done. that was fast.', '> bullet time.', '> faster than an agent.'],
  busy: '> many programs running. hold on…',
  model: '> new program loaded: {0}',
  poke: ["> hey. i'm mid-hack here.", "> what? the phone's not ringing.",
    "> yes, i'm real. probably.", '> ping received. hi.',
    '> that tickles the mainframe.', "> careful. that's the red pill."],
  poke_spam: ["> stop. you're tripping the alarms.", '> enough! the agents will notice.'],
  pet: ['> …thanks. the matrix feels warmer.', '> ok. that was nice.',
    '> head pats. not in the training program.'],
  rg_memory: '> kill -9 {0}. memory leak contained.',
  rg_vram: '> {0} flooded the vram. process terminated.',
  rg_game_vram: '> {0} owns the vram. watching, not touching.',
  rg_panic: '> panic handler: {0} unplugged.',
  rg_stuck: '> oom imminent. no target in sight…',
  rg_agents: '> all agent processes down. operators still online.',
};

const CAT = [' /\\_/\\ ', '(=o.o=)'];

const has = (set, ...tags) => tags.some((t) => set.has(t));

export class Mascot extends BaseMascot {
  constructor(cols = 64, rows = 18) {
    super(cols, rows);
    this.cols = Math.max(int(cols), COMP_W + 2);
    this.rows = Math.max(int(rows), H + 2);
    this.cx = fd(this.cols - COMP_W, 2);
    this.top = this.rows - 1 - H;
    this.stream = streamLines(CODE_WORDS, SCREEN_W, 53);
    this.drops = [];
    for (let x = 0; x < this.cols; x++) {
      const r = lcg(41, x, this.cols);
      if (r() < 0.55) continue;
      const speed = 0.18 + r() * 0.42;
      const length = 3 + int(r() * Math.max(4, this.rows * 0.7));
      this.drops.push([x, speed, length, r() * (this.rows + length)]);
    }
  }

  bubble(state, tool = null, mood = null) {
    if (state === 'idle' && Object.hasOwn(MOOD_BUBBLES, mood || '')) return MOOD_BUBBLES[mood];
    return super.bubble(state, tool, mood);
  }

  // ---------- composition ----------

  render(state, tick, tool = null, mood = null, scene = new Set()) {
    if (!STATES.includes(state)) state = 'idle';
    const given = MOODS.includes(mood);
    if (!given) mood = 'calm';
    if (mood === 'sleepy' && ['thinking', 'tool', 'writing'].includes(state)) mood = 'focused';
    tick = int(tick);
    const cv = new Canvas(this.cols, this.rows);
    const kind = state === 'tool' ? propKind(tool) : null;
    const fidget = state === 'idle' && given ? this.fidget(tick, mood, scene) : null;
    this.head = [this.cx + 17, this.top + 1];
    this.rain(cv, state, tick, mood, scene);
    this.sky(cv, state, tick, scene);
    if (scene.has('festive')) this.banner(cv, tick);
    this.desk(cv);
    if (kind === 'helpers') this.agents(cv, tick);
    if (fidget === 'mug') this.mug(cv, tick);
    this.hacker(cv, state, tick, kind, mood, scene, fidget);
    this.monitor(cv, state, tick, kind, mood, scene);
    if (scene.has('clock')) this.alarm(cv, tick);
    if (fidget === 'cat') this.cat(cv, tick);
    if (scene.has('fog')) this.fog(cv, tick);
    if (scene.has('snow')) this.snow(cv, tick);
    if (scene.has('hot')) this.shimmer(cv, tick);
    if (scene.has('party')) this.confetti(cv, tick);
    if (has(scene, 'rain', 'storm')) {
      const dry = this.umbrella(cv, scene.has('party'));
      this.weatherRain(cv, tick, dry, scene.has('storm'));
    }
    if (scene.has('party')) this.hat(cv, tick);
    this.moodMarks(cv, state, tick, mood, scene);
    if (state === 'error') this.glitch(cv, tick);
    return cv;
  }

  rain(cv, state, tick, mood, scene) {
    const span = this.rows;
    let fast = state === 'done' ? 1.6 : (state === 'thinking' ? 0.5 : 1.0);
    fast *= RAIN_PACE[mood] ?? 1.0;
    const fogged = scene.has('fog') ? 2 : 0;
    const flash = scene.has('storm') && [0, 2].includes(mod(tick, 47));
    for (const [x, speed, length, ph] of this.drops) {
      const pos = ph + tick * speed * fast;
      const cyc = int(fd(pos, span + length));
      const head = int(fmod(pos, span + length));
      const red = state === 'error' && lcg(47, x, cyc)() < 0.35;
      const behind = this.cx - 1 <= x && x <= this.cx + COMP_W;
      for (let k = 0; k < length; k++) {
        const y = head - k;
        if (!(y >= 0 && y < this.rows)) continue;
        const g = GLYPHS[int(lcg(43, x, y, fd(tick + x * 7, k === 0 ? 2 : 7))() * GLYPHS.length)];
        let colour;
        if (k === 0) {
          colour = red ? RED : (flash ? WHITE : (behind ? RAIN[1 + fogged] : (fogged ? RAIN[fogged] : HEAD)));
        } else {
          const shade = Math.min(RAIN.length - 1, fd(k * RAIN.length, Math.max(1, length)) + (k > 1 ? 1 : 0)
            + (behind ? 2 : 0) + fogged);
          colour = red ? (k < fd(length, 2) ? RED_DIM : '#5a0c0c') : RAIN[shade];
        }
        cv.cell(x, y, g, colour);
      }
    }
  }

  desk(cv) {
    cv.sprite(this.cx - 1, this.rows - 1, '='.repeat(COMP_W + 2), HOOD_DIM);
  }

  // ---------- the hacker ----------

  fidget(tick, mood, scene) {
    if (mod(tick, 120) < 90) return null;
    const table = FIDGETS[mood] || FIDGETS.calm;
    let pick = lcg(79, fd(tick, 120))() * table.reduce((a, [, w]) => a + w, 0);
    for (const [name, weight] of table) {
      pick -= weight;
      if (pick < 0) {
        if (name === 'cat' && scene.has('clock')) return 'look';
        return name === 'rest' ? null : name;
      }
    }
    return null;
  }

  hacker(cv, state, tick, kind, mood, scene, fidget) {
    const x0 = this.cx;
    const top = this.top;
    const idle = state === 'idle';
    const game = idle && scene.has('game');
    const typing = ['writing', 'tool'].includes(state) || (idle && (
      game || fidget === 'type' || mood === 'focused'
      || (!['sad', 'sleepy'].includes(mood) && mod(fd(tick, 40), 3) === 0)));
    HACKER.forEach((r, i) => cv.sprite(x0, top + i, r, i < 9 ? HOOD : HOOD_DIM));
    const rim = scene.has('snow') ? SNOW : (scene.has('music') ? MUSIC : HOOD_DIM);
    cv.put(x0 + 8, top + 0, '~~~~~', rim);
    for (const c of [5, 15]) cv.cell(x0 + c, top + 1, cv.get(x0 + c, top + 1), HOOD_DIM);
    if (scene.has('music')) {
      for (const i of [3, 4, 5]) {
        cv.cell(x0 + 1, top + i, '[#['[i - 3], MUSIC);
        cv.cell(x0 + 19, top + i, ']#]'[i - 3], MUSIC);
      }
    }
    if (scene.has('cold')) {
      for (let c = 6; c < 15; c++) cv.cell(x0 + c, top + 7, '=', SCARF[c % 2]);
      cv.cell(x0 + 13, top + 8, '#', SCARF[0]);
    }
    let hand = 'o';
    if (typing) hand = mod(fd(tick, mood === 'excited' ? 1 : 2), 2) ? 'o' : '.';
    if (state === 'done' || fidget === 'stretch') {
      const up = state === 'done' ? mod(fd(tick, 4), 2) : 1;
      const col = state === 'done' ? EYE : HOOD;
      cv.put(x0, top + 7 - up, '\\o', col);
      cv.put(x0 + 19, top + 7 - up, 'o/', col);
    } else if (fidget === 'pill') {
      cv.cell(x0 + 2, top + 9, 'o', PILL[0]);
      cv.cell(x0 + 18, top + 9, 'o', PILL[1]);
    } else {
      const other = typing ? (hand === '.' ? 'o' : '.') : 'o';
      cv.cell(x0 + 2, top + 9, hand, EYE);
      cv.cell(x0 + 18, top + 9, other, EYE);
    }
    cv.put(x0 + 9, top + 10, '(@)', mod(fd(tick, 8), 2) ? TEXT : TEXT_DIM);
    this.face(cv, state, tick, mood, scene, fidget, game);
  }

  face(cv, state, tick, mood, scene, fidget, game) {
    const x0 = this.cx;
    const top = this.top;
    const idle = state === 'idle';
    let gaze = ['tool', 'writing'].includes(state) ? 1 : 0;
    if (state === 'thinking') gaze = [0, 1, 0, -1][mod(fd(tick, 12), 4)];
    let el = 'o';
    let er = 'o';
    let colour = EYE;
    let mouth = { done: 'v', error: 'o', thinking: '~' }[state] ?? '_';
    let mouthCol = !['done', 'error'].includes(state) ? HOOD_DIM : colour;
    if (state === 'done') {
      el = er = '^';
    } else if (state === 'error') {
      el = er = 'x';
      colour = mouthCol = RED;
    } else if (idle) {
      [el, er, mouth] = MOOD_FACE[mood];
      if (mood !== 'calm') mouthCol = HOOD;
      if (mood === 'happy') mouth = 'vw'[mod(fd(tick, 40), 2)];
      else if (mood === 'focused') gaze = 1;
      else if (mood === 'shy') gaze = [0, -1][mod(fd(tick, 30), 2)];
      else if (mood === 'worried') gaze = [0, -1, 0, 1][mod(fd(tick, 9), 4)];
      else if (mood === 'sleepy') {
        if (mod(tick, 70) < 10) mouth = 'O';
        else if (mod(fd(tick, 25), 5) === 0) el = er = 'o';
      }
    } else {
      if (mood === 'excited' && mod(fd(tick, 8), 3) === 0) el = er = '*';
      else if (mood === 'curious') er = 'O';
      mouth = { happy: 'v', excited: 'D', shy: '~', worried: '~', sad: 'n', focused: '-' }[mood] ?? mouth;
    }
    if (game) {
      el = 'o';
      er = 'o';
      gaze = 1;
    }
    if (fidget === 'look' || fidget === 'pill') {
      gaze = [-1, 1][mod(fd(tick, fidget === 'look' ? 6 : 10), 2)];
    } else if (fidget === 'stretch') {
      el = '-';
      er = '-';
      mouth = 'O';
    } else if (fidget === 'cat' && mod(tick, 120) >= 105) {
      el = 'O';
      er = 'O';
      gaze = 1;
    }
    if (el === '*') colour = HEAD;
    const shades = idle && scene.has('sun') && !scene.has('night') && !game
      && ['calm', 'happy', 'excited', 'focused'].includes(mood) && fidget !== 'stretch';
    const shut = blink(tick) && !['done', 'error'].includes(state) && !['-', '^', '>', '.'].includes(el);
    if (shades) {
      cv.put(x0 + 7, top + EYE_Y, '[=]-[=]', WHITE);
      cv.put(x0 + 8, top + EYE_Y, '=', TEXT_FAINT);
      cv.put(x0 + 12, top + EYE_Y, '=', TEXT_FAINT);
    } else {
      if (shut) el = er = '-';
      cv.cell(x0 + EYE_L + gaze, top + EYE_Y, el, shut ? HOOD : colour);
      cv.cell(x0 + EYE_R + gaze, top + EYE_Y, er, shut ? HOOD : colour);
    }
    cv.cell(x0 + MOUTH[1], top + MOUTH[0], mouth, mouthCol);
    if (Object.hasOwn(BROWS, mood) && !['done', 'error'].includes(state)) {
      const [left, right, inset] = BROWS[mood];
      cv.cell(x0 + EYE_L + inset, top + EYE_Y - 1, left, HOOD);
      cv.cell(x0 + EYE_R - inset, top + EYE_Y - 1, right, HOOD);
    }
    if (mood === 'shy' && state !== 'error') {
      for (const c of [8, 12]) cv.cell(x0 + c, top + MOUTH[0], '=', BLUSH);
    }
    if (mood === 'sad' && state !== 'done' && mod(tick, 40) < 12) {
      cv.cell(x0 + EYE_L + gaze, top + MOUTH[0], ",'"[mod(fd(tick, 4), 2)], SWEAT);
    }
  }

  // ---------- the monitor ----------

  monitor(cv, state, tick, kind, mood, scene) {
    const sx = this.cx + W;
    const sy = this.top + 2;
    const edge = state === 'error' && mod(fd(tick, 3), 2) ? RED : EDGE;
    MONITOR.forEach((r, i) => cv.sprite(sx, sy + i, r, i < 8 ? edge : HOOD_DIM));
    if (scene.has('snow')) cv.put(sx + 1, sy, '~'.repeat(MW - 2), SNOW);
    const tx = sx + 2;
    const ty = sy + 1;
    const line = (k, text, colour = TEXT) => cv.put(tx, ty + k, text.slice(0, SCREEN_W), colour);

    if (state === 'thinking') {
      this.decrypt(line, tick);
    } else if (state === 'writing') {
      this.code(line, cv, tx, ty, tick);
    } else if (state === 'done') {
      line(0, '> exit 0', TEXT_DIM);
      const on = mod(fd(tick, 4), 4) !== 0;
      line(2, '   ACCESS', on ? TEXT : TEXT_DIM);
      line(3, '   GRANTED', on ? TEXT : TEXT_DIM);
      line(5, `[${'#'.repeat(11)}]`, TEXT_DIM);
    } else if (state === 'error') {
      line(0, '> segfault', RED_DIM);
      if (mod(fd(tick, 3), 2)) {
        line(2, '   ACCESS', RED);
        line(3, '   DENIED', RED);
      }
      line(5, 'core dumped', RED_DIM);
    } else if (state === 'tool') {
      SCREENS[kind].call(this, line, cv, tx, ty, tick);
    } else if (scene.has('game')) {
      this.snake(cv, tx, ty, tick);
    } else {
      const slow = mood === 'sleepy' ? 2 : 1;
      line(0, mod(fd(tick, 5 * slow), 2) ? '> _' : '>', slow > 1 ? TEXT_DIM : TEXT);
      const msg = MOOD_SCREEN[mood] ?? MOOD_SCREEN.calm;
      const n = Math.min(msg.length, fd(mod(tick, 90), 4 * slow));
      line(2, msg.slice(0, n), slow > 1 ? TEXT_FAINT : TEXT_DIM);
      if (mod(tick, 90) > 60 && mood === 'calm') line(4, 'knock, knock', TEXT_FAINT);
    }
  }

  snake(cv, tx, ty, tick) {
    const at = (p) => {
      const q = mod(p, SCREEN_W * SCREEN_H);
      const row = fd(q, SCREEN_W);
      const col = mod(q, SCREEN_W);
      return [tx + (row % 2 === 0 ? col : SCREEN_W - 1 - col), ty + row];
    };
    const head = tick;
    const food = at(head + 9 - mod(head, 9) + 4);
    cv.cell(food[0], food[1], '*', AMBER);
    for (let k = 5; k > 0; k--) {
      const [x, y] = at(head - k);
      cv.cell(x, y, 'o', TEXT_DIM);
    }
    const [x, y] = at(head);
    cv.cell(x, y, '@', HEAD);
  }

  decrypt(line, tick) {
    const w = DECRYPT[mod(fd(tick, 60), DECRYPT.length)];
    const t = mod(tick, 60);
    const out = [];
    Array.from(w.padEnd(SCREEN_W)).forEach((c, i) => {
      const locked = t > 8 + i * 3;
      if (c === ' ' || locked) out.push(c);
      else out.push(GLYPHS[int(lcg(59, i, fd(tick, 2))() * GLYPHS.length)]);
    });
    line(0, '> decrypt', TEXT_DIM);
    line(2, out.join(''), TEXT);
    const spin = '-\\|/'[mod(tick, 4)];
    line(4, `${spin} ${'.'.repeat(mod(fd(tick, 4), 4))}`, TEXT_DIM);
  }

  code(line, cv, tx, ty, tick) {
    const ls = this.stream;
    const total = mod(tick, ls.length * SCREEN_W);
    const li = fd(total, SCREEN_W);
    const col = mod(total, SCREEN_W);
    for (let k = 0; k < SCREEN_H; k++) {
      const j = li - (SCREEN_H - 1) + k;
      if (j < 0) continue;
      const text = j < li ? ls[j] : ls[j].slice(0, col);
      line(k, text, k === SCREEN_H - 1 ? TEXT : TEXT_DIM);
    }
    if (mod(fd(tick, 3), 2)) cv.cell(tx + Math.min(col, SCREEN_W - 1), ty + SCREEN_H - 1, '_', HEAD);
  }

  agents(cv, tick) {
    const y = this.rows - 1 - AGENT.length;
    [Math.max(0, this.cx - 6), Math.min(this.cols - 5, this.cx + COMP_W + 1)].forEach((x, n) => {
      if (mod(fd(tick, 10), 4) < n + 1) return;
      const step = mod(fd(tick, 4) + n, 2);
      AGENT.forEach((r, i) => cv.sprite(x, y + i, r, i ? TEXT_DIM : HOOD));
      cv.put(x + 1, y + 1, '=_=', EYE);
      if (step) cv.put(x + 1, y + 3, '/ \\', TEXT_DIM);
    });
  }

  // ---------- the scene ----------

  sky(cv, state, tick, scene) {
    const overcast = has(scene, 'cloud', 'rain', 'storm', 'snow', 'fog');
    if (overcast) {
      const colour = scene.has('day') ? CLOUD : CLOUD_DIM;
      const span = this.cols + 16;
      for (let i = 0; i < Math.max(2, fd(this.cols, 28)); i++) {
        const r = lcg(83, i);
        const y = (this.rows >= 16 ? int(r() * 3) : 0) + (scene.has('festive') ? 2 : 0);
        const x = mod(int(r() * span + tick / (9 + i * 3)), span) - 14;
        cv.sprite(x, y, '  .--.     ', colour);
        cv.sprite(x, y + 1, ' (    ).-. ', colour);
        cv.sprite(x, y + 2, '(_________)', colour);
      }
    }
    if (scene.has('storm')) {
      const cyc = fd(tick, 47);
      const t = mod(tick, 47);
      if (t <= 3) {
        const x = int(lcg(89, cyc)() * (this.cols - 4)) + 1;
        [[1, '/'], [0, '/'], [0, '\\'], [1, '/']].forEach(([dx, ch], k) => {
          cv.sprite(x + dx + (k > 1 ? 1 : 0), 1 + k, ch, t % 2 === 0 ? WHITE : SUN);
        });
      }
    }
    if (state === 'error' || overcast) return;
    const y = scene.has('festive') ? 2 : 0;
    if (scene.has('sun') || scene.has('day')) {
      const rays = ['\\ | /', '  |  ', '\\   /'][mod(fd(tick, 8), 3)];
      cv.put(1, y, rays, SUN);
      cv.sprite(1, y + 1, '-(O)-', SUN);
      cv.put(1, y + 2, rays[0] === '\\' ? '/ | \\' : '  |  ', SUN);
    } else if (scene.has('night')) {
      cv.sprite(2, y, '.-', MOON);
      cv.sprite(1, y + 1, '(', MOON);
      cv.sprite(2, y + 2, '`-', MOON);
    }
  }

  banner(cv, tick) {
    cv.sprite(0, 0, '-'.repeat(this.cols), HOOD_DIM);
    for (let x = 0; x < this.cols; x++) {
      if (x % 3 === 1) {
        const flag = 'Vv'[mod(fd(tick, 4) + fd(x, 3), 2)];
        cv.sprite(x, 1, flag, CONFETTI[fd(x, 3) % CONFETTI.length]);
      }
    }
  }

  alarm(cv, tick) {
    const x = this.cx + W + MW - 6;
    const y = this.top;
    const ring = mod(fd(tick, 2), 10) < 5;
    const shake = ring ? mod(tick, 2) : 0;
    cv.sprite(x + shake, y, 'o.-.o', AMBER);
    cv.sprite(x + shake, y + 1, '(   )', WHITE);
    cv.cell(x + shake + 2, y + 1, '|/-\\'[mod(fd(tick, 3), 4)], AMBER);
    if (ring) {
      cv.cell(x - 1, y, '(', AMBER);
      cv.cell(x + 6, y, ')', AMBER);
      cv.cell(x + 6, y - 1, '!', AMBER);
    }
  }

  cat(cv, tick) {
    const t = mod(tick, 120) - 90;
    const x = this.cx + W + Math.min(mod(t, 15), MW - 7);
    const y = this.top;
    cv.sprite(x, y, CAT[0], HOOD_DIM);
    cv.sprite(x, y + 1, CAT[1], HOOD_DIM);
    cv.put(x + 2, y + 1, 'o', EYE);
    cv.put(x + 4, y + 1, 'o', EYE);
    if (t >= 15 && mod(t, 4) < 2) cv.cell(x + 7, y, '!', HEAD);
  }

  mug(cv, tick) {
    const x = this.cx - 4;
    const y = this.rows - 2;
    cv.sprite(x, y, 'c[_]', HOOD);
    cv.cell(x + 2, y - 1 - mod(fd(tick, 4), 2), 's~'[mod(fd(tick, 3), 2)], HOOD_DIM);
  }

  fog(cv, tick) {
    [fd(this.rows, 3), fd(this.rows, 2) + 1, this.rows - 3].forEach((y, k) => {
      const off = mod(fd(tick, 6 + k * 2) + k * 7, 12);
      for (let x = 0; x < this.cols; x++) {
        if (mod(x + off, 12) < 6 && mod(x + off, 12) !== 2 && !cv.occ[y][x]) cv.cell(x, y, '~', FOG);
      }
    });
  }

  snow(cv, tick) {
    const span = this.rows + 2;
    const n = Math.max(6, fd(this.cols * this.rows, 50));
    for (let i = 0; i < n; i++) {
      const r = lcg(97, i);
      const speed = 0.05 + r() * 0.06;
      const pos = r() * span + tick * speed;
      const fall = int(fd(pos, span));
      const y = int(pos - fall * span) - 1;
      const x = int(lcg(101, i, fall)() * this.cols + [0, 1, 1, 0, 0, -1, -1, 0][mod(fd(tick, 5) + i, 8)]);
      if (x >= 0 && x < this.cols && y >= 0 && y < this.rows && !cv.occ[y][x]) {
        cv.cell(x, y, '*.*'[i % 3], SNOW);
      }
    }
  }

  shimmer(cv, tick) {
    const y = this.rows - 2;
    for (let x = 0; x < this.cols; x++) {
      if (lcg(103, x, fd(tick, 3))() < 0.2) {
        const yy = y - mod(x + fd(tick, 3), 3);
        if (!cv.occ[yy][x]) cv.cell(x, yy, '~', HEAT);
      }
    }
  }

  confetti(cv, tick) {
    const span = this.rows;
    const n = Math.max(5, fd(this.cols * this.rows, 90));
    for (let i = 0; i < n; i++) {
      const r = lcg(107, i);
      const y = int(fmod(r() * span + tick * (0.15 + r() * 0.1), span));
      const x = int(r() * this.cols) + [0, 1, 0, -1][mod(fd(tick, 4) + i, 4)];
      if (x >= 0 && x < this.cols && !cv.occ[y][x]) cv.cell(x, y, ".,'"[i % 3], CONFETTI[i % CONFETTI.length]);
    }
  }

  hat(cv, tick) {
    const x = this.cx;
    const top = this.top;
    cv.cell(x + 10, top - 2, '*+'[mod(fd(tick, 4), 2)], AMBER);
    cv.sprite(x + 9, top - 1, '/:\\', CONFETTI[0]);
    cv.sprite(x + 8, top, '/_:_\\', CONFETTI[0]);
    cv.cell(x + 10, top - 1, ':', AMBER);
    cv.cell(x + 10, top, ':', AMBER);
  }

  umbrella(cv, party) {
    const x = this.cx;
    const y = this.top - (party && this.top >= 4 ? 4 : 2);
    cv.sprite(x, y, '   .-~~~~~~~~~~~-.   ', BROLLY);
    cv.sprite(x, y + 1, '  /_/_/_/_|_\\_\\_\\_\\  ', BROLLY);
    for (let yy = y + 2; yy < this.top; yy++) cv.sprite(x + 10, yy, '|', BROLLY);
    return [x + 2, x + 18, y + 2];
  }

  weatherRain(cv, tick, dry, heavy = false) {
    const [xa, xb, yDry] = dry;
    const span = this.rows + 2;
    const n = fd(this.cols * this.rows, heavy ? 12 : 22);
    for (let i = 0; i < n; i++) {
      const r = lcg(109, i);
      const speed = (heavy ? 0.9 : 0.6) + r() * 0.3;
      const y = int(fmod(r() * span + tick * speed, span)) - 1;
      const x = mod(int(r() * this.cols) - fd(y, 2), this.cols);
      if (y >= yDry && xa <= x && x <= xb) continue;
      if (y >= 0 && y < this.rows && !cv.occ[y][x]) cv.cell(x, y, i % 4 ? '/' : ',', WET);
    }
  }

  moodMarks(cv, state, tick, mood, scene) {
    const x0 = this.cx;
    const top = this.top;
    const idle = state === 'idle';
    if (idle && mood === 'sleepy') {
      const t = mod(tick, 36);
      for (let j = 0; j < 3; j++) {
        if (fd(t, 9) > j) cv.cell(x0 + 19 + j, top + 3 - j, 'zzZ'[j], j < 2 ? HOOD_DIM : HOOD);
      }
    } else if (idle && mood === 'curious' && mod(fd(tick, 5), 4)) {
      cv.cell(x0 + 19, top + 1, '?', HEAD);
    } else if (idle && mood === 'excited' && mod(fd(tick, 3), 4) < 2) {
      cv.cell(x0 + 1, top + 1, '!', HEAD);
      cv.cell(x0 + 19, top + 1, '!', HEAD);
    }
    if ((mood === 'worried' && state !== 'done') || scene.has('hot')) {
      const t = mod(tick, 16);
      if (t < 10) cv.cell(x0 + 19, top + 2 + (t >= 5 ? 1 : 0), t < 5 ? "'" : ',', SWEAT);
    }
    if (scene.has('music') && state !== 'error') {
      for (let j = 0; j < 2; j++) {
        const t = mod(tick + j * 12, 24);
        if (t < 20) cv.cell(x0 - 1 - fd(t, 6) - j * 2, top + 5 - fd(t, 5), 'db'[j], MUSIC);
      }
    }
    if (scene.has('cold') && idle) {
      const t = mod(tick, 40);
      if (t < 12) cv.cell(x0 + 1 - fd(t, 6), top + 6 - fd(t, 4), '.oO'[Math.min(2, fd(t, 4))], SNOW);
    }
  }

  // ---------- error ----------

  glitch(cv, tick) {
    if (mod(tick, 8) === 0) return;
    const r = lcg(73, tick);
    const n = 2 + int(r() * 3);
    for (let i = 0; i < n; i++) {
      const y = int(r() * this.rows);
      cv.shiftRow(y, int(r() * 7) - 3);
    }
    for (let i = 0; i < fd(this.cols, 4); i++) {
      const x = int(r() * this.cols);
      const y = int(r() * this.rows);
      if (cv.get(x, y) !== ' ') cv.cell(x, y, GLYPHS[int(r() * GLYPHS.length)], RED);
    }
  }
}
Mascot.moods = true;
Mascot.BUBBLES = {
  idle: 'Wake up… the terminal has you.',
  thinking: 'Decoding the signal…',
  writing: 'Typing straight into the Matrix…',
  done: 'Access granted. There is no bug.',
  error: 'A glitch in the Matrix. Déjà vu…',
};
Mascot.TOOL_BUBBLES = {
  Bash: 'Jacking into the shell…',
  Read: 'Reading the source code…',
  Edit: 'Rewriting the code…',
  MultiEdit: 'Rewriting the code…',
  Write: 'Writing a new program into the system…',
  NotebookEdit: 'Patching the construct…',
  Grep: 'Tracing the pattern…',
  Glob: 'Scanning the filesystem…',
  WebSearch: 'Searching the net…',
  WebFetch: 'Downloading a construct…',
  Task: 'Spawning agents…',
  Agent: 'Spawning agents…',
  TodoWrite: 'Updating the mission log…',
};
Mascot.OTHER = 'Follow the white rabbit: %s';

// The monitor's screen per prop kind (mascot_matrix._screen_<kind>).
const SCREENS = {
  bash(line, cv, tx, ty, tick) {
    const cyc = fd(tick, 30);
    const cmd = CMDS[mod(cyc, CMDS.length)];
    const n = Math.min(cmd.length, fd(mod(tick, 30), 2));
    for (let k = 0; k < 3; k++) line(k, `$ ${CMDS[mod(cyc - 3 + k, CMDS.length)]}`, TEXT_FAINT);
    line(3, 'ok', TEXT_FAINT);
    line(5, `$ ${cmd.slice(0, n)}`, TEXT);
    if (mod(fd(tick, 3), 2)) cv.cell(tx + 2 + n, ty + 5, '_', HEAD);
  },
  book(line, cv, tx, ty, tick) {
    const off = fd(tick, 3);
    for (let k = 0; k < SCREEN_H; k++) {
      const r = lcg(61, off + k);
      const hexs = [0, 1, 2, 3].map(() => int(r() * 256).toString(16).padStart(2, '0')).join(' ');
      const hot = k === mod(fd(tick, 2), SCREEN_H);
      line(k, hexs, hot ? TEXT : TEXT_DIM);
    }
  },
  pad(line, cv, tx, ty, tick) {
    const t = mod(tick, DIFF.length * 10);
    const n = fd(t, 10);
    const part = mod(t, 10);
    for (let k = 0; k < Math.min(SCREEN_H, n + 1); k++) {
      const [sign, text] = DIFF[k];
      const shown = k < n ? text : text.slice(0, part);
      line(k, `${sign} ${shown}`, sign === '+' ? TEXT : RED_DIM);
    }
    if (mod(fd(tick, 3), 2) && n < SCREEN_H) cv.cell(tx + 2 + Math.min(part, DIFF[n][1].length), ty + n, '_', HEAD);
  },
  lens(line, cv, tx, ty, tick) {
    line(0, '/neo/', AMBER);
    const hit = 1 + mod(fd(tick, 6), SCREEN_H - 1);
    for (let k = 1; k < SCREEN_H; k++) {
      const r = lcg(67, k, fd(tick, 60));
      let words = '';
      for (let i = 0; i < SCREEN_W; i++) words += GLYPHS[int(r() * 26)];
      if (k === hit) {
        cv.put(tx, ty + k, words.slice(0, 3), TEXT_DIM);
        cv.put(tx + 3, ty + k, ' neo ', INK, false, HILITE_BG);
        cv.put(tx + 8, ty + k, words.slice(8, SCREEN_W), TEXT_DIM);
      } else {
        line(k, words, TEXT_FAINT);
      }
    }
  },
  globe(line, cv, tx, ty, tick) {
    const pct = mod(tick * 3, 101);
    const bar = int(pct / 100 * 9);
    line(0, 'GET /construct', TEXT_DIM);
    line(2, `[${'#'.repeat(bar)}${'.'.repeat(9 - bar)}]`, TEXT);
    line(3, `${String(pct).padStart(3)}%  56k`, TEXT_DIM);
    const r = lcg(71, fd(tick, 4));
    let bits = '';
    for (let i = 0; i < SCREEN_W; i++) bits += '01'[int(r() * 2)];
    line(5, bits, TEXT_FAINT);
    for (let j = 0; j < 3; j++) {
      const px = this.cx + W - 1 - mod(tick + j * 4, 12);
      cv.cell(px, this.top + 8, j === 0 ? '<' : '-', TEXT);
    }
  },
  helpers(line, cv, tx, ty, tick) {
    const n = mod(fd(tick, 10), 4);
    for (let k = 0; k < Math.min(3, n + 1); k++) line(k, `spawn smith${k + 1}`, k < n ? TEXT_DIM : TEXT);
    line(4, `[${Math.min(3, n + 1)} agents]`, TEXT);
  },
  todo(line, cv, tx, ty, tick) {
    const done = mod(fd(tick, 12), TODO.length + 1);
    TODO.forEach((item, k) => {
      const box = k < done ? '[x]' : (k === done && mod(fd(tick, 3), 2) ? '[>]' : '[ ]');
      line(k, `${box} ${item}`, k < done ? TEXT_DIM : TEXT);
    });
  },
  wand(line, cv, tx, ty, tick) {
    line(0, 'follow the', TEXT_DIM);
    line(1, 'white rabbit', TEXT);
    const hop = mod(fd(tick, 2), 8);
    const x = tx + mod(fd(tick, 2), SCREEN_W - 5);
    const up = [1, 2, 3].includes(hop) ? 1 : 0;
    cv.put(x, ty + 3 - up, '(\\_/)', WHITE);
    cv.put(x, ty + 4 - up, '(o.o)', WHITE);
    if (!up) cv.put(x, ty + 5, '(")")', WHITE);
  },
};

const theme = makeTheme({
  meta: { id: 'matrix', name: 'Matrix', panel: PANEL },
  Mascot,
  say: (topic) => word(TOPIC_LINES, topic),
  emoteColours: EMOTE_COLOURS,
});

export const { meta, lines, spans, render, bubble, say, emoteColours } = theme;
