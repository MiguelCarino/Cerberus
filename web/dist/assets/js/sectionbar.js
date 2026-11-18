// Section bars: one horizontal row of pill tabs (the Carino DICOM section
// tabs), used for Settings' sections and the session's Terminal / Files /
// Preview / Replay / Flow / Diff. The main views are a side nav
// (sidenav.js), which reuses wireBar / revealCurrent for its phone row.
// Every bar is the same nav.sbar with a.sbar-tab (hash links,
// aria-current="page") or button.sbar-tab (role="tab", aria-selected) in
// it; styles in app.css "section bars".
//
// The bar never wraps: when the pills do not fit it scrolls sideways by
// itself (never the page), the current pill is scrolled into view, and an
// edge fade (data-fade-start / data-fade-end) hints at more on that side.

import { h, clear, keepFocus } from './dom.js';

// edgeFade says which edges hide more pills: {start, end}. slack absorbs
// sub-pixel scroll positions.
export function edgeFade(scrollLeft, scrollWidth, clientWidth, slack = 2) {
  const left = Math.max(0, Number(scrollLeft) || 0);
  const over = (Number(scrollWidth) || 0) - (Number(clientWidth) || 0);
  if (over <= slack) return { start: false, end: false };
  return { start: left > slack, end: left < over - slack };
}

// revealLeft is the scrollLeft that brings the pill at [itemLeft,
// itemLeft + itemWidth) into a viewWidth-wide bar now at scrollLeft, with
// pad pixels of room on that side (so the fade never covers it). The bar
// moves as little as it can; a pill already in view keeps scrollLeft.
export function revealLeft(scrollLeft, viewWidth, itemLeft, itemWidth, pad = 28) {
  const room = Math.max(0, (viewWidth - itemWidth) / 2);
  const p = Math.min(pad, room);
  if (itemLeft - p < scrollLeft) return Math.max(0, itemLeft - p);
  if (itemLeft + itemWidth + p > scrollLeft + viewWidth) return Math.max(0, itemLeft + itemWidth + p - viewWidth);
  return scrollLeft;
}

// nextTabIndex is the tab a key moves to in a role="tablist" bar (the
// arrows wrap; Home and End go to the ends), or -1 for any other key.
export function nextTabIndex(key, at, count) {
  if (count <= 0) return -1;
  if (key === 'ArrowRight') return (at + 1) % count;
  if (key === 'ArrowLeft') return (at - 1 + count) % count;
  if (key === 'Home') return 0;
  if (key === 'End') return count - 1;
  return -1;
}

const CURRENT = '.sbar-tab[aria-current], .sbar-tab[aria-selected="true"], .side-link[aria-current]';

// paintFade sets the bar's edge-fade marks from where it is scrolled.
export function paintFade(bar) {
  const f = edgeFade(bar.scrollLeft, bar.scrollWidth, bar.clientWidth);
  bar.toggleAttribute('data-fade-start', f.start);
  bar.toggleAttribute('data-fade-end', f.end);
}

// revealCurrent scrolls the bar (only the bar) so the current pill shows.
export function revealCurrent(bar) {
  const cur = bar.querySelector(CURRENT);
  if (cur && !cur.hidden) bar.scrollLeft = revealLeft(bar.scrollLeft, bar.clientWidth, cur.offsetLeft, cur.offsetWidth);
  paintFade(bar);
}

// wireBar follows the bar's scroll and size for the edge fade. Once per bar.
export function wireBar(bar) {
  if (bar.dataset.sbarWired) return bar;
  bar.dataset.sbarWired = '1';
  bar.addEventListener('scroll', () => paintFade(bar), { passive: true });
  if (typeof ResizeObserver === 'function') new ResizeObserver(() => revealCurrent(bar)).observe(bar);
  return bar;
}

// wireTabKeys gives a role="tablist" bar the arrow, Home and End keys and
// a roving tabindex: only the selected tab is in the Tab order.
export function wireTabKeys(bar) {
  const tabs = () => [...bar.querySelectorAll('.sbar-tab')].filter((t) => !t.hidden && !t.classList.contains('disabled'));
  const sync = () => {
    for (const t of bar.querySelectorAll('[role="tab"]')) t.tabIndex = t.getAttribute('aria-selected') === 'true' ? 0 : -1;
  };
  bar.addEventListener('keydown', (e) => {
    const list = tabs();
    const i = nextTabIndex(e.key, list.indexOf(document.activeElement), list.length);
    if (i < 0 || !list.length) return;
    e.preventDefault();
    list[i].focus();
    if (list[i].getAttribute('role') === 'tab') list[i].click();
  });
  return sync;
}

// tabNode builds one pill link: {key, label, href, current, title, extra}.
function tabNode(it) {
  return h('a', {
    class: 'sbar-tab', href: it.href, 'aria-current': it.current ? 'page' : null, title: it.title || null,
    'data-fkey': `sbar:${it.key}`, 'data-tab': it.tab || null,
  }, h('span', { class: 'sbar-label', text: it.label }), it.extra || null);
}

// setSections redraws a bar's pills: items are tab specs or the string
// 'sep' for a thin divider. Keyboard focus on a pill survives the redraw.
export function setSections(bar, items) {
  keepFocus(bar, () => {
    clear(bar).append(...items.filter(Boolean).map((it) => (it === 'sep'
      ? h('span', { class: 'sbar-sep', 'aria-hidden': 'true' })
      : tabNode(it))));
  });
  requestAnimationFrame(() => revealCurrent(bar));
  return bar;
}

// sectionBar is a new bar of links: {label, items, cls}.
export function sectionBar({ label, items = [], cls = '' }) {
  const bar = h('nav', { class: `sbar${cls ? ` ${cls}` : ''}`, 'aria-label': label });
  wireBar(bar);
  return setSections(bar, items);
}
