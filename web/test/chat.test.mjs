// Unit tests for the chat view's pure helpers (chat.js): the stage state
// machine and frame timing, grid sizing, image scaling maths, permission
// arming, events → mood reactions, stored messages → transcript, status and
// end lines, model groups, history, find, corners and the SSE parser.
// Run: node --test web/test/*.test.mjs
import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  StageMachine, FAST_MS, SLOW_MS, QUIET_S, SLEEP_S, DONE_TO_IDLE_S,
  stageGrid, ART_MIN, ART_LIMIT, mergeRuns,
  scaleDims, imageEncoding, IMAGE_BYTES, dataURLBytes,
  permPress, isCommandAsk, ARM_MS, answeredLine, askHeadline, askDetails,
  moodEvents, contentText, messagesToItems, beforeLastUser, replayFrom,
  statusParts, endLine, thinkingLabel, keepTail, toolSummary, excerpt, fmtTokensShort, fmtCost, shortPath,
  modelGroups, isClaudeModel, switchesBackend, historyStep, pushHistory, findAll, findStep, findLabel,
  CORNERS, nextCorner, stagePrefs, parseSSE, agoText, deletePress, chatIdOk, modeLabel,
} from '../dist/assets/js/chat.js';
import { parseRoute, folderNote } from '../dist/assets/js/util.js';
import { dropPrompt } from '../dist/assets/js/files.js';

// ---------- state machine ----------

test('stage follows the chat: thinking, tool(prop), writing, done → idle after 15 s', () => {
  const m = new StageMachine(0);
  assert.equal(m.state, 'idle');
  m.event({ type: 'user', text: 'hi' }, 1);
  assert.equal(m.state, 'thinking');
  assert.equal(m.running, true);
  m.event({ type: 'tool', id: 't1', name: 'run_command', canon: 'Bash', input: {} }, 2);
  assert.deepEqual([m.state, m.tool], ['tool', 'Bash']);
  m.event({ type: 'tool', id: 't2', name: 'mystery' }, 2.5);
  assert.deepEqual([m.state, m.tool], ['tool', 'mystery'], 'no canon: the native name');
  m.event({ type: 'tool_done', id: 't1' }, 3);
  assert.equal(m.state, 'thinking');
  m.event({ type: 'text', text: 'ok' }, 4);
  assert.equal(m.state, 'writing');
  m.event({ type: 'done', ok: true }, 5);
  assert.deepEqual([m.state, m.running], ['done', false]);
  assert.equal(m.tick(5 + DONE_TO_IDLE_S - 0.1), 'done');
  assert.equal(m.tick(5 + DONE_TO_IDLE_S + 0.1), 'idle');
});

test('stage: errors and stops', () => {
  const m = new StageMachine(0);
  m.event({ type: 'user' }, 0);
  m.event({ type: 'done', ok: false, error: 'boom' }, 1);
  assert.equal(m.state, 'error');
  assert.equal(m.tick(1 + DONE_TO_IDLE_S + 1), 'idle');
  m.event({ type: 'user' }, 30);
  m.event({ type: 'done', ok: false, stopped: true }, 31);
  assert.equal(m.state, 'idle', 'a stop is not an error');
  m.event({ type: 'error', text: 'x' }, 32);
  assert.equal(m.state, 'error');
  assert.equal(m.event(null, 33), false);
});

test('frame timing: 10/s active, 2.5/s quiet, stopped after 120 s and when hidden', () => {
  const m = new StageMachine(0);
  assert.equal(m.interval(0), FAST_MS);
  assert.equal(FAST_MS, 100);
  assert.equal(SLOW_MS, 400);
  assert.equal(m.interval(QUIET_S - 0.1), FAST_MS);
  assert.equal(m.interval(QUIET_S + 0.1), SLOW_MS);
  assert.equal(m.interval(SLEEP_S + 1), 0, 'asleep');
  assert.equal(m.interval(1, true), 0, 'hidden: no timers');
  m.event({ type: 'user' }, 200);
  assert.equal(m.interval(200 + SLEEP_S * 2), FAST_MS, 'a running chat keeps drawing');
  m.event({ type: 'done', ok: true }, 500);
  assert.equal(m.interval(500 + SLEEP_S + 1), SLOW_MS, 'done still has to fall back to idle');
  m.tick(500 + SLEEP_S + 1);
  assert.equal(m.interval(500 + SLEEP_S + 1), 0);
  m.wake(1000);
  assert.equal(m.interval(1000), FAST_MS, 'a poke wakes it');
});

