// Model size hints for the flow editor (SCOPE "Flow quality (2026-11-16)").
// The pure mirror of internal/flows/modelsize.go: a parameter count read
// from a model's id, and whether a model is small for planning, judging
// and agent work. The server's validation gives the full warning; the
// editor uses these to mark small models in the plan and judge pickers.

const SMALL_PARAMS_B = 5; // below this many billion parameters a model is small
const SMALL_SIZE_MIB = 3500; // without a size in the name: a file below this is small

// "1.7b", "27b", "e4b", "8x7b" (the expert count left out), "360m", each
// between non-alphanumerics.
const PARAMS_RE = /(?:^|[^a-z0-9.])(?:\d+x)?e?(\d+(?:\.\d+)?)([bm])(?:$|[^a-z0-9])/;

// paramsB is the parameter count in billions written in a model id
// ("lfm3-1.7b" → 1.7, "gemma-4-E4B" → 4), or 0 when the id says none.
export function paramsB(id) {
  const m = PARAMS_RE.exec(String(id || '').toLowerCase());
  if (!m) return 0;
  let n = Number(m[1]);
  if (!Number.isFinite(n) || n <= 0) return 0;
  if (m[2] === 'm') n /= 1000;
  return n;
}

// isSmallModel takes a models-list entry ({ID, SizeMiB}) or an id.
export function isSmallModel(m) {
  const id = typeof m === 'string' ? m : (m && m.ID) || '';
  const p = paramsB(id);
  if (p > 0) return p < SMALL_PARAMS_B;
  const mib = (m && typeof m === 'object' && Number(m.SizeMiB)) || 0;
  return mib > 0 && mib < SMALL_SIZE_MIB;
}

// smallRole is the work a step's model field is for, when a small model
// is a poor fit for it ('' when any model will do).
export function smallRole(step, field) {
  if (!step || field !== 'model') return '';
  if (step.kind === 'plan') return 'planning';
  if (step.kind === 'judge') return 'judging';
  if (step.kind === 'session') return 'agent work';
  return '';
}

// pickerLabel is a model's label in a picker for that work: small models
// say so ("lfm3-1.7b · local · small for planning").
export function pickerLabel(label, m, role) {
  return role && isSmallModel(m) ? `${label} · small for ${role}` : label;
}
