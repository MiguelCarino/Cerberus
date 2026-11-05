// Unit tests for the Monitor's chart window, the server-side group totals,
// model-server links, focus keys and the "unknown" DAC risk.
// Run: node --test web/test/*.test.mjs
import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  chartDomain, stackedAreas, groupProcs, mergeGroups, isModelProc, focusKey, dacChip, filterGroups, sortGroups,
} from '../dist/assets/js/monitor.js';

test('chartDomain spans the selected window up to now', () => {
  const now = Date.parse('2026-11-01T14:22:00Z');
  const pts = [{ T: now - 16 * 60e3, CPU: 1 }, { T: now - 60e3, CPU: 1 }];
  assert.deepEqual(chartDomain(pts, 3600e3, now), [now - 3600e3, now]);
  // A day point starting before the window widens it to that point.
  const day = [{ T: now - 31 * 86400e3, CPU: 1 }];
  assert.equal(chartDomain(day, 30 * 86400e3, now)[0], now - 31 * 86400e3);
  assert.deepEqual(chartDomain([], 3600e3, now), [now - 3600e3, now]);
});

test('stackedAreas over a domain: the newest point sits at the right, missing time stays empty', () => {
  const now = Date.parse('2026-11-01T14:00:00Z');
  const one = stackedAreas([{ T: now, CPU: 10 }], 600, 100, undefined, [now - 7 * 86400e3, now]);
  assert.equal(one.xs[0], 600);
  const two = stackedAreas([{ T: now - 1800e3, CPU: 1 }, { T: now, CPU: 1 }], 600, 100, undefined, [now - 3600e3, now]);
  assert.deepEqual(two.xs, [300, 600]);
  // Points outside the domain are clamped into the box.
  const out = stackedAreas([{ T: now - 7200e3, CPU: 1 }, { T: now, CPU: 1 }], 600, 100, undefined, [now - 3600e3, now]);
  assert.equal(out.xs[0], 0);
});

test('mergeGroups takes totals and counts from the server, rows from the listed processes', () => {
  const listed = groupProcs([
    { PID: 10, Name: 'firefox', Group: 'firefox', CPUPct: 12, RSSMiB: 800 },
    { PID: 11, Name: 'firefox', Group: 'firefox', CPUPct: 3, RSSMiB: 400 },
  ]);
  const server = [
    { name: 'firefox', cpu: 20, rss_mib: 3000, gpu_mib: 0, count: 40, cerberus: false, session_id: '' },
    { name: 'kernel', cpu: 1, rss_mib: 0, gpu_mib: 0, count: 300, cerberus: false, session_id: '' },
  ];
  const g = mergeGroups(listed, server);
  const ff = g.find((x) => x.name === 'firefox');
  assert.equal(ff.count, 40);
  assert.equal(ff.shown, 2);
  assert.equal(ff.rss_mib, 3000);
  assert.equal(ff.procs.length, 2);
  const k = g.find((x) => x.name === 'kernel');
  assert.equal(k.shown, 0);
  assert.equal(k.count, 300);
  assert.equal(sortGroups(g, 'ram')[0].name, 'firefox');
  assert.equal(filterGroups(g, 'kern').length, 1);
  // Without server groups the listed ones stand.
  assert.equal(mergeGroups(listed, undefined)[0].shown, 2);
});

test('isModelProc: model servers link to Models; sessions and the daemon do not', () => {
  assert.equal(isModelProc({ Name: 'llama-server', Cerberus: true }), true);
  assert.equal(isModelProc({ Name: 'ollama', Cerberus: true }), true);
  assert.equal(isModelProc({ Name: 'cerberus', Cerberus: true }), false);
  assert.equal(isModelProc({ Name: 'llama-server', Cerberus: true, SessionID: 'abc' }), false);
  assert.equal(isModelProc({ Name: 'llama-server' }), false);
});

test('focusKey is stable per kind and id', () => {
  assert.equal(focusKey('m', 42), 'm:42');
  assert.equal(focusKey('g', 'firefox'), 'g:firefox');
  assert.notEqual(focusKey('m', 42), focusKey('ps', 42));
});

test('dacChip: an unknown household is never shown as no risk', () => {
  const c = dacChip('unknown', 82, 250);
  assert.equal(c.tone, 'muted');
  assert.equal(c.label, 'DAC unknown');
});