// ---------- grid ----------

test('stageGrid: natural size at scale 1, shrinks to 0.6, grows to 2.0, caps at the limit', () => {
  // A cell of 8 × 16 px.
  let g = stageGrid(8 * 160, 16 * 24, 8, 16);
  assert.deepEqual(g, { cols: 160, rows: 24, scale: 1 });
  // A wide, short stage keeps scale 1 and widens the scenery.
  g = stageGrid(8 * 300, 16 * 20, 8, 16);
  assert.deepEqual(g, { cols: 300, rows: 20, scale: 1 });
  // Too small for 48 × 14: the font shrinks (steps of 0.05), never under 0.6.
  g = stageGrid(8 * 48 * 0.75, 16 * 14 * 0.75, 8, 16);
  assert.equal(g.scale, 0.75);
  assert.deepEqual([g.cols, g.rows], ART_MIN);
  g = stageGrid(50, 30, 8, 16);
  assert.equal(g.scale, 0.6);
  assert.deepEqual([g.cols, g.rows], ART_MIN, 'never fewer cells than ART_MIN');
  // A tall column: more rows than 24 grow the font.
  g = stageGrid(8 * 100, 16 * 48, 8, 16);
  assert.equal(g.scale, 2);
  assert.deepEqual([g.cols, g.rows], [50, 24]);
  g = stageGrid(8 * 60, 16 * 36, 8, 16);
  assert.equal(g.scale, 1.25);
  // Huge stage: capped at ART_LIMIT.
  g = stageGrid(100000, 100000, 8, 16);
  assert.deepEqual([g.cols, g.rows], ART_LIMIT);
  // Every result is in range and fits ART_MIN at its scale unless at the floor.
  for (const [w, hh] of [[360, 130], [640, 220], [1280, 300], [500, 900], [300, 120]]) {
    const r = stageGrid(w, hh, 7.8, 15);
    assert.ok(r.scale >= 0.6 && r.scale <= 2, `${w}x${hh} scale ${r.scale}`);
    assert.ok(r.scale === 0.6 || (48 * 7.8 * r.scale <= w && 14 * 15 * r.scale <= hh), `${w}x${hh} fits`);
  }
});

test('mergeRuns joins same-colour neighbours and drops empty runs', () => {
  assert.deepEqual(mergeRuns([['ab', '#f00'], ['c', '#f00'], ['', '#0f0'], ['d', null], [' ', null], ['e', '#f00', '#000']]),
    [['abc', '#f00', null], ['d ', null, null], ['e', '#f00', '#000']]);
});

// ---------- images ----------

test('scaleDims fits the long side into 1568 px, never upscales', () => {
  assert.deepEqual(scaleDims(800, 600), { w: 800, h: 600, scaled: false });
  assert.deepEqual(scaleDims(1568, 10), { w: 1568, h: 10, scaled: false });
  assert.deepEqual(scaleDims(4032, 3024), { w: 1568, h: 1176, scaled: true });
  assert.deepEqual(scaleDims(3000, 6000), { w: 784, h: 1568, scaled: true });
  assert.deepEqual(scaleDims(20000, 10), { w: 1568, h: 1, scaled: true }, 'never 0 px');
});

test('imageEncoding: PNG up to 3.5 MB, JPEG q88 above', () => {
  assert.deepEqual(imageEncoding(IMAGE_BYTES), { type: 'image/png' });
  assert.deepEqual(imageEncoding(IMAGE_BYTES + 1), { type: 'image/jpeg', quality: 0.88 });
  assert.equal(IMAGE_BYTES, 3_500_000);
  assert.equal(dataURLBytes('data:image/png;base64,AAAA'), 3);
  assert.equal(dataURLBytes('data:image/png;base64,AA=='), 1);
  assert.equal(dataURLBytes('nope'), 0);
});

// ---------- permission arming ----------

test('permPress: armed after 600 ms; deny is one press', () => {
  const card = { shownAt: 1000, command: false, pending: null };
  assert.equal(permPress(card, 'once', 1000 + ARM_MS - 1).action, 'ignore');
  assert.equal(permPress(card, 'once', 1000 + ARM_MS).action, 'answer');
  const cmd = { shownAt: 0, command: true, pending: null };
  assert.equal(permPress(cmd, 'deny', 700).action, 'answer', 'deny never needs a second press');
  assert.equal(permPress(cmd, 'deny', 100).action, 'ignore', 'but waits for arming');
});

