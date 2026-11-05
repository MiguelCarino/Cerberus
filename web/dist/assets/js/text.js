// Pure text helpers: search-snippet highlighting, inserting dictated text at
// the cursor, and vault name/value checks. No DOM, so node --test imports
// them directly.

// queryWords splits a search query into distinct lower-case words, longest
// first so a regex alternation prefers the longer of two overlapping words.
export function queryWords(q) {
  const seen = new Set();
  for (const w of String(q || '').toLowerCase().split(/\s+/)) if (w) seen.add(w);
  return [...seen].sort((a, b) => b.length - a.length || a.localeCompare(b));
}

const escapeRe = (s) => s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');

// highlightParts cuts text into [{ text, mark }] runs, mark = true where a
// query word matches (case-insensitive). The view turns marked runs into
// <mark> elements holding text nodes, so snippets are never parsed as HTML.
export function highlightParts(text, words) {
  text = String(text ?? '');
  const ws = (words || []).filter(Boolean);
  if (!text) return [];
  if (!ws.length) return [{ text, mark: false }];
  const re = new RegExp(ws.map(escapeRe).join('|'), 'giu');
  const out = [];
  let last = 0;
  for (const m of text.matchAll(re)) {
    if (!m[0]) continue;
    if (m.index > last) out.push({ text: text.slice(last, m.index), mark: false });
    const prev = out[out.length - 1];
    // Adjacent matches ("foo" "bar" in "foobar") read better as one mark.
    if (prev && prev.mark && m.index === last) prev.text += m[0];
    else out.push({ text: m[0], mark: true });
    last = m.index + m[0].length;
  }
  if (last < text.length) out.push({ text: text.slice(last), mark: false });
  return out;
}

// insertAtCursor puts text into value at the selection [start, end),
// replacing what was selected, with a space on either side when it would
// otherwise run into a neighbouring word. Returns the new value and caret.
export function insertAtCursor(value, start, end, text) {
  value = String(value ?? '');
  text = String(text ?? '').trim();
  const len = value.length;
  start = Number.isInteger(start) ? Math.max(0, Math.min(start, len)) : len;
  end = Number.isInteger(end) ? Math.max(start, Math.min(end, len)) : start;
  if (!text) return { value, caret: end };
  const before = value.slice(0, start);
  const after = value.slice(end);
  const pre = before && !/\s$/.test(before) ? ' ' : '';
  const post = after && !/^\s/.test(after) ? ' ' : '';
  const ins = pre + text + post;
  return { value: before + ins + after, caret: before.length + pre.length + text.length };
}

// ---------- vault ----------

export const SECRET_NAME_RE = /^[A-Za-z0-9_.-]{1,64}$/;
export const SECRET_MAX_BYTES = 16 * 1024;

// secretError says what is wrong with a name and value before they are
// sent ('' when fine). The server checks the same rules.
export function secretError(name, value) {
  if (!SECRET_NAME_RE.test(name || '')) return 'Name: 1–64 letters, digits, dot, dash or underscore';
  if (!value) return 'Enter a value';
  if (new TextEncoder().encode(value).length > SECRET_MAX_BYTES) return 'Values are limited to 16 KiB';
  return '';
}
