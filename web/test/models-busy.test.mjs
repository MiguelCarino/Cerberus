// The Models view's load line (gateway phase G5).
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { busyText } from '../dist/assets/js/models.js';

test('busyText', () => {
  assert.equal(busyText({}), '');
  assert.equal(busyText({ BusyForS: 30 }), '', 'a short request is not news');
  assert.equal(busyText({ BusyForS: 75 }), 'busy for 75 s');
  assert.equal(busyText({ BusyForS: 400, Waiting: 2, LongestWaitS: 12.4 }), 'busy for 7 min · 2 queued, oldest 12 s');
  assert.equal(busyText({ Waiting: 1, LongestWaitS: 0.2 }), '1 queued');
  assert.equal(busyText({ Stalls: 1 }), '1 stall since start');
  assert.equal(busyText({ Stalls: 3 }), '3 stalls since start');
});
