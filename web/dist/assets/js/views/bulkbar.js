// Bulk bar: shown at the bottom of Sessions while sessions are selected.
// Broadcast text goes to every selected session as input or as a follow-up
// (quick keys too), and the actions run as one POST /api/sessions/bulk with
// an in-page confirm for destructive ones and a result summary toast.

import { h, openDialog, confirmDialog, toast } from '../dom.js';
import { store } from '../store.js';
import { BULK_MAX, broadcastKey, clampPriority, normTag, sessionName } from '../control.js';
import { runBulk, deleteSessions } from './ctl.js';

const KEYS = ['y', 'n', 'Enter', 'Ctrl-C'];

// bulkBar({ ids(), clear() }) → { el, update(), focus(), destroy() }.
export function bulkBar({ ids, clear }) {
  const count = h('span', { class: 'bulk-count', 'aria-live': 'polite' });
  const text = h('input', {
    class: 'input bulk-text', placeholder: 'Text for all selected', 'aria-label': 'Broadcast text',
    autocomplete: 'off', autocapitalize: 'off', spellcheck: 'false', enterkeyhint: 'send',
  });
  let busy = false;

  const act = (label, fn, cls = '') => h('button', { class: `btn btn-sm ${cls}`.trim(), type: 'button', onclick: () => guard(fn) }, label);
  const wallLink = h('a', { class: 'btn btn-sm', href: '#/wall' }, 'Wall');

  const el = h('div', { class: 'bulk-bar', role: 'region', 'aria-label': 'Bulk actions', hidden: true },
    h('div', { class: 'bulk-row' },
      count,
      h('form', { class: 'bulk-send', onsubmit: (e) => { e.preventDefault(); guard(() => send('input')); } },
        text,
        h('button', { class: 'btn btn-sm btn-primary', type: 'submit', title: 'Type into each live session (Enter is added)' }, 'Input'),
        h('button', { class: 'btn btn-sm', type: 'button', title: 'Start a new attempt with this text', onclick: () => guard(() => send('followup')) }, 'Follow-up')),
      h('button', { class: 'btn btn-sm btn-ghost bulk-clear', type: 'button', onclick: () => clear(), 'aria-label': 'Clear selection' }, 'Clear')),
    h('div', { class: 'bulk-row bulk-actions' },
      KEYS.map((k) => h('button', { class: 'qkey qkey-sm', type: 'button', title: `Send ${k} to all`, onclick: () => guard(() => key(k)) }, k)),
      h('span', { class: 'bulk-sep', 'aria-hidden': 'true' }),
      act('Stop', () => simple('stop', 'Stop sessions', 'Stops the agents of every selected session. Their workspaces are kept.', 'Stop')),
      act('Resume', () => simple('resume')),
      act('Accept…', accept, 'btn-primary'),
      act('Discard', () => simple('discard', 'Discard sessions', 'Stops each selected agent and deletes its worktree or scratch folder. Folder sessions keep their folder. This cannot be undone.', 'Discard'), 'btn-danger'),
      act('Delete', async () => { const gone = await deleteSessions(selected()); if (gone.length) clear(); }, 'btn-danger'),
      act('Run checks', () => simple('checks')),
      act('Priority…', priority),
      act('Tag…', tag),
      act('Pin', () => simple('pin')),
      act('Unpin', () => simple('unpin')),
      wallLink));

  const selected = () => {
    const all = ids();
    if (all.length > BULK_MAX) toast(`Only the first ${BULK_MAX} selected sessions are used`, 'warn');
    return all.slice(0, BULK_MAX);
  };

  async function guard(fn) {
    if (busy) return;
    busy = true;
    el.classList.add('busy');
    try { await fn(); } finally { busy = false; el.classList.remove('busy'); }
  }

  async function send(kind) {
    const v = text.value;
    if (!v.trim()) { toast('Type the text to send first', 'warn'); text.focus(); return; }
    const res = await runBulk(selected(), { Kind: kind, Data: kind === 'input' ? v + '\r' : v.trim() });
    if (res.length) text.value = '';
  }

  async function key(label) {
    const list = selected();
    await runBulk(list, { Kind: 'input', Data: broadcastKey(label, list.map((id) => store.session(id)).filter(Boolean)) });
  }

  async function simple(kind, title, message, ok) {
    const list = selected();
    if (title && !(await confirmDialog({ title: `${title} (${list.length})`, message, ok, danger: true }))) return;
    // Resume uses the broadcast text as its optional instruction.
    const op = { Kind: kind };
    if (kind === 'resume') op.Data = text.value.trim();
    await runBulk(list, op);
  }

  async function accept() {
    const list = selected();
    const res = await openDialog((close) => {
      const msg = h('textarea', { class: 'textarea', rows: '2', placeholder: 'Empty = each session’s title' });
      const push = h('input', { type: 'checkbox' });
      return {
        title: `Accept ${list.length} session${list.length === 1 ? '' : 's'}`,
        sub: 'One after another: squash, fast-forward main',
        body: h('div', { class: 'task-form' },
          h('p', { class: 'dlg-msg', text: 'Repo sessions that passed are merged in order; the rest are skipped. A conflict stops only that session.' }),
          h('label', { class: 'field' }, h('span', { class: 'field-label', text: 'Commit message' }), msg),
          h('label', { class: 'check' }, push, 'Push to origin/main')),
        foot: [
          h('button', { class: 'btn btn-ghost', type: 'button', onclick: () => close(null) }, 'Cancel'),
          h('button', { class: 'btn btn-primary', type: 'button', onclick: () => close({ Message: msg.value.trim(), Push: push.checked }) }, 'Accept all'),
        ],
      };
    });
    if (res) await runBulk(list, { Kind: 'accept', ...res });
  }

  async function priority() {
    const list = selected();
    const v = await openDialog((close) => {
      const n = h('input', { class: 'input', type: 'number', min: '-10', max: '10', value: '0', inputmode: 'numeric' });
      setTimeout(() => n.select(), 0);
      const done = () => close(clampPriority(n.value));
      return {
        title: 'Set priority',
        sub: `${list.length} selected · −10..10, higher starts first`,
        body: h('form', { class: 'task-form', onsubmit: (e) => { e.preventDefault(); done(); } },
          h('label', { class: 'field' }, h('span', { class: 'field-label', text: 'Priority' }), n)),
        foot: [
          h('button', { class: 'btn btn-ghost', type: 'button', onclick: () => close(null) }, 'Cancel'),
          h('button', { class: 'btn btn-primary', type: 'button', onclick: done }, 'Set'),
        ],
      };
    });
    if (v != null) await runBulk(list, { Kind: 'priority', Priority: v });
  }

  async function tag() {
    const list = selected();
    const res = await openDialog((close) => {
      const t = h('input', { class: 'input', placeholder: 'e.g. urgent', autocapitalize: 'off', spellcheck: 'false' });
      const err = h('div', { class: 'alert err', hidden: true });
      setTimeout(() => t.focus(), 0);
      const go = (kind) => {
        const v = normTag(t.value);
        if (!v) { err.textContent = 'A tag is 1 to 24 of a-z, 0-9 and -'; err.hidden = false; return; }
        close({ Kind: kind, Tag: v });
      };
      return {
        title: 'Tag sessions',
        sub: `${list.length} selected`,
        body: h('form', { class: 'task-form', onsubmit: (e) => { e.preventDefault(); go('tag'); } },
          h('label', { class: 'field' }, h('span', { class: 'field-label', text: 'Tag' }), t), err),
        foot: [
          h('button', { class: 'btn btn-ghost', type: 'button', onclick: () => close(null) }, 'Cancel'),
          h('button', { class: 'btn btn-danger', type: 'button', onclick: () => go('untag') }, 'Remove'),
          h('button', { class: 'btn btn-primary', type: 'button', onclick: () => go('tag') }, 'Add'),
        ],
      };
    });
    if (res) await runBulk(list, res);
  }

  function update() {
    const list = ids();
    el.hidden = list.length === 0;
    document.body.classList.toggle('bulk-open', list.length > 0);
    const names = list.slice(0, 3).map((id) => sessionName(store.session(id))).join(', ');
    count.textContent = `${list.length} selected`;
    count.title = names + (list.length > 3 ? ', …' : '');
    wallLink.href = `#/wall/${list.slice(0, 9).map(encodeURIComponent).join(',')}`;
  }

  return {
    el,
    update,
    focus: () => { update(); if (!el.hidden) text.focus(); },
    destroy: () => { document.body.classList.remove('bulk-open'); el.remove(); },
  };
}
