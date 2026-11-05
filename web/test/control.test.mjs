// Unit tests for the control center's pure helpers: grid fit, tree build,
// shortcut parser, bulk result summary, tag validation, wall selection,
// limits, feed merging. Run: node --test web/test/*.test.mjs
import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  gridFit, buildTree, shortcutStep, isTypingContext, summarizeBulk, bulkBody, bulkResults, normTag, parseTags,
  allTags, hasTag, clampPriority, validateLimits, limitsText, needsApproval, wallIds, wallStreamURL, parseWallEvent,
  miniTermSize, snapIndex, fmtElapsed, broadcastKey, mergeFeed, filterFeed, moveCursor, sessionName, byListOrder,
  limitsHelp, rowKey, feedTone, FEED_KINDS,
  SEQ_TIMEOUT,
} from '../dist/assets/js/control.js';
import { parseRoute } from '../dist/assets/js/util.js';

test('gridFit picks 1/2/4/6/9 slots', () => {
  const shape = (n, o) => { const g = gridFit(n, o); return `${g.cols}x${g.rows}/${g.slots}`; };
  assert.equal(shape(0), '0x0/0');
  assert.equal(shape(1), '1x1/1');
  assert.equal(shape(2), '2x1/2');
  assert.equal(shape(3), '2x2/4');
  assert.equal(shape(4), '2x2/4');
  assert.equal(shape(5), '3x2/6');
  assert.equal(shape(6), '3x2/6');
  assert.equal(shape(7), '3x3/9');
  assert.equal(shape(9), '3x3/9');
  assert.equal(shape(14), '3x3/9', 'capped at nine');
  // Portrait stacks; narrow screens cap the columns.
  assert.equal(shape(2, { aspect: 0.6 }), '1x2/2');
  assert.equal(shape(6, { aspect: 0.6 }), '2x3/6');
  assert.equal(shape(9, { maxCols: 2 }), '2x5/9');
  assert.equal(shape(6, { maxCols: 1 }), '1x6/6');
});

test('buildTree nests children under parents, orphans and cycles stay visible', () => {
  const t = (id, extra = {}) => ({ ID: id, Updated: '2026-11-01T10:00:00Z', ...extra });
  const list = [
    t('c1', { Parent: 'p', Updated: '2026-11-01T10:05:00Z' }),
    t('p'),
    t('g', { Parent: 'c1' }),
    t('c2', { Parent: 'p', Updated: '2026-11-01T10:09:00Z' }),
    t('o', { Parent: 'missing' }),
    t('x', { Parent: 'y' }), t('y', { Parent: 'x' }),
    t('self', { Parent: 'self' }),
  ];
  const out = buildTree(list);
  assert.equal(out.length, list.length);
  const pos = (id) => out.findIndex((r) => r.s.ID === id);
  const lvl = (id) => out[pos(id)].level;
  assert.ok(pos('p') < pos('c2') && pos('c2') < pos('c1') && pos('c1') < pos('g'), 'children follow, newest sibling first');
  assert.equal(lvl('p'), 0);
  assert.equal(lvl('c1'), 1);
  assert.equal(lvl('g'), 2);
  assert.equal(out[pos('o')].orphan, true);
  assert.equal(lvl('o'), 0);
  assert.equal(out[pos('p')].orphan, false);
  assert.ok(pos('x') >= 0 && pos('y') >= 0, 'cycle members are listed');
  assert.equal(lvl('self'), 0);
});

test('byListOrder: pinned, then priority, then most recent', () => {
  const a = { ID: 'a', Pinned: false, Priority: 5, Updated: '2026-11-01T10:00:00Z' };
  const b = { ID: 'b', Pinned: true, Priority: 0, Updated: '2026-11-01T09:00:00Z' };
  const c = { ID: 'c', Pinned: false, Priority: 5, Updated: '2026-11-01T11:00:00Z' };
  assert.deepEqual([a, b, c].sort(byListOrder).map((s) => s.ID), ['b', 'c', 'a']);
});

