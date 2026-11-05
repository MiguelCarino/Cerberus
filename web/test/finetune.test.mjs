// Unit tests for Settings → Fine-tuning's pure helpers (web/dist/assets/js/finetune.js):
// capture modes, counts, list URLs and paging, rating states, previews,
// corrections, the trainer summary and run states.
// Run: node --test web/test/*.test.mjs
import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  CAPTURE_MODES, captureMode, captureText, countTiles, KINDS, RATED, PAGE_SIZE, examplesURL, exportURL, normPage,
  appendPage, replaceExample, ratingState, ratingBadge, firstLine, promptOf, previews, betterError, BETTER_MAX,
  shellJoin, trainSummary, sessionID, runBadge, runLine, runHref, emptyText, roleLabel,
  trainRows, trainText, sourceLink,
} from '../dist/assets/js/finetune.js';
import { parseRoute } from '../dist/assets/js/util.js';

const ex = (id, extra = {}) => ({
  id, time: '2026-11-02T10:00:00Z', kind: 'delegated', model: 'qwen3-coder:30b', session: 's1',
  messages: [{ role: 'system', content: 'You help.' }, { role: 'user', content: `Summarise ${id}.\nREADME follows` }],
  answer: 'A note store.', rating: null, excluded: false, ...extra,
});

test('capture modes: the three config values, each explained', () => {
  assert.deepEqual(CAPTURE_MODES.map((m) => m.id), ['off', 'delegated', 'cloud']);
  assert.equal(captureMode('cloud'), 'cloud');
  assert.equal(captureMode(''), 'off');
  assert.equal(captureMode('everything'), 'off');
  assert.match(captureText('delegated'), /local model/);
  assert.match(captureText('cloud'), /cloud chat answer/);
  assert.equal(captureText(undefined), captureText('off'));
});

test('countTiles: the five counts, excluded only when some are', () => {
  assert.deepEqual(countTiles(null).map((t) => [t.key, t.value]),
    [['examples', 0], ['rated', 0], ['good', 0], ['corrected', 0], ['usable', 0]]);
  const t = countTiles({ examples: 12, rated: 5, good: 3, corrected: 1, usable: 4, excluded: 2 });
  assert.equal(t.find((x) => x.key === 'usable').label, 'Ready to train');
  assert.equal(t.find((x) => x.key === 'excluded').value, 2);
  assert.equal(countTiles({ examples: -3 })[0].value, 0);
});

test('examplesURL: only set filters, trimmed search, limit and cursor', () => {
  assert.equal(examplesURL(), '/api/finetune/examples?limit=20');
  assert.equal(PAGE_SIZE, 20);
  const u = new URL(examplesURL({ kind: 'cloud', rated: 'corrected', q: '  readme  ' }, 'ex-9'), 'http://x');
  assert.equal(u.searchParams.get('kind'), 'cloud');
  assert.equal(u.searchParams.get('rated'), 'corrected');
  assert.equal(u.searchParams.get('q'), 'readme');
  assert.equal(u.searchParams.get('before'), 'ex-9');
  // Unknown filter values never reach the server (it would answer 400).
  const bad = new URL(examplesURL({ kind: 'other', rated: 'meh', q: ' ' }), 'http://x');
  assert.equal(bad.searchParams.get('kind'), null);
  assert.equal(bad.searchParams.get('rated'), null);
  assert.equal(bad.searchParams.get('q'), null);
  assert.ok(KINDS.some(([k]) => k === '') && RATED.some(([k]) => k === 'unrated'));
});

test('exportURL: plain, with unrated rows, by kind', () => {
  assert.equal(exportURL(), '/api/finetune/export');
  assert.equal(exportURL({ unrated: true }), '/api/finetune/export?unrated=1');
  assert.equal(exportURL({ kind: 'cloud' }), '/api/finetune/export?kind=cloud');
  assert.equal(exportURL({ kind: 'nope' }), '/api/finetune/export');
});

