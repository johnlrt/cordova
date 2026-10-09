import { test } from 'node:test';
import assert from 'node:assert/strict';
import { ApiFailure, decodeUsage, safeReturnPath } from '../src/lib/api/client.ts';

const row = {
  request_id: '11111111-1111-4111-8111-111111111111',
  api_key_id: '22222222-2222-4222-8222-222222222222',
  model_id: 'cordova-chat-small-v1', status: 'completed', input_tokens: 3, output_tokens: 7,
  charged_piconero: '0', latency_ms: 12, created_at: '2026-10-09T10:00:00Z', finished_at: '2026-10-09T10:00:00Z'
};

test('usage decoder accepts exact decimal strings and rejects malformed metadata', () => {
  assert.deepEqual(decodeUsage({ data: [row], next_cursor: null }), [row]);
  for (const value of [
    { data: [{ ...row, charged_piconero: 0 }] },
    { data: [{ ...row, request_id: 'not-an-id' }] },
    { data: [{ ...row, input_tokens: Number.MAX_SAFE_INTEGER + 1 }] },
    { data: [{ ...row, model_id: 'foreign-model' }] },
    { data: Array(101).fill(row) }
  ]) assert.throws(() => decodeUsage(value), ApiFailure);
});

test('return paths cannot escape to another origin', () => {
  assert.equal(safeReturnPath('/dashboard?tab=usage'), '/dashboard?tab=usage');
  for (const value of ['https://attacker.example', '//attacker.example', '/\\attacker.example', null, '']) {
    assert.equal(safeReturnPath(value), '/dashboard');
  }
});
