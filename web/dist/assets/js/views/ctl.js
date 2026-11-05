// Control-center actions shared by the views: metadata patches, spawn
// approval, kill, and bulk operations with their result toast. Each one
// toasts its own outcome and keeps the store current.

import { api, apiResult, post, sid } from '../api.js';
import { toast, errText, confirmDialog } from '../dom.js';
import { store } from '../store.js';
import { bulkBody, bulkResults, summarizeBulk, sessionName, deleteMessage } from '../control.js';

// patchSession sends PATCH /api/sessions/{id} (Patch JSON: Name, Tags,
// Pinned, Priority, Limits) and returns the new Info, or null on failure.
export async function patchSession(id, patch, done = '') {
  try {
    const info = await api(sid(id), { method: 'PATCH', body: patch });
    if (info && info.ID) store.upsert(info);
    if (done) toast(done, 'ok');
    return info || null;
  } catch (e) {
    toast(errText(e), 'err');
    return null;
  }
}

export async function approveSession(id, approve) {
  try {
    await post(`${sid(id)}/approve`, { approve });
    toast(approve ? 'Approved: the child starts when a slot is free' : 'Rejected', 'ok');
  } catch (e) { toast(errText(e), 'err'); }
}

export async function killSession(id) {
  try { await post(`${sid(id)}/kill`); toast('Killed', 'ok'); } catch (e) { toast(errText(e), 'err'); }
}

// runBulk posts one bulk operation and shows the summary toast. It returns
// the per-session results ([] on a request failure).
export async function runBulk(ids, op) {
  try {
    const out = await post('/api/sessions/bulk', bulkBody(ids, op));
    const results = bulkResults(out);
    const sum = summarizeBulk(results, (id) => sessionName(store.session(id)) || id);
    toast(sum.text, sum.tone);
    return results;
  } catch (e) {
    toast(errText(e), 'err');
    return [];
  }
}

// deleteSessions asks, then deletes sessions for good. A repo session whose
// worktree has work that was never accepted is kept on the first pass; the
// second confirm names them and deletes them with force. It resolves to the
// ids that were deleted.
export async function deleteSessions(ids) {
  const list = [...new Set(ids || [])];
  if (!list.length) return [];
  const msg = deleteMessage(list.map((id) => store.session(id) || { ID: id }));
  if (!(await confirmDialog({ title: msg.title, message: msg.message, ok: msg.ok, danger: true }))) return [];
  const done = [];
  let kept = [];
  if (list.length === 1) {
    const id = list[0];
    try {
      const r = await apiResult(sid(id), { method: 'DELETE' });
      if (r.ok) done.push(id);
      else if (r.status === 409 && r.body && r.body.unaccepted) kept = [{ id, why: r.message }];
      else toast(r.message, 'err');
    } catch (e) { toast(errText(e), 'err'); }
  } else {
    const res = await runBulk(list, { Kind: 'delete' });
    for (const x of res) {
      if (x.Result === 'ok') done.push(x.ID);
      else if (x.Result === 'skipped' && /not accepted/.test(x.Detail || '')) kept.push({ id: x.ID, why: x.Detail });
    }
  }
  if (kept.length) {
    const names = kept.map((k) => `“${sessionName(store.session(k.id)) || k.id}”`).join(', ');
    const again = await confirmDialog({
      title: 'Unaccepted work',
      message: `${names} ${kept.length === 1 ? 'has' : 'have'} work in ${kept.length === 1 ? 'its worktree' : 'their worktrees'} that was never accepted. Delete anyway? That work is lost.`,
      ok: 'Delete anyway', danger: true,
    });
    if (again) {
      if (kept.length === 1) {
        try {
          const r = await apiResult(`${sid(kept[0].id)}?force=1`, { method: 'DELETE' });
          if (r.ok) done.push(kept[0].id); else toast(r.message, 'err');
        } catch (e) { toast(errText(e), 'err'); }
      } else {
        const res = await runBulk(kept.map((k) => k.id), { Kind: 'delete', Force: true });
        for (const x of res) if (x.Result === 'ok') done.push(x.ID);
      }
    }
  }
  if (done.length && list.length === 1) toast('Deleted', 'ok');
  return done;
}

// trashIcon is the delete glyph for icon buttons.
export function trashIcon() {
  const ns = 'http://www.w3.org/2000/svg';
  const svg = document.createElementNS(ns, 'svg');
  svg.setAttribute('viewBox', '0 0 16 16');
  svg.setAttribute('width', '16');
  svg.setAttribute('height', '16');
  svg.setAttribute('aria-hidden', 'true');
  svg.setAttribute('fill', 'none');
  svg.setAttribute('stroke', 'currentColor');
  svg.setAttribute('stroke-width', '1.5');
  svg.setAttribute('stroke-linecap', 'round');
  const p = document.createElementNS(ns, 'path');
  p.setAttribute('d', 'M2.5 4h11M6.5 4V2.5h3V4M4 4l.7 9.5h6.6L12 4M6.6 6.5v4.5M9.4 6.5v4.5');
  svg.append(p);
  return svg;
}