test('normPage, appendPage and replaceExample', () => {
  assert.deepEqual(normPage(null), { examples: [], total: 0, next: '' });
  const p1 = normPage({ examples: [ex('a'), ex('b'), null, { no: 'id' }], total: 3, next: 'b' });
  assert.deepEqual(p1.examples.map((e) => e.id), ['a', 'b']);
  // A page that overlaps (a new example arrived) adds only new ids.
  const list = appendPage(p1.examples, normPage({ examples: [ex('b'), ex('c')], total: 3, next: '' }));
  assert.deepEqual(list.map((e) => e.id), ['a', 'b', 'c']);
  const rated = ex('b', { rating: { good: true, by: 'owner' } });
  const next = replaceExample(list, rated);
  assert.equal(next[1], rated);
  assert.equal(next[0], list[0]);
  assert.equal(replaceExample(list, null), list);
});

test('ratingState and ratingBadge follow the server filter rules', () => {
  assert.equal(ratingState(ex('a')), 'unrated');
  assert.equal(ratingState(ex('a', { rating: { good: true } })), 'good');
  assert.equal(ratingState(ex('a', { rating: { good: false } })), 'bad');
  assert.equal(ratingState(ex('a', { rating: { good: false, better: 'X' } })), 'corrected');
  // A correction counts as corrected even when marked good.
  assert.equal(ratingState(ex('a', { rating: { good: true, better: 'X' } })), 'corrected');
  const mine = ratingBadge(ex('a', { rating: { good: true, by: 'owner' } }));
  assert.deepEqual([mine.text, mine.tone], ['Good', 'ok']);
  assert.match(mine.title, /by you/);
  assert.match(ratingBadge(ex('a', { rating: { good: false } })).title, /by the agent/);
  assert.equal(ratingBadge(ex('a')).tone, 'muted');
});

test('firstLine, promptOf and previews', () => {
  assert.equal(firstLine('\n\n  hello   world \nsecond'), 'hello world');
  assert.equal(firstLine(''), '');
  assert.equal(firstLine('x'.repeat(200), 10), 'xxxxxxxxx…');
  assert.equal(promptOf(ex('a')), 'Summarise a.\nREADME follows');
  assert.equal(promptOf({ messages: [{ role: 'system', content: 'only' }] }), 'only');
  assert.equal(promptOf({}), '');
  const pv = previews(ex('a'));
  assert.deepEqual(pv, { prompt: 'Summarise a.', answer: 'A note store.', corrected: false });
  const c = previews(ex('a', { rating: { good: false, better: 'Put, Get and List.' } }));
  assert.equal(c.answer, 'Put, Get and List.');
  assert.ok(c.corrected);
  assert.equal(previews({ messages: [], answer: '' }).prompt, '(empty prompt)');
});

test('betterError: empty and too long', () => {
  assert.match(betterError('   '), /Write the answer/);
  assert.equal(betterError('Put, Get and List.'), '');
  assert.match(betterError('x'.repeat(BETTER_MAX + 1)), /too long/);
});

test('shellJoin and trainSummary', () => {
  assert.equal(shellJoin(['train.sh', '--data', '{dataset}', 'two words', "it's", '']), "train.sh --data {dataset} 'two words' 'it'\\''s' ''");
  assert.equal(shellJoin(null), '');
  const none = trainSummary({}, { usable: 4 });
  assert.equal(none.ready, false);
  assert.match(none.blocked, /train command/);
  const empty = trainSummary({ train_command: ['t', '{dataset}'], base_model: 'qwen3:8b' }, { usable: 0 });
  assert.equal(empty.ready, false);
  assert.match(empty.blocked, /No examples/);
  const ok = trainSummary({ train_command: ['t', '{dataset}'], base_model: 'qwen3:8b' }, { usable: 3 });
  assert.deepEqual([ok.ready, ok.command, ok.base, ok.usable, ok.blocked], [true, 't {dataset}', 'qwen3:8b', 3, '']);
});

