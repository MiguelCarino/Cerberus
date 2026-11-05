// The LLM chat's mascot stage: a speech bubble over an ASCII mascot drawn
// as coloured <span> runs (one per colour, merged), from the mascots
// registry (mascots/index.js) and the mood engine (mascots/mood.js).
//
// The chat's events drive it: a StageMachine (chat.js) picks the state
// (idle, thinking, tool with its prop, writing, done, error → idle after
// 15 s) and moodEvents turns events into Mood reactions. A click pokes the
// mascot, a double click pets it.
//
// Energy: frames at 10/s while something happens, 2.5/s after 10 s of
// quiet, none after 120 s, and none at all while the tab is hidden or the
// stage has no size (collapsed, another view). One timer, rescheduled per
// frame; nothing polls. Reduce motion (the system setting, or the menu)
// draws at a third of the speed with the reaction sign steady.

import { h, openDialog } from '../dom.js';
import { loadTheme, frameSpans, THEMES, NAMES, DEFAULT } from '../mascots/index.js';
import { Mood } from '../mascots/mood.js';
import { StageMachine, stageGrid, mergeRuns, moodEvents, FAST_MS } from '../chat.js';

const KEY = 'cerberus.mascot';
const BASE_PX = 13; // the art's font size at scale 1
const SCENE_REFRESH_S = 60; // time of day: re-read once a minute while drawing

// mascotPrefs normalises the remembered settings.
export function mascotPrefs(raw) {
  const r = raw && typeof raw === 'object' ? raw : {};
  return {
    theme: THEMES.includes(r.theme) ? r.theme : DEFAULT,
    moods: r.moods !== false,
    reduce: ['system', 'on', 'off'].includes(r.reduce) ? r.reduce : 'system',
  };
}

function loadPrefs() {
  try { return mascotPrefs(JSON.parse(localStorage.getItem(KEY) || 'null')); } catch { return mascotPrefs(null); }
}
function savePrefs(p) {
  try { localStorage.setItem(KEY, JSON.stringify(p)); } catch { /* private mode: this page only */ }
}

const reducedQuery = typeof matchMedia === 'function' ? matchMedia('(prefers-reduced-motion: reduce)') : { matches: false, addEventListener() {} };
const nowS = () => performance.now() / 1000;

