// PWA glue (day phase 6): registers the service worker (/sw.js, scope
// "/"), shows "Reload to update" when a new build is waiting, and the
// offline banner ("offline: the host is unreachable", with Retry) while API
// requests cannot reach the host.
//
// Energy: no timers. Updates are checked when the tab becomes visible
// again; Retry is a click (plus the browser's "online" event); the event
// stream's own backoff also flips the banner when the host comes back.

import { api, onReachability, AuthError } from './api.js';
import { h } from './dom.js';
import { updateReady } from './pwa-core.js';

let bar = null;
let msg = null;
let action = null;
let mode = ''; // '' | 'offline' | 'update'
let waiting = null;

function ensureBar() {
  if (bar) return;
  msg = h('span', { class: 'pwa-msg' });
  action = h('button', { class: 'btn btn-sm', type: 'button', onclick: () => onAction() });
  bar = h('div', { class: 'pwa-bar', role: 'status', 'aria-live': 'polite', hidden: true }, msg, action);
  document.body.append(bar);
}

function show(next) {
  ensureBar();
  // Offline wins over an update (reloading while offline would fail).
  if (next === 'update' && mode === 'offline') return;
  mode = next;
  bar.hidden = !next;
  bar.className = `pwa-bar ${next}`;
  document.body.classList.toggle('pwa-offline', next === 'offline');
  if (next === 'offline') { msg.textContent = 'Offline: the host is unreachable.'; action.textContent = 'Retry'; action.disabled = false; }
  if (next === 'update') { msg.textContent = 'A new version of Cerberus is ready.'; action.textContent = 'Reload to update'; action.disabled = false; }
}

function onAction() {
  if (mode === 'offline') retry();
  else if (mode === 'update') applyUpdate();
}

// retry asks the host once; reaching it hides the banner (via the
// reachability listener) and tells the views to refresh.
async function retry() {
  action.disabled = true;
  action.textContent = 'Retrying…';
  try {
    await api('/api/status');
  } catch (e) {
    if (e instanceof AuthError) return;
  } finally {
    if (mode === 'offline') { action.disabled = false; action.textContent = 'Retry'; }
  }
}

function applyUpdate() {
  action.disabled = true;
  if (waiting) waiting.postMessage({ type: 'skip-waiting' });
  else location.reload();
}

// initPWA wires the banner and the service worker.
export function initPWA() {
  onReachability((ok) => {
    if (!ok) show('offline');
    else if (mode === 'offline') {
      show(waiting ? 'update' : '');
      window.dispatchEvent(new Event('cerberus:visible')); // views refetch
    }
  });
  window.addEventListener('online', () => { if (mode === 'offline') retry(); });
  register();
}

async function register() {
  const sw = navigator.serviceWorker;
  // Service workers need a secure context (HTTPS or localhost).
  if (!sw || !window.isSecureContext) return;
  let reg;
  try {
    reg = await sw.register('/sw.js', { scope: '/' });
  } catch {
    return; // an older daemon without /sw.js, or blocked
  }
  if (!reg) return; // some embedders stub register() out
  const hadController = !!sw.controller;
  const check = () => {
    if (updateReady(reg, hadController || !!sw.controller)) { waiting = reg.waiting; show('update'); }
  };
  reg.addEventListener('updatefound', () => {
    const w = reg.installing;
    if (w) w.addEventListener('statechange', check);
  });
  check();
  // The new worker took over after "Reload to update": load the new build.
  let reloading = false;
  sw.addEventListener('controllerchange', () => {
    if (!hadController || reloading) return;
    reloading = true;
    location.reload();
  });
  // Look for a new build when the tab comes back (no timer).
  window.addEventListener('cerberus:visible', () => { reg.update().catch(() => {}); });
}
