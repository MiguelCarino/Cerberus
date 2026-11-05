// Uploads with progress: XHR (fetch has no upload progress), one request
// per file into a session so each file has its own progress bar, cancel
// button and error, and the upload tray that shows them, ending in one
// summary toast. Files go one at a time: a phone on a slow link gets every
// byte for the file it is sending, and the host never streams many at once.

import { notifyUnauthorized, messageOf, sid } from './api.js';
import { h, toast } from './dom.js';
import { store } from './store.js';
import { fmtBytes, uploadError, uploadSummary, destinationLabel, tooLargeFor, DEFAULT_UPLOAD_DIR } from './files.js';

// xhrUpload POSTs files as multipart (field "file", one part each).
// Returns { promise, abort }; the promise resolves (never rejects) with
// { ok, status, data, error, cancelled }.
export function xhrUpload(url, files, { onProgress } = {}) {
  const xhr = new XMLHttpRequest();
  const fd = new FormData();
  for (const f of files) fd.append('file', f, f.name);
  let cancelled = false;
  const promise = new Promise((resolve) => {
    xhr.open('POST', url);
    // Same CSRF marker as api.js: cookie-authenticated writes need it.
    xhr.setRequestHeader('X-Cerberus', '1');
    if (onProgress) xhr.upload.onprogress = (e) => onProgress(e.loaded, e.lengthComputable ? e.total : 0);
    xhr.onload = () => {
      const status = xhr.status;
      if (status === 401) notifyUnauthorized();
      if (status >= 200 && status < 300) {
        let data = null;
        try { data = JSON.parse(xhr.responseText); } catch { /* empty or text */ }
        resolve({ ok: true, status, data });
      } else {
        resolve({ ok: false, status, error: uploadError(status, messageOf(xhr.responseText, status, xhr.statusText)) });
      }
    };
    xhr.onerror = () => resolve({ ok: false, status: 0, error: uploadError(0, '') });
    xhr.onabort = () => resolve({ ok: false, status: 0, cancelled: true, error: cancelled ? 'cancelled' : 'aborted' });
    xhr.send(fd);
  });
  return { promise, abort: () => { cancelled = true; xhr.abort(); } };
}

// filesConfig is what /api/status says about uploads (when it says).
export function filesConfig() {
  const st = store.status();
  const f = (st && (st.files || st.Files)) || {};
  return {
    uploadDir: f.upload_dir || f.UploadDir || DEFAULT_UPLOAD_DIR,
    maxFileMiB: +(f.max_file_mib || f.MaxFileMiB || 0),
  };
}

// ---------- the tray ----------

let tray = null;
let trayList = null;
let trayHead = null;
let hideTimer = 0;
let running = 0;
let queue = Promise.resolve();

function ensureTray() {
  if (tray) return;
  trayHead = h('span', { class: 'up-title', text: 'Uploads' });
  trayList = h('ul', { class: 'up-list' });
  tray = h('section', { class: 'up-tray', role: 'region', 'aria-label': 'Uploads', hidden: true },
    h('div', { class: 'up-head' }, trayHead,
      h('button', { class: 'up-x', type: 'button', 'aria-label': 'Hide uploads', onclick: hideTray }, '✕')),
    trayList);
  document.body.append(tray);
}

function hideTray() {
  clearTimeout(hideTimer);
  if (!tray) return;
  // Rows still uploading stay (hiding does not cancel them).
  for (const li of [...trayList.children]) if (li.dataset.done) li.remove();
  tray.hidden = trayList.children.length === 0;
}

