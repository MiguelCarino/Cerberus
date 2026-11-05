// Pure helpers for Settings → Fine-tuning (views/finetune.js): capture
// modes, the counts, list queries and paging, rating states, previews,
// the trainer summary and run states. No DOM, so node --test covers them.
// The JSON contract is at the top of internal/api/finetune.go.

import { chatIdOk } from './chat.js';

// CAPTURE_MODES are finetune.capture's values, each with one plain
// sentence on what it keeps.
export const CAPTURE_MODES = [
  { id: 'off', label: 'Off', text: 'Nothing new is kept. Examples already collected stay until you delete the folder.' },
  { id: 'delegated', label: 'Delegated', text: 'Keeps each job an agent hands to a local model, its answer and the agent’s rating.' },
  { id: 'cloud', label: 'Cloud too', text: 'Also keeps every cloud chat answer that goes through the gateway. Examples contain prompts and answers.' },
];

// captureMode normalises a capture value ("" and unknown values read as off).
export function captureMode(v) {
  return CAPTURE_MODES.some((m) => m.id === v) ? v : 'off';
}

// captureText is the sentence for a mode.
export function captureText(v) {
  return CAPTURE_MODES.find((m) => m.id === captureMode(v)).text;
}

// countTiles turns GET /api/finetune's stats into labelled numbers.
// Excluded shows only when some are.
export function countTiles(stats) {
  const s = stats || {};
  const n = (k) => Math.max(0, Number(s[k]) || 0);
  const tiles = [
    { key: 'examples', label: 'Examples', value: n('examples') },
    { key: 'rated', label: 'Rated', value: n('rated') },
    { key: 'good', label: 'Good', value: n('good') },
    { key: 'corrected', label: 'Corrected', value: n('corrected') },
    { key: 'usable', label: 'Ready to train', value: n('usable') },
  ];
  if (n('excluded')) tiles.push({ key: 'excluded', label: 'Excluded', value: n('excluded') });
  return tiles;
}

// KINDS and RATED are the list filters ('' = any).
export const KINDS = [['', 'Any kind'], ['delegated', 'Delegated'], ['cloud', 'Cloud']];
export const RATED = [['', 'Any rating'], ['unrated', 'Not rated'], ['good', 'Good'], ['bad', 'Bad'], ['corrected', 'Corrected']];

export const PAGE_SIZE = 20;

// examplesURL builds GET /api/finetune/examples for filters f
// {kind, rated, q} and a page cursor (before = the previous page's next).
export function examplesURL(f = {}, before = '', limit = PAGE_SIZE) {
  const p = new URLSearchParams();
  if (KINDS.some(([k]) => k && k === f.kind)) p.set('kind', f.kind);
  if (RATED.some(([k]) => k && k === f.rated)) p.set('rated', f.rated);
  const q = String(f.q || '').trim();
  if (q) p.set('q', q);
  p.set('limit', String(limit));
  if (before) p.set('before', before);
  return `/api/finetune/examples?${p}`;
}

// exportURL is the JSONL download, optionally with unrated delegated rows
// or one kind.
export function exportURL({ unrated = false, kind = '' } = {}) {
  const p = new URLSearchParams();
  if (unrated) p.set('unrated', '1');
  if (kind === 'delegated' || kind === 'cloud') p.set('kind', kind);
  const s = p.toString();
  return `/api/finetune/export${s ? `?${s}` : ''}`;
}

// normPage reads a list reply defensively.
export function normPage(raw) {
  const r = raw && typeof raw === 'object' ? raw : {};
  return {
    examples: Array.isArray(r.examples) ? r.examples.filter((e) => e && e.id) : [],
    total: Math.max(0, Number(r.total) || 0),
    next: typeof r.next === 'string' ? r.next : '',
  };
}

// appendPage adds a page to the list, skipping ids already shown.
export function appendPage(list, page) {
  const seen = new Set(list.map((e) => e.id));
  return [...list, ...page.examples.filter((e) => !seen.has(e.id))];
}

// replaceExample swaps in a changed example (a rate or exclude reply) by id.
export function replaceExample(list, ex) {
  if (!ex || !ex.id) return list;
  return list.map((e) => (e.id === ex.id ? ex : e));
}

// ratingState: corrected (a better answer was written), good, bad or
// unrated, as the server's rated= filter reads it.
export function ratingState(ex) {
  const r = ex && ex.rating;
  if (!r) return 'unrated';
  if (r.better) return 'corrected';
  return r.good ? 'good' : 'bad';
}

const STATE_TEXT = { corrected: 'Corrected', good: 'Good', bad: 'Bad', unrated: 'Not rated' };
const STATE_TONE = { corrected: 'info', good: 'ok', bad: 'err', unrated: 'muted' };

// ratingBadge is the pill for an example: {text, tone, title}.
export function ratingBadge(ex) {
  const st = ratingState(ex);
  const by = ex && ex.rating ? (ex.rating.by === 'owner' ? 'you' : 'the agent') : '';
  return { state: st, text: STATE_TEXT[st], tone: STATE_TONE[st], title: by ? `${STATE_TEXT[st]}, by ${by}` : 'Nobody has rated this answer yet' };
}

// firstLine is the first non-blank line, collapsed and cut to max chars.
export function firstLine(text, max = 140) {
  const line = String(text || '').split('\n').map((l) => l.trim()).find(Boolean) || '';
  const flat = line.replace(/\s+/g, ' ');
  return flat.length > max ? `${flat.slice(0, max - 1).trimEnd()}…` : flat;
}

