// Small DOM helpers: element builder, in-page dialogs and toasts. Text is
// always set with textContent, never innerHTML, since session output, task
// text and diffs are untrusted.

// h builds an element. attrs: class, text, on<Event> handlers, dataset via
// data-*, booleans (disabled, hidden...) and plain attributes.
export function h(tag, attrs = {}, ...children) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v == null || v === false) continue;
    if (k === 'class') el.className = v;
    else if (k === 'text') el.textContent = v;
    else if (k.startsWith('on') && typeof v === 'function') el.addEventListener(k.slice(2), v);
    else if (k === 'value') el.value = v;
    else if (v === true) el.setAttribute(k, '');
    else el.setAttribute(k, v);
  }
  append(el, children);
  return el;
}

function append(el, children) {
  for (const c of children) {
    if (c == null || c === false) continue;
    if (Array.isArray(c)) append(el, c);
    else el.append(c instanceof Node ? c : String(c));
  }
}

export function clear(el) { el.replaceChildren(); return el; }

// keepFocus runs rebuild (which replaces nodes inside scope) and, when
// focus was inside scope on an element with a data-fkey, focuses the new
// element with the same key: a live refresh must not drop keyboard focus
// to <body>.
export function keepFocus(scope, rebuild) {
  const a = document.activeElement;
  const key = a && a !== document.body && scope.contains(a) && a.dataset ? a.dataset.fkey || '' : '';
  rebuild();
  if (!key) return;
  for (const el of scope.querySelectorAll('[data-fkey]')) {
    if (el.dataset.fkey === key) { el.focus(); break; }
  }
}

// openDialog shows a Branding .modal in a native <dialog>. build(close)
// returns { title, body, foot } nodes. Resolves when closed, with the value
// passed to close().
export function openDialog(build) {
  return new Promise((resolve) => {
    const dlg = h('dialog', { class: 'modal' });
    let result;
    const close = (v) => { result = v; dlg.close(); };
    const { title, sub, body, foot, wide } = build(close);
    if (wide) dlg.classList.add('modal-wide');
    // append() (ours) skips null; the native one would print "null".
    append(dlg, [
      h('div', { class: 'modal-head' },
        h('div', {}, h('div', { class: 'modal-title', text: title }), sub ? h('div', { class: 'modal-sub', text: sub }) : null),
        h('button', { class: 'modal-x', type: 'button', 'aria-label': 'Close', onclick: () => close(undefined) }, '✕')),
      h('div', { class: 'modal-body' }, body),
      foot ? h('div', { class: 'modal-foot' }, foot) : null,
    ]);
    // A route change (back button, a link in the dialog) closes it, so a
    // modal never sits over a view it does not belong to. The hash it opened
    // on is kept: a hashchange queued just before it opened (set the hash,
    // then open a dialog for the new view) must not close it.
    const at = location.hash;
    const onRoute = () => { if (dlg.open && location.hash !== at) close(undefined); };
    window.addEventListener('hashchange', onRoute);
    dlg.addEventListener('close', () => { window.removeEventListener('hashchange', onRoute); dlg.remove(); resolve(result); });
    document.body.append(dlg);
    dlg.showModal();
  });
}

// confirmDialog is the in-page replacement for window.confirm.
export function confirmDialog({ title, message, ok = 'Confirm', danger = false }) {
  return openDialog((close) => ({
    title,
    body: h('p', { class: 'dlg-msg', text: message }),
    foot: [
      h('button', { class: 'btn btn-ghost', type: 'button', onclick: () => close(false) }, 'Cancel'),
      h('button', { class: `btn ${danger ? 'btn-danger' : 'btn-primary'}`, type: 'button', onclick: () => close(true) }, ok),
    ],
  })).then(Boolean);
}

let toastEl = null;
let toastTimer = 0;

// toast shows a short message; kind is ok | warn | err | info.
export function toast(message, kind = 'info') {
  if (!toastEl) {
    toastEl = h('div', { class: 'toast', role: 'status', 'aria-live': 'polite' });
    document.body.append(toastEl);
  }
  toastEl.className = `toast ${kind}`;
  toastEl.textContent = message;
  requestAnimationFrame(() => toastEl.classList.add('show'));
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => toastEl.classList.remove('show'), kind === 'err' ? 6000 : 3000);
}

// errText turns a thrown value into a message for a toast.
export const errText = (e) => (e && e.message) || String(e);

export function pill(text, tone) {
  return h('span', { class: `pill ${tone}`, text });
}
