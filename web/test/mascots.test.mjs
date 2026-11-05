// Unit tests for the mascot engine shared by every theme: the Python-faithful
// arithmetic and noise, the Canvas and its colour runs, the reaction sign, the
// mood engine (sentiment scorer, reactions, priority, topics), the stand-in and
// the lazy theme registry. Expected values were taken from Atrium's Python.
// Run: node --test web/test/*.test.mjs
import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  lcg, fd, mod, dw, displayWidth, propKind, Canvas, blink, drawEmote, calmTick, pyfmt, sceneSet,
  streamLines, BaseMascot, makeTheme,
} from '../dist/assets/js/mascots/common.js';
import {
  readText, toolTopic, word, defaultLine, setWordClock, Mood, sceneOf, MOODS, SCENES, EMOTES, REACT_S,
} from '../dist/assets/js/mascots/mood.js';
import * as standin from '../dist/assets/js/mascots/standin.js';
import { loadTheme, THEMES, NAMES, DEFAULT, hasTheme } from '../dist/assets/js/mascots/index.js';

// ---------- common ----------

test('lcg: the same draws as mascot_common.lcg', () => {
  const r = lcg(7, 3, 80, 20);
  assert.deepEqual([r(), r(), r()], [0.9487162828445435, 0.2769167423248291, 0.1323937177658081]);
  const s = lcg(-5, 2 ** 33 + 7); // negative and wide seeds fold to 32 bits
  assert.deepEqual([s(), s()], [0.7288743257522583, 0.8665408492088318]);
});

test('fd / mod: Python floor division and modulo', () => {
  assert.equal(fd(-7, 2), -4);
  assert.equal(mod(-7, 3), 2);
  assert.equal(fd(-7.5, 2), -4);
  assert.equal(mod(-7.5, 2), 0.5);
  assert.equal(fd(7, 2), 3);
  assert.equal(calmTick(10), 3);
  assert.equal(calmTick(10, false), 10);
});

test('dw: ambiguous and wide are two columns, combining marks none', () => {
  assert.deepEqual(Array.from('a°é─あ́').map(dw), [1, 2, 2, 2, 2, 0]);
  assert.equal(displayWidth('ab'), 2);
});

test('propKind maps tools to props', () => {
  assert.equal(propKind('Bash'), 'bash');
  assert.equal(propKind('NotebookRead'), 'book');
  assert.equal(propKind('MultiEdit'), 'pad');
  assert.equal(propKind('ToolSearch'), 'lens');
  assert.equal(propKind('WebFetch'), 'globe');
  assert.equal(propKind('Agent'), 'helpers');
  assert.equal(propKind('TodoWrite'), 'todo');
  assert.equal(propKind('mcp__a__b'), 'wand');
  assert.equal(propKind(null), 'wand');
});

test('Canvas: clipping, wide characters refused, runs merged as markup()', () => {
  const cv = new Canvas(6, 2);
  cv.put(0, 0, 'ab cd', '#111');
  cv.fill(3, 1, 2, 1, '#222');
  cv.cell(5, 1, 'x', '#333', '#222');
  cv.cell(0, 1, 'あ', '#444');
  cv.cell(9, 9, 'z', '#555');
  assert.deepEqual(cv.spans(), [
    [['ab cd ', '#111']],
    [['?  ', '#444'], ['  ', null, '#222'], ['x', '#333', '#222']],
  ]);
  cv.shiftRow(0, 2);
  assert.deepEqual(cv.lines(), ['d ab c', '?    x']);
});

test('Canvas.sprite: outer spaces transparent, inner opaque, cells occupied', () => {
  const cv = new Canvas(8, 1);
  cv.put(0, 0, '########', '#111');
  cv.sprite(1, 0, ' a  b ', '#222');
  assert.equal(cv.lines()[0], '##a  b##');
  assert.deepEqual(cv.occ[0], [false, false, true, true, true, true, false, false]);
});