function trayRow(file) {
  const bar = h('span', {});
  const state = h('span', { class: 'up-state', text: 'waiting' });
  const cancel = h('button', { class: 'up-cancel', type: 'button', 'aria-label': `Cancel ${file.name}` }, '✕');
  const li = h('li', { class: 'up-row' },
    h('div', { class: 'up-line' },
      h('span', { class: 'up-name', text: file.name, title: file.name }),
      h('span', { class: 'up-size', text: fmtBytes(file.size) }), cancel),
    h('div', { class: 'progress fluid' }, bar), state);
  bar.style.width = '0%';
  // Before its turn a row cancels itself; during it, the cancel aborts the XHR.
  let abort = null;
  const row = {
    li,
    setAbort(fn) { abort = fn; },
    progress(frac) { bar.style.width = `${Math.round(frac * 100)}%`; state.textContent = `${Math.round(frac * 100)}%`; },
    finish(res) {
      li.dataset.done = '1';
      cancel.remove();
      li.classList.add(res.ok ? 'ok' : res.cancelled ? 'cancelled' : 'err');
      state.textContent = res.ok ? 'done' : res.cancelled ? 'cancelled' : res.error;
      if (res.ok) bar.style.width = '100%';
    },
  };
  cancel.addEventListener('click', () => (abort ? abort() : row.finish({ ok: false, cancelled: true })));
  return row;
}

// uploadToSession uploads files into a session's workspace (dir "" = the
// inbox), showing them in the tray. Resolves with per-file results
// { name, ok, error, cancelled, entries } once all are done, after the
// summary toast. Also fires 'cerberus:files-changed' ({ id }) on window
// so an open Files tab refreshes.
export async function uploadToSession(id, files, { dir = '', name = '' } = {}) {
  files = [...(files || [])];
  if (!files.length) return [];
  ensureTray();
  clearTimeout(hideTimer);
  const label = destinationLabel({ kind: 'session', name, dir, uploadDir: filesConfig().uploadDir });
  trayHead.textContent = `Uploading to ${label}`;
  tray.hidden = false;
  const { maxFileMiB } = filesConfig();
  const url = `${sid(id)}/files${dir ? `?dir=${encodeURIComponent(dir)}` : ''}`;
  const rows = files.map((f) => {
    const row = trayRow(f);
    trayList.append(row.li);
    return row;
  });
  const results = [];
  running++;
  // One upload at a time across calls: a second drop queues behind the first.
  const prev = queue;
  let release;
  queue = new Promise((r) => { release = r; });
  await prev;
  for (let i = 0; i < files.length; i++) {
    const f = files[i];
    const row = rows[i];
    let res;
    if (row.li.dataset.done) { results.push({ name: f.name, ok: false, cancelled: true }); continue; }
    if (maxFileMiB > 0 && f.size > maxFileMiB * 1024 * 1024) {
      res = { ok: false, error: `too large (limit ${maxFileMiB} MiB)` };
    } else {
      const up = xhrUpload(url, [f], { onProgress: (l, t) => row.progress(t ? l / t : 0) });
      row.setAbort(up.abort);
      row.progress(0);
      res = await up.promise;
    }
    row.finish(res);
    results.push({ name: f.name, ok: res.ok, error: res.error, cancelled: !!res.cancelled, entries: Array.isArray(res.data) ? res.data : [] });
  }
  release();
  running--;
  const sum = uploadSummary(results, label);
  toast(sum.text, sum.kind);
  window.dispatchEvent(new CustomEvent('cerberus:files-changed', { detail: { id } }));
  // A clean run tidies itself away; errors stay until dismissed.
  if (!running && sum.kind === 'ok') hideTimer = setTimeout(hideTray, 4000);
  if (!running) trayHead.textContent = 'Uploads';
  return results;
}

// uploadToStaging sends New task attachments to /api/uploads: a new
// staging folder, or more files for staging (an id) when given. onProgress
// gets (loaded, total). Returns { promise, abort }; the promise resolves
// { ok, staging, entries, error, cancelled }.
export function uploadToStaging(files, staging = '', { onProgress } = {}) {
  const url = staging ? `/api/uploads?staging=${encodeURIComponent(staging)}` : '/api/uploads';
  const up = xhrUpload(url, files, { onProgress });
  return {
    abort: up.abort,
    promise: up.promise.then((res) => {
      if (!res.ok) return res;
      const d = res.data || {};
      const id = d.staging || d.Staging || '';
      if (!id) return { ok: false, error: 'the server returned no staging id' };
      return { ok: true, staging: id, entries: d.entries || d.Entries || [] };
    }),
  };
}
