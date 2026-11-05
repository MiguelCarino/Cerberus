// Pure helpers for Settings → Import & export (views/settingsio.js). The
// server side is internal/api/bundle.go; the bundle format is
// internal/config/bundle.go (kind "cerberus-profile").

export const BUNDLE_KIND = 'cerberus-profile';
export const BUNDLE_VERSION = 1;

// SECTIONS are the bundle sections in bundle order, with what each holds.
export const SECTIONS = [
  ['models', 'Models', 'Local models with their download links, and model entries'],
  ['agents', 'Agents', 'Agent profiles and the default agent'],
  ['repos', 'Repos', 'Repositories and repo folders'],
  ['providers', 'Providers', 'Cloud APIs (never their keys)'],
  ['prices', 'Prices', 'Prices per model'],
  ['budgets', 'Budgets', 'Spend caps'],
  ['tariff', 'Tariff', 'Electricity tariff'],
  ['monitor', 'Monitor', 'Monitor and energy'],
  ['limits', 'Limits', 'Session, guard and spawn limits'],
  ['sandbox', 'Sandbox', 'Sandbox settings'],
  ['policy', 'Policy', 'Agent tool policy'],
  ['files', 'Files', 'Files settings'],
];
const SECTION_IDS = SECTIONS.map(([id]) => id);

// exportURL is the download link for the chosen sections; every section
// (or none chosen) means all of them.
export function exportURL(sections = SECTION_IDS, secretNames = false) {
  const p = new URLSearchParams();
  const pick = SECTION_IDS.filter((s) => sections.includes(s));
  if (pick.length && pick.length < SECTION_IDS.length) p.set('sections', pick.join(','));
  if (secretNames) p.set('secret_names', '1');
  const q = p.toString();
  return `/api/config/export${q ? `?${q}` : ''}`;
}

// readBundle checks a file's text before it is sent: {bundle} or {error}.
export function readBundle(text) {
  let b;
  try {
    b = JSON.parse(String(text || ''));
  } catch {
    return { error: 'This file is not JSON. Choose a file saved with Export.' };
  }
  if (!b || typeof b !== 'object' || Array.isArray(b)) return { error: 'This file is not a Cerberus settings file.' };
  if (b.kind && b.kind !== BUNDLE_KIND) return { error: `This file is a "${String(b.kind).slice(0, 40)}", not Cerberus settings.` };
  if (!Number.isInteger(b.version) || b.version < 1) return { error: 'This file has no settings version; it was not saved with Export.' };
  if (b.version > BUNDLE_VERSION) return { error: `This file is from a newer Cerberus (format ${b.version}); update Cerberus first.` };
  return { bundle: b };
}

// sectionLabel names a section for people.
export const sectionLabel = (id) => (SECTIONS.find(([s]) => s === id) || [id, id])[1];

// valueText shows a setting's value on one line, cut at max characters.
export function valueText(v, max = 80) {
  if (v === undefined || v === null || v === '') return '(none)';
  let s;
  if (Array.isArray(v)) {
    if (!v.length) return '(none)';
    if (v.every((x) => x && typeof x === 'object' && (x.id || x.name))) {
      s = v.map((x) => x.id || x.name).join(', ');
      s = `${v.length} · ${s}`;
    } else s = JSON.stringify(v);
  } else s = typeof v === 'object' ? JSON.stringify(v) : String(v);
  return s.length > max ? `${s.slice(0, max - 1)}…` : s;
}

// changeRows turns the reply's changes into display rows.
export function changeRows(changes) {
  return (Array.isArray(changes) ? changes : []).map((c) => ({
    path: c.path,
    from: c.sensitive ? '(hidden)' : valueText(c.old),
    to: c.sensitive ? '(hidden)' : valueText(c.new),
    restart: !!c.restart,
  }));
}

// ENGINE maps a model backend to the downloads engine.
const ENGINE = { ollama: 'ollama', lmstudio: 'lmstudio', llamacpp: 'llamacpp' };

// downloadRequest is the POST /api/downloads body for a bundle model, or
// null when it cannot be downloaded from here.
export function downloadRequest(m) {
  if (!m || !m.ref || !ENGINE[m.backend]) return null;
  return { engine: ENGINE[m.backend], ref: m.ref };
}

// modelState is a bundle model's row state:
//   present   an engine lists it here
//   missing   it can be downloaded (and maybe dropped from the registry)
//   orphan    no download link is known; it can only be dropped
export function modelState(m) {
  if (m.present) return 'present';
  return downloadRequest(m) ? 'missing' : 'orphan';
}

// modelCounts counts the rows by state.
export function modelCounts(models) {
  const n = { total: 0, present: 0, missing: 0, orphan: 0, registered: 0 };
  for (const m of models || []) {
    n.total++;
    n[modelState(m)]++;
    if (!m.present && m.registered) n.registered++;
  }
  return n;
}

// sizeText is a model size for people ("4.7 GB", "" unknown).
export function sizeText(mib) {
  const n = Number(mib) || 0;
  if (n <= 0) return '';
  if (n >= 1024) return `${(n / 1024).toFixed(n >= 10240 ? 0 : 1)} GB`;
  return `${Math.round(n)} MB`;
}

// summaryText is one sentence on what an import holds and changes.
export function summaryText(reply) {
  const secs = (reply && reply.sections) || [];
  const ch = ((reply && reply.changes) || []).length;
  const what = secs.length ? secs.map(sectionLabel).join(', ') : 'nothing';
  if (!ch) return `Holds ${what}. Your settings already match it.`;
  return `Holds ${what}. ${ch === 1 ? '1 setting changes' : `${ch} settings change`}.`;
}

// dropModels removes ids from a model list (after Remove or Skip).
export const dropModels = (models, ids) => (models || []).filter((m) => !ids.includes(m.id));

// markRegistry clears registered for ids removed from the registry.
export const markRegistry = (models, ids) => (models || []).map((m) => (ids.includes(m.id) ? { ...m, registered: false } : m));

// refreshModels takes present/registered from a fresh reply for the rows
// still listed, keeping their order; rows the person removed stay gone.
export function refreshModels(rows, fresh) {
  const by = new Map((fresh || []).map((m) => [m.id, m]));
  return (rows || []).map((m) => (by.has(m.id) ? { ...m, present: !!by.get(m.id).present, registered: !!by.get(m.id).registered } : m));
}
