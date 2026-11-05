// Monitor view: wall power and its split, energy and the bill, power charts,
// the system panels and the task manager. Live through /api/monitor/stream
// only while this view is open and the tab visible (the stream holds the
// daemon in active sampling; closing it lets the daemon fall back to idle).

import { api, post, LiveSource } from '../api.js';
import { h, clear, toast, errText, pill, openDialog, confirmDialog } from '../dom.js';
import { fmtMiB } from '../util.js';
import {
  fmtW, fmtKWh, fmtMoney, fmtRate, fmtBytes, fmtBps, fmtPct, fmtDuration, kwhOf, dacChip,
  SERIES, RANGES, normalizeSeries, stackedAreas, chartDomain, nearestIndex, pointValue, pointTime, fmtPointTime,
  groupProcs, mergeGroups, isModelProc, focusKey, sortGroups, filterGroups, lowerNice, memParts, usedPct, tempTone, diskTone,
  livePayload, limitsOf,
} from '../monitor.js';
import * as usageView from './usage.js';
import { doctorCard } from './security.js';
import { store } from '../store.js';

const SVG = 'http://www.w3.org/2000/svg';
const CHART_W = 600;
const CHART_H = 160;
const PROC_MIN_MS = 3000;   // process list refresh, piggybacked on stream ticks
const SERIES_MS = 60000;    // chart refresh while visible

function s(tag, attrs = {}, ...children) {
  const el = document.createElementNS(SVG, tag);
  for (const [k, v] of Object.entries(attrs)) if (v != null) el.setAttribute(k, v);
  for (const c of children) if (c) el.append(c);
  return el;
}

