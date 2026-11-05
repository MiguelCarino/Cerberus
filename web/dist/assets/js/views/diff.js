// Diff view: the session's worktree diff, coloured. Tap a line to comment;
// "Send comments" posts them all as one follow-up.

import { api, post, sid } from '../api.js';
import { h, clear, toast, errText } from '../dom.js';
import { parseDiff, lineRef, formatComments, workspaceKind } from '../util.js';
import { store } from '../store.js';

// Draft comments survive leaving and re-opening the diff (per session, this tab).
const drafts = new Map();

export function mount(root, { id }) {
  const comments = drafts.get(id) || [];
  drafts.set(id, comments);
  const filesEl = h('div', { class: 'diff-files' }, h('p', { class: 't-sec', text: 'Loading diff…' }));
  const countEl = h('span', { class: 'group-count' });
  const sendBtn = h('button', { class: 'btn btn-primary', type: 'button', onclick: send }, 'Send comments ', countEl);
  const list = h('ol', { class: 'cmt-list' });
  const bar = h('div', { class: 'diff-bar' }, list, h('div', { class: 'diff-bar-actions' },
    h('button', { class: 'btn btn-ghost', type: 'button', onclick: () => { comments.length = 0; sync(); } }, 'Clear'), sendBtn));

  root.append(h('section', { class: 'view view-diff' },
    h('div', { class: 'ss-head' },
      h('a', { class: 'btn btn-ghost ss-back', href: `#/s/${encodeURIComponent(id)}`, 'aria-label': 'Back to session' }, '←'),
      h('div', { class: 'ss-headtext' }, h('div', { class: 'ss-title', text: 'Diff' }),
        h('div', { class: 'ss-meta', text: 'Tap a line to comment' }))),
    filesEl, bar));

  let editor = null; // the open inline comment box

  function sync() {
    countEl.textContent = String(comments.length);
    bar.hidden = comments.length === 0;
    clear(list).append(...comments.map((c, i) => h('li', {},
      h('span', { class: 'cmt-ref', text: c.ref }), h('span', { class: 'cmt-text', text: c.text }),
      h('button', { class: 'modal-x', type: 'button', 'aria-label': 'Remove comment', onclick: () => { comments.splice(i, 1); sync(); } }, '✕'))));
    for (const el of filesEl.querySelectorAll('.dl.has-cmt')) el.classList.remove('has-cmt');
    for (const c of comments) filesEl.querySelector(`.dl[data-ref="${CSS.escape(c.ref)}"]`)?.classList.add('has-cmt');
  }

  function openEditor(rowEl, ref, code) {
    if (editor) editor.remove();
    const ta = h('textarea', { class: 'textarea', rows: '3', placeholder: `Comment on ${ref}` });
    const done = () => { editor?.remove(); editor = null; };
    editor = h('div', { class: 'cmt-editor' }, ta, h('div', { class: 'form-actions' },
      h('button', { class: 'btn btn-ghost', type: 'button', onclick: done }, 'Cancel'),
      h('button', { class: 'btn btn-primary', type: 'button', onclick: () => {
        if (ta.value.trim()) { comments.push({ ref, code, text: ta.value.trim() }); sync(); }
        done();
      } }, 'Add')));
    rowEl.after(editor);
    ta.focus();
  }

  async function send() {
    const text = formatComments(comments);
    if (!text) return;
    sendBtn.disabled = true;
    try {
      await post(`${sid(id)}/followup`, { text });
      comments.length = 0;
      toast('Comments sent', 'ok');
      location.hash = `#/s/${encodeURIComponent(id)}`;
    } catch (e) {
      toast(errText(e), 'err');
    } finally {
      sendBtn.disabled = false;
    }
  }

  filesEl.addEventListener('click', (e) => {
    const row = e.target.closest('.dl[data-ref]');
    if (row) openEditor(row, row.dataset.ref, row.dataset.code);
  });

  filesEl.addEventListener('keydown', (e) => {
    const row = e.key === 'Enter' && e.target.closest('.dl[data-ref]');
    if (row) { e.preventDefault(); openEditor(row, row.dataset.ref, row.dataset.code); }
  });

  let cancelled = false;
  // Only repo sessions have a git diff (the session view hides its Diff tab
  // otherwise); an old link to a scratch or folder session gets a note, not
  // a failed request.
  const known = store.session(id);
  (known ? Promise.resolve(known) : api(sid(id)).catch(() => null)).then((info) => {
    if (cancelled) return null;
    if (info && workspaceKind(info) !== 'repo') {
      clear(filesEl).append(h('div', { class: 'empty' },
        h('p', { class: 'empty-title', text: 'No diff' }),
        h('p', { class: 't-sec', text: `A ${workspaceKind(info)} session is not tracked by git. Its files are in the session's Files tab.` }),
        h('a', { class: 'btn', href: `#/s/${encodeURIComponent(id)}/files` }, 'Open Files')));
      return null;
    }
    return api(`${sid(id)}/diff`, { text: true }).then((txt) => {
      if (cancelled) return;
      renderFiles(filesEl, parseDiff(txt));
      sync();
    });
  }).catch((e) => { if (!cancelled) clear(filesEl).append(h('div', { class: 'alert err', text: errText(e) })); });

  sync();
  return () => { cancelled = true; };
}

function renderFiles(el, files) {
  clear(el);
  if (!files.length) { el.append(h('div', { class: 'empty' }, h('p', { class: 'empty-title', text: 'No changes' }))); return; }
  el.append(h('p', { class: 'diff-summary', text: summary(files) }));
  const frag = document.createDocumentFragment();
  for (const f of files) {
    const body = h('div', { class: 'diff-body' });
    if (f.binary) body.append(h('div', { class: 'dl meta', text: 'Binary file' }));
    for (const hk of f.hunks) {
      body.append(h('div', { class: 'dl hunk', text: hk.header }));
      for (const ln of hk.lines) body.append(lineRow(f.path, ln));
    }
    frag.append(h('details', { class: 'diff-file', open: true },
      h('summary', { class: 'diff-fhead' },
        h('span', { class: `fstat fs-${f.status}`, text: f.status }),
        h('span', { class: 'fpath', text: f.status === 'renamed' ? `${f.oldPath} → ${f.newPath}` : f.path }),
        h('span', { class: 'fcount' }, h('span', { class: 'add', text: `+${f.adds}` }), ' ', h('span', { class: 'del', text: `−${f.dels}` }))),
      body));
  }
  el.append(frag);
}

function lineRow(path, ln) {
  if (ln.type === 'meta') return h('div', { class: 'dl meta', text: ln.text });
  const sign = ln.type === 'add' ? '+' : ln.type === 'del' ? '-' : ' ';
  return h('div', { class: `dl ${ln.type}`, 'data-ref': lineRef(path, ln), 'data-code': ln.text, role: 'button', tabindex: '0' },
    h('span', { class: 'ln', text: ln.oldNo ?? '' }),
    h('span', { class: 'ln', text: ln.newNo ?? '' }),
    h('span', { class: 'lc', text: sign + ln.text }));
}

function summary(files) {
  let a = 0;
  let d = 0;
  for (const f of files) { a += f.adds; d += f.dels; }
  return `${files.length} file${files.length === 1 ? '' : 's'} changed · +${a} −${d}`;
}