test('Train now works when only unrated delegated answers exist', () => {
  const cmd = { train_command: ['t', '{dataset}'] };
  const only = trainSummary(cmd, { usable: 0, usable_unrated: 30 });
  assert.deepEqual([only.ready, only.onlyUnrated, only.usable, only.unrated, only.blocked], [true, true, 0, 30, '']);
  assert.equal(trainRows(only, false), 0);
  assert.equal(trainRows(only, true), 30);
  assert.match(trainText(only, false), /tick the box/);
  assert.match(trainText(only, true), /exports the 30 examples/);
  const mixed = trainSummary(cmd, { usable: 2, usable_unrated: 5 });
  assert.deepEqual([mixed.ready, mixed.onlyUnrated, mixed.unrated], [true, false, 3]);
  assert.match(trainText(mixed, false), /exports the 2 ready examples/);
  assert.match(trainText(mixed, true), /exports the 5 examples/);
  // an older server without usable_unrated reads as no unrated rows
  const old = trainSummary(cmd, { usable: 1 });
  assert.deepEqual([old.unrated, old.onlyUnrated], [0, false]);
  assert.match(trainText(old, true), /exports the 1 ready example into/);
  assert.equal(trainSummary(cmd, { usable: 0, usable_unrated: 0 }).ready, false);
});

test('sourceLink: chats open the chat, sessions the session', () => {
  assert.deepEqual(sourceLink('chat-20261102-101500-a1b2c3'), { href: '#/chat/chat-20261102-101500-a1b2c3', label: 'Chat' });
  assert.deepEqual(sourceLink('20261102-0001'), { href: '#/s/20261102-0001', label: 'Session' });
  assert.deepEqual(sourceLink('chat-weird'), { href: '#/s/chat-weird', label: 'Session' });
  assert.equal(sourceLink(''), null);
  assert.equal(sourceLink(undefined), null);
});

test('sessionID reads Go or lower-case Info', () => {
  assert.equal(sessionID({ ID: 'abc' }), 'abc');
  assert.equal(sessionID({ id: 'def' }), 'def');
  assert.equal(sessionID(null), '');
});

test('runs: badge, line and link', () => {
  assert.deepEqual(runBadge({ state: 'running' }), { text: 'running', tone: 'info' });
  assert.deepEqual(runBadge({ state: 'passed' }), { text: 'passed', tone: 'ok' });
  assert.deepEqual(runBadge({ state: 'failed' }), { text: 'failed', tone: 'err' });
  assert.equal(runBadge({ state: 'cli' }).text, 'from the terminal');
  assert.equal(runBadge({ state: 'removed' }).text, 'session deleted');
  assert.equal(runBadge({}).text, 'unknown');
  assert.equal(runLine({ rows: 1, base: 'qwen3:8b' }), '1 row · base qwen3:8b');
  assert.equal(runLine({ rows: 12, unrated: true }), '12 rows · default base · with unrated');
  assert.equal(runHref({ session: 'a b', state: 'running' }), '#/s/a%20b');
  assert.equal(runHref({ session: 'x', state: 'removed' }), '');
  assert.equal(runHref({ state: 'cli' }), '');
});

test('emptyText says what to do next', () => {
  assert.match(emptyText('off', false, 0), /Choose Delegated/);
  assert.match(emptyText('delegated', false, 0), /cerberus_ask/);
  assert.match(emptyText('delegated', true, 5), /Clear them/);
  assert.match(emptyText('delegated', true, 0), /cerberus_ask/);
});

test('roleLabel', () => {
  assert.equal(roleLabel('user'), 'Prompt');
  assert.equal(roleLabel('system'), 'System');
  assert.equal(roleLabel('odd'), 'odd');
});

test('the Fine-tuning group has its own settings route', () => {
  assert.deepEqual(parseRoute('#/settings/finetune'), { view: 'settings', group: 'finetune' });
});