test('blink, pyfmt, sceneSet, streamLines', () => {
  assert.equal(blink(0), true);
  assert.equal(blink(2), false);
  assert.equal(blink(159 + 5), true);
  assert.equal(pyfmt('go %s! 100%%', 'x'), 'go x! 100%');
  assert.deepEqual([...sceneSet('day,rain')], ['day', 'rain']);
  assert.equal(sceneSet(null).size, 0);
  const s = streamLines(['ab', 'c'], 5, 3, 20);
  assert.ok(s.every((l) => l.length === 5));
});

test('drawEmote: pops in, floats, blinks out; steady under reduce motion', () => {
  const at = (emote, steady = false) => {
    const cv = new Canvas(10, 4);
    drawEmote(cv, emote, [8, 2], null, steady);
    return cv.lines();
  };
  assert.equal(at(['<3', 'love', 0.1])[2], '        . ');
  assert.equal(at(['<3', 'love', 1.0])[2], '        <3');
  assert.equal(at(['<3', 'love', 2.0])[1], '        <3');
  assert.deepEqual(at(['<3', 'love', 8.3]), ['', '', '', ''].map(() => ' '.repeat(10)));
  assert.equal(at(['<3', 'love', 8.3], true)[2], '        <3');
  assert.equal(at(['...', 'dim', 1.0])[2], '       ...', 'clipped inside the canvas');
});

test('BaseMascot + makeTheme: the module API for themes without moods', () => {
  class Box extends BaseMascot {
    render(state, tick) {
      const cv = new Canvas(this.cols, this.rows);
      cv.put(0, 0, `${state[0]}${tick % 10}`, '#abcdef');
      return cv;
    }
  }
  Box.BUBBLES = { idle: 'hi', done: 'ok' };
  Box.TOOL_BUBBLES = { Bash: 'shell' };
  Box.OTHER = 'using %s';
  const t = makeTheme({ meta: { id: 'box', name: 'Box', panel: '#000' }, Mascot: Box, say: defaultLine });
  assert.equal(t.meta.moods, false);
  assert.deepEqual(t.lines('done', 13, null, 'happy', ['day'], 4, 2), ['d3  ', '    ']);
  assert.deepEqual(t.spans('idle', 1, null, null, null, 3, 1), [[['i1 ', '#abcdef']]]);
  assert.equal(t.bubble('tool', 'Bash'), 'shell');
  assert.equal(t.bubble('tool', 'mcp__s__find'), 'using find');
  assert.equal(t.bubble('tool', 'Zap'), 'using Zap');
  assert.equal(t.bubble('thinking'), 'hi');
});

// ---------- mood ----------

test('readText: the sentiment scorer agrees with mascot_mood.read_text', () => {
  const cases = [
    ['thanks a lot!', [1 / 3, true, false]],
    ['this is not working again', [-0.5, false, false]],
    ['How do I fix this?', [0, false, true]],
    ['qué pasa', [0, false, true]],
    ['great, all tests pass :)', [0.6, false, false]],
    ['ugh, error after error', [-0.6, false, false]],
    ['cómo estás', [0, false, true]],
    ['te quiero ♡', [0.5, true, false]],
    ['', [0, false, false]],
    [' why?', [0, false, true]],
    ['that is broken but fixed now 🎉', [1 / 3, false, false]],
    ['the thermometer is fine', [0, false, false]],
  ];
  for (const [text, want] of cases) assert.deepEqual(readText(text), want, text);
});

test('toolTopic: tests passing, failing, commits', () => {
  assert.deepEqual(toolTopic('Bash', 'go test ./...', 'ok  pkg 0.2s', false), ['tests_pass']);
  assert.deepEqual(toolTopic('Bash', 'pytest', '3 failed, 2 passed', false), ['tests_fail']);
  assert.deepEqual(toolTopic('Bash', 'git -C x commit -m y', '', false), ['commit']);
  assert.deepEqual(toolTopic('Bash', 'make test', 'boom', true), ['tests_fail']);
  assert.equal(toolTopic('Read', 'pytest', '', false), null);
});

