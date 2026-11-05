// Secrets view: the vault's names and metadata (values are never returned
// by the server), add or replace through a password field that is never
// filled back in, and delete with an in-page confirm.

import { api } from '../api.js';
import { h, clear, openDialog, confirmDialog, toast, errText } from '../dom.js';
import { relTime } from '../util.js';
import { secretError, SECRET_NAME_RE } from '../text.js';

const sec = (name) => `/api/secrets/${encodeURIComponent(name)}`;

const BACKEND_NOTES = {
  'secret-service': {
    tone: 'info',
    text: 'Stored in your desktop keyring (Secret Service, through secret-tool). Only names, notes and lengths are kept in Cerberus’ config folder. Values go only to the sessions and providers that use them, and are masked in session output.',
  },
  file: {
    tone: 'warn',
    text: 'Stored in vault.json, each value encrypted (AES-256-GCM) with the key in vault.key next to it. This guards against accidental leaks such as backups, grep or screenshots of the config, not against someone with access to your account. Values are masked in session output.',
  },
};

export function mount(root, _route, opts = {}) {
  const embedded = !!opts.embedded;
  const noteEl = h('div', {});
  const listEl = h('div', { class: 'secret-list' }, h('p', { class: 't-sec', text: 'Loading…' }));
  const addBtn = h('button', { class: `btn btn-primary${embedded ? '' : ' btn-big'}`, type: 'button', onclick: () => edit(null) }, '+ Add secret');
  root.append(h('section', { class: embedded ? 'secrets-embedded' : 'view view-pad' },
    embedded
      ? h('div', { class: 'secrets-head' }, addBtn)
      : h('div', { class: 'view-head' },
        h('div', {}, h('h1', { class: 'view-title', text: 'Secrets' }),
          h('p', { class: 'view-sub', text: 'Provider keys and tokens for agents. Values are write-only here.' })),
        addBtn),
    noteEl, listEl,
    h('p', { class: 'field-hint secret-use', text: 'Use a secret through secret_env in an agent profile or repo (env var → secret name), or as a provider’s key_secret.' })));

  let items = [];
  let gone = false;

  async function load() {
    try {
      const out = await api('/api/secrets');
      if (gone) return;
      items = ((out && out.items) || []).sort((a, b) => a.Name.localeCompare(b.Name));
      const note = BACKEND_NOTES[out && out.backend];
      clear(noteEl);
      if (note) noteEl.append(h('div', { class: `alert ${note.tone}`, text: note.text }));
      render();
    } catch (e) {
      if (!gone) clear(listEl).append(h('div', { class: 'alert err', text: errText(e) }));
    }
  }

  function render() {
    clear(listEl);
    if (!items.length) {
      listEl.append(h('div', { class: 'empty' },
        h('p', { class: 'empty-title', text: 'No secrets yet' }),
        h('p', { class: 't-sec', text: 'Add an API key or token. It is stored by the vault and never shown again.' })));
      return;
    }
    listEl.append(...items.map((it) => h('div', { class: 'secret-row' },
      h('div', { class: 'secret-main' },
        h('div', { class: 'secret-name', text: it.Name }),
        it.Note ? h('div', { class: 'secret-note', text: it.Note }) : null,
        h('div', { class: 'sess-meta' },
          h('span', { text: `${it.Length} char${it.Length === 1 ? '' : 's'}` }),
          h('span', { text: `updated ${relTime(it.Updated)}` }))),
      h('div', { class: 'secret-actions' },
        h('button', { class: 'btn', type: 'button', onclick: () => edit(it) }, 'Replace'),
        h('button', { class: 'btn btn-danger', type: 'button', onclick: () => remove(it) }, 'Delete')))));
  }

  async function edit(item) {
    const saved = await openDialog((close) => {
      const name = h('input', {
        class: 'input', placeholder: 'e.g. OPENROUTER_KEY', autocapitalize: 'off', spellcheck: 'false', autocomplete: 'off',
        maxlength: '64', readonly: !!item, value: item ? item.Name : '',
      });
      // A fresh, empty password field every time: the old value is never sent
      // to the browser, so there is nothing to show.
      const value = h('input', { class: 'input', type: 'password', autocomplete: 'new-password', placeholder: item ? 'New value' : 'Value' });
      const show = h('input', { type: 'checkbox', onchange: () => { value.type = show.checked ? 'text' : 'password'; } });
      const note = h('input', { class: 'input', placeholder: 'Optional: what it is for', maxlength: '200', value: item ? item.Note || '' : '' });
      const err = h('p', { class: 'field-hint secret-err' });
      const saveBtn = h('button', { class: 'btn btn-primary', type: 'submit' }, item ? 'Replace' : 'Save');
      name.addEventListener('input', () => {
        err.textContent = name.value && !SECRET_NAME_RE.test(name.value) ? 'Letters, digits, dot, dash or underscore (max 64)' : '';
      });
      const form = h('form', { class: 'task-form', onsubmit: async (e) => {
        e.preventDefault();
        const problem = secretError(name.value.trim(), value.value);
        if (problem) { err.textContent = problem; return; }
        if (!item && items.some((it) => it.Name === name.value.trim())) {
          const ok = await confirmDialog({ title: 'Replace secret', ok: 'Replace', message: `${name.value.trim()} already exists. Replace its value?` });
          if (!ok) return;
        }
        saveBtn.disabled = true;
        try {
          await api(sec(name.value.trim()), { method: 'PUT', body: { value: value.value, note: note.value.trim() } });
          value.value = '';
          close(name.value.trim());
        } catch (e2) {
          err.textContent = errText(e2);
          saveBtn.disabled = false;
        }
      } },
      h('label', { class: 'field' }, h('span', { class: 'field-label', text: 'Name' }), name),
      h('label', { class: 'field' }, h('span', { class: 'field-label', text: 'Value' }), value),
      h('label', { class: 'check' }, show, 'Show while typing'),
      h('label', { class: 'field' }, h('span', { class: 'field-label', text: 'Note' }), note),
      err,
      h('div', { class: 'form-actions' },
        h('button', { class: 'btn btn-ghost', type: 'button', onclick: () => close(null) }, 'Cancel'), saveBtn));
      setTimeout(() => (item ? value : name).focus(), 0);
      return { title: item ? `Replace ${item.Name}` : 'Add secret', sub: 'The value is stored, never shown again', body: form };
    });
    if (saved) { toast(`${saved} saved`, 'ok'); load(); }
  }

  async function remove(item) {
    const ok = await confirmDialog({
      title: 'Delete secret', ok: 'Delete', danger: true,
      message: `Delete ${item.Name}? Sessions and providers that use it will fail until it is added again.`,
    });
    if (!ok) return;
    try {
      await api(sec(item.Name), { method: 'DELETE' });
      toast(`${item.Name} deleted`, 'ok');
    } catch (e) {
      toast(errText(e), 'err');
    }
    load();
  }

  const onVisible = () => load();
  window.addEventListener('cerberus:visible', onVisible);
  load();
  return () => { gone = true; window.removeEventListener('cerberus:visible', onVisible); };
}