// createStage returns {el, artBox, event(ev), react(name, kw), override(text),
// prefs(), setPrefs(p), wake(), destroy()}.
export function createStage() {
  let prefs = loadPrefs();
  let theme = null;
  let mood = null;
  const machine = new StageMachine(nowS());
  const ctx = { tools: new Map(), reply: '' };
  const t0 = performance.now();
  let timer = 0;
  let destroyed = false;
  let overrideText = null;
  let grid = { cols: 0, rows: 0, scale: 0 }; // none yet: the first fit sets the font size
  let cell = null;
  let lastSig = '';
  let lastBubble = null;
  let lastTickReact = 0;

  const bubble = h('div', { class: 'stage-bubble', 'aria-hidden': 'true' });
  const art = h('pre', { class: 'stage-art', 'aria-hidden': 'true' });
  const probe = h('span', { class: 'stage-probe', 'aria-hidden': 'true', text: 'M'.repeat(20) });
  const artBox = h('div', { class: 'stage-artbox', role: 'img', 'aria-label': 'Mascot', title: 'Click to poke, double-click to pet' }, art, probe);
  const live = h('div', { class: 'sr-only', 'aria-live': 'polite' });
  const el = h('div', { class: 'stage' }, bubble, artBox, live);

  artBox.addEventListener('click', () => react('poke'));
  artBox.addEventListener('dblclick', () => react('pet'));

  const calm = () => (prefs.reduce === 'on' ? true : prefs.reduce === 'off' ? false : reducedQuery.matches);

  function makeMood() {
    mood = new Mood({ enabled: prefs.moods, calm: calm() });
    mood.refresh();
  }

  async function useTheme(id) {
    const m = await loadTheme(id);
    if (destroyed) return;
    theme = m;
    el.style.setProperty('--stage-bg', (m.meta && m.meta.panel) || '#000');
    lastSig = '';
    schedule();
  }

  // measure the cell at scale 1 once (and after a font load).
  function measure() {
    probe.style.fontSize = `${BASE_PX}px`;
    const r = probe.getBoundingClientRect();
    if (r.width > 0 && r.height > 0) cell = { w: r.width / 20, h: r.height };
  }

  function fit() {
    const w = artBox.clientWidth;
    const hgt = artBox.clientHeight;
    if (w < 8 || hgt < 8) return false;
    if (!cell) measure();
    if (!cell) return false;
    const g = stageGrid(w - 4, hgt - 2, cell.w, cell.h);
    if (g.cols !== grid.cols || g.rows !== grid.rows || g.scale !== grid.scale) {
      grid = g;
      art.style.fontSize = `${(BASE_PX * g.scale).toFixed(2)}px`;
      lastSig = '';
    }
    return true;
  }

  const ro = typeof ResizeObserver === 'function' ? new ResizeObserver(() => { if (fit()) schedule(); }) : null;
  ro?.observe(artBox);
  if (document.fonts && document.fonts.ready) document.fonts.ready.then(() => { cell = null; if (fit()) schedule(); });

  function view(state) {
    if (!mood || !theme || !theme.meta || !theme.meta.moods) return null;
    try { return mood.view(state); } catch { return null; }
  }

  function draw() {
    if (!theme || !fit()) return;
    const now = nowS();
    const state = machine.tick(now);
    const tick = Math.floor((performance.now() - t0) / 100);
    if (mood && now - mood.refreshed > SCENE_REFRESH_S) mood.refresh();
    // The long-run reaction is checked once a second, as Atrium does.
    if (mood && machine.running && now - lastTickReact >= 1) { lastTickReact = now; mood.react('tick'); }
    const v = view(state);
    let rows;
    try {
      rows = frameSpans(theme, {
        state, tick, tool: machine.tool, mood: v ? v.mood : null, scene: v ? v.scene : null,
        cols: grid.cols, rows: grid.rows, emote: v ? v.emote : null, calm: calm(),
      });
    } catch (e) {
      rows = [[[`(mascot error: ${e && e.message})`, null]]];
    }
    paint(rows);
    let text = overrideText;
    if (!text && v && v.line) { try { text = theme.say(v.line); } catch { text = null; } }
    if (!text) { try { text = theme.bubble(state, machine.tool, v ? v.mood : null); } catch { text = ''; } }
    if (text !== lastBubble) {
      lastBubble = text;
      bubble.textContent = text || '';
      bubble.hidden = !text;
      artBox.setAttribute('aria-label', text ? `Mascot: ${text}` : 'Mascot');
    }
  }

  function paint(rows) {
    // Same frame as last time (a quiet idle): leave the DOM alone.
    const sig = rows.map((r) => r.map((x) => `${x[0]}\u0001${x[1] || ''}\u0001${x[2] || ''}`).join('\u0002')).join('\n');
    if (sig === lastSig) return;
    lastSig = sig;
    const frag = document.createDocumentFragment();
    rows.forEach((row, i) => {
      if (i) frag.append('\n');
      for (const [text, fg, bg] of mergeRuns(row)) {
        if (!fg && !bg) { frag.append(text); continue; }
        const s = document.createElement('span');
        s.textContent = text;
        if (fg) s.style.color = fg;
        if (bg) s.style.background = bg;
        frag.append(s);
      }
    });
    art.replaceChildren(frag);
  }

  // schedule asks for a frame soon: at once, or when FAST_MS has passed
  // since the last one (a fast stream of events never draws more than
  // 10 frames a second). An earlier pending frame is kept.
  let lastDraw = -Infinity;
  let due = Infinity;
  function schedule() {
    if (destroyed || document.hidden) return;
    const now = performance.now();
    const at = Math.max(now, lastDraw + FAST_MS);
    if (timer && due <= at) return;
    clearTimeout(timer);
    due = at;
    timer = setTimeout(frame, at - now);
  }

  function frame() {
    timer = 0;
    due = Infinity;
    if (destroyed || document.hidden) return;
    lastDraw = performance.now();
    draw();
    const next = machine.interval(nowS(), document.hidden);
    if (next > 0) { due = lastDraw + next; timer = setTimeout(frame, next); }
  }

  const onVisible = () => {
    if (document.hidden) { clearTimeout(timer); timer = 0; return; }
    machine.wake(nowS());
    schedule();
  };
  document.addEventListener('visibilitychange', onVisible);
  const onReduced = () => { if (mood) mood.calm = calm(); schedule(); };
  reducedQuery.addEventListener?.('change', onReduced);

  function react(name, kw = {}) {
    machine.wake(nowS());
    if (mood) { try { mood.react(name, kw); } catch { /* a reaction is never worth an error */ } }
    schedule();
  }

  makeMood();
  useTheme(prefs.theme);

  return {
    el,
    // event feeds one chat stream event (replays pass quiet = true: the
    // state follows, the mood does not react to history).
    event(ev, { quiet = false } = {}) {
      machine.event(ev, nowS());
      if (!quiet && mood) {
        for (const [name, kw] of moodEvents(ev, ctx)) { try { mood.react(name, kw); } catch { /* ignore */ } }
      }
      schedule();
    },
    react,
    override(text) {
      overrideText = text || null;
      if (text) live.textContent = text;
      schedule();
    },
    state: () => machine.state,
    prefs: () => ({ ...prefs }),
    setPrefs(p) {
      const next = mascotPrefs({ ...prefs, ...p });
      const themeChanged = next.theme !== prefs.theme;
      const moodChanged = next.moods !== prefs.moods || next.reduce !== prefs.reduce;
      prefs = next;
      savePrefs(prefs);
      if (moodChanged) makeMood();
      if (themeChanged) useTheme(prefs.theme);
      machine.wake(nowS());
      schedule();
    },
    wake() { machine.wake(nowS()); schedule(); },
    destroy() {
      destroyed = true;
      clearTimeout(timer);
      ro?.disconnect();
      document.removeEventListener('visibilitychange', onVisible);
      reducedQuery.removeEventListener?.('change', onReduced);
    },
  };
}

