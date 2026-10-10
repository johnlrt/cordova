import { readFileSync } from 'node:fs';
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { fetchNode, fetchNodes, linkNode, renameNode, revokeNode } from '../src/lib/api/account.ts';
import { ApiFailure } from '../src/lib/api/client.ts';

const nodeID = '706134d8-0590-437c-ab24-bb9c396c815b';
const csrf = 'synthetic-csrf-token-long-enough';
const node = JSON.parse(readFileSync(new URL('../../contracts/fixtures/rest/node-unmeasured.json', import.meta.url), 'utf8'));
const nodePage = JSON.parse(readFileSync(new URL('../../contracts/fixtures/rest/node-page.json', import.meta.url), 'utf8'));

test('node inventory reads the signed-in account page with bounded pagination values', async () => {
  const originalFetch = globalThis.fetch;
  let url = '';
  let init: RequestInit | undefined;
  globalThis.fetch = async (input, options) => {
    url = String(input);
    init = options;
    return Response.json(nodePage);
  };
  try {
    assert.deepEqual(await fetchNodes(25, 'synthetic-cursor'), nodePage);
    assert.equal(url, '/api/nodes?limit=25&cursor=synthetic-cursor');
    assert.equal(init?.method, 'GET');
    assert.equal(init?.credentials, 'same-origin');
    assert.equal(init?.cache, 'no-store');
    let called = false;
    globalThis.fetch = async () => { called = true; return Response.json(nodePage); };
    await assert.rejects(fetchNodes(101), ApiFailure);
    assert.equal(called, false);
  } finally { globalThis.fetch = originalFetch; }
});

test('link normalizes the one-time code and posts the chosen name with CSRF', async () => {
  const originalFetch = globalThis.fetch;
  let url = '';
  let init: RequestInit | undefined;
  globalThis.fetch = async (input, options) => {
    url = String(input);
    init = options;
    return Response.json(node, { status: 201 });
  };
  try {
    assert.deepEqual(await linkNode('abcde-fghjk-mnp', '  workstation  ', csrf), node);
    assert.equal(url, '/api/nodes/link');
    assert.equal(init?.method, 'POST');
    assert.equal(new Headers(init?.headers).get('X-CSRF-Token'), csrf);
    assert.deepEqual(JSON.parse(String(init?.body)), { user_code: 'ABCDE-FGHJK-MNP', name: 'workstation' });
  } finally { globalThis.fetch = originalFetch; }
});

test('node mutations validate inputs before making requests and send same-origin CSRF', async () => {
  const originalFetch = globalThis.fetch;
  let requests: Array<{ url: string; method: string; csrf: string | null; body: string }> = [];
  globalThis.fetch = async (input, init) => {
    requests.push({ url: String(input), method: String(init?.method), csrf: new Headers(init?.headers).get('X-CSRF-Token'), body: String(init?.body ?? '') });
    return String(input).endsWith('/revoke')
      ? new Response(null, { status: 204 })
      : Response.json(node);
  };
  try {
    await assert.rejects(linkNode('bad', 'workstation', csrf), ApiFailure);
    await assert.rejects(fetchNode('../outside'), ApiFailure);
    assert.deepEqual(requests, []);

    await renameNode(nodeID, 'new workstation', csrf);
    await revokeNode(nodeID, csrf);
    assert.deepEqual(requests, [
      { url: `/api/nodes/${nodeID}`, method: 'PATCH', csrf, body: JSON.stringify({ name: 'new workstation' }) },
      { url: `/api/nodes/${nodeID}/revoke`, method: 'POST', csrf, body: '{}' }
    ]);
  } finally { globalThis.fetch = originalFetch; }
});

test('node detail preserves safe not-found errors', async () => {
  const originalFetch = globalThis.fetch;
  globalThis.fetch = async () => Response.json({
    error: { message: 'Node not found.', type: 'invalid_request_error', param: null, code: 'not_found' },
    request_id: 'c9eb01e9-899f-4edb-87fd-66e585ec9f79'
  }, { status: 404 });
  try {
    await assert.rejects(fetchNode(nodeID), (error: unknown) =>
      error instanceof ApiFailure && error.status === 404 && error.code === 'not_found');
  } finally { globalThis.fetch = originalFetch; }
});