test('shortcutStep parses sequences and single keys', () => {
  const k = (key, mods = {}) => ({ key, ...mods });
  let r = shortcutStep(null, k('g'), 1000);
  assert.equal(r.action, null);
  assert.equal(r.state.prefix, 'g');
  r = shortcutStep(r.state, k('w'), 1500);
  assert.equal(r.action, 'wall');
  assert.equal(r.state, null);
  assert.equal(shortcutStep(shortcutStep(null, k('g'), 0).state, k('s'), 10).action, 'sessions');
  // The prefix expires.
  const st = shortcutStep(null, k('g'), 0).state;
  assert.equal(shortcutStep(st, k('w'), SEQ_TIMEOUT + 1).action, null);
  // An unknown second key cancels.
  assert.equal(shortcutStep(st, k('q'), 10).action, null);
  for (const [key, action] of [['n', 'new'], ['/', 'search'], ['j', 'down'], ['k', 'up'], ['x', 'select'], ['.', 'bulk'], ['?', 'help'], ['Escape', 'escape'], ['Enter', 'open']]) {
    assert.equal(shortcutStep(null, k(key), 0).action, action, key);
  }
  assert.equal(shortcutStep(null, k('n', { ctrlKey: true }), 0).action, null);
  assert.equal(shortcutStep(null, k('j', { metaKey: true }), 0).action, null);
  assert.equal(shortcutStep(null, k('z'), 0).action, null);
});

test('isTypingContext keeps shortcuts out of fields, terminals and dialogs', () => {
  assert.equal(isTypingContext({ tag: 'INPUT', type: 'text' }), true);
  assert.equal(isTypingContext({ tag: 'input', type: 'search' }), true);
  assert.equal(isTypingContext({ tag: 'INPUT', type: 'checkbox' }), false);
  assert.equal(isTypingContext({ tag: 'TEXTAREA' }), true);
  assert.equal(isTypingContext({ tag: 'SELECT' }), true);
  assert.equal(isTypingContext({ tag: 'DIV', editable: true }), true);
  assert.equal(isTypingContext({ tag: 'DIV', inTerminal: true }), true);
  assert.equal(isTypingContext({ tag: 'BODY', dialogOpen: true }), true);
  assert.equal(isTypingContext({ tag: 'BODY' }), false);
  assert.equal(isTypingContext(null), false);
});

test('summarizeBulk counts results and quotes the first problem', () => {
  const names = { a: 'Alpha', b: 'Beta' };
  let s = summarizeBulk([{ ID: 'a', Result: 'ok' }, { ID: 'b', Result: 'ok' }], (id) => names[id]);
  assert.deepEqual([s.ok, s.skipped, s.error, s.total, s.tone], [2, 0, 0, 2, 'ok']);
  assert.equal(s.text, '2 of 2 done');
  s = summarizeBulk([{ ID: 'a', Result: 'ok' }, { ID: 'b', Result: 'error', Detail: 'merge conflict' }, { ID: 'c', Result: 'skipped', Detail: 'not passed' }], (id) => names[id] || id);
  assert.equal(s.tone, 'warn');
  assert.equal(s.text, '1 of 3 done · 1 skipped · 1 failed (Beta: merge conflict)');
  s = summarizeBulk([{ ID: 'a', Result: 'error', Detail: 'gone' }]);
  assert.equal(s.tone, 'err');
  s = summarizeBulk([{ ID: 'c', Result: 'skipped', Detail: 'closed' }]);
  assert.equal(s.tone, 'warn');
  assert.match(s.text, /c: closed/);
  s = summarizeBulk([{ ID: 'x', Result: 'weird' }]);
  assert.equal(s.error, 1, 'unknown results count as errors');
  assert.equal(summarizeBulk([]).text, 'Nothing to do');
});

test('bulkBody dedupes, caps at 100 and clamps priority; bulkResults takes both shapes', () => {
  const ids = Array.from({ length: 120 }, (_, i) => `s${i}`);
  const b = bulkBody([...ids, 's1'], { Kind: 'priority', Priority: 40 });
  assert.equal(b.ids.length, 100);
  assert.deepEqual(b.op, { Kind: 'priority', Data: '', Push: false, Message: '', Priority: 10, Tag: '', Force: false });
  assert.deepEqual(bulkResults([{ ID: 'a' }]), [{ ID: 'a' }]);
  assert.deepEqual(bulkResults({ results: [{ ID: 'b' }] }), [{ ID: 'b' }]);
  assert.deepEqual(bulkResults(null), []);
});

test('tags: validation, parsing, filter', () => {
  assert.equal(normTag(' Docs '), 'docs');
  assert.equal(normTag('a-1'), 'a-1');
  assert.equal(normTag('no_underscore'), '');
  assert.equal(normTag('x'.repeat(25)), '');
  assert.equal(normTag('x'.repeat(24)), 'x'.repeat(24));
  assert.equal(normTag(''), '');
  let r = parseTags('Docs, urgent docs  bad!', []);
  assert.deepEqual(r.tags, ['docs', 'urgent']);
  assert.equal(r.errors.length, 1);
  r = parseTags('i j', ['a', 'b', 'c', 'd', 'e', 'f', 'g']);
  assert.equal(r.tags.length, 8);
  assert.match(r.errors[0], /at most 8/);
  assert.deepEqual(allTags([{ Tags: ['b', 'a'] }, { Tags: ['a'] }, {}]), ['a', 'b']);
  assert.equal(hasTag({ Tags: ['a'] }, 'a'), true);
  assert.equal(hasTag({}, 'a'), false);
  assert.equal(hasTag({}, ''), true);
});

