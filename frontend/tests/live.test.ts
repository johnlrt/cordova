import { test } from 'node:test';
import assert from 'node:assert/strict';
import { ApiFailure } from '../src/lib/api/client.ts';
import { decodeUsagePage } from '../src/lib/api/usage.ts';
import { decodeLiveFrame, isFirstReadyFrame, isNextSequence } from '../src/lib/live/protocol.ts';
import { InvalidationDeduper } from '../src/lib/live/invalidation.ts';
import { LiveConnection, retryDelay } from '../src/lib/live/connection.ts';

const sessionId = 'c9eb01e9-899f-4edb-87fd-66e585ec9f79';
const accountId = '4e345523-1a8c-4f4b-9d48-0593cf3e7eca';
const resourceId = '706134d8-0590-437c-ab24-bb9c396c815b';
const frame = (type: string, payload: Record<string, unknown>, seq = 1, session = sessionId) => JSON.stringify({
  v: 1, type, message_id: accountId, request_id: null, session_id: session, seq,
  sent_at: '2026-10-09T10:00:00Z', payload, error: null
});

test('live frame validation accepts own-account invalidations and rejects malformed wire data', () => {
  const raw = frame('live.event', { event_id: 'stream-42', topic: 'usage', resource_id: resourceId, revision: 3, action: 'invalidate' }, 2);
  assert.equal(decodeLiveFrame(raw).type, 'live.event');
  assert.throws(() => decodeLiveFrame(raw.replace('"revision":3', '"revision":0')), ApiFailure);
  assert.throws(() => decodeLiveFrame(raw.replace('"resource_id":"706134d8-0590-437c-ab24-bb9c396c815b",', '')), ApiFailure);
  assert.throws(() => decodeLiveFrame(raw.replace('"topic":"usage"', '"topic":"node"')), ApiFailure);
  assert.throws(() => decodeLiveFrame(raw.replace('"v":1', '"v":1,"v":1')), ApiFailure);
  assert.throws(() => decodeLiveFrame(raw.replace('"sent_at":"2026-10-09T10:00:00Z"', '"sent_at":"tomorrow"')), ApiFailure);
  assert.throws(() => decodeLiveFrame(frame('live.resync', { reason: 'expired' })), ApiFailure);
});

test('invalidation dedupe drops duplicate IDs and stale or reordered revisions', () => {
  const deduper = new InvalidationDeduper();
  const event = { event_id: 'one', topic: 'usage' as const, resource_id: resourceId, revision: 2, action: 'invalidate' as const };
  assert.equal(deduper.accept(event), true);
  assert.equal(deduper.accept(event), false);
  assert.equal(deduper.accept({ ...event, event_id: 'older', revision: 1 }), false);
  assert.equal(deduper.accept({ ...event, event_id: 'newer', revision: 3 }), true);
  deduper.reset();
  assert.equal(deduper.accept(event), true);
});

test('live reconnect delay is full-jitter bounded and never exceeds sixty seconds', () => {
  assert.equal(retryDelay(0, () => 0), 0);
  assert.equal(retryDelay(2, () => 0.5), 2000);
  assert.equal(retryDelay(99, () => 0.999), 59_940);
});

test('usage snapshots reject numeric money, extra fields and invalid states', () => {
  const row = {
    request_id: resourceId, api_key_id: sessionId, model_id: 'cordova-chat-small-v1', status: 'completed', input_tokens: 4, output_tokens: 5,
    charged_piconero: '0', latency_ms: 18, created_at: '2026-10-09T10:00:00Z', finished_at: '2026-10-09T10:00:01Z'
  };
  assert.equal(decodeUsagePage({ data: [row], next_cursor: null }).data[0].status, 'completed');
  assert.throws(() => decodeUsagePage({ data: [{ ...row, status: 'settled' }], next_cursor: null }), ApiFailure);
  assert.throws(() => decodeUsagePage({ data: [{ ...row, api_key_id: 'not-a-uuid' }], next_cursor: null }), ApiFailure);
  assert.throws(() => decodeUsagePage({ data: [{ ...row, charged_piconero: 0 }], next_cursor: null }), ApiFailure);
  assert.throws(() => decodeUsagePage({ data: [{ ...row, status: 'unknown' }], next_cursor: null }), ApiFailure);
  assert.throws(() => decodeUsagePage({ data: [{ ...row, owner_account_id: accountId }], next_cursor: null }), ApiFailure);
});

