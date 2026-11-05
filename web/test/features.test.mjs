// Unit tests for the Phase 2 pure helpers: snippet highlighting, cursor
// insertion, vault checks, spend and plan gauges, fleet counts and the
// recorder's format choice. Run: node --test web/test/
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { queryWords, highlightParts, insertAtCursor, secretError } from '../dist/assets/js/text.js';
import { fmtUSD, fmtUSDExact, fmtTokens, spendRows, budgetState, budgetBar, planGauge, resetText } from '../dist/assets/js/spend.js';
import { batchSessions, stateCounts, countChips, batchProgress, expandTask, resultCounts, resultSummary } from '../dist/assets/js/fleet.js';
import { pickMime, fmtClock, rmsLevel, micSupport, MIME_CANDIDATES, insecureNote, recordLimitMs } from '../dist/assets/js/media.js';
import { parseRoute, canResume } from '../dist/assets/js/util.js';

const marks = (parts) => parts.filter((p) => p.mark).map((p) => p.text);
const joined = (parts) => parts.map((p) => p.text).join('');

test('queryWords: distinct, lower-case, longest first', () => {
  assert.deepEqual(queryWords('  Fix the NAVBAR fix  '), ['navbar', 'fix', 'the']);
  assert.deepEqual(queryWords(''), []);
  assert.deepEqual(queryWords(undefined), []);
});

test('highlightParts marks every match case-insensitively and keeps the text', () => {
  const text = 'Fixed the Navbar; navbar.js fixed.';
  const parts = highlightParts(text, queryWords('navbar fix'));
  assert.equal(joined(parts), text);
  assert.deepEqual(marks(parts), ['Fix', 'Navbar', 'navbar', 'fix']);
  assert.equal(parts[0].mark, true);
});

test('highlightParts: regex characters are literal, adjacent matches merge', () => {
  const parts = highlightParts('call a.b(c) then a+b', queryWords('a.b( a+b'));
  assert.deepEqual(marks(parts), ['a.b(', 'a+b']);
  assert.deepEqual(marks(highlightParts('foobar baz', ['foo', 'bar'])), ['foobar']);
  // Overlapping words: the longer one wins at a position.
  assert.deepEqual(marks(highlightParts('navbars', queryWords('nav navbar'))), ['navbar']);
});

test('highlightParts: no words, empty text, HTML stays plain text', () => {
  assert.deepEqual(highlightParts('', ['x']), []);
  assert.deepEqual(highlightParts('plain', []), [{ text: 'plain', mark: false }]);
  const parts = highlightParts('<img src=x onerror=alert(1)> img', ['img']);
  assert.equal(joined(parts), '<img src=x onerror=alert(1)> img');
  assert.deepEqual(marks(parts), ['img', 'img']);
});

test('insertAtCursor spaces words apart and replaces the selection', () => {
  assert.deepEqual(insertAtCursor('', 0, 0, ' hello '), { value: 'hello', caret: 5 });
  assert.deepEqual(insertAtCursor('fix bug', 3, 3, 'the'), { value: 'fix the bug', caret: 7 });
  assert.deepEqual(insertAtCursor('ab', 1, 1, 'X'), { value: 'a X b', caret: 3 });
  assert.deepEqual(insertAtCursor('one two three', 4, 7, '2'), { value: 'one 2 three', caret: 5 });
  assert.deepEqual(insertAtCursor('end ', 4, 4, 'more'), { value: 'end more', caret: 8 });
  // No selection info (an unfocused field): append at the end.
  assert.deepEqual(insertAtCursor('task', null, null, 'more'), { value: 'task more', caret: 9 });
  assert.deepEqual(insertAtCursor('keep', 2, 2, '   '), { value: 'keep', caret: 2 });
});

test('secretError mirrors the vault rules', () => {
  assert.equal(secretError('OPENAI_KEY', 'sk-1'), '');
  assert.equal(secretError('a.b-c_1', 'v'), '');
  assert.match(secretError('bad name', 'v'), /Name/);
  assert.match(secretError('x'.repeat(65), 'v'), /Name/);
  assert.match(secretError('', 'v'), /Name/);
  assert.match(secretError('OK', ''), /value/);
  assert.equal(secretError('OK', 'é'.repeat(8192)), ''); // 16 KiB exactly in UTF-8
  assert.match(secretError('OK', 'é'.repeat(8193)), /16 KiB/);
});

