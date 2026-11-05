// Permission cards for the LLM chat: "🔒 The model wants to use X" with the
// command or path, an edit's lines, warnings, and Allow once / Allow for
// this session / Always allow ▾ / Deny (with an optional note that reaches
// the model). "Always allow ▾" lists the rules the server offers for the
// request (req.always) and saves the one picked for good (decision
// "always" + option). Buttons arm after 600 ms; allowing a command once or
// for the session takes two presses (400 ms – 5 s apart), picking from the
// Always menu is already a second step. An answered card collapses to one
// line.

import { h } from '../dom.js';
import { permPress, isCommandAsk, askHeadline, askDetails, answeredLine } from '../chat.js';
import { chatMenuItems } from '../permits.js';
import { openMenu } from './alwaysmenu.js';

// permCard builds a card for req. answer(decision, note, option) sends the
// answer and resolves true when the server took it. Returns {el, req,
// press(d), answered(ev)}; press is what Alt+A/S/F/D call (F opens the
// Always menu).
export function permCard(req, answer) {
  let card = { shownAt: performance.now(), command: isCommandAsk(req), pending: null };
  let done = false;
  let sending = false;
  const alwaysItems = req.forced ? [] : chatMenuItems(req.always);
  const note = h('input', { class: 'input pc-note', type: 'text', placeholder: 'Note for the model (optional)', 'aria-label': 'Note for the model', maxlength: '500' });
  const hint = h('div', { class: 'pc-hint', 'aria-live': 'polite' });
  const btnOnce = h('button', { class: 'btn btn-primary', type: 'button', onclick: () => press('once') }, h('span', { text: 'Allow once' }), h('kbd', { class: 'pc-k', text: 'Alt+A' }));
  const btnSess = h('button', { class: 'btn', type: 'button', onclick: () => press('session') }, h('span', { text: 'Allow for this session' }), h('kbd', { class: 'pc-k', text: 'Alt+S' }));
  const btnAlways = alwaysItems.length ? h('button', {
    class: 'btn', type: 'button', 'aria-haspopup': 'menu', 'aria-expanded': 'false',
    title: 'Save a rule: it runs without asking from now on, in every chat and session',
    onclick: (e) => { e.stopPropagation(); press('always'); },
  }, h('span', { text: 'Always allow ▾' }), h('kbd', { class: 'pc-k', text: 'Alt+F' })) : null;
  const btnDeny = h('button', { class: 'btn btn-danger', type: 'button', onclick: () => press('deny') }, h('span', { text: 'Deny' }), h('kbd', { class: 'pc-k', text: 'Alt+D' }));
  note.addEventListener('keydown', (e) => { if (e.key === 'Enter') { e.preventDefault(); press('deny'); } });
  const buttons = [btnOnce, btnSess, btnAlways, btnDeny].filter(Boolean);
  for (const b of buttons) b.classList.add('pc-arming');
  // The arming delay is visual too; one timer, never repeated.
  const armTimer = setTimeout(() => { for (const b of buttons) b.classList.remove('pc-arming'); }, 600);

  const head = askHeadline(req);
  const details = askDetails(req);
  const warn = [req.blocked, req.reason].filter(Boolean);
  const body = h('div', { class: 'pc-body' },
    h('div', { class: 'pc-title' }, h('span', { 'aria-hidden': 'true', text: '🔒 ' }), `The model wants to use ${req.tool || req.display || 'a tool'}`, req.agent ? h('span', { class: 'pc-agent', text: ` · ${req.agent}` }) : null),
    head ? h('pre', { class: 'pc-head', text: head }) : null,
    details.length ? h('div', { class: 'pc-lines' }, details.map(([k, l]) => h('div', { class: `pc-l ${k}`, text: `${k === 'add' ? '+ ' : k === 'del' ? '- ' : ''}${l}` }))) : null,
    warn.map((w) => h('div', { class: 'pc-warn', text: `⚠ ${w}` })),
    req.forced ? h('div', { class: 'pc-warn', text: '⚠ The command policy always asks for this one: session rules and saved permissions do not cover it.' }) : null,
    h('div', { class: 'pc-actions' }, btnOnce, req.forced ? null : btnSess, btnAlways, btnDeny),
    note, hint);
  const line = h('div', { class: 'pc-line', hidden: true });
  const el = h('div', { class: 'cm-row' }, h('div', { class: 'pc', role: 'group', 'aria-label': 'Permission request' }, body, line));

  async function send(decision, option) {
    sending = true;
    for (const b of buttons) b.disabled = true;
    const ok = await answer(decision, decision === 'deny' ? note.value.trim() : '', option);
    sending = false;
    if (!ok && !done) for (const b of buttons) b.disabled = false;
  }

  async function press(decision) {
    if (done || sending) return;
    if (decision === 'always' && !btnAlways) return;
    const r = permPress(card, decision, performance.now());
    card = r.card;
    if (r.action === 'ignore') return;
    if (decision === 'always') {
      openMenu(btnAlways, alwaysItems, (it) => { if (!done && !sending) send('always', it.option); });
      return;
    }
    if (r.action === 'confirm') {
      hint.textContent = 'Press again to run this command (within 5 s).';
      const b = decision === 'session' ? btnSess : btnOnce;
      b.classList.add('pc-confirm');
      setTimeout(() => { b.classList.remove('pc-confirm'); if (!done && card.pending) hint.textContent = ''; }, 5000);
      return;
    }
    await send(decision);
  }

  return {
    el,
    req,
    press,
    isOpen: () => !done,
    // answered collapses the card to its one line (from the "answered" event).
    answered(ev) {
      if (done) return;
      done = true;
      clearTimeout(armTimer);
      body.hidden = true;
      line.hidden = false;
      line.textContent = answeredLine(ev, req);
      line.classList.add(ev.decision === 'deny' ? 'deny' : 'allow');
    },
  };
}
