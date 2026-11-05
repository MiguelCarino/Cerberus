// The LLM chat's transcript pieces as DOM: the markdown tree from
// chatmd.js turned into elements (textContent only: model text never
// becomes markup), assistant replies with copy-raw and the thinking fold,
// tool chips, notes, error cards and the end line. Permission cards live in
// chatperm.js.

import { h, toast, errText } from '../dom.js';
import { parseMarkdown, safeHref } from '../chatmd.js';
import { thinkingLabel, keepTail, toolSummary, excerpt, endLine } from '../chat.js';

// ---------- markdown ----------

function inline(nodes) {
  const out = [];
  for (const n of nodes || []) {
    if (typeof n === 'string') { out.push(document.createTextNode(n)); continue; }
    switch (n.type) {
      case 'strong': out.push(h('strong', {}, inline(n.children))); break;
      case 'em': out.push(h('em', {}, inline(n.children))); break;
      case 'del': out.push(h('del', {}, inline(n.children))); break;
      case 'code': out.push(h('code', { class: 'md-c', text: n.text })); break;
      case 'br': out.push(h('br')); break;
      case 'link': {
        // The tree only holds http(s) links; check again at the edge anyway.
        const href = safeHref(n.href);
        out.push(href ? h('a', { href, target: '_blank', rel: 'noopener noreferrer' }, inline(n.children)) : h('span', {}, inline(n.children)));
        break;
      }
      default: if (n.children) out.push(...inline(n.children));
    }
  }
  return out;
}

// copyText copies s and says so on the button for 1.5 s.
// copyText puts s on the clipboard and says so on btn. The Clipboard API
// needs a secure page (HTTPS or localhost); over plain http (a tailnet
// address, say) a hidden textarea and execCommand('copy') still work.
export async function copyText(s, btn) {
  let ok = false;
  try {
    await navigator.clipboard.writeText(s);
    ok = true;
  } catch {
    ok = legacyCopy(s);
  }
  copied(btn, ok);
}

// copyLater copies text that is still being fetched (a session's log):
// the clipboard write starts inside the click, as Safari requires, and
// waits for the text. Where that is not possible it waits, then copies.
export async function copyLater(textPromise, btn) {
  if (btn) btn.disabled = true;
  try {
    if (window.isSecureContext && navigator.clipboard && navigator.clipboard.write && typeof ClipboardItem !== 'undefined') {
      const blob = Promise.resolve(textPromise).then((t) => new Blob([t], { type: 'text/plain' }));
      try {
        await navigator.clipboard.write([new ClipboardItem({ 'text/plain': blob })]);
        copied(btn, true);
        return;
      } catch { /* refused, or the fetch failed: the await below says which */ }
    }
    await copyText(await textPromise, btn); // a fetch error lands in the catch
  } catch (e) {
    toast(errText(e), 'err');
  } finally {
    if (btn) btn.disabled = false;
  }
}

// downloadText saves text as a file named name (a plain-text log).
export function downloadText(name, text) {
  const url = URL.createObjectURL(new Blob([text], { type: 'text/plain;charset=utf-8' }));
  const a = h('a', { href: url, download: name, hidden: true });
  document.body.append(a);
  a.click();
  a.remove();
  setTimeout(() => URL.revokeObjectURL(url), 1000);
}

function legacyCopy(s) {
  const ta = h('textarea', { readonly: true, 'aria-hidden': 'true', style: 'position:fixed;top:0;left:0;width:1px;height:1px;opacity:0' });
  ta.value = s;
  const was = document.activeElement;
  document.body.append(ta);
  ta.select();
  let ok = false;
  try { ok = document.execCommand('copy'); } catch { ok = false; }
  ta.remove();
  if (was && typeof was.focus === 'function') was.focus({ preventScroll: true });
  return ok;
}

function copied(btn, ok) {
  if (!ok) {
    toast('Could not copy here: use Download instead', 'warn');
    return;
  }
  if (btn) {
    const was = btn.textContent;
    btn.textContent = '✓ copied';
    setTimeout(() => { btn.textContent = was; }, 1500);
  } else {
    toast('Copied', 'ok');
  }
}

