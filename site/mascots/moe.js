// The moe theme's mascot: a chibi cat-eared girl drawn in ASCII, animated, on
// a dark plum panel. A faithful port of Atrium's moe_mascot.py: the same
// (state, tick, tool, mood, scene, cols, rows) draws the same frame, cell for
// cell and colour for colour (golden tests: web/test/mascots-moe.test.mjs).
//
// The picture is three layers on a cell grid, painted back to front:
//   ambient  stars (or a sun and clouds by day), a crescent moon, sakura petals
//            drifting down, sparkles, grass along the bottom row; per state a
//            heart burst (done), a raincloud (error), a heart rising (idle).
//   mascot   21 x 11 cells, centred, feet on the row above the grass; opaque
//            between the edges of each row so petals pass behind her. Face and
//            pose per state and (idle) per mood; she blinks every few seconds.
//   props    a terminal for Bash, a book for Read, a notepad for Edit/Write, a
//            magnifier for searches, a globe for WebFetch, helper cats for
//            Task/Agent, a checklist for TodoWrite, a wand otherwise; a thought
//            bubble when thinking, a streaming scroll when writing.
// Moods change her idle face and pose (accents only while working); scenes add
// weather, music, party hats, papel picado, an alarm clock (mood.js SCENES).
// Bubbles are bilingual (Japanese + English), like the desktop's.

import { Canvas, STATES, lcg, propKind, fd, mod, fmod, int, streamLines, pyfmt, makeTheme } from './common.js';
import { MOODS, word } from './mood.js';

export const PANEL = '#2b1d2e';

// ---------- palette (moe night) ----------

const OUTLINE = '#f6dce8';
const HAIR = '#e7b8d8';
const BLUSH = '#ff7a9c';
const EYE = '#d8a6f0';
const EYE_COOL = '#9fc4f5';
const PIN = '#f78fb3';
const PETAL = ['#f78fb3', '#ff9db5', '#f4a3c4'];
const SPARKLE = '#ffd98a';
const STAR_DIM = '#8a6a8f';
const HEART = '#ff7a9c';
const PROP = '#8fe3e0';
const PROP2 = '#9be6b4';
const RAIN = '#9fc4f5';
const CLOUD = '#b9a7c9';
const GRASS = '#9be6b4';
const GRASS_DIM = '#5f8f78';
const MOON = '#f3d27a';
const INK = '#d8a6f0';
const SUN = '#ffd98a';
const SNOW = '#f3ecf7';
const FOG = '#7d6a85';
const BAD = '#ff6470';
const CONFETTI = ['#ff7a9c', '#8fe3e0', '#ffd98a', '#9be6b4', '#9fc4f5', '#d8a6f0'];

export const EMOTE_COLOURS = { accent: EYE, warn: SPARKLE, bad: BAD, good: GRASS, love: HEART, dim: CLOUD };

// ---------- the mascot art ----------
// 21 columns, mirror-symmetric about column 10. Rows 0-6 the head, 7-10 the body.

const W = 21;
const H = 11;

const HEAD = [
  '   /\\           /\\   ',
  '  /  \\.-------./  \\  ',
  ' |   .\'~~~~~~~`.   | ',
  ' |  /           \\  | ',
  ' |  |           |  | ',
  '  \\  \\_________/  /  ',
  '   `-._ |>o<| _.-\'   ',
];
const EARS_DROOP = [
  '                     ',
  ' ,__ \\.-------./ __, ',
];

const BODY = {
  neutral: ['      /|  :  |\\      ', '     o |_____| o     ', '      /_______\\      ', '        |   |        '],
  wave: ['      /|  :  |\\_o    ', '     o |_____|       ', '      /_______\\      ', '        |   |        '],
  chin: ['      /|  :  |)      ', '     o |_____|       ', '      /_______\\      ', '        |   |        '],
  hold: ['      /|  :  |\\      ', '     o |_____|--o    ', '      /_______\\      ', '        |   |        '],
  type: ['      /|  :  |\\      ', '      o|_____|o      ', '      /_______\\      ', '        |   |        '],
  cheer: ['    o_/|  ', '       |_____|       ', '      /_______\\      ', '        |   |        '],
  cheer2: ['    \\o/|  ', '       |_____|       ', '      /_______\\      ', '        |   |        '],
  slump: ['       |  :  |       ', '      o|_____|o      ', '      /_______\\      ', '        |   |        '],
  air: ['    o_/|  ', '       |_____|       ', '      /_______\\      ', '       /     \\       '],
};
// "cheer", "cheer2" and "air" give only the left half of row 7: mirrored here.
{
  const swap = { '/': '\\', '\\': '/', '(': ')', ')': '(', '`': "'", "'": '`' };
  const mirror = (s) => Array.from(s).reverse().map((c) => swap[c] || c).join('');
  for (const key of ['cheer', 'cheer2', 'air']) BODY[key][0] = `${BODY[key][0]}:${mirror(BODY[key][0])}`;
}

const TAIL = [['  (', '  `-'], [' ( ', '  `-'], ['(  ', ' `--'], [' ( ', '  `-']];

const FACE = {
  idle: ['O', 'w', '"'],
  thinking: ['o', '~', '"'],
  tool: ['O', 'o', '"'],
  writing: ['O', 'o', '"'],
  done: ['^', 'w', '"'],
  error: ['T', 'n', ''],
};
const MOOD_FACE = {
  happy: ['^', '^', 'w', '"'],
  excited: ['*', '*', 'D', '"'],
  shy: ['>', '<', '~', '/'],
  curious: ['o', 'O', 'o', ''],
  focused: ['o', 'o', '-', ''],
  worried: ['o', 'o', '~', ''],
  sad: ['.', '.', 'n', ''],
  sleepy: ['-', '-', '.', '"'],
};
const BROWS = { focused: ['\\', '/'], worried: ['/', '\\'], sad: ['/', '\\'] };
const FIDGETS = {
  calm: [['wave', 2], ['look', 2], ['stretch', 1], ['petal', 1], ['rest', 2]],
  happy: [['wave', 3], ['petal', 2], ['look', 1]],
  excited: [['wave', 2], ['petal', 2], ['stretch', 1]],
  shy: [['look', 2], ['rest', 2]],
  curious: [['look', 4], ['petal', 1]],
  focused: [['rest', 3], ['look', 1]],
  worried: [['look', 2], ['rest', 2]],
  sad: [['rest', 1]],
  sleepy: [['stretch', 3], ['rest', 2]],
};

