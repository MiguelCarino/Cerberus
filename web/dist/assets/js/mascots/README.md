# Mascots

ES-module ports of Atrium's ASCII mascots (`~/Github/Atrium/home/.config/hypr/scripts/`), for the Chat
view's stage. Pure modules: no DOM, no network, so `node --test` imports them directly.

| File | Port of | What it holds |
|------|---------|---------------|
| `common.js` | `mascot_common.py`, parts of `mascots.py` | Canvas, LCG noise, `dw`, `propKind`, `blink`, `drawEmote`, `calmTick`, `BaseMascot`, `makeTheme`, `frameSpans`, Python arithmetic (`fd`, `mod`, `fmod`, `int`) |
| `eaw.js` | `unicodedata` | wide / ambiguous / combining ranges (generated: `web/test/fixtures/mascots/gen-eaw.py`) |
| `mood.js` | `mascot_mood.py` | MOODS, SCENES, EMOTES, DEFAULT_LINES, `word`, the sentiment scorer (`readText`), `toolTopic`, the `Mood` engine (reactions, priority, idle topics). Surroundings are the time of day only |
| `moe.js` | `moe_mascot.py` | the default theme: the girl with cat ears, with moods, scenes and bilingual bubbles |
| `standin.js` | `llm-chat.py` `_StandIn` | a centred kaomoji, used when no theme module loads |
| `index.js` | `mascots.py` registry | `THEMES`, `NAMES`, `loadTheme(id)` (lazy `import()`, falls back to moe, then to the stand-in) |

## The theme module API

Every theme is `./<id>.js` and exports:

```js
export const meta;          // {id, name, panel, moods}: panel is the stage background colour
export function lines(state, tick, tool, mood, scene, cols, rows);  // -> rows strings, each cols wide
export function spans(state, tick, tool, mood, scene, cols, rows);  // -> rows of runs
export function render(state, tick, tool, mood, scene, cols, rows); // -> {canvas, head}
export function bubble(state, tool, mood);                          // -> the speech line
export function say(topic);                                         // -> a mood topic in its voice
export const emoteColours;  // tone -> colour (accent warn bad good love dim)
```

- `state` is one of idle, thinking, tool, writing, done, error. `tool` is a tool name or null. `mood` is one
  of `mood.js` MOODS or null. `scene` is a Set, an array or `"tag,tag"` of SCENES.
- `spans` gives one array per row of runs `[text, colour]`, or `[text, colour, background]` when a cell has a
  background. Runs are merged per colour exactly as Atrium's `Canvas.markup()` merges them: a blank cell joins
  any run with the same background. `colour` is null for a run with nothing to colour.
- `head` is the emote anchor `[x, y]` (the cell above the right side of the head), or null.
  `frameSpans(theme, {state, tick, tool, mood, scene, cols, rows, emote, calm})` draws a reaction's emote
  there and applies reduce motion (the stage at a third of its speed, the emote steady).
- Frames are pure functions of their arguments: the same inputs give the same text and colours as the Python
  `mascots.py --frame` for that theme.

The easy way to write one: port the Python `Mascot` class to a JS class with `render(state, tick, tool, mood,
scene)` returning a `Canvas` (and setting `this.head` if it draws moods), then:

```js
import { BaseMascot, makeTheme } from './common.js';
import { defaultLine } from './mood.js';

class Mascot extends BaseMascot { render(state, tick, tool) { /* ... */ } }
Mascot.BUBBLES = { idle: '...', /* ... */ };
Mascot.TOOL_BUBBLES = { Bash: '...', /* ... */ };
Mascot.OTHER = 'using %s';

const theme = makeTheme({ meta: { id: 'matrix', name: 'Matrix', panel: PANEL }, Mascot, say: defaultLine,
  emoteColours: EMOTE_COLOURS });
export const { meta, lines, spans, render, bubble, say, emoteColours } = theme;
```

Port faithfully: keep Python's floor division and modulo (`fd`, `mod`, `fmod` for floats), truncating
`int()` (`int`), and the order of the `r()` draws. Walk strings by code point (`Array.from`), as Python does.
The theme id is already registered in `index.js`; creating the file is enough.

## Golden tests

Fixtures live in `web/test/fixtures/mascots/`, written by `gen.sh` from Atrium's Python (read-only: it runs
`python3 -I -B`, so nothing is written into Atrium). For a new theme port:

```sh
web/test/fixtures/mascots/gen.sh matrix      # writes matrix.frames.txt and matrix.markup.txt
```

- `frames.txt` is the shared frame spread (states, tools, ticks, sizes 48x14, 80x20 and 160x24, moods,
  scenes). Add `frames-THEME.txt` (same format) for frames only your theme needs.
- `THEME.frames.txt` holds the text and the bubble (`mascots.py --frame` output, verbatim);
  `THEME.markup.txt` the emote anchor and the Pango markup per row (the colours).
- Then add `web/test/mascots-THEME.test.mjs`:

  ```js
  import * as theme from '../dist/assets/js/mascots/matrix.js';
  import { goldenTests } from './fixtures/mascots/golden.mjs';
  goldenTests('matrix', theme);
  ```

  `goldenTests` asserts exact character equality for every frame, the same bubble, the same colour runs and
  the same head anchor.
- Keep each theme's fixtures under about 400 KB (moe: ~320 KB for 48 frames). Commit them with the port.
- Run `node --test web/test/*.test.mjs`.
- Per-theme folders (`fixtures/mascots/<theme>/`, read with `readFrames('<theme>/<theme>')`) are generated with
  `d=web/test/fixtures/mascots/<theme>; OUT=$d FRAMES=$d/frames.txt web/test/fixtures/mascots/gen.sh <theme>`; seasons adds
  `HYPR_SEASON=<season>` and names the files by season.
- A frame line must not ask for fewer rows than the theme's minimum (matrix 10, win95 14): Python clamps the
  rows while the header keeps the requested count, and the reader misparses the fixture.

## Theme extras

- win95, luna, orbit and seasons draw the time: `setClock(fn)` (a function returning a `Date`; `null` restores
  the real clock). Their tests set the clock per frame from the fixture's strip.
- seasons: `setSeason(s | null)` pins a season, `setHemisphere('north' | 'south')` picks the month table (north
  by default), plus `seasonFor`, `currentSeason`, `moonGlyph` and `almanacDate`.
- `icons.js` is not a theme: the 18×18 notification sprites per theme and mood (`calm`, `alert`, `relieved`),
  drawn by `drawIcon(ctx, theme, mood, scale)` or `iconDataURL(theme, mood)` (null without a canvas).
