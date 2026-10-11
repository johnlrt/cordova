import { test, expect, type Page, type Route } from '@playwright/test';
import type { Node } from '../../src/lib/api/generated/contracts.ts';

const account = {
  id: 'c9eb01e9-899f-4edb-87fd-66e585ec9f79', username: 'node-owner',
  created_at: '2026-10-09T10:00:00Z', totp_enabled: false, privacy_notice_version: 'privacy-v1'
};
const csrf = 'synthetic-csrf-token-long-enough';
const nodeID = '706134d8-0590-437c-ab24-bb9c396c815b';
const secondNodeID = '806134d8-0590-437c-ab24-bb9c396c815b';
const baseNode: Node = {
  id: nodeID, name: 'new-device', status: 'enrolling', rank: null, rank_updated_at: null, hardware: null,
  verified_capacity_bytes: null, measured_tokens_per_second: null, latency_ms: null, uptime_ratio: null,
  success_ratio: null, last_seen_at: null, models: [{ model_id: 'cordova-chat-small-v1', state: 'on_disk', active_slots: 0,
    max_slots: 0, verified_capacity_bytes: 0, updated_at: '2026-10-09T10:00:00Z' }], earnings_pending_piconero: null,
  earnings_available_piconero: null, hold_until: null
};

async function openManagement(page: Page, options: {
  status?: number; hold?: boolean; initialNodes?: Node[]; extraPage?: Node[];
} = {}) {
  let responseStatus = options.status ?? 201;
  let hold = options.hold ?? false;
  let requestCount = 0;
  let capturedCode = '';
  let capturedName = '';
  let revokeCount = 0;
  let prepareCount = 0;
  let nodes = [...(options.initialNodes ?? [])];
  let release!: () => void;
  let started!: () => void;
  const blocked = new Promise<void>((resolve) => { release = resolve; });
  const requestStarted = new Promise<void>((resolve) => { started = resolve; });
  await page.route((url) => url.pathname.startsWith('/api/'), async (route: Route) => {
    const request = route.request();
    const url = new URL(request.url());
    const path = url.pathname;
    if (path === '/api/session') {
      await route.fulfill({ status: 200, json: { account, csrf_token: csrf, expires_at: '2026-10-10T10:00:00Z' } });
    } else if (path === '/api/nodes' && request.method() === 'GET') {
      if (url.searchParams.get('cursor') === 'second-page') {
        const data = options.extraPage ?? [];
        nodes = [...nodes, ...data];
        await route.fulfill({ status: 200, json: { data, next_cursor: null } });
      } else {
        await route.fulfill({ status: 200, json: { data: nodes, next_cursor: options.extraPage?.length ? 'second-page' : null } });
      }
    } else if (path === '/api/nodes/link' && request.method() === 'POST') {
      requestCount++;
      started();
      const body = request.postDataJSON() as { user_code?: string; name?: string };
      capturedCode = String(body.user_code ?? '');
      capturedName = String(body.name ?? '');
      expect(request.headers()['x-csrf-token']).toBe(csrf);
      if (hold) await blocked;
      if (responseStatus === 201) {
        const linked = { ...baseNode, name: capturedName };
        nodes = [linked, ...nodes];
        await route.fulfill({ status: 201, json: linked });
      } else {
        const code = responseStatus === 409 ? 'conflict' : responseStatus === 429 ? 'rate_limited' : responseStatus === 400 ? 'invalid_request' : 'not_found';
        const type = responseStatus === 409 ? 'invalid_request_error' : responseStatus === 429 ? 'rate_limit_error' : responseStatus === 401 ? 'authentication_error' : 'invalid_request_error';
        await route.fulfill({ status: responseStatus,
          headers: responseStatus === 429 ? { 'Retry-After': '8' } : {},
          json: { error: { message: 'Safe enrollment error.', type, param: null, code }, request_id: account.id }
        });
      }
    } else if (path === `/api/nodes/${nodeID}` && request.method() === 'GET') {
      const found = nodes.find((node) => node.id === nodeID);
      await route.fulfill(found ? { status: 200, json: found } : {
        status: 404, json: { error: { message: 'Node not found.', type: 'invalid_request_error', param: null, code: 'not_found' }, request_id: account.id }
      });
    } else if (path === `/api/nodes/${nodeID}` && request.method() === 'PATCH') {
      const body = request.postDataJSON() as { name: string };
      const renamed = { ...baseNode, name: body.name };
      nodes = nodes.map((node) => node.id === nodeID ? renamed : node);
      await route.fulfill({ status: 200, json: renamed });
    } else if (path === `/api/nodes/${nodeID}/revoke` && request.method() === 'POST') {
      revokeCount++;
      nodes = nodes.filter((node) => node.id !== nodeID);
      await route.fulfill({ status: 204, body: '' });
    } else if (path === `/api/nodes/${nodeID}/models/cordova-chat-small-v1/prepare` && request.method() === 'POST') {
      prepareCount++;
      expect(request.headers()['x-csrf-token']).toBe(csrf);
      await route.fulfill({ status: 202, json: { model_id: 'cordova-chat-small-v1', status: 'preparing' } });
    } else {
      await route.fulfill({ status: 404, json: { error: { message: 'Not found.', type: 'invalid_request_error', param: null, code: 'not_found' }, request_id: account.id } });
    }
  });
  await page.goto('/dashboard/nodes');
  await expect(page.getByRole('heading', { name: 'Your nodes' })).toBeVisible();
  return {
    count: () => requestCount,
    code: () => capturedCode,
    name: () => capturedName,
    revokeCount: () => revokeCount,
    prepareCount: () => prepareCount,
    status: (value: number) => { responseStatus = value; },
    hold: (value: boolean) => { hold = value; },
    waitStarted: () => requestStarted,
    release: () => release()
  };
}

