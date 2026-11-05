// Chat view (day phase 5): Atrium's desktop LLM chat in the browser. A
// chat runs on the server (tools execute on this host, closing the tab does
// not stop it); this view shows its transcript, follows its event stream
// (GET /api/chats/{id}/stream, replayed from an index after a reconnect),
// answers permission cards and sends messages with images.
//
// Layout: the mascot stage (stage.js) sits on top on phones and narrow
// windows and in a right-hand column from 1100 px; Ctrl+K folds it into a
// corner card (click "Move" to go round the corners), remembered per
// browser. Below: the transcript, the input strip (image pills, the text,
// attach, mic, Send/Queue, Stop) and the status line.
//
// Energy: the stream is a LiveSource (closed while the tab is hidden), the
// elapsed clock ticks once a second only while a run goes and the tab is
// visible, the stage sleeps when quiet, and tool spinners are CSS.

import { api, post, LiveSource, AuthError, messageOf, notifyUnauthorized } from '../api.js';
import { h, clear, toast, errText } from '../dom.js';
import {
  chatIdOk, messagesToItems, beforeLastUser, replayFrom, statusParts, stagePrefs, nextCorner, historyStep,
  pushHistory, findAll, findStep, findLabel, scaleDims, imageEncoding, IMAGE_BYTES, MAX_IMAGES, parseSSE,
  shortPath, modelProblem, needsModelPick, modelGroups, switchesBackend, streamVerdict, replayPlan, preRunQueue,
} from '../chat.js';
import { userBubble, assistantBlock, toolChip, noteRow, errorCard, endRow, copyText, fixCard } from './chatdom.js';
import { permCard } from './chatperm.js';
import { createStage, openMascotMenu } from './stage.js';
import { openModelPicker, openModeMenu, openRecent, openFolder } from './chatmenus.js';
import { pushDropTarget, pickerButton, onPasteFiles } from './drop.js';
import { micControl } from './mic.js';
import { openFlowSheet } from './flow.js';
import { store } from '../store.js';
import { isOwn, ownerOf } from '../users.js';

const LAST_KEY = 'cerberus.chat.last';
const STAGE_KEY = 'cerberus.chat.stage';
const HIST_KEY = 'cerberus.chat.history';
const STICK_PX = 40; // the transcript follows new output unless scrolled up this far

// The stream's event types (agent events plus the chat's own).
const EVENTS = ['start', 'text', 'reasoning', 'tool', 'tool_done', 'ask', 'no_tools', 'usage', 'note', 'done',
  'user', 'queued', 'dropped', 'answered', 'state', 'status', 'switched', 'error', 'rules'];

function load(key, dflt) {
  try { const v = JSON.parse(localStorage.getItem(key)); return v ?? dflt; } catch { return dflt; }
}
function save(key, v) {
  try { localStorage.setItem(key, JSON.stringify(v)); } catch { /* private mode: this page only */ }
}

const enc = encodeURIComponent;

