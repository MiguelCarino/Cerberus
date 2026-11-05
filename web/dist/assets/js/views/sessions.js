// Sessions view: history search with filter chips, fleet tasks, the list
// grouped by state (parent/child tree, pins, priority, tags, inline rename,
// spawn approval), multi-select with the bulk bar, and the New task form.
//
// The Wall lives here too, steered by the Active / All filter in the head:
// Active shows the live tiles of the active sessions as a grid and only
// those sessions below; All shows every session with the tiles as one short
// strip. A visit starts on Active when something is running, else All. A
// tile tapped fills the screen with its input. #/wall opens this view on
// Active; #/wall/a,b,c stays the full-screen watch page for a chosen set
// (views/wall.js).

import { previewChip } from '../preview.js';
import { api, post, isReachable, onReachability } from '../api.js';
import { h, clear, openDialog, confirmDialog, toast, errText, pill } from '../dom.js';
import { store } from '../store.js';
import { groupSessions, stateTone, relTime, workspaceKind, workspaceLabel, repoName, debounce, isLive } from '../util.js';
import { queryWords, highlightParts } from '../text.js';
import { batchSessions, stateCounts, countChips, batchProgress } from '../fleet.js';
import {
  sessionName, buildTree, allTags, hasTag, parseTags, clampPriority, needsApproval, moveCursor, rowKey, isWatchable,
  PRIORITY_MIN, PRIORITY_MAX,
} from '../control.js';
import { folderPicker } from './picker.js';
import { field, loadChoices, agentPickers, textWithMic } from './form.js';
import { micControl } from './mic.js';
import { newFleetTask } from './fleetform.js';
import { bulkBar } from './bulkbar.js';
import { patchSession, approveSession, deleteSessions, trashIcon } from './ctl.js';
import { approvalButtons, approvalText } from './approval.js';
import { feedButton } from './feed.js';
import { setShortcutTarget, takePending } from '../shortcuts.js';
import { attachmentsField } from './attach.js';
import { pushDropTarget } from './drop.js';
import { mount as mountWall } from './wall.js';
import { canWriteSessions, ownerLabel, ownerChoices, ownedBy } from '../users.js';

const FILTERS = [['repo', 'Any repo'], ['agent', 'Any agent'], ['state', 'Any state']];

// Kept across visits: coming back from a session keeps the selection,
// the tag filter and open editors.
const selected = new Set();
const editing = new Set();
let tagFilter = '';
let ownerFilter = ''; // users and roles: one owner's sessions ("" = everyone)
// Priority steps not yet confirmed by the server (id -> value): an editor
// rebuilt meanwhile starts from here, not from the store's older value.
const pendingPrio = new Map();