export function mount(root) {
  const state = { data: null, series: [], range: RANGES[0], procs: [], groups: [], sort: 'cpu', q: '', open: new Set() };

  // ---------- skeleton ----------
  const overviewEl = h('div', { class: 'mon-overview' }, h('p', { class: 't-sec', text: 'Loading…' }));
  const billEl = h('div', { class: 'mon-bill' });
  const rangeBtns = h('div', { class: 'mon-ranges', role: 'tablist' }, RANGES.map((r) =>
    h('button', { class: 'fchip', type: 'button', role: 'tab', 'data-range': r.key, onclick: () => setRange(r) }, r.label)));
  const chartEl = h('div', { class: 'mon-chart' });
  const legendEl = h('div', { class: 'mon-legend' }, SERIES.map((x) =>
    h('span', { class: `mon-key k-${x.key.toLowerCase()}` }, h('i', {}), x.label)));
  const systemEl = h('div', { class: 'mon-system' });
  const disksEl = h('div', { class: 'mon-disks' });
  const search = h('input', { class: 'input mon-search', type: 'search', placeholder: 'Filter by app, command, PID', oninput: (e) => { state.q = e.target.value; renderTasks(); } });
  const sortSel = h('select', { class: 'select mon-sort', 'aria-label': 'Sort tasks', onchange: (e) => { state.sort = e.target.value; renderTasks(); } },
    [['cpu', 'CPU'], ['ram', 'Memory'], ['gpu', 'GPU'], ['name', 'Name']].map(([v, t]) => h('option', { value: v, text: `Sort: ${t}` })));
  const tasksEl = h('div', { class: 'mon-tasks' }, h('p', { class: 't-sec', text: 'Loading tasks…' }));
  const tasksNote = h('p', { class: 'mon-note' });
  // Phones: five tabs do not fit, so Usage lives here as a section (CSS
  // shows it only under 640 px). Its view mounts only while it is open, so
  // a closed section polls nothing.
  const usageBody = h('div', { class: 'mon-usage-body' });
  let usageCleanup = null;
  // Users and roles: Usage is for roles that see spend; process actions
  // (signal, renice) are for admins.
  const canUsage = store.can('usage');
  const canSignal = store.can('monitor.signal');
  const usageEl = h('details', { class: 'mon-usage', hidden: !canUsage },
    h('summary', { class: 'mon-usage-sum' }, h('span', { class: 'group-title', text: 'Usage & spend' })),
    usageBody);
  usageEl.addEventListener('toggle', () => {
    if (usageEl.open && !usageCleanup && !gone) {
      clear(usageBody);
      usageCleanup = usageView.mount(usageBody) || (() => {});
    } else if (!usageEl.open && usageCleanup) {
      usageCleanup();
      usageCleanup = null;
      clear(usageBody);
    }
  });

  // The doctor's ok/warn/fail counts; tap for the checklist.
  const doctor = doctorCard();

  root.append(h('section', { class: 'view view-pad mon' },
    h('div', { class: 'view-head' },
      h('div', {}, h('h1', { class: 'view-title', text: 'Monitor' }),
        h('p', { class: 'view-sub', text: 'Power, energy, cost and everything running on this machine.' })),
      canUsage ? h('a', { class: 'btn mon-usage-link', href: '#/usage' }, 'Usage & spend →') : null),
    overviewEl, doctor.el, billEl, usageEl,
    h('div', { class: 'mon-chart-head' }, h('h2', { class: 'group-title', text: 'Power' }), rangeBtns),
    h('div', { class: 'mon-chart-card' }, chartEl, legendEl),
    h('h2', { class: 'group-title', text: 'System' }), systemEl,
    h('h2', { class: 'group-title', text: 'Drives' }), disksEl,
    h('div', { class: 'mon-tasks-head' }, h('h2', { class: 'group-title', text: 'Tasks' }),
      h('button', { class: 'btn', type: 'button', onclick: () => openTasks() }, 'Show tasks…')),
    h('p', { class: 'mon-note', text: 'Every process on this machine, grouped by app, with CPU, memory and GPU. The list is read only while it is open.' })));

  let gone = false;
  let seriesTimer = 0;
  let seriesGen = 0; // the newest loadSeries call
  let lastProcs = 0;
  let procBusy = false;
  let hideTip = null; // the chart's tooltip hider (set by renderChart)

  // ---------- data ----------
  async function loadAll() {
    try {
      const d = await api('/api/monitor');
      if (gone) return;
      state.data = d;
      renderAll();
    } catch (e) {
      if (!gone) clear(overviewEl).append(h('div', { class: 'alert err', text: `Monitor unavailable: ${errText(e)}` }));
    }
    loadSeries();
  }

  // ---------- tasks popup ----------
  // The process table opens in a popup. Processes are read only while it is
  // open (every few seconds, on the stream's ticks), never in the background.
  let tasksOpen = false;
  let closeTasks = null;
  function openTasks() {
    if (tasksOpen) return;
    tasksOpen = true;
    loadProcs();
    openDialog((close) => {
      closeTasks = close;
      return {
        title: 'Tasks',
        sub: 'Everything running on this machine, grouped by app',
        wide: true,
        body: h('div', { class: 'mon-tasks-pop' }, h('div', { class: 'mon-tools' }, search, sortSel), tasksEl, tasksNote),
      };
    }).then(() => { tasksOpen = false; closeTasks = null; });
  }

  async function loadProcs() {
    if (procBusy || gone) return;
    procBusy = true;
    lastProcs = Date.now();
    try {
      const d = await api('/api/monitor/processes');
      if (gone) return;
      state.procs = (d && d.processes) || [];
      state.groups = mergeGroups(groupProcs(state.procs), d && d.groups);
      renderTasks();
    } catch (e) {
      if (!gone) clear(tasksEl).append(h('div', { class: 'alert err', text: errText(e) }));
    } finally {
      procBusy = false;
    }
  }

  // loadSeries fetches the chart and schedules the next fetch. Only the
  // newest call schedules (seriesGen), so a hide/show or a range change
  // while a fetch is in flight never leaves a second loop running.
  async function loadSeries() {
    clearTimeout(seriesTimer);
    seriesTimer = 0;
    if (gone || document.hidden) return;
    const gen = ++seriesGen;
    const r = state.range;
    try {
      const since = new Date(Date.now() - r.ms).toISOString();
      const raw = await api(`/api/monitor/series?res=${r.res}&since=${encodeURIComponent(since)}`);
      if (gone || gen !== seriesGen || r !== state.range) return;
      state.series = normalizeSeries(raw);
      renderChart();
    } catch (e) {
      if (gone || gen !== seriesGen) return;
      clear(chartEl).append(h('p', { class: 't-sec', text: `No history yet (${errText(e)}).` }));
    }
    clearTimeout(seriesTimer);
    if (!gone && !document.hidden) seriesTimer = setTimeout(loadSeries, SERIES_MS);
  }

  function setRange(r) {
    state.range = r;
    renderRanges();
    clear(chartEl).append(h('p', { class: 't-sec', text: 'Loading…' }));
    loadSeries();
  }

  const stream = new LiveSource('/api/monitor/stream', {
    snapshot: (raw) => {
      let d;
      try { d = JSON.parse(raw); } catch { return; }
      const live = livePayload(d);
      if (!live || gone) return;
      const prev = state.data || {};
      state.data = { ...prev, snapshot: live.snapshot, power: live.power || prev.power,
        totals: live.totals || prev.totals, bill: live.bill || prev.bill, alerts: live.alerts || prev.alerts };
      renderOverview();
      renderBill();
      renderSystem();
      if (tasksOpen && Date.now() - lastProcs >= PROC_MIN_MS && !document.hidden) loadProcs();
    },
  }).start();

  // ---------- rendering ----------
  function renderAll() {
    renderOverview();
    renderBill();
    renderRanges();
    renderSystem();
  }

  function renderRanges() {
    for (const b of rangeBtns.children) {
      const on = b.dataset.range === state.range.key;
      b.setAttribute('aria-pressed', String(on));
      b.setAttribute('aria-selected', String(on));
    }
  }

  function renderOverview() {
    const d = state.data || {};
    const p = d.power || (d.snapshot && d.snapshot.Power) || {};
    const t = d.totals || {};
    const b = d.bill || {};
    const cur = b.currency || 'MXN';
    const measured = p.CPUSource === 'rapl' && !p.Estimated;
    const gpuW = +p.GPUW || 0;
    const wall = +p.WallW || 0;
    const split = [['CPU', p.CPUW, 'cpu'], ['GPU', p.GPUW, 'gpu'], ['Rest', p.RestW, 'rest']];
    const sum = split.reduce((a, [, w]) => a + (+w || 0), 0);
    clear(overviewEl).append(
      h('div', { class: 'mon-power' },
        h('div', { class: 'mon-wall' },
          h('span', { class: 'mon-wall-lbl', text: 'At the wall' }),
          h('span', { class: 'mon-wall-val num', text: fmtW(wall) }),
          h('span', { class: 'mon-wall-badges' },
            pill(measured ? 'CPU measured' : 'CPU estimated', measured ? 'ok' : 'warn'),
            gpuW > 0 ? pill('GPU measured', 'ok') : null,
            pill('rest estimated', 'muted'))),
        h('div', { class: 'mon-split' },
          h('div', { class: 'mon-splitbar', role: 'img', 'aria-label': split.map(([n, w]) => `${n} ${fmtW(+w || 0)}`).join(', ') },
            split.map(([n, w, k]) => h('span', { class: `seg k-${k}`, style: `flex-basis:${sum > 0 ? ((+w || 0) / sum) * 100 : 0}%`, title: `${n} ${fmtW(+w || 0)}` }))),
          h('div', { class: 'mon-splitlegend' }, split.map(([n, w, k]) =>
            h('span', { class: `mon-key k-${k}` }, h('i', {}), `${n} `, h('b', { class: 'num', text: fmtW(+w || 0) })))))),
      h('div', { class: 'mon-tiles' },
        tile('Today', fmtKWh(kwhOf(t.Today)), fmtMoney(b.today_cost, cur)),
        tile('This month', fmtKWh(kwhOf(t.MonthToDate)), fmtMoney(b.month_cost, cur)),
        tile('Projected month', fmtKWh(kwhOf(t.ProjectedMonthKWh)), fmtMoney(b.projected_month_cost, cur), 'accent'),
        tile('Last 30 days', fmtKWh(kwhOf(t.Last30Days)), d.snapshot ? `up ${fmtDuration(d.snapshot.Uptime)}` : '')));
  }

  function renderBill() {
    const b = (state.data && state.data.bill) || {};
    const m = b.marginal || {};
    const chip = dacChip(m.DACRisk, m.DACAvgKWh, b.dac_limit_kwh);
    clear(billEl).append(
      h('div', { class: 'mon-bill-card' },
        h('div', { class: 'mon-bill-main' },
          h('span', { class: 'mon-bill-lbl', text: b.tariff_name || 'Tariff' }),
          h('span', { class: 'mon-bill-rate num', text: fmtRate(m.PerKWh) }),
          h('span', { class: 't-sm t-sec', text: m.TierName ? `marginal price · ${m.TierName} tier` : 'marginal price' })),
        h('div', { class: 'mon-bill-facts' },
          fact('This PC', `${fmtKWh(m.PCKWhMonth)}/month`),
          fact('Household', m.HouseholdSet ? `${fmtKWh(m.HouseholdKWh)}/month` : 'not set (PC alone)'),
          fact('PC adds to the bill', `${fmtMoney(m.CostMonth, b.currency || 'MXN')}/month`)),
        h('div', { class: 'mon-bill-dac' }, pill(chip.label, chip.tone), h('span', { class: 't-sm t-sec', text: chip.text }))),
      // Element.append would print a null child as the text "null".
      ...(b.verified ? [] : [h('div', { class: 'alert warn mon-approx', text: 'Approximate tariff: prices change often. Check the tiers against your receipt, then set household_kwh_month and "verified": true under tariff in the config.' })]));
  }

  function renderChart() {
    clear(chartEl);
    const pts = state.series;
    if (!pts.length) { chartEl.append(h('p', { class: 't-sec mon-empty', text: 'No power history for this range yet. It fills in as Cerberus runs.' })); return; }
    const domain = chartDomain(pts, state.range.ms);
    const a = stackedAreas(pts, CHART_W, CHART_H, undefined, domain);
    const grid = [0.25, 0.5, 0.75].map((f) => s('line', { x1: 0, x2: CHART_W, y1: CHART_H * f, y2: CHART_H * f, class: 'mon-grid' }));
    const areas = SERIES.map((x) => s('path', { d: a.paths[x.key], class: `mon-area k-${x.key.toLowerCase()}` }));
    const cursor = s('line', { x1: 0, x2: 0, y1: 0, y2: CHART_H, class: 'mon-cursor', visibility: 'hidden' });
    const svg = s('svg', { viewBox: `0 0 ${CHART_W} ${CHART_H}`, preserveAspectRatio: 'none', class: 'mon-svg', role: 'img',
      'aria-label': `Power over the last ${state.range.label}, up to ${fmtW(Math.max(...a.totals))}` }, ...grid, ...areas, cursor);
    const tip = h('div', { class: 'mon-tip', hidden: true });
    const axis = h('div', { class: 'mon-axis' }, h('span', { text: fmtW(a.max) }), h('span', { text: fmtW(a.max / 2) }), h('span', { text: '0 W' }));
    const times = h('div', { class: 'mon-times' },
      h('span', { text: fmtPointTime(domain[0], state.range.res) }),
      h('span', { text: 'now' }));
    const wrap = h('div', { class: 'mon-plot' }, svg, tip);

    const show = (clientX) => {
      const r = svg.getBoundingClientRect();
      if (!r.width) return;
      const px = ((clientX - r.left) / r.width) * CHART_W;
      const i = nearestIndex(a.xs, px);
      if (i < 0) return;
      const p = pts[i];
      const x = a.xs[i];
      cursor.setAttribute('x1', x); cursor.setAttribute('x2', x); cursor.setAttribute('visibility', 'visible');
      clear(tip).append(h('b', { text: fmtPointTime(pointTime(p), state.range.res) }),
        ...SERIES.map((k) => h('span', { class: `mon-key k-${k.key.toLowerCase()}` }, h('i', {}), `${k.label} ${fmtW(pointValue(p, k.key))}`)),
        h('span', { text: `At the wall ${fmtW(pointValue(p, 'Wall') || a.totals[i])}` }));
      tip.hidden = false;
      const left = (x / CHART_W) * r.width;
      tip.style.left = `${Math.min(Math.max(left, 70), r.width - 70)}px`;
    };
    const hide = () => { cursor.setAttribute('visibility', 'hidden'); tip.hidden = true; };
    hideTip = hide;
    wrap.addEventListener('pointermove', (e) => show(e.clientX));
    wrap.addEventListener('pointerdown', (e) => show(e.clientX));
    // A touch fires pointerleave right after the finger lifts: keep the
    // values up after a tap (a tap outside the chart hides them).
    wrap.addEventListener('pointerleave', (e) => { if (e.pointerType !== 'touch') hide(); });
    chartEl.append(h('div', { class: 'mon-chart-grid' }, axis, wrap), times,
      ...(state.range.res === 'minute' ? []
        : [h('p', { class: 'mon-note', text: 'Hourly and daily averages count time Cerberus was not running as 0 W.' })]));
  }

  function renderSystem() {
    const snap = (state.data && state.data.snapshot) || null;
    if (!snap) { clear(systemEl).append(h('p', { class: 't-sec', text: 'Waiting for the first sample…' })); return; }
    const lim = limitsOf(state.data.alerts);
    const cpu = snap.CPU || {};
    const mem = memParts(snap.Mem);
    const gpus = (snap.GPU && snap.GPU.GPUs) || [];
    const nets = (snap.Net || []).filter((n) => n && n.Name);

    const cores = (cpu.Cores || []).map((c, i) => h('span', { class: 'core', title: `Thread ${i}: ${fmtPct(c)}${cpu.MHz && cpu.MHz[i] ? ` · ${cpu.MHz[i]} MHz` : ''}` },
      h('span', { class: 'core-fill', style: `height:${Math.max(2, Math.min(100, +c || 0))}%` })));
    const mhz = (cpu.MHz || []).filter((x) => x > 0);
    const avgMHz = mhz.length ? Math.round(mhz.reduce((a, b) => a + b, 0) / mhz.length) : 0;

    clear(systemEl).append(
      card('CPU', cpu.Model || '', [
        stat('Load', fmtPct(cpu.Pct), (cpu.Load || []).length ? `avg ${(cpu.Load || []).map((l) => (+l).toFixed(1)).join(' ')}` : ''),
        stat('Temp', cpu.TempC ? `${Math.round(cpu.TempC)} °C` : '–', '', tempTone(cpu.TempC, lim.cpu_temp_c)),
        stat('Clock', avgMHz ? `${(avgMHz / 1000).toFixed(2)} GHz` : '–', mhz.length ? `max ${(Math.max(...mhz) / 1000).toFixed(2)}` : ''),
      ], h('div', { class: 'cores', role: 'img', 'aria-label': `${(cpu.Cores || []).length} threads` }, cores)),
      card('Memory', mem.total ? `${fmtMiB(mem.total)} total` : '', [
        stat('Used', fmtMiB(mem.used), `${fmtPct(mem.pct)}`),
        stat('Available', fmtMiB(mem.avail), mem.cached ? `${fmtMiB(mem.cached)} cache` : ''),
        stat('Swap', mem.swapTotal ? fmtMiB(mem.swapUsed) || '0' : 'off', mem.swapTotal ? `of ${fmtMiB(mem.swapTotal)}` : ''),
      ], bar(mem.pct, mem.pct >= 90 ? 'err' : mem.pct >= 80 ? 'warn' : ''),
      mem.zramCompr > 0 ? h('p', { class: 'mon-note', text: `zram: ${fmtMiB(mem.zramOrig)} stored in ${fmtMiB(mem.zramCompr)} (${mem.zramRatio.toFixed(1)}×)` }) : null),
      ...gpus.map((g) => card(g.Name || `GPU ${g.Index}`, g.PState || '', [
        stat('Power', g.PowerW ? fmtW(g.PowerW) : '–', g.PowerLimitW ? `of ${fmtW(g.PowerLimitW)}` : ''),
        stat('Util', `${g.UtilPct ?? 0}%`, ''),
        stat('Temp', g.TempC ? `${g.TempC} °C` : '–', '', tempTone(g.TempC, lim.gpu_temp_c)),
        stat('VRAM', fmtMiB(g.MemUsedMiB), g.MemTotalMiB ? `of ${fmtMiB(g.MemTotalMiB)}` : ''),
      ], bar(usedPct(g.MemUsedMiB, g.MemTotalMiB), ''))),
      card('Network', '', nets.length ? nets.map((n) => stat(n.Name, `↓ ${fmtBps(n.RxBps)}`, `↑ ${fmtBps(n.TxBps)}`)) : [stat('Interfaces', '–', 'none')]));

    renderDisks(snap.Disks || [], lim);
  }

  function renderDisks(disks, lim) {
    clear(disksEl);
    if (!disks.length) { disksEl.append(h('p', { class: 't-sec', text: 'No drives reported.' })); return; }
    for (const d of disks) {
      const pct = Number.isFinite(d.UsedPct) ? d.UsedPct : usedPct(d.UsedB, d.SizeB);
      const tone = diskTone(pct, lim.disk_pct);
      disksEl.append(h('div', { class: 'disk' },
        h('div', { class: 'disk-head' },
          h('span', { class: 'disk-name', text: (d.Mounts || []).join(', ') || d.Device }),
          h('span', { class: 'disk-tags' },
            d.Rotational ? pill(d.Idle ? 'HDD idle' : 'HDD', d.Idle ? 'muted' : 'info') : pill('SSD', 'muted'),
            d.TempC ? pill(`${Math.round(d.TempC)} °C`, tempTone(d.TempC, lim.nvme_temp_c)) : null)),
        h('div', { class: 'disk-sub', text: [d.Model, d.Device, d.FSType].filter(Boolean).join(' · ') }),
        bar(pct, tone === 'ok' ? '' : tone),
        h('div', { class: 'disk-facts' },
          h('span', { class: 'num', text: `${fmtBytes(d.UsedB)} of ${fmtBytes(d.SizeB)} (${fmtPct(pct)})` }),
          h('span', { class: 'num', text: `${fmtBytes(d.FreeB)} free` }),
          h('span', { class: 'num', text: `R ${fmtBps(d.ReadBps)} · W ${fmtBps(d.WriteBps)}${d.BusyPct ? ` · ${fmtPct(d.BusyPct)} busy` : ''}` }))));
    }
  }

  function renderTasks() {
    // The table is rebuilt on every refresh: remember which control had
    // keyboard focus and put it back, so tabbing through rows survives.
    const active = document.activeElement;
    const fk = active && tasksEl.contains(active) ? active.dataset.focus : '';
    clear(tasksEl);
    if (!state.groups.length) {
      tasksEl.append(h('p', { class: 't-sec', text: 'No process data yet (it is collected only while this view is open).' }));
      tasksNote.textContent = '';
      return;
    }
    const groups = sortGroups(filterGroups(state.groups, state.q), state.sort);
    tasksEl.append(h('div', { class: 'tt-head', 'aria-hidden': 'true' },
      h('span', { text: 'App' }), h('span', { text: 'CPU' }), h('span', { text: 'Memory' }), h('span', { text: 'GPU' }), h('span', {})));
    for (const g of groups) tasksEl.append(groupRow(g));
    const total = state.groups.reduce((n, g) => n + (g.count || 0), 0);
    tasksNote.textContent = `${total} processes in ${state.groups.length} apps (the busiest ${state.procs.length} listed). Only your own, unprotected processes can be stopped.`;
    if (fk) {
      const el = [...tasksEl.querySelectorAll('[data-focus]')].find((x) => x.dataset.focus === fk);
      if (el) el.focus({ preventScroll: true });
    }
  }

  function groupRow(g) {
    const open = state.open.has(g.name);
    const link = g.session_id ? h('a', { class: 'tt-link', href: `#/s/${encodeURIComponent(g.session_id)}`, text: 'session', 'data-focus': focusKey('s', g.name) })
      : g.procs.some(isModelProc) ? modelLink(focusKey('ml', g.name)) : null;
    const single = g.count === 1 && g.procs.length === 1;
    const head = h('div', { class: `tt-row tt-group${g.cerberus ? ' is-cerberus' : ''}` },
      h('button', { class: 'tt-name', type: 'button', 'aria-expanded': String(open), 'data-focus': focusKey('g', g.name),
        onclick: () => { open ? state.open.delete(g.name) : state.open.add(g.name); renderTasks(); } },
        h('span', { class: 'tt-caret', text: open ? '▾' : '▸' }), h('span', { class: 'tt-title', text: g.name }),
        g.count > 1 ? h('span', { class: 'tt-count', text: `×${g.count}` }) : null,
        g.cerberus ? pill('cerberus', 'warn') : null),
      cell('CPU', fmtPct(g.cpu)), cell('Memory', fmtMiB(Math.round(g.rss_mib)) || '0'), cell('GPU', g.gpu_mib ? fmtMiB(g.gpu_mib) : g.gpu_pct ? fmtPct(g.gpu_pct) : '–'),
      h('span', { class: 'tt-act' }, link, single ? actionBtn(g.procs[0]) : null));
    if (!open) return head;
    const hidden = (g.count || 0) - g.procs.length;
    return h('div', { class: 'tt-block' }, head, ...g.procs.map((p) => procRow(p)),
      ...(hidden > 0 ? [h('div', { class: 'tt-row tt-proc tt-more', text: g.procs.length
        ? `${g.procs.length} of ${g.count} shown; the other ${hidden} use too little to list.`
        : `${g.count} processes, none busy enough to list.` })] : []));
  }

  function modelLink(fk) {
    return h('a', { class: 'tt-link', href: '#/models', text: 'model', title: 'A model server: load and unload it under Models', 'data-focus': fk });
  }

  function procRow(p) {
    return h('div', { class: `tt-row tt-proc${p.Cerberus ? ' is-cerberus' : ''}` },
      h('span', { class: 'tt-name tt-pname' },
        h('span', { class: 'tt-pid num', text: String(p.PID) }),
        h('span', { class: 'tt-cmd', title: p.Cmd || p.Name, text: p.Cmd || p.Name }),
        p.Nice > 0 ? h('span', { class: 'tt-nice', text: `nice ${p.Nice}` }) : null),
      cell('CPU', fmtPct(p.CPUPct)), cell('Memory', fmtMiB(p.RSSMiB) || '0'), cell('GPU', p.GPUMiB ? fmtMiB(p.GPUMiB) : p.GPUPct ? fmtPct(p.GPUPct) : '–'),
      h('span', { class: 'tt-act' },
        p.SessionID ? h('a', { class: 'tt-link', href: `#/s/${encodeURIComponent(p.SessionID)}`, text: 'session', 'data-focus': focusKey('ps', p.PID) })
          : isModelProc(p) ? modelLink(focusKey('pm', p.PID)) : null,
        actionBtn(p)));
  }

  function actionBtn(p) {
    if (!canSignal) return null;
    if (p.Protected) return h('span', { class: 'tt-lock', title: 'Protected: Cerberus will not stop or renice this process', 'aria-label': 'Protected' }, '🔒');
    return h('button', { class: 'btn btn-ghost tt-menu', type: 'button', 'aria-label': `Actions for ${p.Name} (${p.PID})`, 'data-focus': focusKey('m', p.PID), onclick: () => procMenu(p) }, '⋯');
  }

  async function procMenu(p) {
    const choice = await openDialog((close) => ({
      title: p.Name || `PID ${p.PID}`,
      sub: `PID ${p.PID}${p.User ? ` · ${p.User}` : ''}`,
      body: h('div', { class: 'proc-menu' },
        h('p', { class: 'mon-cmd mono', text: p.Cmd || p.Name || '' }),
        h('button', { class: 'btn', type: 'button', onclick: () => close('renice') }, `Lower priority (nice ${lowerNice(p.Nice)})`),
        h('button', { class: 'btn', type: 'button', onclick: () => close('term') }, 'Terminate (ask it to quit)'),
        h('button', { class: 'btn btn-danger', type: 'button', onclick: () => close('kill') }, 'Kill (force)')),
    }));
    if (!choice) return;
    const words = {
      renice: ['Lower priority?', `${p.Name} (${p.PID}) gets nice ${lowerNice(p.Nice)}. Raising it back needs root.`, 'Lower priority', false],
      term: ['Terminate process?', `${p.Name} (${p.PID}) is asked to quit (SIGTERM). Unsaved work in it may be lost.`, 'Terminate', true],
      kill: ['Kill process?', `${p.Name} (${p.PID}) is stopped immediately (SIGKILL). Anything unsaved in it is lost.`, 'Kill', true],
    }[choice];
    const ok = await confirmDialog({ title: words[0], message: words[1], ok: words[2], danger: words[3] });
    if (!ok) return;
    const base = `/api/monitor/processes/${encodeURIComponent(p.PID)}`;
    try {
      if (choice === 'renice') await post(`${base}/renice`, { nice: lowerNice(p.Nice) });
      else await post(`${base}/signal`, { sig: choice });
      toast(`${words[2]}: ${p.Name} (${p.PID})`, 'ok');
    } catch (e) {
      toast(errText(e), 'err');
    }
    loadProcs();
  }

  // ---------- lifecycle ----------
  const onVisible = () => { loadAll(); };
  const onHidden = () => { if (document.hidden) clearTimeout(seriesTimer); };
  const onDocDown = (e) => { if (hideTip && !chartEl.contains(e.target)) hideTip(); };
  window.addEventListener('cerberus:visible', onVisible);
  document.addEventListener('visibilitychange', onHidden);
  document.addEventListener('pointerdown', onDocDown);
  renderRanges();
  loadAll();

  return () => {
    gone = true;
    clearTimeout(seriesTimer);
    stream.stop();
    doctor.unmount();
    if (usageCleanup) { usageCleanup(); usageCleanup = null; }
    window.removeEventListener('cerberus:visible', onVisible);
    document.removeEventListener('visibilitychange', onHidden);
    document.removeEventListener('pointerdown', onDocDown);
  };
}

