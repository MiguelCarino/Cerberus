// The LLM chat's sheets: the model picker (Ctrl+M), the mode menu with the
// session rules, the recent-chats sheet (Ctrl+O: Cerberus chats, Claude
// CLI sessions and Atrium's importable ones, with search, two-press delete
// and continue) and the working-folder chooser.

import { api } from '../api.js';
import { h, clear, openDialog, toast, errText } from '../dom.js';
import { modelGroups, switchesBackend, MODES, agoText, deletePress, shortPath } from '../chat.js';
import { queryWords, highlightParts } from '../text.js';
import { debounce } from '../util.js';
import { folderPicker } from './picker.js';
import { ownerLabel } from '../users.js';

// openModelPicker resolves to the picked model id (or undefined). data is
// GET /api/chats/models; current the chat's model, next the pick waiting
// for the next message.
// manage: the person may load and unload models (admins).
export function openModelPicker({ data, current = '', next = '', hasMessages = false, reload, offerDefault = false, setDefault = null, manage = true }) {
  let models = data;
  const list = h('div', { class: 'mp-list' });
  const note = h('p', { class: 'mp-note', 'aria-live': 'polite' });
  return openDialog((close) => {
    const render = () => {
      clear(list);
      const groups = modelGroups(models);
      if (!groups.length) list.append(h('p', { class: 't-sec', text: 'No models found. Configure llama.cpp, LM Studio, Ollama or an API provider in Settings.' }));
      for (const g of groups) {
        list.append(h('div', { class: 'mp-group', text: g.label }));
        for (const it of g.items) {
          const marks = [
            it.local ? h('span', { class: `mp-dot${it.loaded ? ' on' : ''}`, title: it.loaded ? 'Loaded' : 'Not loaded', text: it.loaded ? '●' : '○' }) : null,
            it.isDefault ? h('span', { class: 'mp-star', title: 'The default model', text: '★' }) : null,
          ];
          const tags = [
            it.id === current ? h('span', { class: 'pill muted', text: 'now' }) : null,
            it.id === next && next !== current ? h('span', { class: 'pill info', text: 'next message' }) : null,
          ];
          const pick = h('button', {
            class: `mp-item${it.id === (next || current) ? ' sel' : ''}`, type: 'button', 'aria-label': `Use ${it.id}`,
            onclick: () => close(it.id),
            onmouseenter: () => { note.textContent = noteFor(it.id); },
            onfocus: () => { note.textContent = noteFor(it.id); },
          }, marks, h('span', { class: 'mp-label', text: it.label }), tags);
          const unload = manage && it.local && it.loaded
            ? h('button', { class: 'btn btn-sm btn-ghost mp-unload', type: 'button', title: `Unload ${it.id} from memory`, onclick: () => unloadModel(it.id) }, 'Unload')
            : null;
          // "Make default" when no default is set (or asked for): new chats and
          // "default" then use it, so this failure does not come back.
          const makeDefault = setDefault && offerDefault && !it.isDefault && g.key !== 'claude'
            ? h('button', { class: 'btn btn-sm btn-ghost mp-default', type: 'button', title: `Use ${it.id} as the default model from now on`,
              onclick: async () => {
                try { await setDefault(it.id); close(it.id); } catch (e) { toast(errText(e), 'err'); }
              } }, 'Make default')
            : null;
          list.append(h('div', { class: 'mp-row' }, pick, makeDefault, unload));
        }
      }
    };
    const noteFor = (id) => (switchesBackend(next || current, id, hasMessages)
      ? 'Switching between Claude and the gateway starts a new chat in the same folder.'
      : 'A pick applies to the next message.');
    async function unloadModel(id) {
      try {
        await api(`/api/models/${encodeURIComponent(id)}/unload`, { method: 'POST', body: {} });
        toast(`Unloaded ${id}`, 'ok');
        if (reload) { models = await reload(); render(); }
      } catch (e) { toast(errText(e), 'err'); }
    }
    render();
    note.textContent = 'A pick applies to the next message. ● loaded · ○ not loaded · ★ default';
    return {
      title: 'Model',
      sub: current ? `Now: ${current}` : 'For this chat',
      body: h('div', { class: 'mp' }, note, list),
      foot: reload ? [h('button', { class: 'btn btn-ghost', type: 'button', onclick: async () => { try { models = await reload(); render(); } catch (e) { toast(errText(e), 'err'); } } }, 'Rescan')] : null,
    };
  });
}

