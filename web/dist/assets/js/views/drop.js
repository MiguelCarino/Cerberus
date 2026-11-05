// Getting files in: the drop-anywhere overlay (desktop only), the picker
// button and clipboard paste.
//
// Views say where a drop goes by pushing a target ({ dest(), onFiles }):
// the session view pushes its workspace, Sessions pushes "new task", and the
// New task form, while open, pushes itself on top. The last pushed wins.
// Dragging files over the window shows a full-screen overlay naming that
// destination. The overlay is a manual popover so it sits above an open
// modal <dialog> (both live in the top layer; the later one is on top).
// Nothing runs between drags: the listeners only react to drag events.

import { h, toast } from '../dom.js';
import { dropPrompt, isFileDrag, pastedName } from '../files.js';

const targets = [];

// pushDropTarget registers where drops go until the returned function is
// called. target: { dest: () => ({ kind, name, dir }), onFiles(files) }.
export function pushDropTarget(target) {
  targets.push(target);
  return () => {
    const i = targets.lastIndexOf(target);
    if (i >= 0) targets.splice(i, 1);
  };
}

const top = () => targets[targets.length - 1] || null;

// Phones and tablets have no drag and drop worth the name: the picker is
// their path. A fine pointer that can hover means a desktop.
const desktop = typeof matchMedia === 'function' ? matchMedia('(hover: hover) and (pointer: fine)') : { matches: true };

let overlay = null;
let overlayText = null;
let depth = 0;
let shown = false;

function ensureOverlay() {
  if (overlay) return;
  overlayText = h('p', { class: 'drop-title' });
  overlay = h('div', { class: 'drop-overlay', popover: 'manual', 'aria-hidden': 'true' },
    h('div', { class: 'drop-box' }, dropIcon(), overlayText,
      h('p', { class: 'drop-sub', text: 'Release to upload. Esc cancels.' })));
  document.body.append(overlay);
}

function showOverlay() {
  const t = top();
  if (!t) return;
  ensureOverlay();
  overlayText.textContent = dropPrompt(t.dest());
  if (shown) return;
  shown = true;
  // An open modal makes everything outside it inert, a popover included:
  // inside the topmost open dialog the overlay can still take the drop.
  const dialogs = document.querySelectorAll('dialog[open]');
  const host = dialogs.length ? dialogs[dialogs.length - 1] : document.body;
  if (overlay.parentNode !== host) host.append(overlay);
  if (overlay.showPopover) { try { overlay.showPopover(); } catch { /* already open */ } }
  overlay.classList.add('show');
}

function hideOverlay() {
  depth = 0;
  if (!shown) return;
  shown = false;
  overlay.classList.remove('show');
  if (overlay.hidePopover) { try { overlay.hidePopover(); } catch { /* not open */ } }
}

// outsideWindow: the pointer left the window (some browsers give every
// dragleave a null relatedTarget, so the position decides).
function outsideWindow(e) {
  return e.clientX <= 0 || e.clientY <= 0 || e.clientX >= window.innerWidth || e.clientY >= window.innerHeight;
}

// initDrop wires the window's drag events once.
export function initDrop() {
  window.addEventListener('dragenter', (e) => {
    if (!isFileDrag(e.dataTransfer && e.dataTransfer.types)) return;
    depth++;
    if (desktop.matches && top()) showOverlay();
  });
  window.addEventListener('dragover', (e) => {
    if (!isFileDrag(e.dataTransfer && e.dataTransfer.types)) return;
    // Always take file drags: a file dropped on a page with no target
    // would otherwise make the browser navigate away to open it.
    e.preventDefault();
    e.dataTransfer.dropEffect = top() ? 'copy' : 'none';
  });
  window.addEventListener('dragleave', (e) => {
    if (!isFileDrag(e.dataTransfer && e.dataTransfer.types)) return;
    depth = Math.max(0, depth - 1);
    if (depth === 0 || outsideWindow(e)) hideOverlay();
  });
  window.addEventListener('drop', (e) => {
    if (!isFileDrag(e.dataTransfer && e.dataTransfer.types)) return;
    e.preventDefault();
    hideOverlay();
    const files = [...(e.dataTransfer.files || [])];
    const t = top();
    if (!files.length) return;
    if (!t) { toast('Open a session, or Sessions for a new task, to drop files', 'warn'); return; }
    t.onFiles(files);
  });
  window.addEventListener('dragend', hideOverlay);
  // Capture phase, and the key is used up: Esc closes the drop target, not
  // the New task dialog underneath it.
  window.addEventListener('keydown', (e) => {
    if (e.key !== 'Escape' || !shown) return;
    e.preventDefault();
    e.stopPropagation();
    hideOverlay();
  }, true);
}

