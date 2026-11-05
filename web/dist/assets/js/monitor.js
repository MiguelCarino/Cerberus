// Pure helpers for the Monitor view: unit formatting, the stacked power
// chart maths, process grouping and sorting, and the DAC risk chip. No DOM,
// so node --test imports them directly.

// ---------- units ----------

// fmtW renders watts: "4.2 W" below 10, whole watts above, "1.21 kW" past 1000.
export function fmtW(w) {
  if (w == null || !Number.isFinite(w) || w <= 0) return '0 W';
  if (w >= 1000) return `${(w / 1000).toFixed(2)} kW`;
  if (w < 10) return `${w.toFixed(1)} W`;
  return `${Math.round(w)} W`;
}

// fmtKWh renders energy given in kWh: Wh below 1 kWh, kWh above.
export function fmtKWh(kwh) {
  if (kwh == null || !Number.isFinite(kwh) || kwh <= 0) return '0 Wh';
  if (kwh < 1) return `${Math.round(kwh * 1000)} Wh`;
  if (kwh >= 100) return `${Math.round(kwh).toLocaleString('en')} kWh`;
  return `${kwh.toFixed(kwh >= 10 ? 1 : 2)} kWh`;
}

// fmtMoney renders an amount in the tariff's currency ("$12.34 MXN"). A tiny
// non-zero amount reads "<$0.01" so it never looks free.
export function fmtMoney(n, currency = 'MXN') {
  const cur = currency ? ` ${currency}` : '';
  if (n == null || !Number.isFinite(n) || n <= 0) return `$0.00${cur}`;
  if (n < 0.01) return `<$0.01${cur}`;
  if (n >= 1000) return `$${Math.round(n).toLocaleString('en')}${cur}`;
  return `$${n.toFixed(2)}${cur}`;
}

// fmtRate is the marginal price per kWh ("$4.05/kWh").
export function fmtRate(n) {
  if (n == null || !Number.isFinite(n) || n <= 0) return '–';
  return `$${n.toFixed(n < 10 ? 3 : 2)}/kWh`;
}

const UNITS = ['B', 'KB', 'MB', 'GB', 'TB', 'PB'];

// fmtBytes renders a byte count in powers of 1000 (as drives are sold).
export function fmtBytes(b) {
  if (b == null || !Number.isFinite(b) || b <= 0) return '0 B';
  let i = 0;
  while (b >= 1000 && i < UNITS.length - 1) { b /= 1000; i++; }
  return `${b.toFixed(b >= 100 || i === 0 ? 0 : 1)} ${UNITS[i]}`;
}

// fmtBps renders a byte rate ("12.3 MB/s"), "idle" for nothing.
export function fmtBps(bps) {
  if (bps == null || !Number.isFinite(bps) || bps < 1) return 'idle';
  return `${fmtBytes(bps)}/s`;
}

// fmtPct renders a percentage with no decimals ("–" when unknown).
export function fmtPct(p) {
  return p == null || !Number.isFinite(p) ? '–' : `${Math.round(p)}%`;
}

// fmtDuration renders a Go time.Duration (nanoseconds) or seconds-as-string.
export function fmtDuration(ns) {
  if (typeof ns === 'string') return ns;
  if (ns == null || !Number.isFinite(ns) || ns <= 0) return '–';
  const s = Math.floor(ns / 1e9);
  const d = Math.floor(s / 86400), hh = Math.floor((s % 86400) / 3600), mm = Math.floor((s % 3600) / 60);
  if (d) return `${d}d ${hh}h`;
  if (hh) return `${hh}h ${mm}m`;
  return `${mm}m`;
}

// ---------- energy totals ----------

// kwhOf reads wall kWh from one energy.Totals entry ({CPU, GPU, Rest, Wall}
// in kWh, e.g. totals.Today) or a plain number (totals.ProjectedMonthKWh).
export function kwhOf(t) {
  if (t == null) return 0;
  const v = typeof t === 'number' ? t : t.Wall;
  return Number.isFinite(v) && v > 0 ? v : 0;
}

// ---------- alert limits ----------

// DEFAULT_LIMITS mirror config.Defaults().Monitor.Alerts, for a daemon that
// does not send them.
export const DEFAULT_LIMITS = Object.freeze({ disk_pct: 90, cpu_temp_c: 90, gpu_temp_c: 85, nvme_temp_c: 70 });

// limitsOf merges the configured /api/monitor alerts over the defaults
// (a zero or missing limit falls back to its default).
export function limitsOf(alerts) {
  const out = { ...DEFAULT_LIMITS };
  for (const k of Object.keys(out)) {
    const v = alerts && alerts[k];
    if (Number.isFinite(v) && v > 0) out[k] = v;
  }
  return out;
}