// ---------- small builders ----------

function tile(label, value, sub, tone) {
  return h('div', { class: `stat-tile mon-tile${tone ? ` t-${tone}` : ''}` },
    h('div', { class: 'st-label', text: label }), h('div', { class: 'st-value num', text: value }),
    sub ? h('div', { class: 'mon-tile-sub num', text: sub }) : null);
}

function fact(label, value) {
  return h('div', { class: 'mon-fact' }, h('span', { class: 'stat-lbl', text: label }), h('span', { class: 'num', text: value }));
}

function card(title, sub, stats, ...extra) {
  return h('div', { class: 'gpu-card mon-card' },
    h('div', { class: 'gpu-head' }, h('span', { class: 'gpu-name', text: title }), sub ? h('span', { class: 'mon-card-sub', text: sub }) : null),
    h('div', { class: 'gpu-stats' }, stats), ...extra);
}

function stat(label, value, sub, tone) {
  return h('div', { class: 'stat' }, h('span', { class: 'stat-lbl', text: label }),
    h('span', { class: `stat-val num${tone && tone !== 'muted' ? ` tone-${tone}` : ''}`, text: value }),
    sub ? h('span', { class: 'stat-sub', text: sub }) : null);
}

function bar(pct, tone) {
  const p = Math.max(0, Math.min(100, +pct || 0));
  return h('div', { class: `progress fluid${tone ? ` tone-${tone}` : ''}`, title: `${Math.round(p)}%` }, h('span', { style: `width:${p}%` }));
}

function cell(label, value) {
  return h('span', { class: 'tt-cell num', 'data-label': label, text: value });
}