test('permPress: a run command needs two presses 400 ms – 5 s apart', () => {
  let card = { shownAt: 0, command: true, pending: null };
  let r = permPress(card, 'once', 1000);
  assert.equal(r.action, 'confirm');
  card = r.card;
  r = permPress(card, 'once', 1000 + 399);
  assert.equal(r.action, 'ignore', 'too soon (a key bounce)');
  card = r.card;
  r = permPress(card, 'once', 1000 + 400);
  assert.equal(r.action, 'answer');
  // Too late: the press starts over.
  card = { shownAt: 0, command: true, pending: null };
  card = permPress(card, 'session', 1000).card;
  r = permPress(card, 'session', 1000 + 5001);
  assert.equal(r.action, 'confirm');
  assert.equal(permPress(r.card, 'session', 1000 + 5001 + 500).action, 'answer');
  // Switching buttons starts over too.
  card = permPress({ shownAt: 0, command: true, pending: null }, 'once', 1000).card;
  assert.equal(permPress(card, 'session', 1600).action, 'confirm');
});

test('isCommandAsk and card lines', () => {
  assert.equal(isCommandAsk({ tool: 'Bash' }), true);
  assert.equal(isCommandAsk({ tool: 'Edit' }), false);
  assert.equal(askHeadline({ tool: 'Bash', input: { command: 'npm test' } }), 'npm test');
  assert.equal(askHeadline({ tool: 'Write', input: { file_path: '/h/x.txt' }, display: 'write_file' }), '/h/x.txt');
  const d = askDetails({ tool: 'Edit', input: { old_string: 'a\nb', new_string: 'c' } });
  assert.deepEqual(d, [['del', 'a'], ['del', 'b'], ['add', 'c']]);
  const many = askDetails({ input: { old_string: 'x\n'.repeat(20), new_string: '' } });
  assert.equal(many.length, 11);
  assert.equal(many[10][0], 'more');
  assert.deepEqual(askDetails({ input: { content: 'a\nb' } }), [['info', '2 lines · 3 chars']]);
  const req = { tool: 'Bash', input: { command: 'ls' } };
  assert.equal(answeredLine({ decision: 'once', by: 'user' }, req), '✓ allowed once · ls');
  assert.equal(answeredLine({ decision: 'session', rule: { text: 'Bash(ls)' } }, req), '✓ allowed for this session · ls · Bash(ls)');
  assert.equal(answeredLine({ decision: 'deny', note: 'not now' }, req), '✗ denied · ls · “not now”');
  assert.equal(answeredLine({ decision: 'deny', by: 'stop' }, req), '✗ denied · ls (stop)');
  assert.equal(answeredLine({ decision: 'once', by: 'rule' }, req), '✓ allowed once · ls (session rule)');
});

// ---------- events → mood ----------

test('moodEvents maps the stream to Mood.react calls', () => {
  const ctx = {};
  assert.deepEqual(moodEvents({ type: 'user', text: 'thanks!', images: 1 }, ctx), [['prompt', { text: 'thanks!', images: 1 }]]);
  assert.deepEqual(moodEvents({ type: 'text', text: 'Hel' }, ctx), []);
  moodEvents({ type: 'text', text: 'lo' }, ctx);
  assert.deepEqual(moodEvents({ type: 'tool', id: 'a', name: 'run_command', canon: 'Bash', input: { command: 'go test ./...' } }, ctx), [['tool_start', { tool: 'Bash' }]]);
  assert.deepEqual(moodEvents({ type: 'tool_done', id: 'a', output: 'ok  x 0.1s', is_error: false }, ctx),
    [['tool_ok', { tool: 'Bash', command: 'go test ./...', output: 'ok  x 0.1s' }]]);
  assert.deepEqual(moodEvents({ type: 'tool_done', id: 'zz', output: 'e', is_error: true }, ctx), [['tool_error', { tool: null, command: null, output: 'e' }]]);
  assert.deepEqual(moodEvents({ type: 'ask', req: {} }, ctx), [['permission', {}]]);
  assert.deepEqual(moodEvents({ type: 'answered', decision: 'deny', by: 'user' }, ctx), [['permission_denied', {}]]);
  assert.deepEqual(moodEvents({ type: 'answered', decision: 'once', by: 'user' }, ctx), [['permission_allowed', {}]]);
  assert.deepEqual(moodEvents({ type: 'answered', decision: 'once', by: 'rule' }, ctx), []);
  assert.deepEqual(moodEvents({ type: 'done', ok: true, seconds: 3.5 }, ctx), [['reply', { text: 'Hello' }], ['result_ok', { duration: 3.5 }]]);
  assert.deepEqual(moodEvents({ type: 'done', ok: false, stopped: true }, ctx), [['stopped', {}]]);
  assert.deepEqual(moodEvents({ type: 'done', ok: false }, ctx), [['result_error', {}]]);
  assert.deepEqual(moodEvents({ type: 'status' }, ctx), []);
});

