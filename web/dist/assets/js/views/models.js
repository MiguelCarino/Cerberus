// Models view: GPU cards, the model list with load/unload, and 24 h speed
// and efficiency per model. The list follows /api/events ("models" and
// "prefill", step web-api) instead of polling; only the GPU cards are read
// every 10 s (GET /api/gpu, which lists no models), while this view is open
// and the tab is visible. The 24 h figures are re-read on "usage" hints, at
// most every 15 s.

import { api, post } from '../api.js';
import { h, clear, toast, errText, pill, confirmDialog } from '../dom.js';
import { store } from '../store.js';
import { fmtNum, fmtMiB, fmtEnergy, fmtUptime, relTime, versionLabel } from '../util.js';
import { modelEnergy, fmtMoney } from '../monitor.js';
import { openGetModels } from './getmodels.js';
import { trashIcon } from './ctl.js';
import { deleteModelMessage, busyText, failureText, mergeLive, reserveText, withSince, tickBusy } from '../models.js';

const GPU_MS = 10000;
const USAGE_MS = 15000;

export function mount(root) {
  const gpuEl = h('div', { class: 'gpu-grid' });
  // The VRAM a load leaves free below the card's top (railguard's line).
  const reserveEl = h('p', { class: 'model-facts gpu-reserve', hidden: true });
  const hostEl = h('p', { class: 'view-sub' });
  const modelsEl = h('div', { class: 'model-list' }, h('p', { class: 't-sec', text: 'Loading…' }));
  root.append(h('section', { class: 'view view-pad' },
    h('div', { class: 'view-head' }, h('div', {}, h('h1', { class: 'view-title', text: 'Models' }), hostEl),
      // Users and roles: loading, unloading, deleting and downloading
      // models are admin actions.
      store.can('models.manage') ? h('button', { class: 'btn btn-primary', type: 'button', onclick: () => openGetModels().then(() => { if (!gone) refresh(); }) }, 'Get models') : null),
    h('h2', { class: 'group-title', text: 'GPUs' }), gpuEl, reserveEl,
    h('h2', { class: 'group-title', text: 'Models' }), modelsEl));

  let models = [];
  let agg = new Map();
  const busy = new Set();
  let timer = 0;
  let gone = false;
  let usageAt = 0;
  let usageTimer = 0;
  // The last "models" event and a count of them: a refresh that was in
  // flight while one arrived applies it again over its older snapshot.
  let lastLive = null;
  let liveSeq = 0;

  const renderReserve = (r) => {
    const text = reserveText(r);
    reserveEl.textContent = text;
    reserveEl.hidden = !text;
  };
  const renderStatus = (st) => {
    if (!st) return;
    hostEl.textContent = [st.host, versionLabel(st.version), uptimeText(st.uptime)]
      .filter(Boolean).join(' · ');
    renderGPUs(gpuEl, st.gpu);
    renderReserve(st.vram_reserve);
  };

  const renderModels = () => {
    clear(modelsEl);
    if (!models.length) { modelsEl.append(h('p', { class: 't-sec', text: 'No models found. Configure llama.cpp, LM Studio, Ollama or a provider.' })); return; }
    const sorted = [...models].sort((a, b) => (b.Loaded - a.Loaded) || (b.Local - a.Local) || a.ID.localeCompare(b.ID));
    modelsEl.append(...sorted.map((m) => modelRow(m, agg.get(m.ID), busy.has(m.ID), toggle, remove)));
  };

  // refresh reads everything once: on open, on return to the tab, on an
  // event-stream reconnect and after an action. /api/status carries the
  // model list (it used to be fetched twice).
  async function refresh() {
    clearTimeout(timer);
    const seq = liveSeq;
    try {
      const [st, us] = await Promise.all([store.refreshStatus(), api('/api/usage?limit=1').catch(() => null)]);
      if (gone) return;
      renderStatus(st);
      models = withSince((st && st.models) || []);
      if (liveSeq !== seq && lastLive) models = mergeLive(models, lastLive).models;
      setUsage(us);
      renderModels();
    } catch (e) {
      if (!gone) toast(errText(e), 'err');
    }
    schedule();
  }

  function setUsage(us) {
    usageAt = Date.now();
    if (us) agg = new Map((us.agg_24h || []).map((a) => [a.Model, a]));
  }

  // The GPU cards (and the busy times on the list) every 10 s.
  async function tickGPU() {
    try {
      const g = await api('/api/gpu');
      if (gone) return;
      renderGPUs(gpuEl, g && g.gpu);
      renderReserve(g && g.vram_reserve);
      models = tickBusy(models);
      renderModels();
    } catch { /* the next tick retries */ }
    schedule();
  }

  function schedule() {
    clearTimeout(timer);
    if (!gone && !document.hidden) timer = setTimeout(tickGPU, GPU_MS);
  }

  // A request finished: the 24 h figures may have moved. At most every 15 s.
  function onUsage() {
    if (usageTimer || gone) return;
    usageTimer = setTimeout(async () => {
      usageTimer = 0;
      if (gone || document.hidden) return;
      const us = await api('/api/usage?limit=1').catch(() => null);
      if (gone || !us) return;
      setUsage(us);
      renderModels();
    }, Math.max(0, USAGE_MS - (Date.now() - usageAt)));
  }

  async function onLive(live) {
    lastLive = live;
    liveSeq++;
    const r = mergeLive(models, live);
    models = r.models;
    renderModels();
    if (r.unknown) {
      // A model appeared (a download finished, a listing changed).
      try { models = mergeLive(withSince((await api('/api/models')) || []), live).models; } catch { return; }
      if (!gone) renderModels();
    }
  }

  function onPrefill(p) {
    const m = models.find((x) => x.ID === p.model);
    if (!m) return;
    m.Prefill = p.done ? null : p;
    renderModels();
  }

  const unsub = store.subscribe((kind, payload) => {
    if (gone) return;
    if (kind === 'models') onLive(payload);
    else if (kind === 'prefill') onPrefill(payload);
    else if (kind === 'usage') onUsage();
    else if (kind === 'events-open') refresh();
  });

  async function toggle(m) {
    busy.add(m.ID);
    renderModels();
    const verb = m.Loaded ? 'unload' : 'load';
    try {
      await post(`/api/models/${encodeURIComponent(m.ID)}/${verb}`);
      toast(`${m.ID} ${verb}ed`, 'ok');
    } catch (e) {
      toast(errText(e), 'err');
    }
    busy.delete(m.ID);
    refresh();
  }

  // remove deletes a local model's files from disk, after a confirm that
  // says how much goes and that it must be downloaded again to use it.
  async function remove(m) {
    const msg = deleteModelMessage(m);
    if (!(await confirmDialog({ title: msg.title, message: msg.message, ok: 'Delete', danger: true }))) return;
    busy.add(m.ID);
    renderModels();
    try {
      await post(`/api/models/${encodeURIComponent(m.ID)}/delete`);
      toast(`${m.ID} deleted`, 'ok');
    } catch (e) {
      toast(errText(e), 'err');
    }
    busy.delete(m.ID);
    refresh();
  }

  const onVisible = () => refresh();
  const onHidden = () => { if (document.hidden) clearTimeout(timer); };
  window.addEventListener('cerberus:visible', onVisible);
  document.addEventListener('visibilitychange', onHidden);
  renderStatus(store.status());
  refresh();

  return () => {
    gone = true;
    unsub();
    clearTimeout(timer);
    clearTimeout(usageTimer);
    window.removeEventListener('cerberus:visible', onVisible);
    document.removeEventListener('visibilitychange', onHidden);
  };
}