export function mount(root, r = {}) {
  // Users and roles: "Chat only" has no tools (no mode picker, no working
  // folder); a user's chats work in their own folder (no folder picker);
  // only admins set the default model or load models. An admin can open
  // anyone's chat read only (and stop its reply); only its owner sends.
  const me = store.me();
  const hasTools = store.can('chat.tools');
  const host = store.can('host');
  const canSettings = store.can('settings');
  const canModels = store.can('models.manage');
  let foreign = null; // another person's chat shown read only ({name})
  // ---------- state ----------
  let chatId = null;
  let chat = null; // the Chat (title, cwd, model, mode, messages)
  let status = {};
  let rules = [];
  let modelsData = null;
  let nextModel = '';
  let nextMode = '';
  let newCwd = '';
  let source = null;
  let next = 0; // the next event index wanted
  let buffer = null; // a busy chat's backlog, until index bufferUntil - 1
  let bufferUntil = 0;
  let epoch = ''; // the daemon's log epoch (a restart changes it)
  let reloads = 0; // reloads in a row a stream's hello asked for
  let pendingQueue = []; // a busy chat's queue, for the replay (preRunQueue)
  let lastUserEl = null; // the newest user bubble
  let lastUserText = ''; // its text, for Retry after a model problem
  let fixCards = []; // model-problem cards whose buttons still apply
  let readOnly = null; // the desktop session shown read-only ({id, title})
  let cur = null; // the reply being written
  let lastModel = '';
  const tools = new Map(); // tool id → chip
  const cards = new Map(); // req id → card (insertion order = oldest first)
  let queuedRows = [];
  let images = []; // {name, w, h, url}
  let sent = load(HIST_KEY, []); // the input history (Up/Down)
  if (!Array.isArray(sent)) sent = [];
  let histIdx = sent.length;
  let draft = '';
  let stick = true;
  let gone = false;
  let elapsedTimer = 0;
  let runT0 = 0;
  let quickAbort = null;
  let stagePos = stagePrefs(load(STAGE_KEY, null));

  // ---------- DOM ----------
  const stage = createStage();
  const titleEl = h('div', { class: 'chat-title', text: 'New chat' });
  const cwdBtn = h('button', { class: 'chat-cwd', type: 'button', title: 'Working folder', hidden: !host, onclick: () => chooseFolder() }, 'home folder');
  const modelBtn = h('button', { class: 'btn btn-sm chat-model', type: 'button', title: 'Model (Ctrl+M)', onclick: () => pickModel() }, 'Model');
  // The same button on the title row: the keyboard folds the tools row away
  // on phones, and the model must stay one tap away.
  const kbModelBtn = h('button', { class: 'chat-kbmodel', type: 'button', title: 'Model', onmousedown: (e) => e.preventDefault(), onclick: () => pickModel() }, 'Model');
  const modeBtn = h('button', { class: 'btn btn-sm', type: 'button', title: 'Permissions and session rules', hidden: !hasTools, onclick: () => pickMode() }, 'Ask me');
  const mascotBtn = h('button', { class: 'btn btn-sm btn-ghost', type: 'button', title: 'Mascot (Ctrl+J)', 'aria-label': 'Mascot menu', onclick: () => openMascotMenu(stage) }, '✿');
  const cornerBtn = h('button', { class: 'btn btn-sm btn-ghost', type: 'button', title: 'Corner card (Ctrl+K)', 'aria-label': 'Fold the stage into a corner card', onclick: () => toggleCorner() }, '◲');
  const recentBtn = h('button', { class: 'btn btn-sm', type: 'button', title: 'Recent chats (Ctrl+O)', onclick: () => recent() }, 'Chats');
  const newBtn = h('button', { class: 'btn btn-sm btn-primary', type: 'button', title: 'New chat (Alt+N)', onclick: () => newChat() }, 'New');
  // Flow: who handed what to whom in this chat, as a graph in a sheet.
  const flowBtn = h('button', { class: 'btn btn-sm', type: 'button', title: 'Flow: the models this chat used', disabled: true, onclick: () => showFlow() }, 'Flow');
  let flowSheet = null; // the open Flow sheet ({refresh})

  // The quick answer ("??"): one question, at most 3 short sentences.
  const quickInput = h('input', { class: 'input quick-in', type: 'text', placeholder: '?? quick answer', 'aria-label': 'Quick answer: ask one short question', autocomplete: 'off', enterkeyhint: 'send' });
  const quickText = h('div', { class: 'quick-text', 'aria-live': 'polite' });
  const quickPop = h('div', { class: 'quick-pop', hidden: true },
    quickText,
    h('div', { class: 'quick-actions' },
      h('button', { class: 'btn btn-sm btn-ghost', type: 'button', onclick: (e) => copyText(quickText.textContent, e.currentTarget) }, '⧉ copy'),
      h('button', { class: 'btn btn-sm btn-ghost', type: 'button', 'aria-label': 'Close the quick answer', onclick: () => closeQuick() }, '✕')));
  const quickForm = h('form', { class: 'quick', role: 'search', onsubmit: (e) => { e.preventDefault(); quick(); } }, quickInput, quickPop);

  const head = h('header', { class: 'chat-head' },
    recentBtn,
    h('div', { class: 'chat-headtext' }, titleEl, cwdBtn),
    kbModelBtn,
    quickForm,
    h('div', { class: 'chat-tools' }, modelBtn, modeBtn, flowBtn, mascotBtn, cornerBtn, newBtn));

  const moveBtn = h('button', { class: 'btn btn-sm btn-ghost', type: 'button', title: 'Move to the next corner', 'aria-label': 'Move the card to the next corner', onclick: () => moveCorner() }, '↻');
  const expandBtn = h('button', { class: 'btn btn-sm btn-ghost', type: 'button', title: 'Back to the full stage (Ctrl+K)', 'aria-label': 'Back to the full stage', onclick: () => toggleCorner() }, '⤢');
  const cardBar = h('div', { class: 'stage-cardbar' }, moveBtn, expandBtn);
  const stageWrap = h('div', { class: 'chat-stage' }, cardBar, stage.el);
  // The card moves on a click on its bubble too (the art itself pokes).
  stage.el.querySelector('.stage-bubble')?.addEventListener('click', () => { if (stagePos.corner) moveCorner(); });

  const findInput = h('input', { class: 'input input-sm', type: 'search', placeholder: 'Find in the chat', 'aria-label': 'Find in the chat' });
  const findCount = h('span', { class: 'find-count', text: '0 / 0' });
  const findBar = h('div', { class: 'chat-find', hidden: true },
    findInput, findCount,
    h('button', { class: 'btn btn-sm btn-ghost', type: 'button', 'aria-label': 'Previous match', onclick: () => findMove(-1) }, '↑'),
    h('button', { class: 'btn btn-sm btn-ghost', type: 'button', 'aria-label': 'Next match', onclick: () => findMove(1) }, '↓'),
    h('button', { class: 'btn btn-sm btn-ghost', type: 'button', 'aria-label': 'Close find', onclick: () => closeFind() }, '✕'));

  const log = h('div', { class: 'chat-log', role: 'log', 'aria-label': 'Conversation' });
  const empty = h('div', { class: 'chat-empty' },
    h('p', { class: 'empty-title', text: 'LLM chat' }),
    h('p', { class: 't-sec', text: !hasTools
      ? 'Ask anything. This is a plain chat: the model cannot read files or run commands. You can attach images.'
      : host ? 'Ask anything. The model can read files, search and run commands in the working folder; anything that changes something asks you first.'
        : 'Ask anything. The model works in your own folder on this host; anything that changes something asks you first.' }),
    h('p', { class: 't-sec t-sm', text: 'Enter sends · Shift+Enter new line · Esc stops · Alt+N new chat · Ctrl+O recent chats · Ctrl+M model' }));

  const input = h('textarea', { class: 'textarea chat-text', rows: '1', placeholder: 'Message the model', 'aria-label': 'Message', enterkeyhint: 'send' });
  const pills = h('div', { class: 'chat-pills', hidden: true });
  const sendBtn = h('button', { class: 'btn btn-primary chat-send', type: 'button', onclick: () => send() }, 'Send');
  const stopBtn = h('button', { class: 'btn btn-danger chat-stop', type: 'button', hidden: true, title: 'Stop (Esc)', onclick: () => stop() }, 'Stop');
  const picker = pickerButton((files) => addImages(files), { label: '', cls: 'btn icon-btn', title: 'Attach images' });
  const mic = micControl(input);
  const statusEl = h('div', { class: 'chat-status', 'aria-live': 'off' });
  const pickHint = h('div', { class: 'chat-pickhint', hidden: true, role: 'status' },
    h('span', { text: 'No model is loaded and none is set as the default.' }),
    h('button', { class: 'btn btn-sm btn-primary', type: 'button', onmousedown: (e) => e.preventDefault(), onclick: () => pickModel({ offerDefault: true }) }, 'Pick a model'));
  const strip = h('div', { class: 'chat-input' },
    pickHint,
    pills,
    h('div', { class: 'chat-inrow' }, picker, input, mic.el, sendBtn, stopBtn),
    mic.note, statusEl);

  // A desktop (Atrium) session opened read-only: its transcript, and
  // "Continue here" in place of the input strip.
  const roText = h('span', { class: 'chat-ro-text', text: 'A desktop LLM chat session, read-only.' });
  const roBtn = h('button', { class: 'btn btn-primary btn-sm', type: 'button', onclick: () => continueReadOnly() }, 'Continue here');
  // Another person's chat (admins): Stop its reply, nothing else.
  const roStop = h('button', { class: 'btn btn-danger btn-sm', type: 'button', hidden: true, onclick: () => stop() }, 'Stop reply');
  const roBar = h('div', { class: 'chat-ro', hidden: true }, roText, roBtn, roStop);

  const main = h('div', { class: 'chat-main' }, findBar, log, roBar, strip);
  const body = h('div', { class: 'chat-body' }, stageWrap, main);
  const view = h('section', { class: 'view-chat' }, head, body);
  root.append(view);
  applyStage();
  // The corner card's bottom corners sit above the input strip.
  const stripRO = typeof ResizeObserver === 'function'
    ? new ResizeObserver(() => body.style.setProperty('--strip-h', strip.offsetHeight + 'px')) : null;
  stripRO?.observe(strip);

  // ---------- stage placement ----------
  function applyStage() {
    body.classList.toggle('corner', stagePos.corner);
    for (const c of ['top-right', 'bottom-right', 'bottom-left', 'top-left']) body.classList.toggle(`at-${c}`, stagePos.corner && stagePos.where === c);
    cardBar.hidden = !stagePos.corner;
    cornerBtn.setAttribute('aria-pressed', String(stagePos.corner));
    stage.wake();
  }
  function toggleCorner() { stagePos = { ...stagePos, corner: !stagePos.corner }; save(STAGE_KEY, stagePos); applyStage(); }
  function moveCorner() { stagePos = { ...stagePos, where: nextCorner(stagePos.where) }; save(STAGE_KEY, stagePos); applyStage(); }

  // ---------- transcript ----------
  log.addEventListener('scroll', () => { stick = log.scrollHeight - log.scrollTop - log.clientHeight < STICK_PX; }, { passive: true });
  let scrollQueued = false;
  function add(el) {
    empty.remove();
    log.append(el);
    follow();
    return el;
  }
  function follow() {
    if (!stick || scrollQueued) return;
    scrollQueued = true;
    requestAnimationFrame(() => { scrollQueued = false; log.scrollTop = log.scrollHeight; });
  }
  function resetLog() {
    clear(log);
    log.append(empty);
    tools.clear();
    cards.clear();
    queuedRows = [];
    pendingQueue = [];
    lastUserEl = null;
    cur = null;
    setReadOnly(null);
    stick = true;
    updateOverride();
  }
  function ensureCur() {
    if (!cur) { cur = assistantBlock(); add(cur.el); }
    return cur;
  }
  function endCur() {
    if (!cur) return;
    cur.flush();
    if (cur.empty()) cur.el.remove();
    cur = null;
  }

  function renderItems(items) {
    for (const it of items) {
      if (it.kind === 'user') add(userBubble(it.text, { images: it.images }));
      else if (it.kind === 'assistant') {
        const b = assistantBlock();
        b.set(it.text);
        if (it.reasoning) b.think(it.reasoning);
        add(b.el);
      } else if (it.kind === 'tool') {
        const c = toolChip({ name: it.name, input: it.input, home: homeDir() });
        c.done(it.output, it.isError);
        if (it.isError) c.el.querySelector('details').open = false; // history: closed
        add(c.el);
      }
    }
  }

  // ---------- events ----------
  function handle(ev, replay = false) {
    const t = ev.type;
    switch (t) {
      case 'user': {
        endCur();
        const q = queuedRows.shift();
        if (q) q.remove();
        lastUserEl = add(userBubble(ev.text, { images: ev.images || 0 }));
        lastUserText = ev.text || '';
        retireFixCards();
        lastModel = ev.model || lastModel;
        stick = true;
        follow();
        break;
      }
      case 'queued': {
        const row = userBubble(ev.text, { images: ev.images || 0, queued: true });
        queuedRows.push(row);
        add(row);
        break;
      }
      case 'dropped':
        for (const row of queuedRows) {
          const b = row.querySelector('.cm-user');
          if (b) { b.classList.add('dropped'); b.title = 'Dropped: the run was stopped'; }
        }
        queuedRows = [];
        break;
      case 'text': ensureCur().append(ev.text); follow(); break;
      case 'reasoning': ensureCur().think(ev.text); follow(); break;
      case 'tool': {
        endCur();
        const c = toolChip({ name: ev.name, canon: ev.canon, input: ev.input, home: homeDir() });
        tools.set(ev.id, c);
        add(c.el);
        break;
      }
      case 'tool_done': {
        const c = tools.get(ev.id);
        if (c) { c.done(ev.output, ev.is_error); tools.delete(ev.id); follow(); }
        break;
      }
      case 'ask': {
        if (!ev.req || cards.has(ev.req.id)) break;
        endCur();
        const card = permCard(ev.req, (decision, note, option) => answer(ev.req.id, decision, note, option));
        cards.set(ev.req.id, card);
        add(card.el);
        updateOverride();
        countWaiting();
        break;
      }
      case 'answered': {
        const card = cards.get(ev.req);
        if (card) { card.answered(ev); cards.delete(ev.req); }
        updateOverride();
        countWaiting();
        break;
      }
      case 'note': add(noteRow(ev.text)); break;
      case 'no_tools': add(noteRow(`No tools this time${ev.why ? ': ' + ev.why : ''}. Plain chat.`)); break;
      case 'usage':
        status = { ...status, used: (ev.prompt || 0) + (ev.completion || 0) };
        renderStatus();
        break;
      case 'done': {
        endCur();
        if (flowSheet) flowSheet.refresh(); // a reply ended: new numbers
        const prob = !ev.ok && !ev.stopped ? modelProblem(ev.error) : null;
        if (prob) add(modelFix(prob, ev.error, replay));
        else add(endRow(ev, lastModel || status.model));
        break;
      }
      case 'error': {
        if (typeof ev.text !== 'string') break;
        const prob = modelProblem(ev.text);
        add(prob ? modelFix(prob, ev.text, replay) : errorCard(ev.text));
        break;
      }
      case 'state':
        status = { ...status, busy: !!ev.busy, queue: ev.queue || 0, state: ev.state || status.state };
        if (ev.busy && !runT0) runT0 = performance.now();
        if (!ev.busy) runT0 = 0;
        renderStatus();
        break;
      case 'status':
        status = { ...ev };
        delete status.type;
        delete status.i;
        runT0 = status.busy ? performance.now() - (status.elapsed || 0) * 1000 : 0;
        if (status.mode && status.mode === nextMode) nextMode = '';
        renderStatus();
        break;
      case 'rules': rules = ev.rules || []; break;
      case 'switched':
        if (!replay && ev.to && chatIdOk(ev.to)) { toast(`New chat for ${ev.model || 'that model'}`, 'info'); location.hash = `#/chat/${ev.to}`; }
        break;
      default: break;
    }
    stage.event(ev, { quiet: replay });
  }

  // The status line's "N waiting" follows the open cards live (the
  // server's status events come only at a run's end).
  function countWaiting() {
    const n = [...cards.values()].filter((c) => c.isOpen()).length;
    if ((status.waiting || 0) === n) return;
    status = { ...status, waiting: n };
    renderStatus();
  }

  // A stream's first event: reload the chat when the daemon restarted or
  // the log lost events the page has not seen (at most 3 times in a row).
  function onHello(data) {
    if (gone || !chatId) return;
    let hello;
    try { hello = JSON.parse(data); } catch { return; }
    if (!epoch && hello && hello.epoch) epoch = hello.epoch;
    if (streamVerdict(hello, { epoch, want: next }) === 'ok') { reloads = 0; return; }
    if (reloads >= 3) return;
    reloads++;
    openChat(chatId).catch((e) => { if (!(e instanceof AuthError)) toast(errText(e), 'err'); });
  }

  function onEvent(type, data) {
    if (gone) return;
    let ev;
    try { ev = JSON.parse(data); } catch { return; }
    if (!ev || typeof ev !== 'object') return;
    if (!ev.type) ev.type = type;
    const i = typeof ev.i === 'number' ? ev.i : -1;
    if (i >= 0) {
      if (i < next && !buffer) return; // seen already (a reconnect replays from "from")
      next = Math.max(next, i + 1);
      if (source) source.url = streamURL();
    }
    if (buffer && i >= 0 && i < bufferUntil) {
      buffer.push(ev);
      if (i >= bufferUntil - 1) flushBuffer();
      return;
    }
    if (buffer) flushBuffer();
    handle(ev);
  }

  // flushBuffer replays a busy chat's running message from its backlog.
  function flushBuffer() {
    const evs = buffer || [];
    buffer = null;
    const run = evs.slice(replayFrom(evs));
    for (const ev of run) handle(ev, true);
    // Messages queued before the running one started are not in the
    // replay: they go above its user bubble, where they were queued.
    const rows = preRunQueue(pendingQueue, run).map((q) => userBubble(q, { queued: true }));
    pendingQueue = [];
    if (!rows.length) return;
    if (lastUserEl && lastUserEl.parentNode === log) for (const row of rows) log.insertBefore(row, lastUserEl);
    else for (const row of rows) add(row);
    queuedRows = [...rows, ...queuedRows];
  }

  const streamURL = () => `/api/chats/${enc(chatId)}/stream?from=${next}`;

  function openStream(from, until = 0) {
    closeStream();
    next = from;
    buffer = until > from ? [] : null;
    bufferUntil = until;
    const handlers = { open: () => {}, hello: (data) => { if (data != null) onHello(data); } };
    for (const t of EVENTS) handlers[t] = (data) => { if (data != null) onEvent(t, data); };
    source = new LiveSource(streamURL(), handlers).start();
  }
  function closeStream() {
    if (source) { source.stop(); source = null; }
  }

  // ---------- status ----------
  function ctxOf(id) {
    const m = ((modelsData && modelsData.models) || []).find((x) => (x.ID || x.id) === id);
    return m ? m.Ctx || m.ctx || 0 : 0;
  }
  function renderStatus() {
    const st = { ...status };
    if (st.busy && runT0) st.elapsed = (performance.now() - runT0) / 1000;
    statusEl.textContent = statusParts(st, { nextModel, nextMode, ctxOf }).join(' · ');
    const busy = !!status.busy;
    flowBtn.disabled = !chatId || !!readOnly;
    roStop.hidden = !foreign || !status.busy;
    stopBtn.hidden = !busy;
    sendBtn.textContent = busy ? 'Queue' : 'Send';
    sendBtn.title = busy ? 'Queue: sent when the current run finishes' : 'Send (Enter)';
    const model = nextModel || status.model || (chat && chat.model) || (modelsData && modelsData.default) || '';
    modelBtn.textContent = model ? shortModel(model) : 'Model';
    kbModelBtn.textContent = model ? shortModel(model) : 'Model';
    kbModelBtn.classList.toggle('pending', !!nextModel);
    pickHint.hidden = !modelsData || !!readOnly || !needsModelPick(modelsData, model);
    modelBtn.classList.toggle('pending', !!nextModel);
    modeBtn.textContent = modeName(nextMode || status.mode || (chat && chat.mode) || (modelsData && modelsData.default_mode) || 'ask');
    modeBtn.classList.toggle('pending', !!nextMode);
    const cwd = (readOnly && readOnly.cwd) || (chat && chat.cwd) || status.cwd || newCwd;
    // LRM marks keep the slashes in place: the button is direction:rtl so a
    // long path loses its start, not its end.
    cwdBtn.textContent = cwd ? '\u200e' + shortPath(cwd, homeDir()) + '\u200e' : 'home folder';
    cwdBtn.title = cwd ? `Working folder: ${cwd}` : 'Working folder: your home folder';
    titleEl.textContent = readOnly ? (readOnly.title || 'Desktop session') : (chat && chat.title) || (chatId ? 'Untitled chat' : 'New chat');
    tickElapsed();
  }
  function tickElapsed() {
    const want = !!status.busy && !document.hidden && !gone;
    if (want && !elapsedTimer) elapsedTimer = setInterval(renderStatus, 1000);
    if (!want && elapsedTimer) { clearInterval(elapsedTimer); elapsedTimer = 0; }
  }
  const homeDir = () => (modelsData && modelsData.home) || '';
  const modeName = (m) => ({ ask: 'Ask me', auto: 'Auto', acceptEdits: 'Accept edits', plan: 'Plan' }[m] || m);
  const shortModel = (m) => (m.length > 28 ? '…' + m.slice(-27) : m);

  function updateOverride() {
    const open = [...cards.values()].filter((c) => c.isOpen());
    stage.override(open.length ? `May I use ${open[0].req.tool || 'this tool'}? Answer the card 🔒` : null);
  }

  // showFlow opens this chat's Flow graph in a wide sheet.
  function showFlow() {
    if (!chatId || readOnly || flowSheet) return;
    const sheet = openFlowSheet(`/api/chats/${enc(chatId)}/flow`, { title: 'Flow', sub: (chat && chat.title) || 'This chat' });
    flowSheet = sheet;
    sheet.done.then(() => { if (flowSheet === sheet) flowSheet = null; });
  }

  // ---------- loading ----------
  async function loadModels() {
    modelsData = await api('/api/chats/models');
    return modelsData;
  }

  async function openChat(id) {
    closeStream();
    resetLog();
    chatId = id;
    const v = await api(`/api/chats/${enc(id)}`);
    if (gone || chatId !== id) return;
    chat = v.chat || {};
    status = v.status || {};
    rules = v.rules || [];
    epoch = v.epoch || '';
    lastModel = chat.model || '';
    if (!isOwn(chat, me)) setForeign(ownerOf(chat).name || ownerOf(chat).id);
    runT0 = status.busy ? performance.now() - (status.elapsed || 0) * 1000 : 0;
    save(LAST_KEY, id);
    // A busy chat replays its running message from the log (from its
    // "user" event); an idle one, or one whose run start the log no longer
    // holds, shows what is stored (open cards and the queue included) and
    // listens from now on.
    const plan = replayPlan(v);
    const msgs = plan.replay ? beforeLastUser(chat.messages || []) : (chat.messages || []);
    renderItems(messagesToItems(msgs));
    if (plan.replay) pendingQueue = (v.queue || []).slice();
    else {
      for (const req of v.asks || []) handle({ type: 'ask', req }, true);
      for (const q of v.queue || []) { const row = userBubble(q, { queued: true }); queuedRows.push(row); add(row); }
    }
    renderStatus();
    openStream(plan.from, plan.until);
    stick = true;
    follow();
  }

  function startNew() {
    closeStream();
    resetLog();
    chatId = null;
    chat = null;
    status = {};
    rules = [];
    epoch = '';
    renderStatus();
  }

  // ---------- a desktop session, read-only ----------
  function setReadOnly(ro) {
    readOnly = ro;
    foreign = null;
    roBar.hidden = !ro;
    strip.hidden = !!ro;
    roBtn.hidden = false;
    roStop.hidden = true;
    if (ro) roText.textContent = 'Desktop LLM chat, read-only' + (ro.title ? ': ' + ro.title : '') + '. Continue here makes a copy in Cerberus.';
  }
  // setForeign shows someone else's chat read only (admins see every chat).
  function setForeign(name) {
    foreign = { name };
    roBar.hidden = false;
    strip.hidden = true;
    roBtn.hidden = true;
    roText.textContent = `${name || 'Another user'}’s chat, read only. Only its owner can send messages or answer its requests.`;
  }
  async function viewAtrium(id) {
    startNew();
    const c = await api('/api/chats/atrium?id=' + enc(id));
    if (gone || chatId) return;
    renderItems(messagesToItems((c && c.messages) || []));
    setReadOnly({ id, title: (c && c.title) || '', cwd: (c && c.cwd) || '' });
    renderStatus();
    stick = true;
    follow();
  }
  async function continueReadOnly() {
    if (!readOnly) return;
    roBtn.disabled = true;
    try {
      const c = await api('/api/chats/atrium/' + enc(readOnly.id) + '/continue', { method: 'POST', body: {} });
      if (c && chatIdOk(c.id)) location.hash = '#/chat/' + c.id;
    } catch (e) { toast(errText(e), 'err'); } finally { roBtn.disabled = false; }
  }

  async function init() {
    try {
      await loadModels();
    } catch (e) {
      if (e instanceof AuthError) return;
      disabled(errText(e));
      return;
    }
    if (gone) return;
    let id = r.id;
    if (id === 'new') id = null;
    else if (!id) id = load(LAST_KEY, null);
    if (id && chatIdOk(id)) {
      try {
        await openChat(id);
      } catch (e) {
        if (e instanceof AuthError) return;
        if (r.id) toast(errText(e), 'err');
        save(LAST_KEY, null);
        startNew();
        if (r.id) window.history.replaceState(null, '', '#/chat/new');
      }
    } else {
      startNew();
    }
    renderStatus();
    if (matchMedia('(hover: hover) and (pointer: fine)').matches) input.focus();
  }

  function disabled(msg) {
    clear(log).append(h('div', { class: 'chat-off' },
      h('div', { class: 'alert warn', text: msg }),
      canSettings
        ? h('p', { class: 't-sec' }, 'The LLM chat is switched with ', h('code', { text: 'chat.enabled' }), ' in ', h('a', { href: '#/settings' }, 'Settings'), '.')
        : h('p', { class: 't-sec', text: 'Ask an admin to turn the LLM chat on.' })));
    for (const b of [sendBtn, input, modelBtn, modeBtn]) b.disabled = true;
  }

  // ---------- sending ----------
  async function send() {
    const text = input.value.trim();
    if (!text && !images.length) return;
    if (sendBtn.disabled) return;
    sendBtn.disabled = true;
    try {
      if (!chatId) {
        const c = await post('/api/chats', { cwd: (host && newCwd) || undefined, model: nextModel || undefined, mode: (hasTools && nextMode) || undefined });
        chatId = c.id;
        chat = c;
        save(LAST_KEY, c.id);
        // Keep the URL in step without a remount (replaceState fires no hashchange).
        window.history.replaceState(null, '', `#/chat/${c.id}`);
        openStream(0);
      }
      const bodyOut = { text, images: images.map((x) => x.url) };
      if (nextModel) bodyOut.model = nextModel;
      if (nextMode && hasTools) bodyOut.mode = nextMode;
      const res = await post(`/api/chats/${enc(chatId)}/messages`, bodyOut);
      sent = pushHistory(sent, text);
      save(HIST_KEY, sent);
      histIdx = sent.length;
      draft = '';
      input.value = '';
      autosize();
      images = [];
      renderPills();
      if (nextModel && chat) chat.model = nextModel;
      nextModel = '';
      if (res && res.switched && res.id && res.id !== chatId) {
        location.hash = `#/chat/${res.id}`;
        return;
      }
      renderStatus();
    } catch (e) {
      if (!(e instanceof AuthError)) toast(errText(e), 'err');
    } finally {
      sendBtn.disabled = false;
    }
  }

  async function stop() {
    if (!chatId || !status.busy) return;
    try { await post(`/api/chats/${enc(chatId)}/stop`); } catch (e) { toast(errText(e), 'err'); }
  }

  async function answer(req, decision, note, option) {
    try {
      await post(`/api/chats/${enc(chatId)}/answer`, { req, decision, note: note || undefined, option: decision === 'always' ? option : undefined });
      return true;
    } catch (e) {
      if (/answered already/i.test(errText(e))) return true;
      toast(errText(e), 'err');
      return false;
    }
  }

  // ---------- images ----------
  function renderPills() {
    clear(pills);
    pills.hidden = !images.length;
    images.forEach((img, i) => pills.append(h('span', { class: 'chat-pill' },
      h('span', { text: `🖼 ${img.name} · ${img.w}×${img.h}` }),
      h('button', { type: 'button', class: 'chat-pill-x', 'aria-label': `Remove ${img.name}`, onclick: () => { images.splice(i, 1); renderPills(); } }, '✕'))));
  }

  async function addImages(files) {
    const list = [...files];
    const pics = list.filter((f) => /^image\//.test(f.type));
    const other = list.length - pics.length;
    if (other) toast(`Only images go into a chat (${other} other file${other === 1 ? '' : 's'} left out). Files for agents: a session's Files tab.`, 'warn');
    for (const f of pics) {
      if (images.length >= MAX_IMAGES) { toast(`At most ${MAX_IMAGES} images per message`, 'warn'); break; }
      try {
        images.push(await prepareImage(f));
        renderPills();
      } catch (e) {
        toast(`${f.name}: ${errText(e)}`, 'err');
      }
    }
  }

  const unpushDrop = pushDropTarget({ dest: () => ({ kind: 'chat', name: 'this chat' }), onFiles: (files) => addImages(files) });
  onPasteFiles(input, (files) => addImages(files));

  // ---------- quick answer ----------
  async function quick() {
    const q = quickInput.value.trim();
    if (!q) return;
    if (quickAbort) quickAbort.abort();
    const ctl = new AbortController();
    quickAbort = ctl;
    quickPop.hidden = false;
    quickText.textContent = '…';
    let got = '';
    try {
      const res = await fetch('/api/quick', {
        method: 'POST', credentials: 'same-origin', signal: ctl.signal,
        headers: { 'X-Cerberus': '1', 'Content-Type': 'application/json' },
        body: JSON.stringify({ text: q }),
      });
      if (res.status === 401) { notifyUnauthorized(); return; }
      if (!res.ok) { quickText.textContent = messageOf(await res.text().catch(() => ''), res.status, res.statusText); return; }
      const reader = res.body.getReader();
      const dec = new TextDecoder();
      let buf = '';
      for (;;) {
        const { done, value } = await reader.read();
        if (done) break;
        buf += dec.decode(value, { stream: true });
        const p = parseSSE(buf);
        buf = p.rest;
        for (const e of p.events) {
          let d = {};
          try { d = JSON.parse(e.data || '{}'); } catch { /* keep {} */ }
          if (e.event === 'text') { got += d.text || ''; quickText.textContent = got; }
          else if (e.event === 'error') quickText.textContent = `${got ? got + '\n' : ''}✗ ${d.error || 'failed'}`;
          else if (e.event === 'done' && d.model) quickPop.title = `Answered by ${d.model}`;
        }
      }
      if (!got && quickText.textContent === '…') quickText.textContent = '(no answer)';
    } catch (e) {
      if (e && e.name === 'AbortError') return;
      quickText.textContent = `✗ ${errText(e)}`;
    } finally {
      if (quickAbort === ctl) quickAbort = null;
    }
  }
  function closeQuick() {
    if (quickAbort) { quickAbort.abort(); quickAbort = null; }
    quickPop.hidden = true;
    quickText.textContent = '';
  }
  quickInput.addEventListener('keydown', (e) => { if (e.key === 'Escape') { e.preventDefault(); closeQuick(); quickInput.blur(); } });

  // ---------- menus ----------
  async function pickModel({ offerDefault = false } = {}) {
    if (!modelsData) { try { await loadModels(); } catch (e) { toast(errText(e), 'err'); return ''; } }
    const current = (chat && chat.model) || status.model || '';
    const hasMessages = !!(chat && chat.messages && chat.messages.length) || !!status.busy;
    const noDefault = !modelsData.default || modelsData.default === 'default';
    const id = await openModelPicker({ data: modelsData, current, next: nextModel, hasMessages, reload: loadModels,
      offerDefault: canSettings && (offerDefault || noDefault), setDefault: canSettings ? setDefault : null, manage: canModels });
    if (!id) return '';
    nextModel = id === current ? '' : id;
    if (nextModel && switchesBackend(current, nextModel, hasMessages)) toast('Your next message starts a new chat (another backend)', 'info');
    stage.react('model', { name: id });
    renderStatus();
    return id;
  }

  // setDefault makes id the default model (engines.default_model), so new
  // chats and "default" use it from now on.
  async function setDefault(id) {
    await api('/api/config', { method: 'PATCH', body: { set: { 'engines.default_model': id } } });
    await loadModels();
    toast(`${id} is the default model now`, 'ok');
    renderStatus();
  }

  // ---------- model problems ----------
  // modelFix is the card for a run that failed for want of a model: what
  // happened in plain words, then the fixes. Only the newest card's buttons
  // work; an older one keeps its text.
  function modelFix(prob, raw, replay) {
    const model = (chat && chat.model) || status.model || '';
    const named = model && model !== 'default' ? model : '';
    const local = named && modelGroups(modelsData).some((g) => g.items.some((it) => it.id === named && it.local));
    let title;
    if (prob.kind === 'no-default') title = 'No model to answer: nothing is loaded and no default model is set.';
    else if (prob.kind === 'unknown') title = `The model ${prob.model || named} is not available.`;
    else title = named ? `${named} is not loaded.` : 'No model is loaded.';
    const actions = [];
    if (local && canModels && prob.kind !== 'unknown') actions.push({ label: `Load ${shortModel(named)} and retry`, primary: true, run: () => loadAndRetry(named) });
    actions.push({ label: 'Pick a model', primary: !actions.length, run: async () => { const id = await pickModel({ offerDefault: prob.kind === 'no-default' }); if (id && lastUserText) retry(); } });
    if (lastUserText) actions.push({ label: 'Retry', run: () => retry() });
    const card = fixCard({ title, detail: raw, actions: replay ? [] : actions });
    if (!replay) fixCards.push(card);
    return card.el;
  }
  function retireFixCards() {
    for (const c of fixCards) c.retire();
    fixCards = [];
  }
  async function loadAndRetry(id) {
    toast(`Loading ${id}…`, 'info');
    try {
      await api(`/api/models/${enc(id)}/load`, { method: 'POST', body: {} });
      await loadModels();
      toast(`${id} is loaded`, 'ok');
      retry();
    } catch (e) {
      if (!(e instanceof AuthError)) toast(errText(e), 'err');
    }
  }
  function retry() {
    if (!lastUserText || status.busy) return;
    input.value = lastUserText;
    autosize();
    send();
  }

  async function pickMode() {
    const mode = status.mode || (chat && chat.mode) || (modelsData && modelsData.default_mode) || 'ask';
    const forget = chatId ? async () => { await api(`/api/chats/${enc(chatId)}/rules`, { method: 'DELETE' }); rules = []; } : null;
    const m = await openModeMenu({ mode, next: nextMode, rules, forget });
    if (!m) return;
    nextMode = m === mode ? '' : m;
    renderStatus();
  }

  async function chooseFolder() {
    const current = (chat && chat.cwd) || newCwd;
    const path = await openFolder({ current });
    if (!path) return;
    if (!chatId) { newCwd = path; renderStatus(); return; }
    try {
      chat = await api(`/api/chats/${enc(chatId)}`, { method: 'PATCH', body: { cwd: path } });
      renderStatus();
    } catch (e) { toast(errText(e), 'err'); }
  }

  async function recent() {
    const res = await openRecent({ current: chatId || '', imports: store.can('chat.import'), allUsers: !!(me && me.hasUsers && store.can('users')), me });
    if (res && res.open && chatIdOk(res.open)) location.hash = `#/chat/${res.open}`;
    else if (res && res.atrium && chatIdOk(res.atrium)) {
      if (location.hash !== '#/chat/new') window.history.replaceState(null, '', '#/chat/new');
      viewAtrium(res.atrium).catch((e) => { if (!(e instanceof AuthError)) toast(errText(e), 'err'); });
    }
  }

  function newChat() {
    if (location.hash === '#/chat/new') { startNew(); input.focus(); return; }
    location.hash = '#/chat/new';
  }

  // ---------- find (Ctrl+F) ----------
  let matches = [];
  let matchIdx = -1;
  const hl = typeof CSS !== 'undefined' && CSS.highlights && typeof Highlight === 'function';
  function openFind() {
    findBar.hidden = false;
    findInput.focus();
    findInput.select();
    runFind();
  }
  function closeFind() {
    findBar.hidden = true;
    matches = [];
    matchIdx = -1;
    if (hl) { CSS.highlights.delete('chat-find'); CSS.highlights.delete('chat-find-cur'); }
    input.focus();
  }
  function runFind() {
    const q = findInput.value;
    matches = [];
    if (q) {
      const walker = document.createTreeWalker(log, NodeFilter.SHOW_TEXT);
      for (let n = walker.nextNode(); n; n = walker.nextNode()) {
        if (n.parentElement && n.parentElement.closest('[hidden]')) continue;
        for (const off of findAll(n.data, q)) {
          const range = document.createRange();
          range.setStart(n, off);
          range.setEnd(n, off + q.length);
          matches.push(range);
        }
      }
    }
    matchIdx = matches.length ? matches.length - 1 : -1; // the newest first
    showMatch();
  }
  function findMove(dir) { matchIdx = findStep(matchIdx, matches.length, dir); showMatch(); }
  function showMatch() {
    findCount.textContent = findLabel(matchIdx, matches.length);
    if (hl) {
      CSS.highlights.set('chat-find', new Highlight(...matches));
      if (matchIdx >= 0) CSS.highlights.set('chat-find-cur', new Highlight(matches[matchIdx]));
      else CSS.highlights.delete('chat-find-cur');
    }
    const r = matches[matchIdx];
    if (!r) return;
    const det = r.startContainer.parentElement && r.startContainer.parentElement.closest('details');
    if (det && !det.open) det.open = true;
    r.startContainer.parentElement?.scrollIntoView({ block: 'center' });
    stick = false;
  }
  findInput.addEventListener('input', runFind);
  findInput.addEventListener('keydown', (e) => {
    if (e.key === 'Enter') { e.preventDefault(); findMove(e.shiftKey ? 1 : -1); }
    else if (e.key === 'Escape') { e.preventDefault(); closeFind(); }
  });

  // ---------- input keys ----------
  function autosize() {
    input.style.height = 'auto';
    input.style.height = `${Math.min(160, Math.max(44, input.scrollHeight))}px`;
  }
  input.addEventListener('input', () => { autosize(); if (histIdx === sent.length) draft = input.value; });
  input.addEventListener('keydown', (e) => {
    if (e.isComposing) return;
    if (e.key === 'Enter' && !e.shiftKey && !e.altKey) { e.preventDefault(); send(); return; }
    if (e.key === 'ArrowUp' && !e.shiftKey && input.selectionStart === input.selectionEnd && !input.value.slice(0, input.selectionStart).includes('\n')) {
      if (!sent.length || histIdx === 0) return;
      e.preventDefault();
      if (histIdx === sent.length) draft = input.value;
      const s = historyStep(sent, histIdx, -1, draft);
      histIdx = s.idx;
      input.value = s.value;
      autosize();
      return;
    }
    if (e.key === 'ArrowDown' && !e.shiftKey && histIdx < sent.length && input.selectionStart === input.selectionEnd && !input.value.slice(input.selectionEnd).includes('\n')) {
      e.preventDefault();
      const s = historyStep(sent, histIdx, 1, draft);
      histIdx = s.idx;
      input.value = s.value;
      autosize();
    }
  });

  // Page keys while the chat is open. Ctrl (or ⌘) + F/N/O/M/J/K (Ctrl+N
  // reaches only installed PWA windows), Alt+N new chat, Alt+A/S/F/D for the
  // oldest card, Esc stops a run. Quiet while a dialog is open.
  function onKey(e) {
    if (e.defaultPrevented || e.isComposing || document.querySelector('dialog[open]')) return;
    const mod = (e.ctrlKey || e.metaKey) && !e.altKey && !e.shiftKey;
    if (mod) {
      const k = e.key.toLowerCase();
      const act = { f: openFind, n: newChat, o: recent, m: pickModel, j: () => openMascotMenu(stage), k: toggleCorner }[k];
      if (act) { e.preventDefault(); act(); }
      return;
    }
    if (e.altKey && !e.ctrlKey && !e.metaKey) {
      // Alt+N: browsers keep Ctrl+N (a new window) for themselves.
      if (e.code === 'KeyN' && !e.shiftKey) { e.preventDefault(); newChat(); return; }
      const d = { KeyA: 'once', KeyS: 'session', KeyF: 'always', KeyD: 'deny' }[e.code];
      if (!d) return;
      const card = [...cards.values()].find((c) => c.isOpen());
      if (!card) return;
      e.preventDefault();
      card.press(d);
      return;
    }
    if (e.key === 'Escape' && status.busy && e.target !== findInput && e.target !== quickInput) {
      e.preventDefault();
      stop();
    }
  }
  document.addEventListener('keydown', onKey);

  const onVisible = () => { if (!document.hidden) renderStatus(); else tickElapsed(); };
  document.addEventListener('visibilitychange', onVisible);

  init();

  return () => {
    gone = true;
    closeStream();
    stripRO?.disconnect();
    stage.destroy();
    unpushDrop();
    if (quickAbort) quickAbort.abort();
    if (elapsedTimer) clearInterval(elapsedTimer);
    if (hl) { CSS.highlights.delete('chat-find'); CSS.highlights.delete('chat-find-cur'); }
    document.removeEventListener('keydown', onKey);
    document.removeEventListener('visibilitychange', onVisible);
  };
}

// prepareImage scales a picked, pasted or dropped image to ≤ 1568 px and
// encodes it as a data: URL: a PNG, or JPEG q88 when the PNG is over
// 3.5 MB. A small PNG or JPEG goes as it is.
export async function prepareImage(file) {
  const bmp = await createImageBitmap(file);
  try {
    const d = scaleDims(bmp.width, bmp.height);
    const name = file.name || 'image';
    if (!d.scaled && (file.type === 'image/png' || file.type === 'image/jpeg') && file.size <= IMAGE_BYTES) {
      return { name, w: d.w, h: d.h, url: await blobURL(file) };
    }
    const canvas = document.createElement('canvas');
    canvas.width = d.w;
    canvas.height = d.h;
    const ctx = canvas.getContext('2d');
    ctx.drawImage(bmp, 0, 0, d.w, d.h);
    let blob = await toBlob(canvas, 'image/png');
    const encd = imageEncoding(blob.size);
    if (encd.type === 'image/jpeg') {
      // JPEG has no transparency: paint it over white.
      ctx.globalCompositeOperation = 'destination-over';
      ctx.fillStyle = '#fff';
      ctx.fillRect(0, 0, d.w, d.h);
      blob = await toBlob(canvas, 'image/jpeg', encd.quality);
    }
    return { name, w: d.w, h: d.h, url: await blobURL(blob) };
  } finally {
    bmp.close?.();
  }
}

function toBlob(canvas, type, quality) {
  return new Promise((resolve, reject) => canvas.toBlob((b) => (b ? resolve(b) : reject(new Error('could not encode the image'))), type, quality));
}

function blobURL(blob) {
  return new Promise((resolve, reject) => {
    const fr = new FileReader();
    fr.onload = () => resolve(String(fr.result));
    fr.onerror = () => reject(fr.error || new Error('could not read the image'));
    fr.readAsDataURL(blob);
  });
}
