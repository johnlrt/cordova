import { test } from 'node:test';
import assert from 'node:assert/strict';
import { ApiFailure } from '../src/lib/api/client.ts';
import { decodeRegistration, decodeSession, fetchLiveTicket, loginAccount, logoutAccount, registerAccount } from '../src/lib/api/account.ts';
import { safeReturnPath } from '../src/lib/api/navigation.ts';

const sessionFixture = {
  account: {
    id: 'c9eb01e9-899f-4edb-87fd-66e585ec9f79', username: 'cordova-user',
    created_at: '2026-10-09T10:00:00Z', totp_enabled: false, privacy_notice_version: 'privacy-v1'
  },
  csrf_token: 'synthetic-csrf-token-long-enough', expires_at: '2026-10-10T10:00:00Z'
};

test('account session decoder accepts only the planned DTO shape', () => {
  assert.equal(decodeSession(sessionFixture).account.username, 'cordova-user');
  for (const value of [null, {}, { ...sessionFixture, csrf_token: '' },
    { ...sessionFixture, account: { ...sessionFixture.account, id: 'not-a-uuid' } },
    { ...sessionFixture, account: { ...sessionFixture.account, totp_enabled: 'false' } },
    { ...sessionFixture, expires_at: 'tomorrow' }]) {
    assert.throws(() => decodeSession(value), ApiFailure);
  }
  assert.deepEqual(decodeRegistration({ ...sessionFixture, recovery_codes: ['synthetic-one-time-code'] }).recovery_codes,
    ['synthetic-one-time-code']);
  assert.throws(() => decodeRegistration({ ...sessionFixture, recovery_codes: [] }), ApiFailure);
});

test('return path accepts only same-origin local routes', () => {
  assert.equal(safeReturnPath('/dashboard/nodes?filter=online#table'), '/dashboard/nodes?filter=online#table');
  for (const value of ['https://evil.example', '//evil.example/path', '/\\evil.example/path', 'javascript:alert(1)', '\\evil.example']) {
    assert.equal(safeReturnPath(value), '/dashboard');
  }
});

test('login uses same-origin cookies, no-store and sends no credential in a URL', async () => {
  const originalFetch = globalThis.fetch;
  let requestUrl = '';
  let requestInit: RequestInit | undefined;
  globalThis.fetch = async (input, init) => {
    requestUrl = String(input);
    requestInit = init;
    return Response.json(sessionFixture);
  };
  try {
    const session = await loginAccount('cordova-user', 'synthetic password', '123456');
    assert.equal(session.account.id, sessionFixture.account.id);
    assert.equal(requestUrl, '/api/auth/login');
    assert.equal(requestInit?.credentials, 'same-origin');
    assert.equal(requestInit?.cache, 'no-store');
    assert.deepEqual(JSON.parse(String(requestInit?.body)), { username: 'cordova-user', password: 'synthetic password', totp_code: '123456' });
  } finally { globalThis.fetch = originalFetch; }
});

test('logout sends CSRF and handles the contract 204 response', async () => {
  const originalFetch = globalThis.fetch;
  let requestInit: RequestInit | undefined;
  globalThis.fetch = async (_input, init) => { requestInit = init; return new Response(null, { status: 204 }); };
  try {
    await logoutAccount('synthetic-csrf-token');
    assert.equal(requestInit?.method, 'POST');
    assert.equal(new Headers(requestInit?.headers).get('X-CSRF-Token'), 'synthetic-csrf-token');
    assert.equal(requestInit?.body, '{}');
  } finally { globalThis.fetch = originalFetch; }
});

test('registration sends the acknowledged privacy notice version in its JSON body', async () => {
  const originalFetch = globalThis.fetch;
  let requestBody: unknown;
  globalThis.fetch = async (_input, init) => {
    requestBody = JSON.parse(String(init?.body));
    return Response.json({ ...sessionFixture, recovery_codes: ['synthetic-recovery-code'] }, { status: 201 });
  };
  try {
    const result = await registerAccount('cordova-user', 'synthetic password', 'privacy-v1');
    assert.equal(result.recovery_codes[0], 'synthetic-recovery-code');
    assert.deepEqual(requestBody, { username: 'cordova-user', password: 'synthetic password', privacy_notice_version: 'privacy-v1' });
  } finally { globalThis.fetch = originalFetch; }
});

test('live ticket uses a CSRF-protected same-origin POST and never puts the ticket in a URL', async () => {
  const originalFetch = globalThis.fetch;
  let requestUrl = '';
  let requestHeaders: Headers | undefined;
  globalThis.fetch = async (input, init) => {
    requestUrl = String(input);
    requestHeaders = new Headers(init?.headers);
    return Response.json({ ticket: 'synthetic-one-use-ticket-long-enough-to-validate', expires_at: new Date(Date.now() + 10_000).toISOString() });
  };
  try {
    const ticket = await fetchLiveTicket('synthetic-csrf-token');
    assert.ok(ticket.startsWith('synthetic-one-use-ticket'));
    assert.equal(requestUrl, '/api/live-ticket');
    assert.equal(requestHeaders?.get('X-CSRF-Token'), 'synthetic-csrf-token');
  } finally { globalThis.fetch = originalFetch; }
});

test('rate-limit errors retain only a bounded retry delay', async () => {
  const originalFetch = globalThis.fetch;
  globalThis.fetch = async () => Response.json({ error: { message: 'Too many attempts', type: 'rate_limit_error', param: null, code: 'rate_limited' }, request_id: 'c9eb01e9-899f-4edb-87fd-66e585ec9f79' }, {
    status: 429, headers: { 'Retry-After': '12' }
  });
  try {
    await assert.rejects(loginAccount('cordova-user', 'synthetic password'), (error: unknown) =>
      error instanceof ApiFailure && error.status === 429 && error.retryAfter === 12 && error.requestId === sessionFixture.account.id);
  } finally { globalThis.fetch = originalFetch; }
});