// ---------- DAC chip ----------

// dacChip maps bill.marginal.DACRisk to a label and a status tone.
export function dacChip(risk, avg, limit) {
  const tail = Number.isFinite(avg) && avg > 0
    ? ` (${Math.round(avg)}${Number.isFinite(limit) && limit > 0 ? ` of ${Math.round(limit)}` : ''} kWh/mo avg)` : '';
  switch (risk) {
    case 'near': return { label: 'Near DAC', tone: 'warn', text: `Household is close to the DAC limit${tail}.` };
    case 'pushes': return { label: 'PC pushes into DAC', tone: 'err', text: `Without this PC you would stay under the DAC limit${tail}.` };
    case 'already': return { label: 'In DAC', tone: 'err', text: `Household average is over the DAC limit${tail}: every kWh is priced at DAC.` };
    case 'none': return { label: 'No DAC risk', tone: 'ok', text: `Well under the DAC limit${tail}.` };
    default: return { label: 'DAC unknown', tone: 'muted', text: 'Set household_kwh_month in the tariff config to see DAC risk.' };
  }
}

// ---------- power chart ----------

export const SERIES = [
  { key: 'CPU', label: 'CPU' },
  { key: 'GPU', label: 'GPU' },
  { key: 'Rest', label: 'Rest' },
];

// A /api/monitor/series point is energy.Point: {T (RFC 3339), CPU, GPU,
// Rest, Wall}, average watts over the step (Wall includes PSU losses).

// pointValue reads one field's watts (CPU, GPU, Rest or Wall); missing,
// invalid or negative is 0.
export function pointValue(p, key) {
  const v = p ? p[key] : NaN;
  return Number.isFinite(v) ? Math.max(0, v) : 0;
}

// pointTime reads a point's T in ms (RFC 3339 string, or ms in tests).
export function pointTime(p) {
  const t = p ? p.T : undefined;
  if (typeof t === 'number') return t;
  const ms = Date.parse(t);
  return Number.isFinite(ms) ? ms : NaN;
}

// normalizeSeries takes the bare array the API returns and keeps the points
// with a valid time, sorted by it.
export function normalizeSeries(raw) {
  const list = Array.isArray(raw) ? raw : [];
  return list.filter((p) => Number.isFinite(pointTime(p))).sort((a, b) => pointTime(a) - pointTime(b));
}

// niceMax rounds a maximum up to 1, 2 or 5 × 10^n so axis labels are round.
export function niceMax(v) {
  if (!Number.isFinite(v) || v <= 0) return 10;
  const p = 10 ** Math.floor(Math.log10(v));
  for (const m of [1, 2, 5, 10]) if (v <= m * p) return m * p;
  return 10 * p;
}

// chartDomain is the x-axis time window [from, to] (ms) for a range ending
// now: the whole selected window, so missing time shows as empty space and
// the newest point sits at the right edge. It widens to the first point when
// a step starts before the window (a day point starts at midnight).
export function chartDomain(points, rangeMs, now = Date.now()) {
  let from = now - rangeMs;
  const first = points && points.length ? pointTime(points[0]) : NaN;
  if (Number.isFinite(first) && first < from) from = first;
  const last = points && points.length ? pointTime(points[points.length - 1]) : NaN;
  const to = Number.isFinite(last) && last > now ? last : now;
  return [from, to];
}

// stackedAreas computes SVG paths for the stacked CPU/GPU/Rest areas in a
// w×h box. x is spread by time over domain ([from, to] ms; default: the
// points' own span), so gaps stay gaps in time; y from the bottom.
// Returns { max, xs, totals, paths: {CPU, GPU, Rest}, tops: {key: [y...]} }.
export function stackedAreas(points, w, h, keys = SERIES.map((s) => s.key), domain = null) {
  const n = points.length;
  const out = { max: niceMax(0), xs: [], totals: [], paths: {}, tops: {} };
  if (!n) { for (const k of keys) out.paths[k] = ''; return out; }
  const t0 = domain ? domain[0] : pointTime(points[0]);
  const t1 = domain ? domain[1] : pointTime(points[n - 1]);
  const span = t1 - t0;
  const clampX = (v) => Math.max(0, Math.min(w, v));
  out.xs = points.map((p, i) => (span > 0 ? clampX(((pointTime(p) - t0) / span) * w) : n === 1 ? w / 2 : (i / (n - 1)) * w));
  const cum = points.map(() => 0);
  const levels = [];
  for (const k of keys) {
    const lo = cum.slice();
    points.forEach((p, i) => { cum[i] += pointValue(p, k); });
    levels.push({ k, lo, hi: cum.slice() });
  }
  out.totals = cum.slice();
  out.max = niceMax(Math.max(...cum));
  const y = (v) => +(h - (v / out.max) * h).toFixed(2);
  const x = (i) => +out.xs[i].toFixed(2);
  for (const { k, lo, hi } of levels) {
    if (n === 1) {
      // A single point is drawn as a thin bar so it is visible.
      const cx = x(0);
      out.paths[k] = `M${cx - 2},${y(hi[0])}L${cx + 2},${y(hi[0])}L${cx + 2},${y(lo[0])}L${cx - 2},${y(lo[0])}Z`;
    } else {
      let d = `M${x(0)},${y(hi[0])}`;
      for (let i = 1; i < n; i++) d += `L${x(i)},${y(hi[i])}`;
      for (let i = n - 1; i >= 0; i--) d += `L${x(i)},${y(lo[i])}`;
      out.paths[k] = `${d}Z`;
    }
    out.tops[k] = hi.map(y);
  }
  return out;
}

