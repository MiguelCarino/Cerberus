// "Get models" sheet in Models (day phase 6): download local models without
// the CLI. One box takes an LM Studio model name (`lms get` picks the best match), an Ollama name
// ("qwen2.5-coder:7b", or "ollama <name>") or a Hugging Face .gguf link
// (into the llama.cpp models folder). The queue below shows each download's
// progress (bytes, rate, ETA) from the event stream, with Cancel. A
// download never loads the model.

import { api, post } from '../api.js';
import { h, clear, openDialog, toast, errText } from '../dom.js';
import { store } from '../store.js';
import { snapshotLoader } from '../util.js';
import {
  classifyRef, normDownloads, mergeDownload, progressPct, progressText, isFinal, engineLabel,
} from '../downloads.js';

const HINT = {
  empty: 'Type an LM Studio model name, an Ollama name such as llama3.2:3b, or paste a Hugging Face .gguf link.',
  lmstudio: 'LM Studio: lms get finds this name in its catalogue and downloads the best match. An owner/name it does not know is tried on Hugging Face.',
  hfrepo: 'Hugging Face repo → LM Studio downloads the build (GGUF or MLX) that fits this machine.',
  auto: 'LM Studio finds this name when it is installed, else Ollama pulls it. Type "ollama " or "lms " first to choose.',
  ollama: 'Ollama: pulls this model by name.',
  hf: 'Hugging Face file → the llama.cpp models folder (resumable, SHA-256 checked when published).',
};