// openModeMenu resolves to the picked mode (or undefined). rules are the
// chat's session rules; forget() clears them.
export function openModeMenu({ mode = 'ask', next = '', rules = [], forget = null }) {
  const cur = next || mode;
  return openDialog((close) => {
    const ruleBox = h('div', { class: 'rules' });
    const renderRules = (list) => {
      clear(ruleBox);
      ruleBox.append(h('div', { class: 'mp-group', text: 'Session rules' }));
      if (!list.length) { ruleBox.append(h('p', { class: 't-sec rules-empty', text: 'None yet. “Allow for this session” on a card adds one; it then answers the cards it covers.' })); return; }
      ruleBox.append(h('ul', { class: 'rules-list' }, list.map((r) => h('li', { class: 'mono', text: r.text || `${r.tool}${r.content ? ' ' + r.content : ''}` }))));
      if (forget) {
        ruleBox.append(h('button', {
          class: 'btn btn-danger btn-sm', type: 'button',
          onclick: async (e) => {
            e.currentTarget.disabled = true;
            try { await forget(); renderRules([]); toast('Session rules forgotten', 'ok'); } catch (err) { toast(errText(err), 'err'); e.currentTarget.disabled = false; }
          },
        }, 'Forget them'));
      }
    };
    renderRules(rules || []);
    return {
      title: 'Permissions',
      sub: 'Applies to the next message',
      body: h('div', { class: 'mode-menu' },
        h('div', { class: 'mode-list', role: 'radiogroup', 'aria-label': 'Permission mode' }, MODES.map(([k, label, help]) =>
          h('button', { class: `mode-item${k === cur ? ' sel' : ''}`, type: 'button', role: 'radio', 'aria-checked': String(k === cur), onclick: () => close(k) },
            h('span', { class: 'mode-label', text: label }), h('span', { class: 'mode-help', text: help })))),
        ruleBox),
    };
  });
}

