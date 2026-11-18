// The side nav: the main views as a column of links left of main#app, as
// in Carino DICOM's sidebar (an icon and the label per link, the current
// one in the accent with a faint accent fill, Account at the foot). On
// phones the same links make one row above the content that scrolls
// sideways like a section bar: sectionbar.js's wireBar / revealCurrent
// give it the edge fade and keep the current link in view. app.js fills
// it per role from users.js mainTabs(); styles in app.css "side nav".

import { h, clear, keepFocus } from './dom.js';
import { wireBar, revealCurrent } from './sectionbar.js';

const SVG = 'http://www.w3.org/2000/svg';

// NAV_ICONS: one plain outline glyph per main view on a 24 px grid, drawn
// with currentColor strokes (no icon font, no request).
export const NAV_ICONS = {
  // a speech bubble
  chat: ['M21 15a2 2 0 0 1-2 2H7l-4 4V5a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2z'],
  // a terminal prompt
  sessions: ['M4 17l6-6-6-6', 'M12 19h8'],
  // two steps joined: a branch
  flows: ['M6 3v12', 'M18 9a3 3 0 1 0 0-6 3 3 0 0 0 0 6z', 'M6 21a3 3 0 1 0 0-6 3 3 0 0 0 0 6z', 'M18 9a9 9 0 0 1-9 9'],
  // a chip with pins
  models: ['M6 6h12v12H6z', 'M10 10h4v4h-4z', 'M10 2v4M14 2v4M10 18v4M14 18v4M2 10h4M2 14h4M18 10h4M18 14h4'],
  // a pulse line
  monitor: ['M22 12h-4l-3 9L9 3l-3 9H2'],
  // bars on an axis
  usage: ['M3 3v18h18', 'M8 17v-5', 'M13 17V7', 'M18 17v-8'],
  // three sliders
  settings: ['M4 21v-7M4 10V3M12 21v-9M12 8V3M20 21v-5M20 12V3', 'M1 14h6M9 8h6M17 16h6'],
  // a person
  account: ['M20 21a8 8 0 0 0-16 0', 'M12 13a5 5 0 1 0 0-10 5 5 0 0 0 0 10z'],
};

function navIcon(tab) {
  const svg = document.createElementNS(SVG, 'svg');
  for (const [k, v] of Object.entries({ viewBox: '0 0 24 24', fill: 'none', stroke: 'currentColor', 'stroke-width': '2',
    'stroke-linecap': 'round', 'stroke-linejoin': 'round', 'aria-hidden': 'true', focusable: 'false' })) svg.setAttribute(k, v);
  for (const d of NAV_ICONS[tab] || []) {
    const p = document.createElementNS(SVG, 'path');
    p.setAttribute('d', d);
    svg.append(p);
  }
  return svg;
}

// navItems turns mainTabs(me) rows ([tab, label, what it holds]) into the
// links' specs: {key, label, href, title}.
export function navItems(tabs) {
  return (tabs || []).map(([tab, label, sub]) => ({ key: tab, label, href: '#/' + tab, title: sub || '' }));
}

// navLink is one link: {key, label, href, title, extra}.
function navLink(it) {
  return h('a', { class: 'side-link', href: it.href, title: it.title || null, 'data-tab': it.key, 'data-fkey': `nav:${it.key}` },
    h('span', { class: 'nav-ico' }, navIcon(it.key)),
    h('span', { class: 'nav-label', text: it.label }),
    it.extra || null);
}

// wireSideNav follows the nav's sideways scroll and size (phones) for the
// edge fade. Once per nav.
export const wireSideNav = (nav) => wireBar(nav);

// setSideNav redraws the links. Keyboard focus on a link survives it.
export function setSideNav(nav, items) {
  keepFocus(nav, () => { clear(nav).append(...items.filter(Boolean).map(navLink)); });
  return nav;
}

// markSideNav lights the link of tab (aria-current="page") and, when the
// nav is a row, scrolls only the nav so that link shows.
export function markSideNav(nav, tab) {
  for (const a of nav.querySelectorAll('.side-link')) {
    if (a.dataset.tab === tab) a.setAttribute('aria-current', 'page');
    else a.removeAttribute('aria-current');
  }
  requestAnimationFrame(() => revealCurrent(nav));
}