async function fillAndConfirm(page: Page, code = 'ABCDE-FGHIJ-KLM') {
  await page.getByLabel('Node name').fill('  workstation  ');
  await page.getByLabel('Code shown by the node CLI').fill(code);
  await page.getByLabel('I checked the CLI code and approve a node on a device I control.').check();
}

test('[nodes] links an owner-approved one-time code and clears it after success', async ({ page }) => {
  const api = await openManagement(page);
  await expect(page.getByText('No nodes yet')).toBeVisible();
  await expect(page.getByRole('button', { name: 'Approve and link' })).toBeDisabled();
  expect(api.count()).toBe(0);
  await fillAndConfirm(page, 'abcde-fghij-klm');
  const codeValidity = await page.getByLabel('Code shown by the node CLI').evaluate((input: HTMLInputElement) => ({
    value: input.value, pattern: input.pattern, patternMismatch: input.validity.patternMismatch
  }));
  expect(codeValidity).toEqual({ value: 'ABCDE-FGHIJ-KLM', pattern: '[A-Z2-7]{5}-[A-Z2-7]{5}-[A-Z2-7]{3}', patternMismatch: false });
  await page.getByRole('button', { name: 'Approve and link' }).click();
  await expect.poll(api.count).toBe(1);
  await expect(page.locator('.key-message')).toContainText('Sign-in approved');
  await expect(page.getByRole('link', { name: 'workstation' }).first()).toBeVisible();
  expect(api.code()).toBe('ABCDE-FGHIJ-KLM');
  expect(api.name()).toBe('workstation');
  await expect(page.getByLabel('Code shown by the node CLI')).toHaveValue('');
  expect(new URL(page.url()).search).toBe('');
  const stored = await page.evaluate(() => `${JSON.stringify(localStorage)} ${JSON.stringify(sessionStorage)}`);
  expect(stored).not.toContain('ABCDE-FGHIJ-KLM');
  await page.setViewportSize({ width: 390, height: 844 });
  expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(390);
});