// ---------- picker ----------

// pickerButton is a button that opens the file picker (several files; on a
// phone this offers the camera, the gallery and files) and hands the choice
// to onFiles. The input lives inside the button's wrapper, so iOS keeps it.
export function pickerButton(onFiles, { label = 'Attach', cls = 'btn', title = 'Attach files', icon = true } = {}) {
  const input = h('input', { type: 'file', multiple: true, hidden: true, tabindex: '-1' });
  input.addEventListener('change', () => {
    const files = [...(input.files || [])];
    input.value = '';
    if (files.length) onFiles(files);
  });
  const btn = h('button', { class: cls, type: 'button', title, 'aria-label': title, onclick: () => input.click() },
    icon ? clipIcon() : null, label ? h('span', { class: icon ? 'pick-label' : '', text: label }) : null);
  return h('span', { class: 'pick-wrap' }, btn, input);
}

// ---------- paste ----------

// onPasteFiles calls onFiles with the files in a paste on el (screenshots,
// files copied in a file manager), renamed when the browser gave a generic
// "image.png". A text-only paste is left alone.
export function onPasteFiles(el, onFiles) {
  el.addEventListener('paste', (e) => {
    const list = [...((e.clipboardData && e.clipboardData.files) || [])];
    if (!list.length) return;
    e.preventDefault();
    onFiles(list.map(renamePasted));
  });
}

function renamePasted(f) {
  const name = pastedName(f.name, f.type);
  if (name === f.name) return f;
  try { return new File([f], name, { type: f.type, lastModified: f.lastModified }); } catch { return f; }
}

// ---------- icons (inline SVG, currentColor) ----------

function svg(d, size = 18) {
  const ns = 'http://www.w3.org/2000/svg';
  const el = document.createElementNS(ns, 'svg');
  el.setAttribute('viewBox', '0 0 24 24');
  el.setAttribute('width', String(size));
  el.setAttribute('height', String(size));
  el.setAttribute('aria-hidden', 'true');
  el.setAttribute('fill', 'none');
  el.setAttribute('stroke', 'currentColor');
  el.setAttribute('stroke-width', '2');
  el.setAttribute('stroke-linecap', 'round');
  el.setAttribute('stroke-linejoin', 'round');
  const p = document.createElementNS(ns, 'path');
  p.setAttribute('d', d);
  el.append(p);
  return el;
}

export const clipIcon = () => svg('M21 11.5l-8.6 8.6a5.5 5.5 0 0 1-7.8-7.8l8.6-8.6a3.7 3.7 0 0 1 5.2 5.2l-8.6 8.6a1.8 1.8 0 0 1-2.6-2.6l7.9-7.9');
export const folderIcon = () => svg('M3 6.5A1.5 1.5 0 0 1 4.5 5h4.2l2 2.2h8.8A1.5 1.5 0 0 1 21 8.7v9.8a1.5 1.5 0 0 1-1.5 1.5h-15A1.5 1.5 0 0 1 3 18.5z', 16);
export const fileIcon = () => svg('M14 3H6.5A1.5 1.5 0 0 0 5 4.5v15A1.5 1.5 0 0 0 6.5 21h11a1.5 1.5 0 0 0 1.5-1.5V8zM14 3v5h5', 16);
const dropIcon = () => svg('M12 3v12m0 0l-4.5-4.5M12 15l4.5-4.5M4 17v2.5A1.5 1.5 0 0 0 5.5 21h13a1.5 1.5 0 0 0 1.5-1.5V17', 48);
