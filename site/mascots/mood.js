// The mascot's mood: how she feels, what is around her, what she says. A port
// of Atrium's mascot_mood.py for the browser.
//
// The chat view keeps one Mood and asks it, every frame, for a View:
//
//   const mood = new Mood();
//   mood.refresh();                       re-read the surroundings (the clock)
//   mood.react(event, {text, ...});       something happened in the chat
//   const v = mood.view(state);           {mood, scene, line, emote}
//
// mood is one of MOODS; scene a Set of SCENES (drawn around her); line null or
// a topic array for the theme's say(topic) to word (["morning"], ["model",
// "qwen3"], ...; DEFAULT_LINES is the plain English); emote null or [text,
// tone, age seconds], the sign over her head.
//
// Where the mood comes from, strongest first (the "mood priority"):
//   1. a reaction    a chat event, for a few seconds (thanks -> shy, a failing
//                    tool -> worried, twice in a row: frustrated, ...)
//   2. the talk      word matching on your prompts (counted double) and the
//                    replies, a score that fades over minutes
//   3. surroundings  late night -> sleepy, the morning -> happy (and work
//                    hours, a holiday or a storm when an env provides them)
//   4. chance        when nothing else has a say, a new mood every few minutes
// While the model works (thinking, tool, writing) sleepy turns to focused.
//
// The browser has no weather, sensors or calendar: surroundings() is the time
// of day only. scene_of / topics / the ambient mood still read the other env
// keys (weather, gpu, cpu, music, game, working, off, holiday, event,
// birthdays) when a caller supplies them, exactly as the desktop does.
// Settings (mascot.json in Atrium) are constructor options here.

export const MOODS = ['calm', 'happy', 'excited', 'shy', 'curious', 'focused', 'worried', 'sad', 'sleepy'];
export const SCENES = ['day', 'night', 'sun', 'cloud', 'rain', 'storm', 'snow', 'fog', 'hot', 'cold',
  'music', 'game', 'party', 'festive', 'clock'];
export const ACTIVE = ['thinking', 'tool', 'writing'];
export const SIGNALS = ['talk', 'weather', 'time', 'calendar', 'workhours', 'pc', 'chance'];

export const REACTION_GROUPS = {
  greetings: ['hello', 'welcome_back', 'wake'],
  notices: ['image', 'code', 'long_prompt', '?question'],
  feelings: ['thanks', 'worried', 'frustrated', 'relieved', 'determined', 'proud', 'sad', '?error', '?result'],
  work: ['tests_pass', 'tests_fail', 'commit', 'fast', 'busy', 'aborted', 'model'],
  stage: ['poke', 'poke_spam', 'pet'],
};
export const REACTIONS = Object.keys(REACTION_GROUPS);
export const GROUP_OF = Object.fromEntries(
  Object.entries(REACTION_GROUPS).flatMap(([g, ts]) => ts.map((t) => [t, g])),
);

export const HOT_C = 30;
export const COLD_C = 8;
export const GPU_HOT = 80;
export const CPU_HOT = 85;
export const REACT_S = 9;
export const AWAY_S = 2 * 3600;
export const DOZE_S = 20 * 60;
export const LONG_PROMPT = 1200;
export const FAST_S = 4.0;
export const BUSY_TOOLS = 12;
export const POKE_SPAM = [5, 4.0];
export const PERMISSION_HOLD_S = 600;
export const TALK_HALF_LIFE = 300.0;
export const CHANCE_S = [180, 420];

