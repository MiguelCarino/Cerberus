// A small menu under a button ("Always ▾" on approvals, "Always allow ▾"
// on chat cards): one at a time, closed by a pick, Escape, a click
// elsewhere, scrolling or resizing. Arrow keys move between entries. It
// lives in <body> with fixed positioning, kept inside the viewport (phones
// at 360 px too), so a list row's overflow cannot clip it.

import { h } from '../dom.js';

let open = null; // {menu, anchor, off}

export function closeMenu() {
  if (!open) return;
  const { menu, anchor, off } = open;
  open = null;
  off();
  menu.remove();
  anchor.setAttribute('aria-expanded', 'false');
}

// openMenu shows items ([{label, title}]) under anchor; onPick(item) runs
// for the chosen one. A second call on the same anchor just closes it.
export function openMenu(anchor, items, onPick) {
  const same = open && open.anchor === anchor;
  closeMenu();
  if (same || !items.length) return;
  const buttons = items.map((it) => h('button', {
    class: 'am-item', type: 'button', role: 'menuitem', title: it.title || null,
    onclick: (e) => { e.stopPropagation(); closeMenu(); onPick(it); },
  }, it.label));
  const menu = h('div', { class: 'am-menu', role: 'menu' }, buttons);
  document.body.append(menu);
  place(menu, anchor);
  anchor.setAttribute('aria-expanded', 'true');
  const onDown = (e) => { if (!menu.contains(e.target) && !anchor.contains(e.target)) closeMenu(); };
  const onKey = (e) => {
    if (e.key === 'Escape') { e.preventDefault(); closeMenu(); anchor.focus(); return; }
    if (e.key !== 'ArrowDown' && e.key !== 'ArrowUp') return;
    e.preventDefault();
    const i = buttons.indexOf(document.activeElement);
    const n = e.key === 'ArrowDown' ? (i + 1) % buttons.length : (i - 1 + buttons.length) % buttons.length;
    buttons[n].focus();
  };
  const onMove = () => closeMenu();
  document.addEventListener('pointerdown', onDown, true);
  document.addEventListener('keydown', onKey, true);
  window.addEventListener('resize', onMove);
  window.addEventListener('scroll', onMove, true);
  window.addEventListener('hashchange', onMove);
  open = {
    menu, anchor, off: () => {
      document.removeEventListener('pointerdown', onDown, true);
      document.removeEventListener('keydown', onKey, true);
      window.removeEventListener('resize', onMove);
      window.removeEventListener('scroll', onMove, true);
      window.removeEventListener('hashchange', onMove);
    },
  };
  buttons[0].focus({ preventScroll: true });
}

function place(menu, anchor) {
  const r = anchor.getBoundingClientRect();
  const vw = document.documentElement.clientWidth;
  const vh = window.innerHeight;
  const w = Math.min(menu.offsetWidth, vw - 16);
  const left = Math.max(8, Math.min(r.left, vw - w - 8));
  let top = r.bottom + 4;
  if (top + menu.offsetHeight > vh - 8 && r.top - menu.offsetHeight - 4 > 8) top = r.top - menu.offsetHeight - 4;
  menu.style.left = `${left}px`;
  menu.style.top = `${top}px`;
}
