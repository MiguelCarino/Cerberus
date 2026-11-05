// Unit tests for the chat's markdown parser (chatmd.js): the node tree for
// each construct, streaming leftovers, and XSS cases (raw HTML stays text,
// only http(s) links survive). Run: node --test web/test/*.test.mjs
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { parseMarkdown, parseInline, safeHref, plainText } from '../dist/assets/js/chatmd.js';

// Every string reachable in a tree, and every link href, for the XSS checks.
function walk(nodes, out = { hrefs: [], types: new Set() }) {
  for (const n of nodes || []) {
    if (typeof n === 'string' || !n) continue;
    out.types.add(n.type);
    if (n.type === 'link') out.hrefs.push(n.href);
    if (n.children) walk(n.children, out);
    if (n.items) for (const it of n.items) { walk(it.children, out); if (it.sub) walk([it.sub], out); }
    if (n.head) for (const c of n.head) walk(c, out);
    if (n.rows) for (const r of n.rows) for (const c of r) walk(c, out);
  }
  return out;
}

test('headings h1-h3 (deeper ones become h3), setext', () => {
  const t = parseMarkdown('# One\n## Two\n### Three\n#### Four\n\nTitle\n=====');
  assert.deepEqual(t.map((b) => [b.type, b.level]), [['heading', 1], ['heading', 2], ['heading', 3], ['heading', 3], ['heading', 1]]);
  assert.deepEqual(t[0].children, ['One']);
});

test('inline: bold, italic, strike, code, bold-italic, escapes, snake_case', () => {
  assert.deepEqual(parseInline('a **b** c'), ['a ', { type: 'strong', children: ['b'] }, ' c']);
  assert.deepEqual(parseInline('*i* _j_'), [{ type: 'em', children: ['i'] }, ' ', { type: 'em', children: ['j'] }]);
  assert.deepEqual(parseInline('~~x~~'), [{ type: 'del', children: ['x'] }]);
  assert.deepEqual(parseInline('`a*b*`'), [{ type: 'code', text: 'a*b*' }]);
  assert.deepEqual(parseInline('``a ` b``'), [{ type: 'code', text: 'a ` b' }]);
  assert.deepEqual(parseInline('***z***'), [{ type: 'strong', children: [{ type: 'em', children: ['z'] }] }]);
  assert.deepEqual(parseInline('\\*not\\*'), ['*not*']);
  assert.deepEqual(parseInline('my_var_name'), ['my_var_name']);
  assert.deepEqual(parseInline('2 * 3 * 4'), ['2 * 3 * 4']);
  assert.deepEqual(parseInline('**unclosed'), ['**unclosed']);
  assert.deepEqual(parseInline('`unclosed'), ['`unclosed']);
});

test('links: markdown, bare and <autolinks>, trailing punctuation trimmed', () => {
  assert.deepEqual(parseInline('[site](https://carino.systems/x)'), [{ type: 'link', href: 'https://carino.systems/x', children: ['site'] }]);
  assert.deepEqual(parseInline('see https://a.example/b.'), ['see ', { type: 'link', href: 'https://a.example/b', children: ['https://a.example/b'] }, '.']);
  assert.deepEqual(parseInline('(https://a.example/b)'), ['(', { type: 'link', href: 'https://a.example/b', children: ['https://a.example/b'] }, ')']);
  assert.deepEqual(parseInline('<http://h.example>'), [{ type: 'link', href: 'http://h.example/', children: ['http://h.example'] }]);
  assert.deepEqual(parseInline('[**b**](http://x.example)'), [{ type: 'link', href: 'http://x.example/', children: [{ type: 'strong', children: ['b'] }] }]);
});

test('XSS: raw HTML stays text, unsafe link targets are dropped', () => {
  const src = [
    '<script>alert(1)</script>',
    '<img src=x onerror=alert(1)>',
    '[click](javascript:alert(1))',
    '[x](JaVaScRiPt:alert(1))',
    '[d](data:text/html;base64,PHNjcmlwdD4=)',
    '[v](vbscript:msgbox)',
    '[f](file:///etc/passwd)',
    '[rel](/api/chats)',
    'javascript:alert(1)',
    '<a href="javascript:alert(1)">a</a>',
    '`<b>code</b>`',
    '```html\n<script>evil()</script>\n```',
    '| <i>x</i> | [t](javascript:1) |\n|---|---|\n| <svg onload=1> | ok |',
  ].join('\n\n');
  const tree = parseMarkdown(src);
  const w = walk(tree);
  assert.deepEqual(w.hrefs, [], 'no link survives');
  for (const t of w.types) assert.ok(['para', 'code', 'table', 'em', 'strong'].includes(t), `unexpected node ${t}`);
  const text = plainText(tree);
  assert.ok(text.includes('<script>alert(1)</script>'), 'kept as literal text');
  assert.ok(text.includes('<img src=x onerror=alert(1)>'));
  assert.ok(text.includes('click') && !text.includes('javascript:alert(1))'), 'label kept, target dropped');
  const code = tree.find((b) => b.type === 'code');
  assert.equal(code.text, '<script>evil()</script>');
  assert.equal(code.lang, 'html');
});

