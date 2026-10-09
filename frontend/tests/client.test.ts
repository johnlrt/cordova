import { test } from 'node:test';
import assert from 'node:assert/strict';
import { ApiFailure, decodeHealth, decodeModels, errorMessage } from '../src/lib/api/client.ts';

test('health must describe the supported mock API, not arbitrary successful JSON', () => {
  assert.deepEqual(decodeHealth({ status: 'ok', stage: '1', mode: 'mock' }), { status: 'ok', stage: '1', mode: 'mock' });
  for (const value of [null, {}, { status: 'ok' }, { status: 'ok', stage: '1', mode: 'real' }]) {
    assert.throws(() => decodeHealth(value), ApiFailure);
  }
});

test('catalog rejects incompatible, missing, duplicate and unsafe values', () => {
  const model = { id: 'cordova-chat-small-v1', object: 'model', context_tokens: 8192, max_output_tokens: 2048, availability: 'ready' };
  assert.equal(decodeModels({ object: 'list', data: [model] })[0].id, model.id);
  assert.deepEqual(decodeModels({ object: 'list', data: [] }), []);
  for (const data of [[{}], [model, model], [{ ...model, context_tokens: '8192' }], [{ ...model, max_output_tokens: -1 }]]) {
    assert.throws(() => decodeModels({ object: 'list', data }), ApiFailure);
  }
});

test('error copy does not render an arbitrary server exception or credential', () => {
  const sentinel = 'private API key sentinel';
  assert.ok(!errorMessage(new Error(sentinel)).includes(sentinel));
  assert.ok(!errorMessage(new ApiFailure(sentinel, 503)).includes(sentinel));
  assert.match(errorMessage(new ApiFailure('unauthenticated', 401)), /key/);
});