// What each topic says when the mascot has no voice of its own: {0}, {1} are
// the topic's arguments.
export const DEFAULT_LINES = {
  thanks: "you're welcome!",
  worried: 'oh no...',
  frustrated: 'again?! one more try...',
  relieved: 'phew, it works now!',
  determined: "this one's long -- hang in there!",
  proud: 'done, and nicely too!',
  sad: 'sorry...',
  happy: 'yay!',
  curious: 'hmm, good question...',
  rain: "it's raining -- take an umbrella",
  storm: 'a storm... stay inside!',
  snow: 'snow!',
  fog: 'foggy out there',
  sun: 'sunny today!',
  hot: "it's hot... {0}",
  cold: 'brr, {0} outside',
  gpu_hot: 'the GPU is at {0}°C...',
  cpu_hot: 'the CPU is at {0}°C...',
  music: 'nice song~',
  game: 'game time!',
  event: '{0} in {1} min',
  event_now: '{0} is starting now!',
  birthday: "it's {0}'s birthday!",
  holiday: 'today is {0}!',
  go_home: "it's {0} -- work's over, go home",
  bedtime: "it's {0}... go to bed",
  morning: 'good morning!',
  lunch: 'lunch time?',
  weekend: "it's the weekend~",
  friday: "it's Friday afternoon~",
  hello: 'hi! what are we doing today?',
  welcome_back: 'welcome back!',
  wake: "huh? oh -- I'm up, I'm up!",
  long_prompt: "whoa, that's a long one...",
  code: 'code! let me take a look',
  image: 'ooh, a picture',
  aborted: 'stopping!',
  tests_pass: 'tests are green!',
  tests_fail: 'tests failed...',
  commit: 'shipped!',
  fast: 'quick one!',
  busy: 'busy busy...',
  model: 'new model: {0}',
  poke: 'hey there!',
  poke_spam: 'hey! stop poking!',
  pet: 'aww...',
  rg_memory: '{0} was eating all the memory -- I stopped it',
  rg_vram: "{0} filled the GPU's memory -- I stopped it",
  rg_game_vram: '{0} is using nearly all the GPU memory...',
  rg_panic: 'got it -- {0} is gone',
  rg_stuck: "memory's running out and I can't find who to stop...",
  rg_agents: "all the agents' commands stopped -- the sessions are safe",
};
export const TOPICS = Object.keys(DEFAULT_LINES);

// The sign over the head for each topic (and for the quiet reactions, by
// their mood): [text, tone]. Single-column ASCII only.
export const EMOTE_TONES = ['accent', 'warn', 'bad', 'good', 'love', 'dim'];
export const EMOTES = {
  thanks: ['<3', 'love'], pet: ['<3', 'love'],
  worried: ['!', 'warn'], frustrated: ['#', 'bad'], poke_spam: ['#', 'bad'],
  relieved: ['~', 'accent'], determined: ['!', 'accent'],
  proud: ['*', 'warn'], sad: ['...', 'dim'], happy: ['^^', 'accent'],
  hello: ['o/', 'accent'], welcome_back: ['o/', 'accent'], wake: ['!', 'warn'],
  long_prompt: ['!?', 'warn'], code: ['{}', 'accent'], image: ['[o]', 'accent'],
  aborted: ['!', 'warn'], tests_pass: ['+', 'good'], tests_fail: ['x', 'bad'],
  commit: ['*', 'warn'], fast: ['+', 'accent'], busy: ['...', 'accent'],
  model: ['?', 'accent'], poke: ['!', 'accent'],
  event: ['!', 'warn'], event_now: ['!', 'bad'],
  '@curious': ['?', 'accent'], '@happy': ['^^', 'accent'], '@worried': ['!', 'warn'],
  '@calm': ['-', 'dim'],
};

