// Terminal input shared by the session view and a focused wall tile: the
// xterm theme, the ordered keystroke sender, the quick-key row and the input
// bar (with the mic) that types into a live session or sends a follow-up.

import { post, sid } from '../api.js';
import { h, toast, errText } from '../dom.js';
import { isLive, takesFollowUp, termInputHint, QUICK_KEYS, quickKeyData } from '../util.js';
import { micControl } from './mic.js';
import { pickerButton, onPasteFiles } from './drop.js';

export const TERM_THEME = {
  background: '#050505', foreground: '#e5e5e5', cursor: '#eab308', cursorAccent: '#050505',
  selectionBackground: 'rgba(234,179,8,0.30)',
};

export const TERM_FONT = "'IBM Plex Mono', ui-monospace, monospace";

// makeSender serialises keystrokes into ordered POSTs: while one request is
// in flight, new input is buffered and sent as one batch afterwards.
export function makeSender(id) {
  let buf = '';
  let busy = false;
  const flush = async () => {
    if (busy || !buf) return;
    busy = true;
    const data = buf;
    buf = '';
    try {
      await post(`${sid(id)}/input`, { data });
    } catch (e) {
      toast(errText(e), 'err');
    } finally {
      busy = false;
      if (buf) flush();
    }
  };
  return (d) => { buf += d; flush(); };
}

// termInput builds the quick keys, mic note and input bar for one session.
// getInfo() returns the current Info; getTerm() the xterm (for the arrow
// keys' application mode). sync() refreshes enabled state and placeholder.
// onFiles(files, { fromPaste }), when given, adds an attach button and
// takes files pasted into the input (the session view uploads them).
export function termInput(id, { getInfo, getTerm, send = makeSender(id), onFiles = null }) {
  const input = h('input', { class: 'input ss-input', autocomplete: 'off', autocapitalize: 'off', spellcheck: 'false', enterkeyhint: 'send', 'aria-label': 'Input' });
  const keys = h('div', { class: 'qkeys' }, QUICK_KEYS.map(([label]) =>
    h('button', { class: 'qkey', type: 'button', onclick: () => sendKey(label) }, label)));
  const sendBtn = h('button', { class: 'btn btn-primary', type: 'submit' }, 'Send');
  const mic = micControl(input);
  const attach = onFiles ? pickerButton((files) => onFiles(files, { fromPaste: false }), { label: '', cls: 'btn ss-attach', title: 'Attach files' }) : null;
  if (onFiles) onPasteFiles(input, (files) => onFiles(files, { fromPaste: true }));
  const bar = h('form', { class: 'ss-inputbar', onsubmit: (e) => { e.preventDefault(); submit(); } }, attach, input, mic.el, sendBtn);
  const micNote = h('div', { class: 'ss-micnote' }, mic.note);

  function sendKey(label) {
    const info = getInfo();
    if (!info || !isLive(info.State)) return;
    // No term.focus() here: on a phone it would pop up the keyboard.
    let data = quickKeyData(label, info.WaitingReason);
    // Full-screen TUIs switch the arrows to application mode (ESC O A).
    if (getTerm()?.modes?.applicationCursorKeysMode) data = data.replace(/^\x1b\[([AB])$/, '\x1bO$1');
    send(data);
  }

  async function submit() {
    const info = getInfo();
    const text = input.value;
    if (!info) return;
    if (isLive(info.State)) {
      send(text + '\r');
      input.value = '';
    } else if (takesFollowUp(info) && text.trim()) {
      try {
        await post(`${sid(id)}/followup`, { text: text.trim() });
        input.value = '';
        toast('Follow-up sent', 'ok');
      } catch (e) { toast(errText(e), 'err'); }
    }
  }

  function sync() {
    const info = getInfo();
    const live = !!info && isLive(info.State);
    const follow = takesFollowUp(info);
    input.disabled = !live && !follow;
    sendBtn.disabled = input.disabled;
    input.placeholder = termInputHint(info && info.State, info && info.Agent);
    keys.hidden = !live;
  }

  return { keys, bar, micNote, input, send, sync };
}
