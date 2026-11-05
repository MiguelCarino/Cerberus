// Unit tests for the UI's pure helpers. Run: node --test web/test/
import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  parseDiff, lineRef, formatComments, b64ToBytes, groupSessions, stateTone, countWaiting,
  quickKeyData, parseRoute, relTime, fmtMiB, fmtNum, repoName, isLive, canFollowUp,
  workspaceKind, workspaceLabel, canDiff, canAcceptSession, canSave, breadcrumbs, folderNote, joinPath,
} from '../dist/assets/js/util.js';

const SAMPLE = [
  'diff --git a/src/main.go b/src/main.go',
  'index 1111111..2222222 100644',
  '--- a/src/main.go',
  '+++ b/src/main.go',
  '@@ -1,4 +1,5 @@ package main',
  ' package main',
  '-import "fmt"',
  '+import (',
  '+\t"fmt"',
  '+)',
  ' ',
  '@@ -10 +11 @@ func main() {',
  '--- a removed line that looks like a header',
  '+--- an added one too',
  'diff --git a/new.txt b/new.txt',
  'new file mode 100644',
  'index 0000000..3333333',
  '--- /dev/null',
  '+++ b/new.txt',
  '@@ -0,0 +1,2 @@',
  '+hello',
  '+world',
  '\\ No newline at end of file',
  'diff --git a/old.txt b/old.txt',
  'deleted file mode 100644',
  '--- a/old.txt',
  '+++ /dev/null',
  '@@ -1 +0,0 @@',
  '-bye',
  'diff --git a/img.png b/img.png',
  'new file mode 100644',
  'Binary files /dev/null and b/img.png differ',
  'diff --git a/a.txt b/b.txt',
  'similarity index 100%',
  'rename from a.txt',
  'rename to b.txt',
  '',
].join('\n');

test('parseDiff: files, statuses and counts', () => {
  const files = parseDiff(SAMPLE);
  assert.deepEqual(files.map((f) => [f.path, f.status, f.adds, f.dels, f.binary]), [
    ['src/main.go', 'modified', 4, 2, false],
    ['new.txt', 'added', 2, 0, false],
    ['old.txt', 'deleted', 0, 1, false],
    ['img.png', 'added', 0, 0, true],
    ['b.txt', 'renamed', 0, 0, false],
  ]);
  assert.equal(files[4].oldPath, 'a.txt');
});

test('parseDiff: line numbers and header-like content lines', () => {
  const [f] = parseDiff(SAMPLE);
  assert.equal(f.hunks.length, 2);
  const h1 = f.hunks[0];
  assert.equal(h1.section, 'package main');
  assert.deepEqual(h1.lines.map((l) => [l.type, l.oldNo, l.newNo]), [
    ['ctx', 1, 1], ['del', 2, null], ['add', null, 2], ['add', null, 3], ['add', null, 4], ['ctx', 3, 5],
  ]);
  const h2 = f.hunks[1];
  assert.deepEqual(h2.lines.map((l) => [l.type, l.text]), [
    ['del', '-- a removed line that looks like a header'],
    ['add', '--- an added one too'],
  ]);
  assert.equal(h2.lines[0].oldNo, 10);
  assert.equal(h2.lines[1].newNo, 11);
});

test('parseDiff: no-newline marker and empty input', () => {
  const nf = parseDiff(SAMPLE)[1];
  assert.equal(nf.hunks[0].lines.at(-1).type, 'meta');
  assert.deepEqual(parseDiff(''), []);
  assert.deepEqual(parseDiff(undefined), []);
});

test('parseDiff: plain diff without git headers', () => {
  const files = parseDiff('--- a/x\n+++ b/x\n@@ -1 +1 @@\n-a\n+b\n--- a/y\n+++ b/y\n@@ -1 +1 @@\n-c\n+d\n');
  assert.deepEqual(files.map((f) => [f.path, f.adds, f.dels]), [['x', 1, 1], ['y', 1, 1]]);
});

