// The orchard-dark theme's mascot: Hum, the same honeybee as orchard.js (the
// drawing, the moods, the scenes, his words), bound to that file's DARK
// palette -- light lines on the dark stage (#232325). A port of Atrium's
// mascot_orchard_dark.py (golden tests: web/test/mascots-themes-b.test.mjs).
// The bee keeps his amber and his dark stripes; the outlines, the leaves, the
// hive's straw and the card turn over for the night appearance.

import { makeTheme } from './common.js';
import { word } from './mood.js';
import { Mascot as Orchard, DARK, TOPIC_LINES } from './orchard.js';

export const PANEL = DARK.panel;
export const EMOTE_COLOURS = {
  accent: '#409cff', warn: '#ff9f0a', bad: '#ff453a', good: '#30d158', love: '#ff375f', dim: '#a1a1a6',
};

class Mascot extends Orchard {}
Mascot.PAL = DARK;

export { Mascot };

const theme = makeTheme({
  meta: { id: 'orchard-dark', name: 'Orchard Dark', panel: PANEL },
  Mascot,
  say: (topic) => word(TOPIC_LINES, topic),
  emoteColours: EMOTE_COLOURS,
});

export const { meta, lines, spans, render, bubble, say, emoteColours } = theme;
