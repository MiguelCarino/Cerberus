// Golden tests for the day-phase-5 mascot ports matrix, win95, aero, cyberpunk
// and lofi. Every frame in fixtures/mascots/<theme>/ (written from Atrium's
// Python by gen.sh, see below) must match exactly: text, bubble, colour runs
// and the emote anchor.
//
// win95's taskbar shows the real time, so each of its frames is drawn with the
// clock set to the time printed in that fixture's taskbar (the frames and the
// markup file each carry their own: they are generated a moment apart).
//
// Regenerate (Atrium is only read; each theme's frame list is its frames.txt):
//
//   cd web/test/fixtures/mascots
//   for t in matrix win95 aero cyberpunk lofi; do
//     cp gen.sh $t/ && $t/gen.sh $t; rm -f $t/gen.sh; done
//
// Run: node --test web/test/*.test.mjs
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { COMBOS, displayWidth, frameSpans } from '../dist/assets/js/mascots/common.js';
import { setWordClock, MOODS, SCENES } from '../dist/assets/js/mascots/mood.js';
import { goldenTests, readFrames, readMarkup } from './fixtures/mascots/golden.mjs';
import { loadTheme } from '../dist/assets/js/mascots/index.js';
import * as matrix from '../dist/assets/js/mascots/matrix.js';
import * as win95 from '../dist/assets/js/mascots/win95.js';
import * as aero from '../dist/assets/js/mascots/aero.js';
import * as cyberpunk from '../dist/assets/js/mascots/cyberpunk.js';
import * as lofi from '../dist/assets/js/mascots/lofi.js';

const THEMES = { matrix, win95, aero, cyberpunk, lofi };
const PANELS = { matrix: '#010803', win95: '#4a4a4a', aero: '#12325a', cyberpunk: '#07090f', lofi: '#1c1830' };
const NAMES = { matrix: 'Matrix', win95: 'Windows 95', aero: 'Aero', cyberpunk: 'Cyberpunk', lofi: 'Lo-fi' };

// ---------- the clock in win95's taskbar ----------

// clockOf reads " H:MM AM|PM " off a frame's last row: [hours 0-23, minutes].
function clockOf(row) {
  const hit = row.match(/ (\d+):(\d\d) (AM|PM) *$/);
  if (!hit) return null;
  let h = Number(hit[1]) % 12;
  if (hit[3] === 'PM') h += 12;
  return [h, Number(hit[2])];
}

test('clockOf reads the taskbar clock', () => {
  assert.deepEqual(clockOf('[Start] ...   12:05 AM '), [0, 5]);
  assert.deepEqual(clockOf('[Start] ...   12:40 PM '), [12, 40]);
  assert.deepEqual(clockOf('[Start] ...    9:07 PM '), [21, 7]);
  assert.equal(clockOf('no clock here'), null);
});

// labelOf is a fixture header's label for the arguments goldenTests passes.
const labelOf = (a) => [a[0], a[2] || '-', a[1], a[5], a[6], a[3] || '-',
  a[4] && a[4].length ? a[4].join(',') : '-'].join(' ');

// clocked wraps a theme module so each frame is drawn at its fixture's time.
function clocked(mod, fixture) {
  const textAt = new Map();
  const colourAt = new Map();
  for (const f of readFrames(fixture)) textAt.set(f.label, clockOf(f.lines.at(-1)));
  for (const f of readMarkup(fixture)) colourAt.set(f.label, clockOf(f.spans.at(-1).map((r) => r[0]).join('')));
  const at = (map, a) => {
    const t = map.get(labelOf(a)) || [12, 0];
    mod.setClock(() => new Date(2026, 0, 1, t[0], t[1]));
  };
  return {
    ...mod,
    lines: (...a) => { at(textAt, a); return mod.lines(...a); },
    render: (...a) => { at(colourAt, a); return mod.render(...a); },
  };
}

// ---------- the goldens ----------

for (const [id, mod] of Object.entries(THEMES)) {
  const fixture = `${id}/${id}`;
  goldenTests(fixture, id === 'win95' ? clocked(mod, fixture) : mod);
}
test.after(() => win95.setClock(null));

// ---------- the module API ----------

