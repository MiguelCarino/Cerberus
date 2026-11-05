// Folder picker: the detected repos as quick picks, then a browser over the
// user's home folder (/api/fs). Used by the New task form (where a git repo
// becomes an isolated worktree and any other folder is edited in place) and
// by a scratch session's "Save to folder…".

import { api } from '../api.js';
import { h, clear, errText } from '../dom.js';
import { breadcrumbs, folderNote, repoName } from '../util.js';

// The picker's styles live in their own file (app.css keeps the navbar
// rules); load it once, on first use of this module.
if (!document.querySelector('link[data-ws-css]')) {
  document.head.append(h('link', { rel: 'stylesheet', href: 'assets/workspace.css', 'data-ws-css': '' }));
}

// folderPicker returns { el, value() } where value() is the chosen
// { path, git } or null. onChange fires on every choice.
// browse: false offers only the repo quick picks (users and roles: a
// non-admin's sessions run in scratch folders or repos shared with them;
// the host folder browser is admin only).
export function folderPicker({ repos = [], purpose = 'task', onChange, browse = true } = {}) {
  let listing = null;
  let home = '';
  let selected = null;
  let hidden = false;
  let loading = false;

  const quick = h('div', { class: 'fp-quick' });
  const crumbs = h('div', { class: 'fp-crumbs' });
  const list = h('div', { class: 'fp-list', role: 'list' });
  const useBtn = h('button', { class: 'btn', type: 'button', onclick: () => pickCurrent() }, 'Use this folder');
  const hiddenBtn = h('button', { class: 'fchip', type: 'button', 'aria-pressed': 'false', onclick: () => toggleHidden() }, 'Hidden');
  const chosen = h('div', { class: 'fp-chosen' });
  const el = h('div', { class: 'fp' }, quick,
    h('div', { class: 'fp-bar' }, crumbs, hiddenBtn),
    list,
    h('div', { class: 'fp-foot' }, useBtn),
    chosen);

  if (!browse) {
    for (const part of el.querySelectorAll('.fp-bar, .fp-list, .fp-foot')) part.hidden = true;
    if (!repos.length) quick.append(h('p', { class: 't-sec', text: 'No repos are shared with you. Ask an admin, or choose None.' }));
  }
  if (purpose === 'task' && repos.length) {
    quick.append(h('div', { class: 'fp-label', text: 'Detected repos' }),
      h('div', { class: 'fp-chips' }, repos.map((r) =>
        h('button', { class: 'fchip', type: 'button', onclick: () => choose({ path: r.path, git: true }) },
          r.name || repoName(r.path)))));
  }

  async function load(path) {
    loading = true;
    renderList();
    try {
      const q = new URLSearchParams();
      if (path) q.set('path', path);
      if (hidden) q.set('hidden', '1');
      const qs = q.toString();
      listing = await api('/api/fs' + (qs ? '?' + qs : ''));
      if (listing.home) home = listing.path;
    } catch (e) {
      clear(list).append(h('div', { class: 'alert err', text: errText(e) }));
      loading = false;
      return;
    }
    loading = false;
    render();
  }

  function choose(sel) {
    selected = sel;
    renderChosen();
    onChange?.(selected);
  }

  function pickCurrent() {
    if (!listing || listing.home) return;
    choose({ path: listing.path, git: !!listing.git });
  }

  function toggleHidden() {
    hidden = !hidden;
    hiddenBtn.setAttribute('aria-pressed', String(hidden));
    load(listing ? listing.path : '');
  }

  function render() {
    clear(crumbs).append(...breadcrumbs(listing.path, home).map((c, i, all) =>
      i === all.length - 1
        ? h('span', { class: 'fp-crumb current', text: c.name })
        : h('button', { class: 'fp-crumb', type: 'button', onclick: () => load(c.path) }, c.name)));
    useBtn.disabled = !!listing.home;
    useBtn.textContent = listing.home ? 'Open a folder to use it' : `Use “${repoName(listing.path)}”`;
    renderList();
  }

  function renderList() {
    clear(list);
    if (loading) { list.append(h('div', { class: 'fp-empty', text: 'Loading…' })); return; }
    if (!listing) return;
    if (listing.parent) {
      list.append(h('button', { class: 'fp-row up', type: 'button', role: 'listitem', onclick: () => load(listing.parent) },
        h('span', { class: 'fp-name', text: '..' })));
    }
    if (!listing.entries.length) list.append(h('div', { class: 'fp-empty', text: 'No sub-folders' }));
    for (const e of listing.entries) {
      list.append(h('button', { class: 'fp-row', type: 'button', role: 'listitem', onclick: () => load(e.path) },
        h('span', { class: 'fp-name', text: e.name }),
        e.git ? h('span', { class: 'fp-git', text: 'git' }) : null,
        h('span', { class: 'fp-go', 'aria-hidden': 'true', text: '›' })));
    }
  }

  function renderChosen() {
    clear(chosen);
    if (!selected) return;
    const note = folderNote(selected, purpose);
    chosen.append(h('div', { class: 'fp-sel' }, h('span', { class: 'fp-label', text: 'Chosen' }), h('code', { text: selected.path })));
    if (note) chosen.append(h('div', { class: `alert ${note.tone}`, text: note.text }));
  }

  if (browse) load('');
  return { el, value: () => selected };
}