// ---------- what she says ----------

export const BUBBLES = {
  idle: 'なにする？ ready when you are (=^･ω･^=)',
  thinking: 'うーん… let me think (｡•ˇ‸ˇ•｡)',
  writing: 'かきかき… writing it down ✎',
  done: 'できた！ all done ♡ (ﾉ◕ヮ◕)ﾉ*:･ﾟ✧',
  error: 'ごめんね… something went wrong (╥﹏╥)',
};
export const TOOL_BUBBLES = {
  Bash: 'コマンド実行中！ running a command',
  Read: 'よみよみ… reading (・ω・)',
  Edit: 'なおしてる！ editing ✎',
  MultiEdit: 'なおしてる！ editing ✎',
  Write: 'かいてる！ writing a file ✎',
  NotebookEdit: 'ノートを編集中 editing a notebook',
  Grep: 'さがしてる… searching (・・ )?',
  Glob: 'ファイルさがし… looking for files',
  WebSearch: 'ネットでさがす！ searching the web',
  WebFetch: 'ダウンロード中… fetching a page',
  Task: 'おてつだい呼ぶね！ calling helpers',
  Agent: 'おてつだい呼ぶね！ calling helpers',
  TodoWrite: 'やることリスト！ updating the list ☑',
};
export const MOOD_BUBBLES = {
  happy: 'ごきげん♪ ready when you are (=^･ω･^=)',
  excited: 'わくわく！ what are we doing? ✧',
  shy: '…な、なに？ (〃ω〃)',
  curious: 'なになに？ what\'s next? (・・ )?',
  focused: 'しゅうちゅう… ready ( •̀ω•́ )',
  worried: 'だいじょうぶかな… (・_・;)',
  sad: 'しょんぼり… (´・ω・`)',
  sleepy: 'ねむい… sleepy (´-ω-)zz',
};
export const TOPIC_LINES = {
  thanks: 'えへへ…どういたしまして ♡ (〃▽〃)',
  worried: 'だいじょうぶ…？ uh-oh (・_・;)',
  frustrated: 'うぅ…もう一回！ one more try (｀ε´)',
  relieved: 'ほっ…なおった！ phew, fixed (´▽`)',
  determined: 'ながいね…がんばる！ hang in there ٩(ˊᗜˋ)و',
  proud: 'やったー！ big job done ✧',
  sad: 'ごめんね… sorry (´;ω;`)',
  happy: 'うれしい！ yay ♪',
  curious: 'ふむふむ… hmm (・・ )?',
  rain: '雨だね… take an umbrella ☂',
  storm: 'かみなり！ stay inside ⚡',
  snow: 'ゆきだ！ it\'s snowing ❄',
  fog: 'きりがすごい… foggy out',
  sun: 'いい天気！ sunny today ☀',
  hot: 'あつい… {0} outside',
  cold: 'さむい… {0} brr',
  gpu_hot: 'GPUあちち… {0}°C ♨',
  cpu_hot: 'CPUあちち… {0}°C ♨',
  music: 'いい曲〜 ♪',
  game: 'ゲームタイム！ game on',
  event: 'あと{1}分！ {0} in {1} min',
  event_now: 'もうすぐ！ {0} is starting',
  birthday: '{0}のおたんじょうび！ happy birthday 🎂',
  holiday: 'きょうは{0}！ 🎉',
  go_home: '{0}… おつかれさま！ work\'s over, go home',
  bedtime: '{0}… もう寝よう？ go to bed (´-ω-)zz',
  morning: 'おはよう！ good morning ☀',
  lunch: 'おひるごはん？ lunch time',
  weekend: 'しゅうまつだ〜 weekend ♪',
  friday: '金曜日だ〜！ almost the weekend ♪',
  hello: ['こんにちは！ what shall we do? ✧', 'やっほー！ hi hi ♡', 'おはなししよ？ let\'s make something ♪'],
  welcome_back: 'おかえりなさい！ welcome back ♡',
  wake: 'はっ！ねてないよ！ I\'m up! (⊙_⊙)',
  long_prompt: ['わぁ、ながい！ a long one… (°o°)', 'よむね…！ reading it all ( •̀ω•́ )'],
  code: 'コードだ！ let me see ( •̀ω•́ )',
  image: 'え、写真？ ooh, a picture ✧',
  aborted: 'ストップ！ stopping (・_・)ゞ',
  tests_pass: ['テスト合格！ all green ✧', 'ぜんぶパス！ tests pass ♪', 'みどり〜！ green green (ﾉ◕ヮ◕)ﾉ'],
  tests_fail: ['テスト失敗… tests failed (´;ω;`)', 'あかい… red tests (・_・;)'],
  commit: ['コミット！ shipped ✧', 'おくったよ！ pushed ♡', 'できあがり！ it\'s out (ﾉ◕ヮ◕)ﾉ'],
  fast: ['はやっ！ quick one ✧', 'いっしゅん！ done in a blink', 'らくしょう♪ easy peasy'],
  busy: ['いそがしい〜 busy busy (>_<)', 'まだまだ！ so many tools ٩(ˊᗜˋ)و'],
  model: 'あたらしい子？ hello, {0} ✧',
  poke: ['ひゃっ！ hey! (〃ω〃)', 'なぁに？ yes? (・・ )?', 'つんつんしないで〜 (>ω<)',
    'にゃっ！ nya! ♪', 'えへへ、くすぐったい (〃▽〃)', 'あそぶ？ wanna play? ✧'],
  poke_spam: ['もう！やめて〜！ stop poking! (｀ε´)', 'つつきすぎ！ too many pokes! (>_<)',
    'むぅ…おこるよ！ I\'ll get mad! (｀へ´)'],
  pet: ['ふにゃ〜 ♡ (´ω`)', 'なでなで…うれしい ♡', 'もっと〜 more pats please ♡',
    'ごろごろ… purr ♡ (=^･ω･^=)'],
  rg_memory: 'めっ！ {0} がメモリ食べすぎ…止めたよ (｀・ω・´)',
  rg_vram: '{0} がGPUいっぱい…止めたよ！ (・_・;)',
  rg_game_vram: '{0} のGPUメモリ、ぱんぱん… textures low? (・・;)',
  rg_panic: 'はいっ！ {0} ばいばい ✧',
  rg_stuck: 'メモリが…でも誰を止めれば…？ (；ω；)',
  rg_agents: 'エージェントのお仕事、ぜんぶ止めたよ ♡ sessions safe',
};

