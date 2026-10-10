import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createAPIKey, decodeAPIKeyList, decodeCreatedAPIKey, listAPIKeys, revokeAPIKey } from '../src/lib/api/account.ts';
import { parseXmrCap, formatXmrCap } from '../src/lib/api/key-money.ts';
import { ApiFailure } from '../src/lib/api/client.ts';

const key = {
  id: 'c9eb01e9-899f-4edb-87fd-66e585ec9f80', account_id: 'c9eb01e9-899f-4edb-87fd-66e585ec9f79',
  name: 'local development', prefix: 'cvk_12345678',
  permissions: ['chat:write', 'models:read'], model_ids: ['cordova-chat-small-v1'],
  daily_spend_cap_piconero: '10000000000', created_at: '2026-10-09T10:00:00Z',
  last_used_at: null, revoked_at: null
};

test('XMR spend cap conversion is exact and enforces the piconero range', () => {
  assert.equal(parseXmrCap('0'), '0');
  assert.equal(parseXmrCap('0.000000000001'), '1');
  assert.equal(parseXmrCap('1.000000000009'), '1000000000009');
  assert.equal(parseXmrCap('9000000'), '9000000000000000000');
  for (const value of ['', '-1', '+1', '01', '.5', '1.', '1e2', '0.0000000000001', '9000000.000000000001', '9999999']) {
    assert.equal(parseXmrCap(value), null, `should reject ${value}`);
  }
  assert.equal(formatXmrCap('1000000000009', 'en'), '1.000000000009 XMR');
  assert.equal(formatXmrCap('9000000000000000000', 'en'), '9,000,000 XMR');
  assert.equal(formatXmrCap('9000000000000000001', 'en'), '—');
});

test('API key decoders reject malformed server payloads', () => {
  assert.deepEqual(decodeAPIKeyList({ data: [key], next_cursor: null }).data, [key]);
  assert.throws(() => decodeAPIKeyList({ data: [{ ...key, verifier: 'must-not-appear' }], next_cursor: null }), ApiFailure);
  assert.deepEqual(decodeCreatedAPIKey({ key, secret: `cvk_${'a'.repeat(64)}` }).key, key);
  assert.throws(() => decodeCreatedAPIKey({ key, secret: 'not-a-valid-secret' }), ApiFailure);
});

test('key list, create and revoke use same-origin, no-store requests and CSRF for mutations', async () => {
  const originalFetch = globalThis.fetch;
  const requests: Array<{ url: string; init?: RequestInit }> = [];
  globalThis.fetch = async (input, init) => {
    const url = String(input);
    requests.push({ url, init });
    if (url === '/api/keys' && init?.method === 'POST') {
      return Response.json({ key, secret: `cvk_${'a'.repeat(64)}` }, { status: 201 });
    }
    if (init?.method === 'DELETE') return new Response(null, { status: 204 });
    return Response.json({ data: [key], next_cursor: null });
  };
  try {
    assert.equal((await listAPIKeys()).data[0].id, key.id);
    const created = await createAPIKey({ name: key.name, permissions: ['chat:write'], model_ids: key.model_ids,
      daily_spend_cap_piconero: '10000000000' }, 'synthetic-csrf-token-long-enough');
    assert.equal(created.secret, `cvk_${'a'.repeat(64)}`);
    await revokeAPIKey(key.id, 'synthetic-csrf-token-long-enough');

    assert.equal(requests[0].url, '/api/keys');
    assert.equal(requests[0].init?.credentials, 'same-origin');
    assert.equal(requests[0].init?.cache, 'no-store');
    assert.equal(requests[1].init?.method, 'POST');
    assert.equal(new Headers(requests[1].init?.headers).get('X-CSRF-Token'), 'synthetic-csrf-token-long-enough');
    assert.deepEqual(JSON.parse(String(requests[1].init?.body)), {
      name: key.name, permissions: ['chat:write'], model_ids: key.model_ids, daily_spend_cap_piconero: '10000000000'
    });
    assert.equal(requests[2].url, `/api/keys/${key.id}`);
    assert.equal(requests[2].init?.method, 'DELETE');
    assert.equal(new Headers(requests[2].init?.headers).get('X-CSRF-Token'), 'synthetic-csrf-token-long-enough');
  } finally { globalThis.fetch = originalFetch; }
});

test('mutating key operations require CSRF and revoke rejects unsafe path identifiers', async () => {
  await assert.rejects(createAPIKey({ name: key.name, permissions: ['chat:write'], model_ids: key.model_ids,
    daily_spend_cap_piconero: '0' }, ''), (error: unknown) => error instanceof ApiFailure && error.code === 'csrf_failed');
  await assert.rejects(revokeAPIKey('../session', 'synthetic-csrf-token'),
    (error: unknown) => error instanceof ApiFailure && error.code === 'invalid_request');
});
