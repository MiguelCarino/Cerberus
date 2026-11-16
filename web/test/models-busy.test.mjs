// The Models view's load line (gateway phase G5).
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { busyText, failureText, prefillText, noProgressNote, mergeLive, reserveText, withSince, tickBusy } from '../dist/assets/js/models.js';

test('busyText', () => {
  assert.equal(busyText({}), '');
  assert.equal(busyText({ BusyForS: 30 }), '', 'a short request is not news');
  assert.equal(busyText({ BusyForS: 75 }), 'busy for 75 s');
  assert.equal(busyText({ BusyForS: 400, Waiting: 2, LongestWaitS: 12.4 }), 'busy for 7 min · 2 queued, oldest 12 s');
  assert.equal(busyText({ Waiting: 1, LongestWaitS: 0.2 }), '1 queued');
  assert.equal(busyText({ Stalls: 1 }), '1 stall since start');
  assert.equal(busyText({ Stalls: 3 }), '3 stalls since start');
});

test('failureText', () => {
  const now = Date.parse('2026-11-15T12:18:07Z');
  assert.equal(failureText({}), '');
  assert.equal(failureText({ LastFailure: { Time: '2026-11-15T12:15:07Z', Cause: 'railguard-vram', Text: 'ended by Railguard: VRAM 97% full' } }, now),
    'Ended by Railguard: VRAM 97% full (3 min ago)');
  assert.equal(failureText({ LastFailure: { Time: '2026-11-15T12:18:00Z', Text: 'killed by SIGTERM' } }, now), 'Killed by SIGTERM (7 s ago)');
  assert.equal(failureText({ LastFailure: { Time: '2026-11-13T12:18:00Z', Text: 'old' } }, now), '', 'older than a day is not shown');
});

test('prefillText: reading prompt P/T (web-api-ops-8)', () => {
  assert.equal(prefillText(null), '');
  assert.equal(prefillText({ processed: 12000, total: 48000 }), 'reading prompt 12k/48k');
  assert.equal(prefillText({ Processed: 1500, Total: 9000 }), 'reading prompt 1.5k/9k');
  assert.equal(prefillText({ processed: 512, total: 900 }), 'reading prompt 512/900');
  assert.equal(prefillText({ processed: 48000, total: 48000 }), '', 'read: not shown');
  assert.equal(prefillText({ processed: 1, total: 9, done: true }), '');
  assert.equal(busyText({ BusyForS: 75, Prefill: { Processed: 12000, Total: 48000 } }), 'reading prompt 12k/48k · busy for 75 s');
  assert.equal(noProgressNote({ Backend: 'lmstudio', InFlight: 1 }), 'LM Studio gives no prompt progress');
  assert.equal(noProgressNote({ Backend: 'llamacpp', InFlight: 1 }), '');
  assert.equal(busyText({ Backend: 'ollama', InFlight: 1, BusyForS: 75 }), 'busy for 75 s · Ollama gives no prompt progress');
});

test('mergeLive applies a models event (web-api-ops-6)', () => {
  const now = Date.parse('2026-11-15T12:00:00Z');
  const list = [{ ID: 'qwen', Backend: 'llamacpp', SizeMiB: 9000, Loaded: false }, { ID: 'cloud/x', Backend: 'openai' }];
  const r = mergeLive(list, [{ id: 'qwen', loaded: true, in_flight: 1, max_parallel: 2, waiting: 1,
    wait_since: '2026-11-15T11:59:50Z', busy_since: '2026-11-15T11:58:00Z', stalls: 0, last_used: '2026-11-15T11:58:00Z' }], now);
  assert.equal(r.unknown, false);
  const q = r.models[0];
  assert.equal(q.Loaded, true);
  assert.equal(q.SizeMiB, 9000, 'static fields kept');
  assert.equal(q.InFlight, 1);
  assert.equal(q.BusyForS, 120);
  assert.equal(q.LongestWaitS, 10);
  assert.equal(r.models[1], list[1], 'a model not in the event is unchanged');
  assert.equal(mergeLive(list, [{ id: 'new-model', loaded: false }]).unknown, true);
  // Ticking from the stamps.
  const t = tickBusy(r.models, now + 60000);
  assert.equal(t[0].BusyForS, 180);
  const s = withSince([{ ID: 'a', BusyForS: 30 }], now);
  assert.equal(tickBusy(s, now + 30000)[0].BusyForS, 60);
});

test('mergeLive clears a cloud model that left the event (web-api-2)', () => {
  const now = Date.parse('2026-11-15T12:00:00Z');
  const list = [{ ID: 'cloud/x', Backend: 'openai', Local: false }, { ID: 'qwen', Backend: 'llamacpp', Local: true, InFlight: 1, BusySince: '2026-11-15T11:59:00Z' }];
  let r = mergeLive(list, [{ id: 'cloud/x', in_flight: 1, busy_since: '2026-11-15T11:59:00Z' }], now);
  assert.equal(r.models[0].InFlight, 1);
  // The request ended: the next event no longer lists the cloud model.
  r = mergeLive(r.models, [], now + 1000);
  const c = r.models[0];
  assert.equal(c.InFlight, 0);
  assert.equal(c.BusySince, '');
  assert.equal(busyText(tickBusy(r.models, now + 600000)[0]), '', 'not busy 10 minutes later');
  assert.equal(r.models[1].InFlight, 1, 'a local model missing from an event is left as it was');
});

test('reserveText shows the VRAM reserve in effect', () => {
  assert.equal(reserveText({ mib: 1505, source: 'railguard', line_pct: 97 }), 'Railguard line 97% · reserve 1505 MiB');
  assert.equal(reserveText({ mib: 2048, source: 'fixed' }), 'VRAM reserve 2048 MiB (set in Settings)');
  assert.equal(reserveText({ mib: 1024, source: 'default' }), 'VRAM reserve 1024 MiB (no Railguard)');
  assert.equal(reserveText({ source: 'off' }), 'VRAM reserve off');
  assert.equal(reserveText(null), '');
});