const STREAM_WORDS = ['moe', 'nya', 'ok', 'so', 'code', 'fix', 'yay', 'the', 'sakura',
  'it', 'now', 'neko', 'kawaii', 'and', 'done', 'a', 'test', 'uwu'];

const SWAY = [0, 1, 1, 0, 0, -1, -1, 0];
const has = (set, ...tags) => tags.some((t) => set.has(t));

export class Mascot {
  constructor(cols = 64, rows = 18) {
    this.cols = Math.max(Math.trunc(cols), W + 2);
    this.rows = Math.max(Math.trunc(rows), H + 2);
    this.x0 = fd(this.cols - W, 2);
    this.top = this.rows - 1 - H;
    const area = this.cols * this.rows;
    this.nPetals = Math.max(4, fd(area, 85));
    this.nStars = Math.max(3, fd(area, 110));
    this.nSparkles = Math.max(2, fd(area, 260));
    this.stream = streamLines(STREAM_WORDS, 9, 31, 400);
    this.stars = [];
    for (let i = 0; i < this.nStars; i++) {
      const r = lcg(7, i, this.cols, this.rows);
      const x = int(r() * this.cols);
      const y = int(r() * Math.max(2, fd(this.rows, 2)));
      const period = 24 + int(r() * 30);
      this.stars.push([x, y, period, int(r() * period)]);
    }
    this.grassV = Array.from({ length: this.cols }, (_, x) => lcg(23, x)());
    this.head = null;
  }

  bubble(state, tool = null, mood = null) {
    if (state === 'idle' && Object.hasOwn(MOOD_BUBBLES, mood || '')) return MOOD_BUBBLES[mood];
    if (state === 'tool') {
      if (tool && tool.startsWith('mcp__')) return pyfmt('まほう発動！ %s ✧', tool.split('__').at(-1));
      return Object.hasOwn(TOOL_BUBBLES, tool || '') ? TOOL_BUBBLES[tool || '']
        : pyfmt('まほう発動！ %s ✧', tool || 'magic');
    }
    return BUBBLES[state] ?? BUBBLES.idle;
  }

  // ---------- composition ----------

  render(state, tick, tool = null, mood = null, scene = new Set()) {
    if (!STATES.includes(state)) state = 'idle';
    if (!MOODS.includes(mood)) mood = 'calm';
    tick = Math.trunc(tick);
    const cv = new Canvas(this.cols, this.rows);
    const kind = state === 'tool' ? propKind(tool) : null;

    let dy = 0;
    if (state === 'done') {
      const hop = [0, 0, 1, 2, 2, 2, 1, 0, 0, 0, 0, 0];
      dy = hop[mod(tick, hop.length)];
    } else if (state === 'idle' && mood === 'excited' && mod(fd(tick, 30), 2)) {
      dy = [0, 1, 1, 0, 0, 0][mod(tick, 6)];
    }
    dy = Math.min(dy, this.top);
    const top = this.top - dy;

    this.sky(cv, state, tick, scene);
    if (scene.has('fog')) this.fog(cv, tick);
    if (scene.has('snow')) this.snow(cv, tick);
    this.petals(cv, state, tick, scene);
    if (scene.has('festive')) this.banner(cv, tick);
    this.sparkles(cv, state, tick);
    this.grass(cv, tick);
    if (scene.has('hot')) this.shimmer(cv, tick);
    if (state === 'idle' && ['calm', 'happy', 'shy', 'excited'].includes(mood)) {
      this.risingHeart(cv, tick, mood === 'shy' ? 35 : 70);
    }
    if (kind === 'helpers') this.helpers(cv, tick);
    else if (scene.has('clock')) this.alarm(cv, tick);

    const hx = this.mascot(cv, state, tick, top, kind, dy, mood, scene);
    const rainy = has(scene, 'rain', 'storm');
    this.head = [this.x0 + hx + (rainy ? 19 : 18), Math.max(0, top - 1)];
    this.moodMarks(cv, state, tick, top, hx, mood, scene);
    if (scene.has('party')) this.hat(cv, tick, top, hx);
    if (rainy) {
      const dry = this.parasol(cv, top - (scene.has('party') ? 2 : 0), hx);
      if (state !== 'error') this.weatherRain(cv, tick, dry, scene.has('storm'));
    }

    if (state === 'thinking') this.thought(cv, tick, top);
    else if (state === 'writing') this.scroll(cv, tick, top);
    else if (kind && kind !== 'helpers') this.prop(cv, kind, tick, top);
    if (state === 'done') this.burst(cv, tick, top);
    if (state === 'error') {
      this.rain(cv, tick);
      this.cloud(cv, tick);
    }
    return cv;
  }

  // ---------- ambient ----------

  sky(cv, state, tick, scene) {
    const overcast = has(scene, 'cloud', 'rain', 'storm', 'snow', 'fog');
    if (overcast) this.clouds(cv, tick, !scene.has('day'));
    if (scene.has('storm')) this.lightning(cv, tick);
    if (scene.has('day')) {
      if (!overcast && state !== 'error' && this.cols >= 50) this.sun(cv, tick, scene.has('festive') ? 2 : 0);
      return;
    }
    const moon = this.cols >= 60 && this.rows >= 16 && state !== 'error' && !overcast;
    if (moon) {
      cv.put(2, 0, '.-', MOON);
      cv.put(1, 1, '(', MOON);
      cv.put(2, 2, '`-', MOON);
    }
    for (const [x, y, period, ph] of this.stars) {
      if (moon && x < 5 && y < 3) continue;
      if (overcast && mod(x + y, 3)) continue;
      const t = mod(tick + ph, period);
      const seq = '.+*+.';
      if (t < seq.length) cv.cell(x, y, seq[t], SPARKLE);
      else cv.cell(x, y, '.', STAR_DIM);
    }
  }

  petals(cv, state, tick, scene) {
    const span = this.rows + 4;
    let n = state !== 'error' ? this.nPetals : fd(this.nPetals, 2);
    if (scene.has('snow')) n = 0;
    else if (has(scene, 'rain', 'storm')) n = fd(n, 3);
    const party = scene.has('party');
    const glyphs = ['*', '~', ',', "'", '`', '~'];
    const width = this.cols + this.rows;
    for (let i = 0; i < n; i++) {
      const r = lcg(11, i);
      const speed = 0.10 + r() * 0.12;
      const drift = 0.6 + r() * 0.8;
      const ph = r() * span;
      const colour = party && i % 2 ? CONFETTI[i % CONFETTI.length] : PETAL[i % PETAL.length];
      const pos = ph + tick * speed;
      const fall = int(fd(pos, span));
      const yy = pos - fall * span - 2;
      const rr = lcg(13, i, fall);
      const x0 = rr() * width;
      const sway = SWAY[mod(int(pos * 2), 8)] * 0.5;
      const x = mod(int(x0 + (yy + 2) * drift + sway), width) - fd(this.rows, 2);
      const y = int(yy);
      const g = glyphs[mod(int(pos * 3) + i, glyphs.length)];
      cv.cell(x, y, g, colour);
    }
  }

