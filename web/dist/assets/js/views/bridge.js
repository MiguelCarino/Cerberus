// Carino Bridge for Cerberus (assets/carino-bridge.js, copied verbatim from
// the fleet): receive files from another Carino page into a session or a new
// task, and send a workspace file to Carino Files.
//
// Receiving: the UI opened at "#carino-bridge" by another page. Senders on
// https://*.carino.systems and this page's own origin deliver straight
// away; anyone else is asked about first, naming their origin. Either way
// the sheet that follows says where the files came from (the sender's
// origin), never whether it is "trusted".

import { h, openDialog, toast, errText, pill } from '../dom.js';
import { store } from '../store.js';
import { stateTone, isClosed, workspaceLabel } from '../util.js';
import { sessionName } from '../control.js';
import { fmtBytes, originAllowed, originHost, bridgeTargets } from '../files.js';
import { uploadToSession } from '../upload.js';
import { newTask } from './sessions.js';

// FILES_URL is Carino Files, the fleet's file workbench.
export const FILES_URL = 'https://files.carino.systems/';

const bridge = () => (typeof window !== 'undefined' && window.CarinoBridge) || null;

// initBridgeReceive arms the receiver when this page is a handoff; returns
// false on an ordinary visit.
export function initBridgeReceive() {
  const cb = bridge();
  if (!cb) return false;
  return cb.receive({
    // Exact origins only; the *.carino.systems rule is applied in ask(),
    // which waves those through without a question.
    allow: ['same-origin'],
    otherwise: 'ask',
    ask: async (origin, n) => {
      if (originAllowed(origin, location.origin)) return true;
      return askStranger(origin, n);
    },
    onFiles: (files, { origin }) => {
      // Take the files now (the sender is waiting for the acknowledgement)
      // and let the user choose where they go at their own pace.
      setTimeout(() => chooseDestination([...files], origin), 0);
      return true;
    },
    onError: (e) => toast(errText(e), 'err'),
  });
}

function askStranger(origin, n) {
  const host = originHost(origin);
  return openDialog((close) => ({
    title: 'Files from another page',
    sub: origin,
    body: h('p', { class: 'dlg-msg' },
      'The page at ', h('b', { class: 'mono', text: host }),
      ` wants to hand Cerberus ${n} file${n === 1 ? '' : 's'}. Accept only if you just sent them from that page.`),
    foot: [
      h('button', { class: 'btn btn-ghost', type: 'button', onclick: () => close(false) }, 'Refuse'),
      h('button', { class: 'btn btn-primary', type: 'button', onclick: () => close(true) }, 'Accept'),
    ],
  })).then(Boolean);
}

// chooseDestination is the "Choose session or new task" sheet.
async function chooseDestination(files, origin) {
  const host = originHost(origin);
  // Only roles that start sessions take files (users and roles).
  const me = store.me() || await store.loadMe().catch(() => null);
  if (!me || !store.can('sessions')) { toast(`Files from ${host} were not kept: your account cannot start sessions`, 'warn'); return; }
  await store.refreshSessions().catch(() => {});
  // Only sessions whose workspace exists take files now: a queued repo
  // session has no worktree yet, and the handoff cannot be retried.
  const open = bridgeTargets(store.sessions(), isClosed);
  const choice = await openDialog((close) => ({
    title: 'Choose session or new task',
    sub: `${files.length} file${files.length === 1 ? '' : 's'} from ${host}`,
    body: h('div', { class: 'brx' },
      h('ul', { class: 'brx-files' }, files.map((f) =>
        h('li', {}, h('span', { class: 'brx-name', text: f.name }), h('span', { class: 'brx-size', text: fmtBytes(f.size) })))),
      h('button', { class: 'btn btn-primary brx-new', type: 'button', onclick: () => close({ kind: 'task' }) }, '+ New task with these files'),
      open.length ? h('p', { class: 'field-label brx-or', text: 'Or add them to a session’s inbox' }) : null,
      open.length ? h('div', { class: 'brx-list' }, open.map((s) =>
        h('button', { class: 'brx-sess', type: 'button', onclick: () => close({ kind: 'session', s }) },
          h('span', { class: 'brx-sname', text: sessionName(s) }),
          h('span', { class: 'brx-smeta', text: workspaceLabel(s) }),
          pill(s.State, stateTone(s.State))))) : null),
  }));
  if (!choice) { toast(`Files from ${host} were not kept`, 'info'); return; }
  if (choice.kind === 'task') { location.hash = '#/sessions'; newTask(files); return; }
  location.hash = `#/s/${encodeURIComponent(choice.s.ID)}/files`;
  uploadToSession(choice.s.ID, files, { name: sessionName(choice.s) });
}

// sendToCarinoFiles opens Carino Files and hands it one workspace file.
// It must run inside the click handler: the bridge opens the window
// synchronously so pop-up blockers allow it. The file is fetched only once
// Files is listening.
export function sendToCarinoFiles(url, entry) {
  const cb = bridge();
  if (!cb) { toast('The bridge script did not load', 'err'); return; }
  const target = originHost(FILES_URL);
  cb.send(FILES_URL, async () => {
    const r = await fetch(url, { credentials: 'same-origin', headers: { 'X-Cerberus': '1' } });
    if (!r.ok) throw new Error(`bridge: could not read ${entry.Name} (${r.status})`);
    const blob = await r.blob();
    return [new File([blob], entry.Name, { type: entry.Mime || blob.type || '' })];
  }, { timeout: 30000 }).then(
    (res) => toast(res.acknowledged ? `Handed ${entry.Name} to ${target}` : `Sent ${entry.Name} to ${target}`, 'ok'),
    (e) => toast(`${target}: ${errText(e).replace(/^bridge: /, '')}`, 'err'),
  );
}
