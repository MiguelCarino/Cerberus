// Account (#/account, every role): who is signed in and their role, change
// password, the browsers signed in to this account (this one marked) with
// sign out, their own paired devices (pair, revoke), the personal /v1 API
// key (admin and user: made once, shown once; revoke), the monthly budget
// and spend, and Sign out. Nothing polls: data loads on mount and when the
// tab becomes visible again.

import { api, apiResult, AuthError } from '../api.js';
import { h, clear, openDialog, confirmDialog, toast, errText, pill } from '../dom.js';
import { store } from '../store.js';
import { relTime } from '../util.js';
import { normDevices, nextPress } from '../security.js';
import {
  ROLE_LABEL, ROLE_HELP, whoText, newPasswordError, passwordChangeErrorText, spendText, budgetShare, budgetText,
  normLogins, browserName, canSavePermits,
} from '../users.js';
import { openPairDialog } from './security.js';
import { signOut, hardGo } from './accountmenu.js';

const iso = (ms) => (ms ? new Date(ms).toISOString() : '');

async function copyText(text, what) {
  try {
    await navigator.clipboard.writeText(text);
    toast(`${what} copied`, 'ok');
  } catch {
    toast('Copy failed: select the text and copy it by hand', 'warn');
  }
}

export function mount(root) {
  let gone = false;
  const me = store.me();
  const isLogin = !!(me && me.user);
  const armed = new Map();

  const card = (title, ...body) => h('section', { class: 'acct-card' }, h('h2', { class: 'group-title', text: title }), ...body);
  const fact = (label, value) => (value ? h('div', { class: 'acct-fact' }, h('span', { class: 'acct-fact-k', text: label }), h('span', { class: 'acct-fact-v', text: value })) : null);

  // ---------- profile ----------
  const who = whoText(me);
  const profile = card('Profile',
    h('div', { class: 'acct-who' },
      h('span', { class: 'acct-avatar', 'aria-hidden': 'true', text: isLogin ? ([...who][0] || '?').toUpperCase() : '★' }),
      h('div', {},
        h('div', { class: 'acct-name-big', text: who }),
        h('div', { class: 'acct-sub' },
          isLogin ? h('span', { class: 'mono', text: me.user.name }) : null,
          pill(ROLE_LABEL[me.role] || me.role, me.role === 'admin' ? 'warn' : 'info')))),
    h('p', { class: 'field-hint', text: ROLE_HELP[me.role] || '' }),
    isLogin ? h('div', { class: 'acct-facts' },
      fact('Account made', me.user.created ? relTime(me.user.created) : ''),
      fact('Last sign-in', me.user.lastLogin ? relTime(me.user.lastLogin) : '')) : null,
    !isLogin && me.kind === 'admin-token' ? h('p', { class: 't-sec', text: me.hasUsers
      ? 'You are signed in with the admin token, the break-glass key. Sign in with your own account for daily use.'
      : 'You are signed in with the admin token. Create the first admin account to sign in with a name and password.' }) : null,
    !isLogin && me.kind === 'admin-token' && !me.hasUsers && !me.legacy
      ? h('div', { class: 'form-actions' }, h('a', { class: 'btn btn-primary', href: '#/first-admin' }, 'Create the first admin')) : null,
    !isLogin && me.kind === 'device' ? h('p', { class: 't-sec', text: 'This browser is signed in with a paired device’s token.' }) : null,
    me.legacy ? null : h('div', { class: 'form-actions' }, h('button', { class: 'btn', type: 'button', onclick: () => signOut() }, 'Sign out')));

  // ---------- password ----------
  let pwCard = null;
  if (isLogin) {
    const err = h('p', { class: 'login-err', role: 'alert', hidden: true });
    const user = h('input', { type: 'text', autocomplete: 'username', value: me.user.name, hidden: true, readonly: true, 'aria-hidden': 'true', tabindex: '-1' });
    const cur = h('input', { class: 'input', type: 'password', autocomplete: 'current-password', 'aria-label': 'Current password' });
    const pw = h('input', { class: 'input', type: 'password', autocomplete: 'new-password', 'aria-label': 'New password' });
    const pw2 = h('input', { class: 'input', type: 'password', autocomplete: 'new-password', 'aria-label': 'New password again' });
    const go = h('button', { class: 'btn btn-primary', type: 'submit' }, 'Change password');
    const say = (t) => { err.textContent = t; err.hidden = !t; };
    const field = (label, el) => h('label', { class: 'field' }, h('span', { class: 'field-label', text: label }), el);
    pwCard = card('Password',
      h('form', { class: 'task-form acct-pw', onsubmit: async (e) => {
        e.preventDefault();
        if (!cur.value) { say('Enter your current password.'); return; }
        const problem = newPasswordError(pw.value, pw2.value, me.user.name);
        if (problem) { say(problem); return; }
        go.disabled = true;
        say('');
        try {
          const r = await apiResult('/api/me/password', { method: 'POST', body: { current: cur.value, new: pw.value }, quiet: true });
          if (gone) return;
          if (r.ok) {
            cur.value = ''; pw.value = ''; pw2.value = '';
            toast('Password changed. Your other browsers and devices were signed out.', 'ok');
            loadLogins();
            loadDevices();
          } else say(passwordChangeErrorText(r.status, r.body));
        } catch (e2) {
          if (!gone && !(e2 instanceof AuthError)) say(errText(e2));
        }
        go.disabled = false;
      } },
      user,
      field('Current password', cur),
      field('New password', pw),
      field('New password again', pw2),
      h('p', { class: 'field-hint', text: 'At least 10 characters, not your user name. Changing it signs out your other browsers and devices.' }),
      err,
      h('div', { class: 'form-actions' }, go)));
  }

  // ---------- signed-in browsers ----------
  const loginsEl = h('div', { class: 'dev-list' }, h('p', { class: 't-sec', text: 'Loading…' }));
  const loginsCard = isLogin ? card('Signed-in browsers',
    h('p', { class: 'field-hint', text: 'Every browser signed in to your account. Sign out one you do not recognise, then change your password.' }),
    loginsEl) : null;

  async function loadLogins() {
    if (!loginsCard) return;
    try {
      const list = normLogins(await api('/api/me/logins'));
      if (gone) return;
      clear(loginsEl);
      if (!list.length) { loginsEl.append(h('p', { class: 't-sec', text: 'None.' })); return; }
      loginsEl.append(...list.map((l) => h('div', { class: 'dev-row' },
        h('div', { class: 'dev-main' },
          h('div', { class: 'dev-name' }, h('span', { text: browserName(l.agent) }), l.current ? pill('this browser', 'ok') : null),
          h('div', { class: 'sess-meta' },
            l.lastSeen ? h('span', { title: l.lastSeen, text: `seen ${relTime(l.lastSeen)}` }) : null,
            l.created ? h('span', { title: l.created, text: `signed in ${relTime(l.created)}` }) : null,
            l.addr ? h('span', { class: 'mono', text: l.addr }) : null)),
        h('div', { class: 'dev-actions' },
          h('button', { class: 'btn', type: 'button', onclick: (e) => endLogin(l, e.currentTarget) }, 'Sign out')))));
    } catch (e) {
      if (!gone && !(e instanceof AuthError)) clear(loginsEl).append(h('div', { class: 'alert err', text: errText(e) }));
    }
  }

  async function endLogin(l, btn) {
    if (l.current) { signOut(); return; }
    btn.disabled = true;
    try {
      await api(`/api/me/logins/${encodeURIComponent(l.id)}`, { method: 'DELETE' });
      toast('That browser is signed out', 'ok');
    } catch (e) { toast(errText(e), 'err'); }
    if (!gone) loadLogins();
  }

  // ---------- devices ----------
  const devicesEl = h('div', { class: 'dev-list' }, h('p', { class: 't-sec', text: 'Loading…' }));
  const pairBtn = h('button', { class: 'btn btn-primary', type: 'button', hidden: me.kind === 'device',
    onclick: () => openPairDialog(() => { if (!gone) loadDevices(); }) }, '+ Pair a device');
  const devicesCard = card('Your devices',
    h('p', { class: 'field-hint', text: me.kind === 'device'
      ? 'Devices paired to your account. Pair new ones from a browser signed in with your password.'
      : 'Pair a phone or laptop so it has its own sign-in you can revoke. It gets your role.' }),
    h('div', { class: 'form-actions acct-pair' }, pairBtn),
    devicesEl);

  async function loadDevices() {
    try {
      const raw = await api('/api/devices');
      if (gone) return;
      let list = normDevices(raw);
      // Admins list every device; here only their own.
      if (me.user && store.can('devices.all')) list = list.filter((d) => !d.user || d.user === me.user.id || d.current);
      clear(devicesEl);
      if (!list.length) { devicesEl.append(h('p', { class: 't-sec', text: 'No paired devices.' })); return; }
      devicesEl.append(...list.map((d) => {
        const btn = h('button', { class: 'btn btn-danger', type: 'button', 'aria-label': `Revoke ${d.name}` }, 'Revoke');
        btn.addEventListener('click', () => pressRevoke(d, btn));
        return h('div', { class: 'dev-row' },
          h('div', { class: 'dev-main' },
            h('div', { class: 'dev-name' }, h('span', { text: d.name }), d.current ? pill('this device', 'ok') : null, d.client ? pill('MCP client', 'info') : null),
            h('div', { class: 'sess-meta' },
              d.created ? h('span', { text: `paired ${relTime(iso(d.created))}` }) : null,
              h('span', { text: d.lastSeen ? `seen ${relTime(iso(d.lastSeen))}` : 'never used' }),
              d.lastAddr ? h('span', { class: 'mono', text: d.lastAddr }) : null)),
          h('div', { class: 'dev-actions' }, btn));
      }));
    } catch (e) {
      if (!gone && !(e instanceof AuthError)) clear(devicesEl).append(h('div', { class: 'alert err', text: errText(e) }));
    }
  }

  function pressRevoke(d, btn) {
    const { fire, armedAt } = nextPress(armed.get(d.id) || 0, Date.now());
    clearTimeout(btn._t);
    if (!fire) {
      armed.set(d.id, armedAt);
      btn.textContent = d.current ? 'Revoke this device?' : 'Press again to revoke';
      btn.classList.add('armed');
      btn._t = setTimeout(() => { armed.delete(d.id); btn.textContent = 'Revoke'; btn.classList.remove('armed'); }, 4000);
      return;
    }
    armed.delete(d.id);
    btn.disabled = true;
    api(`/api/devices/${encodeURIComponent(d.id)}`, { method: 'DELETE' })
      .then(() => toast(`${d.name} revoked`, 'ok'), (e) => toast(errText(e), 'err'))
      .finally(() => { if (!gone) loadDevices(); });
  }

  // ---------- personal API key ----------
  let keyCard = null;
  const keyState = h('p', { class: 't-sec' });
  const keyMake = h('button', { class: 'btn btn-primary', type: 'button', onclick: () => makeKey() }, 'Create key');
  const keyDrop = h('button', { class: 'btn btn-danger', type: 'button', onclick: () => dropKey() }, 'Revoke key');
  let key = me.apiKey;
  if (isLogin && store.can('gateway')) {
    keyCard = card('API key',
      h('p', { class: 'field-hint', text: 'A personal key for the OpenAI-compatible /v1 gateway (editors, scripts). Requests made with it count toward your budget.' }),
      keyState,
      h('div', { class: 'form-actions' }, keyMake, keyDrop));
  }
  function renderKey() {
    if (!keyCard) return;
    keyState.textContent = key.set ? `A key ending in …${key.hint} is active${key.made ? `, made ${relTime(key.made)}` : ''}.` : 'No key yet.';
    keyMake.textContent = key.set ? 'Replace key' : 'Create key';
    keyDrop.hidden = !key.set;
  }
  async function makeKey() {
    if (key.set && !(await confirmDialog({ title: 'Replace the API key', ok: 'Replace', danger: true,
      message: 'The old key stops working at once. Anything that uses it needs the new one.' }))) return;
    try {
      const r = await api('/api/me/apikey', { method: 'POST', body: {} });
      if (gone) return;
      key = { set: true, hint: (r && r.hint) || '', made: new Date().toISOString() };
      renderKey();
      showOnce('Your API key', 'Copy it now: it is shown only once. Use it as the Bearer token for /v1.', (r && r.key) || '');
    } catch (e) { toast(errText(e), 'err'); }
  }
  async function dropKey() {
    if (!(await confirmDialog({ title: 'Revoke the API key', ok: 'Revoke', danger: true, message: 'Anything that uses it stops working at once.' }))) return;
    try {
      await api('/api/me/apikey', { method: 'DELETE' });
      key = { set: false, hint: '', made: '' };
      renderKey();
      toast('API key revoked', 'ok');
    } catch (e) { toast(errText(e), 'err'); }
  }

  // ---------- budget ----------
  let budgetCard = null;
  if (me.budget && me.hasUsers) {
    const b = me.budget;
    const share = budgetShare(b.spentUSD, b.spentTokens, b);
    budgetCard = card('This month',
      h('p', { class: 'acct-spend', text: spendText(b.spentUSD, b.spentTokens, b) }),
      share >= 0 ? h('div', { class: `acct-meter${share >= 1 ? ' over' : share >= 0.8 ? ' near' : ''}`, role: 'meter', 'aria-valuemin': '0', 'aria-valuemax': '100', 'aria-valuenow': String(Math.round(Math.min(1, share) * 100)), 'aria-label': 'Budget used' },
        h('span', { style: `width:${Math.round(Math.min(1, share) * 100)}%` })) : null,
      h('p', { class: 'field-hint', text: `Budget: ${budgetText(b)}. ${share >= 1 ? 'It is used up: ask an admin to raise it.' : 'An admin sets it. Subscription CLIs (claude, opencode) are not counted.'}` }));
  }

  const permitsCard = canSavePermits(me) ? card('Saved permissions',
    h('p', { class: 'field-hint', text: 'Tool calls you chose to allow every time (“Always allow” in a chat, “Every time” on an agent approval). See them, remove them or add one.' }),
    h('div', { class: 'form-actions' }, h('a', { class: 'btn', href: '#/settings/permissions' }, 'Open Permissions'))) : null;

  root.append(h('section', { class: 'view view-pad acct' },
    h('div', { class: 'view-head' },
      h('div', {}, h('h1', { class: 'view-title', text: 'Account' }),
        h('p', { class: 'view-sub', text: 'Your sign-in, devices and spend.' }))),
    h('div', { class: 'acct-grid' }, profile, budgetCard, pwCard, keyCard, permitsCard, loginsCard, devicesCard)));
  renderKey();

  function load() {
    loadLogins();
    loadDevices();
    // Fresh spend and key state.
    store.loadMe().then((m) => {
      if (gone || !m) return;
      if (m.role !== me.role || (m.user && me.user && m.user.id !== me.user.id)) { hardGo('#/account'); return; }
      key = m.apiKey;
      renderKey();
    }).catch(() => {});
  }
  const onVisible = () => load();
  window.addEventListener('cerberus:visible', onVisible);
  load();
  return () => { gone = true; window.removeEventListener('cerberus:visible', onVisible); };
}

// showOnce shows a secret (a generated password, an API key) with Copy,
// once. Exported for the Users panel.
export function showOnce(title, note, secret) {
  return openDialog((close) => ({
    title,
    sub: 'Shown only now',
    body: h('div', { class: 'pair-result' },
      h('div', { class: 'alert warn', text: note }),
      h('div', { class: 'pair-url-row' },
        h('input', { class: 'input mono pair-url', readonly: true, value: secret, 'aria-label': title, onfocus: (e) => e.target.select() }),
        h('button', { class: 'btn', type: 'button', onclick: () => copyText(secret, title) }, 'Copy'))),
    foot: h('button', { class: 'btn btn-primary', type: 'button', onclick: () => close() }, 'Done'),
  }));
}
