// Files tab of the session view: browse the session's workspace (folders
// first, breadcrumbs), preview a file (image, text and code in monospace up
// to 256 KiB, PDF in a sandboxed frame, audio, video), download it, delete
// it, make folders, upload into the folder on screen, tell the agent about a
// file, and hand it to Carino Files over the bridge.
//
// The listing is fetched when the tab opens, after uploads and deletes, on
// Refresh and when the page becomes visible again; nothing polls.

import { api, post } from '../api.js';
import { h, clear, openDialog, confirmDialog, toast, errText } from '../dom.js';
import { relTime } from '../util.js';
import {
  fmtBytes, relCrumbs, joinRel, parentRel, normRel, previewKind, filesPath, rawPath, deletePath,
  folderNameError, sendToAgentText, deletesWithContents, TEXT_PREVIEW_BYTES,
} from '../files.js';
import { filesConfig } from '../upload.js';
import { pickerButton, folderIcon, fileIcon } from './drop.js';
import { sendToCarinoFiles } from './bridge.js';

// filesPanel builds the tab for session id.
//   upload(files, dir)  uploads into dir (the caller owns the tray)
//   sendText(text)      sends text to the agent (live input or follow-up)
// Returns { el, dir(), open(), close(), refresh(), destroy() }.
//   readOnly           a watcher (viewer role): browse and download only
export function filesPanel(id, { upload, sendText, readOnly = false }) {
  let dir = '.';
  let listing = null;
  let selected = null; // Entry being previewed
  let listCtl = null;  // aborts a listing fetch in flight
  let textCtl = null;  // aborts a text preview fetch in flight (a listing refresh leaves it alone)
  let active = false;

  // Delete the folder on screen (shown below the workspace root).
  const delFolder = h('button', { class: 'btn btn-sm btn-danger', type: 'button', hidden: true, 'data-write': '',
    onclick: () => remove({ Dir: true, Rel: dir, Name: dir.split('/').pop() }) }, 'Delete folder');
  const crumbs = h('nav', { class: 'fx-crumbs', 'aria-label': 'Folder' });
  const list = h('div', { class: 'fx-list', role: 'list' });
  const preview = h('div', { class: 'fx-preview', hidden: true });
  const status = h('p', { class: 'fx-status', hidden: true });
  const el = h('div', { class: `fx${readOnly ? ' fx-ro' : ''}`, hidden: true },
    h('div', { class: 'fx-bar' }, crumbs,
      h('div', { class: 'fx-tools' },
        readOnly ? null : pickerButton((files) => upload(files, dir), { label: 'Upload', cls: 'btn btn-sm', title: 'Upload files into this folder' }),
        readOnly ? null : h('button', { class: 'btn btn-sm', type: 'button', onclick: newFolder }, 'New folder'),
        delFolder,
        h('button', { class: 'btn btn-sm btn-ghost', type: 'button', 'aria-label': 'Refresh', title: 'Refresh', onclick: () => load() }, '↻'))),
    h('div', { class: 'fx-body' }, h('div', { class: 'fx-col' }, status, list), preview));

  async function load(next = dir) {
    if (listCtl) listCtl.abort();
    listCtl = new AbortController();
    const want = normRel(next) || '.';
    try {
      const l = await api(filesPath(id, want), { signal: listCtl.signal });
      dir = (l && l.Rel) || want;
      listing = l || { Entries: [] };
      renderList();
    } catch (e) {
      if (e.name === 'AbortError') return;
      // The folder vanished (deleted by the agent): fall back to its parent.
      if (want !== '.' && /not found/i.test(errText(e))) { load(parentRel(want)); return; }
      listing = null;
      clear(list);
      showStatus(errText(e), true);
    }
  }

  function showStatus(text, err = false) {
    status.textContent = text;
    status.classList.toggle('err', err);
    status.hidden = !text;
  }

  function renderList() {
    delFolder.hidden = readOnly || dir === '.';
    clear(crumbs).append(...relCrumbs(dir).map((c, i, all) => (i === all.length - 1
      ? h('span', { class: 'fx-crumb current', 'aria-current': 'location', text: c.name })
      : h('button', { class: 'fx-crumb', type: 'button', onclick: () => go(c.rel) }, c.name))));
    const entries = (listing && listing.Entries) || [];
    showStatus(!entries.length ? 'This folder is empty. Drop files here, or use Upload.'
      : listing.Truncated ? `Showing the first ${entries.length} entries.` : '');
    // append() would print a null as the text "null": the ".." row is
    // added only below the workspace root.
    const up = dir !== '.' ? [h('button', { class: 'fx-row fx-up', type: 'button', role: 'listitem', onclick: () => go(parentRel(dir)) },
      folderIcon(), h('span', { class: 'fx-name', text: '..' }), h('span', { class: 'fx-meta', text: 'up' }))] : [];
    clear(list).append(...up, ...entries.map(row));
    if (selected && !entries.some((e) => e.Rel === selected.Rel)) closePreview();
  }

  function row(e) {
    const meta = e.Dir ? relTime(e.Mod) : `${fmtBytes(e.Size)} · ${relTime(e.Mod)}`;
    const tags = [e.Link && !e.Outside ? 'link' : null, e.Outside ? 'outside' : null].filter(Boolean);
    return h('button', {
      class: `fx-row${selected && selected.Rel === e.Rel ? ' sel' : ''}${e.Outside ? ' outside' : ''}`,
      type: 'button', role: 'listitem', disabled: e.Outside, 'data-rel': e.Rel,
      title: e.Outside ? 'A link pointing outside the workspace: listed, never opened' : e.Name,
      onclick: () => (e.Dir ? go(e.Rel) : openPreview(e)),
    },
    e.Dir ? folderIcon() : fileIcon(),
    h('span', { class: 'fx-name', text: e.Name }),
    tags.map((t) => h('span', { class: 'fx-tag', text: t })),
    h('span', { class: 'fx-meta', text: meta }));
  }

  function go(rel) {
    closePreview();
    load(rel);
  }

  // ---------- preview ----------

  function closePreview() {
    if (textCtl) textCtl.abort();
    selected = null;
    clear(preview); // drops <video>/<audio>, which stops playback
    preview.hidden = true;
    el.classList.remove('fx-previewing');
    for (const r of list.querySelectorAll('.fx-row.sel')) r.classList.remove('sel');
  }

  function openPreview(e) {
    if (textCtl) textCtl.abort();
    selected = e;
    el.classList.add('fx-previewing');
    preview.hidden = false;
    for (const r of list.querySelectorAll('.fx-row')) r.classList.toggle('sel', r.dataset.rel === e.Rel);
    const src = rawPath(id, e.Rel);
    const kind = previewKind(e);
    const body = h('div', { class: `fx-view fx-${kind}` });
    clear(preview).append(
      h('div', { class: 'fx-phead' },
        h('button', { class: 'btn btn-ghost btn-sm fx-back', type: 'button', 'aria-label': 'Back to the folder', onclick: closePreview }, '←'),
        h('div', { class: 'fx-ptext' },
          h('div', { class: 'fx-pname', text: e.Name, title: e.Rel }),
          h('div', { class: 'fx-pmeta', text: [fmtBytes(e.Size), e.Mime, relTime(e.Mod)].filter(Boolean).join(' · ') }))),
      h('div', { class: 'fx-pactions' },
        h('a', { class: 'btn btn-sm', href: rawPath(id, e.Rel, true), download: e.Name }, 'Download'),
        readOnly ? null : h('button', { class: 'btn btn-sm btn-primary', type: 'button', onclick: () => sendToAgent(e) }, 'Send to agent'),
        h('button', { class: 'btn btn-sm', type: 'button', onclick: () => sendToCarinoFiles(src, e) }, 'Open in Carino Files'),
        readOnly ? null : h('button', { class: 'btn btn-sm btn-danger', type: 'button', onclick: () => remove(e) }, 'Delete')),
      body);
    switch (kind) {
      case 'image':
        body.append(h('img', { src, alt: e.Name, decoding: 'async' }));
        break;
      case 'pdf':
        // The browser's own viewer in a sandboxed frame; a browser that
        // refuses PDFs in sandboxed frames still has the link.
        body.append(h('iframe', { src, title: e.Name, sandbox: '', referrerpolicy: 'no-referrer' }),
          h('a', { class: 'fx-open', href: src, target: '_blank', rel: 'noopener' }, 'Open the PDF in a new tab ↗'));
        break;
      case 'audio':
        body.append(h('audio', { src, controls: true, preload: 'metadata' }));
        break;
      case 'video':
        body.append(h('video', { src, controls: true, preload: 'metadata', playsinline: true }));
        break;
      case 'text':
        loadText(e, src, body);
        break;
      default:
        body.append(h('p', { class: 'fx-none', text: 'No preview for this type. Download it, or hand it to Carino Files.' }));
    }
  }

  async function loadText(e, src, body) {
    const pre = h('pre', { class: 'code fx-text', text: 'Loading…' });
    const note = h('p', { class: 'fx-note', hidden: true });
    body.append(note, pre);
    textCtl = new AbortController();
    const signal = textCtl.signal;
    try {
      const r = await fetch(src, { credentials: 'same-origin', signal, headers: { Range: `bytes=0-${TEXT_PREVIEW_BYTES - 1}` } });
      if (!r.ok && r.status !== 206) throw new Error(`HTTP ${r.status}`);
      const { text, cut } = await readPrefix(r, TEXT_PREVIEW_BYTES);
      if (selected !== e) return;
      pre.textContent = text;
      if (cut || e.Size > TEXT_PREVIEW_BYTES) {
        note.textContent = `Showing the first ${fmtBytes(TEXT_PREVIEW_BYTES)} of ${fmtBytes(e.Size)}. Download it for the rest.`;
        note.hidden = false;
      }
    } catch (err) {
      if (err.name === 'AbortError') return;
      pre.textContent = `Could not load the preview: ${errText(err)}`;
    }
  }

  // ---------- actions ----------

  async function remove(e) {
    // Inside the inbox a folder goes with its contents; elsewhere only an
    // empty folder can be deleted (the server says so when it is not).
    const all = e.Dir && deletesWithContents(e.Rel, filesConfig().uploadDir);
    const ok = await confirmDialog({
      title: e.Dir ? 'Delete folder' : 'Delete file', ok: 'Delete', danger: true,
      message: all ? `Delete ${e.Rel} and everything in it? This cannot be undone.`
        : e.Dir ? `Delete the folder ${e.Rel}? Only an empty folder can be deleted here.`
          : `Delete ${e.Rel} from the workspace? This cannot be undone.`,
    });
    if (!ok) return;
    try {
      await api(deletePath(id, e.Rel, all), { method: 'DELETE' });
      toast(`Deleted ${e.Name}`, 'ok');
      closePreview();
      load(e.Dir && e.Rel === dir ? parentRel(dir) : dir);
    } catch (err) { toast(errText(err), 'err'); }
  }

  async function newFolder() {
    const name = await openDialog((close) => {
      const input = h('input', { class: 'input', placeholder: 'e.g. reference', autocapitalize: 'off', spellcheck: 'false' });
      const err = h('div', { class: 'alert err', hidden: true });
      const ok = () => {
        const msg = folderNameError(input.value);
        if (msg) { err.textContent = msg; err.hidden = false; return; }
        close(input.value.trim());
      };
      setTimeout(() => input.focus(), 0);
      return {
        title: 'New folder',
        sub: `Inside ${dir === '.' ? 'the workspace root' : dir}`,
        body: h('form', { class: 'task-form', onsubmit: (ev) => { ev.preventDefault(); ok(); } },
          h('label', { class: 'field' }, h('span', { class: 'field-label', text: 'Name' }), input), err,
          h('button', { type: 'submit', hidden: true })),
        foot: [
          h('button', { class: 'btn btn-ghost', type: 'button', onclick: () => close(null) }, 'Cancel'),
          h('button', { class: 'btn btn-primary', type: 'button', onclick: ok }, 'Create'),
        ],
      };
    });
    if (!name) return;
    const path = joinRel(dir, name);
    try {
      await post(`/api/sessions/${encodeURIComponent(id)}/files/mkdir`, { path });
      toast(`Created ${path}`, 'ok');
      load();
    } catch (err) { toast(errText(err), 'err'); }
  }

  async function sendToAgent(e) {
    const text = await openDialog((close) => {
      const msg = h('textarea', { class: 'textarea', rows: '3', value: sendToAgentText(e.Rel) });
      setTimeout(() => { msg.focus(); msg.setSelectionRange(msg.value.length, msg.value.length); }, 0);
      return {
        title: 'Send to agent',
        sub: 'A message naming the file; edit it first if you like',
        body: h('div', { class: 'task-form' }, h('label', { class: 'field' }, h('span', { class: 'field-label', text: 'Message' }), msg)),
        foot: [
          h('button', { class: 'btn btn-ghost', type: 'button', onclick: () => close(null) }, 'Cancel'),
          h('button', { class: 'btn btn-primary', type: 'button', onclick: () => close(msg.value.trim()) }, 'Send'),
        ],
      };
    });
    if (text) sendText(text);
  }

  // ---------- lifecycle ----------

  const onChanged = (ev) => { if (active && ev.detail && ev.detail.id === id) load(); };
  const onVisible = () => { if (active) load(); };
  window.addEventListener('cerberus:files-changed', onChanged);
  window.addEventListener('cerberus:visible', onVisible);

  return {
    el,
    dir: () => dir,
    open() { active = true; el.hidden = false; load(); },
    close() { active = false; el.hidden = true; closePreview(); if (listCtl) listCtl.abort(); },
    refresh: () => load(),
    destroy() {
      active = false;
      if (listCtl) listCtl.abort();
      if (textCtl) textCtl.abort();
      window.removeEventListener('cerberus:files-changed', onChanged);
      window.removeEventListener('cerberus:visible', onVisible);
    },
  };
}

// readPrefix reads at most max bytes of a response body as UTF-8 and stops
// the download there (servers that ignore Range still send only that much
// before the reader is cancelled).
async function readPrefix(r, max) {
  if (!r.body || !r.body.getReader) {
    const t = await r.text();
    return { text: t.slice(0, max), cut: t.length > max };
  }
  const reader = r.body.getReader();
  const chunks = [];
  let got = 0;
  let cut = false;
  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    const room = max - got;
    if (value.length >= room) {
      chunks.push(value.subarray(0, room));
      got += room;
      cut = value.length > room;
      reader.cancel().catch(() => {});
      break;
    }
    chunks.push(value);
    got += value.length;
  }
  const buf = new Uint8Array(got);
  let off = 0;
  for (const c of chunks) { buf.set(c, off); off += c.length; }
  return { text: new TextDecoder('utf-8').decode(buf), cut };
}
