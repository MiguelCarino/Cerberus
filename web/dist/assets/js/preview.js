// Work previews: pure helpers for the Preview tab (views/preview.js), the
// session rows and the wall tiles. No DOM here (web/test/preview.test.mjs).
//
// A preview is entered through the app: /preview/<id>/ for the static site
// and /preview/<id>/?src=<port> for a live server. The server checks the
// signed-in person, then redirects to the preview's own origin with a
// one-time capability. Links built here never carry a capability, so
// "Copy link" shares nothing that works without signing in.

// Device width presets: Phone and Tablet frame the page at that width;
// Desktop fills the panel.
export const PRESETS = [
  { key: 'phone', label: 'Phone', width: 390 },
  { key: 'tablet', label: 'Tablet', width: 768 },
  { key: 'desktop', label: 'Desktop', width: 0 },
];

// presetWidth is the iframe width in CSS pixels for a preset key, given
// the room the panel has: 0 (fill) for Desktop, unknown keys and when the
// preset is wider than the room.
export function presetWidth(key, room) {
  const p = PRESETS.find((x) => x.key === key);
  if (!p || !p.width) return 0;
  if (room > 0 && p.width >= room) return 0;
  return p.width;
}

// previewSources lists what the picker offers from GET
// /api/sessions/{id}/preview: the static site, then each live server.
// Each source is {key, label, url, kind}; url is app-relative.
export function previewSources(p) {
  const out = [];
  if (!p) return out;
  if (p.static && p.static.url) {
    const dir = p.static.dir && p.static.dir !== '.' ? ` (${p.static.dir}/)` : '';
    out.push({ key: 'static', label: `Static${dir}`, url: p.static.url, kind: 'static' });
  }
  for (const l of p.live || []) {
    const managed = l.source === 'managed';
    out.push({
      key: `live:${l.port}`,
      label: `${managed ? 'Run preview' : 'Live'} :${l.port}`,
      url: l.url || '',
      kind: managed ? 'managed' : 'live',
      port: l.port,
    });
  }
  return out;
}

// pickSource keeps the chosen source while it exists; else the managed
// server, then a live one, then the static site (a dev server shows the
// work in progress better than a stale build).
export function pickSource(sources, want) {
  if (!sources.length) return null;
  const keep = sources.find((s) => s.key === want);
  if (keep) return keep;
  return sources.find((s) => s.kind === 'managed') || sources.find((s) => s.kind === 'live') || sources[0];
}

// absoluteURL turns an app-relative entry path into a full link on the
// app's own origin (for Copy link).
export function absoluteURL(path, origin) {
  if (!path) return '';
  try { return new URL(path, origin).href; } catch { return ''; }
}

// previewChip is the session row / wall tile chip for a session Info:
// {url, label} or null when there is nothing to open. A live server wins
// over the static site; without a static site the first live port.
export function previewChip(info) {
  const p = info && info.Preview;
  if (!p) return null;
  const id = encodeURIComponent(info.ID);
  const live = (p.Live || [])[0];
  if (live) return { url: `/preview/${id}/?src=${live}`, label: `Preview :${live}` };
  if (p.Static) return { url: `/preview/${id}/`, label: 'Preview' };
  return null;
}

// managedTone maps the Run preview state to a pill tone.
export function managedTone(state) {
  switch (state) {
    case 'running': return 'ok';
    case 'starting': return 'info';
    case 'failed': return 'err';
    case 'exited': return 'warn';
    default: return 'muted';
  }
}

// managedRunning: Start would do nothing, Stop has something to stop.
export const managedRunning = (state) => state === 'starting' || state === 'running';

// emptyHint explains how a preview appears, for the empty state.
export function emptyHint(p) {
  const lines = ['Nothing to preview yet. A preview appears when:'];
  lines.push('the work has an index.html (in the folder root, public/, dist/, build/, out/, site/, _site/ or docs/);');
  if (p && p.detect) lines.push('the agent starts a dev server (it shows up here as Live :port);');
  if (p && p.managed && p.managed.command) lines.push(`you press Run preview, which runs: ${p.managed.command}`);
  else lines.push('you press Run preview with a preview command in .cerberus.json ({"preview":{"command":"… {port} …"}}) or a dev script in package.json.');
  if (p && !p.ports) lines.push('Live servers need server.preview_ports (Settings, Server).');
  return lines;
}