// Test runners' summary lines and the commands that run them / ship code.
// Python's \b and \s are Unicode-aware; these patterns only meet ASCII words.
const TESTS_PASS = /\b(\d+ passed|OK(?: \(|\s*$)|ok\s+\S+\s+[\d.]+s|test result: ok|Tests?:\s+\d+ passed|\d+ passing)\b/m;
const TESTS_FAIL = /\b(\d+ failed|FAILED(?: \(|\b)|FAIL\s+\S+|test result: FAILED|Tests?:\s+\d+ failed|\d+ failing)/m;
const TEST_CMD = /\b(pytest|unittest|run\.sh|go test|cargo test|npm (?:run )?test|yarn test|pnpm test|jest|mocha|make (?:test|check)|ctest|tox)\b/;
const COMMIT_CMD = /\bgit\s+(?:-C\s+\S+\s+)?(commit|push)\b/;

// toolTopic: a Bash run's outcome worth a word -- ["tests_pass"],
// ["tests_fail"], ["commit"] -- or null.
export function toolTopic(tool, command, output, isError) {
  if ((tool || '') !== 'Bash' || !command) return null;
  if (TEST_CMD.test(command)) {
    const out = output || '';
    const fails = TESTS_FAIL.test(out);
    const passes = TESTS_PASS.test(out);
    if (fails || (isError && !passes)) return ['tests_fail'];
    if (passes || !isError) return ['tests_pass'];
  }
  if (COMMIT_CMD.test(command) && !isError) return ['commit'];
  return null;
}

// formatTopic is Python's fmt.format(*args) for {0}/{1}/{} fields; a field
// with no argument leaves fmt as it was (Python's IndexError path).
function formatTopic(fmt, args) {
  let auto = 0;
  let missing = false;
  const out = fmt.replace(/\{(\d*)\}/g, (_, n) => {
    const i = n === '' ? auto++ : Number(n);
    if (i >= args.length) {
      missing = true;
      return '';
    }
    return String(args[i]);
  });
  return missing ? fmt : out;
}

const variants = new WeakMap(); // table -> Map(topic -> [index, until])
let clockNow = () => (typeof performance !== 'undefined' ? performance.now() : Date.now()) / 1000;

// setWordClock swaps the monotonic clock word() rotates variants by (tests).
export function setWordClock(fn) {
  clockNow = fn;
}

// word: a topic in a mascot's words, table[name] formatted with the topic's
// arguments, DEFAULT_LINES when the table lacks it. An array entry is a set of
// variants: the next one each time the topic comes round again (after
// REACT_S / 3), the same one while it is being said.
export function word(table, topic) {
  let fmt = table[topic[0]] || DEFAULT_LINES[topic[0]] || topic[0];
  if (Array.isArray(fmt)) {
    let m = variants.get(table);
    if (!m) {
      m = new Map();
      variants.set(table, m);
    }
    const now = clockNow();
    let [index, until] = m.get(topic[0]) || [-1, 0];
    if (now >= until) index = (index + 1) % fmt.length;
    m.set(topic[0], [index, now + REACT_S / 3]);
    fmt = fmt[index % fmt.length];
  }
  return formatTopic(fmt, topic.slice(1));
}

const EMPTY = {};
export function defaultLine(topic) {
  return word(EMPTY, topic);
}

// ---------- word matching ----------
// English and Spanish, lower case. NEGATIVE goes first and its matches are cut
// out, so "not working" never also counts as "working".

export const THANKS = ['thanks', 'thank you', 'thx', 'ty', 'gracias', 'arigato', 'arigatou',
  'good girl', 'good job', 'well done', 'cute', 'kawaii', 'love you', 'te quiero'];
export const POSITIVE = ['great', 'awesome', 'perfect', 'nice', 'love', 'cool', 'works', 'worked',
  'working', 'excellent', 'amazing', 'brilliant', 'beautiful', 'yay', 'lol',
  'haha', 'fixed', 'solved', 'success', 'succeeded', 'passes', 'passed',
  'all tests pass', 'done', 'glad', 'happy', 'wonderful', 'genial', 'perfecto',
  'excelente', 'chido', 'padre', 'funciona', 'bien', 'listo', 'bonito',
  ':)', ':d', '<3', '♡', '❤', '🎉', '😊', '😄', '👍', '✨', '^^'];
export const NEGATIVE = ['not working', "doesn't work", 'does not work', "didn't work", "won't work",
  'no funciona', 'not work', 'broken', 'broke', 'wrong', 'error', 'errors',
  'fail', 'failed', 'fails', 'failing', 'failure', 'bug', 'buggy', 'crash',
  'crashed', 'crashes', 'stuck', 'ugh', 'wtf', 'damn', 'hate', 'terrible',
  'awful', 'annoying', 'useless', 'worse', 'worst', 'sorry', 'apologize',
  'unfortunately', 'unable', "can't", 'cannot', "couldn't", 'problem',
  'issue', 'mal', 'falla', 'roto', 'error', 'otra vez', 'todavía', 'sigue',
  'still', 'again', 'frustrat', ':(', '😠', '😡', '😢', '😭', '💀'];
export const QUESTION = ['how', 'what', 'why', 'where', 'which', 'who', 'when', 'can you', 'could you',
  'cómo', 'como', 'qué', 'que', 'por qué', 'dónde', 'cuál'];

// Python's \b in a str pattern: a word character is a letter, a digit or _
// in any script.
const WORD = '[\\p{L}\\p{N}_]';
const ALNUM = /^[\p{L}\p{N}]$/u;

function phraseRe(words, flags) {
  const sorted = [...words].sort((a, b) => Array.from(b).length - Array.from(a).length);
  const parts = sorted.map((w) => {
    const cs = Array.from(w);
    let p = w.replace(/[.*+?^${}()|[\]\\/]/g, '\\$&');
    if (ALNUM.test(cs[0])) p = `(?<!${WORD})${p}`;
    if (ALNUM.test(cs[cs.length - 1])) p = `${p}(?!${WORD})`;
    return p;
  });
  return new RegExp(parts.join('|'), flags);
}

const NEG_RE = phraseRe(NEGATIVE, 'giu');
const POS_RE = phraseRe(POSITIVE, 'giu');
const THANKS_RE = phraseRe(THANKS, 'iu');
const Q_RE = phraseRe(QUESTION, 'iuy');

function count(re, s) {
  re.lastIndex = 0;
  let n = 0;
  while (re.exec(s)) n++;
  return n;
}

// readText returns [score, thanks, question] for some text: score in [-1, 1],
// the balance of positive and negative words, damped so one word is not a
// verdict.
export function readText(text) {
  text = Array.from((text || '').toLowerCase()).slice(0, 8000).join('');
  const neg = count(NEG_RE, text);
  const rest = text.replace(NEG_RE, ' ');
  const thanks = THANKS_RE.test(rest);
  const pos = count(POS_RE, rest) + (thanks ? 1 : 0);
  Q_RE.lastIndex = 0;
  const question = text.trimEnd().endsWith('?') || Q_RE.test(text.trim());
  const score = pos !== neg ? (pos - neg) / (Math.abs(pos - neg) + 2.0) : 0.0;
  return [Math.max(-1.0, Math.min(1.0, score)), thanks, question];
}

// ---------- surroundings ----------

// surroundings is everything outside the conversation the browser can know:
// the time of day.
export function surroundings(now = new Date()) {
  return { now, hour: now.getHours() + now.getMinutes() / 60.0 };
}

// sceneOf gives the SCENES tags for a surroundings object.
export function sceneOf(s, signals) {
  const out = new Set();
  const w = s.weather;
  const night = w ? w.night : !(s.hour >= 7 && s.hour < 19);
  if (signals.weather || signals.time) out.add(night ? 'night' : 'day');
  if (w) {
    if (!(w.kind === 'sun' && night)) out.add(w.kind);
    if (w.feels != null && w.feels >= HOT_C) out.add('hot');
    if (w.feels != null && w.feels <= COLD_C) out.add('cold');
  }
  if ((s.gpu || 0) >= GPU_HOT || (s.cpu || 0) >= CPU_HOT) out.add('hot');
  if (s.music) out.add('music');
  if (s.game) out.add('game');
  if (signals.calendar) {
    if (s.birthdays && s.birthdays.length) out.add('party');
    if (s.holiday) out.add('festive');
    if (s.event) out.add('clock');
  }
  return new Set([...out].filter((t) => SCENES.includes(t)));
}

const pad2 = (n) => String(n).padStart(2, '0');
const hhmm = (d) => `${pad2(d.getHours())}:${pad2(d.getMinutes())}`;
// Python's weekday(): Monday 0 .. Sunday 6.
const weekday = (d) => (d.getDay() + 6) % 7;

// ---------- the engine ----------

// Mood: see the top of the file. opts:
//   signals   {talk, weather, time, calendar, workhours, pc, chance} (all on)
//   reactions {greetings, notices, feelings, work, stage} (all on)
//   enabled   false: always calm, no scenery, plain lines
//   randomOnly  only chance decides
//   calm      reduce motion (the view reads it; the stage slows)
//   clock     () => seconds (default Date.now() / 1000)
//   rng       () => [0, 1) (default Math.random)
//   now       () => Date, for surroundings (default new Date())
export class Mood {
  constructor(opts = {}) {
    const signals = Object.fromEntries(SIGNALS.map((k) => [k, true]));
    Object.assign(signals, opts.signals || {});
    this.enabled = opts.enabled !== false;
    if (!this.enabled) for (const k of SIGNALS) signals[k] = false;
    else if (opts.randomOnly) for (const k of SIGNALS) signals[k] = k === 'chance';
    this.signals = signals;
    const reactions = Object.fromEntries(REACTIONS.map((k) => [k, this.enabled]));
    if (this.enabled) Object.assign(reactions, opts.reactions || {});
    this.reactions = reactions;
    this.calm = Boolean(opts.calm);
    this.clock = opts.clock || (() => Date.now() / 1000);
    this.rng = opts.rng || Math.random;
    this.nowDate = opts.now || (() => new Date());
    this.env = surroundings(this.nowDate());
    this.scene = new Set();
    this.reaction = null; // [mood, until, topic, emote, started]
    this.lastPrompt = null;
    this.lastActivity = this.clock();
    this.toolsThisRun = 0;
    this.busySaid = false;
    this.pokes = [];
    this.talk = 0.0;
    this.talkAt = 0.0;
    this.errorsInRow = 0;
    this.errorsThisRun = 0;
    this.runStarted = null;
    this.longSaid = false;
    this.chance = ['calm', 0.0];
    this.idleTopic = [null, 0.0];
    this.announced = new Set();
    this.refreshed = 0.0;
  }

  uniform(a, b) {
    return a + (b - a) * this.rng();
  }

  // refresh re-reads the surroundings (env: extra keys a caller knows, e.g.
  // in tests). Returns true when the scene changed.
  refresh(env = null) {
    const e = { ...surroundings(this.nowDate()), ...(env || {}) };
    const scene = sceneOf(e, this.signals);
    const changed = scene.size !== this.scene.size || [...scene].some((t) => !this.scene.has(t));
    this.env = e;
    this.scene = scene;
    this.refreshed = this.clock();
    return changed;
  }

  // react to a conversation or stage event:
  //   "prompt"        {text, images}: what you sent
  //   "reply"         {text}: a finished reply (the talk score)
  //   "tool_ok", "tool_error"   {tool, command, output}
  //   "tool_start"    a tool began (counts the run's calls: "busy")
  //   "result_ok", "result_error"   {duration}: the run's seconds
  //   "stopped"       you stopped the run
  //   "tick"          a run is going: the long-run reaction
  //   "permission", "permission_allowed", "permission_denied"
  //   "model"         {name}: the model you just picked
  //   "poke", "pet"   a click / double click on the mascot.
  react(event, { text = null, duration = null, tool = null, command = null, output = null,
    images = 0, name = null } = {}) {
    const now = this.clock();
    const talkOn = this.signals.talk !== false;
    const idleFor = event !== 'tick' && event !== 'reply' ? now - this.lastActivity : 0.0;
    if (['prompt', 'poke', 'pet', 'model'].includes(event)) this.lastActivity = now;
    if (event === 'prompt') {
      const first = this.lastPrompt === null;
      const away = this.lastPrompt !== null && now - this.lastPrompt > AWAY_S;
      this.lastPrompt = now;
      this.runStarted = now;
      this.errorsThisRun = 0;
      this.longSaid = false;
      this.toolsThisRun = 0;
      this.busySaid = false;
      if (!talkOn) return;
      const [score, thanks, question] = readText(text);
      this.nudge(score * 2.0);
      const body = text || '';
      const half = REACT_S / 2;
      this.first(now,
        [thanks, 'shy', 'thanks', REACT_S],
        [score <= -0.3, 'worried', 'worried', REACT_S],
        [first, 'happy', 'hello', REACT_S],
        [away, 'happy', 'welcome_back', REACT_S],
        [idleFor > DOZE_S, 'excited', 'wake', half],
        [images, 'curious', 'image', half],
        [body.includes('```'), 'focused', 'code', half],
        [Array.from(body).length >= LONG_PROMPT, 'excited', 'long_prompt', half],
        [question, 'curious', '?question', half]);
    } else if (event === 'reply') {
      if (talkOn) this.nudge(readText(text)[0]);
    } else if (event === 'tool_start') {
      this.toolsThisRun += 1;
      if (talkOn && this.toolsThisRun >= BUSY_TOOLS && !this.busySaid) {
        this.busySaid = true;
        this.first(now, [true, 'focused', 'busy', REACT_S / 2]);
      }
    } else if (event === 'tool_error') {
      this.errorsInRow += 1;
      this.errorsThisRun += 1;
      if (talkOn) {
        const topic = toolTopic(tool, command, output, true);
        this.first(now,
          [topic, 'worried', topic && topic[0], REACT_S],
          [this.errorsInRow >= 2, 'worried', 'frustrated', REACT_S],
          [true, 'worried', '?error', REACT_S / 2]);
        this.nudge(-0.15);
      }
    } else if (event === 'tool_ok') {
      this.errorsInRow = 0;
      const topic = talkOn ? toolTopic(tool, command, output, false) : null;
      if (topic && topic[0] === 'tests_fail') {
        this.first(now, [true, 'worried', 'tests_fail', REACT_S]);
        this.nudge(-0.15);
      } else if (topic) {
        this.first(now, [true, 'excited', topic[0], REACT_S]);
        this.nudge(0.15);
      }
    } else if (event === 'result_ok') {
      if (talkOn) {
        this.first(now,
          [this.errorsThisRun, 'happy', 'relieved', REACT_S],
          [duration && duration > 60, 'excited', 'proud', REACT_S],
          [duration !== null && duration < FAST_S, 'happy', 'fast', REACT_S / 2],
          [true, 'happy', '?result', REACT_S / 2]);
        this.nudge(0.2);
      }
      this.runStarted = null;
    } else if (event === 'result_error') {
      if (talkOn) {
        this.first(now, [true, 'sad', 'sad', REACT_S]);
        this.nudge(-0.4);
      }
      this.runStarted = null;
    } else if (event === 'stopped') {
      this.runStarted = null;
      if (talkOn) this.first(now, [true, 'worried', 'aborted', REACT_S / 2]);
    } else if (event === 'permission') {
      this.react_('curious', null, now, PERMISSION_HOLD_S);
    } else if (event === 'permission_allowed') {
      this.reaction = null;
      this.react_('happy', null, now, REACT_S / 2);
    } else if (event === 'permission_denied') {
      this.reaction = null;
      this.react_('calm', null, now, REACT_S / 2);
    } else if (event === 'model') {
      if (talkOn && name && this.reactions.work !== false) {
        this.reaction = ['curious', now + REACT_S / 2, ['model', name], EMOTES.model, now];
      }
    } else if (event === 'poke') {
      this.pokes = this.pokes.filter((t) => now - t < POKE_SPAM[1]).concat([now]);
      if (this.reactions.stage === false) return;
      this.reaction = null;
      if (this.pokes.length >= POKE_SPAM[0]) {
        this.pokes = [];
        this.react_('worried', 'poke_spam', now, REACT_S / 2);
      } else {
        this.react_(idleFor < DOZE_S ? 'happy' : 'excited', 'poke', now, REACT_S / 3);
      }
    } else if (event === 'pet') {
      this.pokes = [];
      if (this.reactions.stage === false) return;
      this.reaction = null;
      this.react_('shy', 'pet', now, REACT_S / 2);
    } else if (event === 'tick') {
      if (talkOn && this.runStarted && !this.longSaid && now - this.runStarted > 60) {
        this.longSaid = true;
        this.first(now, [true, 'focused', 'determined', REACT_S]);
      }
    }
  }

  // first reacts with the first [applies, mood, topic, hold] that applies and
  // whose group is on; a "?name" topic is a quiet reaction (no line).
  first(now, ...options) {
    for (const [applies, mood, topic, hold] of options) {
      if (!applies || this.reactions[GROUP_OF[topic]] === false) continue;
      this.react_(mood, topic.startsWith('?') ? null : topic, now, hold);
      return true;
    }
    return false;
  }

  react_(mood, topic, now, hold = REACT_S) {
    // A quiet reaction does not cut a speaking one short.
    if (topic === null && this.reaction && this.reaction[2] && now < this.reaction[1]) return;
    const emote = topic ? EMOTES[topic] || null : EMOTES[`@${mood}`] || null;
    this.reaction = [mood, now + hold, topic ? [topic] : null, emote, now];
  }

  nudge(delta) {
    this.talk = Math.max(-1.0, Math.min(1.0, this.talkNow() + delta * 0.5));
    this.talkAt = this.clock();
  }

  talkNow() {
    if (!this.talk) return 0.0;
    const age = this.clock() - this.talkAt;
    return this.talk * 0.5 ** (age / TALK_HALF_LIFE);
  }

  // view is the mascot's {mood, scene, line, emote} for a state.
  view(state = 'idle') {
    if (!this.enabled) return { mood: 'calm', scene: new Set(), line: null, emote: null };
    const now = this.clock();
    let [mood, line] = this.moodNow(state, this.env, now);
    if (ACTIVE.includes(state) && mood === 'sleepy') mood = 'focused';
    let emote = null;
    const r = this.reaction;
    if (r && r[3] && now < r[1]) emote = [r[3][0], r[3][1], now - r[4]];
    return { mood, scene: this.scene, line, emote };
  }

  moodNow(state, env, now) {
    const sig = this.signals;
    // 1. a reaction
    if (this.reaction && now < this.reaction[1]) return [this.reaction[0], this.reaction[2]];
    this.reaction = null;
    const line = state === 'idle' ? this.idleLine(env, now) : null;
    const ev = sig.calendar ? env.event : null;
    if (ev) {
      const key = JSON.stringify(['event', ...ev.slice(1)]);
      if (!this.announced.has(key)) {
        this.announced.add(key);
        const topic = ev[0] <= 1 ? ['event_now', ev[1]] : ['event', ev[1], ev[0]];
        this.reaction = [ev[0] <= 5 ? 'worried' : 'curious', now + REACT_S * 2, topic, EMOTES[topic[0]], now];
        return [this.reaction[0], topic];
      }
    }
    // 2. the talk
    const talk = sig.talk ? this.talkNow() : 0.0;
    if (talk >= 0.3) return [talk >= 0.7 ? 'excited' : 'happy', line];
    if (talk <= -0.5) return ['sad', line];
    if (talk <= -0.2) return ['worried', line];
    // 3. surroundings
    const amb = this.ambientMood(env);
    if (amb) return [amb, line];
    // 4. chance
    if (sig.chance !== false) return [this.chanceMood(env, now), line];
    return ['calm', line];
  }

  ambientMood(env) {
    const sig = this.signals;
    const h = env.hour ?? 12;
    if (sig.calendar && ((env.birthdays && env.birthdays.length) || env.holiday)) return 'excited';
    const w = env.weather;
    if (w && w.kind === 'storm') return 'worried';
    if (sig.time && (h >= 23.5 || h < 5.5)) return 'sleepy';
    if (sig.workhours && env.working) return 'focused';
    if (sig.time && h >= 6 && h < 9) return 'happy';
    return null;
  }

  chanceMood(env, now) {
    let [mood, until] = this.chance;
    if (now < until) return mood;
    const h = env.hour ?? 12;
    const table = [['calm', 6], ['happy', 3], ['curious', 2], ['shy', 1], ['excited', 1],
      ['sleepy', h >= 22 || h < 7 || (h >= 14 && h < 15.5) ? 3 : 1]];
    const total = table.reduce((s, [, w]) => s + w, 0);
    let pick = this.uniform(0, total);
    for (const [name, wt] of table) {
      pick -= wt;
      if (pick <= 0) {
        mood = name;
        break;
      }
    }
    this.chance = [mood, now + this.uniform(...CHANCE_S)];
    return mood;
  }

  // idleLine: one topic, held for half a minute, then the next that applies
  // -- or null (the mascot's own idle line) every other turn.
  idleLine(env, now) {
    const [held, until] = this.idleTopic;
    if (now < until) return held;
    const options = this.topics(env);
    const turn = Math.floor(now / 30);
    const topic = options.length && turn % 2 === 0 ? options[Math.floor(turn / 2) % options.length] : null;
    this.idleTopic = [topic, now + 30];
    return topic;
  }

  // topics: every topic that applies right now, most pressing first.
  topics(env = null) {
    env = env || this.env;
    const sig = this.signals;
    const now = env.now || this.nowDate();
    const h = env.hour ?? now.getHours();
    const wd = weekday(now);
    const out = [];
    if (sig.calendar) {
      const ev = env.event;
      if (ev) out.push(ev[0] <= 1 ? ['event_now', ev[1]] : ['event', ev[1], ev[0]]);
      for (const name of env.birthdays || []) out.push(['birthday', name]);
      if (env.holiday) out.push(['holiday', env.holiday]);
    }
    if (sig.workhours && !env.working && !env.off && wd < 5 && h >= 18 && h < 18.75) {
      out.push(['go_home', hhmm(now)]);
    }
    if (sig.time) {
      if (h >= 0.5 && h < 5) out.push(['bedtime', hhmm(now)]);
      else if (h >= 6 && h < 9.5) out.push(['morning']);
      else if (h >= 13.5 && h < 14.5 && wd < 5) out.push(['lunch']);
      if (wd >= 5 && h >= 9 && h < 21) out.push(['weekend']);
      else if (wd === 4 && h >= 15 && h < 19) out.push(['friday']);
    }
    if (sig.pc) {
      if ((env.gpu || 0) >= GPU_HOT) out.push(['gpu_hot', env.gpu]);
      else if ((env.cpu || 0) >= CPU_HOT) out.push(['cpu_hot', env.cpu]);
      if (env.game) out.push(['game']);
      else if (env.music) out.push(['music']);
    }
    const w = sig.weather ? env.weather : null;
    if (w) {
      if (['rain', 'storm', 'snow', 'fog'].includes(w.kind)) out.push([w.kind]);
      else if (w.kind === 'sun' && !w.night) out.push(['sun']);
      if (w.feels != null && w.feels >= HOT_C) out.push(['hot', `${w.feels}°C`]);
      else if (w.feels != null && w.feels <= COLD_C) out.push(['cold', `${w.feels}°C`]);
    }
    return out;
  }
}
