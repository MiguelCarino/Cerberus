// Unit tests for the file drop helpers (day phase 2): size formatting,
// destination naming, the bridge origin rule, preview kinds, workspace
// paths, upload outcomes and progress. Run: node --test web/test/
import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  fmtBytes, normRel, joinRel, parentRel, relCrumbs, destinationLabel, dropPrompt, originAllowed, originHost,
  extOf, previewKind, isFileDrag, uploadError, uploadSummary, fileProgress, fileKey, addFiles, pastedName,
  sendToAgentText, insertedPaths, filesPath, rawPath, deletePath, folderNameError, TEXT_PREVIEW_BYTES,
  tooLargeFor, deletesWithContents, bridgeTargets,
} from '../dist/assets/js/files.js';
import { parseRoute } from '../dist/assets/js/util.js';

test('fmtBytes uses binary units and trims needless decimals', () => {
  assert.equal(fmtBytes(0), '0 B');
  assert.equal(fmtBytes(512), '512 B');
  assert.equal(fmtBytes(1023), '1023 B');
  assert.equal(fmtBytes(1024), '1 KiB');
  assert.equal(fmtBytes(1536), '1.5 KiB');
  assert.equal(fmtBytes(34 * 1024 + 100), '34 KiB');
  assert.equal(fmtBytes(200 * 1024 * 1024), '200 MiB');
  assert.equal(fmtBytes(2.25 * 1024 ** 3), '2.3 GiB');
  assert.equal(fmtBytes(5 * 1024 ** 5), '5120 TiB');
  assert.equal(fmtBytes(-1), '–');
  assert.equal(fmtBytes(undefined), '–');
  assert.equal(fmtBytes(NaN), '–');
});

test('workspace paths: normalise, join, parent, refuse climbing', () => {
  assert.equal(normRel(''), '.');
  assert.equal(normRel('.'), '.');
  assert.equal(normRel('/a//b/./c/'), 'a/b/c');
  assert.equal(normRel('a\\b'), 'a/b');
  assert.equal(normRel('a/../b'), '');
  assert.equal(joinRel('.', 'x'), 'x');
  assert.equal(joinRel('inbox', 'x.png'), 'inbox/x.png');
  assert.equal(joinRel('inbox', '.'), 'inbox');
  assert.equal(joinRel('inbox', '../etc'), '');
  assert.equal(parentRel('a/b/c'), 'a/b');
  assert.equal(parentRel('a'), '.');
  assert.equal(parentRel('.'), '.');
  assert.deepEqual(relCrumbs('.'), [{ name: 'workspace', rel: '.' }]);
  assert.deepEqual(relCrumbs('docs/img'), [
    { name: 'workspace', rel: '.' }, { name: 'docs', rel: 'docs' }, { name: 'img', rel: 'docs/img' }]);
});

test('destinationLabel and dropPrompt name where files go', () => {
  assert.equal(destinationLabel({ kind: 'session', name: 'Fix navbar' }), 'Fix navbar › inbox');
  assert.equal(destinationLabel({ kind: 'session', name: 'Fix navbar', uploadDir: 'drop' }), 'Fix navbar › drop');
  assert.equal(destinationLabel({ kind: 'session', name: 'S', dir: 'docs/img/' }), 'S › docs/img');
  assert.equal(destinationLabel({ kind: 'session', name: 'S', dir: '.' }), 'S › workspace root');
  assert.equal(destinationLabel({ kind: 'session', name: '  ' }), 'this session › inbox');
  assert.equal(destinationLabel({ kind: 'task' }), 'a new task');
  assert.equal(destinationLabel(null), '');
  assert.equal(dropPrompt({ kind: 'task' }), 'Drop to attach to a new task');
  assert.equal(dropPrompt({ kind: 'session', name: 'S' }), 'Drop to upload to S › inbox');
});

test('originAllowed: same origin and https *.carino.systems only', () => {
  const self = 'http://box.lan:7340';
  assert.equal(originAllowed(self, self), true);
  assert.equal(originAllowed('https://files.carino.systems', self), true);
  assert.equal(originAllowed('https://a.b.carino.systems', self), true);
  assert.equal(originAllowed('https://FILES.carino.systems', self), false); // origins are lower-case; not this form
  for (const bad of [
    'http://files.carino.systems', // not https
    'https://files.carino.systems:8443', // odd port
    'https://evilcarino.systems',
    'https://carino.systems.evil.com',
    'https://carino.systems', // the apex is not "under" it
    'https://.carino.systems',
    'https://files.carino.systems/path',
    'null', '', undefined, 'not a url', 'http://box.lan:7341',
  ]) assert.equal(originAllowed(bad, self), false, String(bad));
});

test('originHost labels a handoff by where it came from', () => {
  assert.equal(originHost('https://files.carino.systems'), 'files.carino.systems');
  assert.equal(originHost('http://box.lan:7340'), 'box.lan:7340');
  assert.equal(originHost('weird'), 'weird');
});