// promptOf is the example's prompt: its last user turn (else the last turn).
export function promptOf(ex) {
  const msgs = (ex && Array.isArray(ex.messages)) ? ex.messages : [];
  for (let i = msgs.length - 1; i >= 0; i--) if (msgs[i] && msgs[i].role === 'user') return String(msgs[i].content || '');
  return msgs.length ? String(msgs[msgs.length - 1].content || '') : '';
}

// previews: the prompt's first line and the answer's (the corrected
// answer when there is one).
export function previews(ex, max = 140) {
  const better = ex && ex.rating && ex.rating.better;
  return {
    prompt: firstLine(promptOf(ex), max) || '(empty prompt)',
    answer: firstLine(better || (ex && ex.answer), max) || '(empty answer)',
    corrected: !!better,
  };
}

// betterError checks a correction before it is sent ('' = fine).
export const BETTER_MAX = 256 * 1024;
export function betterError(text) {
  const t = String(text || '');
  if (!t.trim()) return 'Write the answer it should have given.';
  if (new Blob([t]).size > BETTER_MAX) return 'That is too long (256 KiB at most).';
  return '';
}

// shellJoin shows an argv as one command line (quoted where needed).
export function shellJoin(argv) {
  if (!Array.isArray(argv)) return '';
  return argv.map((a) => {
    const s = String(a);
    return s === '' ? "''" : /^[\w@%+=:,./{}-]+$/.test(s) ? s : `'${s.replace(/'/g, `'\\''`)}'`;
  }).join(' ');
}

// trainSummary reads finetune.* from GET /api/config's config: the
// command line, base model and whether Train now can work. usable is the
// rows a default export writes; unrated the extra delegated rows nobody
// rated (stats.usable_unrated − usable), which the "Also use delegated
// answers nobody rated" choice adds. When only unrated rows exist, Train
// now still works (onlyUnrated: the choice starts ticked and is needed).
export function trainSummary(ft, stats) {
  const f = ft || {};
  const cmd = Array.isArray(f.train_command) ? f.train_command.filter((a) => a !== '') : [];
  const usable = Math.max(0, Number(stats && stats.usable) || 0);
  const withUnrated = Math.max(usable, Number(stats && stats.usable_unrated) || 0);
  const unrated = withUnrated - usable;
  let blocked = '';
  if (!cmd.length) blocked = 'Set a train command first (Advanced → Train command).';
  else if (!usable && !unrated) blocked = 'No examples are ready yet: rate some answers Good or correct them.';
  return {
    command: shellJoin(cmd), base: String(f.base_model || ''), usable, unrated,
    onlyUnrated: !usable && unrated > 0, ready: !blocked, blocked,
  };
}

// trainRows is how many rows Train now exports, with or without the
// unrated delegated answers.
export function trainRows(t, withUnrated) {
  const s = t || {};
  return (Number(s.usable) || 0) + (withUnrated ? Number(s.unrated) || 0 : 0);
}

// trainText is the Train now dialog's sentence for the current choice
// ('' rows: nothing to export, Start is disabled).
export function trainText(t, withUnrated) {
  const n = trainRows(t, withUnrated);
  if (!n) return 'Nothing is rated yet: tick the box below to train on the delegated answers nobody rated.';
  const what = withUnrated && (Number(t && t.unrated) || 0) ? 'example' : 'ready example';
  return `Cerberus exports the ${n} ${what}${n === 1 ? '' : 's'} into a new run folder and starts the trainer as a session, so you can watch it, stop it and get notified when it ends.`;
}

// sourceLink is where an example came from: {href, label} for a chat or a
// session, null when it has none. Examples captured from a chat carry the
// chat's id as their session (the chat sends it as X-Cerberus-Session).
export function sourceLink(session) {
  const s = String(session || '');
  if (!s) return null;
  if (chatIdOk(s)) return { href: `#/chat/${encodeURIComponent(s)}`, label: 'Chat' };
  return { href: `#/s/${encodeURIComponent(s)}`, label: 'Session' };
}

// sessionID reads a session Info's id (Go field names, or lower case).
export const sessionID = (info) => String((info && (info.ID || info.id)) || '');

const RUN_TONE = { running: 'info', queued: 'info', waiting: 'warn', passed: 'ok', done: 'ok', failed: 'err', error: 'err', stopped: 'muted', removed: 'muted', cli: 'muted' };

// runBadge is the pill for a run's state.
export function runBadge(run) {
  const st = String((run && run.state) || '') || 'unknown';
  const text = st === 'cli' ? 'from the terminal' : st === 'removed' ? 'session deleted' : st;
  return { text, tone: RUN_TONE[st] || 'muted' };
}

// runLine summarises a run: "12 rows · base qwen3 · with unrated".
export function runLine(run) {
  const r = run || {};
  const n = Math.max(0, Number(r.rows) || 0);
  return [`${n} row${n === 1 ? '' : 's'}`, r.base ? `base ${r.base}` : 'default base', r.unrated ? 'with unrated' : ''].filter(Boolean).join(' · ');
}

// runHref links a run to its session, '' when it has none (CLI runs,
// deleted sessions).
export function runHref(run) {
  return run && run.session && run.state !== 'removed' ? `#/s/${encodeURIComponent(run.session)}` : '';
}

// emptyText says what to do next when the list is empty.
export function emptyText(capture, filtered, totalExamples) {
  if (filtered && totalExamples) return 'Nothing matches these filters. Clear them to see every example.';
  if (captureMode(capture) === 'off') return 'Collection is off. Choose Delegated above, then let an agent hand jobs to a local model.';
  return 'No examples yet. They appear when an agent hands a job to a local model (cerberus_ask).';
}

// roleLabel names a message's role for the reader.
export function roleLabel(role) {
  return { system: 'System', user: 'Prompt', assistant: 'Answer', tool: 'Tool' }[role] || String(role || 'Message');
}