// openGetModels shows the sheet; it resolves when closed.
export function openGetModels() {
  let items = [];
  let engines = {};
  let gone = false;

  const input = h('input', {
    class: 'input', type: 'text', placeholder: 'Model name, name:tag or https://huggingface.co/…/model.gguf',
    autocapitalize: 'off', autocomplete: 'off', spellcheck: 'false', 'aria-label': 'Model',
  });
  const hint = h('p', { class: 'gm-hint', text: HINT.empty });
  const err = h('div', { class: 'alert err', hidden: true });
  const getBtn = h('button', { class: 'btn btn-primary', type: 'submit' }, 'Get');
  const enginesEl = h('p', { class: 'gm-engines' });
  const queue = h('div', { class: 'gm-queue', 'aria-live': 'polite' });

  // failNote is the last failure of a download started here, shown under
  // the box until the text changes (then the box's own check shows).
  let failNote = '';
  const update = () => {
    const c = classifyRef(input.value);
    hint.textContent = HINT[c.kind] || HINT.empty;
    const msg = c.error || failNote;
    err.hidden = !msg;
    err.textContent = msg;
    getBtn.disabled = c.kind === 'empty' || !!c.error;
  };
  input.addEventListener('input', () => { failNote = ''; update(); });

  // Downloads started from this sheet: when one fails after it was queued,
  // its link comes back into the box (if the box is empty) with the reason,
  // so it can be fixed and sent again without reopening the sheet.
  const mine = new Set();
  const failedSeen = new Set();
  function noteFailure(it) {
    if (!it || !it.id || !mine.has(it.id) || it.state !== 'failed' || failedSeen.has(it.id)) return;
    failedSeen.add(it.id);
    if (input.value.trim()) return;
    putBack(it, `${it.name || it.ref} failed: ${it.error || 'the download did not finish'}. ${(it.suggest || []).length ? 'Try a build below, or fix it and press Get.' : 'Fix it and press Get.'}`);
  }
  // putBack fills the box with a download's reference and focuses it.
  function putBack(it, note = '') {
    input.value = refText(it);
    failNote = note;
    update();
    input.focus();
    input.select();
  }
  const refText = (it) => (it.engine === 'ollama' ? `ollama ${it.ref}` : it.engine === 'lmstudio' ? `lms ${it.ref}` : it.ref);

  async function add(req) {
    getBtn.disabled = true;
    failNote = '';
    try {
      const it = await post('/api/downloads', req);
      input.value = '';
      if (it && it.id) {
        mine.add(it.id);
        // The reply is older than a final state the stream already brought.
        const prev = items.find((x) => x.id === it.id);
        if (!(prev && isFinal(prev))) items = mergeDownload(items, it);
        renderQueue();
        // A quick failure can arrive on the event stream before this reply:
        // look at the merged item, not only the reply.
        noteFailure(items.find((x) => x.id === it.id) || it);
      }
      toast(`Queued ${(it && it.name) || req.ref}`, 'ok');
    } catch (e) {
      // Refused outright: the text stays in the box to fix.
      failNote = errText(e);
    }
    update();
    input.focus(); // keep typing: the next model, or a fix
  }

  // retry sends a failed download again, as it was.
  function retry(it) {
    add({ engine: it.engine, ref: it.ref });
  }

  function submit(e) {
    e.preventDefault();
    const c = classifyRef(input.value);
    if (c.kind === 'empty' || c.error) return;
    add({ engine: c.engine, ref: c.ref });
  }

  // cancel stops a queued or running download; for a finished one the same
  // call forgets it.
  async function cancel(it) {
    try {
      await api(`/api/downloads/${encodeURIComponent(it.id)}`, { method: 'DELETE' });
      if (isFinal(it)) { items = items.filter((x) => x.id !== it.id); renderQueue(); }
    } catch (e) {
      toast(errText(e), 'err');
    }
  }

  function renderEngines() {
    const names = Object.keys(engines);
    enginesEl.hidden = !names.length;
    enginesEl.textContent = names.length
      ? `Can download with: ${names.filter((n) => engines[n]).map(engineLabel).join(', ') || 'none (no engine is set up)'}`
      : '';
  }

  function renderQueue() {
    clear(queue);
    if (!items.length) { queue.append(h('p', { class: 't-sec', text: 'No downloads yet.' })); return; }
    queue.append(...items.map((it) => {
      const pct = progressPct(it);
      const tone = it.state === 'failed' ? 'err' : it.state === 'done' ? 'ok' : it.state === 'canceled' ? 'muted' : 'run';
      return h('div', { class: `gm-item tone-${tone}` },
        h('div', { class: 'gm-item-head' },
          h('span', { class: 'gm-name', text: it.name || it.ref }),
          h('span', { class: 'chip-tag', text: engineLabel(it.engine) }),
          it.state === 'failed' || it.state === 'canceled'
            ? h('button', { class: 'btn btn-sm btn-ghost', type: 'button', title: 'Put it in the box to change it', onclick: () => putBack(it) }, 'Edit') : null,
          it.state === 'failed' || it.state === 'canceled'
            ? h('button', { class: 'btn btn-sm', type: 'button', onclick: () => retry(it) }, 'Retry') : null,
          h('button', { class: 'btn btn-sm btn-ghost', type: 'button', onclick: () => cancel(it) }, isFinal(it) ? 'Remove' : 'Cancel')),
        isFinal(it) && it.state !== 'done' ? null : h('div', {
          class: `progress fluid${pct == null ? ' indeterminate' : ''}`, role: 'progressbar', 'aria-label': `${it.name || it.ref} progress`,
          'aria-valuemin': '0', 'aria-valuemax': '100', 'aria-valuenow': pct == null ? null : String(Math.round(pct)),
        }, h('span', { style: `width:${pct == null ? 100 : pct.toFixed(1)}%` })),
        h('div', { class: 'gm-progress', text: progressText(it) }),
        it.state === 'failed' && (it.suggest || []).length ? h('div', { class: 'gm-suggest' },
          h('span', { class: 'gm-suggest-label', text: 'Try:' }),
          it.suggest.map((ref) => h('button', {
            class: 'btn btn-sm gm-suggest-btn', type: 'button', title: `Put ${ref} in the box`,
            onclick: () => { input.value = ref; failNote = ''; update(); input.focus(); },
          }, ref.replace(/^lms https:\/\/huggingface\.co\//, '')))) : null,
        it.path && it.state === 'done' ? h('div', { class: 'gm-path mono', text: it.path }) : null);
    }));
  }

  // The queue snapshot; a download event that arrives while it is in
  // flight (a final "done" above all) is re-applied on top, so a stale
  // "running" in the reply cannot stick.
  const loader = snapshotLoader({
    fetch: () => api('/api/downloads'),
    onSnapshot(reply, events) {
      if (gone) return;
      const n = normDownloads(reply);
      items = events.reduce(mergeDownload, n.items);
      engines = n.engines;
      renderEngines();
      renderQueue();
    },
    onError(e) { if (!gone) clear(queue).append(h('div', { class: 'alert err', text: errText(e) })); },
  });
  const load = () => loader.load();

  const unsub = store.subscribe((kind, payload) => {
    if (kind === 'download') { loader.track(payload); items = mergeDownload(items, payload); renderQueue(); noteFailure(payload); }
    else if (kind === 'events-open' || (kind === 'resync' && payload && payload.what === 'downloads')) load();
  });
  queue.append(h('p', { class: 't-sec', text: 'Loading…' }));
  load();
  update();
  setTimeout(() => input.focus(), 0);

  return openDialog((close) => ({
    title: 'Get models',
    sub: 'Downloads only: nothing is loaded',
    body: h('div', { class: 'gm' },
      h('form', { class: 'gm-form', onsubmit: submit }, input, h('div', { class: 'gm-btns' }, getBtn)),
      hint, err,
      h('h3', { class: 'group-title gm-qtitle', text: 'Queue' }), enginesEl, queue),
    foot: [h('button', { class: 'btn btn-ghost', type: 'button', onclick: () => close() }, 'Close')],
  })).finally(() => { gone = true; unsub(); });
}
