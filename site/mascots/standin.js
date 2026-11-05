// The stand-in: what the stage draws when no theme module loads (a port of
// llm-chat.py's _StandIn). One centred kaomoji, the same module API as every
// theme, so nothing else has to know. Its faces hold wide characters: the
// stand-in is not drawn on the mascot grid, so it centres by code points as
// Python's str.center does, and colours the whole frame in one run.

import { defaultLine } from './mood.js';

export const FACES = {
  idle: '(｡•ᴗ•｡)', thinking: '(・・?)', tool: '(ง •̀_•́)ง',
  writing: 'φ(..)', done: '(≧◡≦) ♡', error: '(╥﹏╥)',
};
export const STAGE_FG = '#c8d3e6';

export const meta = { id: 'standin', name: 'Stand-in', panel: '#000000', moods: false };
export const emoteColours = {};

// center is Python's str.center(width) (the odd space goes left when width
// is odd and the margin is odd, as CPython's pad does).
export function center(s, width) {
  const n = Array.from(s).length;
  const marg = width - n;
  if (marg <= 0) return s;
  const left = Math.floor(marg / 2) + (marg & width & 1);
  return ' '.repeat(left) + s + ' '.repeat(marg - left);
}

export function lines(state, tick, tool = null, mood = null, scene = null, cols = 64, rows = 18) {
  const out = Array.from({ length: rows }, () => ' '.repeat(cols));
  out[Math.floor(rows / 2)] = center(FACES[state] || FACES.idle, cols);
  return out;
}

export function spans(...args) {
  return lines(...args).map((l) => [[l, STAGE_FG]]);
}

// render: the stand-in has no cell grid (frameSpans falls back to spans).
export function render() {
  return null;
}

export function bubble(state, tool = null) {
  return {
    idle: 'なにする？', thinking: 'うーん…', tool: `${tool || 'tool'} がんばる！`,
    writing: 'かきかき…', done: 'できた！', error: 'あれれ…',
  }[state] ?? '…';
}

export function say(topic) {
  return defaultLine(topic);
}