export function mount(root, r = {}) {
  // Users and roles: a viewer watches (no New task, no bulk bar, no
  // approvals, no row tools); admins and viewers see whose each session is
  // and can filter by owner.
  const me = store.me();
  const writable = canWriteSessions(me);
  const watchAll = !!(me && me.hasUsers && store.can('sessions.watch_all'));
  if (!writable) selected.clear();
  // 'active' or 'all'; #/wall asks for Active.
  let show = r.layout === 'wall' || store.sessions().some(isWatchable) ? 'active' : 'all';
  let showPicked = r.layout === 'wall';
  if (r.layout) history.replaceState(null, '', '#/sessions');
  const list = h('div', { class: 'sess-groups' });
  const fleetEl = h('div', { class: 'fleet-list' });
  const results = h('div', { class: 'hist-results', hidden: true });
  const tagBar = h('div', { class: 'tag-filter', role: 'group', 'aria-label': 'Filter by tag' });
  // ---------- Active / All ----------
  const showSeg = h('div', { class: 'sess-show', role: 'radiogroup', 'aria-label': 'Show' });
  function drawShow() {
    const n = store.sessions().filter(isWatchable).length;
    const total = store.sessions().length;
    showSeg.replaceChildren(...[['active', 'Active', n, 'Running and waiting sessions, with their live terminals'],
      ['all', 'All', total, 'Every session; live terminals as a strip']].map(([v, label, count, title]) => h('button', {
      class: `toggle-btn${show === v ? ' active' : ''}`, type: 'button', role: 'radio', title,
      'aria-checked': show === v ? 'true' : 'false',
      onclick: () => setShow(v, true),
    }, label, h('span', { class: 'sess-show-n', text: store.loaded() ? String(count) : '' }))));
  }
  function setShow(v, picked = false) {
    if (picked) showPicked = true;
    if (show === v) return;
    show = v;
    wall.setStrip(v === 'all');
    drawShow();
    render();
  }
  const ownerSel = watchAll ? h('select', {
    class: 'select fchip-select owner-filter', 'aria-label': 'Owner',
    onchange: (e) => { ownerFilter = e.target.value; render(); },
  }, h('option', { value: '', text: 'Everyone' })) : null;
  const head = h('div', { class: 'view-head' },
    h('div', {}, h('h1', { class: 'view-title' }, 'Sessions', writable ? null : h('span', { class: 'ro-label', text: 'Read only' })),
      h('p', { class: 'view-sub', text: writable ? 'Agents in a repo worktree, a folder of yours, or a scratch folder.' : 'Every session, live. You can watch, not steer.' })),
    h('div', { class: 'head-actions' },
      showSeg,
      ownerSel,
      feedButton(),
      writable ? h('button', { class: 'btn btn-big', type: 'button', onclick: () => newFleetTask() },
        '+ ', h('span', { class: 'wide-only', text: 'New ' }), 'fleet task') : null,
      writable ? h('button', { class: 'btn btn-primary btn-big', type: 'button', onclick: () => newTask() }, '+ New task') : null));
  const bulk = bulkBar({
    ids: () => [...selected].filter((id) => store.session(id)),
    clear: () => { selected.clear(); render(); },
  });

  // ---------- history search ----------
  const filters = { repo: '', agent: '', state: '' };
  const searchInput = h('input', {
    class: 'input hist-input', type: 'search', placeholder: 'Search titles, tasks and transcripts', 'aria-label': 'Search sessions',
    autocomplete: 'off', autocapitalize: 'off', spellcheck: 'false', enterkeyhint: 'search',
    oninput: () => { syncChips(); searchSoon(); },
  });
  const selects = Object.fromEntries(FILTERS.map(([key, any]) => [key, h('select', {
    class: 'fchip fchip-select', 'aria-label': any,
    onchange: (e) => { filters[key] = e.target.value; syncChips(); searchSoon.cancel(); runSearch(); },
  }, h('option', { value: '', text: any }))]));
  const clearChip = h('button', { class: 'fchip clear', type: 'button', hidden: true, onclick: () => resetSearch() }, 'Clear');
  const bar = h('div', { class: 'hist-bar' }, searchInput,
    h('div', { class: 'hist-chips' }, FILTERS.map(([k]) => selects[k]), clearChip));

  const wallBox = h('div', { class: 'sess-wall' });
  const view = h('section', { class: 'view view-pad view-sessions' }, head, wallBox, bar, results, fleetEl, tagBar, list);
  root.append(view, bulk.el);
  const wall = mountWall(wallBox, { embedded: true, strip: show === 'all' });

  const searching = () => !!(searchInput.value.trim() || filters.repo || filters.agent || filters.state);
  let hits = null;
  let words = [];
  let ctl = null;
  let gone = false;

  function syncChips() {
    for (const [k] of FILTERS) selects[k].classList.toggle('active', !!filters[k]);
    clearChip.hidden = !searching();
    const on = searching();
    results.hidden = !on;
    list.hidden = on;
    fleetEl.hidden = on;
    tagBar.hidden = on || !tagBar.childElementCount;
  }

  // The owner filter lists the owners of the sessions the store knows.
  function fillOwners() {
    if (!ownerSel) return;
    const opts = ownerChoices(store.sessions());
    if (ownerFilter && !opts.some((o) => o.id === ownerFilter)) opts.push({ id: ownerFilter, name: ownerFilter });
    const sig = opts.map((o) => o.id + '=' + o.name).join(',');
    if (ownerSel.dataset.sig !== sig) {
      ownerSel.dataset.sig = sig;
      ownerSel.replaceChildren(h('option', { value: '', text: 'Everyone' }), ...opts.map((o) => h('option', { value: o.id, text: o.name })));
    }
    ownerSel.value = ownerFilter;
    ownerSel.classList.toggle('active', !!ownerFilter);
  }

  // Filter choices come from the sessions the store knows; a chosen value
  // stays even when no listed session has it any more.
  function fillFilters() {
    const all = store.sessions();
    const values = {
      repo: all.filter((s) => workspaceKind(s) !== 'scratch').map((s) => repoName(s.Dir || s.Repo)),
      agent: all.map((s) => s.Agent),
      state: all.map((s) => s.State),
    };
    for (const [k, any] of FILTERS) {
      const set = new Set(values[k].filter(Boolean));
      if (filters[k]) set.add(filters[k]);
      const opts = [...set].sort((a, b) => a.localeCompare(b));
      const sel = selects[k];
      const have = [...sel.options].slice(1).map((o) => o.value);
      if (have.join('\n') === opts.join('\n')) continue;
      clear(sel).append(h('option', { value: '', text: any }), ...opts.map((v) => h('option', { value: v, text: v })));
      sel.value = filters[k];
    }
  }

  async function runSearch() {
    if (ctl) ctl.abort();
    ctl = null;
    if (!searching()) { hits = null; renderResults(); return; }
    const q = searchInput.value.trim();
    const params = new URLSearchParams();
    if (q) params.set('q', q);
    for (const [k] of FILTERS) if (filters[k]) params.set(k, filters[k]);
    const mine = new AbortController();
    ctl = mine;
    clear(results).append(h('p', { class: 't-sec hist-status', text: 'Searching…' }));
    try {
      const out = await api(`/api/search?${params}`, { signal: mine.signal });
      if (gone || ctl !== mine) return;
      hits = Array.isArray(out) ? out : [];
      words = queryWords(q);
      renderResults();
    } catch (e) {
      if (gone || mine.signal.aborted) return;
      clear(results).append(h('div', { class: 'alert err', text: errText(e) }));
    }
  }
  const searchSoon = debounce(runSearch, 300);

  function resetSearch() {
    searchInput.value = '';
    for (const [k] of FILTERS) { filters[k] = ''; selects[k].value = ''; }
    hits = null;
    if (ctl) ctl.abort();
    syncChips();
  }

  function renderResults() {
    if (!hits) return;
    clear(results);
    results.append(h('p', { class: 'hist-status', text: hits.length ? `${hits.length} session${hits.length === 1 ? '' : 's'}` : 'No sessions match.' }));
    for (const hit of hits) {
      const s = (hit.Session && store.session(hit.Session.ID)) || hit.Session;
      if (!s) continue;
      results.append(h('div', { class: 'hit' }, row(s, 0, false),
        (hit.Snippets || []).length ? h('div', { class: 'hit-snips' }, hit.Snippets.map((sn) => snippet(sn, words))) : null));
    }
  }

  // ---------- fleet tasks ----------
  let batches = [];
  const loadBatches = async () => {
    try {
      const out = await api('/api/batches');
      if (gone) return;
      batches = (Array.isArray(out) ? out : []).sort((a, b) => (Date.parse(b.Created) || 0) - (Date.parse(a.Created) || 0));
      renderFleet();
    } catch { /* the list still works without fleet tasks */ }
  };
  const loadBatchesSoon = debounce(loadBatches, 500);

  function renderFleet() {
    clear(fleetEl);
    if (!batches.length) return;
    const open = [];
    const old = [];
    for (const b of batches) {
      const counts = liveCounts(b);
      const p = batchProgress(counts);
      const settled = (counts.accepted || 0) + (counts.discarded || 0) === p.total && p.total > 0;
      (settled ? old : open).push(batchRow(b, counts, p));
    }
    if (open.length) {
      fleetEl.append(h('h2', { class: 'group-title' }, 'Fleet tasks', h('span', { class: 'group-count', text: String(open.length) })),
        h('div', { class: 'sess-list' }, open));
    }
    if (old.length) {
      fleetEl.append(h('details', { class: 'fleet-old' },
        h('summary', { class: 'group-title' }, 'Closed fleet tasks', h('span', { class: 'group-count', text: String(old.length) })),
        h('div', { class: 'sess-list' }, old)));
    }
  }

  // ---------- the list ----------
  let order = []; // ids in display order, for j/k
  let cursor = -1;
  let dirty = false;
  let pressing = false; // a pointer is down in the view: a rebuild now would eat the click
  const rowKeys = new Map(); // id -> rowKey of its rendered row

  function renderTags() {
    const tags = allTags(store.sessions());
    if (tagFilter && !tags.includes(tagFilter)) tags.push(tagFilter);
    clear(tagBar);
    if (tags.length) {
      tagBar.append(h('span', { class: 'fchip-lbl', text: 'Tags' }),
        ...tags.map((t) => h('button', {
          class: 'fchip', type: 'button', 'aria-pressed': String(t === tagFilter),
          onclick: () => { tagFilter = t === tagFilter ? '' : t; render(); },
        }, t)));
    }
    tagBar.hidden = searching() || !tags.length;
  }

  const render = () => {
    // Never rebuild under a half-typed rename or tag: wait for focusout.
    const ae = document.activeElement;
    if (ae && list.contains(ae) && ae.closest('.sess-edit')) { dirty = true; return; }
    if (pressing) { dirty = true; return; }
    dirty = false;
    rowKeys.clear();
    fillFilters();
    renderTags();
    for (const id of [...selected]) if (!store.session(id)) selected.delete(id);
    clear(list);
    order = [];
    // Before the first list arrives, Active was a guess: settle it once.
    if (!showPicked && store.loaded()) { showPicked = true; if (show === 'active' && !store.sessions().some(isWatchable)) { show = 'all'; wall.setStrip(true); } }
    drawShow();
    fillOwners();
    const tagged = ownedBy(store.sessions(), ownerFilter).filter((s) => hasTag(s, tagFilter));
    const all = show === 'active' ? tagged.filter(isWatchable) : tagged;
    const groups = groupSessions(all);
    if (!groups.length && show === 'active' && store.loaded() && tagged.length) {
      list.append(h('div', { class: 'empty' },
        h('p', { class: 'empty-title', text: tagFilter ? `Nothing tagged ${tagFilter} is running` : 'Nothing is running now' }),
        h('p', { class: 't-sec', text: `${tagged.length} finished or stopped session${tagged.length === 1 ? '' : 's'}.` }),
        h('button', { class: 'btn btn-primary', type: 'button', onclick: () => setShow('all', true) }, 'Show all')));
    } else if (!groups.length) {
      list.append(h('div', { class: 'empty' },
        h('p', { class: 'empty-title', text: !store.loaded() ? (isReachable() ? 'Loading…' : 'Waiting for the host…') : tagFilter ? `No sessions tagged ${tagFilter}` : 'No sessions yet' }),
        store.loaded() && !tagFilter ? h('p', { class: 't-sec', text: 'Start one with “New task”: describe the work, choose an agent, and pick a folder now or later.' }) : null));
    }
    for (const g of groups) {
      const rows = buildTree(g.items).map(({ s, level, orphan }) => { order.push(s.ID); return row(s, level, orphan); });
      list.append(h('div', { class: `sess-group g-${g.key}` },
        h('h2', { class: 'group-title' }, g.label, h('span', { class: 'group-count', text: String(g.items.length) })),
        h('div', { class: 'sess-list' }, rows)));
    }
    if (cursor >= order.length) cursor = order.length - 1;
    markCursor(false);
    bulk.update();
  };
  const renderSoon = debounce(render, 150);
  list.addEventListener('focusout', () => { if (dirty) setTimeout(() => { if (!gone) render(); }, 0); });
  view.addEventListener('pointerdown', () => { pressing = true; });
  const onPointerUp = () => {
    if (!pressing) return;
    pressing = false;
    // After the click has been dispatched to the button that was pressed.
    if (dirty) setTimeout(() => { if (!gone && !pressing) render(); }, 0);
  };
  window.addEventListener('pointerup', onPointerUp);
  window.addEventListener('pointercancel', onPointerUp);

  // patchRow updates the live parts of a session's rendered rows (activity
  // line, time) without rebuilding them. False when it has no row.
  function patchRow(s) {
    const els = view.querySelectorAll(`.sess-item[data-id="${CSS.escape(s.ID)}"]`);
    for (const el of els) {
      const main = el.querySelector('.sess-main');
      let act = el.querySelector('.sess-activity');
      const text = isLive(s.State) && s.Activity ? s.Activity : '';
      if (text && act) act.textContent = text;
      else if (text && main) main.append(h('div', { class: 'sess-activity', text }));
      else if (act) act.remove();
      const when = el.querySelector('.sess-when');
      if (when) when.textContent = relTime(s.Updated);
    }
    return els.length > 0;
  }

  function markCursor(scroll) {
    for (const el of list.querySelectorAll('.sess-item.cursor')) el.classList.remove('cursor');
    const id = order[cursor];
    if (!id) return;
    const el = list.querySelector(`.sess-item[data-id="${CSS.escape(id)}"]`);
    if (!el) return;
    el.classList.add('cursor');
    if (scroll) el.scrollIntoView({ block: 'nearest' });
  }

  function toggleSelect(id, on = !selected.has(id)) {
    if (on) selected.add(id); else selected.delete(id);
    const el = list.querySelector(`.sess-item[data-id="${CSS.escape(id)}"]`);
    if (el) {
      el.classList.toggle('selected', on);
      const cb = el.querySelector('.sess-check input');
      if (cb) cb.checked = on;
    }
    bulk.update();
  }

  // row is one session: checkbox, the link, pin, approve/reject or state,
  // the edit toggle, and (when open) the inline editor.
  function row(s, level, orphan) {
    const tone = stateTone(s.State);
    const name = sessionName(s);
    const isSel = selected.has(s.ID);
    const pending = needsApproval(s);
    rowKeys.set(s.ID, rowKey(s));
    const parent = orphan && s.Parent ? store.session(s.Parent) : null;
    const cb = h('input', { type: 'checkbox', 'aria-label': `Select ${name}`, onchange: (e) => toggleSelect(s.ID, e.target.checked) });
    cb.checked = isSel;
    // Tag chips sit inside the card, under the link, so they stay buttons
    // (a tap filters the list) without opening the session.
    const tagRow = (s.Tags || []).length ? h('div', { class: 'tag-row' }, s.Tags.map((t) => h('button', {
      class: `tag-chip${t === tagFilter ? ' on' : ''}`, type: 'button', title: `Show only ${t}`,
      onclick: () => { tagFilter = t === tagFilter ? '' : t; render(); },
    }, t))) : null;
    // The preview chip opens the work's preview in a new window.
    const pv = previewChip(s);
    const pvLink = pv ? h('a', { class: 'pv-chip', href: pv.url, target: '_blank', rel: 'noopener', title: 'Open the preview in a new window' }, `${pv.label} ↗`) : null;
    const item = h('div', {
      class: `sess-item${isSel ? ' selected' : ''}${level ? ' child' : ''}`, 'data-id': s.ID,
      style: level ? `--level:${Math.min(level, 4)}` : null,
    },
    h('div', { class: `sess-row tone-${tone}` },
      writable ? h('label', { class: 'sess-check' }, cb) : null,
      h('div', { class: 'sess-body' }, h('a', { class: 'sess-main', href: `#/s/${encodeURIComponent(s.ID)}`, ondblclick: (e) => { if (!writable) return; e.preventDefault(); openEditor(s.ID, true); } },
        h('div', { class: 'sess-title' },
          level ? h('span', { class: 'tree-mark', 'aria-hidden': 'true', text: '↳ ' }) : null,
          s.Pinned ? h('span', { class: 'pin-mark', title: 'Pinned', text: '● ' }) : null,
          name),
        h('div', { class: 'sess-meta' },
          h('span', { text: workspaceLabel(s) }),
          workspaceKind(s) !== 'repo' ? h('span', { class: 'ws-tag', text: workspaceKind(s) }) : null,
          h('span', { text: s.Agent + (s.Model ? ` · ${s.Model}` : '') }),
          s.Attempt > 1 ? h('span', { text: `attempt ${s.Attempt}` }) : null,
          s.Priority ? h('span', { class: 'prio-mark', text: `P${s.Priority > 0 ? '+' : ''}${s.Priority}` }) : null,
          parent ? h('span', { class: 'from-mark', text: `from ${sessionName(parent)}` }) : null,
          h('span', { class: 'sess-when', text: relTime(s.Updated) }),
          ownerLabel(s, me) ? h('span', { class: 'owner-chip', title: 'Owner', text: ownerLabel(s, me) }) : null),
        s.State === 'waiting' && s.WaitingReason ? h('div', { class: 'sess-reason', text: approvalText(s) || s.WaitingReason }) : null,
        s.State === 'queued' && s.WaitingReason ? h('div', { class: 'sess-reason muted', text: s.WaitingReason }) : null,
        isLive(s.State) && s.Activity ? h('div', { class: 'sess-activity', text: s.Activity }) : null), pvLink, tagRow),
      h('div', { class: 'sess-side' },
        !writable ? (s.State === 'waiting' ? h('span', { class: 'badge-wait', text: 'Waiting' }) : pill(s.State, tone))
        : pending
          ? h('div', { class: 'approve-btns' },
            h('button', { class: 'btn btn-primary btn-sm', type: 'button', onclick: () => approveSession(s.ID, true) }, 'Approve'),
            h('button', { class: 'btn btn-danger btn-sm', type: 'button', onclick: () => reject(s) }, 'Reject'))
          : approvalButtons(s) || (s.State === 'waiting' ? h('span', { class: 'badge-wait', text: 'Waiting' }) : pill(s.State, tone)),
        !writable ? null : h('div', { class: 'sess-tools' },
          h('button', {
            class: `icon-btn${s.Pinned ? ' on' : ''}`, type: 'button', 'aria-pressed': String(!!s.Pinned),
            title: s.Pinned ? 'Unpin' : 'Pin to the top', 'aria-label': s.Pinned ? `Unpin ${name}` : `Pin ${name}`,
            onclick: () => patchSession(s.ID, { Pinned: !s.Pinned }),
          }, pinIcon()),
          h('button', {
            class: `icon-btn${editing.has(s.ID) ? ' on' : ''}`, type: 'button', 'aria-expanded': String(editing.has(s.ID)),
            title: 'Rename, priority, tags', 'aria-label': `Edit ${name}`, onclick: () => openEditor(s.ID, !editing.has(s.ID)),
          }, '⋯'),
          h('button', {
            class: 'icon-btn sess-del', type: 'button', title: 'Delete', 'aria-label': `Delete ${name}`,
            onclick: () => deleteSessions([s.ID]),
          }, trashIcon())))),
    writable && editing.has(s.ID) ? editor(s) : null);
    return item;
  }

  function openEditor(id, open) {
    if (open) editing.add(id); else editing.delete(id);
    render();
    if (open) list.querySelector(`.sess-item[data-id="${CSS.escape(id)}"] .sess-edit input`)?.focus();
  }

  async function reject(s) {
    const ok = await confirmDialog({ title: 'Reject spawn', ok: 'Reject', danger: true,
      message: `An agent asked to start “${sessionName(s)}”. Rejecting means it never runs.` });
    if (ok) approveSession(s.ID, false);
  }

  // editor: inline rename, priority stepper and tag chips for one session.
  function editor(s) {
    const nameIn = h('input', { class: 'input input-sm', value: s.Name || '', placeholder: s.Title || s.Task || 'Name', 'aria-label': 'Name', maxlength: '120' });
    const saveName = async () => {
      const v = nameIn.value.trim();
      if (v === (s.Name || '')) return;
      await patchSession(s.ID, { Name: v }, v ? 'Renamed' : 'Name cleared');
    };
    nameIn.addEventListener('keydown', (e) => {
      if (e.key === 'Enter') { e.preventDefault(); saveName().then(() => openEditor(s.ID, false)); }
      if (e.key === 'Escape') { e.preventDefault(); nameIn.value = s.Name || ''; openEditor(s.ID, false); }
    });
    let prio = pendingPrio.has(s.ID) ? pendingPrio.get(s.ID) : (s.Priority || 0);
    const prioVal = h('output', { class: 'prio-val', text: String(prio) });
    const pushPrio = debounce(() => {
      const sent = prio;
      patchSession(s.ID, { Priority: sent }).finally(() => {
        if (pendingPrio.get(s.ID) === sent) pendingPrio.delete(s.ID);
      });
    }, 400);
    const step = (d) => {
      prio = clampPriority(prio + d);
      pendingPrio.set(s.ID, prio);
      prioVal.textContent = String(prio);
      pushPrio();
    };
    const tagIn = h('input', { class: 'input input-sm', placeholder: 'add tag', 'aria-label': 'Add tags', autocapitalize: 'off', spellcheck: 'false', maxlength: '60' });
    const addTags = async () => {
      if (!tagIn.value.trim()) return;
      const { tags, errors } = parseTags(tagIn.value, s.Tags || []);
      if (errors.length) { toast(errors[0], 'warn'); return; }
      tagIn.value = '';
      await patchSession(s.ID, { Tags: tags });
    };
    tagIn.addEventListener('keydown', (e) => { if (e.key === 'Enter') { e.preventDefault(); addTags(); } });
    return h('div', { class: 'sess-edit' },
      h('div', { class: 'edit-field' }, h('span', { class: 'field-label', text: 'Name' }),
        h('div', { class: 'edit-inline' }, nameIn, h('button', { class: 'btn btn-sm', type: 'button', onclick: saveName }, 'Save'))),
      h('div', { class: 'edit-field' }, h('span', { class: 'field-label', text: 'Priority' }),
        h('div', { class: 'stepper', role: 'group', 'aria-label': 'Priority' },
          h('button', { class: 'btn btn-sm', type: 'button', 'aria-label': 'Lower priority', onclick: () => step(-1) }, '−'),
          prioVal,
          h('button', { class: 'btn btn-sm', type: 'button', 'aria-label': 'Raise priority', onclick: () => step(1) }, '+'),
          h('span', { class: 'stepper-hint', text: `${PRIORITY_MIN}..${PRIORITY_MAX}, higher starts first` }))),
      h('div', { class: 'edit-field' }, h('span', { class: 'field-label', text: 'Tags' }),
        h('div', { class: 'edit-inline tag-edit' },
          (s.Tags || []).map((t) => h('span', { class: 'tag-chip' }, t,
            h('button', { class: 'tag-x', type: 'button', 'aria-label': `Remove tag ${t}`,
              onclick: () => patchSession(s.ID, { Tags: (s.Tags || []).filter((x) => x !== t) }) }, '×'))),
          tagIn, h('button', { class: 'btn btn-sm', type: 'button', onclick: addTags }, 'Add'))));
  }

  // ---------- keyboard (see shortcuts.js) ----------
  const wallFocused = () => document.body.hasAttribute('data-wall-focus');
  setShortcutTarget({
    down: () => { if (wallFocused()) return; cursor = moveCursor(cursor, 1, order.length); markCursor(true); },
    up: () => { cursor = moveCursor(cursor, -1, order.length); markCursor(true); },
    select: () => { if (writable && order[cursor]) toggleSelect(order[cursor]); },
    open: () => { if (order[cursor]) location.hash = `#/s/${encodeURIComponent(order[cursor])}`; },
    bulk: () => { if (!writable) return; if (!selected.size && order[cursor]) toggleSelect(order[cursor], true); bulk.focus(); },
    search: () => searchInput.focus(),
    new: () => { if (writable) newTask(); },
    escape: () => {
      if (wall.unfocus()) return true;
      if (selected.size) { selected.clear(); render(); return true; }
      if (cursor >= 0) { cursor = -1; markCursor(false); return true; }
      return false;
    },
  });

  const known = (gid) => batches.some((b) => b.ID === gid);
  const unsub = store.subscribe((kind, payload) => {
    if (kind !== 'sessions' && kind !== 'session') return;
    // Activity lines arrive about once a second per running session: when
    // nothing a row is built from changed, patch it in place; otherwise
    // coalesce into one render.
    if (kind === 'session' && payload && rowKeys.get(payload.ID) === rowKey(payload) && patchRow(payload)) return;
    if (kind === 'session') renderSoon(); else render();
    renderFleet();
    if (hits) renderResults();
    if (kind === 'session' && payload && payload.Group && !known(payload.Group)) loadBatchesSoon();
  });
  const onVisible = () => { loadBatches(); if (searching()) runSearch(); };
  window.addEventListener('cerberus:visible', onVisible);

  // Offline: the empty list says it is waiting instead of loading forever.
  const offReach = onReachability(() => { if (!store.loaded()) render(); });
  syncChips();
  render();
  store.refreshSessions().catch(() => {});
  loadBatches();
  // Files dropped on Sessions start a new task with them attached.
  const popDrop = writable ? pushDropTarget({ dest: () => ({ kind: 'task' }), onFiles: (files) => newTask(files) }) : () => {};
  const pending = takePending();
  if (pending === 'new' && writable) setTimeout(() => newTask(), 0);
  if (pending === 'search') setTimeout(() => searchInput.focus(), 0);
  return () => {
    gone = true;
    offReach();
    popDrop();
    if (ctl) ctl.abort();
    searchSoon.cancel();
    loadBatchesSoon.cancel();
    renderSoon.cancel();
    unsub();
    window.removeEventListener('pointerup', onPointerUp);
    window.removeEventListener('pointercancel', onPointerUp);
    bulk.destroy();
    wall();
    setShortcutTarget(null);
    window.removeEventListener('cerberus:visible', onVisible);
  };
}