test('priority and limits', () => {
  assert.equal(clampPriority(12), 10);
  assert.equal(clampPriority(-99), -10);
  assert.equal(clampPriority('3'), 3);
  assert.equal(clampPriority('x'), 0);
  let r = validateLimits({ MaxMinutes: '30', MemoryMax: '4g', MaxOutputMiB: '' });
  assert.deepEqual(r.limits, { MaxMinutes: 30, MemoryMax: '4G', MaxOutputMiB: 0 });
  assert.equal(validateLimits({ MaxMinutes: '', MemoryMax: '', MaxOutputMiB: '' }).limits.MaxMinutes, 0);
  assert.match(validateLimits({ MaxMinutes: '1.5' }).error, /whole number/);
  assert.match(validateLimits({ MemoryMax: 'lots' }).error, /Memory/);
  assert.match(validateLimits({ MaxMinutes: '20000' }).error, /week/);
  assert.equal(validateLimits({ MemoryMax: '1.5G' }).error, '');
  assert.equal(limitsText({ MaxMinutes: 30, MemoryMax: '4G', MaxOutputMiB: 50 }), '30 min · 4G memory · 50 MiB output');
  assert.equal(limitsText({ MaxMinutes: 0, MemoryMax: '', MaxOutputMiB: 0 }), '');
});

test('needsApproval and sessionName', () => {
  assert.equal(needsApproval({ State: 'queued', WaitingReason: 'needs approval' }), true);
  assert.equal(needsApproval({ State: 'queued', WaitingReason: 'guard: memory' }), false);
  assert.equal(needsApproval({ State: 'running', WaitingReason: 'needs approval' }), false);
  assert.equal(sessionName({ ID: 'i', Title: 't', Name: 'n' }), 'n');
  assert.equal(sessionName({ ID: 'i', Title: 't' }), 't');
  assert.equal(sessionName({ ID: 'i' }), 'i');
  assert.equal(sessionName(null), '');
});

test('wallIds: route set, then kept plus active, capped', () => {
  const s = (id, State, extra = {}) => ({ ID: id, State, Updated: '2026-11-01T10:00:00Z', ...extra });
  const list = [s('r1', 'running'), s('w1', 'waiting'), s('d1', 'accepted'), s('p1', 'passed'), s('r2', 'running', { Pinned: true })];
  assert.deepEqual(wallIds(list), ['w1', 'r2', 'r1']);
  assert.deepEqual(wallIds(list, { kept: ['p1', 'gone'] }), ['p1', 'w1', 'r2', 'r1']);
  assert.deepEqual(wallIds(list, { routeIds: ['d1', 'r1', 'd1', 'nope'] }), ['d1', 'r1']);
  assert.deepEqual(wallIds(list, { max: 2 }), ['w1', 'r2']);
  assert.deepEqual(wallIds([]), []);
});

test('wall stream URL, events and terminal sizing', () => {
  assert.equal(wallStreamURL(['a', 'b c']), '/api/wall/stream?ids=a,b%20c');
  assert.equal(wallStreamURL(Array.from({ length: 20 }, (_, i) => `${i}`)).split(',').length, 16);
  assert.deepEqual(parseWallEvent('{"id":"a","b64":"aGk="}'), { id: 'a', b64: 'aGk=' });
  assert.equal(parseWallEvent('{"id":1}'), null);
  assert.equal(parseWallEvent('nope'), null);
  const m = miniTermSize(600, 300, 100);
  assert.equal(m.cols, 100);
  assert.ok(m.fontSize >= 5 && m.fontSize <= 13);
  assert.ok(m.fontSize * 0.6 * 100 <= 600);
  assert.ok(m.rows >= 4);
  assert.equal(miniTermSize(100, 50, 200).fontSize, 5, 'never below the minimum');
  assert.equal(miniTermSize(4000, 2000, 80).fontSize, 13, 'never above the maximum');
  assert.equal(miniTermSize(600, 300, 0).cols, 100, 'unknown PTY width defaults to 100');
  assert.equal(snapIndex(0, 360, 3), 0);
  assert.equal(snapIndex(370, 360, 3), 1);
  assert.equal(snapIndex(5000, 360, 3), 2);
  assert.equal(snapIndex(10, 0, 3), 0);
});

