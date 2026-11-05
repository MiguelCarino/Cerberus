// Work previews: the pure helpers behind the Preview tab and the chips.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  PRESETS, presetWidth, previewSources, pickSource, absoluteURL, previewChip, managedTone, managedRunning, emptyHint,
} from '../dist/assets/js/preview.js';
import { rowKey } from '../dist/assets/js/control.js';
import { parseRoute } from '../dist/assets/js/util.js';

test('presets: phone and tablet frame a width, desktop fills', () => {
  assert.deepEqual(PRESETS.map((p) => p.key), ['phone', 'tablet', 'desktop']);
  assert.equal(presetWidth('phone', 1200), 390);
  assert.equal(presetWidth('tablet', 1200), 768);
  assert.equal(presetWidth('desktop', 1200), 0);
  assert.equal(presetWidth('nope', 1200), 0);
  // No room for the device width: fill instead of overflowing.
  assert.equal(presetWidth('tablet', 700), 0);
  assert.equal(presetWidth('phone', 390), 0);
  assert.equal(presetWidth('phone', 0), 390);
});

const reply = {
  ports: true, detect: true,
  static: { url: '/preview/s1/', dir: 'public' },
  live: [
    { port: 5173, url: '/preview/s1/?src=5173', source: 'detected', state: 'running' },
    { port: 41000, url: '/preview/s1/?src=41000', source: 'managed', state: 'running' },
  ],
  managed: { state: 'running', command: 'npm run dev -- --port {port}' },
};

test('sources: static then live; labels and kinds', () => {
  const s = previewSources(reply);
  assert.deepEqual(s.map((x) => x.key), ['static', 'live:5173', 'live:41000']);
  assert.equal(s[0].label, 'Static (public/)');
  assert.equal(s[1].label, 'Live :5173');
  assert.equal(s[2].label, 'Run preview :41000');
  assert.equal(s[2].kind, 'managed');
  assert.deepEqual(previewSources(null), []);
  assert.equal(previewSources({ static: { url: '/preview/x/', dir: '.' }, live: [] })[0].label, 'Static');
});

test('pickSource keeps the choice, else managed, live, static', () => {
  const s = previewSources(reply);
  assert.equal(pickSource(s, 'static').key, 'static');
  assert.equal(pickSource(s, '').key, 'live:41000');
  assert.equal(pickSource(s.filter((x) => x.kind !== 'managed'), 'gone').key, 'live:5173');
  assert.equal(pickSource(s.slice(0, 1), 'live:1').key, 'static');
  assert.equal(pickSource([], 'static'), null);
});

test('absoluteURL builds a link on the app origin, no capability', () => {
  assert.equal(absoluteURL('/preview/s1/?src=5173', 'http://127.0.0.1:7340'), 'http://127.0.0.1:7340/preview/s1/?src=5173');
  assert.equal(absoluteURL('', 'http://x'), '');
  assert.ok(!absoluteURL('/preview/s1/', 'https://desk.ts.net:7343').includes('cerberus_preview'));
});

test('previewChip: live first, then static, none without a preview', () => {
  assert.equal(previewChip({ ID: 's 1' }), null);
  assert.deepEqual(previewChip({ ID: 's1', Preview: { Static: true } }), { url: '/preview/s1/', label: 'Preview' });
  assert.deepEqual(previewChip({ ID: 's 1', Preview: { Static: true, Live: [5173] } }), { url: '/preview/s%201/?src=5173', label: 'Preview :5173' });
  assert.equal(previewChip({ ID: 's1', Preview: { Managed: 'starting' } }), null);
});

test('managed state helpers', () => {
  assert.equal(managedTone('running'), 'ok');
  assert.equal(managedTone('failed'), 'err');
  assert.equal(managedTone(''), 'muted');
  assert.ok(managedRunning('starting') && managedRunning('running'));
  assert.ok(!managedRunning('stopped') && !managedRunning('exited'));
});

test('empty hint names the detected command and the missing ports', () => {
  const lines = emptyHint({ ports: false, detect: true, managed: { command: 'pnpm run dev --port {port}' } });
  assert.match(lines.join('\n'), /pnpm run dev --port \{port\}/);
  assert.match(lines.join('\n'), /preview_ports/);
  assert.match(lines.join('\n'), /Live :port/);
  assert.doesNotMatch(emptyHint({ ports: true, detect: false, managed: {} }).join('\n'), /preview_ports|Live :port/);
});

test('rowKey changes when the preview summary does', () => {
  const a = { ID: 's1', State: 'running' };
  assert.notEqual(rowKey(a), rowKey({ ...a, Preview: { Static: false, Live: [5173] } }));
  assert.notEqual(rowKey({ ...a, Preview: { Live: [1] } }), rowKey({ ...a, Preview: { Live: [2] } }));
});

test('#/s/<id>/preview opens the Preview tab', () => {
  assert.deepEqual(parseRoute('#/s/abc/preview'), { view: 'session', id: 'abc', tab: 'preview' });
});