test('lineRef and formatComments', () => {
  assert.equal(lineRef('a.go', { newNo: 5, oldNo: 4 }), 'a.go:5');
  assert.equal(lineRef('a.go', { newNo: null, oldNo: 4 }), 'a.go:4 (removed line)');
  assert.equal(formatComments([]), '');
  assert.equal(formatComments([{ ref: 'x', text: '   ' }]), '');
  const msg = formatComments([
    { ref: 'a.go:5', code: '  x := 1', text: 'rename x' },
    { ref: 'b.go:2', code: '', text: 'two\nlines' },
  ]);
  assert.match(msg, /^Review comments/);
  assert.match(msg, /1\. a\.go:5\n {3}> x := 1\n {3}rename x/);
  assert.match(msg, /2\. b\.go:2\n {3}two\n {3}lines/);
});

test('b64ToBytes keeps raw bytes (UTF-8 split across chunks)', () => {
  const utf8 = Buffer.from('héllo ✓\x1b[0m');
  assert.deepEqual([...b64ToBytes(utf8.toString('base64'))], [...utf8]);
  assert.equal(b64ToBytes('').length, 0);
  // A multi-byte character split in two events stays intact when joined.
  const a = b64ToBytes(Buffer.from([0xe2, 0x9c]).toString('base64'));
  const b = b64ToBytes(Buffer.from([0x93]).toString('base64'));
  assert.equal(new TextDecoder().decode(new Uint8Array([...a, ...b])), '✓');
});

test('groupSessions orders groups and sorts newest first', () => {
  const list = [
    { ID: '1', State: 'running', Updated: '2026-11-01T10:00:00Z' },
    { ID: '2', State: 'waiting', Updated: '2026-11-01T09:00:00Z' },
    { ID: '3', State: 'running', Updated: '2026-11-01T11:00:00Z' },
    { ID: '4', State: 'accepted', Updated: '2026-11-01T08:00:00Z' },
    { ID: '5', State: 'something-new', Updated: '2026-11-01T07:00:00Z' },
    { ID: '6', State: 'failed', Updated: '2026-11-01T06:00:00Z' },
  ];
  const g = groupSessions(list);
  assert.deepEqual(g.map((x) => [x.key, x.items.map((s) => s.ID)]), [
    ['waiting', ['2']], ['active', ['3', '1', '5']], ['review', ['6']], ['done', ['4']],
  ]);
  assert.deepEqual(groupSessions([]), []);
  assert.equal(countWaiting(list), 1);
});

test('state helpers', () => {
  const cases = [
    ['waiting', 'warn', true, false], ['running', 'info', true, false], ['passed', 'ok', false, true],
    ['failed', 'err', false, true], ['queued', 'muted', false, false], ['accepted', 'ok', false, false],
  ];
  for (const [s, tone, live, follow] of cases) {
    assert.equal(stateTone(s), tone, s);
    assert.equal(isLive(s), live, s);
    assert.equal(canFollowUp(s), follow, s);
  }
});

test('quickKeyData', () => {
  assert.equal(quickKeyData('Ctrl-C'), '\x03');
  assert.equal(quickKeyData('↑'), '\x1b[A');
  assert.equal(quickKeyData('y', ''), 'y');
  assert.equal(quickKeyData('y', 'approval'), 'y\r');
  assert.equal(quickKeyData('Esc', 'approval'), '\x1b');
  assert.equal(quickKeyData('nope'), '');
});

test('parseRoute', () => {
  const cases = [
    ['', { view: 'home' }], ['#/sessions', { view: 'sessions' }], ['#/s/abc', { view: 'session', id: 'abc' }],
    ['#/s/a%2Fb/diff', { view: 'diff', id: 'a/b' }], ['#/models', { view: 'models' }], ['#/usage', { view: 'usage' }],
    ['#/login', { view: 'login' }], ['#/bogus', { view: 'notfound', path: '#/bogus' }], ['#/secrets', { view: 'secrets' }], ['#/settings/about', { view: 'settings', group: 'about' }],
  ];
  for (const [hash, want] of cases) assert.deepEqual(parseRoute(hash), want, hash);
});

