// Desktop keyboard shortcuts: one keydown listener feeds control.js's
// shortcutStep. Navigation (g w, g s) works everywhere; list keys (j, k, x,
// Enter, ., Escape) go to the target the current view registers; n and /
// open Sessions first when another view has no handler. Nothing fires while
// typing in a field, in a focused terminal or with a dialog open.

import { h, openDialog } from './dom.js';
import { SHORTCUTS, shortcutStep, isTypingContext } from './control.js';

let target = null;
let pending = null;
let state = null;

// setShortcutTarget registers the current view's handlers ({down, up,
// select, open, bulk, search, new, escape}); null clears them.
export function setShortcutTarget(t) { target = t; }

// takePending returns (once) an action queued for the Sessions view, e.g.
// "new" when n was pressed on the wall.
export function takePending() { const p = pending; pending = null; return p; }

function typing(ev) {
  const el = ev.target instanceof Element ? ev.target : null;
  return isTypingContext({
    tag: el?.tagName, type: el?.getAttribute?.('type'), editable: !!el?.isContentEditable,
    inTerminal: !!el?.closest?.('.xterm'), dialogOpen: !!document.querySelector('dialog[open]'),
  });
}

let helpOpen = false;
export function showHelp() {
  if (helpOpen) return;
  helpOpen = true;
  openDialog(() => ({
    title: 'Keyboard shortcuts',
    sub: 'Not while typing or in a terminal',
    body: h('table', { class: 'kbd-table' }, h('tbody', {}, SHORTCUTS.map(([seq, , help]) => h('tr', {},
      h('td', {}, seq.split(' ').map((k, i) => [i ? ' then ' : '', h('kbd', { text: k === 'Escape' ? 'Esc' : k })])),
      h('td', { text: help }))))),
  })).then(() => { helpOpen = false; });
}

function run(action, ev) {
  switch (action) {
    case 'wall': location.hash = '#/wall'; return true;
    case 'sessions': location.hash = '#/sessions'; return true;
    case 'chat': location.hash = '#/chat'; return true;
    case 'help': showHelp(); return true;
    case 'new': case 'search':
      if (target?.[action]) { target[action](); return true; }
      pending = action;
      location.hash = '#/sessions';
      return true;
    default: {
      const fn = target?.[action];
      if (!fn) return false;
      // A handler may return false to let the key through (Escape).
      return fn(ev) !== false;
    }
  }
}

export function initShortcuts() {
  document.addEventListener('keydown', (ev) => {
    if (ev.defaultPrevented || ev.isComposing || typing(ev)) { state = null; return; }
    // Enter on a link or button keeps its own meaning.
    if (ev.key === 'Enter' && ev.target instanceof Element && ev.target.closest('a, button')) return;
    const step = shortcutStep(state, ev);
    state = step.state;
    if (step.state) { ev.preventDefault(); return; }
    if (step.action && run(step.action, ev)) ev.preventDefault();
  });
}