test('fmtUSDExact keeps sub-cent caps readable', () => {
  assert.equal(fmtUSDExact(0.005), '$0.005');
  assert.equal(fmtUSDExact(0.000001), '$0.000001');
  assert.equal(fmtUSDExact(0.0012345), '$0.0012');
  assert.equal(fmtUSDExact(0.0000001234), '$0.00000012');
  assert.equal(fmtUSDExact(3.456), '$3.46');
  assert.equal(fmtUSDExact(0), '$0.00');
});

test('fmtUSD and fmtTokens', () => {
  assert.equal(fmtUSD(0), '$0.00');
  assert.equal(fmtUSD(undefined), '$0.00');
  assert.equal(fmtUSD(0.004), '<$0.01');
  assert.equal(fmtUSD(3.456), '$3.46');
  assert.equal(fmtUSD(1234.5), '$1,235');
  assert.equal(fmtTokens(0), '0');
  assert.equal(fmtTokens(950), '950');
  assert.equal(fmtTokens(12345), '12.3k');
  assert.equal(fmtTokens(250000), '250k');
  assert.equal(fmtTokens(4567000), '4.57M');
  assert.equal(fmtTokens(45670000), '45.7M');
});

test('spendRows merges today and month, sorted by month', () => {
  const { rows, total } = spendRows({ today: { openrouter: 0.5 }, month: { openrouter: 4, anthropic: 9.25 } });
  assert.deepEqual(rows, [
    { provider: 'anthropic', today: 0, month: 9.25 },
    { provider: 'openrouter', today: 0.5, month: 4 },
  ]);
  assert.deepEqual(total, { today: 0.5, month: 13.25 });
  assert.deepEqual(spendRows(undefined), { rows: [], total: { today: 0, month: 0 } });
});

test('budget state and bar maths', () => {
  assert.equal(budgetState(1, 5, 10), 'ok');
  assert.equal(budgetState(5, 5, 10), 'soft');
  assert.equal(budgetState(10, 5, 10), 'hard');
  assert.equal(budgetState(50, 0, 0), 'ok');
  assert.equal(budgetState(6, 5, 0), 'soft');

  const b = budgetBar({ provider: 'openrouter', period: 'day', soft_usd: 5, hard_usd: 20, spent: 10 });
  assert.deepEqual([b.pct, b.softPct, b.state, b.tone, b.cap], [50, 25, 'soft', 'warn', 20]);
  // Server state wins; Go-style field names also work; over the cap clamps.
  const c = budgetBar({ Provider: '*', Period: 'month', SoftUSD: 0, HardUSD: 10, spent: 15, state: 'hard' });
  assert.deepEqual([c.provider, c.pct, c.softPct, c.state, c.tone], ['All cloud providers', 100, null, 'hard', 'err']);
  const d = budgetBar({ provider: 'x', soft_usd: 4, spent: 1, state: 'bogus' });
  assert.deepEqual([d.pct, d.state, d.tone], [25, 'ok', 'ok']);
  assert.equal(budgetBar({ provider: 'x', spent: 3 }).pct, 0);
});

test('planGauge: percentages against estimates, hint when unset', () => {
  const g = planGauge(450000, 1000000);
  assert.deepEqual([g.pct, g.raw, g.tone, g.hint], [45, 45, 'ok', 'your estimate']);
  assert.equal(planGauge(750, 1000).tone, 'warn');
  assert.equal(planGauge(950, 1000).tone, 'err');
  const over = planGauge(1500, 1000);
  assert.deepEqual([over.pct, over.raw, over.tone], [100, 150, 'err']);
  const unset = planGauge(1234, 0);
  assert.equal(unset.pct, null);
  assert.equal(unset.hint, 'set estimates in config');
  assert.match(unset.label, /1\.2k tokens/);
});

test('resetText', () => {
  const now = Date.parse('2026-11-01T12:00:00Z');
  assert.equal(resetText('2026-11-01T14:15:00Z', now), 'resets in 2 h 15 min');
  assert.equal(resetText('2026-11-01T12:09:30Z', now), 'resets in 10 min');
  assert.equal(resetText('2026-11-01T11:00:00Z', now), '');
  assert.equal(resetText('0001-01-01T00:00:00Z', now), '');
  assert.equal(resetText('', now), '');
});

