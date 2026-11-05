// Unit tests for the Monitor view's pure helpers: units, energy totals, the
// DAC chip, stacked chart maths, process grouping and sorting, system panel
// derivations and stream payload handling. Run: node --test web/test/*.test.mjs
import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  fmtW, fmtKWh, fmtMoney, fmtRate, fmtBytes, fmtBps, fmtPct, fmtDuration, kwhOf, dacChip,
  pointValue, pointTime, normalizeSeries, niceMax, stackedAreas, nearestIndex,
  groupProcs, sortGroups, filterGroups, lowerNice, memParts, usedPct, tempTone, diskTone, livePayload, modelEnergy, limitsOf,
} from '../dist/assets/js/monitor.js';
import { parseRoute } from '../dist/assets/js/util.js';

test('fmtW: decimals below 10 W, whole watts, kW past 1000', () => {
  assert.equal(fmtW(0), '0 W');
  assert.equal(fmtW(undefined), '0 W');
  assert.equal(fmtW(4.24), '4.2 W');
  assert.equal(fmtW(312.6), '313 W');
  assert.equal(fmtW(1210), '1.21 kW');
});

test('fmtKWh: Wh under 1 kWh, then kWh', () => {
  assert.equal(fmtKWh(0), '0 Wh');
  assert.equal(fmtKWh(0.4567), '457 Wh');
  assert.equal(fmtKWh(3.456), '3.46 kWh');
  assert.equal(fmtKWh(42.17), '42.2 kWh');
  assert.equal(fmtKWh(1234.5), '1,235 kWh');
});

test('fmtMoney: currency suffix, tiny amounts never read as free', () => {
  assert.equal(fmtMoney(0), '$0.00 MXN');
  assert.equal(fmtMoney(0.004), '<$0.01 MXN');
  assert.equal(fmtMoney(12.345), '$12.35 MXN');
  assert.equal(fmtMoney(2500.4), '$2,500 MXN');
  assert.equal(fmtMoney(3, ''), '$3.00');
  assert.equal(fmtRate(4.054), '$4.054/kWh');
  assert.equal(fmtRate(0), '–');
});

test('fmtBytes and fmtBps use powers of 1000; idle under 1 B/s', () => {
  assert.equal(fmtBytes(0), '0 B');
  assert.equal(fmtBytes(999), '999 B');
  assert.equal(fmtBytes(1.9e12), '1.9 TB');
  assert.equal(fmtBytes(7.3e12), '7.3 TB');
  assert.equal(fmtBps(0.5), 'idle');
  assert.equal(fmtBps(12_300_000), '12.3 MB/s');
  assert.equal(fmtPct(33.6), '34%');
  assert.equal(fmtPct(undefined), '–');
});

test('fmtDuration renders Go nanosecond durations', () => {
  assert.equal(fmtDuration(0), '–');
  assert.equal(fmtDuration(5 * 60e9), '5m');
  assert.equal(fmtDuration((3 * 3600 + 7 * 60) * 1e9), '3h 7m');
  assert.equal(fmtDuration((2 * 86400 + 5 * 3600) * 1e9), '2d 5h');
  assert.equal(fmtDuration('1h2m3s'), '1h2m3s');
});

test('kwhOf reads energy.Totals entries (wall kWh) and plain numbers', () => {
  // As GET /api/monitor serialises energy.Totals.
  const totals = { Today: { CPU: 0.01, GPU: 0.002, Rest: 0.03, Wall: 0.05 }, Last30Days: { CPU: 0, GPU: 0, Rest: 0, Wall: 0 },
    ProjectedMonthKWh: 14.2, Loaded: true };
  assert.equal(kwhOf(totals.Today), 0.05);
  assert.equal(kwhOf(totals.Last30Days), 0);
  assert.equal(kwhOf(totals.ProjectedMonthKWh), 14.2);
  assert.equal(kwhOf(undefined), 0);
  assert.equal(kwhOf(NaN), 0);
  assert.equal(kwhOf({ CPU: 1 }), 0);
});

test('limitsOf uses the configured alert limits, defaults for missing or zero', () => {
  assert.deepEqual(limitsOf(undefined), { disk_pct: 90, cpu_temp_c: 90, gpu_temp_c: 85, nvme_temp_c: 70 });
  const l = limitsOf({ disk_pct: 80, cpu_temp_c: 0, gpu_temp_c: 75, nvme_temp_c: 65, high_power_w: 600 });
  assert.deepEqual(l, { disk_pct: 80, cpu_temp_c: 90, gpu_temp_c: 75, nvme_temp_c: 65 });
});

