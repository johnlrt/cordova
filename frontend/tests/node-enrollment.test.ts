import { test } from 'node:test';
import assert from 'node:assert/strict';
import { approveNodeEnrollment } from '../src/lib/api/account.ts';
import { ApiFailure } from '../src/lib/api/client.ts';

test('node approval normalizes the one-time code and sends a same-origin CSRF-protected request', async () => {
  const originalFetch = globalThis.fetch;
  let url = '';
  let init: RequestInit | undefined;
  globalThis.fetch = async (input, options) => {
    url = String(input);
    init = options;
    return new Response(null, { status: 204 });
  };
  try {
    await approveNodeEnrollment('abcde-fghij-klm', 'synthetic-csrf-token-long-enough');
    assert.equal(url, '/api/node/enrollments/approve');
    assert.equal(init?.method, 'POST');
    assert.equal(init?.credentials, 'same-origin');
    assert.equal(init?.cache, 'no-store');
    assert.equal(new Headers(init?.headers).get('X-CSRF-Token'), 'synthetic-csrf-token-long-enough');
    assert.deepEqual(JSON.parse(String(init?.body)), { user_code: 'ABCDE-FGHIJ-KLM' });
  } finally { globalThis.fetch = originalFetch; }
});

test('node approval validates code shape and requires CSRF before sending', async () => {
  const originalFetch = globalThis.fetch;
  let called = false;
  globalThis.fetch = async () => { called = true; return new Response(null, { status: 204 }); };
  try {
    await assert.rejects(approveNodeEnrollment('bad-code', 'synthetic-csrf-token'),
      (error: unknown) => error instanceof ApiFailure && error.code === 'invalid_request');
    await assert.rejects(approveNodeEnrollment('ABCDE-FGHIJ-KLM', ''),
      (error: unknown) => error instanceof ApiFailure && error.code === 'csrf_failed');
    assert.equal(called, false);
  } finally { globalThis.fetch = originalFetch; }
});

test('node approval keeps safe expiry and replay status codes', async () => {
  const originalFetch = globalThis.fetch;
  globalThis.fetch = async () => Response.json({
    error: { message: 'The enrollment code has expired.', type: 'invalid_request_error', param: null, code: 'not_found' },
    request_id: 'c9eb01e9-899f-4edb-87fd-66e585ec9f79'
  }, { status: 410 });
  try {
    await assert.rejects(approveNodeEnrollment('ABCDE-FGHIJ-KLM', 'synthetic-csrf-token-long-enough'),
      (error: unknown) => error instanceof ApiFailure && error.status === 410 && error.code === 'not_found');
  } finally { globalThis.fetch = originalFetch; }
});