function codeBlock(n) {
  const copy = h('button', { class: 'md-copy', type: 'button', 'aria-label': 'Copy code', onclick: (e) => copyText(n.text, e.currentTarget) }, '⧉ copy');
  return h('div', { class: `md-code${n.open ? ' open' : ''}` },
    h('div', { class: 'md-code-head' }, h('span', { class: 'md-lang', text: n.lang || 'text' }), copy),
    h('pre', {}, h('code', { text: n.text })));
}

function list(n) {
  const tag = n.ordered ? 'ol' : 'ul';
  const attrs = { class: 'md-list' };
  if (n.ordered && n.start !== 1) attrs.start = String(n.start);
  return h(tag, attrs, n.items.map((it) => {
    const box = it.task === null ? null : h('span', { class: `md-task${it.task ? ' on' : ''}`, 'aria-hidden': 'true', text: it.task ? '☑' : '☐' });
    const label = it.task === null ? null : h('span', { class: 'sr-only', text: it.task ? 'done: ' : 'to do: ' });
    return h('li', { class: it.task === null ? '' : 'md-taskitem' }, box, label, inline(it.children), it.sub ? list(it.sub) : null);
  }));
}

function table(n) {
  const cell = (tag, c, i) => h(tag, n.align[i] ? { style: `text-align:${n.align[i]}` } : {}, inline(c));
  return h('div', { class: 'md-table', tabindex: '0', role: 'region', 'aria-label': 'Table' },
    h('table', {},
      h('thead', {}, h('tr', {}, n.head.map((c, i) => cell('th', c, i)))),
      h('tbody', {}, n.rows.map((r) => h('tr', {}, r.map((c, i) => cell('td', c, i)))))));
}

// blocksToDom builds the elements for parsed blocks.
export function blocksToDom(blocks) {
  return (blocks || []).map((b) => {
    switch (b.type) {
      case 'heading': return h(`h${b.level}`, { class: 'md-h' }, inline(b.children));
      case 'para': return h('p', {}, inline(b.children));
      case 'code': return codeBlock(b);
      case 'list': return list(b);
      case 'quote': return h('blockquote', {}, blocksToDom(b.children));
      case 'rule': return h('hr');
      case 'table': return table(b);
      default: return null;
    }
  }).filter(Boolean);
}

// markdown renders src into a fresh .md element.
export function markdown(src) {
  return h('div', { class: 'md' }, blocksToDom(parseMarkdown(src)));
}

// ---------- user bubble ----------

export function userBubble(text, { images = 0, queued = false } = {}) {
  const el = h('div', { class: `cm-user${queued ? ' queued' : ''}`, title: queued ? 'Queued: sent when the current run finishes' : null },
    images ? h('span', { class: 'cm-imgs', text: `🖼 ${images}` }) : null,
    h('span', { class: 'cm-user-text', text: text || (images ? '' : '…') }));
  return h('div', { class: 'cm-row cm-row-user' }, el);
}

// ---------- assistant reply ----------

// assistantBlock is a reply that grows while it streams: append(text) adds
// words (re-rendered at most every 60 ms), think(text) adds reasoning to
// the fold. raw() is the reply's markdown.
export function assistantBlock() {
  let raw = '';
  let reasoning = '';
  let timer = 0;
  const body = h('div', { class: 'md' });
  const foldLabel = h('summary', { text: thinkingLabel('') });
  const foldText = h('pre', { class: 'cm-think-text' });
  const fold = h('details', { class: 'cm-think', hidden: true }, foldLabel, foldText);
  const copy = h('button', { class: 'cm-copy', type: 'button', title: 'Copy the reply as markdown', hidden: true, onclick: (e) => copyText(raw, e.currentTarget) }, '⧉');
  const el = h('div', { class: 'cm-row cm-row-bot' }, h('div', { class: 'cm-bot' }, fold, body, copy));

  const render = () => {
    timer = 0;
    body.replaceChildren(...blocksToDom(parseMarkdown(raw)));
    copy.hidden = !raw.trim();
  };
  return {
    el,
    append(t) {
      raw += t || '';
      if (!timer) timer = setTimeout(render, 60);
    },
    set(t) { raw = t || ''; render(); },
    think(t) {
      reasoning = keepTail(reasoning + (t || ''));
      fold.hidden = !reasoning.trim();
      foldLabel.textContent = thinkingLabel(reasoning);
      foldText.textContent = reasoning;
    },
    flush() { if (timer) { clearTimeout(timer); render(); } },
    raw: () => raw,
    empty: () => !raw.trim() && !reasoning.trim(),
  };
}