test('inbound live frame requires sequence numbers to be within the v1 range', () => {
  const valid = frame('live.pong', { time: '2026-10-09T10:00:00Z' }, 1);
  assert.equal(decodeLiveFrame(valid).type, 'live.pong');
  assert.throws(() => decodeLiveFrame(valid.replace('"seq":1', '"seq":4294967296')), ApiFailure);
});

test('live.ready is bound to the login ticket, expected first sequence and account', () => {
  const readyRaw = JSON.stringify({
    v: 1, type: 'live.ready', message_id: accountId, request_id: resourceId, session_id: sessionId, seq: 1,
    sent_at: '2026-10-09T10:00:00Z',
    payload: { session_id: sessionId, account_id: accountId, heartbeat_interval_ms: 15_000 }, error: null
  });
  const ready = decodeLiveFrame(readyRaw);
  assert.equal(isFirstReadyFrame(ready, resourceId, accountId), true);
  assert.equal(isFirstReadyFrame(ready, resourceId, sessionId), false);
  assert.equal(isNextSequence(ready, 0), true);
  assert.equal(isNextSequence(ready, 1), false);
});

test('live connection authenticates with a first-frame ticket, resyncs REST, and closes on logout', async () => {
  const oldFetch = globalThis.fetch;
  const originals = ['WebSocket', 'window', 'document', 'location'].map((name) => [name, Object.getOwnPropertyDescriptor(globalThis, name)] as const);
  const sent: string[] = [];
  let socket: InstanceType<typeof FakeSocket> | undefined;
  const listeners = new Map<string, () => void>();
  const fakeDocument = {
    visibilityState: 'visible',
    addEventListener: (name: string, callback: () => void) => listeners.set(name, callback),
    removeEventListener: (name: string) => listeners.delete(name)
  };
  class FakeSocket {
    static OPEN = 1;
    readonly url: string;
    sent: string[] = [];
    readyState = 0;
    onopen?: () => void;
    onmessage?: (event: { data: unknown }) => void;
    onerror?: () => void;
    onclose?: (event: { code: number }) => void;
    constructor(url: string) {
      this.url = url;
      socket = this;
      this.readyState = 1;
      queueMicrotask(() => this.onopen?.());
    }
    send(raw: string) { sent.push(raw); this.sent.push(raw); }
    emit(raw: string) { this.onmessage?.({ data: raw }); }
    close(code: number) { this.readyState = 3; this.onclose?.({ code }); }
  }
  Object.defineProperty(globalThis, 'WebSocket', { value: FakeSocket, configurable: true });
  Object.defineProperty(globalThis, 'window', { value: globalThis, configurable: true });
  Object.defineProperty(globalThis, 'document', { value: fakeDocument, configurable: true });
  Object.defineProperty(globalThis, 'location', { value: { protocol: 'https:', host: 'cordova.example' }, configurable: true });
  globalThis.fetch = async () => Response.json({ ticket: 'synthetic-one-use-ticket-long-enough-to-validate', expires_at: new Date(Date.now() + 10_000).toISOString() });
  const statuses: string[] = [];
  const invalidations: string[] = [];
  let resyncs = 0;
  let current = true;
  let secondConnection: LiveConnection | undefined;
  const account = {
    account: { id: accountId, username: 'cordova-user', created_at: '2026-10-09T10:00:00Z', totp_enabled: false, privacy_notice_version: 'privacy-v1' },
    csrf_token: 'synthetic-csrf-token-long-enough', expires_at: '2026-10-10T10:00:00Z'
  };
  const connection = new LiveConnection(account, 1, {
    onStatus: (status) => statuses.push(status),
    onInvalidate: (event) => invalidations.push(event.event_id),
    onResync: () => { resyncs++; },
    onPoll: () => {}
  }, () => current, () => { current = false; });
  const waitUntil = async (predicate: () => boolean) => {
    const deadline = Date.now() + 1000;
    while (!predicate() && Date.now() < deadline) await new Promise((resolve) => setTimeout(resolve, 5));
    assert.equal(predicate(), true, 'timed out waiting for the mock socket');
  };
  try {
    connection.start();
    await waitUntil(() => Boolean(socket?.onopen && sent.length > 0));
    const auth = JSON.parse(sent[0]);
    assert.equal(auth.type, 'live.auth');
    assert.equal(auth.session_id, null);
    assert.equal(auth.payload.ticket.startsWith('synthetic-one-use-ticket'), true);
    assert.equal(socket?.url, 'wss://cordova.example/api/live');
    const ready = JSON.stringify({
      v: 1, type: 'live.ready', message_id: resourceId, request_id: auth.request_id, session_id: sessionId, seq: 1,
      sent_at: '2026-10-09T10:00:00Z', payload: { session_id: sessionId, account_id: accountId, heartbeat_interval_ms: 15000 }, error: null
    });
    socket?.emit(ready);
    assert.equal(JSON.parse(sent[1]).type, 'live.subscribe');
    assert.deepEqual(JSON.parse(sent[1]).payload.topics, ['nodes', 'usage', 'billing']);
    const event = { event_id: 'stream-42', topic: 'usage', resource_id: resourceId, revision: 2, action: 'invalidate' };
    socket?.emit(frame('live.event', event, 2));
    await new Promise((resolve) => setTimeout(resolve, 275));
    assert.deepEqual(invalidations, ['stream-42']);
    socket?.emit(frame('live.event', event, 3));
    await new Promise((resolve) => setTimeout(resolve, 275));
    assert.deepEqual(invalidations, ['stream-42']);
    socket?.emit(frame('live.resync', { reason: 'gap' }, 4));
    assert.equal(resyncs, 1);
    assert.equal(JSON.parse(sent[2]).payload.last_event_id, null);
    const firstSocket = socket;
    connection.stop();
    current = false;
    assert.equal(socket?.readyState, 3);
    assert.ok(statuses.includes('connected'));
    firstSocket?.emit(frame('live.event', { ...event, event_id: 'after-logout', revision: 3 }, 5));
    assert.deepEqual(invalidations, ['stream-42']);

    current = true;
    secondConnection = new LiveConnection(account, 2, {
      onStatus: (status) => statuses.push(status), onInvalidate: () => {}, onResync: () => { resyncs++; }, onPoll: () => {}
    }, () => current, () => { current = false; });
    secondConnection.start();
    await waitUntil(() => socket !== firstSocket && Boolean(socket?.sent.length));
    const secondSocket = socket;
    const secondAuth = JSON.parse(secondSocket!.sent[0]);
    secondSocket?.emit(JSON.stringify({
      v: 1, type: 'live.ready', message_id: accountId, request_id: secondAuth.request_id, session_id: sessionId, seq: 1,
      sent_at: '2026-10-09T10:00:00Z', payload: { session_id: sessionId, account_id: resourceId, heartbeat_interval_ms: 15000 }, error: null
    }));
    assert.equal(secondSocket?.readyState, 3);
    assert.equal(resyncs, 2);
    secondConnection.stop();
  } finally {
    connection.stop();
    secondConnection?.stop();
    current = false;
    current = false;
    globalThis.fetch = oldFetch;
    for (const [name, descriptor] of originals) {
      if (descriptor) Object.defineProperty(globalThis, name, descriptor);
      else Reflect.deleteProperty(globalThis, name);
    }
  }
});