test('safeHref only lets http(s) through', () => {
  assert.equal(safeHref('https://x.example/a?b=1'), 'https://x.example/a?b=1');
  assert.equal(safeHref('HTTP://X.EXAMPLE'), 'http://x.example/');
  for (const bad of ['javascript:alert(1)', ' javascript:x', 'data:text/html,x', '//evil.example', '/rel', 'mailto:a@b', 'https://', '']) {
    assert.equal(safeHref(bad), null, bad);
  }
});

test('lists: bullets, numbered with start, tasks, nesting, continuation', () => {
  const [ul, ol] = parseMarkdown('- a\n- [x] done\n- [ ] todo\n  - nested\n  more text\n\n3. three\n4. four');
  assert.equal(ul.type, 'list');
  assert.equal(ul.ordered, false);
  assert.deepEqual(ul.items.map((i) => i.task), [null, true, false]);
  assert.deepEqual(ul.items[1].children, ['done']);
  assert.equal(ul.items[2].sub.items[0].children[0], 'nested');
  assert.equal(ol.ordered, true);
  assert.equal(ol.start, 3);
  assert.equal(ol.items.length, 2);
  const [cont] = parseMarkdown('- one\n  still one\n- two');
  assert.deepEqual(cont.items[0].children, ['one\nstill one']);
});

test('quotes, rules and paragraphs', () => {
  const t = parseMarkdown('> quoted **b**\n> more\n\n---\n\npara\nline two');
  assert.equal(t[0].type, 'quote');
  assert.equal(t[0].children[0].type, 'para');
  assert.equal(t[1].type, 'rule');
  assert.deepEqual(t[2], { type: 'para', children: ['para\nline two'] });
  assert.equal(parseMarkdown('***')[0].type, 'rule');
});

test('pipe tables: alignment, escaped pipes, code with pipes, ragged rows', () => {
  const [t] = parseMarkdown('| a | b | c |\n|:--|:-:|--:|\n| 1 | `x|y` | a\\|b |\n| only |');
  assert.equal(t.type, 'table');
  assert.deepEqual(t.align, ['left', 'center', 'right']);
  assert.deepEqual(t.head.map((c) => plainText(c)), ['a', 'b', 'c']);
  assert.deepEqual(t.rows[0][1], [{ type: 'code', text: 'x|y' }]);
  assert.deepEqual(t.rows[0][2], ['a|b']);
  assert.equal(t.rows[1].length, 3, 'padded to the header width');
  // Not a table without the separator row.
  assert.equal(parseMarkdown('a | b\nc | d')[0].type, 'para');
});

test('fenced code: language, tildes, unclosed while streaming', () => {
  const t = parseMarkdown('```go\nfunc main() {}\n```\n~~~\nplain\n~~~\n```py\nprint(1');
  assert.deepEqual(t.map((b) => [b.type, b.lang, b.open]), [['code', 'go', false], ['code', '', false], ['code', 'py', true]]);
  assert.equal(t[2].text, 'print(1');
  // A shorter fence does not close a longer one.
  const [c] = parseMarkdown('````\n```\ninner\n```\n````');
  assert.equal(c.text, '```\ninner\n```');
});

test('hard breaks and huge nesting stay bounded', () => {
  assert.deepEqual(parseInline('a  \nb'), ['a', { type: 'br' }, 'b']);
  const deep = '*'.repeat(2000) + 'x' + '*'.repeat(2000);
  assert.doesNotThrow(() => parseMarkdown(deep));
  const quotes = '>'.repeat(200) + ' x';
  assert.doesNotThrow(() => parseMarkdown(quotes));
});

test('plainText flattens blocks', () => {
  assert.equal(plainText(parseMarkdown('# T\n\n- a\n- b')), 'T\na\nb\n');
});
