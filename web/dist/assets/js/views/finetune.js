// Settings → Fine-tuning (#/settings/finetune): review the examples kept
// for training a local model and run the owner's trainer.
//
//   Collection  finetune.capture as a segmented control (saved at once
//               through PATCH /api/config), one sentence on what it keeps,
//               and the counts from GET /api/finetune.
//   Examples    GET /api/finetune/examples, filtered by kind, rating and
//               text, 20 at a time ("Show more" follows the next cursor).
//               Each one opens whole; Good, Bad, Correct… (write the better
//               answer) and Exclude/Include post to the example's routes.
//   Train       the train command and base model (read-only, with a link
//               to the Advanced settings), Download JSONL, and Train now:
//               POST /api/finetune/train starts the trainer as a session
//               and the page opens it. Past runs below, their states kept
//               current by the shared session event stream.
//
// Nothing polls: data loads on mount and when the tab is visible again.
// Pure helpers live in ../finetune.js.

import { api, apiResult, post, AuthError } from '../api.js';
import { h, clear, openDialog, toast, errText, pill } from '../dom.js';
import { relTime } from '../util.js';
import { store } from '../store.js';
import {
  CAPTURE_MODES, captureMode, captureText, countTiles, KINDS, RATED, PAGE_SIZE, examplesURL, exportURL, normPage,
  appendPage, replaceExample, ratingBadge, previews, betterError, trainSummary, sessionID, runBadge, runLine, runHref,
  emptyText, roleLabel, trainRows, trainText, sourceLink,
} from '../finetune.js';

const exURL = (id) => `/api/finetune/examples/${encodeURIComponent(id)}`;
let uid = 0;

// put appends children, skipping null and false (the native append would
// print "null").
const put = (el, ...kids) => { el.append(...kids.flat().filter((c) => c != null && c !== false)); return el; };