test('[nodes] distinguishes expired, reused and throttled codes', async ({ page }) => {
  const api = await openManagement(page, { status: 410 });
  await fillAndConfirm(page);
  await page.getByRole('button', { name: 'Approve and link' }).click();
  await expect(page.getByRole('alert')).toContainText('This code expired');

  api.status(409);
  await page.getByLabel('Code shown by the node CLI').fill('UVWXY-23456-AAA');
  await page.getByRole('button', { name: 'Approve and link' }).click();
  await expect(page.getByRole('alert')).toContainText('already approved');

  api.status(429);
  await page.getByRole('button', { name: 'Approve and link' }).click();
  await expect(page.getByRole('alert')).toContainText('Too many attempts');
  expect(api.count()).toBe(3);
});

test('[nodes] blocks duplicate linking while the server is processing the code', async ({ page }) => {
  const api = await openManagement(page, { hold: true });
  await fillAndConfirm(page);
  await page.getByRole('button', { name: 'Approve and link' }).click();
  await api.waitStarted();
  await page.locator('form').dispatchEvent('submit', { bubbles: true, cancelable: true });
  expect(api.count()).toBe(1);
  api.release();
  await expect(page.locator('.key-message')).toContainText('Sign-in approved');
  expect(api.count()).toBe(1);
});

test('[nodes] filters inventory, loads another page, renames and revokes a node accessibly', async ({ page }) => {
  const first = { ...baseNode, name: 'workstation' };
  const second = { ...baseNode, id: secondNodeID, name: 'remote-box', status: 'offline' as const };
  const api = await openManagement(page, { initialNodes: [first], extraPage: [second] });
  await expect(page.getByRole('link', { name: 'workstation' }).first()).toBeVisible();
  await page.getByLabel('Search').fill('remote');
  await page.getByRole('button', { name: 'Load more nodes' }).click();
  await expect(page.getByRole('link', { name: 'remote-box' })).toBeVisible();
  await expect(page.getByRole('link', { name: 'workstation' })).toHaveCount(0);
  await page.getByLabel('Search').fill('');
  await page.getByLabel('Status').selectOption('offline');
  await expect(page.getByRole('link', { name: 'remote-box' })).toBeVisible();
  await page.getByLabel('Status').selectOption('all');

  const detailLink = page.locator('.node-detail-link').first();
  await detailLink.click();
  await page.waitForURL(`**/dashboard/nodes/${nodeID}`);
  await expect(page.getByRole('heading', { name: 'workstation' })).toBeVisible();
  await expect(page.getByText('Not tested yet').first()).toBeVisible();
  await page.getByLabel('Node name').fill('renamed workstation');
  await page.getByRole('button', { name: 'Save name' }).click();
  await expect(page.getByRole('status')).toContainText('Node name updated');
  await expect(page.getByRole('heading', { name: 'renamed workstation' })).toBeVisible();

  await page.getByRole('button', { name: 'Revoke node' }).click();
  const dialog = page.getByRole('dialog', { name: 'Revoke renamed workstation?' });
  await expect(dialog).toBeVisible();
  await page.getByRole('button', { name: 'Cancel' }).click();
  await expect(dialog).toBeHidden();
  await page.getByRole('button', { name: 'Revoke node' }).click();
  await page.getByLabel('I understand this device will lose access.').check();
  await page.getByRole('button', { name: 'Confirm revoke' }).click();
  await expect(page.getByRole('heading', { name: 'Your nodes' })).toBeVisible();
  await expect(page.getByRole('link', { name: 'remote-box' })).toBeVisible();
  expect(api.revokeCount()).toBe(1);
});

test('[nodes] requests preparation for an owner node model', async ({ page }) => {
  const api = await openManagement(page, { initialNodes: [{ ...baseNode, status: 'online' }] });
  await page.locator('.node-detail-link').first().click();
  await page.waitForURL(`**/dashboard/nodes/${nodeID}`);
  const prepare = page.getByRole('button', { name: 'Prepare model cordova-chat-small-v1' });
  await expect(prepare).toBeVisible();
  await prepare.click();
  await expect(page.getByRole('status')).toContainText('Preparation for cordova-chat-small-v1 started');
  expect(api.prepareCount()).toBe(1);
});
