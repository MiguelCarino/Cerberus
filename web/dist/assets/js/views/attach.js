// New task attachments: drop (via the overlay), pick or paste files; chips
// show name and size with a remove button. Files over the server's per-file
// limit are refused when added. The files stay in the browser until Start,
// then go to /api/uploads one request per file (the first makes the
// staging folder, the rest add to it with ?staging=ID), so each chip has its
// own progress bar and its own error; the staging id rides in the create
// body (TaskSpec.Staging). Pressing Start again (say, after a create error
// or after removing a chip that failed) reuses what is already staged and
// sends only the files that are not.

import { api } from '../api.js';
import { h, clear, toast } from '../dom.js';
import { fmtBytes, addFiles, fileKey, tooLargeFor } from '../files.js';
import { filesConfig, uploadToStaging } from '../upload.js';
import { pickerButton, onPasteFiles } from './drop.js';

// attachmentsField returns { el, add(files), files(), stage(), busy(),
// abort(), used(), discard(), pasteInto(el) }.
//   stage() → Promise<string>: the staging id ("" with no files); throws
//   with a message on failure or cancel (the failed chip says why).
//   abort() cancels a running stage(); the form stays as it was.
//   discard() deletes a staging set that was never used (dialog closed).
export function attachmentsField() {
  let files = [];
  let stagingId = ''; // the staging folder holding the files in staged
  const staged = new Set(); // fileKey of files already in stagingId
  const failed = new Map(); // fileKey → error of the last try
  let current = null; // in-flight upload
  let cancelled = false;
  const chips = h('div', { class: 'att-chips' });
  const empty = h('span', { class: 'att-empty', text: 'Drop, paste or pick files. They land in the session’s inbox.' });
  const picker = pickerButton((list) => add(list), { label: 'Add files', cls: 'btn btn-sm' });
  const el = h('div', { class: 'field att-field' },
    h('div', { class: 'field-top' }, h('span', { class: 'field-label', text: 'Attachments' }), picker),
    h('div', { class: 'att-zone' }, empty, chips));

  const bars = new Map();
  function render() {
    bars.clear();
    empty.hidden = files.length > 0;
    clear(chips).append(...files.map((f) => {
      const k = fileKey(f);
      const bar = h('span', {});
      if (staged.has(k)) bar.style.width = '100%';
      bars.set(k, bar);
      const err = failed.get(k);
      return h('span', { class: `att-chip${err ? ' att-failed' : ''}`, title: err ? `${f.name}: ${err}` : f.name },
        h('span', { class: 'att-name', text: f.name }),
        h('span', { class: 'att-size', text: err || fmtBytes(f.size) }),
        h('button', { class: 'att-x', type: 'button', 'aria-label': `Remove ${f.name}`, disabled: !!current,
          onclick: () => { files = files.filter((x) => x !== f); failed.delete(k); render(); } }, '✕'),
        h('span', { class: 'att-bar' }, bar));
    }));
  }

  function add(list) {
    if (current) { toast('Wait for the upload to finish', 'warn'); return; }
    const { maxFileMiB } = filesConfig();
    const fit = [];
    for (const f of list || []) {
      const why = tooLargeFor(f.size, maxFileMiB);
      if (why) toast(`${f.name}: ${why}`, 'warn');
      else fit.push(f);
    }
    if (!fit.length) return;
    const before = files.length;
    files = addFiles(files, fit);
    if (files.length === before) toast('Already attached', 'info');
    render();
  }

  async function stage() {
    if (!files.length) return '';
    const keys = new Set(files.map(fileKey));
    // A staged file that was removed cannot be taken out of the folder on
    // its own: start a fresh one.
    if (stagingId && [...staged].some((k) => !keys.has(k))) {
      discardId(stagingId);
      stagingId = '';
      staged.clear();
    }
    cancelled = false;
    for (const f of files) {
      const k = fileKey(f);
      if (staged.has(k)) continue;
      failed.delete(k);
      const up = uploadToStaging([f], stagingId, {
        onProgress: (loaded, total) => {
          const b = bars.get(k);
          if (b) b.style.width = `${total ? Math.round((loaded / total) * 100) : 0}%`;
        },
      });
      current = up;
      render();
      const res = await up.promise;
      current = null;
      if (!res.ok) {
        if (!res.cancelled) failed.set(k, res.error || 'failed');
        render();
        if (res.cancelled || cancelled) throw new Error('Upload cancelled');
        throw new Error(`${f.name}: ${res.error}. Remove it or press Start to try again.`);
      }
      stagingId = res.staging;
      staged.add(k);
      render();
    }
    return stagingId;
  }

  function discardId(id) {
    api(`/api/uploads/${encodeURIComponent(id)}`, { method: 'DELETE' }).catch(() => {});
  }

  render();
  return {
    el,
    add,
    files: () => files.slice(),
    stage,
    busy: () => !!current,
    abort: () => { cancelled = true; if (current) current.abort(); },
    // used() marks the staged set as consumed by a create.
    used: () => { stagingId = ''; staged.clear(); },
    discard: () => {
      cancelled = true;
      if (current) current.abort();
      if (stagingId) { discardId(stagingId); stagingId = ''; staged.clear(); }
    },
    pasteInto: (target) => onPasteFiles(target, add),
  };
}
