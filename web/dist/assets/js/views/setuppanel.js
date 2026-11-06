// The session page's small "Setup" disclosure (docs/SCOPE.md "E3 Setup
// snapshot"): the hash of the setup the latest run started with, its
// fields when opened, and the fields that differ from another session's
// setup when compared. Fetched when opened, and again when the hash
// changes (a new run), never polled.

import { api, sid } from '../api.js';
import { h, clear, errText } from '../dom.js';
import { shownFields, diffSetups, setupSummary, cleanSessionId } from '../setup.js';

export function setupPanel(id) {
  const summary = h('summary', { text: 'Setup' });
  const body = h('div', { class: 'setup-body' });
  const el = h('details', { class: 'checks setup-panel' }, summary, body);
  let hash = '';
  let view = null;
  let loading = null;
  let other = null; // { id, view } of the compared session
  let otherErr = '';

  el.addEventListener('toggle', () => { if (el.open && !view) load(); });

  async function load() {
    if (loading) return loading;
    loading = api(`${sid(id)}/setup`).then((v) => { view = v; }).catch((e) => { view = { error: errText(e) }; })
      .finally(() => { loading = null; render(); });
    return loading;
  }

  async function compare(text) {
    const oid = cleanSessionId(text);
    otherErr = '';
    if (!oid) { other = null; otherErr = 'Not a session id.'; render(); return; }
    try {
      other = { id: oid, view: await api(`${sid(oid)}/setup`) };
    } catch (e) {
      other = null;
      otherErr = `${oid}: ${errText(e)}`;
    }
    render();
  }

  function render() {
    summary.textContent = view && view.hash ? setupSummary(view) : (hash ? `Setup · ${hash}` : 'Setup');
    clear(body);
    if (!view) { body.append(h('p', { class: 't-sec', text: 'Loading…' })); return; }
    if (view.error) { body.append(h('p', { class: 't-sec', text: view.error })); return; }
    const list = h('dl', { class: 'setup-list' });
    for (const f of shownFields(view.setup)) list.append(h('dt', { text: f.field }), h('dd', { text: f.value }));
    body.append(list);
    const input = h('input', { class: 'input', type: 'text', placeholder: 'Another session id', 'aria-label': 'Compare with session id',
      value: other ? other.id : '' });
    const go = () => compare(input.value);
    input.addEventListener('keydown', (e) => { if (e.key === 'Enter') { e.preventDefault(); go(); } });
    body.append(h('div', { class: 'setup-compare' }, input, h('button', { class: 'btn', type: 'button', onclick: go }, 'Compare')));
    if (otherErr) body.append(h('p', { class: 't-sec', text: otherErr }));
    if (other && other.view) {
      const diff = diffSetups(view.setup, other.view.setup);
      if (!diff.length) {
        body.append(h('p', { class: 't-sec', text: `Same setup as ${other.id} (${other.view.hash}).` }));
      } else {
        const dl = h('dl', { class: 'setup-list setup-diff' });
        for (const d of diff) {
          dl.append(h('dt', { text: d.field }), h('dd', {}, h('span', { text: d.a || '–' }), h('span', { class: 'setup-arrow', text: ' → ' }),
            h('span', { text: d.b || '–' })));
        }
        body.append(h('p', { class: 't-sec', text: `Differs from ${other.id} (${other.view.hash}) in ${diff.length} field${diff.length === 1 ? '' : 's'}:` }), dl);
      }
    }
  }

  // sync is called on every session update: a new hash (a new run) drops
  // the loaded setup, and reloads it when the disclosure is open.
  function sync(info) {
    const next = (info && info.SetupHash) || '';
    if (next === hash) return;
    hash = next;
    view = null;
    render();
    if (el.open) load();
  }

  render();
  return { el, sync, hasSetup: () => !!hash };
}
