// Session view: the live terminal (xterm.js over SSE), the input bar with
// quick keys and the mic (shared with the wall, see terminput.js), the
// session actions (checks, stop, kill, diff, preview, accept, resume,
// discard), the limits editor, spawn approval and the parent/child links.

import { Terminal } from '../../vendor/xterm/xterm.mjs';
import { FitAddon } from '../../vendor/addon-fit/addon-fit.mjs';
import { api, post, sid, LiveSource } from '../api.js';
import { h, clear, openDialog, confirmDialog, toast, errText, pill } from '../dom.js';
import { store } from '../store.js';
import {
  b64ToBytes, debounce, stateTone, isLive, canFollowUp, takesFollowUp, canStop, canDiscard, canResume,
  workspaceKind, workspaceLabel, canDiff, canAcceptSession, canSave, joinPath, fmtEnergy,
} from '../util.js';
import { sessionName, needsApproval, validateLimits, limitsText, limitsHelp } from '../control.js';
import { fmtMoney } from '../monitor.js';
import { folderPicker } from './picker.js';
import { micControl } from './mic.js';
import { textWithMic } from './form.js';
import { TERM_THEME, TERM_FONT, termInput } from './terminput.js';
import { patchSession, approveSession, killSession, deleteSessions } from './ctl.js';
import { filesPanel } from './files.js';
import { pushDropTarget } from './drop.js';
import { uploadToSession } from '../upload.js';
import { insertedPaths } from '../files.js';
import { replayPanel } from './replay.js';
import { delegationText } from '../delegation.js';
import { flowPanel } from './flow.js';
import { previewPanel } from './preview.js';
import { previewChip } from '../preview.js';
import { copyLater } from './chatdom.js';
import { canWriteSessions, ownerLabel } from '../users.js';
import { mount as mountNotFound } from './notfound.js';

// mount shows the session. One the store does not know yet is fetched
// first: a session that does not exist, or that this account may not see
// (the server answers 404 either way), gets the Not found view instead of
// a terminal, stream and composer that can never work.
export function mount(root, args) {
  if (store.session(args.id)) return mountSession(root, args);
  let gone = false;
  let inner = null;
  const wait = h('section', { class: 'view view-pad' }, h('p', { class: 't-sec', text: 'Loading…' }));
  root.append(wait);
  api(sid(args.id)).then((s) => {
    if (gone) return;
    store.upsert(s);
    wait.remove();
    inner = mountSession(root, args);
  }).catch((e) => {
    if (gone) return;
    wait.remove();
    if (e && e.status === 404) {
      inner = mountNotFound(root, { path: `#/s/${args.id}` });
      return;
    }
    inner = mountSession(root, args);
  });
  return () => { gone = true; if (inner) inner(); };
}