test('fleet counts and progress', () => {
  const fetched = [{ ID: 'a', State: 'running' }, { ID: 'b', State: 'passed' }, { ID: 'c', State: 'failed' }];
  const live = (id) => (id === 'a' ? { ID: 'a', State: 'passed' } : undefined);
  const list = batchSessions(['a', 'b', 'c', 'gone'], fetched, live);
  assert.deepEqual(list.map((s) => s.State), ['passed', 'passed', 'failed']);
  const counts = stateCounts(list);
  assert.deepEqual(counts, { passed: 2, failed: 1 });
  assert.deepEqual(batchProgress(counts), { total: 3, finished: 3, passed: 2 });
  assert.deepEqual(batchProgress({ running: 2, queued: 1 }), { total: 3, finished: 0, passed: 0 });
  assert.deepEqual(countChips({ accepted: 1, running: 2, zzz: 1, waiting: 0 }).map((c) => c.state), ['running', 'accepted', 'zzz']);
});

test('expandTask and result tallies', () => {
  const repo = { Name: 'Topo', Path: '/h/Github/Topo' };
  assert.equal(expandTask('Update {name} at {repo}; {name}!', repo), 'Update Topo at /h/Github/Topo; Topo!');
  assert.equal(expandTask('no placeholders', repo), 'no placeholders');
  assert.equal(expandTask('{name}', { Path: '/h/x/Retina/' }), 'Retina');
  const res = [{ Result: 'accepted' }, { Result: 'accepted' }, { Result: 'skipped' }];
  assert.deepEqual(resultCounts(res), { accepted: 2, skipped: 1, failed: 0, discarded: 0 });
  assert.equal(resultSummary(res), '2 accepted, 1 skipped');
  assert.equal(resultSummary([]), 'Nothing to do');
});

test('pickMime prefers Opus and falls back to the browser default', () => {
  assert.equal(pickMime((m) => true), 'audio/webm;codecs=opus');
  assert.equal(pickMime((m) => m.startsWith('audio/ogg')), 'audio/ogg;codecs=opus');
  assert.equal(pickMime((m) => m === 'audio/mp4'), 'audio/mp4'); // Safari
  assert.equal(pickMime(() => false), '');
  assert.equal(pickMime(() => { throw new Error('x'); }), '');
  assert.equal(pickMime(undefined), '');
  assert.ok(MIME_CANDIDATES.every((m) => m.startsWith('audio/')));
});

test('recorder clock, level and support', () => {
  assert.equal(fmtClock(0), '0:00');
  assert.equal(fmtClock(65.9), '1:05');
  assert.equal(fmtClock(300), '5:00');
  assert.equal(rmsLevel(new Uint8Array(64).fill(128)), 0);
  assert.equal(rmsLevel(new Uint8Array(0)), 0);
  const loud = new Uint8Array(64).map((_, i) => (i % 2 ? 255 : 0));
  assert.equal(rmsLevel(loud), 1);
  assert.deepEqual(micSupport({ secure: false, mediaDevices: true, recorder: true }), { ok: false, reason: 'insecure' });
  assert.deepEqual(micSupport({ secure: true, mediaDevices: false, recorder: true }), { ok: false, reason: 'unsupported' });
  assert.deepEqual(micSupport({ secure: true, mediaDevices: true, recorder: true }), { ok: true, reason: '' });
});

test('routes and resume eligibility', () => {
  assert.deepEqual(parseRoute('#/secrets'), { view: 'secrets' });
  assert.deepEqual(parseRoute('#/b/f1x'), { view: 'batch', id: 'f1x' });
  assert.deepEqual(parseRoute('#/b'), { view: 'sessions' });
  for (const s of ['passed', 'failed', 'stopped', 'error']) assert.ok(canResume(s), s);
  for (const s of ['running', 'waiting', 'accepted', 'discarded', 'queued']) assert.ok(!canResume(s), s);
});

test('insecureNote uses the server hint once, else the fallback', () => {
  const hint = 'microphone needs HTTPS: open https://desk.ts.net';
  const withHint = insecureNote(hint);
  assert.equal(withHint, "Microphone needs HTTPS: open https://desk.ts.net. Your phone keyboard's dictation works anyway.");
  assert.equal((withHint.match(/needs HTTPS/g) || []).length, 1);
  const fallback = insecureNote('');
  assert.equal(fallback, "Microphone needs HTTPS: see README (tailscale cert). Your phone keyboard's dictation works anyway.");
  assert.equal(insecureNote(undefined), fallback);
});

test('recordLimitMs stops a second before the server cap', () => {
  assert.equal(recordLimitMs(300), 299000);
  assert.equal(recordLimitMs(1), 1000);
});
