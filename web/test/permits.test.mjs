// Saved permissions: the pure helpers behind "Always ▾", "Always allow ▾"
// and Settings → Permissions.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  normAlways, ruleWords, alwaysLabel, approvalMenuItems, chatMenuItems, sourceLabel, placeholderFor, normPermits, shortDate,
} from '../dist/assets/js/permits.js';
import { viewAllowed, tabVisible, canSavePermits, normMe } from '../dist/assets/js/users.js';
import { permPress, answeredLine } from '../dist/assets/js/chat.js';

test('normAlways: chat and session shapes, junk dropped', () => {
  assert.deepEqual(normAlways([{ text: 'Bash(ls)', tool: 'Bash', content: 'ls' }, { Text: 'Bash(wc:*)', Tool: 'Bash', Content: 'wc:*' }, null, { text: 'x' }, 5]),
    [{ text: 'Bash(ls)', tool: 'Bash', content: 'ls' }, { text: 'Bash(wc:*)', tool: 'Bash', content: 'wc:*' }]);
  assert.deepEqual(normAlways(undefined), []);
});

test('ruleWords and alwaysLabel', () => {
  assert.equal(ruleWords({ tool: 'Bash', content: 'wc -l *.go' }), 'wc -l *.go');
  assert.equal(ruleWords({ tool: 'Bash', content: 'go test:*' }), 'any “go test …” command');
  assert.equal(ruleWords({ tool: 'WebFetch', content: 'domain:docs.python.org' }), 'WebFetch on docs.python.org (and below)');
  assert.equal(ruleWords({ tool: 'cerberus_spawn', content: '' }), 'any cerberus_spawn');
  assert.equal(ruleWords({ tool: 'Read', content: 'dir:/srv/x', text: 'Read in /srv/x' }), 'Read in /srv/x');
  assert.equal(alwaysLabel({ tool: 'Bash', content: 'wc -l *.go' }), 'Every time: wc -l *.go');
});

test('approvalMenuItems: this run first, then one save per option', () => {
  const items = approvalMenuItems('run_command', [{ Text: 'Bash(wc -l *.go)', Tool: 'Bash', Content: 'wc -l *.go' }, { Text: 'Bash(wc:*)', Tool: 'Bash', Content: 'wc:*' }]);
  assert.equal(items.length, 3);
  assert.deepEqual([items[0].kind, items[0].answer, items[0].label], ['run', 'a', 'This run: any run_command']);
  assert.deepEqual([items[1].answer, items[1].option, items[1].label], ['save', 0, 'Every time: wc -l *.go']);
  assert.deepEqual([items[2].option, items[2].label], [1, 'Every time: any “wc …” command']);
  assert.equal(approvalMenuItems('', []).length, 1, 'nothing offered: only this run');
});

test('chatMenuItems: one entry per option', () => {
  const items = chatMenuItems([{ text: 'Bash(npm test)', tool: 'Bash', content: 'npm test' }]);
  assert.deepEqual(items.map((i) => [i.option, i.label]), [[0, 'Every time: npm test']]);
  assert.deepEqual(chatMenuItems(null), []);
});

test('Permissions panel helpers', () => {
  assert.equal(sourceLabel('chat'), 'LLM chat');
  assert.equal(sourceLabel('session'), 'Agent session');
  assert.equal(sourceLabel('settings'), 'Added here');
  assert.equal(placeholderFor('WebFetch'), 'domain:docs.python.org');
  assert.equal(placeholderFor('Nope'), '');
  const d = normPermits({ rules: [{ id: 'a', tool: 'Bash', content: 'ls', text: 'Bash(ls)', source: 'chat', created: '2026-11-03T10:00:00Z', uses: 2 }, { tool: 'x' }],
    policy: { allow: ['ls'], ask_always: null, deny: ['rm -rf /'] }, policy_link: '#/settings/safety' });
  assert.equal(d.rules.length, 1);
  assert.equal(d.rules[0].uses, 2);
  assert.deepEqual(d.policy, { allow: ['ls'], askAlways: [], deny: ['rm -rf /'] });
  assert.equal(d.link, '#/settings/safety');
  assert.deepEqual(normPermits(null), { rules: [], policy: { allow: [], askAlways: [], deny: [] }, link: '' });
  assert.equal(shortDate('0001-01-01T00:00:00Z'), '');
  assert.equal(shortDate(''), '');
  assert.match(shortDate('2026-11-03T10:00:00Z'), /2026/);
});

test('Settings → Permissions: users reach it, not the rest of Settings', () => {
  const user = normMe({ role: 'user' });
  const admin = normMe({ role: 'admin' });
  const chat = normMe({ role: 'chat' });
  const viewer = normMe({ role: 'viewer' });
  assert.equal(canSavePermits(user), true);
  assert.equal(canSavePermits(admin), true);
  assert.equal(canSavePermits(chat), false);
  assert.equal(canSavePermits(viewer), false);
  assert.equal(viewAllowed({ view: 'settings', group: 'permissions' }, user), true);
  assert.equal(viewAllowed({ view: 'settings', group: 'security' }, user), false);
  assert.equal(viewAllowed({ view: 'settings', group: 'permissions' }, viewer), false);
  assert.equal(viewAllowed({ view: 'settings', group: 'permissions' }, admin), true);
  assert.equal(tabVisible('settings', user), false, 'no Settings tab for users');
  assert.equal(tabVisible('settings', admin), true);
});

test('chat card: always answers in one press, its answered line', () => {
  const card = { shownAt: 0, command: true, pending: null };
  assert.equal(permPress(card, 'always', 100).action, 'ignore');
  assert.equal(permPress(card, 'always', 1000).action, 'answer');
  const line = answeredLine({ decision: 'always', by: 'user', rule: { text: 'Bash(npm test)' } }, { tool: 'Bash', input: { command: 'npm test' } });
  assert.match(line, /always allowed \(saved\).*Bash\(npm test\)/);
  const covered = answeredLine({ decision: 'once', by: 'rule', rule: { text: 'your saved permission Bash(npm test)' } }, { tool: 'Bash', input: { command: 'npm test' } });
  assert.match(covered, /\(saved permission\)/);
});
