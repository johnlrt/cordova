import { readFileSync } from 'node:fs';
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { matchesLiveClientFrame, matchesLiveServerFrame, matchesOpenApi, matchesWebSocketSchema } from '../src/lib/api/generated/contracts.ts';
import { decodeRegistration, decodeSession } from '../src/lib/api/account.ts';
import { decodeUsagePage } from '../src/lib/api/usage.ts';
import { decodeLiveFrame } from '../src/lib/live/protocol.ts';

function fixture<T>(path: string): T {
  return JSON.parse(readFileSync(new URL(`../../contracts/fixtures/${path}`, import.meta.url), 'utf8')) as T;
}

test('frontend boundary fixtures match generated M1 REST schemas', () => {
  const cases = [
    ['rest/session-valid.json', 'SessionView', true],
    ['rest/session-missing-account-id.json', 'SessionView', false],
    ['rest/registration-valid.json', 'RegistrationResult', true],
    ['rest/usage-valid.json', 'UsagePage', true],
    ['rest/usage-numeric-money.json', 'UsagePage', false],
    ['rest/usage-wrong-enum.json', 'UsagePage', false],
    ['rest/node-valid.json', 'Node', true],
    ['rest/node-wrong-enum.json', 'Node', false],
    ['rest/key-valid.json', 'ApiKey', true],
    ['rest/key-numeric-cap.json', 'ApiKey', false],
    ['rest/balance-valid.json', 'Balance', true],
    ['rest/balance-numeric-money.json', 'Balance', false],
    ['rest/account-error-unknown-code.json', 'AccountErrorResponse', false]
  ] as const;
  for (const [path, schema, expected] of cases) {
    assert.equal(matchesOpenApi(fixture(path), schema), expected, path);
  }

  assert.equal(decodeSession(fixture('rest/session-valid.json')).account.username, 'cordova-user');
  assert.equal(decodeRegistration(fixture('rest/registration-valid.json')).recovery_codes.length, 2);
  assert.equal(decodeUsagePage(fixture('rest/usage-valid.json')).data[0].charged_piconero, '0');
  assert.throws(() => decodeUsagePage(fixture('rest/usage-numeric-money.json')));
  assert.throws(() => decodeUsagePage(fixture('rest/usage-wrong-enum.json')));
});

test('C2 safe-error fixtures cover the complete generated registry', () => {
  const errors = fixture<unknown[]>('rest/account-safe-errors.json');
  assert.equal(matchesOpenApi(errors, 'AccountSafeErrorFixtures'), true);
  assert.equal(new Set(errors.map((error) => (error as { error: { code: string } }).error.code)).size, 21);
});

test('generated live validators accept only inbound dashboard frames', () => {
  const auth = fixture<unknown>('ws/live-auth.json');
  assert.equal(matchesWebSocketSchema(auth, '#/$defs/LiveVariant'), true);
  assert.equal(matchesLiveClientFrame(auth), true);
  assert.equal(matchesLiveServerFrame(auth), false);
  assert.equal(matchesLiveClientFrame(fixture('ws/live-auth-short-ticket.json')), false);
  assert.equal(matchesLiveClientFrame(fixture('ws/live-subscribe.json')), true);

  const event = fixture<Record<string, unknown>>('ws/live-event.json');
  const resync = fixture<Record<string, unknown>>('ws/live-resync.json');
  assert.equal(matchesLiveServerFrame(event), true);
  assert.equal(matchesLiveServerFrame(fixture('ws/live-event-missing-resource.json')), false);
  assert.equal(matchesLiveClientFrame(event), false);
  assert.equal(matchesLiveServerFrame(resync), true);
  assert.equal(matchesLiveServerFrame(fixture('ws/live-resync-wrong-reason.json')), false);
  assert.equal(decodeLiveFrame(JSON.stringify(event)).type, 'live.event');
  assert.equal(decodeLiveFrame(JSON.stringify(resync)).type, 'live.resync');

  const missingResource = structuredClone(event);
  delete (missingResource.payload as Record<string, unknown>).resource_id;
  assert.throws(() => decodeLiveFrame(JSON.stringify(missingResource)));
  assert.throws(() => decodeLiveFrame(JSON.stringify(auth)));
  assert.throws(() => decodeLiveFrame(JSON.stringify(fixture('ws/node-hello.json'))));
});
