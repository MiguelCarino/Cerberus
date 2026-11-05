// Mic button for task text: press to record, press again to stop (or wait
// for the length limit), and the local transcript (POST
// /api/voice/transcribe) is inserted at the cursor for review. It is never
// sent on its own. Hidden when /api/voice says no backend is available.

import { api } from '../api.js';
import { h, toast, errText } from '../dom.js';
import { insertAtCursor } from '../text.js';
import { store } from '../store.js';
import {
  pickMime, fmtClock, rmsLevel, micSupport, insecureNote, recordLimitMs, DICTATION_HINT, MAX_UPLOAD_BYTES,
} from '../media.js';

const DEFAULT_MAX_SECONDS = 300;
let infoPromise = null;

// voiceInfo fetches /api/voice once per page load; a failure is retried on
// the next call.
export function voiceInfo() {
  // Users and roles: no dictation for roles without the voice cap.
  if (store.me() && !store.can('voice')) return Promise.resolve({ available: false });
  if (!infoPromise) {
    infoPromise = api('/api/voice').catch(() => { infoPromise = null; return { available: false }; });
  }
  return infoPromise;
}

// micControl returns { el, note }: el is the button with its timer and level
// meter (place it next to the field), note an explanation shown when the
// microphone cannot be used here (place it under the field). target is the
// textarea or input that receives the text.
export function micControl(target) {
  const label = h('span', { class: 'mic-label', text: 'Dictate' });
  const btn = h('button', { class: 'btn mic-btn', type: 'button', 'aria-label': 'Record a voice note', onclick: () => press() },
    micIcon(), label);
  const bar = h('span', { class: 'mic-level-bar' });
  const meter = h('span', { class: 'mic-level', 'aria-hidden': 'true' }, bar);
  const time = h('span', { class: 'mic-time', 'aria-live': 'off' });
  const el = h('span', { class: 'mic', hidden: true }, btn, meter, time);
  const noteText = h('span');
  const note = h('div', { class: 'alert warn mic-note', hidden: true }, noteText);

  let maxSeconds = DEFAULT_MAX_SECONDS;
  let state = 'idle'; // idle | starting | rec | busy
  let rec = null;
  let stream = null;
  let ctx = null;
  let analyser = null;
  let buf = null;
  let chunks = [];
  let started = 0;
  let tick = 0;
  let limit = 0;
  let discard = false;

  voiceInfo().then((info) => {
    if (!info || !info.available) return;
    if (info.max_seconds > 0) maxSeconds = info.max_seconds;
    const sup = support();
    el.hidden = false;
    el.classList.toggle('mic-off', !sup.ok);
    noteText.textContent = noteFor(sup.reason, info.secure_hint);
  });

  function support() {
    return micSupport({
      secure: window.isSecureContext,
      mediaDevices: !!(navigator.mediaDevices && navigator.mediaDevices.getUserMedia),
      recorder: typeof MediaRecorder !== 'undefined',
    });
  }

  function setState(s) {
    state = s;
    el.classList.toggle('recording', s === 'rec');
    btn.disabled = s === 'starting' || s === 'busy';
    label.textContent = s === 'rec' ? 'Stop' : s === 'busy' ? 'Transcribing…' : s === 'starting' ? 'Starting…' : 'Dictate';
    btn.setAttribute('aria-label', s === 'rec' ? 'Stop recording' : 'Record a voice note');
    if (s !== 'rec') { time.textContent = ''; bar.style.width = '0%'; }
  }

  function press() {
    if (state === 'rec') { stopRec(); return; }
    if (state !== 'idle') return;
    const sup = support();
    if (!sup.ok) { note.hidden = !note.hidden; return; }
    startRec();
  }

  async function startRec() {
    setState('starting');
    try {
      stream = await navigator.mediaDevices.getUserMedia({ audio: { echoCancellation: true, noiseSuppression: true } });
    } catch (e) {
      setState('idle');
      toast(e && e.name === 'NotAllowedError' ? 'Microphone permission was denied' : `Microphone: ${errText(e)}`, 'err');
      return;
    }
    const mime = pickMime((m) => MediaRecorder.isTypeSupported(m));
    try {
      rec = new MediaRecorder(stream, mime ? { mimeType: mime } : undefined);
    } catch (e) {
      release();
      setState('idle');
      toast(`Recording is not supported here: ${errText(e)}`, 'err');
      return;
    }
    chunks = [];
    discard = false;
    rec.ondataavailable = (e) => { if (e.data && e.data.size) chunks.push(e.data); };
    rec.onstop = () => finish(mime);
    try {
      ctx = new AudioContext();
      analyser = ctx.createAnalyser();
      analyser.fftSize = 512;
      ctx.createMediaStreamSource(stream).connect(analyser);
      buf = new Uint8Array(analyser.fftSize);
    } catch { analyser = null; } // the meter is optional
    started = performance.now();
    rec.start(1000);
    // The meter and timer tick only while recording.
    tick = setInterval(update, 100);
    limit = setTimeout(stopRec, recordLimitMs(maxSeconds));
    setState('rec');
    update();
  }

  function update() {
    // The field went away (dialog closed, view left): drop the recording.
    if (!el.isConnected) { discard = true; stopRec(); return; }
    const sec = (performance.now() - started) / 1000;
    time.textContent = `${fmtClock(sec)} / ${fmtClock(maxSeconds)}`;
    if (analyser) {
      analyser.getByteTimeDomainData(buf);
      bar.style.width = `${Math.round(rmsLevel(buf) * 100)}%`;
    }
  }

  function stopRec() {
    clearInterval(tick);
    clearTimeout(limit);
    tick = limit = 0;
    if (rec && rec.state !== 'inactive') rec.stop();
    else { release(); setState('idle'); }
  }

  function release() {
    if (stream) for (const t of stream.getTracks()) t.stop();
    stream = null;
    if (ctx) ctx.close().catch(() => {});
    ctx = analyser = buf = null;
  }

  async function finish(mime) {
    const type = (rec && rec.mimeType) || mime || 'audio/webm';
    rec = null;
    release();
    if (discard) { chunks = []; setState('idle'); return; }
    const blob = new Blob(chunks, { type });
    chunks = [];
    if (!blob.size) { setState('idle'); toast('Nothing was recorded', 'warn'); return; }
    if (blob.size > MAX_UPLOAD_BYTES) { setState('idle'); toast('The recording is over 25 MiB', 'err'); return; }
    setState('busy');
    try {
      const out = await api('/api/voice/transcribe', { method: 'POST', body: blob });
      const text = ((out && out.text) || '').trim();
      if (!text) toast('No speech recognised', 'warn');
      else if (target.isConnected) insert(text);
    } catch (e) {
      toast(`Transcription failed: ${errText(e)}`, 'err');
    }
    setState('idle');
  }

  function insert(text) {
    const { value, caret } = insertAtCursor(target.value, target.selectionStart, target.selectionEnd, text);
    target.value = value;
    try { target.setSelectionRange(caret, caret); } catch { /* some input types have no selection */ }
    target.dispatchEvent(new Event('input', { bubbles: true }));
    toast('Transcript inserted: review it before sending', 'ok');
  }

  return { el, note };
}

function noteFor(reason, hint) {
  if (reason === 'insecure') return insecureNote(hint);
  if (reason === 'unsupported') return `This browser cannot record audio. ${DICTATION_HINT}`;
  return '';
}

const SVG = 'http://www.w3.org/2000/svg';

function micIcon() {
  const svg = document.createElementNS(SVG, 'svg');
  svg.setAttribute('viewBox', '0 0 24 24');
  svg.setAttribute('class', 'mic-icon');
  svg.setAttribute('aria-hidden', 'true');
  for (const d of ['M12 3a3 3 0 0 0-3 3v6a3 3 0 0 0 6 0V6a3 3 0 0 0-3-3z', 'M5 11a7 7 0 0 0 14 0', 'M12 18v3']) {
    const p = document.createElementNS(SVG, 'path');
    p.setAttribute('d', d);
    svg.append(p);
  }
  return svg;
}
