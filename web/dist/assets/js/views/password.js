// Forced password change (#/password): a generated or admin-reset password
// must be replaced before anything else. New password twice; the current
// one is not asked (the fresh sign-in is the proof while must_change is
// set). The server re-issues the cookie, then the page opens the role's
// home.

import { apiResult } from '../api.js';
import { h } from '../dom.js';
import { store } from '../store.js';
import { newPasswordError, passwordChangeErrorText } from '../users.js';
import { hardGo, signOut } from './accountmenu.js';

export function mount(root) {
  let gone = false;
  const me = store.me();
  const name = (me && me.user && me.user.name) || '';
  const err = h('p', { class: 'login-err', role: 'alert', hidden: true });
  // A hidden user name helps password managers save the new password.
  const user = h('input', { type: 'text', autocomplete: 'username', value: name, hidden: true, readonly: true, 'aria-hidden': 'true', tabindex: '-1' });
  const pw = h('input', { class: 'input', type: 'password', autocomplete: 'new-password', 'aria-label': 'New password' });
  const pw2 = h('input', { class: 'input', type: 'password', autocomplete: 'new-password', 'aria-label': 'New password again' });
  const go = h('button', { class: 'btn btn-primary btn-big', type: 'submit' }, 'Change password');

  root.append(h('section', { class: 'view view-pad view-login' },
    h('form', { class: 'login-card', onsubmit: (e) => { e.preventDefault(); change(); } },
      h('h1', { class: 'view-title', text: 'New password' }),
      h('p', { class: 't-sec', text: `${name ? `Hello, ${name}. ` : ''}Your password was set by an admin. Choose your own before you go on.` }),
      user,
      h('label', { class: 'field' }, h('span', { class: 'field-label', text: 'New password' }), pw,
        h('span', { class: 'field-hint', text: 'At least 10 characters, not your user name.' })),
      h('label', { class: 'field' }, h('span', { class: 'field-label', text: 'New password again' }), pw2),
      err,
      go,
      h('button', { class: 'btn btn-ghost', type: 'button', onclick: () => signOut() }, 'Sign out instead'))));

  const say = (text) => { err.textContent = text; err.hidden = !text; };

  async function change() {
    const problem = newPasswordError(pw.value, pw2.value, name);
    if (problem) { say(problem); return; }
    go.disabled = true;
    say('');
    try {
      const r = await apiResult('/api/me/password', { method: 'POST', body: { new: pw.value }, quiet: true });
      if (gone) return;
      if (r.ok) { hardGo(store.home()); return; }
      say(passwordChangeErrorText(r.status, r.body));
    } catch (e) {
      if (!gone) say(e && e.message ? e.message : 'Cannot reach Cerberus.');
    }
    go.disabled = false;
  }

  setTimeout(() => pw.focus(), 0);
  return () => { gone = true; };
}
