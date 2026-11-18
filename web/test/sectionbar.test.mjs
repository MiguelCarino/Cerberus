// Section bars (sectionbar.js): the pure helpers behind the edge fade, the
// scroll-into-view of the current pill and the tablist keys, and the CSS
// contract that every bar shares one block.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { edgeFade, revealLeft, nextTabIndex } from '../dist/assets/js/sectionbar.js';

test('edgeFade: no fade when everything fits', () => {
  assert.deepEqual(edgeFade(0, 300, 300), { start: false, end: false });
  assert.deepEqual(edgeFade(0, 301, 300), { start: false, end: false }, 'sub-pixel overflow is no overflow');
  assert.deepEqual(edgeFade(0, 200, 300), { start: false, end: false });
});

test('edgeFade: fades the side that hides more', () => {
  assert.deepEqual(edgeFade(0, 800, 360), { start: false, end: true });
  assert.deepEqual(edgeFade(200, 800, 360), { start: true, end: true });
  assert.deepEqual(edgeFade(440, 800, 360), { start: true, end: false });
  assert.deepEqual(edgeFade(439.5, 800, 360), { start: true, end: false }, 'within slack of the end');
  assert.deepEqual(edgeFade(undefined, 800, 360), { start: false, end: true });
  assert.deepEqual(edgeFade(-5, 800, 360), { start: false, end: true }, 'overscroll bounce');
});

test('revealLeft: a pill in view keeps the scroll', () => {
  assert.equal(revealLeft(0, 360, 100, 80), 0);
  assert.equal(revealLeft(120, 360, 200, 80), 120);
});

test('revealLeft: moves as little as it can, with room for the fade', () => {
  // Off the right edge: its right side lands 28 px inside the bar.
  assert.equal(revealLeft(0, 360, 500, 80), 500 + 80 + 28 - 360);
  // Off the left edge: its left side lands 28 px inside, never below 0.
  assert.equal(revealLeft(400, 360, 300, 80), 300 - 28);
  assert.equal(revealLeft(400, 360, 10, 80), 0);
  // A pill wider than the room left for padding gets what room there is.
  assert.equal(revealLeft(0, 100, 300, 90), 300 + 90 + 5 - 100);
});

test('nextTabIndex: arrows wrap, Home and End go to the ends', () => {
  assert.equal(nextTabIndex('ArrowRight', 0, 4), 1);
  assert.equal(nextTabIndex('ArrowRight', 3, 4), 0);
  assert.equal(nextTabIndex('ArrowLeft', 0, 4), 3);
  assert.equal(nextTabIndex('ArrowLeft', -1, 4), 2, 'focus outside the list starts from the end');
  assert.equal(nextTabIndex('Home', 2, 4), 0);
  assert.equal(nextTabIndex('End', 0, 4), 3);
  assert.equal(nextTabIndex('Enter', 0, 4), -1);
  assert.equal(nextTabIndex('ArrowRight', 0, 0), -1);
});

test('one CSS block styles every bar; the old bars are gone', () => {
  const css = (f) => readFileSync(new URL(`../dist/assets/${f}`, import.meta.url), 'utf8');
  const app = css('app.css');
  for (const sel of ['.sbar {', '.sbar-tab {', '.sbar-tab[aria-current], .sbar-tab[aria-selected="true"] {', '.sbar[data-fade-end]']) {
    assert.ok(app.includes(sel), sel);
  }
  // The filled pill reads in both themes: Colourful's on-accent ink, Gold's near-black.
  assert.match(app, /color: var\(--ct-on-accent, #050505\); background: var\(--accent\)/);
  const all = ['app.css', 'features.css', 'control.css', 'chat.css', 'users.css', 'settings.css', 'extras.css', 'files.css', 'preview.css', 'monitor.css'].map(css).join('\n');
  for (const gone of ['.bottom-tabs', '.cn-tabs', '.set-nav-item', '.mon-usage {', '.view-bar']) assert.ok(!all.includes(gone), gone);
  // Nothing else draws its own tab pills.
  assert.ok(!/\.ss-tab\s*\{/.test(all), 'session tabs use .sbar-tab');
});
