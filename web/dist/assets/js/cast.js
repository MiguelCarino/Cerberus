// Pure helpers for the session Replay tab (day phase 6): parsing asciicast
// v2 recordings, idle-gap capping, seeking, the scrubber's activity marks
// and links to a moment. No DOM, so node --test imports them directly.

// Playback speeds offered by the player.
export const SPEEDS = [1, 2, 4, 16];

// IDLE_CAP is the longest pause replayed, in seconds: longer gaps between
// output events are shortened to it.
export const IDLE_CAP = 2;

// parseCast reads an asciicast v2 text: a JSON header line, then one
// [time, code, data] event per line. Output ("o") events become frames;
// markers ("m") are kept apart; input ("i") and other codes are ignored.
// Unparseable lines are skipped (a run still being recorded may end in a
// partial line). Returns {header, frames: [{t, data}], markers: [{t, label}],
// duration}, or null when the header is missing or not version 2.
export function parseCast(text) {
  if (typeof text !== 'string' || !text) return null;
  const lines = text.split('\n');
  let header;
  try { header = JSON.parse(lines[0]); } catch { return null; }
  if (!header || typeof header !== 'object' || Array.isArray(header) || header.version !== 2) return null;
  const frames = [];
  const markers = [];
  let last = 0;
  for (let i = 1; i < lines.length; i++) {
    const line = lines[i].trim();
    if (!line) continue;
    let ev;
    try { ev = JSON.parse(line); } catch { continue; }
    if (!Array.isArray(ev) || ev.length < 3 || !Number.isFinite(ev[0]) || typeof ev[2] !== 'string') continue;
    // Times never go backwards in a valid file; clamp a bad one.
    const t = Math.max(last, ev[0]);
    last = t;
    if (ev[1] === 'o') frames.push({ t, data: ev[2] });
    else if (ev[1] === 'm') markers.push({ t, label: ev[2] });
  }
  return {
    header: {
      width: Math.max(1, Math.floor(+header.width || 80)),
      height: Math.max(1, Math.floor(+header.height || 24)),
      timestamp: +header.timestamp || 0,
      title: typeof header.title === 'string' ? header.title : '',
    },
    frames,
    markers,
    duration: last,
  };
}

// capIdle maps recorded times onto playback times where no gap between
// consecutive events is longer than cap seconds. It returns a function
// from a recorded time to its capped time, plus the list of capped times
// for the frames (same order). Markers go through the same map.
export function capIdle(frames, cap = IDLE_CAP) {
  const times = new Array(frames.length);
  // Cumulative "removed" time at each frame, for mapping arbitrary times.
  const removed = new Array(frames.length);
  let prev = 0;
  let cut = 0;
  for (let i = 0; i < frames.length; i++) {
    const gap = frames[i].t - prev;
    if (gap > cap) cut += gap - cap;
    removed[i] = cut;
    times[i] = frames[i].t - cut;
    prev = frames[i].t;
  }
  const recorded = frames.map((f) => f.t);
  const map = (t) => {
    // The last frame at or before t decides the cut so far; inside a long
    // gap the time runs normally up to the cap and then holds.
    const i = lastIndexAtOrBefore(recorded, t);
    if (i < 0) return Math.min(t, cap);
    const base = frames[i].t;
    return base - removed[i] + Math.min(t - base, cap);
  };
  return { times, map, duration: frames.length ? times[times.length - 1] : 0 };
}

// lastIndexAtOrBefore finds the last index whose value is <= t in an
// ascending array (-1 when none): binary search.
export function lastIndexAtOrBefore(arr, t) {
  let lo = 0;
  let hi = arr.length - 1;
  let ans = -1;
  while (lo <= hi) {
    const mid = (lo + hi) >> 1;
    if (arr[mid] <= t) { ans = mid; lo = mid + 1; } else hi = mid - 1;
  }
  return ans;
}

// framesUpTo joins the output of frames [0, i] into one string, for a seek:
// the terminal is reset and fed everything up to the new position at once.
export function framesUpTo(frames, i) {
  let out = '';
  for (let k = 0; k <= i && k < frames.length; k++) out += frames[k].data;
  return out;
}

// activityBins splits [0, duration] into n bins and returns, per bin, the
// amount of output (characters) in it scaled to 0..1, for the scrubber's
// activity marks. times are playback times of the frames.
export function activityBins(times, frames, duration, n = 100) {
  const bins = new Array(n).fill(0);
  if (!frames.length || !(duration > 0) || n <= 0) return bins;
  for (let i = 0; i < frames.length; i++) {
    const b = Math.min(n - 1, Math.floor((times[i] / duration) * n));
    bins[b] += frames[i].data.length;
  }
  const max = Math.max(...bins);
  return max > 0 ? bins.map((v) => v / max) : bins;
}

// nextDelay is how long (ms, real time) to wait before frame i+1 when
// frame i was just written, at speed.
export function nextDelay(times, i, speed) {
  if (i + 1 >= times.length) return -1;
  const from = i < 0 ? 0 : times[i];
  return Math.max(0, ((times[i + 1] - from) * 1000) / (speed || 1));
}

// fmtClock renders seconds as "m:ss" (or "h:mm:ss").
export function fmtClock(sec) {
  if (!Number.isFinite(sec) || sec < 0) sec = 0;
  const s = Math.floor(sec);
  const h = Math.floor(s / 3600);
  const m = Math.floor((s % 3600) / 60);
  const ss = String(s % 60).padStart(2, '0');
  return h ? `${h}:${String(m).padStart(2, '0')}:${ss}` : `${m}:${ss}`;
}

// replayHash is the in-app link to a moment of a run:
// "#/s/<id>/replay/<attempt>/<seconds>" (seconds to one decimal, omitted
// when zero).
export function replayHash(id, attempt, t = 0) {
  let h = `#/s/${encodeURIComponent(id)}/replay`;
  if (attempt) h += `/${attempt}`;
  if (attempt && t > 0) h += `/${Math.round(t * 10) / 10}`;
  return h;
}

// parseReplayTime reads the time part of a replay link ("83.5", "1:23.5"
// or "1:02:03"): seconds, or 0 when absent or invalid.
export function parseReplayTime(s) {
  if (s == null || s === '') return 0;
  const parts = String(s).split(':');
  if (parts.length > 3) return 0;
  let t = 0;
  for (const p of parts) {
    if (!/^\d+(\.\d+)?$/.test(p)) return 0;
    t = t * 60 + parseFloat(p);
  }
  return t;
}

// normRuns turns GET /api/sessions/{id}/runs (a list, or {runs}) into
// [{attempt, size, modified, recording}] newest attempt first.
export function normRuns(reply) {
  const list = Array.isArray(reply) ? reply : (reply && (reply.runs || reply.Runs)) || [];
  return list
    .map((r) => ({
      attempt: Math.floor(+(r.attempt ?? r.Attempt) || 0),
      size: +(r.size ?? r.Size) || 0,
      modified: r.modified ?? r.Modified ?? '',
      recording: !!(r.recording ?? r.Recording),
    }))
    .filter((r) => r.attempt > 0)
    .sort((a, b) => b.attempt - a.attempt);
}
