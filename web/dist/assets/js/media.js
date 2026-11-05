// Pure helpers for the voice recorder: recording format choice, the timer
// text, the input level and whether the microphone can be used here. No
// DOM, so node --test imports them directly.

// Formats the server's ffmpeg step accepts, best first: Opus in WebM
// (Chromium, Firefox), Opus in Ogg (Firefox), AAC in MP4 (Safari).
export const MIME_CANDIDATES = [
  'audio/webm;codecs=opus',
  'audio/ogg;codecs=opus',
  'audio/mp4;codecs=mp4a.40.2',
  'audio/webm',
  'audio/ogg',
  'audio/mp4',
];

// pickMime returns the first candidate isTypeSupported accepts, or "" to let
// the browser use its default.
export function pickMime(isTypeSupported) {
  if (typeof isTypeSupported !== 'function') return '';
  for (const m of MIME_CANDIDATES) {
    try { if (isTypeSupported(m)) return m; } catch { /* treat as unsupported */ }
  }
  return '';
}

// fmtClock renders seconds as m:ss.
export function fmtClock(sec) {
  sec = Math.max(0, Math.floor(+sec || 0));
  return `${Math.floor(sec / 60)}:${String(sec % 60).padStart(2, '0')}`;
}

// rmsLevel turns an AnalyserNode byte time-domain buffer (128 = silence)
// into a 0..1 level, scaled so normal speech fills most of the meter.
export function rmsLevel(buf) {
  if (!buf || !buf.length) return 0;
  let sum = 0;
  for (let i = 0; i < buf.length; i++) {
    const v = (buf[i] - 128) / 128;
    sum += v * v;
  }
  return Math.min(1, Math.sqrt(sum / buf.length) * 3);
}

// micSupport says whether recording can work here: getUserMedia exists only
// in a secure context (HTTPS or localhost).
export function micSupport({ secure, mediaDevices, recorder }) {
  if (!secure) return { ok: false, reason: 'insecure' };
  if (!mediaDevices || !recorder) return { ok: false, reason: 'unsupported' };
  return { ok: true, reason: '' };
}

export const INSECURE_MESSAGE = 'microphone needs HTTPS: see README (tailscale cert)';
export const DICTATION_HINT = "Your phone keyboard's dictation works anyway.";
export const MAX_UPLOAD_BYTES = 25 * 1024 * 1024;

// insecureNote explains why the microphone is off over plain HTTP. The
// server's secure_hint (which knows public_url and the TLS addresses) wins;
// the generic message is only the fallback, never both.
export function insecureNote(hint) {
  const h = typeof hint === 'string' ? hint.trim() : '';
  const msg = h || INSECURE_MESSAGE;
  const sentence = msg.charAt(0).toUpperCase() + msg.slice(1);
  const stop = /[.!?]$/.test(sentence) ? '' : '.';
  return sentence + stop + ' ' + DICTATION_HINT;
}

// recordLimitMs is when the recorder stops on its own: a second before the
// server's max_seconds, so a timer that fires late (hidden tab, locked
// phone) still leaves the recording within the limit.
export function recordLimitMs(maxSeconds) {
  return Math.max(1, maxSeconds - 1) * 1000;
}