// nearestIndex returns the index of the x closest to px (binary search; xs ascending).
export function nearestIndex(xs, px) {
  if (!xs.length) return -1;
  let lo = 0, hi = xs.length - 1;
  while (hi - lo > 1) {
    const mid = (lo + hi) >> 1;
    if (xs[mid] < px) lo = mid; else hi = mid;
  }
  return Math.abs(xs[lo] - px) <= Math.abs(xs[hi] - px) ? lo : hi;
}

// RANGES are the chart windows: resolution and how far back.
export const RANGES = [
  { key: '1h', label: '1 h', res: 'minute', ms: 3600e3 },
  { key: '7d', label: '7 d', res: 'hour', ms: 7 * 86400e3 },
  { key: '30d', label: '30 d', res: 'day', ms: 30 * 86400e3 },
];

// fmtPointTime labels a point for the tooltip per resolution.
export function fmtPointTime(ms, res) {
  const d = new Date(ms);
  if (res === 'day') return d.toLocaleDateString([], { month: 'short', day: 'numeric' });
  if (res === 'hour') return d.toLocaleString([], { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' });
  return d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
}

// ---------- processes ----------

// groupProcs aggregates processes by Group (falling back to Name). Each group
// sums CPU, RSS and GPU memory and keeps its processes sorted by CPU.
export function groupProcs(procs) {
  const map = new Map();
  for (const p of procs || []) {
    const name = p.Group || p.Name || `pid ${p.PID}`;
    let g = map.get(name);
    if (!g) {
      g = { name, cpu: 0, rss_mib: 0, gpu_mib: 0, gpu_pct: 0, count: 0, cerberus: false, session_id: '', protected: true, procs: [] };
      map.set(name, g);
    }
    g.cpu += +p.CPUPct || 0;
    g.rss_mib += +p.RSSMiB || 0;
    g.gpu_mib += +p.GPUMiB || 0;
    g.gpu_pct += +p.GPUPct || 0;
    g.count++;
    g.cerberus = g.cerberus || !!p.Cerberus;
    if (!g.session_id && p.SessionID) g.session_id = p.SessionID;
    g.protected = g.protected && !!p.Protected;
    g.procs.push(p);
  }
  for (const g of map.values()) g.procs.sort((a, b) => (b.CPUPct || 0) - (a.CPUPct || 0) || (b.RSSMiB || 0) - (a.RSSMiB || 0));
  return [...map.values()];
}

// mergeGroups puts the server's complete per-app totals (/api/monitor/
// processes groups: every process, not only the top 150 sent) over the
// client groups built from the listed processes. Each result keeps the
// listed processes in procs, and shown = how many of count are listed.
export function mergeGroups(listed, server) {
  if (!Array.isArray(server) || !server.length) {
    return (listed || []).map((g) => ({ ...g, shown: g.procs.length }));
  }
  const byName = new Map((listed || []).map((g) => [g.name, g]));
  const out = [];
  for (const sg of server) {
    if (!sg || !sg.name) continue;
    const g = byName.get(sg.name);
    byName.delete(sg.name);
    const procs = g ? g.procs : [];
    out.push({
      name: sg.name, cpu: +sg.cpu || 0, rss_mib: +sg.rss_mib || 0, gpu_mib: +sg.gpu_mib || 0,
      gpu_pct: g ? g.gpu_pct : 0, count: Math.max(+sg.count || 0, procs.length),
      cerberus: !!sg.cerberus || (g ? g.cerberus : false), session_id: sg.session_id || (g ? g.session_id : ''),
      protected: g ? g.protected : true, procs, shown: procs.length,
    });
  }
  for (const g of byName.values()) out.push({ ...g, shown: g.procs.length }); // listed after the groups were taken
  return out;
}

// MODEL_SERVERS mirror sysmon's model server names.
export const MODEL_SERVERS = new Set(['llama-server', 'llmster', 'ollama', 'lms', 'vllm']);

// isModelProc: a Cerberus-related process that is a model server (not a
// session, not the daemon), which the Tasks table links to Models.
export function isModelProc(p) {
  return !!p && !!p.Cerberus && !p.SessionID && MODEL_SERVERS.has(p.Name);
}

// focusKey names a task-table control so focus survives a refresh.
export function focusKey(kind, id) {
  return `${kind}:${String(id)}`;
}

export const SORTS = {
  cpu: (g) => g.cpu,
  ram: (g) => g.rss_mib,
  gpu: (g) => g.gpu_mib + g.gpu_pct,
  name: null,
};

// sortGroups sorts groups by cpu | ram | gpu (descending) or name (ascending).
// Ties fall back to name so the order is stable between refreshes.
export function sortGroups(groups, by = 'cpu') {
  const key = SORTS[by];
  return [...groups].sort((a, b) => {
    if (key) { const d = key(b) - key(a); if (Math.abs(d) > 1e-9) return d; }
    return a.name.localeCompare(b.name);
  });
}

// filterGroups keeps groups whose name, a process name or command matches q.
export function filterGroups(groups, q) {
  const s = (q || '').trim().toLowerCase();
  if (!s) return groups;
  return groups.filter((g) => g.name.toLowerCase().includes(s)
    || g.procs.some((p) => (p.Name || '').toLowerCase().includes(s) || (p.Cmd || '').toLowerCase().includes(s) || String(p.PID) === s));
}

// lowerNice is the next lower priority for "Lower priority": +5, capped at 19.
export function lowerNice(nice) {
  const n = Number.isFinite(nice) ? nice : 0;
  return Math.min(19, Math.max(0, n) + 5);
}

// ---------- system panels ----------

// usedPct is used/total as a 0..100 percentage (0 when unknown).
export function usedPct(used, total) {
  return total > 0 && used >= 0 ? Math.max(0, Math.min(100, (used / total) * 100)) : 0;
}

// memParts derives the memory bar from sysmon's Mem (MiB fields).
export function memParts(m) {
  m = m || {};
  const total = +m.TotalMiB || 0;
  const avail = +m.AvailMiB || 0;
  const used = Math.max(0, total - avail);
  const swapUsed = +m.SwapUsedMiB || 0;
  const swapTotal = +m.SwapTotalMiB || 0;
  const zramRatio = m.ZramComprMiB > 0 ? (m.ZramOrigMiB || 0) / m.ZramComprMiB : 0;
  return { total, used, avail, cached: +m.CachedMiB || 0, pct: usedPct(used, total), swapUsed, swapTotal,
    swapPct: usedPct(swapUsed, swapTotal), zramOrig: +m.ZramOrigMiB || 0, zramCompr: +m.ZramComprMiB || 0, zramRatio };
}

// tempTone colours a temperature against a limit: warn within 10 °C, err past it.
export function tempTone(c, limit) {
  if (!Number.isFinite(c) || c <= 0 || !Number.isFinite(limit) || limit <= 0) return 'muted';
  if (c >= limit) return 'err';
  if (c >= limit - 10) return 'warn';
  return 'ok';
}

// diskTone colours a disk's fill: warn at 80%, err at the alert threshold (default 90).
export function diskTone(pct, alertPct = 90) {
  if (!Number.isFinite(pct)) return 'muted';
  if (pct >= alertPct) return 'err';
  if (pct >= 80) return 'warn';
  return 'ok';
}

// livePayload checks a stream event's data, which is the same object as
// GET /api/monitor ({snapshot, power, totals, bill, alerts}); null otherwise.
export function livePayload(data) {
  if (!data || typeof data !== 'object' || !data.snapshot || typeof data.snapshot !== 'object') return null;
  return { snapshot: data.snapshot, power: data.power || data.snapshot.Power || null,
    totals: data.totals || null, bill: data.bill || null, alerts: data.alerts || null };
}

// ---------- per-model energy ----------

const num = (v) => (Number.isFinite(v) && v > 0 ? v : 0);

// modelEnergy reads a model's wall energy (J) and cost for the last 24 h and
// this month from engine.ModelInfo (EnergyJ24h, CostMXN24h, EnergyJMonth,
// CostMXNMonth); missing values are 0.
export function modelEnergy(m) {
  m = m || {};
  return { j24: num(m.EnergyJ24h), jMonth: num(m.EnergyJMonth), cost24: num(m.CostMXN24h), costMonth: num(m.CostMXNMonth) };
}
