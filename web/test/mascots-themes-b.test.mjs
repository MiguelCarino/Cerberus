// Golden tests for the day-phase-5 mascot ports luna, orbit, seasons, orchard
// and orchard-dark, and for the notification sprites (icons.js). Every frame
// in fixtures/mascots/<theme>/ (written from Atrium's Python by gen.sh, see
// below) must match exactly: text, bubble, colour runs and the emote anchor.
//
// luna, orbit and seasons draw the real time (and seasons the date and the
// moon) in their bottom strip, so each frame is drawn with the clock set to
// the time printed in that fixture's strip.
//
// Regenerate (Atrium is only read). gen.sh reads frames.txt next to itself, so
// a copy runs from a folder holding the frame list wanted: each theme's own
// frames.txt (about 30 frames), and for spring, summer and winter the shorter
// seasons/frames-short.txt (the seasons fixtures are the largest):
//
//   cd web/test/fixtures/mascots
//   for t in luna orbit orchard orchard-dark; do
//     cp gen.sh $t/ && $t/gen.sh $t; rm -f $t/gen.sh; done
//   cp gen.sh seasons/ && HYPR_SEASON=autumn seasons/gen.sh seasons; rm -f seasons/gen.sh
//   mv seasons/seasons.frames.txt seasons/autumn.frames.txt
//   mv seasons/seasons.markup.txt seasons/autumn.markup.txt
//   tmp=$(mktemp -d); cp gen.sh $tmp/; cp seasons/frames-short.txt $tmp/frames.txt
//   for s in spring summer winter; do HYPR_SEASON=$s $tmp/gen.sh seasons
//     mv $tmp/seasons.frames.txt seasons/$s.frames.txt
//     mv $tmp/seasons.markup.txt seasons/$s.markup.txt; done; rm -r $tmp
//   python3 -I -B icons/gen-icons.py > icons/sprites.json
//
// HYPR_SEASON is Atrium's own per-process season override (theme.season).
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { COMBOS, displayWidth, frameSpans } from '../dist/assets/js/mascots/common.js';
import { setWordClock } from '../dist/assets/js/mascots/mood.js';
import { readFrames, readMarkup, FIXTURES } from './fixtures/mascots/golden.mjs';
import * as luna from '../dist/assets/js/mascots/luna.js';
import * as orbit from '../dist/assets/js/mascots/orbit.js';
import * as orchard from '../dist/assets/js/mascots/orchard.js';
import * as orchardDark from '../dist/assets/js/mascots/orchard-dark.js';
import * as seasons from '../dist/assets/js/mascots/seasons.js';
import * as icons from '../dist/assets/js/mascots/icons.js';
import { THEMES } from '../dist/assets/js/mascots/index.js';

// The year the fixtures were generated in: the strip shows day and month only,
// and the moon's phase needs the whole date.
const GEN_YEAR = 2026;
const MONTHS = ['JAN', 'FEB', 'MAR', 'APR', 'MAY', 'JUN', 'JUL', 'AUG', 'SEP', 'OCT', 'NOV', 'DEC'];

// clockFrom reads the strip (the last row's text): the last HH:MM on it and,
// when there, "D MON". Missing parts take noon and 1 November.
function clockFrom(row) {
  const t = [...row.matchAll(/(\d{2}):(\d{2})/g)].at(-1);
  const d = row.match(/ (\d{1,2}) (JAN|FEB|MAR|APR|MAY|JUN|JUL|AUG|SEP|OCT|NOV|DEC) /);
  return new Date(GEN_YEAR, d ? MONTHS.indexOf(d[2]) : 10, d ? Number(d[1]) : 1,
    t ? Number(t[1]) : 12, t ? Number(t[2]) : 0);
}

// golden: goldenTests (fixtures/mascots/golden.mjs) with the clock set per
// frame, for fixtures in a theme's own folder. setup(when) runs before each
// frame is drawn (setClock, a season pin).
function golden(label, base, mod, setup = () => {}, min = 10) {
  const frames = readFrames(base);
  test(`${label}: fixtures are there`, () => {
    assert.ok(frames.length >= min, `${frames.length} frames in ${base}.frames.txt`);
  });
  for (const f of frames) {
    test(`${label} text: ${f.label}`, () => {
      setup(clockFrom(f.lines.at(-1)));
      const got = mod.lines(f.state, f.tick, f.tool, f.mood, f.scene, f.cols, f.rows);
      assert.equal(got.length, f.lines.length);
      for (let r = 0; r < got.length; r++) assert.equal(got[r], f.lines[r], `row ${r}`);
      assert.equal(mod.bubble(f.state, f.tool, f.mood), f.bubble);
    });
  }
  for (const f of readMarkup(base)) {
    test(`${label} colours: ${f.label}`, () => {
      setup(clockFrom(f.spans.at(-1).map((run) => run[0]).join('')));
      const { canvas, head } = mod.render(f.state, f.tick, f.tool, f.mood, f.scene, f.cols, f.rows);
      assert.deepEqual(canvas.spans(), f.spans);
      if (f.head) assert.deepEqual(head, f.head);
    });
  }
}

