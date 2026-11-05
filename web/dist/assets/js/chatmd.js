// Markdown for the LLM chat, parsed into a plain node tree (no DOM, no
// HTML): views/chatdom.js turns the tree into elements with textContent
// only, so model text can never become markup. Raw HTML in the source stays
// literal text; links are kept only for http(s) URLs.
//
// Blocks:  {type: 'heading', level 1-3, children}
//          {type: 'para', children}
//          {type: 'code', lang, text, open}     (open: the fence is not closed yet, streaming)
//          {type: 'list', ordered, start, items: [{task: null|true|false, children, sub: list|null}]}
//          {type: 'quote', children: blocks}
//          {type: 'rule'}
//          {type: 'table', align: ['left'|'center'|'right'|null], head: [inline[]], rows: [[inline[]]]}
// Inline:  a string (text), {type: 'strong'|'em'|'del', children}, {type: 'code', text},
//          {type: 'link', href, children}, {type: 'br'}
//
// A port of the subset Atrium's AssistantBlock renders (h1-h3, bold, italic,
// strike, code spans, links, bullets, numbered and task lists, quotes, rules,
// pipe tables, fenced code), tolerant of half-written input while a reply
// streams in.

// safeHref returns the URL when it is http(s), else null.
export function safeHref(url) {
  const s = String(url || '').trim();
  if (!/^https?:\/\//i.test(s)) return null;
  try {
    const u = new URL(s);
    if (u.protocol !== 'http:' && u.protocol !== 'https:') return null;
    return u.href;
  } catch {
    return null;
  }
}

const FENCE = /^ {0,3}(`{3,}|~{3,})\s*([^\s`]*)[^`]*$/;
const HEADING = /^ {0,3}(#{1,6})\s+(.*?)\s*#*\s*$/;
const RULE = /^ {0,3}([-*_])(?:\s*\1){2,}\s*$/;
const BULLET = /^(\s*)([-*+])\s+(.*)$/;
const ORDERED = /^(\s*)(\d{1,9})[.)]\s+(.*)$/;
const QUOTE = /^ {0,3}>\s?(.*)$/;
const TASK = /^\[([ xX])\]\s+(.*)$/;

const isBlank = (l) => /^\s*$/.test(l);

// parseMarkdown parses src into blocks.
export function parseMarkdown(src) {
  const lines = String(src ?? '').replace(/\r\n?/g, '\n').split('\n');
  return parseBlocks(lines, 0);
}

function parseBlocks(lines, depth) {
  const out = [];
  let i = 0;
  let para = [];
  const flush = () => {
    if (para.length) out.push({ type: 'para', children: parseInline(para.join('\n')) });
    para = [];
  };
  while (i < lines.length) {
    const line = lines[i];
    let m;
    if (isBlank(line)) { flush(); i++; continue; }
    if ((m = FENCE.exec(line))) {
      flush();
      const fence = m[1];
      const lang = m[2] || '';
      const body = [];
      i++;
      let open = true;
      while (i < lines.length) {
        const l = lines[i];
        const close = new RegExp(`^ {0,3}${fence[0] === '`' ? '`' : '~'}{${fence.length},}\\s*$`);
        if (close.test(l)) { open = false; i++; break; }
        body.push(l);
        i++;
      }
      out.push({ type: 'code', lang, text: body.join('\n'), open });
      continue;
    }
    if ((m = HEADING.exec(line))) {
      flush();
      // h4-h6 render as h3: the chat has three heading sizes.
      out.push({ type: 'heading', level: Math.min(3, m[1].length), children: parseInline(m[2]) });
      i++;
      continue;
    }
    if (RULE.test(line) && !(para.length && /^\s*-+\s*$/.test(line))) {
      flush();
      out.push({ type: 'rule' });
      i++;
      continue;
    }
    if (para.length && /^\s*(=+|-+)\s*$/.test(line)) {
      // A setext heading underline.
      const text = para.join('\n');
      para = [];
      out.push({ type: 'heading', level: line.includes('=') ? 1 : 2, children: parseInline(text) });
      i++;
      continue;
    }
    if (QUOTE.test(line) && depth < 8) {
      flush();
      const inner = [];
      while (i < lines.length && (m = QUOTE.exec(lines[i]))) { inner.push(m[1]); i++; }
      out.push({ type: 'quote', children: parseBlocks(inner, depth + 1) });
      continue;
    }
    if (isTableStart(lines, i)) {
      flush();
      const t = parseTable(lines, i);
      out.push(t.node);
      i = t.next;
      continue;
    }
    if (BULLET.test(line) || ORDERED.test(line)) {
      flush();
      const l = parseList(lines, i, depth);
      out.push(l.node);
      i = l.next;
      continue;
    }
    para.push(line.replace(/^\s+/, ''));
    i++;
  }
  flush();
  return out;
}

function indentOf(s) {
  let n = 0;
  for (const ch of s) {
    if (ch === ' ') n++;
    else if (ch === '\t') n += 4;
    else break;
  }
  return n;
}

function listMatch(line) {
  let m = BULLET.exec(line);
  if (m) return { indent: indentOf(m[1]), ordered: false, num: 0, text: m[3] };
  m = ORDERED.exec(line);
  if (m) return { indent: indentOf(m[1]), ordered: true, num: Number(m[2]), text: m[3] };
  return null;
}

function parseList(lines, start, depth) {
  const first = listMatch(lines[start]);
  const node = { type: 'list', ordered: first.ordered, start: first.ordered ? first.num : 1, items: [] };
  let i = start;
  while (i < lines.length) {
    const line = lines[i];
    if (isBlank(line)) {
      // A blank line ends the list unless the next line goes on with it.
      const nx = lines[i + 1];
      const nm = nx !== undefined ? listMatch(nx) : null;
      if (nm && nm.indent >= first.indent && (nm.indent > first.indent || nm.ordered === first.ordered)) { i++; continue; }
      break;
    }
    const m = listMatch(line);
    if (m && m.indent === first.indent && m.ordered === first.ordered) {
      let text = m.text;
      let task = null;
      const t = TASK.exec(text);
      if (t) { task = t[1] !== ' '; text = t[2]; }
      node.items.push({ task, text: [text], sub: null });
      i++;
      continue;
    }
    if (m && m.indent > first.indent && node.items.length && depth < 8) {
      const sub = parseList(lines, i, depth + 1);
      const item = node.items[node.items.length - 1];
      if (item.sub) item.sub.items.push(...sub.node.items);
      else item.sub = sub.node;
      i = sub.next;
      continue;
    }
    if (m) break; // a list of the other kind, or less indented
    if (indentOf(line) > first.indent && node.items.length) {
      // A continuation line of the last item.
      node.items[node.items.length - 1].text.push(line.trim());
      i++;
      continue;
    }
    break;
  }
  for (const it of node.items) {
    it.children = parseInline(it.text.join('\n'));
    delete it.text;
  }
  return { node, next: i };
}

// ---------- tables ----------

const SEP_CELL = /^\s*:?-+:?\s*$/;

function splitRow(line) {
  let s = line.trim();
  if (s.startsWith('|')) s = s.slice(1);
  if (s.endsWith('|') && !s.endsWith('\\|')) s = s.slice(0, -1);
  const cells = [];
  let cur = '';
  let code = false;
  for (let k = 0; k < s.length; k++) {
    const ch = s[k];
    if (ch === '\\' && s[k + 1] === '|') { cur += '|'; k++; continue; }
    if (ch === '`') code = !code;
    if (ch === '|' && !code) { cells.push(cur.trim()); cur = ''; continue; }
    cur += ch;
  }
  cells.push(cur.trim());
  return cells;
}