function mountSession(root, { id, tab = 'terminal', attempt = 0, t = 0 }) {
  let info = store.session(id) || null;
  // Users and roles: a viewer watches this session read only (no input,
  // no actions, no uploads). The server refuses writes either way.
  const me = store.me();
  const readOnly = !canWriteSessions(me);
  const roBar = h('div', { class: 'ss-readonly', role: 'note' }, h('span', { class: 'ro-label', text: 'Read only' }),
    h('span', { class: 't-sec', text: 'You can watch this session, not type into it.' }));
  const titleEl = h('div', { class: 'ss-title' });
  const metaEl = h('div', { class: 'ss-meta' });
  // Energy and cost get their own line: the meta line is cut with an
  // ellipsis, which on a phone would cut the cost (or all of it) off.
  const energyEl = h('div', { class: 'ss-energy-line', hidden: true });
  const stateEl = h('span', {});
  const actions = h('div', { class: 'ss-actions' });
  const notice = h('div', { class: 'ss-notice' });
  const termBox = h('div', { class: 'term-box' });
  let term = null;
  const ti = termInput(id, { getInfo: () => info, getTerm: () => term, onFiles: inputFiles });

  // ---------- Terminal / Files / Replay / Flow tabs (Diff is its own page) ----------
  let files = null; // the Files panel, built on first open
  let replay = null; // the Replay panel, built on first open
  let flow = null; // the Flow panel, built on first open
  let preview = null; // the Preview panel, built on first open
  let view = 'terminal';
  const termTab = h('button', { class: 'ss-tab', type: 'button', role: 'tab', onclick: () => showTab('terminal') }, 'Terminal');
  const filesTab = h('button', { class: 'ss-tab', type: 'button', role: 'tab', onclick: () => showTab('files') }, 'Files');
  const diffTab = h('a', { class: 'ss-tab', href: `#/s/${encodeURIComponent(id)}/diff`, hidden: true }, 'Diff');
  const replayTab = h('button', { class: 'ss-tab', type: 'button', role: 'tab', onclick: () => showTab('replay') }, 'Replay');
  const flowTab = h('button', { class: 'ss-tab', type: 'button', role: 'tab', onclick: () => showTab('flow') }, 'Flow');
  const previewTab = h('button', { class: 'ss-tab', type: 'button', role: 'tab', onclick: () => showTab('preview') }, 'Preview');
  const tabs = h('div', { class: 'ss-tabs', role: 'tablist', 'aria-label': 'Session views' }, termTab, filesTab, previewTab, replayTab, flowTab, diffTab);
  const filesBox = h('div', { class: 'ss-files' });
  const replayBox = h('div', { class: 'ss-replay-box', hidden: true });
  const flowBox = h('div', { class: 'ss-flow-box', hidden: true });
  const previewBox = h('div', { class: 'ss-preview-box', hidden: true });

  root.append(h('section', { class: 'view view-session' },
    h('div', { class: 'ss-head' },
      h('a', { class: 'btn btn-ghost ss-back', href: '#/sessions', 'aria-label': 'Back to sessions' }, '←'),
      h('div', { class: 'ss-headtext' }, titleEl, metaEl, energyEl), stateEl),
    actions, notice, tabs, termBox, filesBox, previewBox, replayBox, flowBox, ...(readOnly ? [roBar] : [ti.keys, ti.micNote, ti.bar])));

  function showTab(next) {
    view = next === 'files' || next === 'replay' || next === 'flow' || next === 'preview' ? next : 'terminal';
    const onFiles = view === 'files';
    const onReplay = view === 'replay';
    const onFlow = view === 'flow';
    const onPreview = view === 'preview';
    termTab.setAttribute('aria-selected', String(view === 'terminal'));
    filesTab.setAttribute('aria-selected', String(onFiles));
    replayTab.setAttribute('aria-selected', String(onReplay));
    flowTab.setAttribute('aria-selected', String(onFlow));
    previewTab.setAttribute('aria-selected', String(onPreview));
    termBox.hidden = view !== 'terminal';
    filesBox.hidden = !onFiles;
    replayBox.hidden = !onReplay;
    flowBox.hidden = !onFlow;
    previewBox.hidden = !onPreview;
    if (onPreview) {
      if (!preview) { preview = previewPanel(id, { readOnly }); previewBox.append(preview.el); }
      preview.open();
    } else if (preview) {
      preview.close();
    }
    if (onReplay) {
      if (!replay) { replay = replayPanel(id, { attempt, t }); replayBox.append(replay.el); }
      replay.open();
    } else if (replay) {
      replay.close();
    }
    // The Flow graph of this session's tree: live only while shown.
    if (onFlow) {
      if (!flow) { flow = flowPanel(`${sid(id)}/flow`); flowBox.append(flow.el); }
      flow.open();
    } else if (flow) {
      flow.close();
    }
    if (onFiles) {
      if (!files) { files = filesPanel(id, { upload: (list, dir) => upload(list, dir), sendText, readOnly }); filesBox.append(files.el); }
      files.open();
    } else if (files) {
      files.close();
    }
    ti.sync();
    if (view !== 'terminal') ti.keys.hidden = true;
    // Keep the address in step without a hashchange (which would remount);
    // the Replay panel keeps its own (the address carries the run).
    if (!onReplay) {
      const hash = `#/s/${encodeURIComponent(id)}${onFiles ? '/files' : onFlow ? '/flow' : onPreview ? '/preview' : ''}`;
      if (location.hash !== hash) history.replaceState(null, '', hash);
    }
    if (view === 'terminal') refit();
  }

  // ---------- files in ----------
  // upload sends files into the workspace: dir "" is the inbox.
  function upload(list, dir = '') {
    return uploadToSession(id, list, { dir, name: info ? sessionName(info) : '' });
  }
  // From the input bar: the inbox; pasted files also get their paths typed
  // into the input, ready to mention in the next message.
  async function inputFiles(list, { fromPaste }) {
    const res = await upload(list, '');
    if (!fromPaste) return;
    const rels = res.flatMap((r) => (r.entries || []).map((e) => e.Rel)).filter(Boolean);
    if (rels.length && !ti.input.disabled) ti.input.value = insertedPaths(ti.input.value, rels);
  }
  // Drops land where the user is looking: the folder open in Files, else
  // the inbox.
  const popDrop = readOnly ? () => {} : pushDropTarget({
    dest: () => ({ kind: 'session', name: info ? sessionName(info) : '', dir: view === 'files' && files ? files.dir() : '' }),
    onFiles: (list) => upload(list, view === 'files' && files ? files.dir() : ''),
  });

  // sendText hands text to the agent: typed into a live session, or a
  // follow-up (a new attempt) for a finished one.
  async function sendText(text) {
    if (!info) return;
    if (isLive(info.State)) { ti.send(text + '\r'); toast('Sent to the agent', 'ok'); return; }
    if (takesFollowUp(info)) {
      try { await post(`${sid(id)}/followup`, { text }); toast('Follow-up sent', 'ok'); } catch (e) { toast(errText(e), 'err'); }
      return;
    }
    toast('The session is not running and cannot take a follow-up', 'warn');
  }

  // ---------- terminal ----------
  term = new Terminal({
    disableStdin: readOnly,
    fontFamily: TERM_FONT,
    fontSize: matchMedia('(max-width: 640px)').matches ? 12 : 13,
    scrollback: 5000, cursorBlink: false, theme: TERM_THEME, allowProposedApi: false,
  });
  const fit = new FitAddon();
  term.loadAddon(fit);
  let disposed = false;

  term.onData((d) => { if (!readOnly && info && isLive(info.State)) ti.send(d); });

  let lastSize = '';
  const pushSize = debounce(() => {
    const key = `${term.cols}x${term.rows}`;
    if (readOnly || key === lastSize || !info || !isLive(info.State)) return;
    lastSize = key;
    post(`${sid(id)}/resize`, { cols: term.cols, rows: term.rows }).catch(() => { lastSize = ''; });
  }, 300);
  const refit = debounce(() => {
    if (disposed || !termBox.clientWidth || !termBox.clientHeight) return;
    try { fit.fit(); } catch { return; }
    pushSize();
  }, 120);
  const ro = new ResizeObserver(() => refit());

  // Fonts must be ready before xterm measures its cell size.
  document.fonts.load("13px 'IBM Plex Mono'").catch(() => {}).finally(() => {
    if (disposed) return;
    term.open(termBox);
    ro.observe(termBox);
    refit();
  });

  // ---------- output stream ----------
  let fresh = true;
  const write = (data) => {
    if (fresh) { term.reset(); fresh = false; }
    term.write(b64ToBytes(data));
  };
  const stream = new LiveSource(`${sid(id)}/stream`, {
    open: () => { fresh = true; },
    backlog: write,
    out: write,
  });

  // ---------- delegation ----------
  // Jobs this session's agent handed to local models (cerberus_ask): read
  // when the view opens and when the session's state changes, never polled.
  let delegated = '';
  let delegatedState = '';
  async function loadDelegated() {
    try {
      const d = await api(`${sid(id)}/delegation?period=30d`);
      const text = delegationText(d && d.total, d && d.reference);
      if (text !== delegated) { delegated = text; if (info) renderNotice(); }
    } catch { /* not shown */ }
  }

  // ---------- state ----------
  const render = () => {
    if (info && info.State !== delegatedState) { delegatedState = info.State; loadDelegated(); }
    if (!info) { titleEl.textContent = 'Loading…'; return; }
    titleEl.textContent = sessionName(info);
    metaEl.textContent = [ownerLabel(info, me), workspaceLabel(info), info.Agent + (info.Model ? ` · ${info.Model}` : ''),
      info.Attempt ? `attempt ${info.Attempt}` : ''].filter(Boolean).join(' · ');
    const energy = sessionEnergyText(info);
    energyEl.textContent = energy ? `energy ${energy}` : '';
    energyEl.hidden = !energy;
    clear(stateEl).append(pill(info.State, stateTone(info.State)));
    renderActions();
    renderNotice();
    diffTab.hidden = workspaceKind(info) !== 'repo';
    diffTab.classList.toggle('disabled', !canDiff(info));
    ti.sync();
    if (view !== 'terminal' || readOnly) ti.keys.hidden = true;
    if (isLive(info.State)) pushSize();
  };

  function renderActions() {
    const s = info.State;
    const kind = workspaceKind(info);
    const pending = needsApproval(info);
    if (readOnly) {
      // Watching: preview, the zip and the log only.
      clear(actions).append(...[
        previewLink(info),
        kind === 'scratch' && canSave(info)
          ? h('a', { class: 'btn', href: `${sid(id)}/archive`, download: `cerberus-${id}.zip` }, 'Download zip') : null,
        btn('⧉ Copy log', (e) => copyLater(api(`${sid(id)}/transcript`, { text: true }), e.currentTarget), false, 'btn-ghost'),
        h('a', { class: 'btn btn-ghost', href: `${sid(id)}/transcript`, download: `cerberus-${id}.log`, 'aria-label': 'Download the log' }, '↓ Log'),
      ].filter(Boolean));
      return;
    }
    clear(actions).append(...[
      pending ? btn('Approve', () => approve(true), false, 'btn-primary') : null,
      pending ? btn('Reject', () => approve(false), false, 'btn-danger') : null,
      btn('Run checks', () => act('checks', 'Checks started'), !canFollowUp(s)),
      btn('Stop', () => act('stop', 'Stopped'), !canStop(s)),
      isLive(s) ? btn('Kill', kill, false, 'btn-danger') : null,
      previewLink(info),
      kind === 'repo' ? btn('Accept', accept, !canAcceptSession(info), 'btn-primary') : null,
      // Saving into a host folder is for admins; others download the zip.
      kind === 'scratch' && store.can('host') ? btn('Save to folder…', saveTo, !canSave(info), 'btn-primary') : null,
      kind === 'scratch' && canSave(info)
        ? h('a', { class: 'btn', href: `${sid(id)}/archive`, download: `cerberus-${id}.zip` }, 'Download zip') : null,
      // The session's plain transcript (every run, colours and cursor moves
      // stripped), fetched at the click so it is complete.
      btn('⧉ Copy log', (e) => copyLater(api(`${sid(id)}/transcript`, { text: true }), e.currentTarget), false, 'btn-ghost'),
      h('a', { class: 'btn btn-ghost', href: `${sid(id)}/transcript`, download: `cerberus-${id}.log`, 'aria-label': 'Download the log' }, '↓ Log'),
      btn('Resume…', resume, !canResume(s)),
      btn('Limits…', editLimits, false),
      btn('Discard', discard, !canDiscard(s), 'btn-danger'),
      btn('Delete…', remove, false, 'btn-danger'),
    ].filter(Boolean)); // append() would print a null as text
  }

  function renderNotice() {
    const checksOpen = notice.querySelector('details.checks')?.open || false;
    clear(notice);
    const kind = workspaceKind(info);
    if (kind === 'folder' && info.Dir) {
      notice.append(h('p', { class: 'ws-path' }, h('span', { class: 'ws-tag', text: 'folder' }), ' ', info.Dir, ' · edited in place'));
    } else if (kind === 'scratch' && info.State !== 'discarded') {
      notice.append(h('p', { class: 'ws-path' }, h('span', { class: 'ws-tag', text: 'scratch' }), ' ',
        store.can('host') ? 'Not saved anywhere yet: use Save to folder or Download zip.' : 'Not saved anywhere yet: use Download zip to keep its files.'));
    }
    lineage(notice, info);
    if (delegated) notice.append(h('p', { class: 'ws-path' }, h('span', { class: 'ws-tag', text: 'delegated' }), ' ', delegated));
    const lim = limitsText(info.Limits, guard());
    if (lim) notice.append(h('p', { class: 'ws-path' }, h('span', { class: 'ws-tag', text: 'limits' }), ' ', lim));
    if (info.State === 'waiting' && info.WaitingReason) notice.append(h('div', { class: 'alert warn', text: `Waiting: ${info.WaitingReason}` }));
    if (info.State === 'queued' && info.WaitingReason) notice.append(h('div', { class: 'alert info', text: `Queued: ${info.WaitingReason}` }));
    if (info.Error) notice.append(h('div', { class: 'alert err', text: info.Error }));
    if (info.ChecksOutput) {
      notice.append(h('details', { class: 'checks', open: checksOpen },
        h('summary', { text: `Check output${info.State === 'failed' ? ' (failed)' : ''}` }),
        h('pre', { class: 'code checks-out', text: info.ChecksOutput })));
    }
  }

  async function act(path, done) {
    try { await post(`${sid(id)}/${path}`); toast(done, 'ok'); } catch (e) { toast(errText(e), 'err'); }
  }

  async function accept() {
    // The repo's configured "push" presets the toggle; failure just leaves it off.
    const repos = await api('/api/repos').catch(() => []);
    const preset = Array.isArray(repos) && repos.some((r) => r.path === info.Repo && r.push);
    const res = await openDialog((close) => {
      const msg = h('textarea', { class: 'textarea', rows: '3', value: info.Title || '' });
      const push = h('input', { type: 'checkbox' });
      push.checked = preset;
      return {
        title: 'Accept changes',
        sub: 'Squash, fast-forward main',
        body: h('div', { class: 'task-form' },
          h('label', { class: 'field' }, h('span', { class: 'field-label', text: 'Commit message' }), msg),
          h('label', { class: 'check' }, push, 'Push to origin/main')),
        foot: [
          h('button', { class: 'btn btn-ghost', type: 'button', onclick: () => close(null) }, 'Cancel'),
          h('button', { class: 'btn btn-primary', type: 'button', onclick: () => close({ message: msg.value.trim(), push: push.checked }) }, 'Accept'),
        ],
      };
    });
    if (!res) return;
    try {
      const out = await post(`${sid(id)}/accept`, res);
      const sha = out && (out.commit || out.sha || out.Commit);
      const done = sha ? `Accepted ${String(sha).slice(0, 10)}` : 'Accepted';
      // Accepted locally but the push failed: one toast says both.
      if (out && out.warning) toast(`${done}, but the push failed: ${out.warning}`, 'err');
      else toast(done, 'ok');
    } catch (e) { toast(errText(e), 'err'); }
  }

  // resume starts a new attempt in the same workspace, continuing the
  // agent's own conversation when its profile supports that.
  async function resume() {
    const text = await openDialog((close) => {
      const msg = h('textarea', { class: 'textarea', rows: '4', placeholder: 'Optional: what to do next. Empty = continue where it left off.' });
      const m = micControl(msg);
      setTimeout(() => msg.focus(), 0);
      return {
        title: 'Resume session',
        sub: 'New attempt in the same workspace',
        body: h('div', { class: 'task-form' }, textWithMic('Instruction', msg, m)),
        foot: [
          h('button', { class: 'btn btn-ghost', type: 'button', onclick: () => close(null) }, 'Cancel'),
          h('button', { class: 'btn btn-primary', type: 'button', onclick: () => close(msg.value.trim()) }, 'Resume'),
        ],
      };
    });
    if (text == null) return;
    try { await post(`${sid(id)}/resume`, { text }); toast('Resumed', 'ok'); } catch (e) { toast(errText(e), 'err'); }
  }

  // remove deletes the session for good and goes back to the list.
  async function remove() {
    const gone = await deleteSessions([id]);
    if (gone.length) location.hash = '#/sessions';
  }

  async function discard() {
    const kind = workspaceKind(info);
    const message = kind === 'folder'
      ? 'This stops the agent and forgets the session. Your folder and the changes already made in it are kept.'
      : kind === 'scratch'
        ? 'This stops the agent and deletes its scratch folder. Save or download anything you want to keep first.'
        : 'This stops the agent and deletes its worktree. Uncommitted work in it is lost.';
    const ok = await confirmDialog({ title: 'Discard session', ok: 'Discard', danger: true, message });
    if (ok) act('discard', 'Discarded');
  }

  // saveTo copies a scratch session's files into a folder the user picks.
  async function saveTo() {
    const res = await openDialog((close) => {
      const picker = folderPicker({ purpose: 'save' });
      const sub = h('input', { class: 'input', placeholder: 'Optional new sub-folder, e.g. report', autocapitalize: 'off', spellcheck: 'false' });
      const overwrite = h('input', { type: 'checkbox' });
      return {
        title: 'Save to folder',
        sub: 'Copies the scratch files',
        body: h('div', { class: 'task-form' }, picker.el,
          h('label', { class: 'field' }, h('span', { class: 'field-label', text: 'Sub-folder' }), sub),
          h('label', { class: 'check' }, overwrite, 'Overwrite files that already exist')),
        foot: [
          h('button', { class: 'btn btn-ghost', type: 'button', onclick: () => close(null) }, 'Cancel'),
          h('button', { class: 'btn btn-primary', type: 'button', onclick: () => {
            const sel = picker.value();
            if (!sel) { toast('Pick a folder first', 'warn'); return; }
            const dir = joinPath(sel.path, sub.value);
            if (!dir) { toast('The sub-folder name cannot contain . or ..', 'warn'); return; }
            close({ dir, overwrite: overwrite.checked });
          } }, 'Save'),
        ],
      };
    });
    if (!res) return;
    try {
      const out = await post(`${sid(id)}/save`, res);
      const n = (out && out.files) || 0;
      toast(`Saved ${n} file${n === 1 ? '' : 's'} to ${res.dir}`, 'ok');
    } catch (e) { toast(errText(e), 'err'); }
  }

  async function approve(yes) {
    if (!yes && !(await confirmDialog({ title: 'Reject spawn', ok: 'Reject', danger: true,
      message: 'An agent asked to start this child session. Rejecting means it never runs.' }))) return;
    await approveSession(id, yes);
  }

  async function kill() {
    const ok = await confirmDialog({ title: 'Kill session', ok: 'Kill', danger: true,
      message: 'Sends SIGKILL to the agent and everything it started, right away. Use it when Stop does not work.' });
    if (ok) await killSession(id);
  }

  async function editLimits() {
    // The memory wording needs the guard's view (default cap, enforced).
    if (!store.status()) await store.refreshStatus().catch(() => null);
    const limits = await limitsDialog(info.Limits || {}, guard());
    if (limits) await patchSession(id, { Limits: limits }, 'Limits saved');
  }

  const unsub = store.subscribe((kind, payload) => {
    if (kind === 'session' && payload.ID === id) { info = payload; render(); }
    if (kind === 'sessions') { const s = store.session(id); if (s) { info = s; render(); } }
    if (kind === 'status' && info) render();
  });
  store.refreshStatus().catch(() => {}); // current memory_enforced for the limits line
  showTab(tab);
  render();
  api(sid(id)).then((s) => { store.upsert(s); }).catch((e) => toast(errText(e), 'err'));
  stream.start();

  return () => {
    disposed = true;
    popDrop();
    if (files) files.destroy();
    if (replay) replay.destroy();
    if (flow) flow.destroy();
    if (preview) preview.destroy();
    unsub();
    stream.stop();
    ro.disconnect();
    refit.cancel();
    pushSize.cancel();
    term.dispose();
  };
}

