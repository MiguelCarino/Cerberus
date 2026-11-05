// Session Replay tab (day phase 6): plays a run's asciicast recording
// (GET /api/sessions/{id}/runs, then /runs/{attempt}.cast) in a read-only
// xterm, with play/pause, 1×/2×/4×/16× speed, pauses capped at 2 s, a
// scrubber with activity marks and a "copy link" to the current moment
// ("#/s/<id>/replay/<attempt>/<seconds>").
//
// Energy: one timer only while playing and visible; hiding the tab, leaving
// the tab or the page pauses. Nothing streams: a run still being recorded
// is replayed as far as it got (Reload fetches the rest).

import { Terminal } from '../../vendor/xterm/xterm.mjs';
import { api, sid } from '../api.js';
import { h, clear, toast, errText } from '../dom.js';
import {
  SPEEDS, IDLE_CAP, parseCast, capIdle, lastIndexAtOrBefore, framesUpTo, activityBins, fmtClock,
  replayHash, normRuns,
} from '../cast.js';
import { fmtBytes } from '../monitor.js';
import { TERM_THEME, TERM_FONT } from './terminput.js';
import { debounce } from '../util.js';

const BINS = 120;

// replayPanel builds the tab for session id. want = {attempt, t} from the
// route (both optional). Returns {el, open(), close(), destroy()}.
export function replayPanel(id, want = {}) {
  const runSel = h('select', { class: 'select input-sm replay-run', 'aria-label': 'Run' });
  const playBtn = h('button', { class: 'btn btn-sm btn-primary replay-play', type: 'button', disabled: true, onclick: () => (playing ? pause() : play()) }, 'Play');
  const speedBtns = SPEEDS.map((s) => h('button', {
    class: 'fchip', type: 'button', 'aria-pressed': String(s === 1), onclick: () => setSpeed(s),
  }, `${s}×`));
  const timeEl = h('span', { class: 'replay-time mono', text: '0:00 / 0:00' });
  const linkBtn = h('button', { class: 'btn btn-sm btn-ghost', type: 'button', disabled: true, onclick: copyLink }, 'Copy link');
  const reloadBtn = h('button', { class: 'btn btn-sm btn-ghost', type: 'button', onclick: () => loadRuns(current && current.attempt) }, 'Reload');
  const marks = h('div', { class: 'replay-marks', 'aria-hidden': 'true' });
  const scrub = h('input', { class: 'replay-scrub', type: 'range', min: '0', max: '0', step: '0.1', value: '0', disabled: true, 'aria-label': 'Position' });
  const note = h('div', { class: 'replay-note', hidden: true });
  const termBox = h('div', { class: 'replay-term' });
  const el = h('div', { class: 'ss-replay', hidden: true },
    h('div', { class: 'replay-bar' },
      runSel, playBtn,
      h('div', { class: 'fchips replay-speed', role: 'group', 'aria-label': 'Speed' }, speedBtns),
      timeEl, linkBtn, reloadBtn),
    h('div', { class: 'replay-scrubber' }, marks, scrub),
    note, termBox);

  let term = null;
  let runs = [];
  let current = null; // {attempt, recording}
  let cast = null; // parsed
  let times = []; // capped playback time per frame
  let duration = 0;
  let idx = -1; // last frame written
  let pos = 0; // playback position (s)
  let playing = false;
  let speed = 1;
  let timer = 0;
  let clockStart = 0; // performance.now() when (re)started
  let posStart = 0;
  let shown = false;
  let destroyed = false;
  let loadSeq = 0;
  let pendingT = want.t || 0;

  runSel.addEventListener('change', () => loadCast(+runSel.value, 0));
  // Dragging only moves the label; the (costlier) terminal rebuild is
  // coalesced.
  const seekSoon = debounce(() => seek(+scrub.value), 60);
  scrub.addEventListener('input', () => { pos = +scrub.value; renderTime(); seekSoon(); });

  function ensureTerm(cols, rows) {
    if (term && term.cols === cols && term.rows === rows) return;
    if (term) term.dispose();
    term = new Terminal({
      cols, rows, fontFamily: TERM_FONT, fontSize: matchMedia('(max-width: 640px)').matches ? 11 : 13,
      scrollback: 2000, cursorBlink: false, disableStdin: true, theme: TERM_THEME, allowProposedApi: false,
    });
    clear(termBox);
    term.open(termBox);
  }

  async function loadRuns(prefer) {
    const seq = ++loadSeq;
    note.hidden = true;
    try {
      runs = normRuns(await api(`${sid(id)}/runs`));
    } catch (e) {
      if (seq === loadSeq) showNote(errText(e), 'err');
      return;
    }
    if (seq !== loadSeq || destroyed) return;
    clear(runSel).append(...runs.map((r) => h('option', { value: String(r.attempt) },
      `Run ${r.attempt}${r.recording ? ' (recording)' : ''} · ${fmtBytes(r.size)}`)));
    if (!runs.length) {
      reset();
      showNote('No recordings yet. Each run is recorded once it starts (unless recording is off in Settings).', 'info');
      return;
    }
    const pick = runs.find((r) => r.attempt === prefer) || runs[0];
    runSel.value = String(pick.attempt);
    await loadCast(pick.attempt, pendingT);
    pendingT = 0;
  }

  async function loadCast(attempt, t) {
    pause();
    const seq = ++loadSeq;
    current = runs.find((r) => r.attempt === attempt) || { attempt, recording: false };
    let text;
    try {
      text = await api(`${sid(id)}/runs/${attempt}.cast`, { text: true });
    } catch (e) {
      if (seq === loadSeq) showNote(errText(e), 'err');
      return;
    }
    if (seq !== loadSeq || destroyed) return;
    cast = parseCast(text);
    if (!cast) { reset(); showNote('This recording cannot be read (not asciicast v2).', 'err'); return; }
    const capped = capIdle(cast.frames, IDLE_CAP);
    times = capped.times;
    duration = capped.duration;
    note.hidden = true;
    const stopped = cast.markers.find((m) => /recording stopped/i.test(m.label));
    if (current.recording) showNote('This run is still being recorded: Reload fetches what it printed since.', 'info');
    else if (stopped) showNote('The recording reached its size limit and stopped there (Settings → Sessions).', 'warn');
    ensureTerm(cast.header.width, cast.header.height);
    renderMarks(capped.map);
    scrub.max = String(duration);
    scrub.disabled = !cast.frames.length;
    playBtn.disabled = !cast.frames.length;
    linkBtn.disabled = !cast.frames.length;
    syncHash();
    seek(Math.min(t || 0, duration));
  }

  function reset() {
    cast = null; times = []; duration = 0; idx = -1; pos = 0;
    scrub.max = '0'; scrub.value = '0'; scrub.disabled = true;
    playBtn.disabled = true; linkBtn.disabled = true;
    clear(marks);
    if (term) term.reset();
    renderTime();
  }

  function showNote(text, tone) {
    note.className = `replay-note alert ${tone}`;
    note.textContent = text;
    note.hidden = false;
  }

  function renderMarks(map) {
    clear(marks);
    if (!cast) return;
    const bins = activityBins(times, cast.frames, duration, BINS);
    marks.append(...bins.map((v) => h('span', { class: 'replay-bin', style: `height:${v > 0 ? Math.max(8, Math.round(v * 100)) : 0}%` })));
    for (const m of cast.markers) {
      const left = duration > 0 ? (map(m.t) / duration) * 100 : 0;
      marks.append(h('span', { class: 'replay-marker', style: `left:${Math.min(100, left).toFixed(2)}%`, title: m.label }));
    }
  }

  function renderTime() {
    timeEl.textContent = `${fmtClock(pos)} / ${fmtClock(duration)}`;
    if (document.activeElement !== scrub) scrub.value = String(pos);
  }

  // seek rebuilds the screen at time t: reset, then everything up to it.
  function seek(t) {
    if (!cast || !term) return;
    pos = Math.max(0, Math.min(t, duration));
    idx = lastIndexAtOrBefore(times, pos);
    term.reset();
    if (idx >= 0) term.write(framesUpTo(cast.frames, idx));
    renderTime();
    if (playing) restartClock();
  }

  function restartClock() {
    clockStart = performance.now();
    posStart = pos;
    schedule();
  }

  function schedule() {
    clearTimeout(timer);
    timer = 0;
    if (!playing) return;
    if (idx + 1 >= times.length) { pos = duration; renderTime(); pause(); return; }
    const wait = ((times[idx + 1] - pos) * 1000) / speed;
    timer = setTimeout(tick, Math.max(0, wait));
  }

  // tick writes every frame due by now in one go (dense output at 16× is
  // one write, not hundreds of timers).
  function tick() {
    timer = 0;
    if (!playing || !cast) return;
    pos = Math.min(duration, posStart + ((performance.now() - clockStart) / 1000) * speed);
    let chunk = '';
    while (idx + 1 < times.length && times[idx + 1] <= pos) chunk += cast.frames[++idx].data;
    if (chunk) term.write(chunk);
    renderTime();
    schedule();
  }

  function play() {
    if (!cast || !cast.frames.length || document.hidden) return;
    if (pos >= duration) seek(0);
    playing = true;
    playBtn.textContent = 'Pause';
    playBtn.setAttribute('aria-pressed', 'true');
    restartClock();
  }

  function pause() {
    playing = false;
    clearTimeout(timer);
    timer = 0;
    playBtn.textContent = 'Play';
    playBtn.setAttribute('aria-pressed', 'false');
  }

  function setSpeed(s) {
    speed = s;
    speedBtns.forEach((b, i) => b.setAttribute('aria-pressed', String(SPEEDS[i] === s)));
    if (playing) restartClock();
  }

  async function copyLink() {
    const url = `${location.origin}${location.pathname}${replayHash(id, current && current.attempt, pos)}`;
    try {
      await navigator.clipboard.writeText(url);
      toast(`Link to ${fmtClock(pos)} copied`, 'ok');
    } catch {
      toast(url, 'info');
    }
  }

  // The address follows the chosen run without a hashchange (a remount).
  function syncHash() {
    if (!shown) return;
    const hash = replayHash(id, current && current.attempt, 0);
    if (location.hash !== hash) history.replaceState(null, '', hash);
  }

  const onVis = () => { if (document.hidden) pause(); };
  document.addEventListener('visibilitychange', onVis);

  return {
    el,
    open() {
      shown = true;
      el.hidden = false;
      if (!runs.length && !cast) loadRuns(want.attempt);
      else syncHash();
    },
    close() {
      shown = false;
      el.hidden = true;
      pause();
    },
    destroy() {
      destroyed = true;
      pause();
      seekSoon.cancel?.();
      document.removeEventListener('visibilitychange', onVis);
      if (term) term.dispose();
    },
  };
}