// ---------- stored messages ----------

test('messagesToItems: user (with stored images), assistant, tool calls joined to results', () => {
  const msgs = [
    { role: 'system', content: 'sys' },
    { role: 'user', content: [{ type: 'text', text: '[image]' }, { type: 'text', text: 'what is this?' }] },
    { role: 'assistant', content: '', reasoning_content: 'hmm', tool_calls: [{ id: 'c1', type: 'function', function: { name: 'read_file', arguments: '{"path":"a.txt"}' } }, { id: 'c2', function: { name: 'run_command', arguments: 'not json' } }] },
    { role: 'tool', tool_call_id: 'c1', content: 'file text' },
    { role: 'tool', tool_call_id: 'c2', content: 'Error: exit 1' },
    { role: 'assistant', content: 'It is **a** cat.' },
  ];
  const items = messagesToItems(msgs);
  assert.deepEqual(items.map((i) => i.kind), ['user', 'assistant', 'tool', 'tool', 'assistant']);
  assert.deepEqual(items[0], { kind: 'user', text: 'what is this?', images: 1 });
  assert.equal(items[1].reasoning, 'hmm');
  assert.deepEqual(items[2].input, { path: 'a.txt' });
  assert.equal(items[2].output, 'file text');
  assert.equal(items[2].isError, false);
  assert.deepEqual(items[3].input, {});
  assert.equal(items[3].isError, true);
  assert.equal(items[4].text, 'It is **a** cat.');
  // tools are named as the live events name them (agent.Canon)
  assert.deepEqual([items[2].name, items[3].name], ['Read', 'Bash']);
  const denied = messagesToItems([
    { role: 'assistant', content: '', tool_calls: [{ id: 'd', function: { name: 'run_command', arguments: '{}' } }] },
    { role: 'tool', tool_call_id: 'd', content: 'The user denied this in the chat window: not now' },
  ]);
  assert.equal(denied[0].isError, true);
  assert.deepEqual(contentText([{ type: 'image_url', image_url: { url: 'data:' } }, { type: 'text', text: 'x' }]), { text: 'x', images: 1 });
  assert.deepEqual(contentText(null), { text: '', images: 0 });
});

test('a busy chat: messages before the last user message; replay from the last user event', () => {
  const msgs = [{ role: 'user', content: 'a' }, { role: 'assistant', content: 'b' }, { role: 'user', content: 'c' }, { role: 'assistant', content: 'partial' }];
  assert.deepEqual(beforeLastUser(msgs), msgs.slice(0, 2));
  assert.deepEqual(beforeLastUser([]), []);
  const evs = [{ type: 'user' }, { type: 'text' }, { type: 'done' }, { type: 'user' }, { type: 'tool' }];
  assert.equal(replayFrom(evs), 3);
  assert.equal(replayFrom([{ type: 'text' }]), 0);
});

// ---------- lines ----------

test('statusParts: model, ctx used/total %, elapsed, cost, mode, next picks', () => {
  const st = { model: 'qwen3', used: 12300, context: 32768, elapsed: 4.25, cost: 0.0123, mode: 'ask', queue: 1, waiting: 1 };
  assert.deepEqual(statusParts(st), ['🔒 1 waiting', 'qwen3', 'ctx 12.3k / 32.8k (38%)', '⏱ 4.3 s', '$0.01', 'Ask me', '1 queued']);
  assert.deepEqual(statusParts({ model: 'a', mode: 'auto' }, { nextModel: 'b', nextMode: 'plan' }), ['a → b', 'Auto → Plan']);
  assert.deepEqual(statusParts({ model: 'm', used: 500 }, { ctxOf: () => 1000 }), ['m', 'ctx 500 / 1k (50%)', 'Ask me']);
  assert.deepEqual(statusParts(null), ['Ask me']);
  assert.equal(fmtTokensShort(950), '950');
  assert.equal(fmtTokensShort(128000), '128k');
  assert.equal(fmtTokensShort(1_200_000), '1.2M');
  assert.equal(fmtCost(0), '');
  assert.equal(fmtCost(0.0042), '$0.0042');
  assert.equal(modeLabel('acceptEdits'), 'Accept edits');
});