// previewLink opens the session's preview in a new window (null when it
// has none); the Preview tab is the shared view.
function previewLink(info) {
  const c = previewChip(info);
  return c ? h('a', { class: 'btn', href: c.url, target: '_blank', rel: 'noopener' }, `${c.label} ↗`) : null;
}

function btn(label, onclick, disabled, cls = '') {
  return h('button', { class: `btn ${cls}`.trim(), type: 'button', disabled, onclick }, label);
}

// lineage notes who spawned this session and which children it has.
// guard is /api/status guard (memory default and whether caps apply), or null.
function guard() {
  const st = store.status();
  return (st && st.guard) || null;
}

function lineage(el, info) {
  if (info.Parent) {
    const parent = store.session(info.Parent);
    el.append(h('p', { class: 'ws-path' }, h('span', { class: 'ws-tag', text: 'spawned' }), ' by ',
      h('a', { href: `#/s/${encodeURIComponent(info.Parent)}`, text: parent ? sessionName(parent) : info.Parent }),
      info.Depth ? ` · depth ${info.Depth}` : ''));
  }
  const kids = store.sessions().filter((s) => s.Parent === info.ID);
  if (kids.length) {
    el.append(h('p', { class: 'ws-path' },
      h('span', { class: 'ws-tag', text: `${kids.length} ${kids.length === 1 ? 'child' : 'children'}` }), ' ',
      kids.map((k, i) => [i ? ', ' : '', h('a', { href: `#/s/${encodeURIComponent(k.ID)}`, text: `${sessionName(k)} (${k.State})` })])));
  }
}

