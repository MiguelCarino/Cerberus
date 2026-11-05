// Shared reader for the mascot golden fixtures (this folder,
// written by gen.sh from Atrium's Python mascots). Not a test file itself:
// each theme's mascots-THEME.test.mjs calls goldenTests(THEME, module).
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync, existsSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

export const FIXTURES = dirname(fileURLToPath(import.meta.url));

function header(line) {
  const [state, tool, tick, cols, rows, mood, scene] = line.slice(4).split(' ');
  return {
    state,
    tool: tool === '-' ? null : tool,
    tick: Number(tick),
    cols: Number(cols),
    rows: Number(rows),
    mood: mood === '-' ? null : mood,
    scene: scene === '-' ? [] : scene.split(','),
    label: line.slice(4),
  };
}

// readFrames parses THEME.frames.txt: [{...header, lines, bubble}].
export function readFrames(theme) {
  const text = readFileSync(join(FIXTURES, `${theme}.frames.txt`), 'utf8');
  const all = text.split('\n');
  const out = [];
  let i = 0;
  while (i < all.length) {
    if (!all[i].startsWith('=== ')) {
      i++;
      continue;
    }
    const f = header(all[i]);
    f.lines = all.slice(i + 1, i + 1 + f.rows).map((l) => l.slice(1, -1));
    f.bubble = all[i + 1 + f.rows];
    out.push(f);
    i += 2 + f.rows;
  }
  return out;
}

const unescape = (s) => s.replace(/&lt;/g, '<').replace(/&gt;/g, '>').replace(/&amp;/g, '&');

// parseMarkup turns one row of Pango markup (mascot_common.Canvas.markup) into
// the runs Canvas.spans() gives: [text, colour] or [text, colour, bg].
export function parseMarkup(row) {
  const out = [];
  const re = /<span(?: foreground="([^"]*)")?(?: background="([^"]*)")?>(.*?)<\/span>/g;
  let pos = 0;
  let m;
  while ((m = re.exec(row))) {
    if (m.index > pos) out.push([unescape(row.slice(pos, m.index)), null]);
    const run = [unescape(m[3]), m[1] ?? null];
    if (m[2]) run.push(m[2]);
    out.push(run);
    pos = m.index + m[0].length;
  }
  if (pos < row.length) out.push([unescape(row.slice(pos)), null]);
  return out;
}

// readMarkup parses THEME.markup.txt: [{...header, head, spans}].
export function readMarkup(theme) {
  const path = join(FIXTURES, `${theme}.markup.txt`);
  if (!existsSync(path)) return [];
  const all = readFileSync(path, 'utf8').split('\n');
  const out = [];
  let i = 0;
  while (i < all.length) {
    if (!all[i].startsWith('=== ')) {
      i++;
      continue;
    }
    const f = header(all[i]);
    const h = all[i + 1].slice(6);
    f.head = h === '-' ? null : h.split(' ').map(Number);
    f.spans = all.slice(i + 2, i + 2 + f.rows).map(parseMarkup);
    out.push(f);
    i += 2 + f.rows;
  }
  return out;
}

// goldenTests: the theme module draws every fixture frame character for
// character, says the same bubble, and (where markup fixtures exist) colours
// every run the same and leaves the same emote anchor.
export function goldenTests(theme, mod) {
  const frames = readFrames(theme);
  test(`${theme}: fixtures are there`, () => {
    assert.ok(frames.length >= 10, `${frames.length} frames in ${theme}.frames.txt`);
  });
  for (const f of frames) {
    test(`${theme} text: ${f.label}`, () => {
      const got = mod.lines(f.state, f.tick, f.tool, f.mood, f.scene, f.cols, f.rows);
      assert.equal(got.length, f.lines.length);
      for (let r = 0; r < got.length; r++) assert.equal(got[r], f.lines[r], `row ${r}`);
      assert.equal(mod.bubble(f.state, f.tool, f.mood), f.bubble);
    });
  }
  for (const f of readMarkup(theme)) {
    test(`${theme} colours: ${f.label}`, () => {
      const { canvas, head } = mod.render(f.state, f.tick, f.tool, f.mood, f.scene, f.cols, f.rows);
      assert.deepEqual(canvas.spans(), f.spans);
      if (f.head) assert.deepEqual(head, f.head);
    });
  }
}
