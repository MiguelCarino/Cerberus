// Login view (#/login, also served at /login): user name and password, one
// generic error line, the lockout wait, and "Sign in with the admin token"
// as a disclosure under the form (open, with a note, while no accounts
// exist). A successful sign-in reloads the page at the role's home, so no
// state of a previous person stays in memory.

import { apiResult } from '../api.js';
import { h } from '../dom.js';
import { loginErrorText, tokenErrorText, cleanName, normMe } from '../users.js';
import { hardGo } from './accountmenu.js';

export function mount(root) {
  let gone = false;
  const err = h('p', { class: 'login-err', role: 'alert', hidden: true });
  const name = h('input', {
    class: 'input', type: 'text', name: 'username', autocomplete: 'username', autocapitalize: 'off', spellcheck: 'false',
    required: true, maxlength: '32', 'aria-label': 'User name',
  });
  const pw = h('input', {
    class: 'input', type: 'password', name: 'password', autocomplete: 'current-password', required: true, 'aria-label': 'Password',
  });
  const show = h('button', { class: 'btn btn-ghost login-show', type: 'button', 'aria-pressed': 'false', 'aria-label': 'Show password', onclick: () => {
    const on = pw.type === 'password';
    pw.type = on ? 'text' : 'password';
    show.setAttribute('aria-pressed', String(on));
    show.textContent = on ? 'Hide' : 'Show';
  } }, 'Show');
  const go = h('button', { class: 'btn btn-primary btn-big', type: 'submit' }, 'Sign in');
  const form = h('form', { class: 'login-form', onsubmit: (e) => { e.preventDefault(); signIn(); } },
    h('label', { class: 'field' }, h('span', { class: 'field-label', text: 'User name' }), name),
    h('label', { class: 'field' }, h('span', { class: 'field-label', text: 'Password' }),
      h('span', { class: 'login-pw' }, pw, show)),
    err,
    go);

  // ---------- the admin token (break-glass and first run) ----------
  const tokErr = h('p', { class: 'login-err', role: 'alert', hidden: true });
  const tok = h('input', {
    class: 'input mono', type: 'password', autocomplete: 'off', autocapitalize: 'off', spellcheck: 'false',
    placeholder: 'Paste the token', 'aria-label': 'Admin token',
  });
  const tokGo = h('button', { class: 'btn', type: 'submit' }, 'Sign in with the token');
  const noUsers = h('p', { class: 'alert info login-first', hidden: true, text: 'No accounts yet. Sign in with the admin token to create the first admin.' });
  const disclosure = h('details', { class: 'login-token' },
    h('summary', { text: 'Sign in with the admin token' }),
    h('form', { class: 'login-form', onsubmit: (e) => { e.preventDefault(); signInToken(); } },
      h('p', { class: 't-sec' }, 'Run ', h('code', { text: 'cerberus url' }), ' on the host for a link with the token, or paste the token from ',
        h('code', { text: 'config.json' }), '.'),
      h('label', { class: 'field' }, h('span', { class: 'field-label', text: 'Admin token' }), tok),
      tokErr,
      h('div', { class: 'form-actions' }, tokGo)));

  const card = h('div', { class: 'login-card' },
    h('h1', { class: 'view-title', text: 'Sign in' }),
    h('p', { class: 't-sec', text: 'Cerberus on this host.' }),
    noUsers,
    form,
    disclosure);
  root.append(h('section', { class: 'view view-pad view-login' }, card));

  const say = (el, text) => { el.textContent = text; el.hidden = !text; };

  async function post(body) {
    try {
      return await apiResult('/api/login', { method: 'POST', body, quiet: true, noAuth: true });
    } catch {
      return { ok: false, status: 0, body: null, retryAfter: 0 };
    }
  }

  async function signIn() {
    const n = cleanName(name.value);
    if (!n || !pw.value) { say(err, 'Enter your user name and password.'); return; }
    go.disabled = true;
    say(err, '');
    const r = await post({ name: n, password: pw.value });
    if (gone) return;
    if (r.ok) { done(r.body); return; }
    go.disabled = false;
    say(err, loginErrorText(r.status, r.body, r.retryAfter));
    pw.value = '';
    pw.focus();
  }

  async function signInToken() {
    const t = tok.value.trim();
    if (!t) { say(tokErr, 'Paste the admin token.'); return; }
    tokGo.disabled = true;
    say(tokErr, '');
    let r = await post({ token: t });
    if (gone) return;
    // A daemon from before users has no /api/login: the ?token= link.
    if (r.status === 404 || r.status === 405) {
      const url = new URL('.', location.href);
      url.search = `?token=${encodeURIComponent(t)}`;
      url.hash = '';
      location.href = url.href;
      return;
    }
    if (r.ok) { done(r.body); return; }
    tokGo.disabled = false;
    say(tokErr, tokenErrorText(r.status, r.body, r.retryAfter));
  }

  // done opens where the person belongs: the first-run screen, the forced
  // password change, or the role's home.
  function done(body) {
    const me = normMe(body);
    hardGo(me.bootstrap ? '#/first-admin' : me.mustChange ? '#/password' : me.home);
  }

  // Whether accounts exist: with none, the token is the only way in.
  apiResult('/api/auth', { quiet: true, noAuth: true }).then((r) => {
    if (gone) return;
    // Already signed in with a password (a fresh load of #/login): home.
    if (r.ok && r.body && r.body.signed_in) { hardGo('#/'); return; }
    const none = r.ok && r.body && r.body.has_users === false;
    // An older daemon (no /api/auth) has only the token.
    const legacy = !r.ok && (r.status === 404 || r.status === 405);
    if (none || legacy) {
      noUsers.hidden = !none;
      disclosure.open = true;
      form.hidden = legacy || none;
      setTimeout(() => tok.focus(), 0);
    }
  }).catch(() => {});
  setTimeout(() => { if (!disclosure.open) name.focus(); }, 0);
  return () => { gone = true; };
}