test('fmtElapsed, moveCursor, broadcastKey', () => {
  const now = Date.parse('2026-11-01T12:00:00Z');
  assert.equal(fmtElapsed('2026-11-01T11:59:30Z', now), '30s');
  assert.equal(fmtElapsed('2026-11-01T11:48:00Z', now), '12m');
  assert.equal(fmtElapsed('2026-11-01T08:55:00Z', now), '3h 05m');
  assert.equal(fmtElapsed('2026-10-30T08:00:00Z', now), '2d 4h');
  assert.equal(fmtElapsed('0001-01-01T00:00:00Z', now), '–');
  assert.equal(moveCursor(-1, 1, 3), 0);
  assert.equal(moveCursor(-1, -1, 3), 2);
  assert.equal(moveCursor(2, 1, 3), 2);
  assert.equal(moveCursor(1, -1, 3), 0);
  assert.equal(moveCursor(0, 1, 0), -1);
  const appr = { State: 'waiting', WaitingReason: 'approval' };
  assert.equal(broadcastKey('y', [appr, appr]), 'y\r');
  assert.equal(broadcastKey('y', [appr, { State: 'running' }]), 'y');
  assert.equal(broadcastKey('Enter', [appr]), '\r');
  assert.equal(broadcastKey('Ctrl-C', []), '\x03');
  assert.equal(broadcastKey('nope', []), '');
});

test('feed merge dedupes and sorts newest first; filter by kinds', () => {
  const f = (t, kind, id = 's') => ({ Time: `2026-11-01T10:0${t}:00Z`, SessionID: id, Name: id, Kind: kind, Text: '' });
  let feed = mergeFeed([], [f(1, 'created'), f(3, 'waiting')]);
  feed = mergeFeed(feed, [f(3, 'waiting'), f(2, 'state'), { bogus: true }]);
  assert.deepEqual(feed.map((x) => x.Kind), ['waiting', 'state', 'created']);
  assert.equal(mergeFeed(feed, [f(4, 'input')], 2).length, 2);
  assert.deepEqual(filterFeed(feed, new Set(['state'])).map((x) => x.Kind), ['state']);
  assert.equal(filterFeed(feed, new Set()).length, 3);
});

test('parseRoute knows the wall', () => {
  assert.deepEqual(parseRoute('#/wall'), { view: 'sessions', layout: 'wall' });
  assert.deepEqual(parseRoute('#/wall/a,b'), { view: 'wall', ids: ['a', 'b'] });
});

test('limits wording follows the guard (no default for time and output; memory may be unenforced)', () => {
  const l = { MaxMinutes: 90, MemoryMax: '4G', MaxOutputMiB: 64 };
  assert.equal(limitsText(l, { memory_enforced: true }), '90 min · 4G memory · 64 MiB output');
  assert.equal(limitsText(l, { memory_enforced: false }), '90 min · 4G memory (not enforced) · 64 MiB output');
  assert.equal(limitsText(l, null), '90 min · 4G memory · 64 MiB output', 'unknown guard: no claim either way');
  let help = limitsHelp({ memory_enforced: true, session_memory_max: '6G' });
  assert.equal(help.minutes, 'no limit');
  assert.equal(help.output, 'no limit');
  assert.equal(help.memory, 'default 6G');
  assert.equal(help.memoryNote, '');
  help = limitsHelp({ memory_enforced: false, session_memory_max: '' });
  assert.equal(help.memory, 'no cap');
  assert.match(help.memoryNote, /not enforced/i);
  assert.equal(limitsHelp(null).memoryNote, '');
});

test('rowKey ignores live activity but sees what a row is built from', () => {
  const s = { ID: 'a', State: 'running', Name: 'x', Tags: ['web'], Priority: 1, Activity: 'one', OutputBytes: 10, LastOutput: 't1' };
  const k = rowKey(s);
  assert.equal(rowKey({ ...s, Activity: 'two', OutputBytes: 99, LastOutput: 't2' }), k);
  for (const change of [{ State: 'waiting' }, { Pinned: true }, { Priority: 2 }, { Tags: ['api'] }, { Name: 'y' },
    { Parent: 'p' }, { WaitingReason: 'input' }, { Attempt: 2 }]) {
    assert.notEqual(rowKey({ ...s, ...change }), k, JSON.stringify(change));
  }
  assert.equal(rowKey(null), '');
});

test('sandbox warnings in the feed read as alerts', () => {
  assert.ok(FEED_KINDS.includes('warning'));
  assert.equal(feedTone('warning'), 'err');
});