// ---------- tool chip ----------

// toolChip is "⚙ name · summary" with a spinner until done(output, isError)
// turns it into ✓ or ✗; the output excerpt opens by itself on an error. The
// spinner (◐◓◑◒) is a CSS animation: no timer, and browsers pause it in a
// hidden tab.
export function toolChip({ name, canon, input, home = '' }) {
  const shown = canon || name || 'tool';
  const mark = h('span', { class: 'tc-mark', 'aria-hidden': 'true' });
  const state = h('span', { class: 'sr-only', text: 'running' });
  const summary = toolSummary(canon || name, input || {}, home);
  const out = h('pre', { class: 'tc-out' });
  const more = h('div', { class: 'tc-more', hidden: true });
  const det = h('details', { class: 'tc' },
    h('summary', {}, mark, state, h('span', { class: 'tc-name', text: `⚙ ${shown}` }), summary ? h('span', { class: 'tc-sum', text: summary }) : null),
    out, more);
  let running = true;
  return {
    el: h('div', { class: 'cm-row cm-row-tool' }, det),
    running: () => running,
    done(output, isError) {
      running = false;
      det.classList.add(isError ? 'err' : 'ok');
      mark.textContent = isError ? '✗' : '✓';
      state.textContent = isError ? 'failed' : 'done';
      const ex = excerpt(output);
      out.textContent = ex.text || '(no output)';
      more.hidden = !ex.more;
      more.textContent = ex.more ? '… more output went to the model' : '';
      if (isError) det.open = true;
    },
  };
}

// ---------- notes, errors, end line ----------

export const noteRow = (text, tone = 'muted') => h('div', { class: `cm-row cm-note ${tone}`, text });

export function errorCard(text) {
  return h('div', { class: 'cm-row' }, h('div', { class: 'alert err cm-err', role: 'alert', text: text || 'Something went wrong' }));
}

// fixCard is a failed run whose cause has fixes (a model problem): a plain
// title, the engine's own words folded under it, and buttons. retire()
// keeps the text and drops the buttons once a newer message is sent.
// actions: [{label, primary, run}] (run may be async; buttons wait for it).
export function fixCard({ title, detail = '', actions = [] }) {
  const bar = h('div', { class: 'cm-fix-actions' });
  const buttons = actions.map((a) => {
    const b = h('button', { class: `btn btn-sm${a.primary ? ' btn-primary' : ''}`, type: 'button' }, a.label);
    b.addEventListener('click', async () => {
      for (const x of buttons) x.disabled = true;
      try { await a.run(); } finally { for (const x of buttons) x.disabled = false; }
    });
    return b;
  });
  bar.append(...buttons);
  bar.hidden = !buttons.length;
  const more = detail ? h('details', { class: 'cm-fix-detail' }, h('summary', { text: 'What the engine said' }), h('pre', { text: detail })) : null;
  const el = h('div', { class: 'cm-row' },
    h('div', { class: 'alert err cm-err cm-fix', role: 'alert' }, h('p', { class: 'cm-fix-title', text: '✗ ' + title }), more, bar));
  return { el, retire() { bar.remove(); } };
}

export function endRow(done, model) {
  const l = endLine(done, model);
  return h('div', { class: `cm-row cm-end ${l.tone}`, text: l.text });
}
