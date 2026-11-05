// Settings → Import & export (#/settings/transfer).
//
//   Export   choose sections (all by default, Models included) and whether
//            to keep secret names, then Download: GET /api/config/export.
//            Secret values never leave.
//   Import   choose a file saved with Export. POST /api/config/import with
//            apply=false shows what it holds and changes; Apply saves it.
//   Models   the bundle's models, each "On this machine" or missing. A
//            missing one can be downloaded (POST /api/downloads, progress
//            from the event stream) or removed: its models[] entry leaves
//            the registry (POST /api/config/models/forget) and the row goes.
//            Nothing is downloaded or removed without a press.
//
// Pure helpers live in ../settingsio.js.

import { api, post } from '../api.js';
import { h, clear, confirmDialog, toast, errText, pill } from '../dom.js';
import { store } from '../store.js';
import { engineLabel, progressText, progressPct } from '../downloads.js';
import {
  SECTIONS, exportURL, readBundle, sectionLabel, changeRows, downloadRequest, modelState, modelCounts, sizeText,
  summaryText, dropModels, markRegistry, refreshModels,
} from '../settingsio.js';

const put = (el, ...kids) => { el.append(...kids.flat().filter((c) => c != null && c !== false)); return el; };

// settingsIOPanel mounts the panel into root. Returns the unmount.
export function settingsIOPanel(root) {
  let gone = false;
  const st = {
    sections: SECTIONS.map(([id]) => id), secretNames: false,
    file: '', bundle: null, reply: null, applied: false, ready: false, busy: false, err: '',
    models: [], dl: new Map(), // model id → download item
  };

  // ---------- Export ----------
  const expLink = h('a', { class: 'btn btn-primary', download: '' }, 'Download settings');
  const expNote = h('p', { class: 'field-hint sio-note' });
  const syncExport = () => {
    expLink.href = exportURL(st.sections, st.secretNames);
    const none = !st.sections.length;
    expLink.classList.toggle('disabled', none);
    expLink.setAttribute('aria-disabled', String(none));
    expNote.textContent = none ? 'Choose at least one section.'
      : `${st.sections.length === SECTIONS.length ? 'Every section' : st.sections.map(sectionLabel).join(', ')}. API keys and tokens are never included${st.secretNames ? '; their names are, so you know which to add' : ''}.`;
  };
  const checks = h('div', { class: 'sio-checks', role: 'group', 'aria-label': 'Sections to export' },
    SECTIONS.map(([id, label, hint]) => h('label', { class: 'sio-check', title: hint },
      h('input', {
        type: 'checkbox', checked: true, value: id,
        onchange: (e) => {
          st.sections = e.target.checked ? [...new Set([...st.sections, id])] : st.sections.filter((s) => s !== id);
          st.sections = SECTIONS.map(([s]) => s).filter((s) => st.sections.includes(s));
          syncExport();
        },
      }),
      h('span', { text: label }))));
  const secretToggle = h('label', { class: 'sio-check sio-secret' },
    h('input', { type: 'checkbox', onchange: (e) => { st.secretNames = e.target.checked; syncExport(); } }),
    h('span', { text: 'Include the names of secrets (never their values)' }));
  expLink.addEventListener('click', (e) => { if (!st.sections.length) e.preventDefault(); });
  syncExport();

  // ---------- Import ----------
  const fileIn = h('input', { type: 'file', accept: '.json,application/json', class: 'sio-file', 'aria-label': 'Settings file', onchange: onFile });
  const pickBtn = h('button', { class: 'btn', type: 'button', onclick: () => fileIn.click() }, 'Choose file…');
  const impEl = h('div', { class: 'sio-imp', 'aria-live': 'polite' });
  const modelsEl = h('section', { class: 'ft-sec sio-models', 'aria-labelledby': 'sio-h-models', hidden: true });

  async function onFile() {
    const f = fileIn.files && fileIn.files[0];
    fileIn.value = '';
    if (!f) return;
    st.file = f.name;
    st.reply = null; st.applied = false; st.ready = false; st.err = ''; st.models = []; st.dl.clear();
    if (f.size > 4 << 20) { st.err = 'This file is too large for a settings file.'; render(); return; }
    const r = readBundle(await f.text());
    if (r.error) { st.err = r.error; st.bundle = null; render(); return; }
    st.bundle = r.bundle;
    await send(false);
  }

  async function send(apply) {
    if (!st.bundle) return;
    st.busy = true; st.err = ''; render();
    try {
      const reply = await post('/api/config/import', { bundle: st.bundle, apply });
      if (gone) return;
      st.reply = reply;
      st.models = reply.models || [];
      // Nothing to save: the model choices are open at once.
      st.ready = apply || !(reply.changes || []).length;
      if (apply) {
        st.applied = true;
        const n = (reply.changed || []).length;
        toast(n ? `Imported: ${n} setting${n === 1 ? '' : 's'} changed` : 'Imported', 'ok');
        if ((reply.changed || []).some((c) => c.restart)) toast('Some changes take effect after a restart (Settings shows how).', 'info');
      }
    } catch (e) {
      st.err = errText(e);
    }
    st.busy = false;
    render();
  }

  // checkAgain asks the engines again for the rows still listed (a removed
  // row stays removed; nothing is saved).
  async function checkAgain() {
    if (!st.bundle) return;
    try {
      const reply = await post('/api/config/import', { bundle: st.bundle, apply: false });
      if (gone) return;
      st.models = refreshModels(st.models, reply.models || []);
      renderModels();
    } catch (e) {
      toast(errText(e), 'err');
    }
  }

  function reset() {
    st.file = ''; st.bundle = null; st.reply = null; st.applied = false; st.ready = false; st.err = ''; st.models = []; st.dl.clear();
    render();
  }

  function renderImport() {
    clear(impEl);
    if (st.err) impEl.append(h('div', { class: 'alert err', role: 'alert', text: st.err }));
    if (!st.bundle) {
      impEl.append(h('p', { class: 'field-hint', text: 'Choose a file saved with Export on this or another machine. You see what it changes before anything is saved.' }));
      return;
    }
    const rep = st.reply;
    const head = h('div', { class: 'sio-file-head' },
      h('span', { class: 'sio-fname mono', text: st.file, title: st.file }),
      st.bundle.created ? h('span', { class: 'sio-when', text: `saved ${new Date(st.bundle.created).toLocaleString()}` }) : null,
      h('button', { class: 'btn btn-sm btn-ghost', type: 'button', onclick: reset }, 'Clear'));
    put(impEl, head);
    if (st.busy && !rep) { impEl.append(h('p', { class: 't-sec', text: 'Reading…' })); return; }
    if (!rep) return;
    impEl.append(h('p', { class: 'sio-sum', text: st.applied ? `Imported. Holds ${(rep.sections || []).map(sectionLabel).join(', ') || 'nothing'}.` : summaryText(rep) }));
    const rows = changeRows(rep.changes);
    if (rows.length && !st.applied) {
      impEl.append(h('ul', { class: 'sio-changes' }, rows.map((r) => h('li', { class: 'sio-change' },
        h('span', { class: 'sio-path mono', text: r.path }),
        h('span', { class: 'sio-vals' },
          h('span', { class: 'sio-from', text: r.from, title: r.from }),
          h('span', { class: 'sio-arrow', 'aria-hidden': 'true', text: '→' }),
          h('span', { class: 'sio-to', text: r.to, title: r.to })),
        r.restart ? pill('restart', 'warn') : null))));
    }
    if ((rep.secret_names || []).length) {
      impEl.append(h('p', { class: 'field-hint' }, 'Secrets it refers to (add them in ',
        h('a', { href: '#/settings/secrets', text: 'Secrets' }), `): ${rep.secret_names.join(', ')}`));
    }
    if (!st.applied) {
      impEl.append(h('div', { class: 'sio-btns' },
        rows.length ? h('button', { class: 'btn btn-primary', type: 'button', disabled: st.busy, onclick: () => send(true) }, st.busy ? 'Saving…' : 'Apply import') : null,
        h('button', { class: 'btn btn-ghost', type: 'button', disabled: st.busy, onclick: reset }, 'Cancel')));
    }
  }

  // ---------- Models ----------
  async function download(m) {
    const req = downloadRequest(m);
    if (!req) return;
    try {
      const it = await post('/api/downloads', req);
      st.dl.set(m.id, it);
      renderModels();
    } catch (e) {
      toast(`${m.id}: ${errText(e)}`, 'err');
    }
  }

  async function remove(list) {
    const reg = list.filter((m) => m.registered).map((m) => m.id);
    const ids = list.map((m) => m.id);
    try {
      if (reg.length) await post('/api/config/models/forget', { ids: reg });
      st.models = dropModels(markRegistry(st.models, reg), ids);
      renderModels();
      toast(reg.length ? `Removed ${reg.length} from the registry` : 'Removed from the list', 'ok');
    } catch (e) {
      toast(errText(e), 'err');
    }
  }

  async function removeAll() {
    const list = st.models.filter((m) => !m.present && !isQueued(m));
    if (!list.length) return;
    const ok = await confirmDialog({
      title: `Remove ${list.length} model${list.length === 1 ? '' : 's'}?`,
      message: 'They leave the registry (their model entries) and this list. No file is deleted, and nothing on this machine changes otherwise.',
      ok: 'Remove', danger: true,
    });
    if (ok) remove(list);
  }

  async function downloadAll() {
    for (const m of st.models.filter((x) => modelState(x) === 'missing' && !isQueued(x))) await download(m);
  }

  const isQueued = (m) => { const it = st.dl.get(m.id); return !!it && it.state !== 'failed' && it.state !== 'canceled'; };

  function renderModels() {
    clear(modelsEl);
    const show = st.ready && st.models.length;
    modelsEl.hidden = !(st.reply && st.models.length);
    if (modelsEl.hidden) return;
    const n = modelCounts(st.models);
    const missing = st.models.filter((m) => modelState(m) === 'missing' && !isQueued(m)).length;
    const removable = st.models.filter((m) => !m.present && !isQueued(m)).length;
    put(modelsEl,
      h('h3', { class: 'group-title', id: 'sio-h-models', text: 'Models in the file' }),
      h('p', { class: 'field-hint', text: `${n.total} model${n.total === 1 ? '' : 's'}: ${n.present} on this machine, ${n.missing + n.orphan} missing.`
        + (show ? ' Download the ones you want; remove the others from the registry.' : ' Apply the import to choose what to do with the missing ones.') }),
      show && (missing || removable) ? h('div', { class: 'sio-btns' },
        missing ? h('button', { class: 'btn btn-primary', type: 'button', onclick: downloadAll }, `Download all missing (${missing})`) : null,
        removable ? h('button', { class: 'btn btn-danger', type: 'button', onclick: removeAll }, `Remove all missing (${removable})`) : null,
        h('button', { class: 'btn btn-ghost', type: 'button', onclick: checkAgain, title: 'Look at the engines again' }, 'Check again')) : null,
      h('ul', { class: 'sio-mlist' }, st.models.map((m) => modelRow(m, show))));
  }

  function modelRow(m, actions) {
    const state = modelState(m);
    const it = st.dl.get(m.id);
    const size = sizeText(m.size_mib);
    const badge = state === 'present' ? pill('on this machine', 'ok')
      : it && it.state === 'done' ? pill('downloaded', 'ok')
        : it && it.state === 'failed' ? pill('failed', 'err')
          : it ? pill(it.state === 'queued' ? 'queued' : 'downloading', 'run')
            : pill(state === 'orphan' ? 'missing · no link' : 'missing', 'warn');
    const row = h('li', { class: `sio-model is-${state}` },
      h('div', { class: 'sio-mhead' },
        h('span', { class: 'sio-mname mono', text: m.id, title: m.id }),
        h('span', { class: 'chip-tag', text: engineLabel(m.backend) }),
        size ? h('span', { class: 'sio-msize', text: size }) : null,
        h('span', { class: 'sio-mbadge' }, badge)),
      m.ref && state !== 'present' ? h('div', { class: 'sio-mref mono', text: m.ref, title: m.ref }) : null);
    if (it && it.state !== 'done') {
      const pct = progressPct(it);
      if (it.state !== 'failed' && it.state !== 'canceled') {
        row.append(h('div', { class: `progress fluid${pct == null ? ' indeterminate' : ''}`, role: 'progressbar', 'aria-label': `${m.id} download`,
          'aria-valuemin': '0', 'aria-valuemax': '100', 'aria-valuenow': pct == null ? null : String(Math.round(pct)) },
        h('span', { style: `width:${pct == null ? 100 : pct.toFixed(1)}%` })));
      }
      row.append(h('div', { class: 'sio-mprog', text: it.state === 'failed' ? (it.error || 'The download failed') : progressText(it) }));
    }
    if (actions && state !== 'present' && !(it && it.state === 'done')) {
      const busy = isQueued(m);
      row.append(h('div', { class: 'sio-mact' },
        state === 'missing' && !busy ? h('button', { class: 'btn btn-sm', type: 'button', onclick: () => download(m) }, it ? 'Retry' : 'Download') : null,
        !busy ? h('button', {
          class: 'btn btn-sm btn-ghost', type: 'button', onclick: () => remove([m]),
          title: m.registered ? 'Remove its model entry from the registry (no file is touched)' : 'It has no model entry here; this only takes it off the list',
        }, m.registered ? 'Remove from registry' : 'Remove') : null));
    }
    return row;
  }

  function render() {
    renderImport();
    renderModels();
  }

  // Download progress for rows started here.
  const unsub = store.subscribe((kind, payload) => {
    if (kind !== 'download' || !payload || !payload.id) return;
    for (const [id, it] of st.dl) {
      if (it.id === payload.id) st.dl.set(id, { ...it, ...payload });
    }
    renderModels();
  });

  put(root, h('div', { class: 'sio' },
    h('section', { class: 'ft-sec', 'aria-labelledby': 'sio-h-exp' },
      h('h3', { class: 'group-title', id: 'sio-h-exp', text: 'Export' }),
      h('p', { class: 'field-hint', text: 'Save these settings to a file to set up another machine, or to keep a copy.' }),
      checks, secretToggle, h('div', { class: 'sio-btns' }, expLink), expNote),
    h('section', { class: 'ft-sec', 'aria-labelledby': 'sio-h-imp' },
      h('h3', { class: 'group-title', id: 'sio-h-imp', text: 'Import' }),
      h('div', { class: 'sio-btns' }, pickBtn, fileIn), impEl),
    modelsEl));
  render();

  return () => { gone = true; unsub(); };
}