function pinIcon() {
  const ns = 'http://www.w3.org/2000/svg';
  const svg = document.createElementNS(ns, 'svg');
  svg.setAttribute('viewBox', '0 0 16 16');
  svg.setAttribute('width', '16');
  svg.setAttribute('height', '16');
  svg.setAttribute('aria-hidden', 'true');
  const p = document.createElementNS(ns, 'path');
  p.setAttribute('d', 'M6 1.5h4l-.6 4.2 2.6 2.3v1.5H8.7L8 15l-.7-5.5H4V8l2.6-2.3z');
  p.setAttribute('fill', 'currentColor');
  svg.append(p);
  return svg;
}

function liveCounts(b) {
  const list = batchSessions(b.Sessions, [], (id) => store.session(id));
  // Sessions the store has not seen yet fall back to the server's counts.
  return list.length === (b.Sessions || []).length ? stateCounts(list) : (b.Counts || stateCounts(list));
}

function batchRow(b, counts, p) {
  return h('a', { class: 'sess-row fleet-row', href: `#/b/${encodeURIComponent(b.ID)}` },
    h('div', { class: 'sess-main' },
      h('div', { class: 'sess-title', text: b.Title || b.Task || b.ID }),
      h('div', { class: 'sess-meta' },
        h('span', { text: `${p.total} repo${p.total === 1 ? '' : 's'}` }),
        h('span', { text: b.Agent + (b.Model ? ` · ${b.Model}` : '') }),
        h('span', { text: relTime(b.Created) })),
      h('div', { class: 'count-chips' }, countChips(counts).map((c) => pill(`${c.n} ${c.state}`, stateTone(c.state))))),
    h('span', { class: 'fleet-prog', text: `${p.finished}/${p.total}` }));
}

