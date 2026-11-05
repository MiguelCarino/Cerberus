// Yes / Always ▾ / No for a session waiting at the built-in agent's
// approval prompt, answered without opening its terminal (the session
// list, the bell). "Always ▾" opens a menu: "This run: any <tool>" (the
// agent's own "a", forgotten when it finishes) and one "Every time: …" per
// rule the agent offered (WaitingAlways), which saves it for the session's
// owner (answer "save" + option). The answer goes to POST
// /api/sessions/{id}/answer with the prompt's number: a prompt already
// answered (here, in the terminal or by another page) is a 409, said in a
// toast. The buttons arm after 600 ms, so a tap meant for something that
// just moved is not taken as an answer.

import { apiResult, sid } from '../api.js';
import { h, toast, errText } from '../dom.js';
import { approvalWait } from '../util.js';
import { approvalMenuItems, ruleWords, normAlways } from '../permits.js';
import { openMenu } from './alwaysmenu.js';

const ARM_MS = 600;

// approvalButtons returns the buttons for session s (or null when it is
// not waiting for approval). onDone(word) runs after a taken answer.
export function approvalButtons(s, { onDone = null } = {}) {
  const w = approvalWait(s);
  if (!w) return null;
  const tool = w.tool || 'this tool';
  const yes = h('button', { class: 'btn btn-primary btn-sm', type: 'button', title: 'Run it this once (y)' }, 'Yes');
  const always = h('button', {
    class: 'btn btn-sm', type: 'button', 'aria-haspopup': 'menu', 'aria-expanded': 'false',
    title: `Allow ${tool} for this run, or save a rule so it never asks again`,
  }, 'Always ▾');
  const no = h('button', { class: 'btn btn-danger btn-sm', type: 'button', title: 'Refuse (n)' }, 'No');
  const buttons = [yes, always, no];
  const shown = performance.now();
  for (const b of buttons) b.classList.add('pc-arming');
  setTimeout(() => { for (const b of buttons) b.classList.remove('pc-arming'); }, ARM_MS);

  async function answer(word, label, option) {
    if (performance.now() - shown < ARM_MS) return;
    for (const b of buttons) b.disabled = true;
    try {
      const body = { answer: word, seq: w.seq };
      if (word === 'save') body.option = option;
      const res = await apiResult(`${sid(s.ID)}/answer`, { method: 'POST', body });
      if (res.ok) {
        const what = word === 'save' ? ruleWords(normAlways(w.always)[option]) : w.ask || 'approval answered';
        toast(`${label}: ${what}`, word === 'n' ? 'warn' : 'ok');
        if (onDone) onDone(word);
        return;
      }
      if (res.status === 409) toast('That prompt was already answered.', 'warn');
      else toast(res.message, 'err');
    } catch (e) {
      toast(errText(e), 'err');
    }
    for (const b of buttons) b.disabled = false;
  }
  yes.onclick = () => answer('y', 'Allowed once');
  always.onclick = (e) => {
    e.stopPropagation();
    if (performance.now() - shown < ARM_MS) return;
    openMenu(always, approvalMenuItems(w.tool, w.always), (it) => {
      if (it.kind === 'save') answer('save', 'Saved, runs without asking', it.option);
      else answer('a', 'Allowed for this run');
    });
  };
  no.onclick = () => answer('n', 'Refused');
  return h('div', { class: 'approve-btns approval-btns', role: 'group', 'aria-label': w.ask ? `Approve ${w.ask}?` : 'Approve?' }, buttons);
}

// approvalText is the line saying what is being approved.
export function approvalText(s) {
  const w = approvalWait(s);
  if (!w) return '';
  return w.ask ? `Approve? ${w.ask}` : 'Waiting for approval (open it to see what for)';
}