for (const [id, mod] of Object.entries(THEMES)) {
  test(`${id}: meta follows the theme API`, () => {
    assert.equal(mod.meta.id, id);
    assert.equal(mod.meta.name, NAMES[id]);
    assert.equal(mod.meta.panel, PANELS[id]);
    assert.equal(mod.meta.moods, true);
    for (const fn of ['lines', 'spans', 'render', 'bubble', 'say']) assert.equal(typeof mod[fn], 'function', fn);
    for (const tone of ['accent', 'warn', 'bad', 'good', 'love', 'dim']) {
      assert.match(mod.emoteColours[tone], /^#[0-9a-f]{6}$/, tone);
    }
  });

  test(`${id}: every state x tool, exact size, spans say the same text`, () => {
    win95.setClock(() => new Date(2026, 0, 1, 15, 4));
    for (const [cols, rows] of [[48, 14], [61, 17], [100, 24]]) {
      for (const [state, tool] of COMBOS) {
        for (const tick of [0, 7, 89, 431]) {
          const ls = mod.lines(state, tick, tool, null, null, cols, rows);
          assert.equal(ls.length, rows);
          for (const l of ls) assert.equal(displayWidth(l), cols);
          const sp = mod.spans(state, tick, tool, null, null, cols, rows);
          assert.deepEqual(sp.map((r) => r.map((run) => run[0]).join('')), ls);
        }
      }
    }
    win95.setClock(null);
  });

  test(`${id}: every mood and scene stays inside the grid`, () => {
    for (const mood of MOODS) {
      for (const [state, tool] of [['idle', null], ['tool', 'Task'], ['done', null], ['error', null]]) {
        for (const tick of [3, 80, 117]) {
          const { canvas, head } = mod.render(state, tick, tool, mood, SCENES, 80, 20);
          assert.equal(canvas.lines().length, 20);
          for (const l of canvas.lines()) assert.equal(displayWidth(l), 80);
          assert.ok(head && head[0] >= 0 && head[0] < 80 && head[1] >= 0 && head[1] < 20, `${mood} head ${head}`);
        }
      }
    }
  });

  test(`${id}: a frame is a pure function of its inputs`, () => {
    win95.setClock(() => new Date(2026, 0, 1, 9, 30));
    const a = mod.lines('idle', 1234, null, 'happy', ['night', 'music'], 80, 20);
    mod.lines('tool', 5, 'Bash', null, null, 160, 24);
    mod.lines('idle', 99, null, 'sad', ['rain'], 48, 14);
    const b = mod.lines('idle', 1234, null, 'happy', new Set(['music', 'night']), 80, 20);
    assert.deepEqual(a, b);
    assert.deepEqual(mod.lines('idle', 3, null, null, 'night,music', 80, 20),
      mod.lines('idle', 3, null, null, ['night', 'music'], 80, 20));
    win95.setClock(null);
  });

  test(`${id}: bubbles per state, tool, MCP tool and mood`, () => {
    assert.equal(mod.bubble('nonsense'), mod.bubble('idle'));
    assert.notEqual(mod.bubble('idle', null, 'sad'), mod.bubble('thinking', null, 'sad'));
    assert.ok(mod.bubble('tool', 'mcp__srv__lookup').includes('lookup'));
    assert.ok(mod.bubble('tool', 'SomeTool').includes('SomeTool'));
    assert.equal(mod.bubble('thinking', null, 'sleepy'), mod.bubble('thinking'));
  });

  test(`${id}: emote over the head, steady under reduce motion`, () => {
    const base = { state: 'idle', tick: 30, cols: 80, rows: 20, mood: 'happy', scene: ['night'] };
    const { head } = mod.render('idle', 30, null, 'happy', ['night'], 80, 20);
    const text = (sp) => sp.map((r) => r.map((run) => run[0]).join(''));
    const steadyHead = mod.render('idle', 10, null, 'happy', ['night'], 80, 20).head;
    const steady = text(frameSpans(mod, { ...base, tick: 30, calm: true, emote: ['<3', 'love', 0.1] }));
    assert.equal(steady[steadyHead[1]].slice(steadyHead[0], steadyHead[0] + 2), '<3');
    const popping = text(frameSpans(mod, { ...base, emote: ['<3', 'love', 0.1] }));
    assert.equal(popping[head[1]][head[0]], '.');
  });
}

test('the bubbles say what Atrium says', () => {
  assert.equal(matrix.bubble('tool', 'mcp__srv__lookup'), 'Follow the white rabbit: lookup');
  assert.equal(matrix.bubble('idle', null, 'excited'), "> i know kung fu. let's go!");
  assert.equal(win95.bubble('tool', null), 'Running tool.exe…');
  assert.equal(win95.bubble('tool', 'Write'), 'Saving to C:\\…');
  assert.equal(aero.bubble('tool', 'Unknown'), 'Working with Unknown…');
  assert.equal(cyberpunk.bubble('done'), "Job's done. Preem work.");
  assert.equal(lofi.bubble('idle', null, 'calm'), "mrrp… lo-fi beats and you. what's up?");
  assert.equal(lofi.bubble('tool', 'mcp__a__yarn'), 'batting at yarn…');
});

test('say words topics in each voice, rotating variants', () => {
  let now = 5000;
  setWordClock(() => now);
  assert.equal(matrix.say(['model', 'qwen3']), '> new program loaded: qwen3');
  assert.equal(win95.say(['event', 'Standup', 10]), 'Scheduled Task: Standup in 10 min.');
  assert.equal(cyberpunk.say(['gpu_hot', 84]), '[SYS] GPU thermal warning: 84°C');
  assert.equal(lofi.say(['hot', '31°C']), "it's 31°C… i'm melting into the cushion.");
  const first = aero.say(['poke']);
  assert.equal(aero.say(['poke']), first, 'the same variant while it is being said');
  now += 10;
  assert.notEqual(aero.say(['poke']), first, 'the next one when it comes round again');
  assert.equal(win95.say(['nope']), 'nope');
});

test('win95: the taskbar clock is the local time, 12-hour', () => {
  const at = (h, m) => {
    win95.setClock(() => new Date(2026, 0, 1, h, m));
    return win95.lines('idle', 0, null, null, null, 80, 20).at(-1);
  };
  assert.ok(at(0, 5).endsWith(' 12:05 AM '));
  assert.ok(at(13, 9).endsWith(' 1:09 PM '));
  assert.ok(at(12, 0).endsWith(' 12:00 PM '));
  win95.setClock(null);
});

test('loadTheme gives these modules', async () => {
  for (const [id, mod] of Object.entries(THEMES)) {
    const got = await loadTheme(id);
    assert.equal(got.meta.id, id);
    assert.equal(got.lines, mod.lines);
  }
});
