// The shared store passes flow run events from /api/events on to its
// subscribers ('flowrun'), with fake EventSource and DOM globals.
import { test } from 'node:test';
import assert from 'node:assert/strict';

const sources = [];
class FakeEventSource {
  constructor(url) { this.url = url; this.listeners = {}; sources.push(this); }
  addEventListener(name, fn) { (this.listeners[name] ||= []).push(fn); }
  close() { this.closed = true; }
  fire(name, data) { for (const fn of this.listeners[name] || []) fn({ data }); }
}
globalThis.EventSource = FakeEventSource;
globalThis.document = { hidden: false, addEventListener() {} };
globalThis.window = { addEventListener() {}, dispatchEvent() {} };
globalThis.fetch = async () => ({ ok: true, status: 200, statusText: 'OK', text: async () => '[]' });

const { store } = await import('../dist/assets/js/store.js');

test('flowrun events reach subscribers; malformed ones do not', () => {
  const got = [];
  const off = store.subscribe((kind, payload) => { if (kind === 'flowrun' || kind === 'resync') got.push([kind, payload]); });
  store.startEvents();
  const es = sources.find((s) => s.url === '/api/events');
  assert.ok(es, 'the event stream opened');
  es.fire('flowrun', JSON.stringify({ run: 'fr-1', flow: 'fl-a', state: 'running', step: 'plan' }));
  es.fire('flowrun', 'not json');
  es.fire('flowrun', JSON.stringify({ flow: 'fl-a' })); // no run: ignored
  es.fire('resync', JSON.stringify({ what: 'flows' }));
  assert.deepEqual(got, [
    ['flowrun', { run: 'fr-1', flow: 'fl-a', state: 'running', step: 'plan' }],
    ['resync', { what: 'flows' }],
  ]);
  off();
  store.stopEvents();
  assert.equal(es.closed, true);
});