function isTableStart(lines, i) {
  const head = lines[i];
  const sep = lines[i + 1];
  if (!head || sep === undefined || !head.includes('|')) return false;
  const cells = splitRow(sep);
  if (!sep.includes('-') || !cells.length || !cells.every((c) => SEP_CELL.test(c))) return false;
  return splitRow(head).length === cells.length || sep.includes('|');
}

function parseTable(lines, i) {
  const head = splitRow(lines[i]);
  const align = splitRow(lines[i + 1]).map((c) => {
    const l = c.startsWith(':');
    const r = c.endsWith(':');
    return l && r ? 'center' : r ? 'right' : l ? 'left' : null;
  });
  const n = Math.max(head.length, align.length);
  const pad = (row) => { const r = row.slice(0, n); while (r.length < n) r.push(''); return r; };
  const rows = [];
  let k = i + 2;
  while (k < lines.length && !isBlank(lines[k]) && lines[k].includes('|')) {
    rows.push(pad(splitRow(lines[k])).map(parseInline));
    k++;
  }
  while (align.length < n) align.push(null);
  return { node: { type: 'table', align: align.slice(0, n), head: pad(head).map(parseInline), rows }, next: k };
}

// ---------- inline ----------

const PUNCT = /[!"#$%&'()*+,\-./:;<=>?@[\\\]^_`{|}~]/;
const WORD = /[\p{L}\p{N}]/u;
const URL_RE = /^https?:\/\/[^\s<>"'`]+/i;

// parseInline parses one block's text into inline nodes.
export function parseInline(src, depth = 0) {
  const s = String(src ?? '');
  const out = [];
  let text = '';
  const push = (node) => {
    if (text) { out.push(text); text = ''; }
    out.push(node);
  };
  let i = 0;
  while (i < s.length) {
    const ch = s[i];
    // Escapes: a backslash before punctuation is the punctuation itself.
    if (ch === '\\' && i + 1 < s.length && PUNCT.test(s[i + 1])) { text += s[i + 1]; i += 2; continue; }
    if (ch === '\\' && s[i + 1] === '\n') { push({ type: 'br' }); i += 2; continue; }
    if (ch === '\n') {
      // Two trailing spaces make a hard break; otherwise a soft one.
      if (text.endsWith('  ')) { text = text.replace(/ +$/, ''); push({ type: 'br' }); } else { text += '\n'; }
      i++;
      continue;
    }
    if (ch === '`') {
      let run = 1;
      while (s[i + run] === '`') run++;
      const fence = '`'.repeat(run);
      const end = findRun(s, fence, i + run);
      if (end >= 0) {
        let code = s.slice(i + run, end).replace(/\n/g, ' ');
        if (code.length > 2 && code.startsWith(' ') && code.endsWith(' ') && code.trim()) code = code.slice(1, -1);
        push({ type: 'code', text: code });
        i = end + run;
        continue;
      }
      text += fence;
      i += run;
      continue;
    }
    if (depth < 6 && (ch === '*' || ch === '_' || ch === '~')) {
      const d = delimAt(s, i, depth);
      if (d) { push(d.node); i = d.next; continue; }
    }
    if (ch === '[' && depth < 6) {
      const l = linkAt(s, i, depth);
      if (l && l.plain) {
        // Not an http(s) link: its label as plain text, the target dropped.
        for (const n of l.plain) { if (typeof n === 'string') text += n; else push(n); }
        i = l.next;
        continue;
      }
      if (l) { push(l.node); i = l.next; continue; }
    }
    if ((ch === 'h' || ch === 'H') && (i === 0 || !WORD.test(s[i - 1]))) {
      const m = URL_RE.exec(s.slice(i));
      if (m) {
        const url = trimUrl(m[0]);
        const href = safeHref(url);
        if (href) { push({ type: 'link', href, children: [url] }); i += url.length; continue; }
      }
    }
    if (ch === '<') {
      // <https://…> autolinks; any other tag stays literal text.
      const m = /^<(https?:\/\/[^\s<>]+)>/i.exec(s.slice(i));
      const href = m && safeHref(m[1]);
      if (href) { push({ type: 'link', href, children: [m[1]] }); i += m[0].length; continue; }
    }
    text += ch;
    i++;
  }
  if (text) out.push(text);
  return out;
}

// findRun finds the next exact run of backticks equal to fence from i.
function findRun(s, fence, from) {
  let k = s.indexOf(fence, from);
  while (k >= 0) {
    const before = s[k - 1] === '`';
    const after = s[k + fence.length] === '`';
    if (!before && !after) return k;
    let e = k;
    while (s[e] === '`') e++;
    k = s.indexOf(fence, e);
  }
  return -1;
}

// trimUrl drops trailing punctuation a sentence put after a bare URL (and an
// unbalanced closing parenthesis).
function trimUrl(u) {
  let url = u;
  for (;;) {
    const last = url[url.length - 1];
    if (/[.,;:!?'"*_~]/.test(last)) { url = url.slice(0, -1); continue; }
    if (last === ')' && (url.match(/\(/g) || []).length < (url.match(/\)/g) || []).length) { url = url.slice(0, -1); continue; }
    break;
  }
  return url;
}

// delimAt parses an emphasis run at s[i]: ** __ (strong), * _ (em), ~~ (del).
function delimAt(s, i, depth) {
  const ch = s[i];
  let run = 1;
  while (s[i + run] === ch) run++;
  if (ch === '~') {
    if (run !== 2) return null;
    return span(s, i, '~~', 'del', null, depth);
  }
  // "_" inside a word (snake_case) is not emphasis.
  if (ch === '_' && i > 0 && WORD.test(s[i - 1])) return null;
  if (run >= 3) {
    const r = span(s, i, ch.repeat(3), 'strong', 'em', depth);
    if (r) return r;
  }
  if (run >= 2) {
    const r = span(s, i, ch + ch, 'strong', null, depth);
    if (r) return r;
  }
  return span(s, i, ch, 'em', null, depth);
}

function span(s, i, delim, type, innerType, depth) {
  const open = i + delim.length;
  if (open >= s.length || /\s/.test(s[open])) return null;
  let k = open;
  for (;;) {
    k = s.indexOf(delim, k);
    if (k < 0) return null;
    if (k === open) { k++; continue; }
    const prevSpace = /\s/.test(s[k - 1]);
    const next = s[k + delim.length];
    const sameRunContinues = next === delim[0] && delim.length < 3;
    const intraword = delim[0] === '_' && next !== undefined && WORD.test(next);
    // Skip a closer that is part of a longer run (** inside ***), escaped or
    // in the middle of a word for "_".
    if (prevSpace || s[k - 1] === '\\' || intraword || (sameRunContinues && delim.length === 1)) { k++; continue; }
    break;
  }
  const inner = s.slice(open, k);
  let children = parseInline(inner, depth + 1);
  if (innerType) children = [{ type: innerType, children }];
  return { node: { type, children }, next: k + delim.length };
}

function linkAt(s, i, depth) {
  // [text](url "title")
  let k = i + 1;
  let level = 1;
  while (k < s.length && level > 0) {
    if (s[k] === '\\') { k += 2; continue; }
    if (s[k] === '[') level++;
    else if (s[k] === ']') level--;
    if (level > 0) k++;
  }
  if (level !== 0 || s[k + 1] !== '(') return null;
  const label = s.slice(i + 1, k);
  const m = /^\(\s*<?([^\s()<>]*(?:\([^\s()]*\)[^\s()<>]*)*)>?(?:\s+("[^"]*"|'[^']*'))?\s*\)/.exec(s.slice(k + 1));
  if (!m) return null;
  const next = k + 1 + m[0].length;
  const href = safeHref(m[1]);
  const children = parseInline(label, depth + 1);
  if (!href) return { plain: children, next };
  return { node: { type: 'link', href, children: children.length ? children : [href] }, next };
}

// plainText flattens inline or block nodes to their text (copy, find, tests).
export function plainText(nodes) {
  let out = '';
  for (const n of nodes || []) {
    if (typeof n === 'string') out += n;
    else if (n.type === 'br') out += '\n';
    else if (n.type === 'code' && typeof n.text === 'string') out += n.text;
    else if (n.type === 'list') out += n.items.map((it) => plainText(it.children) + (it.sub ? '\n' + plainText([it.sub]) : '')).join('\n');
    else if (n.type === 'table') out += [n.head, ...n.rows].map((r) => r.map((c) => plainText(c)).join(' | ')).join('\n');
    else if (n.type === 'rule') out += '---';
    else if (n.children) out += plainText(n.children);
    if (n && typeof n === 'object' && ['para', 'heading', 'code', 'quote', 'list', 'table', 'rule'].includes(n.type)) out += '\n';
  }
  return out;
}