test('formatting helpers', () => {
  const now = Date.parse('2026-11-01T12:00:00Z');
  assert.equal(relTime('2026-11-01T11:59:50Z', now), 'just now');
  assert.equal(relTime('2026-11-01T11:50:00Z', now), '10 min ago');
  assert.equal(relTime('0001-01-01T00:00:00Z', now), '–');
  assert.equal(fmtMiB(512), '512 MiB');
  assert.equal(fmtMiB(24576), '24.0 GiB');
  assert.equal(fmtNum(0), '–');
  assert.equal(fmtNum(42.345), '42.3');
  assert.equal(repoName('/home/u/Github/Cerberus/'), 'Cerberus');
});

test('workspace kind, label and actions', () => {
  assert.equal(workspaceKind({ Repo: '/h/r' }), 'repo'); // sessions from before workspaces
  assert.equal(workspaceKind({}), 'scratch');
  assert.equal(workspaceKind({ Workspace: 'folder', Dir: '/h/notes' }), 'folder');
  assert.equal(workspaceLabel({ Workspace: 'folder', Dir: '/h/notes' }), 'notes');
  assert.equal(workspaceLabel({ Workspace: 'scratch' }), 'scratch');
  assert.equal(workspaceLabel({ Workspace: 'repo', Repo: '/h/Topo', Dir: '/h/Topo' }), 'Topo');

  const repo = { Workspace: 'repo', State: 'passed' };
  const folder = { Workspace: 'folder', State: 'passed' };
  const scratch = { Workspace: 'scratch', State: 'running' };
  assert.ok(canDiff(repo) && canAcceptSession(repo));
  assert.ok(!canDiff(folder) && !canAcceptSession(folder) && !canSave(folder));
  assert.ok(!canDiff({ ...repo, State: 'accepted' }));
  assert.ok(canSave(scratch) && canSave({ ...scratch, State: 'passed' }));
  assert.ok(!canSave({ ...scratch, State: 'discarded' }));
  assert.ok(!canAcceptSession(scratch));
});

test('breadcrumbs from home', () => {
  assert.deepEqual(breadcrumbs('/home/u', '/home/u'), [{ name: '~', path: '/home/u' }]);
  assert.deepEqual(breadcrumbs('/home/u/Github/Topo/', '/home/u'), [
    { name: '~', path: '/home/u' },
    { name: 'Github', path: '/home/u/Github' },
    { name: 'Topo', path: '/home/u/Github/Topo' },
  ]);
  // /home/user2 is not inside /home/u.
  assert.deepEqual(breadcrumbs('/home/u2/x', '/home/u').map((c) => c.path), ['/', '/home', '/home/u2', '/home/u2/x']);
  assert.deepEqual(breadcrumbs('/srv', '').map((c) => c.name), ['/', 'srv']);
});

test('folder notes and joinPath', () => {
  assert.equal(folderNote(null), null);
  assert.equal(folderNote({ path: '/h/r', git: true }).tone, 'info');
  assert.match(folderNote({ path: '/h/r', git: true }).text, /worktree/);
  assert.equal(folderNote({ path: '/h/n', git: false }).tone, 'warn');
  assert.match(folderNote({ path: '/h/n', git: false }).text, /live/);
  assert.match(folderNote({ path: '/h/n', git: true }, 'save').text, /copied/);

  assert.equal(joinPath('/h/out', ''), '/h/out');
  assert.equal(joinPath('/h/out/', ' report/2026/ '), '/h/out/report/2026');
  assert.equal(joinPath('/h/out', '../escape'), '');
  assert.equal(joinPath('/h/out', 'a/./b'), '');
});
