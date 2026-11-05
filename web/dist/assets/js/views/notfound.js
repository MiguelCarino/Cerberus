// Not found (#/<anything unknown>, or a view this role cannot use): says so,
// shows the address, and offers the role's home.

import { h } from '../dom.js';
import { store } from '../store.js';

export function mount(root, r = {}) {
  const home = store.home();
  root.append(h('section', { class: 'view view-pad view-notfound' },
    h('div', { class: 'nf-card' },
      h('p', { class: 'nf-code', 'aria-hidden': 'true', text: '404' }),
      h('h1', { class: 'view-title', text: 'Nothing here' }),
      h('p', { class: 't-sec', text: 'This page does not exist, or your account cannot open it.' }),
      r.path ? h('p', { class: 'nf-path mono', text: r.path }) : null,
      h('div', { class: 'form-actions' },
        h('a', { class: 'btn btn-primary', href: home }, 'Go home'),
        h('a', { class: 'btn btn-ghost', href: '#/account' }, 'Account')))));
  return () => {};
}
