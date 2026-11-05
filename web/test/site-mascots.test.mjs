// The product page (site/) runs byte copies of the app's mascot modules, and
// its stage lists the same themes as mascots/index.js.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { THEMES, NAMES } from '../dist/assets/js/mascots/index.js';

const here = path.dirname(fileURLToPath(import.meta.url));
const app = path.join(here, '../dist/assets/js/mascots');
const site = path.join(here, '../../site/mascots');

test('site/mascots holds byte copies of the app mascots', () => {
  const want = fs.readdirSync(app).filter((f) => f.endsWith('.js')).sort();
  const have = fs.readdirSync(site).filter((f) => f.endsWith('.js')).sort();
  assert.deepEqual(have, want, 'copy with: cp web/dist/assets/js/mascots/*.js site/mascots/');
  for (const f of want) {
    assert.ok(fs.readFileSync(path.join(site, f)).equals(fs.readFileSync(path.join(app, f))), `${f} differs: cp web/dist/assets/js/mascots/*.js site/mascots/`);
  }
});

test('the page stage lists the same themes and names', () => {
  const src = fs.readFileSync(path.join(here, '../../site/mascot-stage.js'), 'utf8');
  const block = src.match(/const NAMES = \{([\s\S]*?)\};/)[1];
  const names = Object.fromEntries([...block.matchAll(/'?([\w-]+)'?:\s*'([^']+)'/g)].map((m) => [m[1], m[2]]));
  assert.deepEqual(Object.keys(names), THEMES);
  assert.deepEqual(names, NAMES);
});