test('word: formats arguments, keeps the line when one is missing', () => {
  assert.equal(defaultLine(['hot']), "it's hot... {0}");
  assert.equal(defaultLine(['event', 'A', 3]), 'A in 3 min');
  assert.equal(word({ x: 'X {1}{0}' }, ['x', 'a', 'b']), 'X ba');
});

function fakeMood(opts = {}) {
  const clock = { t: 100000 };
  const m = new Mood({
    clock: () => clock.t,
    rng: () => 0, // chance: always the first entry, calm
    now: () => new Date(2026, 10, 3, 15, 0), // a Tuesday afternoon: no time topics
    ...opts,
  });
  m.refresh();
  return { m, clock };
}

test('Mood: greeting, thanks, quiet reactions and the hold', () => {
  const { m, clock } = fakeMood();
  m.react('prompt', { text: 'hello there' });
  let v = m.view('thinking');
  assert.equal(v.mood, 'happy');
  assert.deepEqual(v.line, ['hello']);
  assert.deepEqual(v.emote.slice(0, 2), EMOTES.hello);
  clock.t += REACT_S + 1;
  m.react('prompt', { text: 'thanks!' });
  assert.equal(m.view('idle').mood, 'shy');
  // a quiet reaction does not cut "you're welcome" short
  m.react('result_ok', { duration: 10 });
  assert.deepEqual(m.view('idle').line, ['thanks']);
  clock.t += REACT_S + 1;
  assert.equal(m.view('idle').emote, null);
});

test('Mood: errors -> worried, frustrated, relieved; tests and commits', () => {
  const { m, clock } = fakeMood();
  m.react('prompt', { text: 'run it' });
  clock.t += REACT_S + 1;
  m.react('tool_error', { tool: 'Read' });
  assert.equal(m.view('tool').mood, 'worried');
  m.react('tool_error', { tool: 'Read' });
  assert.deepEqual(m.view('tool').line, ['frustrated']);
  clock.t += REACT_S + 1;
  m.react('result_ok', { duration: 30 });
  assert.deepEqual(m.view('done').line, ['relieved']);
  clock.t += REACT_S + 1;
  m.react('tool_ok', { tool: 'Bash', command: 'npm test', output: '12 passing' });
  assert.deepEqual(m.view('tool').line, ['tests_pass']);
  assert.equal(m.view('tool').mood, 'excited');
});

test('Mood: pokes, poke spam, pet, the stage group switch', () => {
  const { m, clock } = fakeMood();
  for (let i = 0; i < 4; i++) {
    m.react('poke');
    clock.t += 0.5;
  }
  assert.deepEqual(m.view('idle').line, ['poke']);
  m.react('poke');
  assert.deepEqual(m.view('idle').line, ['poke_spam']);
  m.react('pet');
  assert.equal(m.view('idle').mood, 'shy');
  const off = fakeMood({ reactions: { stage: false } }).m;
  off.react('poke');
  assert.equal(off.reaction, null);
});

test('Mood: priority -- reaction, talk, surroundings, chance; sleepy works', () => {
  const night = fakeMood({ now: () => new Date(2026, 10, 3, 2, 0) }).m;
  assert.equal(night.view('idle').mood, 'sleepy');
  assert.equal(night.view('tool').mood, 'focused', 'no dozing on the job');
  assert.ok(night.scene.has('night'));
  const { m, clock } = fakeMood();
  assert.equal(m.view('idle').mood, 'calm', 'chance picks calm with rng 0');
  m.react('reply', { text: 'awesome, perfect, great, works!' });
  m.react('reply', { text: 'awesome, perfect, great, works!' });
  assert.equal(m.view('idle').mood, 'happy');
  clock.t += 3600; // the talk score fades
  assert.equal(m.view('idle').mood, 'calm');
  const disabled = new Mood({ enabled: false });
  assert.deepEqual(disabled.view('idle'), { mood: 'calm', scene: new Set(), line: null, emote: null });
});