// finetunePanel mounts the panel into root; opts.openSetting(path) shows
// one setting in its group (the Advanced link). Returns the unmount.
export function finetunePanel(root, opts = {}) {
  let gone = false;
  const st = {
    capture: 'off', stats: null, cfg: null, runs: [],
    filters: { kind: '', rated: '', q: '' }, list: [], total: 0, next: '', loading: false, listErr: '',
  };

  // ---------- skeleton ----------
  const segEl = h('div', { class: 'ft-seg', role: 'radiogroup', 'aria-label': 'Collect training examples' });
  const modeText = h('p', { class: 'ft-mode-text' });
  const tilesEl = h('div', { class: 'ft-tiles' });
  const kindSel = h('select', { class: 'select ft-filter', 'aria-label': 'Kind', onchange: (e) => { st.filters.kind = e.target.value; reloadList(); } },
    KINDS.map(([v, t]) => h('option', { value: v, text: t })));
  const ratedSel = h('select', { class: 'select ft-filter', 'aria-label': 'Rating', onchange: (e) => { st.filters.rated = e.target.value; reloadList(); } },
    RATED.map(([v, t]) => h('option', { value: v, text: t })));
  let qTimer = 0;
  const qIn = h('input', {
    class: 'input ft-q', type: 'search', placeholder: 'Search prompts and answers', 'aria-label': 'Search examples',
    autocapitalize: 'off', spellcheck: 'false', autocomplete: 'off',
    oninput: (e) => { clearTimeout(qTimer); qTimer = setTimeout(() => { st.filters.q = e.target.value; reloadList(); }, 300); },
  });
  const totalEl = h('p', { class: 'ft-total', role: 'status' });
  const listEl = h('div', { class: 'ft-list' });
  const moreBtn = h('button', { class: 'btn ft-more', type: 'button', hidden: true, onclick: () => loadPage(false) }, `Show ${PAGE_SIZE} more`);
  const trainEl = h('div', { class: 'ft-train' });
  const runsEl = h('div', { class: 'ft-runs' });

  root.append(h('div', { class: 'ft' },
    h('section', { class: 'ft-sec', 'aria-labelledby': 'ft-h-col' },
      h('h3', { class: 'group-title', id: 'ft-h-col', text: 'Collection' }),
      segEl, modeText, tilesEl),
    h('section', { class: 'ft-sec', 'aria-labelledby': 'ft-h-ex' },
      h('h3', { class: 'group-title', id: 'ft-h-ex', text: 'Examples' }),
      h('div', { class: 'ft-filters' }, kindSel, ratedSel, qIn),
      totalEl, listEl, moreBtn),
    h('section', { class: 'ft-sec', 'aria-labelledby': 'ft-h-train' },
      h('h3', { class: 'group-title', id: 'ft-h-train', text: 'Train' }),
      trainEl,
      h('h4', { class: 'ft-subhead', text: 'Past runs' }),
      runsEl)));

  // ---------- data ----------
  // statsSeq drops replies that are not the latest: quick rates and
  // excludes each reload the counts, and an older reply arriving last
  // would leave stale tiles and a wrong Train now state.
  let statsSeq = 0;
  async function loadStats() {
    const seq = ++statsSeq;
    try {
      const [ft, cfg] = await Promise.all([api('/api/finetune'), api('/api/config').catch(() => null)]);
      if (gone || seq !== statsSeq) return;
      st.capture = captureMode(ft && ft.capture);
      st.stats = (ft && ft.stats) || null;
      if (cfg && cfg.config) st.cfg = cfg.config.finetune || {};
      renderCollection();
      renderTrain();
    } catch (e) {
      if (gone || seq !== statsSeq || e instanceof AuthError) return;
      clear(tilesEl).append(h('div', { class: 'alert err', text: errText(e) }));
    }
  }

  async function loadRuns() {
    try {
      const out = await api('/api/finetune/runs');
      if (gone) return;
      st.runs = Array.isArray(out && out.runs) ? out.runs : [];
      renderRuns();
    } catch (e) {
      if (!gone && !(e instanceof AuthError)) clear(runsEl).append(h('p', { class: 't-sec', text: errText(e) }));
    }
  }

  let listSeq = 0;
  async function loadPage(fresh) {
    const seq = ++listSeq;
    st.loading = true;
    moreBtn.disabled = true;
    if (fresh) renderList();
    try {
      const page = normPage(await api(examplesURL(st.filters, fresh ? '' : st.next)));
      if (gone || seq !== listSeq) return;
      st.list = fresh ? page.examples : appendPage(st.list, page);
      st.total = page.total;
      st.next = page.next;
      st.listErr = '';
    } catch (e) {
      if (gone || seq !== listSeq || e instanceof AuthError) return;
      st.listErr = errText(e);
    }
    st.loading = false;
    moreBtn.disabled = false;
    renderList();
  }
  const reloadList = () => { st.next = ''; loadPage(true); };

  // ---------- collection ----------
  let savingMode = false;
  function renderCollection() {
    clear(segEl);
    for (const m of CAPTURE_MODES) {
      const on = m.id === st.capture;
      segEl.append(h('button', {
        class: `toggle-btn${on ? ' active' : ''}`, type: 'button', role: 'radio', 'aria-checked': on ? 'true' : 'false',
        tabindex: on ? '0' : '-1', disabled: savingMode, onclick: () => setMode(m.id), onkeydown: segKeys,
      }, m.label));
    }
    modeText.textContent = captureText(st.capture);
    clear(tilesEl).append(...countTiles(st.stats).map((t) => h('div', { class: `ft-tile ft-tile-${t.key}` },
      h('span', { class: 'stat-lbl', text: t.label }),
      h('span', { class: 'stat-val', text: String(t.value) }))));
  }

  // Arrow keys move between the modes, like a radio group.
  function segKeys(e) {
    const d = e.key === 'ArrowRight' || e.key === 'ArrowDown' ? 1 : e.key === 'ArrowLeft' || e.key === 'ArrowUp' ? -1 : 0;
    if (!d) return;
    e.preventDefault();
    const i = CAPTURE_MODES.findIndex((m) => m.id === st.capture);
    const next = CAPTURE_MODES[(i + d + CAPTURE_MODES.length) % CAPTURE_MODES.length];
    setMode(next.id, true);
  }

  async function setMode(mode, focus = false) {
    if (savingMode || mode === st.capture) return;
    const prev = st.capture;
    savingMode = true;
    st.capture = mode;
    renderCollection();
    try {
      const r = await apiResult('/api/config', { method: 'PATCH', body: { set: { 'finetune.capture': mode } } });
      if (gone) return;
      if (!r.ok) {
        st.capture = prev;
        const probs = (r.body && (r.body.problems || r.body.Problems)) || [];
        toast((probs[0] && (probs[0].message || probs[0].Message)) || r.message || 'Not saved', 'err');
      } else {
        toast(mode === 'off' ? 'Collection is off' : `Collecting: ${CAPTURE_MODES.find((m) => m.id === mode).label.toLowerCase()}`, 'ok');
      }
    } catch (e) {
      st.capture = prev;
      if (!gone && !(e instanceof AuthError)) toast(errText(e), 'err');
    } finally {
      savingMode = false;
      if (!gone) {
        renderCollection();
        if (focus) segEl.querySelector('[aria-checked="true"]')?.focus();
        renderListEmpty();
      }
    }
  }

  // ---------- examples ----------
  const filtered = () => !!(st.filters.kind || st.filters.rated || st.filters.q.trim());

  function renderListEmpty() {
    if (st.list.length || st.loading || st.listErr) return;
    clear(listEl).append(h('div', { class: 'empty ft-empty' },
      h('p', { class: 'empty-title', text: filtered() ? 'No matches' : 'No examples yet' }),
      h('p', { class: 't-sec', text: emptyText(st.capture, filtered(), st.stats ? st.stats.examples : 0) }),
      filtered() ? h('button', { class: 'btn', type: 'button', onclick: clearFilters }, 'Clear filters') : null));
  }

  function clearFilters() {
    st.filters = { kind: '', rated: '', q: '' };
    kindSel.value = '';
    ratedSel.value = '';
    qIn.value = '';
    reloadList();
  }

  function renderList() {
    clear(listEl);
    moreBtn.hidden = !st.next || !!st.listErr;
    if (st.listErr) {
      totalEl.textContent = '';
      listEl.append(h('div', { class: 'alert err', text: st.listErr }));
      return;
    }
    if (st.loading && !st.list.length) {
      totalEl.textContent = '';
      listEl.append(h('p', { class: 't-sec', text: 'Loading…' }));
      return;
    }
    totalEl.textContent = st.total
      ? `${st.total} example${st.total === 1 ? '' : 's'}${filtered() ? ' match' : ''} · showing ${st.list.length}, newest first`
      : '';
    if (!st.list.length) { renderListEmpty(); return; }
    listEl.append(...st.list.map(card));
  }

  function card(ex) {
    const b = ratingBadge(ex);
    const pv = previews(ex);
    const head = h('div', { class: 'ft-ex-head' },
      pill(ex.kind === 'cloud' ? 'cloud' : 'delegated', 'muted'),
      h('span', { class: 'ft-ex-model mono', text: ex.model || '–', title: ex.model || '' }),
      h('span', { class: 'ft-ex-time', text: relTime(ex.time || ''), title: ex.time || '' }),
      h('span', { class: `pill ${b.tone} ft-ex-rating`, text: b.text, title: b.title }),
      ex.excluded ? pill('excluded', 'warn') : null);
    return h('article', { class: `ft-ex${ex.excluded ? ' excluded' : ''}`, 'data-id': ex.id },
      head,
      h('button', { class: 'ft-ex-open', type: 'button', 'aria-label': `Open example: ${pv.prompt}`, onclick: () => openExample(ex) },
        h('span', { class: 'ft-ex-prompt', text: pv.prompt }),
        h('span', { class: `ft-ex-answer${pv.corrected ? ' corrected' : ''}` },
          h('span', { class: 'ft-ex-arrow', 'aria-hidden': 'true', text: pv.corrected ? '✎ ' : '↳ ' }),
          pv.answer)),
      actions(ex));
  }

  // actions: Good, Bad, Correct…, Exclude/Include for one example.
  // inDialog swaps the open dialog's content after a change.
  function actions(ex, after) {
    const state = ratingBadge(ex).state;
    const btn = (label, cls, title, fn, pressed) => h('button', {
      class: `btn btn-sm ${cls}`, type: 'button', title, 'aria-pressed': pressed == null ? null : String(pressed),
      onclick: async (e) => { const el = e.currentTarget; el.disabled = true; try { await fn(); } finally { el.disabled = false; } },
    }, label);
    return h('div', { class: 'ft-ex-actions' },
      btn('Good', 'ft-good', 'The answer was right', () => rate(ex, { good: true }, after), state === 'good'),
      btn('Bad', 'ft-bad', 'The answer was wrong (not used for training)', () => rate(ex, { good: false }, after), state === 'bad'),
      btn('Correct…', 'ft-correct', 'Write the answer it should have given', () => correct(ex, after), state === 'corrected'),
      btn(ex.excluded ? 'Include' : 'Exclude', 'btn-ghost ft-excl', ex.excluded ? 'Use this example again' : 'Never export or train on this example',
        () => exclude(ex, after)));
  }

  function applied(next, after) {
    st.list = replaceExample(st.list, next);
    renderList();
    loadStats();
    if (after) after(next);
  }

  async function rate(ex, body, after) {
    try {
      const next = await post(`${exURL(ex.id)}/rate`, body);
      if (gone) return;
      applied(next, after);
      toast(body.better ? 'Correction saved' : body.good ? 'Marked good' : 'Marked bad', 'ok');
    } catch (e) {
      if (!gone && !(e instanceof AuthError)) toast(errText(e), 'err');
    }
  }

  async function exclude(ex, after) {
    try {
      const next = await post(`${exURL(ex.id)}/exclude`, { exclude: !ex.excluded });
      if (gone) return;
      applied(next, after);
      toast(next.excluded ? 'Excluded from training' : 'Included again', 'ok');
    } catch (e) {
      if (!gone && !(e instanceof AuthError)) toast(errText(e), 'err');
    }
  }

  // full fetches the whole example when the list copy was clipped.
  const full = async (ex) => (ex.truncated ? api(exURL(ex.id)) : ex);

  async function correct(ex, after) {
    let whole;
    try { whole = await full(ex); } catch (e) { if (!(e instanceof AuthError)) toast(errText(e), 'err'); return; }
    if (gone) return;
    const id = `ft-better-${++uid}`;
    const prompt = previews(whole, 400).prompt;
    const area = h('textarea', { id, class: 'textarea ft-better', rows: '8', spellcheck: 'true', value: (whole.rating && whole.rating.better) || whole.answer || '' });
    const note = h('input', { class: 'input', type: 'text', maxlength: '500', placeholder: 'Why (optional)', 'aria-label': 'Note', value: (whole.rating && whole.rating.note) || '' });
    const err = h('p', { class: 'set-err', role: 'alert' });
    const res = await openDialog((close) => {
      const send = () => {
        const msg = betterError(area.value);
        err.textContent = msg;
        if (!msg) close({ better: area.value, note: note.value.trim() });
      };
      area.addEventListener('keydown', (e) => { if (e.key === 'Enter' && (e.ctrlKey || e.metaKey)) { e.preventDefault(); send(); } });
      return {
        title: 'Correct the answer',
        sub: whole.model ? `${whole.model} · trains on your answer instead` : 'Training uses your answer instead',
        wide: true,
        body: h('div', { class: 'ft-correct-form' },
          h('p', { class: 'ft-correct-prompt' }, h('b', { text: 'Prompt: ' }), prompt),
          h('label', { class: 'set-sublabel', for: id, text: 'The answer it should have given' }),
          area, note, err),
        foot: [
          h('button', { class: 'btn btn-ghost', type: 'button', onclick: () => close(null) }, 'Cancel'),
          h('button', { class: 'btn btn-primary', type: 'button', onclick: send }, 'Save correction'),
        ],
      };
    });
    if (!res || gone) return;
    await rate(whole, { good: false, better: res.better, note: res.note || undefined }, after);
  }

  async function openExample(ex) {
    let whole;
    try { whole = await api(exURL(ex.id)); } catch (e) { if (!(e instanceof AuthError)) toast(errText(e), 'err'); return; }
    if (gone) return;
    const body = h('div', { class: 'ft-read' });
    const foot = h('div', { class: 'ft-read-foot' });
    const paint = (x) => {
      const b = ratingBadge(x);
      const src = sourceLink(x.session); // a chat (#/chat/…) or a session (#/s/…)
      put(clear(body),
        h('div', { class: 'ft-ex-head' },
          pill(x.kind === 'cloud' ? 'cloud' : 'delegated', 'muted'),
          h('span', { class: 'ft-ex-model mono', text: x.model || '–' }),
          h('span', { class: 'ft-ex-time', text: x.time ? new Date(x.time).toLocaleString() : '' }),
          h('span', { class: `pill ${b.tone}`, text: b.text, title: b.title }),
          x.excluded ? pill('excluded', 'warn') : null,
          src ? h('a', { class: 'ft-ex-sess', href: src.href }, src.label) : null),
        ...(x.messages || []).map((m) => h('div', { class: `ft-msg ft-msg-${m.role || 'x'}` },
          h('div', { class: 'set-sublabel', text: roleLabel(m.role) }),
          h('pre', { class: 'ft-pre', text: m.content || '' }))),
        h('div', { class: 'ft-msg ft-msg-answer' },
          h('div', { class: 'set-sublabel', text: x.rating && x.rating.better ? 'Model’s answer (replaced)' : 'Model’s answer' }),
          h('pre', { class: 'ft-pre', text: x.answer || '' })),
        x.rating && x.rating.better ? h('div', { class: 'ft-msg ft-msg-better' },
          h('div', { class: 'set-sublabel', text: `Corrected answer${x.rating.by === 'owner' ? ' (yours)' : ' (the agent’s)'}` }),
          h('pre', { class: 'ft-pre', text: x.rating.better })) : null,
        x.rating && x.rating.note ? h('p', { class: 'ft-note' }, h('b', { text: 'Note: ' }), x.rating.note) : null);
      clear(foot).append(actions(x, paint));
    };
    paint(whole);
    await openDialog(() => ({ title: 'Example', sub: previews(whole, 80).prompt, wide: true, body, foot }));
  }

  // ---------- train ----------
  function renderTrain() {
    const t = trainSummary(st.cfg, st.stats);
    const advanced = h('a', {
      class: 'ft-link', href: '#/settings/advanced',
      onclick: (e) => { if (opts.openSetting) { e.preventDefault(); opts.openSetting('finetune.train_command'); } },
    }, 'Change in Advanced settings');
    const trainBtn = h('button', { class: 'btn btn-primary', type: 'button', disabled: !t.ready, onclick: () => train(t) }, 'Train now');
    // With nothing rated yet, the download offers the unrated delegated
    // answers (the same rows Train now's unrated choice exports).
    const dl = t.usable
      ? h('a', { class: 'btn', href: exportURL(), download: '' }, `Download JSONL (${t.usable})`)
      : t.unrated
        ? h('a', { class: 'btn', href: exportURL({ unrated: true }), download: '', title: 'Delegated answers nobody rated' }, `Download unrated JSONL (${t.unrated})`)
        : h('button', { class: 'btn', type: 'button', disabled: true, title: 'No examples are ready yet' }, 'Download JSONL');
    const ready = `${t.usable} example${t.usable === 1 ? '' : 's'}${t.unrated ? `, ${t.unrated} more not rated` : ''}`;
    put(clear(trainEl),
      h('dl', { class: 'ft-facts' },
        h('dt', { text: 'Trainer' }), h('dd', { class: 'mono', text: t.command || 'not set' }),
        h('dt', { text: 'Base model' }), h('dd', { class: 'mono', text: t.base || 'not set' }),
        h('dt', { text: 'Ready' }), h('dd', { text: ready })),
      h('p', { class: 'ft-adv' }, advanced),
      t.blocked ? h('p', { class: 'field-hint ft-blocked', text: t.blocked }) : null,
      !t.blocked && t.onlyUnrated ? h('p', { class: 'field-hint ft-blocked', text: 'Nothing is rated yet. Train now can use the delegated answers nobody rated.' }) : null,
      h('div', { class: 'ft-train-btns' }, dl, trainBtn));
  }

  async function train(t) {
    // Ticked from the start when only unrated answers exist; the sentence
    // and Start follow the box.
    const unrated = h('input', { type: 'checkbox', checked: t.onlyUnrated, disabled: !t.unrated });
    const msg = h('p', { class: 'dlg-msg' });
    const start = h('button', { class: 'btn btn-primary', type: 'button' }, 'Start');
    const sync = () => {
      msg.textContent = trainText(t, unrated.checked);
      start.disabled = !trainRows(t, unrated.checked);
    };
    unrated.addEventListener('change', sync);
    sync();
    const n = t.unrated;
    const go = await openDialog((close) => {
      start.onclick = () => close(true);
      return {
        title: 'Train now?',
        sub: t.base ? `From ${t.base}` : 'With the trainer’s default base model',
        body: h('div', { class: 'ft-train-form' }, msg,
          h('label', { class: 'ft-check' }, unrated,
            h('span', { text: n ? `Also use delegated answers nobody rated (${n})` : 'Also use delegated answers nobody rated (none yet)' }))),
        foot: [
          h('button', { class: 'btn btn-ghost', type: 'button', onclick: () => close(false) }, 'Cancel'),
          start,
        ],
      };
    });
    if (!go || gone) return;
    try {
      const info = await post('/api/finetune/train', unrated.checked ? { unrated: true } : {});
      const id = sessionID(info);
      if (info && id) store.upsert(info);
      toast('Training started', 'ok');
      if (id) location.hash = `#/s/${encodeURIComponent(id)}`;
      else loadRuns();
    } catch (e) {
      if (!gone && !(e instanceof AuthError)) toast(errText(e), 'err');
    }
  }

  function renderRuns() {
    clear(runsEl);
    if (!st.runs.length) {
      runsEl.append(h('p', { class: 't-sec ft-runs-empty', text: 'No runs yet. Train now starts one; each keeps its dataset and the trainer’s output in its own folder.' }));
      return;
    }
    runsEl.append(...st.runs.map((r) => {
      const b = runBadge(r);
      const href = runHref(r);
      const title = h('span', { class: 'ft-run-time', text: r.time ? new Date(r.time).toLocaleString() : r.id, title: r.id });
      return h('div', { class: 'ft-run' },
        h('div', { class: 'ft-run-main' },
          href ? h('a', { class: 'ft-run-link', href }, title) : title,
          h('span', { class: 'ft-run-sub', text: runLine(r) }),
          h('span', { class: 'ft-run-dir mono', text: r.out || r.dir || '', title: r.out || r.dir || '' })),
        h('span', { class: `pill ${b.tone}`, text: b.text }));
    }));
  }

  // A run's session changed state: update its pill without a fetch.
  const unsub = store.subscribe((kind, info) => {
    if (kind !== 'session' || !info) return;
    const id = sessionID(info);
    let hit = false;
    st.runs = st.runs.map((r) => (r.session === id && r.state !== info.State ? (hit = true, { ...r, state: info.State }) : r));
    if (hit && !gone) renderRuns();
  });

  // ---------- lifecycle ----------
  const onVisible = () => { loadStats(); loadRuns(); };
  window.addEventListener('cerberus:visible', onVisible);
  renderCollection();
  renderTrain();
  loadStats();
  loadRuns();
  loadPage(true);

  return () => {
    gone = true;
    clearTimeout(qTimer);
    unsub();
    window.removeEventListener('cerberus:visible', onVisible);
  };
}