  sparkles(cv, state, tick) {
    let n = this.nSparkles + (state === 'done' ? 2 : 0);
    if (state === 'error') n = Math.max(1, fd(n, 2));
    const seq = '.+*+.';
    for (let j = 0; j < n; j++) {
      const r = lcg(17, j);
      const period = 18 + int(r() * 22);
      const ph = int(r() * period);
      const t = tick + ph;
      const cyc = fd(t, period);
      const k = mod(t, period);
      if (k >= seq.length) continue;
      const rr = lcg(19, j, cyc);
      const x = int(rr() * this.cols);
      const y = int(rr() * (this.rows - 1));
      if (cv.get(x, y) === ' ') cv.cell(x, y, seq[k], SPARKLE);
    }
  }

  grass(cv, tick) {
    const y = this.rows - 1;
    const wind = mod(fd(tick, 6), 4);
    this.grassV.forEach((v, x) => {
      if (v < 0.22) {
        const c = v < 0.11 ? ",'"[mod(x + wind, 2)] : '.';
        cv.cell(x, y, c, GRASS_DIM);
      } else if (v < 0.30) {
        cv.cell(x, y, mod(x + wind, 3) ? '"' : "'", GRASS);
      } else if (v < 0.335) {
        cv.cell(x, y, '*', PETAL[x % 3]);
      }
    });
    cv.put(this.x0 + 7, y, '._____.', GRASS_DIM, true);
  }

  risingHeart(cv, tick, period = 70) {
    const t = mod(tick, period);
    if (t >= fd(period * 4, 7)) return;
    const cyc = fd(tick, period);
    const side = mod(cyc, 2) ? -1 : 1;
    const x = this.x0 + 10 + side * (13 + mod(fd(t, 8), 2));
    const y = this.top + 4 - fd(t, 5);
    cv.put(x, y, '<3', HEART);
  }

  // ---------- the scene ----------

  sun(cv, tick, y = 0) {
    const rays = ['\\ | /', '  |  ', '\\   /'][mod(fd(tick, 8), 3)];
    cv.put(1, y, rays, SUN);
    cv.put(1, y + 1, '-(', SUN);
    cv.put(3, y + 1, 'O', SUN);
    cv.put(4, y + 1, ')-', SUN);
    cv.put(1, y + 2, rays[0] === '\\' ? '/ | \\' : '  |  ', SUN);
  }

  clouds(cv, tick, dim = false) {
    const colour = dim ? STAR_DIM : CLOUD;
    const span = this.cols + 16;
    const n = Math.max(2, fd(this.cols, 28));
    for (let i = 0; i < n; i++) {
      const r = lcg(47, i);
      const y = this.rows >= 16 ? int(r() * 3) : 0;
      const x = mod(int(r() * span + tick / (9 + i * 3)), span) - 14;
      cv.put(x, y, '  .--.     ', colour);
      cv.put(x, y + 1, ' (    ).-. ', colour);
      cv.put(x, y + 2, '(_________)', colour);
    }
  }

  lightning(cv, tick) {
    const cyc = fd(tick, 47);
    const t = mod(tick, 47);
    if (t > 3) return;
    const x = int(lcg(53, cyc)() * (this.cols - 4)) + 1;
    [[1, '/'], [0, '/'], [0, '\\'], [1, '/']].forEach(([dx, ch], k) => {
      cv.cell(x + dx + (k > 1 ? 1 : 0), 1 + k, ch, t % 2 === 0 ? SUN : OUTLINE);
    });
  }