// openRecent is the recent-chats sheet. It resolves to {open: id} for a
// chat to show (a Cerberus chat, or the copy a Continue made), or
// {atrium: id} for a desktop session to show read-only.
// imports: the Claude and Atrium sections (admins). allUsers: an "All
// users" switch that lists every person's chats (admins); me is who is
// signed in (for the owner labels).
export function openRecent({ current = '', home = '', imports = true, allUsers = false, me = null } = {}) {
  return openDialog((close) => {
    const search = h('input', { class: 'input', type: 'search', placeholder: 'Search chats', 'aria-label': 'Search chats', autocomplete: 'off' });
    const box = h('div', { class: 'rc' }, h('p', { class: 't-sec', text: 'Loading…' }));
    let all = { cerberus: [], claude: [], atrium: [] };
    let everyone = false;
    const scope = allUsers ? h('div', { class: 'fchips rc-scope', role: 'group', 'aria-label': 'Whose chats' },
      [['mine', 'Mine'], ['all', 'All users']].map(([k, label]) => h('button', {
        class: 'fchip', type: 'button', 'data-scope': k, 'aria-pressed': String(k === 'mine'),
        onclick: () => {
          everyone = k === 'all';
          for (const b of scope.querySelectorAll('[data-scope]')) b.setAttribute('aria-pressed', String(b.dataset.scope === k));
          load().catch((e) => clear(box).append(h('div', { class: 'alert err', text: errText(e) })));
        },
      }, label))) : null;

    async function load() {
      const [cer, cl, at] = await Promise.all([
        api(everyone ? '/api/chats?all=1' : '/api/chats').catch(() => []),
        imports ? api('/api/chats/claude').catch(() => ({ available: false, sessions: [] })) : { sessions: [] },
        imports ? api('/api/chats/atrium').catch(() => []) : [],
      ]);
      all = { cerberus: cer || [], claude: (cl && cl.sessions) || [], atrium: at || [] };
      render();
    }

    const section = (title, rows, empty) => [
      h('div', { class: 'mp-group', text: title }),
      rows.length ? h('div', { class: 'rc-list' }, rows) : h('p', { class: 't-sec rc-empty', text: empty }),
    ];

    function cerberusRow(c) {
      let armedAt = 0;
      let disarm = 0;
      const del = h('button', { class: 'btn btn-sm btn-ghost rc-del', type: 'button', title: 'Delete (press twice)', 'aria-label': `Delete ${c.title || c.id}`, disabled: c.busy || c.id === current },
        '🗑');
      del.addEventListener('click', async () => {
        const r = deletePress(armedAt, Date.now());
        armedAt = r.armedAt;
        clearTimeout(disarm);
        if (!r.delete) {
          del.textContent = 'Delete?';
          del.classList.add('armed');
          disarm = setTimeout(() => { armedAt = 0; del.textContent = '🗑'; del.classList.remove('armed'); }, 3000);
          return;
        }
        try {
          await api(`/api/chats/${encodeURIComponent(c.id)}`, { method: 'DELETE' });
          all.cerberus = all.cerberus.filter((x) => x.id !== c.id);
          render();
        } catch (e) { toast(errText(e), 'err'); }
      });
      return h('div', { class: 'rc-row' },
        h('button', { class: `rc-open${c.id === current ? ' sel' : ''}`, type: 'button', onclick: () => close({ open: c.id }) },
          h('span', { class: 'rc-title', text: c.title || 'Untitled chat' }),
          h('span', { class: 'rc-meta', text: [ownerLabel(c, me), agoText(c.updated), shortPath(c.cwd, home), c.model, c.busy ? 'running' : '', c.waiting ? `${c.waiting} waiting` : ''].filter(Boolean).join(' · ') })),
        del);
    }

    function continueRow(s, kind) {
      const go = async (btn) => {
        btn.disabled = true;
        try {
          const path = kind === 'claude' ? `/api/chats/claude/${encodeURIComponent(s.id)}/continue` : `/api/chats/atrium/${encodeURIComponent(s.id)}/continue`;
          const c = await api(path, { method: 'POST', body: {} });
          close({ open: c.id });
        } catch (e) { toast(errText(e), 'err'); btn.disabled = false; }
      };
      const meta = [agoText(s.updated), shortPath(s.cwd, home), s.model, s.messages ? `${s.messages} messages` : ''].filter(Boolean).join(' · ');
      const imported = kind === 'atrium' && s.imported;
      // A desktop session opens read-only (its title); a claude session
      // has no transcript here, only Continue.
      const label = [h('span', { class: 'rc-title', text: s.title || s.id }), h('span', { class: 'rc-meta', text: meta })];
      return h('div', { class: 'rc-row' },
        kind === 'atrium'
          ? h('button', { class: 'rc-open', type: 'button', title: 'Open read-only', onclick: () => close({ atrium: s.id }) }, ...label)
          : h('div', { class: 'rc-open static' }, ...label),
        imported
          ? h('button', { class: 'btn btn-sm', type: 'button', onclick: () => close({ open: s.imported }) }, 'Open copy')
          : h('button', { class: 'btn btn-sm', type: 'button', onclick: (e) => go(e.currentTarget) }, 'Continue here'));
    }

    function render() {
      clear(box);
      box.append(
        ...section('Cerberus', all.cerberus.map(cerberusRow), 'No chats yet.'),
        ...(all.claude.length ? section('Claude · Claude Code', all.claude.slice(0, 15).map((s) => continueRow(s, 'claude')), '') : []),
        ...(all.atrium.length ? section('Atrium (desktop LLM chat)', all.atrium.slice(0, 15).map((s) => continueRow(s, 'atrium')), '') : []));
    }

    async function find(q) {
      if (!q.trim()) { render(); return; }
      try {
        const hits = await api(`/api/chats/search?q=${encodeURIComponent(q)}`);
        const words = queryWords(q);
        clear(box).append(h('div', { class: 'mp-group', text: `${hits.length} match${hits.length === 1 ? '' : 'es'}` }),
          h('div', { class: 'rc-list' }, hits.map((x) => h('div', { class: 'rc-row' },
            h('button', { class: 'rc-open', type: 'button', onclick: () => close({ open: x.id }) },
              h('span', { class: 'rc-title' }, highlightParts(x.title || 'Untitled chat', words).map((p) => (p.mark ? h('mark', { text: p.text }) : p.text))),
              x.snippet ? h('span', { class: 'rc-snip' }, highlightParts(x.snippet, words).map((p) => (p.mark ? h('mark', { text: p.text }) : p.text))) : null,
              h('span', { class: 'rc-meta', text: [agoText(x.updated), shortPath(x.cwd, home)].filter(Boolean).join(' · ') }))))));
      } catch (e) { toast(errText(e), 'err'); }
    }
    const findSoon = debounce((q) => find(q), 250);
    search.addEventListener('input', () => findSoon(search.value));
    load().catch((e) => clear(box).append(h('div', { class: 'alert err', text: errText(e) })));
    setTimeout(() => search.focus(), 0);
    return { title: 'Recent chats', body: h('div', { class: 'rc-wrap' }, scope, search, box) };
  });
}

// openFolder chooses a chat's working folder; resolves to a path or
// undefined.
export function openFolder({ current = '' } = {}) {
  return openDialog((close) => {
    let sel = null;
    const use = h('button', { class: 'btn btn-primary', type: 'button', disabled: true, onclick: () => close(sel && sel.path) }, 'Use this folder');
    const fp = folderPicker({ purpose: 'chat', onChange: (s) => { sel = s; use.disabled = !s; } });
    return {
      title: 'Working folder',
      sub: current ? `Now: ${current}` : 'Default: your home folder',
      body: fp.el,
      foot: [h('button', { class: 'btn btn-ghost', type: 'button', onclick: () => close(undefined) }, 'Cancel'), use],
    };
  });
}
