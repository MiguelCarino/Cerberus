// First run (#/first-admin): signed in with the admin token and no accounts
// yet. Asks for the first admin's name, display name and password (twice),
// creates the account (POST /api/users), signs in as it (POST /api/login)
// and opens the role's home. Existing sessions, chats and flows become
// this admin's.

import { apiResult } from '../api.js';
import { h } from '../dom.js';
import { nameError, cleanName, newPasswordError, displayError, normMe } from '../users.js';
import { hardGo } from './accountmenu.js';

export function mount(root) {
  let gone = false;
  const err = h('p', { class: 'login-err', role: 'alert', hidden: true });
  const name = h('input', { class: 'input', type: 'text', autocomplete: 'username', autocapitalize: 'off', spellcheck: 'false', maxlength: '32', 'aria-label': 'User name', placeholder: 'e.g. miguel' });
  const display = h('input', { class: 'input', type: 'text', autocomplete: 'name', maxlength: '60', 'aria-label': 'Display name', placeholder: 'Optional, e.g. Miguel' });
  const pw = h('input', { class: 'input', type: 'password', autocomplete: 'new-password', 'aria-label': 'Password' });
  const pw2 = h('input', { class: 'input', type: 'password', autocomplete: 'new-password', 'aria-label': 'Password again' });
  const go = h('button', { class: 'btn btn-primary btn-big', type: 'submit' }, 'Create admin and sign in');
  const field = (label, el, hint) => h('label', { class: 'field' }, h('span', { class: 'field-label', text: label }), el,
    hint ? h('span', { class: 'field-hint', text: hint }) : null);

  root.append(h('section', { class: 'view view-pad view-login' },
    h('form', { class: 'login-card', onsubmit: (e) => { e.preventDefault(); create(); } },
      h('h1', { class: 'view-title', text: 'Create the first admin' }),
      h('p', { class: 't-sec', text: 'You signed in with the admin token. Make your own account: you sign in with it from now on, and everything already on this host (sessions, chats, flows, devices) becomes yours. The token keeps working as a break-glass key.' }),
      field('User name', name, 'Lowercase letters, digits, dot, dash or underscore.'),
      field('Display name', display),
      field('Password', pw, 'At least 10 characters.'),
      field('Password again', pw2),
      err,
      go)));

  const say = (text) => { err.textContent = text; err.hidden = !text; };

  async function create() {
    const n = cleanName(name.value);
    const problem = nameError(n) || displayError(display.value) || newPasswordError(pw.value, pw2.value, n);
    if (problem) { say(problem); return; }
    go.disabled = true;
    say('');
    try {
      const r = await apiResult('/api/users', { method: 'POST', body: { name: n, display: display.value.trim() || undefined, role: 'admin', password: pw.value }, quiet: true });
      if (gone) return;
      if (!r.ok) {
        say(r.status === 409 ? 'That name is taken.' : r.message || `Could not create the account (${r.status}).`);
        go.disabled = false;
        return;
      }
      const s = await apiResult('/api/login', { method: 'POST', body: { name: n, password: pw.value }, quiet: true, noAuth: true });
      if (gone) return;
      if (!s.ok) {
        // The account exists; signing in by hand still works.
        hardGo('#/login');
        return;
      }
      const me = normMe(s.body);
      hardGo(me.mustChange ? '#/password' : me.home);
    } catch (e) {
      if (gone) return;
      say(e && e.message ? e.message : 'Cannot reach Cerberus.');
      go.disabled = false;
    }
  }

  setTimeout(() => name.focus(), 0);
  return () => { gone = true; };
}
