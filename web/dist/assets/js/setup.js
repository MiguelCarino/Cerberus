// Setup snapshot helpers (docs/SCOPE.md "E3 Setup snapshot"): the session
// page's Setup disclosure flattens GET /api/sessions/{id}/setup and, when
// compared with another session, lists the fields that differ. The field
// names and order match session.Setup.Fields in Go.

const join = (v) => (Array.isArray(v) ? v.join(' ') : '');

// setupFields flattens a setup (the "setup" of a SetupView) into
// [{field, value}] in a fixed order.
export function setupFields(s) {
  const st = s || {};
  const a = st.agent || {};
  const m = st.model || {};
  const mcp = Array.isArray(st.mcp_servers) ? st.mcp_servers : [];
  return [
    ['agent.name', a.name || ''],
    ['agent.binary', a.binary || ''],
    ['agent.command', join(a.command)],
    ['agent.resume', String(!!a.resume)],
    ['agent.env_keys', Array.isArray(a.env_keys) ? a.env_keys.join(',') : ''],
    ['agent.version', a.version || ''],
    ['agent.interactive', String(!!a.interactive)],
    ['agent.sandbox', a.sandbox || ''],
    ['model.requested', m.requested || ''],
    ['model.id', m.id || ''],
    ['model.engine', m.engine || ''],
    ['model.path', m.path || ''],
    ['model.quantization', m.quantization || ''],
    ['model.context', m.context > 0 ? String(m.context) : ''],
    ['mcp_servers', mcp.map((x) => `${x.name}=${join(x.command)}`).join('; ')],
    ['workspace', st.workspace || ''],
    ['base_commit', st.base_commit || ''],
    ['cerberus_version', st.cerberus_version || ''],
  ].map(([field, value]) => ({ field, value }));
}

// shownFields are the non-empty fields, for the disclosure's list. The
// booleans that are false say nothing and are left out too.
export function shownFields(s) {
  return setupFields(s).filter((f) => f.value !== '' && f.value !== 'false');
}

// diffSetups lists [{field, a, b}] for every field that differs.
export function diffSetups(a, b) {
  const fa = setupFields(a);
  const fb = setupFields(b);
  return fa.map((f, i) => ({ field: f.field, a: f.value, b: fb[i].value })).filter((d) => d.a !== d.b);
}

// setupSummary is the disclosure's one-line label: the hash and, when the
// file no longer matches the hash recorded at run start, a warning.
export function setupSummary(view) {
  if (!view || !view.hash) return 'Setup';
  if (view.verified) return `Setup · ${view.hash}`;
  return view.recorded_hash ? `Setup · ${view.hash} (changed since the run started)` : `Setup · ${view.hash}`;
}

// cleanSessionId trims a pasted session id or #/s/<id> link; null when it
// is not one.
export function cleanSessionId(text) {
  let t = String(text || '').trim();
  const m = t.match(/#\/s\/([^/?#\s]+)/);
  if (m) t = m[1];
  return /^[A-Za-z0-9_-]{1,64}$/.test(t) ? t : null;
}
