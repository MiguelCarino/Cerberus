// Deleting sessions and models: the confirm wording and the bulk body.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { deleteMessage, bulkBody } from '../dist/assets/js/control.js';
import { deleteModelMessage } from '../dist/assets/js/models.js';

test('deleteMessage: one session per workspace kind', () => {
  const repo = deleteMessage([{ ID: 's1', Title: 'Fix login', Workspace: 'repo', State: 'passed' }]);
  assert.equal(repo.title, 'Delete session');
  assert.equal(repo.ok, 'Delete');
  assert.match(repo.message, /^“Fix login”\. /);
  assert.match(repo.message, /worktree is deleted; one with work that was not accepted is kept/);
  assert.doesNotMatch(repo.message, /stopped first/);

  const scratch = deleteMessage([{ ID: 's2', Name: 'Try', State: 'running' }]);
  assert.match(scratch.message, /It is stopped first\./);
  assert.match(scratch.message, /scratch folder is deleted/);

  const folder = deleteMessage([{ ID: 's3', Workspace: 'folder', Dir: '/home/me/x', State: 'stopped' }]);
  assert.match(folder.message, /Your own folders are not touched\./);
  assert.doesNotMatch(folder.message, /scratch|worktree/);
});

test('deleteMessage: several sessions', () => {
  const m = deleteMessage([
    { ID: 'a', Workspace: 'repo', State: 'running' },
    { ID: 'b', Workspace: 'folder', State: 'waiting' },
    { ID: 'c', State: 'failed' },
  ]);
  assert.equal(m.title, 'Delete 3 sessions');
  assert.equal(m.ok, 'Delete 3');
  assert.match(m.message, /^2 still running are stopped first\. Their transcripts/);
  assert.match(m.message, /worktree/);
  assert.match(m.message, /scratch folder/);
  assert.match(m.message, /Your own folders are not touched/);
});

test('bulkBody carries Force for delete', () => {
  assert.equal(bulkBody(['a'], { Kind: 'delete', Force: true }).op.Force, true);
  assert.equal(bulkBody(['a'], { Kind: 'delete' }).op.Force, false);
});

test('deleteModelMessage', () => {
  const m = deleteModelMessage({ ID: 'qwen3-coder:30b', Backend: 'ollama', SizeMiB: 18944, Loaded: true });
  assert.equal(m.title, 'Delete qwen3-coder:30b');
  assert.equal(m.message, 'Ollama removes it. That frees about 18.5 GB. It is unloaded first. To use it again you have to download it again.');
  assert.match(deleteModelMessage({ ID: 'x', Backend: 'llamacpp', SizeMiB: 500 }).message, /^Its \.gguf file is removed from your models folder\. That frees about 500 MB\./);
  assert.match(deleteModelMessage({ ID: 'y', Backend: 'lmstudio' }).message, /LM Studio's models folder\. To use it again/);
});