// shapeTests: every state x tool at a few sizes and ticks gives exact rows of
// exact width, and spans that say the same text.
function shapeTests(label, mod, setup = () => {}) {
  test(`${label}: every state x tool, exact size, spans say the same text`, () => {
    setup(new Date(GEN_YEAR, 0, 15, 9, 5));
    for (const [cols, rows] of [[48, 14], [61, 17], [100, 24], [160, 24]]) {
      for (const [state, tool] of COMBOS) {
        for (const tick of [0, 7, 89, 431]) {
          const ls = mod.lines(state, tick, tool, 'calm', ['night'], cols, rows);
          assert.equal(ls.length, rows, `${state}/${tool} ${cols}x${rows}`);
          for (const l of ls) assert.equal(displayWidth(l), cols);
          const sp = mod.spans(state, tick, tool, 'calm', ['night'], cols, rows);
          assert.deepEqual(sp.map((r) => r.map((run) => run[0]).join('')), ls);
        }
      }
    }
  });
  test(`${label}: meta, API and an emote over the head`, () => {
    for (const fn of ['lines', 'spans', 'render', 'bubble', 'say']) assert.equal(typeof mod[fn], 'function', fn);
    assert.equal(mod.meta.moods, true);
    assert.match(mod.meta.panel, /^#[0-9a-f]{6}$/);
    for (const tone of ['accent', 'warn', 'bad', 'good', 'love', 'dim']) assert.ok(mod.emoteColours[tone], tone);
    const { head } = mod.render('idle', 30, null, 'happy', ['night'], 80, 20);
    assert.ok(head && head[0] >= 0 && head[0] < 80 && head[1] >= 0 && head[1] < 20, `head ${head}`);
    const sp = frameSpans(mod, {
      state: 'idle', tick: 90, cols: 80, rows: 20, mood: 'happy', scene: ['night'], calm: true,
      emote: ['<3', 'love', 0.1],
    });
    const row = sp[head[1]].map((run) => run[0]).join('');
    assert.equal(row.slice(head[0], head[0] + 2), '<3');
  });
}

// ---------- luna ----------

golden('luna', 'luna/luna', luna, (when) => luna.setClock(() => when));
shapeTests('luna', luna, (when) => luna.setClock(() => when));

test('luna: bubbles per state, tool, MCP tool, mood; topics in its words', () => {
  assert.equal(luna.bubble('tool', 'mcp__srv__lookup'), 'Working with lookup…');
  assert.equal(luna.bubble('tool', 'Bash'), 'Running a command…');
  assert.equal(luna.bubble('idle', null, 'sleepy'), 'Folio is idle… zz');
  assert.equal(luna.bubble('thinking', null, 'sleepy'), 'Please wait…');
  assert.equal(luna.bubble('nonsense'), luna.bubble('idle'));
  setWordClock(() => 5000);
  assert.equal(luna.say(['hot', '31°C']), "It's 31°C outside. Stay cool!");
  assert.equal(luna.say(['model', 'qwen3']), 'Say hello to qwen3!');
});

test('luna: the taskbar shows the clock it is given', () => {
  luna.setClock(() => new Date(2026, 0, 2, 7, 4));
  const ls = luna.lines('idle', 0, null, null, null, 80, 20);
  assert.ok(ls.at(-1).endsWith(' 07:04 '), ls.at(-1));
  luna.setClock(null);
});

// ---------- orbit ----------

golden('orbit', 'orbit/orbit', orbit, (when) => orbit.setClock(() => when));
shapeTests('orbit', orbit, (when) => orbit.setClock(() => when));

test('orbit: the strip says the state (STBY PROC EXEC DNLK GO NO-GO) and T+ the tick', () => {
  orbit.setClock(() => new Date(2026, 0, 2, 7, 4));
  const want = { idle: 'STBY', thinking: 'PROC', tool: 'EXEC', writing: 'DNLK', done: 'GO', error: 'NO-GO' };
  for (const [state, word] of Object.entries(want)) {
    const strip = orbit.lines(state, 1234, state === 'tool' ? 'Bash' : null, null, null, 80, 20).at(-1);
    assert.ok(strip.includes(' ' + word + ' '), state + ': ' + strip);
    assert.ok(strip.startsWith('  T+02:03 '), strip);
    assert.ok(strip.endsWith(' 07:04 '), strip);
  }
  assert.equal(orbit.bubble('tool', 'mcp__srv__lookup'), 'Bringing lookup online…');
  assert.equal(orbit.bubble('idle', null, 'sleepy'), 'Pip in safe mode… zz');
  orbit.setClock(null);
});

// ---------- orchard, orchard-dark ----------

golden('orchard', 'orchard/orchard', orchard);
shapeTests('orchard', orchard);
golden('orchard-dark', 'orchard-dark/orchard-dark', orchardDark);
shapeTests('orchard-dark', orchardDark);

test('orchard-dark: the same drawing and words on the dark palette', () => {
  assert.equal(orchardDark.meta.panel, '#232325');
  assert.equal(orchard.meta.panel, '#c4c4ca');
  for (const [state, tool] of COMBOS) {
    assert.deepEqual(orchardDark.lines(state, 77, tool, 'happy', ['night'], 80, 20),
      orchard.lines(state, 77, tool, 'happy', ['night'], 80, 20), `${state}/${tool}`);
    assert.equal(orchardDark.bubble(state, tool), orchard.bubble(state, tool));
  }
  assert.notDeepEqual(orchardDark.spans('idle', 0, null, null, null, 80, 20),
    orchard.spans('idle', 0, null, null, null, 80, 20));
  assert.equal(orchard.bubble('tool', 'mcp__srv__lookup'), 'gathering lookup…');
  assert.equal(orchard.bubble('idle', null, 'calm'), "bzz… the orchard's quiet. what shall we tend?");
});

// ---------- seasons ----------

for (const s of seasons.SEASONS) {
  golden(`seasons ${s}`, `seasons/${s}`, seasons, (when) => {
    seasons.setSeason(s);
    seasons.setClock(() => when);
  }, s === 'autumn' ? 10 : 8);
}
for (const s of seasons.SEASONS) {
  shapeTests(`seasons ${s}`, seasons, (when) => {
    seasons.setSeason(s);
    seasons.setClock(() => when);
  });
}

test('seasons: the season follows the clock and the hemisphere unless pinned', () => {
  seasons.setSeason(null);
  const at = (m) => () => new Date(2026, m - 1, 15, 12, 0);
  const want = ['winter', 'winter', 'spring', 'spring', 'spring', 'summer', 'summer', 'summer',
    'autumn', 'autumn', 'autumn', 'winter'];
  for (let m = 1; m <= 12; m++) {
    seasons.setClock(at(m));
    assert.equal(seasons.currentSeason(), want[m - 1], `month ${m}`);
    assert.equal(seasons.seasonFor(m, 'south'), want[(m + 5) % 12], `south ${m}`);
  }
  seasons.setHemisphere('south');
  seasons.setClock(at(1));
  assert.equal(seasons.currentSeason(), 'summer');
  seasons.setHemisphere('north');
  seasons.setSeason('spring');
  assert.equal(seasons.currentSeason(), 'spring');
  seasons.setSeason('nonsense');
  assert.equal(seasons.currentSeason(), 'winter');
  seasons.setClock(null);
});

test('seasons: the moon and the almanac date match Atrium', () => {
  // [UTC date, mascot_seasons.moon_glyph] from the Python
  const cases = [
    [[2026, 11, 1, 12, 0], '('], [[2026, 11, 10, 0, 0], '.'], [[2026, 11, 17, 6, 0], ')'],
    [[2026, 11, 24, 18, 30], 'O'], [[2026, 12, 1, 0, 0], '('], [[2027, 3, 3, 3, 3], '('],
    [[2000, 1, 6, 18, 14], '.'], [[2026, 1, 3, 0, 0], 'O'],
  ];
  for (const [[y, mo, d, h, mi], glyph] of cases) {
    assert.equal(seasons.moonGlyph(new Date(Date.UTC(y, mo - 1, d, h, mi))), glyph, `${y}-${mo}-${d}`);
  }
  assert.equal(seasons.almanacDate('winter', new Date(2026, 11, 24)), 'WINTER - 24 DEC');
});

test('seasons: emote colours turn with the season, in place; topics in the season\'s words', () => {
  const colours = seasons.emoteColours;
  seasons.setSeason('summer');
  seasons.lines('idle', 0, null, null, null, 80, 20);
  assert.equal(colours.warn, seasons.PALETTES.summer.warn);
  assert.equal(colours.bad, seasons.PALETTES.summer.bad_fg);
  seasons.setSeason('autumn');
  seasons.lines('idle', 0, null, null, null, 80, 20);
  assert.equal(colours.accent, seasons.PALETTES.autumn.accent);
  assert.equal(seasons.emoteColours, colours, 'the same object');
  setWordClock(() => 9000);
  seasons.setSeason('winter');
  assert.equal(seasons.say(['cold', '-3°C']), '-3°C! Scarf on, paws tucked.');
  assert.equal(seasons.say(['music']), 'Is that a song? My ears are dancing ♪');
  seasons.setSeason('spring');
  assert.equal(seasons.say(['cold', '2°C']), '2°C. A late frost, mind the buds.');
  assert.equal(seasons.bubble('tool', 'WebFetch'), "The kite's bringing a page back…");
  assert.equal(seasons.bubble('idle', null, 'sleepy'), 'Curled up in the leaves… zz');
  seasons.setSeason(null);
});

// ---------- icons ----------

const SPRITE_DATA = JSON.parse(readFileSync(join(FIXTURES, 'icons', 'sprites.json'), 'utf8'));

test('icons: every sprite and sign is the Python one, cell for cell', () => {
  assert.equal(icons.GRID, SPRITE_DATA.grid);
  assert.equal(icons.SCALE, SPRITE_DATA.scale);
  assert.deepEqual(icons.MOODS, SPRITE_DATA.moods);
  assert.deepEqual(Object.keys(icons.SPRITES).sort(), Object.keys(SPRITE_DATA.sprites).sort());
  for (const [theme, want] of Object.entries(SPRITE_DATA.sprites)) {
    const got = icons.SPRITES[theme];
    assert.deepEqual(got.palette, want.palette, `${theme} palette`);
    assert.equal(got.rows.length, 16, theme);
    for (let y = 0; y < 16; y++) assert.equal(got.rows[y], want.rows[y], `${theme} row ${y}`);
  }
  assert.deepEqual(icons.SIGNS, SPRITE_DATA.signs);
});

test('icons: every mascot theme has a sprite; unknown themes get moe', () => {
  for (const id of THEMES) assert.ok(icons.SPRITES[id], id);
  assert.equal(icons.sprite('nope'), icons.SPRITES.moe);
});

test('icons: the pixel grid puts the sprite bottom left and the sign top right', () => {
  for (const [theme, data] of Object.entries(SPRITE_DATA.sprites)) {
    for (const mood of SPRITE_DATA.moods) {
      const grid = icons.pixels(theme, mood);
      assert.equal(grid.length, 18);
      const want = Array.from({ length: 18 }, () => new Array(18).fill(null));
      data.rows.forEach((row, y) => [...row].forEach((ch, x) => {
        if (ch !== '.' && data.palette[ch]) want[y + 2][x] = data.palette[ch];
      }));
      const sign = SPRITE_DATA.signs[mood];
      if (sign) {
        sign.rows.forEach((row, y) => [...row].forEach((ch, x) => {
          if (ch !== '.' && sign.palette[ch]) want[y][x + 11] = sign.palette[ch];
        }));
      }
      assert.deepEqual(grid, want, `${theme} ${mood}`);
    }
  }
  // the corner is clear when calm, the "!" in alert's red over it otherwise
  assert.equal(icons.pixels('luna', 'calm')[0][17], null);
  assert.equal(icons.pixels('luna', 'alert')[1][14], '#ffffff');
});

test('icons: drawIcon fills one square per cell; iconDataURL draws on a canvas, null without', () => {
  const rects = [];
  const ctx = { fillRect: (x, y, w, h) => rects.push([ctx.fillStyle, x, y, w, h]) };
  icons.drawIcon(ctx, 'orbit', 'relieved', 2);
  const grid = icons.pixels('orbit', 'relieved');
  const want = [];
  grid.forEach((row, y) => row.forEach((c, x) => { if (c) want.push([c, x * 2, y * 2, 2, 2]); }));
  assert.deepEqual(rects, want);

  assert.equal(icons.iconDataURL('luna'), null, 'no document in node');
  const calls = [];
  const fake = {
    getContext: () => ({
      clearRect: () => calls.push('clear'),
      fillRect: () => calls.push('fill'),
    }),
    toDataURL: (type) => `data:${type};base64,AAAA`,
  };
  assert.equal(icons.iconDataURL('seasons', 'alert', { canvas: fake }), 'data:image/png;base64,AAAA');
  assert.equal(fake.width, 108);
  assert.equal(fake.height, 108);
  assert.equal(calls[0], 'clear');
  assert.ok(calls.filter((c) => c === 'fill').length > 100);
  icons.iconDataURL('nope', 'nope', { canvas: fake, scale: 3 });
  assert.equal(fake.width, 54);
});
