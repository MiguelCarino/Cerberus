// Settings → Permissions: your saved permissions ("Always allow" from a
// chat card, "Every time" from an agent session's approval, or added
// here), each with where it came from, when it was made and last used,
// and Remove; an Add form whose errors are the server's own (permits
// Validate); and, read only, the command policy lists that also decide
// what runs without asking. Admins and users see their own rules only.

import { api, apiResult } from '../api.js';
import { h, clear, toast, errText, confirmDialog } from '../dom.js';
import { normPermits, sourceLabel, shortDate, TOOL_CHOICES, placeholderFor, ruleWords } from '../permits.js';

export function permissionsPanel(root) {
  let gone = false;
  const listEl = h('div', { class: 'perm-list' }, h('p', { class: 't-sec', text: 'Loading…' }));
  const policyEl = h('div', { class: 'perm-policy' });

  const tool = h('select', { class: 'select', 'aria-label': 'Tool', onchange: () => { content.placeholder = placeholderFor(tool.value); err.textContent = ''; } },
    TOOL_CHOICES.map(([t]) => h('option', { value: t, text: t })));
  const content = h('input', {
    class: 'input mono', type: 'text', placeholder: placeholderFor('Bash'), 'aria-label': 'Rule', autocapitalize: 'off', spellcheck: 'false',
    autocomplete: 'off', maxlength: '1000', oninput: () => { err.textContent = ''; },
  });
  const err = h('div', { class: 'field-err perm-err', role: 'alert', 'aria-live': 'polite' });
  const addBtn = h('button', { class: 'btn btn-primary', type: 'submit' }, 'Add');
  const form = h('form', { class: 'perm-add', onsubmit: (e) => { e.preventDefault(); add(); } },
    h('div', { class: 'perm-add-row' }, tool, content, addBtn), err,
    h('p', { class: 'field-hint', text: 'Bash: an exact command, or a prefix such as go test:* (never a whole shell, a deleting or network command, or joined commands). Files: dir:~/folder or an absolute path. Web: domain:host.' }));

  root.append(h('div', { class: 'perm' },
    h('p', { class: 'field-hint', text: 'These run without asking, in every chat and agent session of yours. “Always allow” on a chat card and “Every time” on an agent approval add them; the command policy can still force a question.' }),
    listEl,
    h('h3', { class: 'group-title', text: 'Add a rule' }), form,
    h('h3', { class: 'group-title', text: 'Command policy (read only)' }), policyEl));

  async function load() {
    try {
      const d = normPermits(await api('/api/permits'));
      if (gone) return;
      renderList(d.rules);
      renderPolicy(d);
    } catch (e) {
      if (!gone) clear(listEl).append(h('div', { class: 'alert err', text: errText(e) }));
    }
  }

  function renderList(rules) {
    clear(listEl);
    if (!rules.length) {
      listEl.append(h('div', { class: 'empty' },
        h('p', { class: 'empty-title', text: 'Nothing saved' }),
        h('p', { class: 't-sec', text: 'Every approval asks until you save a rule here or from an approval.' })));
      return;
    }
    listEl.append(...rules.map((r) => h('div', { class: 'secret-row perm-row' },
      h('div', { class: 'secret-main' },
        h('div', { class: 'secret-name perm-text', text: ruleWords(r) }),
        h('div', { class: 'sess-meta perm-meta' },
          h('span', { text: r.text }),
          h('span', { text: sourceLabel(r.source) }),
          h('span', { text: `made ${shortDate(r.created) || '—'}` }),
          h('span', { text: r.uses ? `used ${r.uses}× · last ${shortDate(r.lastUsed) || '—'}` : 'not used yet' }))),
      h('div', { class: 'secret-actions' },
        h('button', { class: 'btn btn-danger', type: 'button', onclick: () => remove(r) }, 'Remove')))));
  }

  function renderPolicy(d) {
    clear(policyEl);
    const group = (title, help, list) => h('div', { class: 'perm-pol' },
      h('div', { class: 'perm-pol-title', text: title }),
      h('p', { class: 'field-hint', text: help }),
      list.length ? h('ul', { class: 'perm-pol-list' }, list.map((x) => h('li', { class: 'mono', text: x }))) : h('p', { class: 't-sec', text: 'None.' }));
    policyEl.append(
      group('Always allowed', 'Commands starting like these run without asking, even in ask mode.', d.policy.allow),
      group('Always ask', 'Asked every time, even in auto mode; saved rules never answer them.', d.policy.askAlways),
      group('Forbidden', 'Never run, not even when allowed.', d.policy.deny),
      d.link ? h('a', { class: 'btn btn-ghost btn-sm', href: d.link }, 'Edit in Settings → Safety') : h('p', { class: 't-sec t-sm', text: 'An admin edits these in Settings → Safety.' }));
  }

  async function add() {
    err.textContent = '';
    addBtn.disabled = true;
    try {
      const res = await apiResult('/api/permits', { method: 'POST', body: { tool: tool.value, content: content.value.trim() } });
      if (!res.ok) { err.textContent = res.message; return; }
      toast(res.body && res.body.added === false ? 'You already have that rule' : 'Saved', 'ok');
      content.value = '';
      await load();
    } catch (e) {
      err.textContent = errText(e);
    } finally {
      addBtn.disabled = false;
    }
  }

  async function remove(r) {
    const ok = await confirmDialog({ title: 'Remove this rule?', message: `${r.text} will ask again from the next call.`, ok: 'Remove', danger: true });
    if (!ok) return;
    try {
      const res = await apiResult(`/api/permits/${encodeURIComponent(r.id)}`, { method: 'DELETE' });
      if (!res.ok && res.status !== 404) { toast(res.message, 'err'); return; }
      toast('Removed', 'ok');
    } catch (e) {
      toast(errText(e), 'err');
    }
    load();
  }

  load();
  return () => { gone = true; };
}