function renderGPUs(el, snap) {
  clear(el);
  const gpus = (snap && snap.GPUs) || [];
  if (!gpus.length) {
    el.append(h('div', { class: 'gpu-card empty-gpu' }, h('p', { class: 't-sec', text: 'No GPU detected (or no monitoring tool installed).' })));
    return;
  }
  for (const g of gpus) {
    const memPct = g.MemTotalMiB ? Math.round((g.MemUsedMiB / g.MemTotalMiB) * 100) : 0;
    el.append(h('div', { class: 'gpu-card' },
      h('div', { class: 'gpu-head' }, h('span', { class: 'gpu-name', text: g.Name || `GPU ${g.Index}` }),
        g.PState ? h('span', { class: 'chip-tag', text: g.PState }) : null),
      h('div', { class: 'gpu-stats' },
        stat('Power', g.PowerW ? `${g.PowerW.toFixed(0)} W` : '–', g.PowerLimitW ? `of ${g.PowerLimitW.toFixed(0)} W` : ''),
        stat('Util', `${g.UtilPct ?? 0}%`, ''),
        stat('Temp', g.TempC ? `${g.TempC} °C` : '–', ''),
        stat('VRAM', fmtMiB(g.MemUsedMiB), g.MemTotalMiB ? `of ${fmtMiB(g.MemTotalMiB)}` : '')),
      h('div', { class: 'progress fluid', title: `VRAM ${memPct}%` }, h('span', { style: `width:${memPct}%` }))));
  }
  if (snap.EnergyJ) el.append(h('p', { class: 'gpu-energy', text: `Energy since start: ${fmtEnergy(snap.EnergyJ)}` }));
}