test('previewKind: server flags, then MIME, then extension', () => {
  assert.equal(previewKind({ Name: 'a.bin', Image: true }), 'image');
  assert.equal(previewKind({ Name: 'a.weird', Text: true }), 'text');
  assert.equal(previewKind({ Name: 'x', Mime: 'application/pdf' }), 'pdf');
  assert.equal(previewKind({ Name: 'x', Mime: 'audio/mpeg' }), 'audio');
  assert.equal(previewKind({ Name: 'x', Mime: 'video/mp4; codecs=avc1' }), 'video');
  assert.equal(previewKind({ Name: 'x', Mime: 'application/json' }), 'text');
  assert.equal(previewKind({ Name: 'x', Mime: 'application/ld+json' }), 'text');
  assert.equal(previewKind({ Name: 'main.go' }), 'text');
  assert.equal(previewKind({ Name: 'Makefile' }), 'text');
  assert.equal(previewKind({ Name: '.env' }), 'text');
  assert.equal(previewKind({ Name: 'photo.JPG' }), 'image');
  assert.equal(previewKind({ Name: 'clip.webm' }), 'video');
  assert.equal(previewKind({ Name: 'song.flac' }), 'audio');
  assert.equal(previewKind({ Name: 'doc.pdf' }), 'pdf');
  assert.equal(previewKind({ Name: 'app.exe', Mime: 'application/octet-stream' }), 'none');
  assert.equal(previewKind({ Name: 'dir', Dir: true }), 'none');
  assert.equal(previewKind({ Name: 'a.png', Outside: true, Image: true }), 'none');
  assert.equal(previewKind({ name: 'shot.png', type: 'image/png' }), 'image'); // a File
  assert.equal(previewKind(null), 'none');
  assert.equal(extOf('archive.tar.gz'), 'gz');
  assert.equal(extOf('noext'), '');
  assert.equal(extOf('trailing.'), '');
  assert.equal(TEXT_PREVIEW_BYTES, 262144);
});

test('isFileDrag looks for the Files type', () => {
  assert.equal(isFileDrag(['text/plain', 'Files']), true);
  assert.equal(isFileDrag(['text/plain']), false);
  assert.equal(isFileDrag(null), false);
});

test('uploadError gives a short reason per status', () => {
  assert.equal(uploadError(413, ''), 'too large');
  assert.equal(uploadError(413, 'limit is 200 MiB'), 'too large: limit is 200 MiB');
  assert.equal(uploadError(400, 'bad name'), 'refused: bad name');
  assert.equal(uploadError(400, ''), 'refused name');
  assert.equal(uploadError(0, ''), 'network error');
  assert.equal(uploadError(401, 'x'), 'sign in required');
  assert.equal(uploadError(500, ''), 'HTTP 500');
});

test('uploadSummary condenses per-file results', () => {
  assert.deepEqual(uploadSummary([{ name: 'a', ok: true }, { name: 'b', ok: true }], 'S › inbox'),
    { kind: 'ok', text: 'Uploaded 2 files to S › inbox' });
  assert.deepEqual(uploadSummary([{ name: 'a', ok: true }]), { kind: 'ok', text: 'Uploaded 1 file' });
  assert.deepEqual(uploadSummary([{ name: 'a', ok: true }, { name: 'big.iso', ok: false, error: 'too large' },
    { name: 'c', ok: false, error: 'refused name' }]),
  { kind: 'err', text: 'Uploaded 1 of 3; big.iso: too large (+1 more)' });
  assert.deepEqual(uploadSummary([{ name: 'x', ok: false, error: 'too large' }]), { kind: 'err', text: 'Upload failed; x: too large' });
  assert.deepEqual(uploadSummary([{ name: 'x', cancelled: true }]), { kind: 'info', text: 'Upload cancelled' });
  assert.deepEqual(uploadSummary([{ name: 'a', ok: true }, { name: 'x', cancelled: true }]),
    { kind: 'warn', text: 'Uploaded 1 file; 1 cancelled' });
  assert.equal(uploadSummary([]).kind, 'info');
});

test('fileProgress spreads one request over its files in order', () => {
  assert.deepEqual(fileProgress([100, 100], 0, 220), [0, 0]);
  assert.deepEqual(fileProgress([100, 100], 110, 220), [1, 0]);
  assert.deepEqual(fileProgress([100, 300], 110, 440), [1, 0]);
  assert.deepEqual(fileProgress([100, 300], 220, 440), [1, 1 / 3]);
  assert.deepEqual(fileProgress([100, 100], 220, 220), [1, 1]);
  assert.deepEqual(fileProgress([0, 100], 10, 100), [0, 0.1]);
  assert.deepEqual(fileProgress([0, 100], 100, 100), [1, 1]);
  assert.deepEqual(fileProgress([50], 10, 0), [0]); // length unknown
});

test('addFiles skips files already attached', () => {
  const a = { name: 'a.png', size: 3, lastModified: 1 };
  const a2 = { name: 'a.png', size: 3, lastModified: 1 };
  const b = { name: 'a.png', size: 4, lastModified: 1 };
  assert.notEqual(fileKey(a), fileKey(b));
  assert.deepEqual(addFiles([a], [a2, b]), [a, b]);
  assert.deepEqual(addFiles([], undefined), []);
});