test('endLine and the thinking fold', () => {
  assert.deepEqual(endLine({ ok: true, steps: 3, seconds: 4.2 }, 'qwen3'), { tone: 'ok', text: '✓ done · 3 steps · 4.2 s · qwen3' });
  assert.deepEqual(endLine({ ok: true, steps: 1, seconds: 0.5, cost: 0.25 }), { tone: 'ok', text: '✓ done · 1 step · 0.5 s · $0.25' });
  assert.deepEqual(endLine({ ok: false, stopped: true, seconds: 2 }), { tone: 'muted', text: '■ stopped · 2.0 s' });
  assert.deepEqual(endLine({ ok: false, error: 'no route' }), { tone: 'err', text: '✗ failed: no route' });
  assert.equal(thinkingLabel('a\nb\nc'), 'thinking · 3 lines');
  assert.equal(thinkingLabel(''), 'thinking · 0 lines');
  assert.equal(thinkingLabel('one'), 'thinking · 1 line');
  assert.equal(keepTail('abcdef', 3), 'def');
});

test('toolSummary and excerpt', () => {
  assert.equal(toolSummary('Bash', { command: 'go   test\n./...' }), 'go test ./...');
  assert.equal(toolSummary('Read', { file_path: '/home/u/x.go' }, '/home/u'), '~/x.go');
  assert.equal(toolSummary('Grep', { pattern: 'foo', glob: '*.go', path: '/home/u/p' }, '/home/u'), '/foo/ *.go ~/p', 'whitespace collapsed');
  assert.equal(toolSummary('TodoWrite', { todos: [{ status: 'completed' }, {}] }), '1/2 todos');
  assert.equal(toolSummary('mcp__x__y', { q: 'hello' }), 'hello');
  assert.equal(toolSummary('Bash', null), '');
  assert.equal(toolSummary('Bash', { command: 'x'.repeat(500) }).length, 160);
  assert.equal(shortPath('/home/u', '/home/u'), '~');
  assert.equal(shortPath('/home/user2/x', '/home/u'), '/home/user2/x');
  const ex = excerpt(Array.from({ length: 20 }, (_, i) => `l${i}`).join('\n'));
  assert.equal(ex.text.split('\n').length, 14);
  assert.equal(ex.more, true);
  assert.deepEqual(excerpt('short'), { text: 'short', more: false });
});

// ---------- models ----------

test('modelGroups: local by backend, API by provider, Claude when present', () => {
  const data = {
    default: 'qwen3',
    models: [
      { ID: 'qwen3', Backend: 'llamacpp', Local: true, Loaded: false, Ctx: 32768 },
      { ID: 'gemma', Backend: 'llamacpp', Local: true, Loaded: true },
      { ID: 'phi', Backend: 'ollama', Local: true },
      { ID: 'openrouter/gpt-x', Backend: 'openai', Local: false },
    ],
    claude: { available: true, models: ['claude', 'claude:opus'] },
  };
  const g = modelGroups(data);
  assert.deepEqual(g.map((x) => x.label), ['Local · llama.cpp', 'Local · Ollama', 'API · openrouter', 'Claude · Claude Code']);
  assert.deepEqual(g[0].items.map((i) => [i.id, i.loaded, i.isDefault]), [['gemma', true, false], ['qwen3', false, true]]);
  assert.equal(g[0].items[1].ctx, 32768);
  assert.equal(g[3].items[0].label, 'claude (its default)');
  assert.deepEqual(modelGroups({ models: [], claude: { available: false, models: ['claude'] } }), []);
  assert.equal(isClaudeModel('Claude:sonnet'), true);
  assert.equal(isClaudeModel('claudette'), false);
  assert.equal(switchesBackend('qwen3', 'claude:opus', true), true);
  assert.equal(switchesBackend('qwen3', 'claude:opus', false), false, 'an empty chat just changes');
  assert.equal(switchesBackend('claude', 'claude:opus', true), false);
});

// ---------- history, find, corners ----------