test('dacChip maps every risk to a tone', () => {
  assert.equal(dacChip('none').tone, 'ok');
  assert.equal(dacChip('near').tone, 'warn');
  assert.equal(dacChip('pushes').tone, 'err');
  assert.equal(dacChip('already').tone, 'err');
  assert.equal(dacChip(undefined).tone, 'muted');
  assert.match(dacChip('near', 240, 250).text, /240 of 250 kWh/);
});

test('series points: the API shape {T, CPU, GPU, Rest, Wall}, sorted, invalid dropped', () => {
  const raw = [
    { T: '2026-11-01T10:02:00-06:00', CPU: 31.5, GPU: 18, Rest: 40, Wall: 104.4 },
    { T: 'bad', CPU: 1 },
    { T: '2026-11-01T10:01:00-06:00', CPU: 30, GPU: 17.9, Rest: 40, Wall: 102.2 },
  ];
  const s = normalizeSeries(raw);
  assert.equal(s.length, 2);
  assert.ok(pointTime(s[0]) < pointTime(s[1]));
  assert.equal(pointTime(s[0]), Date.parse('2026-11-01T16:01:00Z'));
  assert.equal(pointValue(s[1], 'CPU'), 31.5);
  assert.equal(pointValue(s[1], 'Wall'), 104.4);
  assert.equal(pointValue({ Rest: -3 }, 'Rest'), 0);
  assert.equal(pointValue({ CPU: null }, 'CPU'), 0);
  assert.equal(pointValue(null, 'CPU'), 0);
  assert.deepEqual(normalizeSeries(null), []);
  assert.deepEqual(normalizeSeries({ points: raw }), []);
});

test('niceMax rounds up to 1/2/5 × 10^n', () => {
  assert.equal(niceMax(0), 10);
  assert.equal(niceMax(7), 10);
  assert.equal(niceMax(130), 200);
  assert.equal(niceMax(420), 500);
  assert.equal(niceMax(501), 1000);
});

test('stackedAreas stacks CPU, GPU, Rest and scales to the nice max', () => {
  const t = Date.parse('2026-11-01T10:00:00Z');
  const pts = [
    { T: new Date(t).toISOString(), CPU: 50, GPU: 100, Rest: 50, Wall: 230 },
    { T: new Date(t + 60e3).toISOString(), CPU: 100, GPU: 250, Rest: 50, Wall: 460 },
    { T: new Date(t + 120e3).toISOString(), CPU: 50, GPU: 50, Rest: 50, Wall: 172 },
  ];
  const a = stackedAreas(pts, 600, 100);
  assert.equal(a.max, 500);
  assert.deepEqual(a.totals, [200, 400, 150]);
  assert.deepEqual(a.xs, [0, 300, 600]);
  // CPU top at the first point: 50 of 500 → y = 90; rest top (total 200) → y = 60.
  assert.equal(a.tops.CPU[0], 90);
  assert.equal(a.tops.Rest[0], 60);
  assert.ok(a.paths.CPU.startsWith('M0,90'));
  assert.ok(a.paths.GPU.endsWith('Z'));
});

test('stackedAreas: time gaps keep their width; one point is a bar; none is empty', () => {
  const t = Date.parse('2026-11-01T10:00:00Z');
  const gap = stackedAreas([{ T: t, CPU: 1 }, { T: t + 60e3, CPU: 1 }, { T: t + 600e3, CPU: 1 }], 600, 100);
  assert.equal(gap.xs[1], 60);
  const one = stackedAreas([{ T: t, CPU: 10 }], 600, 100);
  assert.match(one.paths.CPU, /^M298,/);
  const none = stackedAreas([], 600, 100);
  assert.equal(none.paths.CPU, '');
});

test('nearestIndex finds the closest x', () => {
  const xs = [0, 100, 200, 300];
  assert.equal(nearestIndex(xs, -5), 0);
  assert.equal(nearestIndex(xs, 149), 1);
  assert.equal(nearestIndex(xs, 151), 2);
  assert.equal(nearestIndex(xs, 999), 3);
  assert.equal(nearestIndex([], 5), -1);
});

const PROCS = [
  { PID: 10, Name: 'firefox', Group: 'firefox', CPUPct: 12, RSSMiB: 800 },
  { PID: 11, Name: 'firefox', Group: 'firefox', CPUPct: 3, RSSMiB: 400, Protected: false },
  { PID: 20, Name: 'llama-server', Group: 'lmstudio', CPUPct: 40, RSSMiB: 2000, GPUMiB: 16000, Cerberus: true },
  { PID: 30, Name: 'bash', Group: 'cerberus-session-abc', CPUPct: 1, RSSMiB: 5, Cerberus: true, SessionID: 'abc' },
  { PID: 1, Name: 'systemd', Group: '', CPUPct: 0, RSSMiB: 12, Protected: true },
];