// limitsDialog edits a session's Limits; it resolves the new Limits, or
// null when cancelled.
export function limitsDialog(cur, guardStatus) {
  const help = limitsHelp(guardStatus);
  return openDialog((close) => {
    const num = (v) => (v ? String(v) : '');
    const minutes = h('input', { class: 'input', type: 'number', min: '0', inputmode: 'numeric', placeholder: help.minutes, value: num(cur.MaxMinutes) });
    const mem = h('input', { class: 'input', placeholder: help.memory, autocapitalize: 'off', spellcheck: 'false', value: cur.MemoryMax || '' });
    const out = h('input', { class: 'input', type: 'number', min: '0', inputmode: 'numeric', placeholder: help.output, value: num(cur.MaxOutputMiB) });
    const err = h('div', { class: 'alert err', hidden: true });
    const save = () => {
      const r = validateLimits({ MaxMinutes: minutes.value, MemoryMax: mem.value, MaxOutputMiB: out.value });
      if (r.error) { err.textContent = r.error; err.hidden = false; return; }
      close(r.limits);
    };
    const lbl = (text, el, hint) => h('label', { class: 'field' }, h('span', { class: 'field-label', text }), el,
      h('span', { class: 'lim-hint', text: hint }));
    return {
      title: 'Session limits',
      sub: 'Empty time or output means no limit; empty memory means the default cap',
      body: h('form', { class: 'task-form', onsubmit: (e) => { e.preventDefault(); save(); } },
        lbl('Time limit (minutes per run)', minutes, 'The run is stopped with “time limit” when it takes longer.'),
        lbl('Memory', mem, 'Memory cap for the run (systemd MemoryMax), e.g. 512M or 4G. Applies from the next run.'),
        help.memoryNote ? h('div', { class: 'alert warn', text: help.memoryNote }) : null,
        lbl('Output limit (MiB per run)', out, 'The run is stopped with “output limit” when it prints more.'),
        err, h('button', { type: 'submit', hidden: true })),
      foot: [
        h('button', { class: 'btn btn-ghost', type: 'button', onclick: () => close(null) }, 'Cancel'),
        h('button', { class: 'btn btn-primary', type: 'button', onclick: save }, 'Save'),
      ],
    };
  });
}

// sessionEnergyText is "12.3 Wh · $0.05 MXN" for a session that used energy
// (its CPU share plus the GPU energy of its model requests), "" otherwise.
export function sessionEnergyText(info) {
  const j = +info.EnergyJ || 0;
  if (j <= 0) return '';
  const cost = +info.CostMXN || 0;
  return [fmtEnergy(j), cost > 0 ? fmtMoney(cost, 'MXN') : ''].filter(Boolean).join(' · ');
}