// openMascotMenu is the mascot menu (Ctrl+J): theme, moods, reduce motion.
export function openMascotMenu(stage) {
  const p = stage.prefs();
  const sel = h('select', { class: 'select', 'aria-label': 'Mascot theme' },
    THEMES.map((id) => h('option', { value: id, selected: id === p.theme, text: NAMES[id] || id })));
  sel.addEventListener('change', () => stage.setPrefs({ theme: sel.value }));
  const moods = h('input', { type: 'checkbox', checked: p.moods });
  moods.addEventListener('change', () => stage.setPrefs({ moods: moods.checked }));
  const reduce = h('select', { class: 'select', 'aria-label': 'Reduce motion' },
    [['system', 'Follow the system'], ['on', 'On (a third of the speed)'], ['off', 'Off']].map(([v, t]) => h('option', { value: v, selected: v === p.reduce, text: t })));
  reduce.addEventListener('change', () => stage.setPrefs({ reduce: reduce.value }));
  return openDialog(() => ({
    title: 'Mascot',
    sub: 'Saved in this browser',
    body: h('div', { class: 'mm' },
      h('label', { class: 'field' }, h('span', { class: 'field-label', text: 'Theme' }), sel),
      h('label', { class: 'mm-check' }, moods, h('span', { text: 'Moods and reactions (time of day, how the chat goes, pokes)' })),
      h('label', { class: 'field' }, h('span', { class: 'field-label', text: 'Reduce motion' }), reduce),
      h('p', { class: 't-sec mm-hint', text: 'Click the mascot to poke it, double-click to pet it. The page keeps the Carino palette; only the stage follows the theme.' })),
  }));
}