test('groupProcs sums by group, keeps Cerberus/session flags and protection', () => {
  const g = groupProcs(PROCS);
  const ff = g.find((x) => x.name === 'firefox');
  assert.equal(ff.count, 2);
  assert.equal(ff.cpu, 15);
  assert.equal(ff.rss_mib, 1200);
  assert.equal(ff.procs[0].PID, 10);
  assert.equal(ff.protected, false);
  const sess = g.find((x) => x.name === 'cerberus-session-abc');
  assert.equal(sess.session_id, 'abc');
  assert.equal(sess.cerberus, true);
  assert.equal(g.find((x) => x.name === 'systemd').protected, true);
});

test('sortGroups by cpu, ram, gpu and name; filterGroups by name, command or pid', () => {
  const g = groupProcs(PROCS);
  assert.equal(sortGroups(g, 'cpu')[0].name, 'lmstudio');
  assert.equal(sortGroups(g, 'ram')[0].name, 'lmstudio');
  assert.equal(sortGroups(g, 'gpu')[0].name, 'lmstudio');
  assert.equal(sortGroups(g, 'name')[0].name, 'cerberus-session-abc');
  assert.deepEqual(filterGroups(g, 'llama').map((x) => x.name), ['lmstudio']);
  assert.deepEqual(filterGroups(g, '11').map((x) => x.name), ['firefox']);
  assert.equal(filterGroups(g, '').length, g.length);
});

test('lowerNice adds 5 and caps at 19', () => {
  assert.equal(lowerNice(0), 5);
  assert.equal(lowerNice(16), 19);
  assert.equal(lowerNice(undefined), 5);
  assert.equal(lowerNice(-5), 5);
});

test('memParts, usedPct, tempTone and diskTone', () => {
  const m = memParts({ TotalMiB: 32000, AvailMiB: 8000, SwapTotalMiB: 8192, SwapUsedMiB: 0, ZramOrigMiB: 300, ZramComprMiB: 100 });
  assert.equal(m.used, 24000);
  assert.equal(m.pct, 75);
  assert.equal(m.zramRatio, 3);
  assert.equal(usedPct(5, 0), 0);
  assert.equal(usedPct(150, 100), 100);
  assert.equal(tempTone(50, 90), 'ok');
  assert.equal(tempTone(82, 90), 'warn');
  assert.equal(tempTone(91, 90), 'err');
  assert.equal(tempTone(0, 90), 'muted');
  assert.equal(diskTone(50), 'ok');
  assert.equal(diskTone(85), 'warn');
  assert.equal(diskTone(92), 'err');
});

test('livePayload reads the stream event (the GET /api/monitor object)', () => {
  assert.equal(livePayload(null), null);
  assert.equal(livePayload({ nope: 1 }), null);
  assert.equal(livePayload({ CPU: { Pct: 3 } }), null);
  const ev = livePayload({ snapshot: { CPU: { Pct: 3 } }, power: { CPUW: 30, GPUW: 18, RestW: 40, WallW: 102, CPUSource: 'model', Estimated: true },
    totals: { Today: { Wall: 0.1 } }, bill: { marginal: { PerKWh: 1.3 }, currency: 'MXN' }, alerts: { disk_pct: 85 } });
  assert.equal(ev.power.WallW, 102);
  assert.equal(ev.bill.marginal.PerKWh, 1.3);
  assert.equal(ev.alerts.disk_pct, 85);
  assert.equal(livePayload({ snapshot: { Power: { WallW: 7 } } }).power.WallW, 7);
});

test('modelEnergy reads engine.ModelInfo energy and cost fields', () => {
  assert.deepEqual(modelEnergy({}), { j24: 0, jMonth: 0, cost24: 0, costMonth: 0 });
  assert.deepEqual(modelEnergy(undefined), { j24: 0, jMonth: 0, cost24: 0, costMonth: 0 });
  assert.deepEqual(modelEnergy({ EnergyJ24h: 3600, EnergyJMonth: 7200, CostMXN24h: 1, CostMXNMonth: 2 }), { j24: 3600, jMonth: 7200, cost24: 1, costMonth: 2 });
  assert.equal(modelEnergy({ EnergyJ: 10 }).j24, 0);
});

test('parseRoute knows #/monitor', () => {
  assert.deepEqual(parseRoute('#/monitor'), { view: 'monitor' });
});