  fog(cv, tick) {
    [fd(this.rows, 3), fd(this.rows, 2) + 1, this.rows - 3].forEach((y, k) => {
      const off = mod(fd(tick, 6 + k * 2) + k * 7, 12);
      for (let x = 0; x < this.cols; x++) {
        const p = mod(x + off, 12);
        if (p < 5 && p !== 2) cv.cell(x, y, '~', FOG);
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
      const x = int(lcg(61, i, fall)() * this.cols + SWAY[mod(fd(tick, 5) + i, 8)]);
      cv.cell(x, y, '*.+'[i % 3], SNOW);
    }
  }

  banner(cv, tick) {
    for (let x = 0; x < this.cols; x++) {
      cv.cell(x, 0, '-', STAR_DIM);
      if (x % 3 === 1) {
        const flag = 'Vv'[mod(fd(tick, 4) + fd(x, 3), 2)];
        cv.cell(x, 1, flag, CONFETTI[fd(x, 3) % CONFETTI.length]);
      }
    }
  }

  shimmer(cv, tick) {
    const y = this.rows - 2;
    for (let x = 0; x < this.cols; x++) {
      if (lcg(67, x, fd(tick, 3))() < 0.08 && cv.get(x, y) === ' ') cv.cell(x, y - (x % 2), '~', SPARKLE);
    }
  }

  alarm(cv, tick) {
    const x = this.x0 - 9;
    const y = this.rows - 4;
    const ring = mod(fd(tick, 2), 10) < 5;
    const shake = ring ? mod(tick, 2) : 0;
    cv.sprite(x + shake, y, 'o.-.o', PROP);
    cv.sprite(x + shake, y + 1, '(   )', PROP);
    cv.cell(x + shake + 2, y + 1, '|/-\\'[mod(fd(tick, 3), 4)], SPARKLE);
    cv.sprite(x + shake, y + 2, "'-^-'", PROP);
    if (ring) {
      cv.cell(x + 6, y - 1, '!', SPARKLE);
      cv.cell(x - 2, y, '(', SPARKLE);
      cv.cell(x + 7, y, ')', SPARKLE);
    }
  }

  hat(cv, tick, top, hx) {
    const fx = this.x0 + hx;
    cv.cell(fx + 10, top - 2, '*+'[mod(fd(tick, 4), 2)], SPARKLE);
    cv.sprite(fx + 9, top - 1, '/:\\', PIN);
    cv.sprite(fx + 8, top, '/_:_\\', PROP);
    cv.cell(fx + 10, top - 1, ':', SPARKLE);
  }

  parasol(cv, top, hx) {
    const fx = this.x0 + hx;
    const y = top - 2;
    cv.sprite(fx, y, '   .-~~~~~~~~~~~-.   ', PIN);
    cv.sprite(fx, y + 1, '  /_/_/_/_|_\\_\\_\\_\\  ', PIN);
    cv.cell(fx + 10, y + 1, '|', PROP);
    return [fx + 2, fx + 18, y + 2];
  }

  weatherRain(cv, tick, dry, heavy = false) {
    const [xa, xb, yDry] = dry;
    const span = this.rows + 2;
    const n = fd(this.cols * this.rows, heavy ? 14 : 24);
    for (let i = 0; i < n; i++) {
      const r = lcg(71, i);
      const speed = (heavy ? 0.7 : 0.5) + r() * 0.3;
      const y = int(fmod(r() * span + tick * speed, span)) - 1;
      const x = int(r() * this.cols);
      if (y >= yDry && xa <= x && x <= xb) continue;
      if (x >= 0 && x < cv.cols && y >= 0 && y < cv.rows && !cv.occ[y][x] && cv.get(x, y) === ' ') {
        cv.cell(x, y, i % 3 ? "'" : ',', RAIN);
      }
    }
  }

  rain(cv, tick) {
    const cx = this.x0 + 4 + this.cloudDx(tick);
    const top = Math.max(0, this.top - 3) + 2;
    const length = Math.max(3, this.rows - 1 - top);
    for (let i = 0; i < 30; i++) {
      const r = lcg(29, i);
      const speed = 0.6 + r() * 0.3;
      const ph = r() * length;
      const y = top + int(fmod(ph + tick * speed, length));
      const x = cx + (i % 15);
      if (x >= 0 && x < cv.cols && y >= 0 && y < cv.rows && !cv.occ[y][x]) {
        cv.cell(x, y, i % 3 ? "'" : ',', RAIN);
      }
    }
  }

  cloudDx(tick) {
    return SWAY[mod(fd(tick, 6), 8)];
  }

  cloud(cv, tick) {
    const x = this.x0 + 3 + this.cloudDx(tick);
    const y = Math.max(0, this.top - 3);
    cv.sprite(x, y, '   .--.  .-.     ', CLOUD);
    cv.sprite(x, y + 1, " (`    )'   `-.  ", CLOUD);
    cv.sprite(x, y + 2, '(_______________)', CLOUD);
    if (mod(fd(tick, 3), 9) === 0) cv.put(x + 7, y + 2, '/', SPARKLE);
  }

  burst(cv, tick, top) {
    const t = mod(tick, 22);
    const cx = this.x0 + 10;
    const cy = top + 4;
    const dirs = [[-1, 0], [1, 0], [-1, -1], [1, -1], [0, -1], [-1, 1], [1, 1]];
    if (t < 16) {
      const rad = 2 + t;
      dirs.forEach(([ux, uy], k) => {
        const x = cx + ux * (6 + rad * 2) - (ux === 0 ? 1 : 0);
        const y = cy + uy * (1 + fd(rad, 2)) - (uy < 0 && ux === 0 ? 2 : 0);
        const glyph = (t + k) % 4 ? '<3' : '**';
        cv.put(x, y, glyph, glyph === '<3' ? HEART : SPARKLE);
      });
    }
  }

  helpers(cv, tick) {
    const y = this.rows - 4;
    [this.x0 - 9, this.x0 + W + 1].forEach((x, n) => {
      const step = mod(fd(tick, 3) + n * 2, 4);
      const hop = step === 1 || step === 2 ? 1 : 0;
      const ear = '/\\_/\\';
      const face = mod(tick + n * 17, 41) > 2
        ? ['(=^.^=)', '(=o.o=)', '(=^.^=)', '(=-.-=)'][mod(fd(tick, 6) + n, 4)] : '(=-.-=)';
      const feet = [' (")(") ', ' (") (")', ' (")(") ', '(") (") '][step];
      cv.sprite(x + 1, y - hop, ear, OUTLINE);
      cv.sprite(x, y + 1 - hop, face, OUTLINE);
      const fx = face.indexOf('^');
      if (fx >= 0) {
        cv.put(x + fx, y + 1 - hop, '^', EYE);
        cv.put(x + fx + 2, y + 1 - hop, '^', EYE);
      }
      cv.put(x + 1, y + 1 - hop, '=', BLUSH);
      cv.put(x + 5, y + 1 - hop, '=', BLUSH);
      cv.put(x, y + 2 - hop, feet, PROP2);
      if (mod(fd(tick, 4) + n, 6) === 0) cv.put(x + 7, y - 1 - hop, '!', SPARKLE);
    });
  }

  // ---------- her ----------

  blink(tick) {
    const t = mod(tick, 53);
    if (t === 0 || t === 1) return true;
    const u = mod(tick, 159);
    return u === 5 || u === 6;
  }

  fidget(tick, mood) {
    if (mod(tick, 90) < 72) return null;
    const table = FIDGETS[mood] || FIDGETS.calm;
    let pick = lcg(43, fd(tick, 90))() * table.reduce((s, [, w]) => s + w, 0);
    for (const [name, weight] of table) {
      pick -= weight;
      if (pick < 0) return name === 'rest' ? null : name;
    }
    return null;
  }

  // mascot draws her and returns the head's column offset (the tilt).
  mascot(cv, state, tick, top, kind, dy, mood = 'calm', scene = new Set()) {
    const x0 = this.x0;
    const idle = state === 'idle';
    const fidget = idle ? this.fidget(tick, mood) : null;
    const game = idle && scene.has('game');

    let pose;
    if (idle) {
      pose = 'neutral';
      if (game || mood === 'shy' || mood === 'worried') pose = 'type';
      else if (mood === 'sad' || mood === 'sleepy') pose = 'slump';
      else if (mood === 'curious') pose = 'chin';
      else if (mood === 'excited' && dy) pose = 'cheer2';
      if (fidget === 'wave' && !game) pose = mod(fd(tick, 3), 2) ? 'wave' : pose;
      else if (fidget === 'petal' && !game) pose = 'wave';
      else if (fidget === 'stretch' && !game) pose = mod(fd(tick, 6), 2) ? 'cheer2' : 'cheer';
    } else if (state === 'thinking') {
      pose = 'chin';
    } else if (state === 'tool') {
      pose = kind === 'helpers' ? 'neutral' : 'hold';
    } else if (state === 'writing') {
      pose = 'hold';
    } else if (state === 'done') {
      pose = dy >= 2 ? 'air' : (mod(fd(tick, 4), 2) ? 'cheer2' : 'cheer');
    } else {
      pose = 'slump';
    }

    let hx = 0;
    if (idle) {
      if (scene.has('music') && mood !== 'sad' && mood !== 'sleepy') hx = [0, 1][mod(fd(tick, 4), 2)];
      else if (mood === 'curious') hx = 1;
      else if (!['sad', 'sleepy', 'focused'].includes(mood)) {
        hx = [0, 0, 1, 1, 1, 0, 0, -1, -1, -1][mod(fd(tick, 5), 10)];
      }
    } else if (state === 'thinking') {
      hx = 1;
    }

    const droop = state === 'error' || (mood === 'sad' && state !== 'done');
    const head = HEAD.slice();
    if (droop) {
      head[0] = EARS_DROOP[0];
      head[1] = EARS_DROOP[1];
    } else if ((state === 'tool' || state === 'writing') && mod(fd(tick, 12), 2)) {
      head[0] = '   /\\           |\\   ';
    }

    BODY[pose].forEach((r, i) => cv.sprite(x0, top + 7 + i, r, OUTLINE));
    if ((state === 'tool' || state === 'writing') && mod(fd(tick, 5), 2)) {
      cv.put(x0 + 8, top + 10, '|   /', OUTLINE);
    }
    let tail = TAIL[mod(fd(tick, 4), TAIL.length)];
    if (droop) tail = ['   ', ' __-'];
    cv.put(x0 + 1, top + 8, tail[0], HAIR);
    cv.put(x0 + 1, top + 9, tail[1], HAIR);
    if (game) {
      cv.put(x0 + 7, top + 8, '[', PROP);
      cv.put(x0 + 8, top + 8, '+', SPARKLE);
      cv.put(x0 + 9, top + 8, mod(fd(tick, 4), 2) ? '#=#' : '=#=', PROP2);
      cv.put(x0 + 12, top + 8, 'o', BLUSH);
      cv.put(x0 + 13, top + 8, ']', PROP);
    }
    if (fidget === 'petal' && !game) cv.cell(x0 + 17, top + 6, '*', PETAL[1]);

    head.forEach((r, i) => cv.sprite(x0 + hx, top + i, r, OUTLINE));
    for (let c = 7; c < 14; c++) cv.cell(x0 + hx + c, top + 2, '~', HAIR);
    for (const [row, cols] of [[0, [3, 4, 16, 17]], [6, [3, 4, 5, 6, 14, 15, 16, 17]]]) {
      if (droop && row === 0) continue;
      for (const c of cols) {
        const ch = head[row][c];
        if (ch !== ' ') cv.cell(x0 + hx + c, top + row, ch, HAIR);
      }
    }
    if (scene.has('cold')) cv.put(x0 + hx + 8, top + 6, '=~=~=', PIN);
    else cv.put(x0 + hx + 9, top + 6, '>o<', PIN);
    if (!droop) cv.cell(x0 + hx + 15, top + 2, mod(fd(tick, 7), 6) ? '*' : '+', PIN);
    if (scene.has('music')) {
      for (const k of [3, 4]) {
        cv.cell(x0 + hx, top + k, '[', PROP);
        cv.cell(x0 + hx + 20, top + k, ']', PROP);
      }
    }

    // the face
    let [eye, mouth, blush] = FACE[state];
    let el = eye;
    let er = eye;
    let gaze = 0;
    if (state === 'thinking') gaze = mod(fd(tick, 15), 3) ? 1 : 0;
    else if (state === 'tool' || state === 'writing') gaze = 1;
    if (state === 'writing') mouth = 'o-Ow-o~-'[mod(fd(tick, 2), 8)];
    else if (idle) mouth = mod(fd(tick, 40), 3) ? 'w' : 'v';
    else if (state === 'done') mouth = 'wvD'[mod(fd(tick, 5), 3)];
    else if (state === 'tool' && ['bash', 'pad', 'todo'].includes(kind)) mouth = mod(fd(tick, 10), 2) ? '-' : 'o';
    if (idle && Object.hasOwn(MOOD_FACE, mood)) {
      [el, er, mouth, blush] = MOOD_FACE[mood];
      if (mood === 'happy') mouth = 'wv'[mod(fd(tick, 40), 2)];
      else if (mood === 'sleepy') {
        if (mod(tick, 70) < 10) mouth = 'O';
        else if (mod(fd(tick, 25), 5) === 0) {
          el = 'o';
          er = 'o';
        }
      }
    } else if (['thinking', 'tool', 'writing'].includes(state)) {
      if (mood === 'excited' && mod(fd(tick, 8), 3) === 0) {
        el = '*';
        er = '*';
      } else if (mood === 'shy') blush = '/';
      else if (mood === 'sad') blush = '';
    }
    if (fidget === 'look') gaze = [-1, 1][mod(fd(tick, 6), 2)];
    else if (fidget === 'petal') {
      el = '^';
      er = '^';
    } else if (fidget === 'stretch') {
      el = '-';
      er = '-';
      mouth = 'O';
    }
    if (game) {
      el = 'o';
      er = 'o';
    }
    const blinking = this.blink(tick) && state !== 'done' && state !== 'error' && !['-', '^', '>'].includes(el);
    let eyeCol = state === 'tool' || state === 'writing' ? EYE_COOL : EYE;
    if (el === '*') eyeCol = SPARKLE;
    if (blinking) {
      el = '-';
      er = '-';
    }
    const fx = x0 + hx;
    const shades = idle && scene.has('sun') && scene.has('day') && !game
      && ['calm', 'happy', 'excited'].includes(mood) && fidget !== 'stretch';
    if (shades) {
      cv.put(fx + 6, top + 3, '[=]---[=]', INK);
    } else {
      cv.cell(fx + 7 + gaze, top + 3, el, blinking ? OUTLINE : eyeCol);
      cv.cell(fx + 13 + gaze, top + 3, er, blinking ? OUTLINE : eyeCol);
      const glint = mod(fd(tick, 3), 11) === 0;
      if ((!blinking && idle && el !== '-' && el !== '.' && glint) || (!blinking && state === 'done' && glint)) {
        cv.cell(fx + 8 + gaze, top + 3, '+', SPARKLE);
        cv.cell(fx + 14 + gaze, top + 3, '+', SPARKLE);
      }
    }
    cv.cell(fx + 10, top + 4, mouth, BLUSH);
    if (blush === '/') {
      for (const c of [6, 7, 8, 12, 13, 14]) cv.cell(fx + c, top + 4, '/', BLUSH);
    } else if (blush) {
      cv.cell(fx + 6, top + 4, blush, BLUSH);
      cv.cell(fx + 14, top + 4, blush, BLUSH);
    }
    if (Object.hasOwn(BROWS, mood) && state !== 'done' && state !== 'error') {
      const [left, right] = BROWS[mood];
      cv.cell(fx + 7, top + 2, left, OUTLINE);
      cv.cell(fx + 13, top + 2, right, OUTLINE);
    }
    if (state === 'error') {
      const t = mod(tick, 12);
      if (t < 8) {
        cv.cell(fx + 7, top + 4, ",'"[fd(t, 4) % 2], RAIN);
        cv.cell(fx + 13, top + 4, "',"[fd(t, 4) % 2], RAIN);
      }
      cv.cell(fx + 6, top + 4, ';', RAIN);
    } else if (idle && mood === 'sad' && mod(tick, 40) < 12) {
      cv.cell(fx + 7, top + 4, ",'"[mod(fd(tick, 4), 2)], RAIN);
    }
    if ((mood === 'worried' && state !== 'done') || scene.has('hot')) {
      const t = mod(tick, 16);
      if (t < 10) cv.cell(fx + 18, top + 2 + (t >= 5 ? 1 : 0), t < 5 ? "'" : ',', RAIN);
    }
    if (state === 'thinking') cv.cell(fx + 14, top + 6, 'o', OUTLINE);
    return hx;
  }

  moodMarks(cv, state, tick, top, hx, mood, scene) {
    const fx = this.x0 + hx;
    const idle = state === 'idle';
    if (idle && mood === 'sleepy') {
      const t = mod(tick, 36);
      for (let j = 0; j < 3; j++) {
        if (fd(t, 9) > j) cv.cell(fx + 20 + j, top + 2 - j, 'zzZ'[j], j < 2 ? INK : OUTLINE);
      }
    } else if (idle && mood === 'curious' && mod(fd(tick, 5), 4)) {
      cv.cell(fx + 18, Math.max(0, top - 1), '?', SPARKLE);
    } else if (idle && mood === 'excited' && mod(fd(tick, 3), 4) < 2) {
      cv.cell(fx + 1, top, '!', SPARKLE);
      cv.cell(fx + 19, top, '!', SPARKLE);
    }
    if (scene.has('music') && state !== 'error') {
      for (let j = 0; j < 2; j++) {
        const t = mod(tick + j * 12, 24);
        const x = fx - 2 - fd(t, 6) - j * 2;
        const y = top + 5 - fd(t, 5);
        if (t < 20) cv.cell(x, y, 'db'[j], [SPARKLE, PIN][j]);
      }
    }
    if (scene.has('cold') && idle) {
      const t = mod(tick, 40);
      if (t < 12) cv.cell(fx + 21 + fd(t, 4), top + 4 - fd(t, 6), '.oO'[Math.min(2, fd(t, 4))], SNOW);
    }
  }

  // ---------- things around her ----------

  thought(cv, tick, top) {
    const x = this.x0 + W + 1;
    const y = Math.max(0, top - 1);
    cv.cell(x - 1, top + 3, '.', INK);
    cv.cell(x, top + 2, 'o', INK);
    cv.sprite(x + 1, y, ' .-~~~~~-. ', OUTLINE);
    cv.sprite(x + 1, y + 1, '(         )', OUTLINE);
    cv.sprite(x + 1, y + 2, " `-~~~~~-' ", OUTLINE);
    const phase = mod(fd(tick, 3), 12);
    if (phase < 8) {
      const n = Math.min(3, fd(phase, 2));
      const dots = n ? '.oO'.slice(0, n).split('').join(' ') : '';
      cv.put(x + 3, y + 1, dots.padEnd(5), INK);
      cv.cell(x + 9, y + 1, '-\\|/'[mod(tick, 4)], SPARKLE);
    } else {
      cv.put(x + 4, y + 1, phase < 11 ? '? !'.slice(0, phase - 8 + 1) : '?!', SPARKLE);
      cv.cell(x + 9, y + 1, '*', SPARKLE);
    }
  }

  scroll(cv, tick, top) {
    const x = this.x0 + 18;
    const y = top + 5;
    const lines = this.stream;
    const width = 9;
    const total = mod(tick, lines.length * width);
    const lineI = fd(total, width);
    const col = total % width;
    cv.sprite(x, y, '.-----------.', OUTLINE);
    for (let k = 0; k < 3; k++) {
      const li = lineI - 2 + k;
      let text;
      if (li < 0) text = ' '.repeat(width);
      else if (li < lineI) text = lines[li];
      else text = lines[li].slice(0, col).padEnd(width);
      cv.sprite(x, y + 1 + k, `|${' '.repeat(11)}|`, OUTLINE);
      cv.put(x + 2, y + 1 + k, text, k < 2 ? PROP : PROP2);
    }
    cv.sprite(x, y + 4, "'-----------'", OUTLINE);
    if (mod(fd(tick, 3), 2)) cv.cell(x + 2 + col, y + 3, '_', SPARKLE);
    else cv.cell(x + 2 + col, y + 3, '/', PIN);
  }

  prop(cv, kind, tick, top) {
    const x = this.x0 + 18;
    const y = top + 6;
    const fn = {
      bash: this.propBash, book: this.propBook, pad: this.propPad, lens: this.propLens,
      globe: this.propGlobe, todo: this.propTodo, wand: this.propWand,
    }[kind];
    fn.call(this, cv, tick, x, y);
  }

  propBash(cv, tick, x, y) {
    const cmds = ['make', 'ls', 'git', 'npm t', 'pytest', 'cargo'];
    const cyc = fd(tick, 30);
    const cmd = cmds[mod(cyc, cmds.length)];
    const n = Math.min(cmd.length, fd(mod(tick, 30), 2));
    cv.sprite(x + 1, y - 1, '.--------.', PROP);
    cv.sprite(x + 1, y, '|        |', PROP);
    cv.sprite(x + 1, y + 1, '|        |', PROP);
    cv.sprite(x + 1, y + 2, "'--------'", PROP);
    cv.put(x + 4, y + 3, '/__\\', PROP);
    const prev = cmds[mod(cyc - 1, cmds.length)];
    cv.put(x + 2, y, '$', STAR_DIM);
    cv.put(x + 4, y, prev.slice(0, 6), STAR_DIM);
    cv.put(x + 2, y + 1, '$', SPARKLE);
    cv.put(x + 4, y + 1, cmd.slice(0, n).slice(0, 5), PROP2);
    if (mod(fd(tick, 3), 2)) cv.cell(x + 4 + Math.min(n, 5), y + 1, '_', SPARKLE);
  }

  propBook(cv, tick, x, y) {
    const flip = mod(fd(tick, 4), 10);
    cv.sprite(x + 1, y - 1, ' ____ ____ ', PROP);
    const spine = flip < 8 ? '|' : (flip === 8 ? '/' : '\\');
    cv.sprite(x + 1, y, `|    ${spine}    |`, PROP);
    cv.sprite(x + 1, y + 1, '|    |    |', PROP);
    cv.sprite(x + 1, y + 2, '|____|____|', PROP);
    const line = mod(fd(tick, 6), 4);
    const texts = [['~~~ ', '~~ ~'], ['~ ~~', '~~~ '], ['~~~ ', '~ ~~'], ['~~ ~', '~~~ ']];
    for (let k = 0; k < 2; k++) {
      const [a, b] = texts[mod(k + fd(tick, 40), 4)];
      cv.put(x + 2, y + k, a, PROP2);
      cv.put(x + 7, y + k, b, PROP2);
    }
    const rx = x + 2 + (line % 2) * 5;
    cv.put(rx, y + fd(line, 2), '~~', SPARKLE);
  }

  propPad(cv, tick, x, y) {
    cv.sprite(x + 1, y - 1, '.--------.', PROP);
    for (let k = 0; k < 3; k++) cv.sprite(x + 1, y + k, '|        |', PROP);
    cv.sprite(x + 1, y + 3, "'--------'", PROP);
    const total = mod(tick, 54);
    const row = fd(total, 18);
    const col = fd(total % 18, 3);
    const scr = ['~-~~-~', '~~-~-~', '-~~~-~'];
    for (let k = 0; k < 3; k++) {
      if (k < row) cv.put(x + 2, y + k, scr[k], PROP2);
      else if (k === row) cv.put(x + 2, y + k, scr[k].slice(0, col), PROP2);
    }
    const px = x + 2 + col;
    const py = y + row;
    const bob = mod(fd(tick, 2), 2);
    cv.put(px, py - 1 - bob, '/', SPARKLE);
    cv.put(px + 1, py - 2 - bob, '/', PIN);
  }

  propLens(cv, tick, x, y) {
    const data = 'a1 f~ :) b3 c7 *k';
    const span = 7;
    const k = mod(fd(tick, 2), span * 2);
    const off = k < span ? k : span * 2 - k;
    cv.put(x + 1, y + 3, data.slice(0, 12), PROP2);
    const lx = x + off;
    cv.sprite(lx + 1, y, ' .-. ', PROP);
    cv.sprite(lx + 1, y + 1, '(   )', PROP);
    cv.sprite(lx + 1, y + 2, " `-'\\", PROP);
    cv.put(lx + 6, y + 3, '\\', PROP);
    if ((off === 3 || off === 4) && mod(fd(tick, 2), 2)) cv.cell(lx + 3, y + 1, '!', SPARKLE);
    else cv.cell(lx + 3, y + 1, 'o', EYE_COOL);
  }

  propGlobe(cv, tick, x, y) {
    const land = ['  __/~\\_   ~~  ', ' /   _ \\__  /\\_', '  \\_/ \\__)  __ ', '    ~~  \\_/    '];
    const off = mod(fd(tick, 3), land[0].length);
    cv.sprite(x + 2, y - 1, ' .-""-. ', PROP);
    cv.sprite(x + 2, y, '/      \\', PROP);
    cv.sprite(x + 2, y + 1, '|      |', PROP);
    cv.sprite(x + 2, y + 2, '\\      /', PROP);
    cv.sprite(x + 2, y + 3, " `-..-' ", PROP);
    for (let k = 0; k < 3; k++) {
      const row = land[k] + land[k];
      cv.put(x + 3, y + k, row.slice(off, off + 6), PROP2);
    }
    for (let j = 0; j < 3; j++) {
      const px = x + 1 - fd(mod(tick + j * 3, 9), 3);
      if (mod(tick + j, 3) === 0) continue;
      cv.cell(px, y + 4, j === 0 ? '<' : '.', SPARKLE);
    }
  }

  propTodo(cv, tick, x, y) {
    cv.sprite(x + 1, y - 1, '.--------.', PROP);
    for (let k = 0; k < 3; k++) cv.sprite(x + 1, y + k, '|        |', PROP);
    cv.sprite(x + 1, y + 3, "'--------'", PROP);
    const done = mod(fd(tick, 12), 5);
    const items = ['~~~~', '~~ ~', '~~~ '];
    for (let k = 0; k < 3; k++) {
      const ticked = k < done;
      let box = ticked ? '[x]' : '[ ]';
      if (k === done && mod(fd(tick, 3), 2)) box = '[>]';
      cv.put(x + 2, y + k, box, ticked ? SPARKLE : PROP);
      cv.put(x + 6, y + k, items[k], ticked ? STAR_DIM : PROP2);
    }
  }

  propWand(cv, tick, x, y) {
    cv.cell(x - 1, y + 2, '_', PROP);
    cv.cell(x, y + 1, '/', PROP);
    cv.cell(x + 1, y, ['*', '+', '*', 'x'][mod(fd(tick, 2), 4)], SPARKLE);
    const trail = '.+*+.';
    for (let j = 0; j < 5; j++) {
      const t = mod(tick + j * 4, 20);
      if (t >= 5) continue;
      const r = lcg(37, j, fd(tick + j * 4, 20));
      const sx = x + 2 + int(r() * 7);
      const sy = y - 2 + int(r() * 5);
      cv.cell(sx, sy, trail[t], j % 2 ? SPARKLE : PETAL[1]);
    }
  }
}
Mascot.moods = true;

const theme = makeTheme({
  meta: { id: 'moe', name: 'Moe', panel: PANEL },
  Mascot,
  say: (topic) => word(TOPIC_LINES, topic),
  emoteColours: EMOTE_COLOURS,
});

export const { meta, lines, spans, render, bubble, say, emoteColours } = theme;