// The status uptime may be seconds or a preformatted Go duration string.
function uptimeText(u) {
  if (u == null || u === '') return '';
  return `up ${typeof u === 'number' ? fmtUptime(u) : u}`;
}

function stat(label, value, sub) {
  return h('div', { class: 'stat' }, h('span', { class: 'stat-lbl', text: label }),
    h('span', { class: 'stat-val', text: value }), sub ? h('span', { class: 'stat-sub', text: sub }) : null);
}

function modelRow(m, a, busy, toggle, remove) {
  const where = m.Local ? (m.Loaded ? pill('loaded', 'ok') : pill('local', 'muted')) : pill('cloud', 'info');
  const facts = [m.Backend, m.SizeMiB ? fmtMiB(m.SizeMiB) : '', m.Ctx ? `${Math.round(m.Ctx / 1024)}k ctx` : '',
    m.InFlight ? `${m.InFlight} in flight` : '', m.Loaded && m.LastUsed ? `used ${relTime(m.LastUsed)}` : '', m.Vision ? 'vision' : '']
    .filter(Boolean).join(' · ');
  const load = busyText(m);
  const fail = failureText(m);
  return h('div', { class: 'model-row' },
    h('div', { class: 'model-main' },
      h('div', { class: 'model-id' }, h('span', { text: m.ID }), where),
      h('div', { class: 'model-facts', text: facts }),
      load ? h('div', { class: 'model-facts model-busy', text: load }) : null,
      fail ? h('div', { class: 'model-facts model-fail', text: fail }) : null,
      h('div', { class: 'model-perf' },
        perf('tok/s', a && fmtNum(a.AvgTokPerSec)),
        perf('tok/J', a && fmtNum(a.AvgTokPerJ, 2)),
        perf('req 24h', a ? String(a.Requests) : '–')),
      energyLine(m)),
    m.Local && store.can('models.manage') ? h('div', { class: 'model-actions' },
      h('button', { class: `btn ${m.Loaded ? '' : 'btn-primary'}`, type: 'button', disabled: busy, onclick: () => toggle(m) },
        busy ? '…' : m.Loaded ? 'Unload' : 'Load'),
      h('button', { class: 'icon-btn model-del', type: 'button', disabled: busy || m.InFlight > 0,
        title: m.InFlight > 0 ? 'Answering requests: delete when idle' : 'Delete from disk', 'aria-label': `Delete ${m.ID} from disk`,
        onclick: () => remove(m) }, trashIcon())) : null);
}

function perf(label, value) {
  return h('span', { class: 'perf' }, h('b', { text: value || '–' }), ` ${label}`);
}

// energyLine is the model's energy and cost (24 h, month), or nothing when
// it has not used any (cloud models cost money, shown under Usage instead).
function energyLine(m) {
  const e = modelEnergy(m);
  if (e.j24 <= 0 && e.jMonth <= 0) return null;
  const part = (j, c, when) => (j > 0 ? [fmtEnergy(j), c > 0 ? fmtMoney(c, 'MXN') : '', when].filter(Boolean).join(' · ') : '');
  return h('div', { class: 'model-energy', text: [part(e.j24, e.cost24, '24 h'), part(e.jMonth, e.costMonth, 'this month')].filter(Boolean).join('   ·   ') });
}
