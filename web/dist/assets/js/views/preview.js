// Preview tab of the session view: the shared rendering space for the
// work. An iframe shows the chosen source (the static site, a live dev
// server the agent runs, or the Run preview server Cerberus starts), with
// a source picker, device width presets, Reload, Open in new window and
// Copy link, and the Run preview server's state, Start/Stop and log.
// Each preview lives on its own origin (server.preview_ports): the iframe
// loads the app's entry URL, which redirects there with a capability.

import { api, post, sid } from '../api.js';
import { h, clear, toast, errText, pill } from '../dom.js';
import { store } from '../store.js';
import {
  PRESETS, presetWidth, previewSources, pickSource, absoluteURL, managedTone, managedRunning, emptyHint,
} from '../preview.js';
import { copyText } from './chatdom.js';

const POLL_MS = 4000;

// previewPanel builds the panel for session id. readOnly (a viewer) hides
// Start and Stop; the server refuses them anyway.
export function previewPanel(id, { readOnly = false } = {}) {
  let data = null;
  let current = null; // the shown source
  let want = '';
  let preset = 'desktop';
  let loadedURL = '';
  let shown = false;
  let timer = 0;
  let busy = false;

  const picker = h('select', { class: 'input pv-source', 'aria-label': 'Preview source', onchange: () => { want = picker.value; render(); } });
  const presetBtns = PRESETS.map((p) => h('button', {
    class: 'btn btn-ghost pv-preset', type: 'button', 'data-preset': p.key, 'aria-pressed': String(p.key === preset),
    onclick: () => { preset = p.key; sizeFrame(); },
  }, p.label));
  const presets = h('div', { class: 'pv-presets', role: 'group', 'aria-label': 'Width' }, ...presetBtns);
  const reloadBtn = h('button', { class: 'btn btn-ghost', type: 'button', title: 'Reload the preview', onclick: reload }, '↻ Reload');
  const openBtn = h('button', { class: 'btn btn-primary pv-open', type: 'button', onclick: openWindow }, 'Open in new window ↗');
  const copyBtn = h('button', { class: 'btn btn-ghost', type: 'button', onclick: (e) => copyLink(e.currentTarget) }, '⧉ Copy link');
  const bar = h('div', { class: 'pv-bar' }, picker, presets, h('div', { class: 'pv-bar-end' }, reloadBtn, copyBtn, openBtn));

  const mState = h('span', {});
  const mCmd = h('code', { class: 'pv-cmd' });
  const startBtn = h('button', { class: 'btn btn-primary', type: 'button', onclick: start }, '▶ Run preview');
  const stopBtn = h('button', { class: 'btn btn-danger', type: 'button', onclick: stop }, '■ Stop preview');
  const logPre = h('pre', { class: 'pv-log' });
  const logBox = h('details', { class: 'pv-logbox' }, h('summary', { text: 'Preview server log' }), logPre);
  const managedRow = h('div', { class: 'pv-managed' },
    h('div', { class: 'pv-mline' }, h('span', { class: 'pv-mlabel', text: 'Run preview' }), mState, mCmd,
      h('span', { class: 'pv-mbtns' }, startBtn, stopBtn)),
    logBox);

  const frame = h('iframe', {
    class: 'pv-frame', title: 'Preview of the work', referrerpolicy: 'no-referrer',
    sandbox: 'allow-scripts allow-same-origin allow-forms allow-popups allow-modals allow-downloads',
  });
  const frameWrap = h('div', { class: 'pv-framewrap' }, frame);
  const empty = h('div', { class: 'pv-empty', hidden: true });
  const stage = h('div', { class: 'pv-stage' }, frameWrap, empty);
  const el = h('div', { class: 'pv-panel' }, bar, managedRow, stage);

  const ro = new ResizeObserver(() => sizeFrame());
  ro.observe(stage);

  async function load() {
    try {
      data = await api(`${sid(id)}/preview`);
    } catch (e) {
      data = null;
      clear(empty).append(h('p', { class: 'alert err', text: errText(e) }));
      empty.hidden = false;
      frameWrap.hidden = true;
      return;
    }
    render();
  }

  function render() {
    const sources = previewSources(data);
    current = pickSource(sources, want || (current && current.key));
    clear(picker).append(...sources.map((s) => h('option', { value: s.key, selected: current && s.key === current.key, disabled: !s.url }, s.url ? s.label : `${s.label} (needs preview ports)`)));
    picker.hidden = sources.length < 2;
    const usable = !!(current && current.url);
    for (const b of [reloadBtn, openBtn, copyBtn]) b.disabled = !usable;
    bar.hidden = !usable; // nothing to size, reload or open yet
    renderManaged();
    if (!usable) {
      if (loadedURL) { frame.removeAttribute('src'); loadedURL = ''; }
      frameWrap.hidden = true;
      const lines = emptyHint(data);
      clear(empty).append(h('p', { text: lines[0] }), h('ul', {}, ...lines.slice(1).map((t) => h('li', { text: t }))));
      empty.hidden = false;
      return;
    }
    empty.hidden = true;
    frameWrap.hidden = false;
    if (current.url !== loadedURL) {
      loadedURL = current.url;
      frame.src = current.url;
    }
    sizeFrame();
  }

  function renderManaged() {
    const m = (data && data.managed) || {};
    const canStart = !readOnly && data && data.can_start;
    clear(mState).append(m.state ? pill(m.state, managedTone(m.state)) : pill('not running', 'muted'));
    mCmd.textContent = m.command ? m.command : 'no preview command found';
    mCmd.title = m.source ? `from ${m.source}` : '';
    startBtn.hidden = !canStart || managedRunning(m.state);
    startBtn.disabled = busy || !m.command;
    stopBtn.hidden = !canStart || !managedRunning(m.state);
    stopBtn.disabled = busy;
    if (m.error) logPre.textContent = `${m.log_tail || ''}\n${m.error}`.trim();
    else logPre.textContent = m.log_tail || '(no output yet)';
    logBox.hidden = !m.state;
    // No command and never started: the empty state says how to add one.
    managedRow.hidden = !m.command && !m.state;
    managedRow.classList.toggle('pv-managed-ro', !canStart);
  }

  function sizeFrame() {
    for (const b of presetBtns) b.setAttribute('aria-pressed', String(b.dataset.preset === preset));
    const w = presetWidth(preset, stage.clientWidth - 2);
    frameWrap.style.width = w ? `${w}px` : '';
    frameWrap.classList.toggle('pv-device', !!w);
  }

  function reload() {
    if (!current || !current.url) return;
    loadedURL = current.url;
    frame.src = current.url; // through the entry again: a closed origin opens anew
  }

  function openWindow() {
    if (!current || !current.url) return;
    window.open(current.url, '_blank', 'noopener');
  }

  function copyLink(btn) {
    if (!current || !current.url) return;
    copyText(absoluteURL(current.url, location.origin), btn);
  }

  async function start() {
    busy = true; renderManaged();
    try {
      data = await post(`${sid(id)}/preview/start`);
      want = '';
      toast('Preview server starting', 'ok');
    } catch (e) {
      toast(errText(e), 'err');
    }
    busy = false;
    render();
  }

  async function stop() {
    busy = true; renderManaged();
    try {
      data = await post(`${sid(id)}/preview/stop`);
      toast('Preview server stopped', 'ok');
    } catch (e) {
      toast(errText(e), 'err');
    }
    busy = false;
    render();
  }

  function poll() {
    clearTimeout(timer);
    if (!shown) return;
    timer = setTimeout(() => {
      if (shown && !document.hidden) load().finally(poll);
      else poll();
    }, POLL_MS);
  }

  // The session's preview summary changed (a dev server came up, the Run
  // preview server moved on): fetch the details now.
  let lastSig = '';
  const unsub = store.subscribe((kind, payload) => {
    if (!shown || kind !== 'session' || !payload || payload.ID !== id) return;
    const sig = JSON.stringify(payload.Preview || null) + (payload.PreviewURL || '');
    if (sig !== lastSig) { lastSig = sig; load(); }
  });

  return {
    el,
    open() { shown = true; load(); poll(); },
    close() { shown = false; clearTimeout(timer); },
    destroy() { shown = false; clearTimeout(timer); unsub(); ro.disconnect(); frame.removeAttribute('src'); },
  };
}
