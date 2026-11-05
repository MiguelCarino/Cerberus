// The mascot registry: one ASCII mascot per theme, loaded on demand with
// import(), so the Chat view only downloads the theme it shows. A port of
// Atrium's mascots.py registry.
//
// Every theme module (./<id>.js) exports the same API (see README.md):
//   meta          {id, name, panel, moods}
//   lines(state, tick, tool, mood, scene, cols, rows)   -> rows strings
//   spans(...same)  -> rows of [text, colour] / [text, colour, bg] runs
//   render(...same) -> {canvas, head} (or null for a theme without a grid)
//   bubble(state, tool, mood) -> speech line;  say(topic) -> topic line
//   emoteColours  tone -> colour for the reaction sign over the head
//
//   const m = await loadTheme('moe');
//   const rows = frameSpans(m, {state: 'tool', tool: 'Bash', tick, cols, rows,
//                               mood: view.mood, scene: view.scene, emote: view.emote});

import * as standin from './standin.js';

export { frameSpans, calmTick, STATES, TOOLS } from './common.js';

export const DEFAULT = 'moe';

// Loaders per theme id. Every id is registered now; a theme whose module is
// not there yet falls back to moe (then to the stand-in) when loaded.
const LOADERS = {
  moe: () => import('./moe.js'),
  matrix: () => import('./matrix.js'),
  win95: () => import('./win95.js'),
  aero: () => import('./aero.js'),
  cyberpunk: () => import('./cyberpunk.js'),
  lofi: () => import('./lofi.js'),
  luna: () => import('./luna.js'),
  orbit: () => import('./orbit.js'),
  seasons: () => import('./seasons.js'),
  orchard: () => import('./orchard.js'),
  'orchard-dark': () => import('./orchard-dark.js'),
};

export const THEMES = Object.keys(LOADERS);

// Display names for menus before a module is loaded (each module's meta.name
// is the same).
export const NAMES = {
  moe: 'Moe', matrix: 'Matrix', win95: 'Windows 95', aero: 'Aero', cyberpunk: 'Cyberpunk',
  lofi: 'Lo-fi', luna: 'Luna', orbit: 'Orbit', seasons: 'Seasons', orchard: 'Orchard',
  'orchard-dark': 'Orchard Dark',
};

const loaded = new Map(); // id -> Promise<module>

export function hasTheme(id) {
  return Object.hasOwn(LOADERS, id);
}

// loadTheme resolves to the theme module for id; an unknown id, or one whose
// module fails to load, gives moe, and moe failing gives the stand-in, so the
// stage always has something to draw.
export function loadTheme(id) {
  const key = hasTheme(id) ? id : DEFAULT;
  if (!loaded.has(key)) {
    const p = LOADERS[key]()
      .catch((err) => {
        // The fallback is remembered: a missing module is fetched once only.
        if (key !== DEFAULT) return loadTheme(DEFAULT);
        if (typeof console !== 'undefined') console.warn('mascots: no theme module', err);
        return standin;
      });
    loaded.set(key, p);
  }
  return loaded.get(key);
}

export { standin };