test('pastedName replaces the generic clipboard name', () => {
  const at = new Date(2026, 10, 1, 9, 5, 7);
  assert.equal(pastedName('image.png', 'image/png', at), 'pasted-20261101-090507.png');
  assert.equal(pastedName('', 'image/jpeg', at), 'pasted-20261101-090507.jpg');
  assert.equal(pastedName('notes.txt', 'text/plain', at), 'notes.txt');
});

test('sendToAgentText and insertedPaths', () => {
  assert.equal(sendToAgentText('inbox/a.png'), 'Take a look at ./inbox/a.png');
  assert.equal(sendToAgentText(['a', 'b/c']), 'Take a look at these files: ./a, ./b/c');
  assert.equal(sendToAgentText(['../x']), '');
  assert.equal(insertedPaths('', ['inbox/a.png']), 'inbox/a.png');
  assert.equal(insertedPaths('look at', ['inbox/a.png', 'inbox/b.png']), 'look at inbox/a.png inbox/b.png');
  assert.equal(insertedPaths('look at ', ['x']), 'look at x');
  assert.equal(insertedPaths('keep', []), 'keep');
});

test('API paths encode ids and paths', () => {
  assert.equal(filesPath('ab', '.'), '/api/sessions/ab/files');
  assert.equal(filesPath('ab', 'docs/a b'), '/api/sessions/ab/files?dir=docs%2Fa%20b');
  assert.equal(rawPath('a/b', 'inbox/x&y.png'), '/api/sessions/a%2Fb/files/raw?path=inbox%2Fx%26y.png');
  assert.equal(rawPath('ab', 'x.pdf', true), '/api/sessions/ab/files/raw?path=x.pdf&download=1');
  assert.equal(deletePath('ab', 'inbox/x'), '/api/sessions/ab/files?path=inbox%2Fx');
});

test('folderNameError', () => {
  assert.equal(folderNameError('reference'), '');
  assert.match(folderNameError(''), /name/);
  assert.match(folderNameError('a/b'), /cannot contain/);
  assert.match(folderNameError('..'), /dot/);
  assert.match(folderNameError('.hidden'), /dot/);
  assert.match(folderNameError('é'.repeat(91)), /too long/);
});

test('parseRoute: the session Files tab has its own address', () => {
  assert.deepEqual(parseRoute('#/s/abc/files'), { view: 'session', id: 'abc', tab: 'files' });
  assert.deepEqual(parseRoute('#/s/abc'), { view: 'session', id: 'abc' });
  assert.deepEqual(parseRoute('#carino-bridge'), { view: 'sessions' });
});

test('uploadError does not repeat the server\'s "too large"', () => {
  assert.equal(uploadError(413, 'too large: the 2 GiB upload limit is reached'), 'too large: the 2 GiB upload limit is reached');
  assert.equal(uploadError(413, 'Too large'), 'Too large');
});

test('tooLargeFor refuses files over the per-file limit', () => {
  assert.equal(tooLargeFor(2048, 200), '');
  assert.equal(tooLargeFor(300 * 1024 * 1024, 200), 'too large (limit 200 MiB)');
  assert.equal(tooLargeFor(200 * 1024 * 1024, 200), '');
  assert.equal(tooLargeFor(1e12, 0), ''); // no known limit: the server decides
});

test('deletesWithContents allows recursive deletes inside the inbox only', () => {
  assert.equal(deletesWithContents('inbox/old', 'inbox'), true);
  assert.equal(deletesWithContents('inbox', 'inbox'), false);
  assert.equal(deletesWithContents('inboxes/x', 'inbox'), false);
  assert.equal(deletesWithContents('src/sub', 'inbox'), false);
  assert.equal(deletesWithContents('up/a/b', 'up/a'), true);
  assert.equal(deletesWithContents('inbox/x'), true);
  assert.equal(deletePath('s1', 'inbox/old', true), '/api/sessions/s1/files?path=inbox%2Fold&recursive=1');
  assert.equal(deletePath('s1', 'a.txt'), '/api/sessions/s1/files?path=a.txt');
});

test('bridgeTargets keeps open sessions that have a workspace, newest first', () => {
  const closed = (st) => st === 'accepted' || st === 'discarded';
  const list = [
    { ID: 'a', State: 'running', Worktree: '/w/a', Updated: '2026-11-01T10:00:00Z' },
    { ID: 'b', State: 'queued', Worktree: '', Updated: '2026-11-01T12:00:00Z' },
    { ID: 'c', State: 'accepted', Worktree: '/w/c', Updated: '2026-11-01T13:00:00Z' },
    { ID: 'd', State: 'idle', Worktree: '/w/d', Updated: '2026-11-01T11:00:00Z' },
  ];
  assert.deepEqual(bridgeTargets(list, closed).map((s) => s.ID), ['d', 'a']);
  assert.deepEqual(bridgeTargets(null, closed), []);
});
