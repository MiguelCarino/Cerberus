// Settings → Users (#/settings/users, admins only): every account with its
// role, status, last sign-in, counts and spend this month. A table on
// desktop, one card per user on phones. Add a user (type a password or let
// the server generate one, shown once); per row: change the role, reset the
// password, disable or enable, the monthly budget, repos shared with a
// user (with "may accept into main"), the agent profiles they may run,
// revoke their API key, and delete (their work moves to an admin; nothing
// is deleted). The last enabled admin's role, disable and delete are off.
// Nothing polls: the list loads on mount, after each change and when the
// tab becomes visible again.

import { api } from '../api.js';
import { h, clear, openDialog, confirmDialog, toast, errText, pill } from '../dom.js';
import { store } from '../store.js';
import { relTime } from '../util.js';
import {
  ROLES, ROLE_LABEL, ROLE_HELP, normUsers, userStatus, spendText, budgetShare, nameError, cleanName, passwordError,
  displayError, parseBudget, budgetText, lastAdminReason, selfReason, deleteSummary, deletedText, cutText,
  agentChoices, repoChoices,
} from '../users.js';
import { showOnce } from './account.js';

const uid = (u) => `/api/users/${encodeURIComponent(u.id)}`;

// usersPanel mounts the panel in box; returns its unmount.
export function usersPanel(box) {
  let gone = false;
  let data = { users: [], roles: ROLES.slice(), agents: [] };
  const me = store.me();
  const myId = me && me.user ? me.user.id : '';

  const listEl = h('div', { class: 'usr-list' }, h('p', { class: 't-sec', text: 'Loading…' }));
  const countEl = h('span', { class: 'usr-count t-sec' });
  box.append(h('div', { class: 'usr' },
    h('div', { class: 'sec-head' }, h('h2', { class: 'group-title', text: 'Accounts' }), countEl,
      h('button', { class: 'btn btn-primary', type: 'button', onclick: () => addUser() }, '+ Add user')),
    h('p', { class: 'field-hint sec-hint', text: 'People who sign in with a name and password. Users see only their own work; admins see everything. Changing a role, disabling or resetting a password signs that person out everywhere at once.' }),
    listEl));

  // ---------- data ----------
  async function load() {
    try {
      data = normUsers(await api('/api/users'));
      if (!gone) render();
    } catch (e) {
      if (!gone) clear(listEl).append(h('div', { class: 'alert err', text: errText(e) }));
    }
  }

  // ---------- list ----------
  function render() {
    const { users } = data;
    countEl.textContent = users.length ? `${users.length} account${users.length === 1 ? '' : 's'}` : '';
    clear(listEl);
    if (!users.length) {
      listEl.append(h('div', { class: 'empty' },
        h('p', { class: 'empty-title', text: 'No accounts yet' }),
        h('p', { class: 't-sec', text: 'Add the first admin: from then on people sign in with a name and password.' })));
      return;
    }
    const th = (t, cls) => h('th', { class: cls, text: t });
    listEl.append(h('table', { class: 'table usr-table' },
      h('thead', {}, h('tr', {}, th('User'), th('Role'), th('Status'), th('Last sign-in'), th('Sessions', 'num'), th('Chats', 'num'), th('This month'), th('', 'usr-act-h'))),
      h('tbody', {}, users.map(row))));
  }

  function row(u) {
    const st = userStatus(u);
    const guard = lastAdminReason(u, data.users) || selfReason(u, me);
    const roleSel = h('select', { class: 'select usr-role', 'aria-label': `Role of ${u.name}`, disabled: !!guard, title: guard || ROLE_HELP[u.role] },
      data.roles.map((r) => h('option', { value: r, text: ROLE_LABEL[r] || r })));
    roleSel.value = u.role;
    roleSel.addEventListener('change', () => changeRole(u, roleSel));
    const share = budgetShare(u.spentUSD, u.spentTokens, u.budget);
    const td = (label, ...kids) => h('td', { 'data-label': label }, ...kids);
    return h('tr', { class: `usr-row${u.disabled ? ' off' : ''}`, 'data-id': u.id },
      h('td', { class: 'usr-who', 'data-label': 'User' },
        h('div', { class: 'usr-name' },
          h('span', { class: 'mono', text: u.name }),
          u.id === myId ? pill('you', 'ok') : null,
          u.legacyOwner ? h('span', { class: 'owner-chip', title: 'Owns everything from before accounts existed', text: 'first admin' }) : null),
        u.display ? h('div', { class: 'usr-display t-sec', text: u.display }) : null),
      td('Role', roleSel),
      td('Status', pill(st.label, st.tone)),
      td('Last sign-in', u.lastLogin ? h('span', { title: u.lastAddr ? `from ${u.lastAddr}` : u.lastLogin, text: relTime(u.lastLogin) }) : h('span', { class: 't-muted', text: 'never' })),
      h('td', { class: 'num', 'data-label': 'Sessions', text: String(u.sessions) }),
      h('td', { class: 'num', 'data-label': 'Chats', text: String(u.chats) }),
      td('This month', h('span', { class: `usr-spend${share >= 1 ? ' over' : share >= 0.8 ? ' near' : ''}`, title: `Budget: ${budgetText(u.budget)}`, text: spendText(u.spentUSD, u.spentTokens, u.budget) })),
      h('td', { class: 'usr-act' },
        h('button', { class: 'btn btn-sm', type: 'button', 'aria-label': `Manage ${u.name}`, onclick: () => manage(u) }, 'Manage…')));
  }

  // ---------- add ----------
  async function addUser() {
    const created = await openDialog((close) => {
      const err = h('p', { class: 'login-err', role: 'alert', hidden: true });
      const name = h('input', { class: 'input', autocomplete: 'off', autocapitalize: 'off', spellcheck: 'false', maxlength: '32', placeholder: 'e.g. ana', 'aria-label': 'User name' });
      const display = h('input', { class: 'input', autocomplete: 'off', maxlength: '60', placeholder: 'Optional, e.g. Ana', 'aria-label': 'Display name' });
      const role = h('select', { class: 'select', 'aria-label': 'Role' }, data.roles.map((r) => h('option', { value: r, text: ROLE_LABEL[r] || r })));
      role.value = data.users.length ? 'user' : 'admin';
      role.disabled = !data.users.length;
      const help = h('p', { class: 'field-hint usr-rolehelp' });
      const syncHelp = () => { help.textContent = ROLE_HELP[role.value] || ''; };
      role.addEventListener('change', syncHelp);
      syncHelp();
      const gen = h('input', { type: 'radio', name: 'usr-pw', checked: true });
      const typed = h('input', { type: 'radio', name: 'usr-pw' });
      const pw = h('input', { class: 'input', type: 'password', autocomplete: 'new-password', 'aria-label': 'Password', disabled: true, placeholder: 'At least 10 characters' });
      const syncPw = () => { pw.disabled = gen.checked; if (!pw.disabled) pw.focus(); };
      gen.addEventListener('change', syncPw);
      typed.addEventListener('change', syncPw);
      const go = h('button', { class: 'btn btn-primary', type: 'submit' }, 'Add user');
      const say = (t) => { err.textContent = t; err.hidden = !t; };
      const form = h('form', { class: 'task-form', onsubmit: async (e) => {
        e.preventDefault();
        const n = cleanName(name.value);
        const problem = nameError(n) || displayError(display.value) || (typed.checked ? passwordError(pw.value, n) : '');
        if (problem) { say(problem); return; }
        go.disabled = true;
        say('');
        try {
          const body = { name: n, role: role.value };
          if (display.value.trim()) body.display = display.value.trim();
          if (typed.checked) body.password = pw.value;
          close(await api('/api/users', { method: 'POST', body }));
        } catch (e2) {
          say(e2 && e2.status === 409 ? 'That name is taken.' : errText(e2));
          go.disabled = false;
        }
      } },
      h('label', { class: 'field' }, h('span', { class: 'field-label', text: 'User name' }), name,
        h('span', { class: 'field-hint', text: 'What they type to sign in: lowercase letters, digits, dot, dash or underscore.' })),
      h('label', { class: 'field' }, h('span', { class: 'field-label', text: 'Display name' }), display),
      h('label', { class: 'field' }, h('span', { class: 'field-label', text: 'Role' }), role, help),
      h('fieldset', { class: 'usr-pwchoice' },
        h('legend', { class: 'field-label', text: 'Password' }),
        h('label', { class: 'check' }, gen, 'Generate one (they change it at first sign-in)'),
        h('label', { class: 'check' }, typed, 'Type one'),
        pw),
      err,
      h('div', { class: 'form-actions' }, h('button', { class: 'btn btn-ghost', type: 'button', onclick: () => close() }, 'Cancel'), go));
      setTimeout(() => name.focus(), 0);
      return { title: 'Add user', sub: 'A new account on this host', body: form };
    });
    if (!created) return;
    const u = created.user || {};
    toast(`Added ${u.name || 'the user'}`, 'ok');
    if (created.password) {
      await showOnce(`Password for ${u.name || 'the new user'}`, 'Give it to them in person or over a private channel. They must change it when they first sign in.', created.password);
    }
    if (!gone) load();
  }

  // ---------- role ----------
  async function changeRole(u, sel) {
    const next = sel.value;
    const ok = await confirmDialog({
      title: `Make ${u.name} ${ROLE_LABEL[next] || next}?`, ok: 'Change role', danger: next === 'admin' || u.role === 'admin',
      message: `${ROLE_HELP[next] || ''} They are signed out of every browser and device and sign in again with the new role.`,
    });
    if (!ok) { sel.value = u.role; return; }
    await patch(u, { role: next }, `${u.name} is now ${ROLE_LABEL[next] || next}.`, () => { sel.value = u.role; });
  }

  async function patch(u, body, done, onFail = () => {}) {
    try {
      const r = await api(uid(u), { method: 'PATCH', body });
      toast(done + cutText(r && r.cut), 'ok');
    } catch (e) {
      onFail();
      toast(errText(e), 'err');
    }
    if (!gone) load();
  }

  // ---------- manage (one user's actions) ----------
  async function manage(u) {
    const guard = lastAdminReason(u, data.users) || selfReason(u, me);
    const choice = await openDialog((close) => {
      const item = (key, label, sub, { danger = false, disabled = false } = {}) => h('button', {
        class: `usr-action${danger ? ' danger' : ''}`, type: 'button', disabled, onclick: () => close(key),
      }, h('span', { class: 'more-label', text: label }), sub ? h('span', { class: 'more-sub', text: sub }) : null);
      const confined = u.role === 'user';
      return {
        title: u.display ? `${u.display} (${u.name})` : u.name,
        sub: `${ROLE_LABEL[u.role] || u.role} · ${userStatus(u).label}`,
        body: h('div', { class: 'usr-actions' },
          guard ? h('p', { class: 'alert info', text: guard }) : null,
          item('password', 'Reset password…', 'Generate or type a new one; they change it at next sign-in'),
          u.disabled
            ? item('enable', 'Enable', 'They can sign in again')
            : item('disable', 'Disable', 'Signs them out everywhere; their work stays', { danger: true, disabled: !!guard }),
          item('budget', 'Monthly budget…', budgetText(u.budget)),
          confined ? item('repos', 'Shared repos…', u.repos.length ? `${u.repos.length} shared` : 'None: scratch folders only') : null,
          confined ? item('agents', 'Allowed agents…', u.agents.length ? u.agents.join(', ') : 'The built-in agent only') : null,
          u.apiKey.set ? item('apikey', 'Revoke API key', `The key ending in …${u.apiKey.hint} stops working`, { danger: true }) : null,
          item('delete', 'Delete…', 'Their work moves to an admin', { danger: true, disabled: !!guard })),
      };
    });
    switch (choice) {
      case 'password': return resetPassword(u);
      case 'enable': return patch(u, { disabled: false }, `${u.name} is enabled.`);
      case 'disable':
        if (await confirmDialog({ title: `Disable ${u.name}?`, ok: 'Disable', danger: true,
          message: 'They are signed out of every browser and device at once and cannot sign in. Their running sessions stop; their sessions, chats and flows stay. Scheduled flows of theirs are skipped while they are disabled.' })) {
          return patch(u, { disabled: true }, `${u.name} is disabled.`);
        }
        return undefined;
      case 'budget': return editBudget(u);
      case 'repos': return editRepos(u);
      case 'agents': return editAgents(u);
      case 'apikey':
        if (await confirmDialog({ title: `Revoke ${u.name}’s API key?`, ok: 'Revoke', danger: true, message: 'Anything that uses it stops working at once.' })) {
          try { await api(`${uid(u)}/apikey`, { method: 'DELETE' }); toast('API key revoked', 'ok'); } catch (e) { toast(errText(e), 'err'); }
          if (!gone) load();
        }
        return undefined;
      case 'delete': return removeUser(u);
      default: return undefined;
    }
  }

  async function resetPassword(u) {
    const res = await openDialog((close) => {
      const err = h('p', { class: 'login-err', role: 'alert', hidden: true });
      const gen = h('input', { type: 'radio', name: 'usr-reset', checked: true });
      const typed = h('input', { type: 'radio', name: 'usr-reset' });
      const pw = h('input', { class: 'input', type: 'password', autocomplete: 'new-password', disabled: true, 'aria-label': 'New password', placeholder: 'At least 10 characters' });
      const sync = () => { pw.disabled = gen.checked; if (!pw.disabled) pw.focus(); };
      gen.addEventListener('change', sync);
      typed.addEventListener('change', sync);
      const go = h('button', { class: 'btn btn-primary', type: 'submit' }, 'Reset password');
      return {
        title: `Reset ${u.name}’s password`,
        sub: 'They are signed out everywhere',
        body: h('form', { class: 'task-form', onsubmit: async (e) => {
          e.preventDefault();
          const problem = typed.checked ? passwordError(pw.value, u.name) : '';
          if (problem) { err.textContent = problem; err.hidden = false; return; }
          go.disabled = true;
          try {
            close(await api(`${uid(u)}/password`, { method: 'POST', body: typed.checked ? { password: pw.value } : {} }));
          } catch (e2) {
            err.textContent = errText(e2);
            err.hidden = false;
            go.disabled = false;
          }
        } },
        h('label', { class: 'check' }, gen, 'Generate one'),
        h('label', { class: 'check' }, typed, 'Type one'),
        pw,
        h('p', { class: 'field-hint', text: 'They must choose their own at the next sign-in. A lockout from failed sign-ins is cleared too.' }),
        err,
        h('div', { class: 'form-actions' }, h('button', { class: 'btn btn-ghost', type: 'button', onclick: () => close() }, 'Cancel'), go)),
      };
    });
    if (!res) return;
    toast(`${u.name}’s password is reset.${cutText(res.cut)}`, 'ok');
    if (res.password) await showOnce(`New password for ${u.name}`, 'Give it to them privately. They must change it when they sign in.', res.password);
    if (!gone) load();
  }

  async function editBudget(u) {
    const budget = await openDialog((close) => {
      const usdIn = h('input', { class: 'input', inputmode: 'decimal', value: u.budget.monthlyUSD ? String(u.budget.monthlyUSD) : '', placeholder: '0 = no cap', 'aria-label': 'US dollars a month' });
      const tokIn = h('input', { class: 'input', inputmode: 'numeric', value: u.budget.monthlyTokens ? String(u.budget.monthlyTokens) : '', placeholder: '0 = no cap', 'aria-label': 'Tokens a month' });
      const err = h('p', { class: 'login-err', role: 'alert', hidden: true });
      const save = () => {
        const r = parseBudget(usdIn.value, tokIn.value);
        if (r.error) { err.textContent = r.error; err.hidden = false; return; }
        close(r.budget);
      };
      return {
        title: `Budget for ${u.name}`,
        sub: `This month so far: ${spendText(u.spentUSD, u.spentTokens, u.budget)}`,
        body: h('form', { class: 'task-form', onsubmit: (e) => { e.preventDefault(); save(); } },
          h('label', { class: 'field' }, h('span', { class: 'field-label', text: 'US$ a month' }), usdIn),
          h('label', { class: 'field' }, h('span', { class: 'field-label', text: 'Tokens a month' }), tokIn),
          h('p', { class: 'field-hint', text: 'Empty or 0 means no cap. Over the cap, new sessions, chat messages and /v1 requests are refused until next month. Subscription CLIs (claude, opencode) are not counted.' }),
          err, h('button', { type: 'submit', hidden: true })),
        foot: [h('button', { class: 'btn btn-ghost', type: 'button', onclick: () => close() }, 'Cancel'),
          h('button', { class: 'btn btn-primary', type: 'button', onclick: save }, 'Save')],
      };
    });
    if (budget) await patch(u, { budget }, `Budget saved for ${u.name}.`);
  }

  async function editRepos(u) {
    let repos = [];
    try { repos = await api('/api/repos'); } catch (e) { toast(errText(e), 'err'); }
    if (gone) return;
    const choices = repoChoices(Array.isArray(repos) ? repos : (repos && repos.repos) || [], u.repos);
    const picked = await openDialog((close) => {
      const rows = choices.map((c) => {
        const on = h('input', { type: 'checkbox', checked: c.shared, 'aria-label': `Share ${c.name}` });
        const acc = h('input', { type: 'checkbox', checked: c.accept, disabled: !c.shared, 'aria-label': `${c.name}: may accept into main` });
        on.addEventListener('change', () => { acc.disabled = !on.checked; if (!on.checked) acc.checked = false; });
        return { c, on, acc, el: h('div', { class: 'usr-repo' },
          h('label', { class: 'check usr-repo-main' }, on, h('span', {}, h('b', { text: c.name }), h('span', { class: 'usr-repo-path mono', text: c.path }))),
          h('label', { class: 'check usr-repo-acc' }, acc, 'May accept into main')) };
      });
      return {
        title: `Repos shared with ${u.name}`,
        sub: 'Their sessions can work in these, sandboxed',
        body: h('div', { class: 'usr-repos' },
          rows.length ? rows.map((r) => r.el) : h('p', { class: 't-sec', text: 'No repos found. Add repos in Settings first.' }),
          h('p', { class: 'field-hint', text: 'Without "may accept", they can see the diff, save and download, but not fast-forward main. Folder workspaces stay admin only.' })),
        foot: [h('button', { class: 'btn btn-ghost', type: 'button', onclick: () => close() }, 'Cancel'),
          h('button', { class: 'btn btn-primary', type: 'button', onclick: () => close(rows.filter((r) => r.on.checked).map((r) => ({ path: r.c.path, accept: r.acc.checked }))) }, 'Save')],
      };
    });
    if (picked) await patch(u, { repos: picked }, `Shared repos saved for ${u.name}.`);
  }

  async function editAgents(u) {
    const choices = agentChoices(data.agents);
    const picked = await openDialog((close) => {
      const warn = h('div', { class: 'alert warn', hidden: true, text: 'A CLI profile (claude, opencode…) runs with your own login of that tool: its config folder is bound into their sandbox, and its use is not counted in their budget.' });
      const boxes = choices.map((c) => {
        const cb = h('input', { type: 'checkbox', checked: c.builtin ? (u.agents.length === 0 || u.agents.includes(c.name)) : u.agents.includes(c.name) });
        return { c, cb, el: h('label', { class: 'check' }, cb, c.name, c.builtin ? h('span', { class: 't-sec', text: ' (built-in)' }) : null, c.cli ? h('span', { class: 'owner-chip', text: 'CLI' }) : null) };
      });
      const sync = () => { warn.hidden = !boxes.some((b) => b.c.cli && b.cb.checked); };
      for (const b of boxes) b.cb.addEventListener('change', sync);
      sync();
      return {
        title: `Agents ${u.name} may run`,
        sub: 'Agent profiles for their sessions',
        body: h('div', { class: 'usr-agents' }, boxes.map((b) => b.el), warn,
          h('p', { class: 'field-hint', text: 'None ticked means the built-in agent only.' })),
        foot: [h('button', { class: 'btn btn-ghost', type: 'button', onclick: () => close() }, 'Cancel'),
          h('button', { class: 'btn btn-primary', type: 'button', onclick: () => close(boxes.filter((b) => b.cb.checked).map((b) => b.c.name)) }, 'Save')],
      };
    });
    if (picked) await patch(u, { agents: picked }, `Allowed agents saved for ${u.name}.`);
  }

  async function removeUser(u) {
    const admins = data.users.filter((x) => x.role === 'admin' && !x.disabled && x.id !== u.id);
    if (!admins.length) { toast('Make another admin first.', 'warn'); return; }
    const res = await openDialog((close) => {
      const def = admins.find((a) => a.id === myId) || admins[0];
      const to = h('select', { class: 'select', 'aria-label': 'Move their work to' },
        admins.map((a) => h('option', { value: a.id, text: a.id === myId ? `${a.name} (you)` : a.name })));
      to.value = def.id;
      const summary = h('p', { class: 'dlg-msg' });
      const sync = () => { summary.textContent = deleteSummary(u, to.value === myId ? 'you' : (admins.find((a) => a.id === to.value) || {}).name || 'the admin'); };
      to.addEventListener('change', sync);
      sync();
      const typed = h('input', { class: 'input mono', autocomplete: 'off', autocapitalize: 'off', spellcheck: 'false', placeholder: u.name, 'aria-label': `Type ${u.name} to confirm` });
      const go = h('button', { class: 'btn btn-danger', type: 'submit', disabled: true }, 'Delete account');
      typed.addEventListener('input', () => { go.disabled = typed.value.trim().toLowerCase() !== u.name; });
      const err = h('p', { class: 'login-err', role: 'alert', hidden: true });
      setTimeout(() => typed.focus(), 0);
      return {
        title: `Delete ${u.name}?`,
        sub: 'The account goes; the work stays',
        body: h('form', { class: 'task-form', onsubmit: async (e) => {
          e.preventDefault();
          if (go.disabled) return;
          go.disabled = true;
          try {
            const r = await api(uid(u), { method: 'DELETE', body: { to: to.value } });
            close({ r, toName: to.value === myId ? 'you' : (admins.find((a) => a.id === to.value) || {}).name || 'the admin' });
          } catch (e2) {
            err.textContent = errText(e2);
            err.hidden = false;
            go.disabled = false;
          }
        } },
        summary,
        h('label', { class: 'field' }, h('span', { class: 'field-label', text: 'Move their work to' }), to),
        h('label', { class: 'field' }, h('span', { class: 'field-label', text: `Type ${u.name} to confirm` }), typed),
        err,
        h('div', { class: 'form-actions' }, h('button', { class: 'btn btn-ghost', type: 'button', onclick: () => close() }, 'Cancel'), go)),
      };
    });
    if (!res) return;
    toast(deletedText(u.name, res.r, res.toName) + cutText(res.r && res.r.cut), 'ok');
    if (!gone) load();
  }

  const onVisible = () => load();
  window.addEventListener('cerberus:visible', onVisible);
  load();
  return () => { gone = true; window.removeEventListener('cerberus:visible', onVisible); };
}