function snippet(text, words) {
  return h('p', { class: 'hit-snip' },
    highlightParts(text, words).map((part) => (part.mark ? h('mark', { text: part.text }) : part.text)));
}

// ---------- New task ----------

// newTask opens the New task form, with files already attached when given
// (dropped on Sessions, or handed over through the Carino Bridge).
export async function newTask(initialFiles = []) {
  let repos = [];
  let choices;
  try {
    [repos, choices] = await Promise.all([api('/api/repos'), loadChoices()]);
  } catch (e) {
    toast(errText(e), 'err');
    return;
  }
  const att = attachmentsField();
  if (initialFiles.length) att.add(initialFiles);
  // While the form is open, a drop anywhere attaches to it.
  const popDrop = pushDropTarget({ dest: () => ({ kind: 'task' }), onFiles: (files) => att.add(files) });
  const created = await openDialog((close) => taskForm({ repos: repos || [], ...choices, att, close }));
  popDrop();
  if (!created) att.discard();
  if (created && created.length === 1) location.hash = `#/s/${encodeURIComponent(created[0].ID)}`;
}

function taskForm({ repos, models, agents, defaultAgent, att, close }) {
  // Users and roles: only admins browse host folders; others pick a repo
  // an admin shared with them.
  const host = store.can('host');
  // Workspace: none (a scratch folder; save its files anywhere later) or a
  // folder, where a git repo gets an isolated worktree and any other folder
  // is edited in place.
  let wsKind = 'scratch';
  let picker = null;
  const pickerBox = h('div', { class: 'ws-picker', hidden: true });
  const noneBtn = h('button', { class: 'toggle-btn active', type: 'button', 'aria-pressed': 'true', onclick: () => setKind('scratch') }, 'None (decide later)');
  const folderBtn = h('button', { class: 'toggle-btn', type: 'button', 'aria-pressed': 'false', hidden: !host && !repos.length, onclick: () => setKind('folder') }, host ? 'Folder' : 'Shared repo');
  const wsHint = h('p', { class: 'field-hint' });
  const setKind = (kind) => {
    wsKind = kind;
    for (const [b, k] of [[noneBtn, 'scratch'], [folderBtn, 'folder']]) {
      b.classList.toggle('active', k === kind);
      b.setAttribute('aria-pressed', String(k === kind));
    }
    pickerBox.hidden = kind !== 'folder';
    if (kind === 'folder' && !picker) {
      picker = folderPicker({ repos, purpose: 'task', browse: host, onChange: () => syncSub() });
      pickerBox.append(picker.el);
    }
    wsHint.textContent = kind === 'scratch'
      ? 'The agent starts in an empty scratch folder. Save its files to a folder or download them as a zip whenever you like.'
      : '';
    syncSub();
  };

  const task = h('textarea', { class: 'textarea', rows: '5', required: true, placeholder: 'What should the agent do?' });
  const mic = micControl(task);
  const title = h('input', { class: 'input', placeholder: 'Optional short title' });
  const pick = agentPickers({ agents, models, defaultAgent });
  const count = h('input', { class: 'input', type: 'number', min: '1', max: '8', value: '1', inputmode: 'numeric' });
  const tagsIn = h('input', { class: 'input', placeholder: 'Optional, e.g. docs urgent', autocapitalize: 'off', spellcheck: 'false' });
  const prioIn = h('input', { class: 'input', type: 'number', min: '-10', max: '10', value: '0', inputmode: 'numeric' });
  const submit = h('button', { class: 'btn btn-primary', type: 'submit' }, 'Start');
  // Stops the attachment upload and keeps the form, so a chip added by
  // mistake can be removed and Start pressed again.
  const cancelUp = h('button', { class: 'btn', type: 'button', hidden: true, onclick: () => att.abort() }, 'Cancel upload');
  const dialogSub = { el: null };

  // Escalation needs checks, which only repo sessions (and .cerberus.json
  // folders) have; keep it, but say where the session runs in the subtitle.
  function syncSub() {
    const sel = picker && picker.value();
    const text = wsKind === 'scratch' ? 'Runs in a scratch folder'
      : !sel ? 'Pick a folder' : sel.git ? 'Runs in a detached worktree' : 'Edits the folder in place';
    if (dialogSub.el) dialogSub.el.textContent = text;
  }

  const form = h('form', { class: 'task-form', onsubmit: async (e) => {
    e.preventDefault();
    const sel = picker && picker.value();
    if (!task.value.trim()) { toast('Describe the task', 'warn'); return; }
    if (wsKind === 'folder' && !sel) { toast('Pick a folder, or choose None', 'warn'); return; }
    const tags = parseTags(tagsIn.value);
    if (tags.errors.length) { toast(tags.errors[0], 'warn'); return; }
    submit.disabled = true;
    const spec = {
      Task: task.value.trim(), Title: title.value.trim(), Agent: pick.agent(),
      Model: pick.model(), EscalateTo: pick.escalate(),
      Count: Math.max(1, Math.min(8, parseInt(count.value, 10) || 1)),
      Tags: tags.tags, Priority: clampPriority(prioIn.value),
    };
    if (wsKind === 'scratch') spec.Workspace = 'scratch';
    else spec.Dir = sel.path; // the server makes a git repo a repo session
    try {
      if (att.files().length) {
        submit.textContent = 'Uploading…';
        cancelUp.hidden = false;
        try {
          spec.Staging = await att.stage();
        } finally {
          cancelUp.hidden = true;
        }
        submit.textContent = 'Start';
      }
      const out = await post('/api/sessions', spec);
      att.used();
      toast(out && out.length > 1 ? `${out.length} sessions started` : 'Session started', 'ok');
      close(out || []);
    } catch (err) {
      // Closed meanwhile (Cancel, Esc): the form is gone, so is the news.
      if (form.isConnected) toast(errText(err), 'err');
      submit.textContent = 'Start';
      submit.disabled = false;
    }
  } },
  h('div', { class: 'field' }, h('span', { class: 'field-label', text: 'Workspace' }),
    h('div', { class: 'ws-choice', role: 'group', 'aria-label': 'Workspace' }, noneBtn, folderBtn)),
  wsHint,
  pickerBox,
  textWithMic('Task', task, mic),
  att.el,
  field('Title', title),
  h('div', { class: 'form-row' }, pick.agentField, pick.modelField),
  pick.hint,
  h('div', { class: 'form-row' }, pick.escalateField, field('Parallel copies', count)),
  h('div', { class: 'form-row' }, field('Tags', tagsIn), field('Priority (−10..10)', prioIn)),
  h('div', { class: 'form-actions' },
    h('button', { class: 'btn btn-ghost', type: 'button', onclick: () => close(null) }, 'Cancel'), cancelUp, submit));

  att.pasteInto(form);
  setKind('scratch');
  setTimeout(() => {
    task.focus();
    dialogSub.el = form.closest('dialog')?.querySelector('.modal-sub') || null;
    syncSub();
  }, 0);
  return { title: 'New task', sub: 'Runs in a scratch folder', body: form };
}
