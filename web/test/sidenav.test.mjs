// The side nav (sidenav.js): the main views as a column left of main#app,
// one scrolling row above it on phones. Its specs from mainTabs, an icon
// per view, and the CSS contract (column, current link, phone row with the
// section bar's edge fade, the views that fold it away).
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { NAV_ICONS, navItems } from '../dist/assets/js/sidenav.js';
import { MAIN_TABS, mainTabs, normMe, ROLE_CAPS } from '../dist/assets/js/users.js';

const css = (f) => readFileSync(new URL(`../dist/assets/${f}`, import.meta.url), 'utf8');

test('navItems: one link per main tab, in order, hash links with the tab title', () => {
  const tabs = MAIN_TABS.map(([t, label, sub]) => [t, label, sub]);
  const items = navItems(tabs);
  assert.deepEqual(items.map((it) => it.key), MAIN_TABS.map(([t]) => t));
  assert.equal(items.at(-1).key, 'account', 'Account is last (the foot of the column)');
  for (const it of items) {
    assert.equal(it.href, '#/' + it.key);
    assert.ok(it.label && it.title, it.key);
  }
  assert.deepEqual(navItems([]), []);
  assert.deepEqual(navItems(null), []);
});

test('navItems follows the role filter of mainTabs', () => {
  const viewer = normMe({ kind: 'login', role: 'viewer', caps: ROLE_CAPS.viewer, has_users: true,
    user: { id: 'u-viewer0000000', name: 'viewer', role: 'viewer' } });
  assert.deepEqual(navItems(mainTabs(viewer)).map((it) => it.key), ['sessions', 'monitor', 'account']);
  assert.deepEqual(navItems(mainTabs(null)), [], 'nobody signed in: no links');
});

test('every main view has an inline icon (paths only, no font, no request)', () => {
  for (const [t] of MAIN_TABS) {
    const paths = NAV_ICONS[t];
    assert.ok(Array.isArray(paths) && paths.length, t);
    for (const d of paths) assert.match(d, /^M[\d\s.,MmLlHhVvCcSsQqTtAaZz-]+$/, `${t}: ${d}`);
  }
});

test('CSS: a column beside #app, the current link in the accent, a row on phones', () => {
  const app = css('app.css');
  assert.match(app, /\.app-shell \{ flex: 1 1 auto; min-height: 0; display: flex; \}/);
  assert.match(app, /\.side-nav \{[^}]*flex: 0 0 212px; display: flex; flex-direction: column;/);
  assert.match(app, /\.side-link\[aria-current\] \{ color: var\(--accent\); border-color: var\(--accent\); background: var\(--accent-faint\); \}/);
  assert.match(app, /\.side-link:focus-visible \{ outline: 2px solid var\(--accent\)/);
  assert.match(app, /\.side-link\[data-tab="account"\] \{ margin-top: auto; \}/);
  assert.match(app, /body\[data-bare\] \.side-nav \{ display: none; \}/);
  assert.match(app, /#app \{ flex: 1 1 auto; min-width: 0; min-height: 0; overflow-y: auto;/);
  // Phones: one row that scrolls sideways with the section bar's edge fade,
  // folded in a session, a diff and under the on-screen keyboard.
  const phone = app.slice(app.indexOf('@media (max-width: 720px)'));
  assert.ok(phone.length > 0 && app.includes('@media (max-width: 720px)'));
  assert.match(phone, /\.app-shell \{ flex-direction: column; \}/);
  assert.match(phone, /\.side-nav \{[^}]*flex-direction: row;[^}]*overflow-x: auto;/);
  assert.match(phone, /\.side-nav\[data-fade-end\]/);
  assert.match(phone, /body\[data-view="session"\] \.side-nav, body\[data-view="diff"\] \.side-nav, body\[data-kb\] \.side-nav \{ display: none; \}/);
  // The waiting badge reads on Colourful's accent too.
  assert.match(app, /\.nav-badge \{[^}]*color: var\(--ct-on-accent, #050505\)/);
});

test('the chat keeps its stage column only with room beside the side nav', () => {
  const chat = css('chat.css');
  assert.ok(chat.includes('@media (min-width: 1320px)'));
  assert.ok(!chat.includes('@media (min-width: 1100px)'));
});
