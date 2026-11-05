/* mascot-stage.js: the live mascot on the product page.
   It draws the real Chat-view mascots (site/mascots/, byte copies of
   web/dist/assets/js/mascots/) into a grid of coloured spans. A theme module
   is loaded only when it is picked. Frames run at 10 per second only while
   the stage is on screen and the tab is visible; with reduced motion they run
   at a third of that and emotes hold still. No network requests beyond the
   theme files on this site. */
// The theme list matches mascots/index.js (a web test keeps them equal); the
// modules themselves load only when the stage comes near the screen.
const NAMES = {
  moe: 'Moe', matrix: 'Matrix', win95: 'Windows 95', aero: 'Aero', cyberpunk: 'Cyberpunk',
  lofi: 'Lo-fi', luna: 'Luna', orbit: 'Orbit', seasons: 'Seasons', orchard: 'Orchard',
  'orchard-dark': 'Orchard Dark',
};
const THEMES = Object.keys(NAMES);
let lib = null;
const mascots = () => (lib = lib || import('./mascots/index.js'));
let frameSpans = null;

const STATES = [
  { id: 'idle', label: 'Idle' },
  { id: 'thinking', label: 'Thinking' },
  { id: 'tool', label: 'Running a command', tool: 'Bash' },
  { id: 'writing', label: 'Writing' },
  { id: 'done', label: 'Done', mood: 'happy', emote: ['<3', 'love'] },
  { id: 'error', label: 'Error', mood: 'worried', emote: ['!', 'bad'] },
];

const root = document.getElementById('mascotStage');
if (root) boot(root);

function chip(label, pressed, onClick) {
  const b = document.createElement('button');
  b.type = 'button';
  b.className = 'ms-chip';
  b.textContent = label;
  b.setAttribute('aria-pressed', pressed ? 'true' : 'false');
  b.addEventListener('click', onClick);
  return b;
}

function boot(root) {
  const panel = root.querySelector('.ms-panel');
  const grid = root.querySelector('.ms-grid');
  const bubble = root.querySelector('.ms-bubble');
  const themesBar = root.querySelector('.ms-themes');
  const statesBar = root.querySelector('.ms-states');
  const calm = window.matchMedia('(prefers-reduced-motion: reduce)');

  let theme = null;
  let themeId = 'moe';
  let state = STATES[0];
  let tick = 0;
  let timer = 0;
  let inView = false;
  let cols = 60;
  let rows = 18;
  let token = 0;

  THEMES.forEach((id) => {
    themesBar.appendChild(chip(NAMES[id] || id, id === themeId, () => pickTheme(id)));
  });
  STATES.forEach((s) => {
    statesBar.appendChild(chip(s.label, s === state, () => pickState(s)));
  });

  function press(bar, label) {
    bar.querySelectorAll('.ms-chip').forEach((b) => b.setAttribute('aria-pressed', b.textContent === label ? 'true' : 'false'));
  }

  // fit picks the grid size and font size from the panel width: about 60
  // columns on a desktop, at least 40 on a phone, never past the edge.
  function fit() {
    const w = panel.clientWidth - 24;
    cols = w >= 560 ? 60 : Math.max(40, Math.floor(w / 7.2));
    rows = w >= 560 ? 18 : 15;
    const fs = Math.min(15, w / (cols * 0.602));
    grid.style.fontSize = fs.toFixed(2) + 'px';
  }

  function draw() {
    if (!theme || !frameSpans) return;
    let spans;
    try {
      spans = frameSpans(theme, {
        state: state.id, tick, tool: state.tool || null, mood: state.mood || null, scene: '',
        cols, rows, emote: state.emote ? [state.emote[0], state.emote[1], Math.min(tick / 10, 5)] : null, calm: calm.matches,
      });
    } catch (e) {
      return;
    }
    const frag = document.createDocumentFragment();
    for (const row of spans) {
      const line = document.createElement('div');
      for (const run of row) {
        const s = document.createElement('span');
        s.textContent = run[0];
        if (run[1]) s.style.color = run[1];
        if (run[2]) s.style.background = run[2];
        line.appendChild(s);
      }
      frag.appendChild(line);
    }
    grid.replaceChildren(frag);
  }

  function setBubble() {
    if (!theme) return;
    let text = '';
    try { text = theme.bubble(state.id, state.tool || null, state.mood || null) || ''; } catch (e) { text = ''; }
    bubble.textContent = text;
    bubble.hidden = !text;
    panel.setAttribute('aria-label', `${NAMES[themeId] || themeId} mascot, ${state.label.toLowerCase()}${text ? ': ' + text : ''}`);
  }

  function run() {
    const want = inView && !document.hidden && !!theme;
    if (want && !timer) {
      timer = setInterval(() => { tick += 1; draw(); }, 100);
    } else if (!want && timer) {
      clearInterval(timer);
      timer = 0;
    }
  }

  async function pickTheme(id) {
    themeId = id;
    press(themesBar, NAMES[id] || id);
    const mine = ++token;
    root.classList.add('loading');
    const m = await mascots();
    frameSpans = m.frameSpans;
    const t = await m.loadTheme(id);
    if (mine !== token) return;
    root.classList.remove('loading');
    theme = t;
    panel.style.background = (t.meta && t.meta.panel) || '#000';
    setBubble();
    draw();
    run();
  }

  function pickState(s) {
    state = s;
    tick = 0;
    press(statesBar, s.label);
    setBubble();
    draw();
  }

  new IntersectionObserver((entries) => {
    inView = entries.some((e) => e.isIntersecting);
    if (inView && !theme && !token) pickTheme(themeId);
    run();
  }, { rootMargin: '120px' }).observe(panel);
  document.addEventListener('visibilitychange', run);
  new ResizeObserver(() => { fit(); draw(); }).observe(panel);
  fit();
}