test('input history: Up/Down through sent messages back to the draft', () => {
  const hist = ['one', 'two'];
  let s = historyStep(hist, 2, -1, 'draft');
  assert.deepEqual(s, { idx: 1, value: 'two' });
  s = historyStep(hist, s.idx, -1, 'draft');
  assert.deepEqual(s, { idx: 0, value: 'one' });
  s = historyStep(hist, s.idx, -1, 'draft');
  assert.deepEqual(s, { idx: 0, value: 'one' }, 'stays at the oldest');
  s = historyStep(hist, 1, 1, 'draft');
  assert.deepEqual(s, { idx: 2, value: 'draft' });
  assert.deepEqual(historyStep([], 0, -1, 'd'), { idx: 0, value: 'd' });
  assert.deepEqual(pushHistory(['a', 'b'], ' a '), ['b', 'a'], 'moved to the end, no duplicate');
  assert.deepEqual(pushHistory(['a'], '  '), ['a']);
  assert.equal(pushHistory(Array.from({ length: 100 }, (_, i) => String(i)), 'x').length, 100);
});

test('find: case-insensitive offsets, wrap-around steps, N / M label', () => {
  assert.deepEqual(findAll('Foo foo FOO', 'foo'), [0, 4, 8]);
  assert.deepEqual(findAll('aaaa', 'aa'), [0, 2]);
  assert.deepEqual(findAll('x', ''), []);
  assert.equal(findStep(-1, 3, 1), 0);
  assert.equal(findStep(-1, 3, -1), 2);
  assert.equal(findStep(2, 3, 1), 0);
  assert.equal(findStep(0, 3, -1), 2);
  assert.equal(findStep(0, 0, 1), -1);
  assert.equal(findLabel(0, 3), '1 / 3');
  assert.equal(findLabel(-1, 0), '0 / 0');
});

test('corner card: cycling and remembered prefs', () => {
  assert.equal(nextCorner('top-right'), 'bottom-right');
  assert.equal(nextCorner('top-left'), 'top-right');
  assert.equal(nextCorner('nonsense'), CORNERS[0]);
  assert.deepEqual(stagePrefs(null), { corner: false, where: 'top-right' });
  assert.deepEqual(stagePrefs({ corner: 1, where: 'bottom-left' }), { corner: true, where: 'bottom-left' });
  assert.deepEqual(stagePrefs({ where: 'middle' }), { corner: false, where: 'top-right' });
});

// ---------- SSE ----------

test('parseSSE: complete events, multi-line data, comments, the unfinished rest', () => {
  const { events, rest } = parseSSE('event: text\ndata: {"text":"a"}\n\n: keep-alive\n\nevent: done\ndata: {"ok":true}\nid: 4\n\nevent: te');
  assert.deepEqual(events, [{ event: 'text', data: '{"text":"a"}', id: '' }, { event: 'done', data: '{"ok":true}', id: '4' }]);
  assert.equal(rest, 'event: te');
  assert.deepEqual(parseSSE('data: a\ndata: b\r\n\r\n').events, [{ event: 'message', data: 'a\nb', id: '' }]);
  assert.deepEqual(parseSSE('').events, []);
});

// ---------- recent, routes ----------

test('recent chats: ago text, two-press delete, ids', () => {
  const now = 1_000_000_000_000;
  assert.equal(agoText(now / 1000 - 5, now), 'just now');
  assert.equal(agoText(now / 1000 - 600, now), '10 min ago');
  assert.equal(agoText(now / 1000 - 7200, now), '2 h ago');
  assert.equal(agoText(now / 1000 - 3 * 86400, now), '3 d ago');
  let r = deletePress(0, 1000);
  assert.deepEqual(r, { delete: false, armedAt: 1000 });
  r = deletePress(r.armedAt, 3500);
  assert.deepEqual(r, { delete: true, armedAt: 0 });
  assert.equal(deletePress(1000, 4001).delete, false, 'disarmed after 3 s');
  assert.equal(chatIdOk('chat-20261101-120000-abcdef'), true);
  assert.equal(chatIdOk('chat-2026-x'), false);
  assert.equal(chatIdOk('../etc'), false);
});

test('routes, folder note and drop prompt for the chat', () => {
  assert.deepEqual(parseRoute('#/chat'), { view: 'chat' });
  assert.deepEqual(parseRoute('#/chat/new'), { view: 'chat', id: 'new' });
  assert.deepEqual(parseRoute('#/chat/chat-20261101-120000-abcdef'), { view: 'chat', id: 'chat-20261101-120000-abcdef' });
  assert.match(folderNote({ path: '/x', git: true }, 'chat').text, /live/);
  assert.equal(dropPrompt({ kind: 'chat' }), 'Drop images to attach them to the chat');
});