test('Mood: permission holds curious until answered; model and long runs', () => {
  const { m, clock } = fakeMood();
  m.react('permission');
  clock.t += 100;
  assert.equal(m.view('tool').mood, 'curious');
  m.react('permission_allowed');
  assert.equal(m.view('tool').mood, 'happy');
  clock.t += REACT_S;
  m.react('model', { name: 'qwen3' });
  assert.deepEqual(m.view('idle').line, ['model', 'qwen3']);
  clock.t += REACT_S;
  m.react('prompt', { text: 'go' });
  clock.t += REACT_S + 61;
  m.react('tick');
  assert.deepEqual(m.view('tool').line, ['determined']);
});

test('Mood: idle topics by the clock, every other half minute', () => {
  const { m, clock } = fakeMood({ now: () => new Date(2026, 10, 7, 7, 0) }); // Saturday morning
  assert.deepEqual(m.topics(), [['morning']]);
  clock.t = 30 * 4; // an even turn
  assert.deepEqual(m.view('idle').line, ['morning']);
  clock.t = 30 * 5 + 1; // an odd turn: her own idle line
  assert.equal(m.view('idle').line, null);
  const late = fakeMood({ now: () => new Date(2026, 10, 7, 1, 30) }).m;
  assert.deepEqual(late.topics()[0], ['bedtime', '01:30']);
});

test('sceneOf: day / night and the optional env keys', () => {
  const on = { time: true, weather: true, calendar: true };
  assert.deepEqual([...sceneOf({ hour: 12 }, on)], ['day']);
  assert.deepEqual([...sceneOf({ hour: 22, weather: { kind: 'sun', night: true, feels: 2 } }, on)].sort(),
    ['cold', 'night']);
  assert.deepEqual([...sceneOf({ hour: 12, music: true, holiday: 'x' }, on)].sort(), ['day', 'festive', 'music']);
  assert.equal(MOODS.length, 9);
  assert.equal(SCENES.length, 15);
});

test('word rotates variants per table by the clock', () => {
  let now = 50;
  setWordClock(() => now);
  const table = { t: ['a', 'b'] };
  assert.equal(word(table, ['t']), 'a');
  assert.equal(word(table, ['t']), 'a');
  now += REACT_S;
  assert.equal(word(table, ['t']), 'b');
  now += REACT_S;
  assert.equal(word(table, ['t']), 'a');
});

// ---------- stand-in and registry ----------

test('standin: a centred kaomoji, Python str.center', () => {
  assert.equal(standin.center('ab', 5), '  ab ');
  assert.equal(standin.center('abc', 6), ' abc  ');
  assert.equal(standin.center('abc', 2), 'abc');
  const ls = standin.lines('done', 0, null, null, null, 20, 5);
  assert.equal(ls.length, 5);
  assert.equal(ls[2].trim(), '(≧◡≦) ♡');
  assert.equal(standin.bubble('tool', 'Bash'), 'Bash がんばる！');
  assert.equal(standin.bubble('nope'), '…');
  assert.deepEqual(standin.spans('idle', 0, null, null, null, 4, 3)[0], [['    ', standin.STAGE_FG]]);
});

test('registry: all eleven themes registered, moe loads, unknown and missing fall back', async () => {
  assert.deepEqual(THEMES, ['moe', 'matrix', 'win95', 'aero', 'cyberpunk', 'lofi', 'luna', 'orbit',
    'seasons', 'orchard', 'orchard-dark']);
  assert.equal(DEFAULT, 'moe');
  for (const id of THEMES) assert.ok(NAMES[id], id);
  assert.ok(hasTheme('orchard-dark'));
  assert.ok(!hasTheme('nope'));
  const moe = await loadTheme('moe');
  assert.equal(moe.meta.id, 'moe');
  assert.equal((await loadTheme('nope')).meta.id, 'moe');
  // a registered theme loads its own module, or moe until that file exists
  const m = await loadTheme('matrix');
  assert.ok(['matrix', 'moe'].includes(m.meta.id));
  assert.strictEqual(await loadTheme('moe'), moe, 'modules are cached');
});
