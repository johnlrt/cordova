import { test, expect, type Page, type Route } from '@playwright/test';

const account = {
  id: 'c9eb01e9-899f-4edb-87fd-66e585ec9f79', username: 'cordova-user',
  created_at: '2026-10-09T10:00:00Z', totp_enabled: false, privacy_notice_version: 'privacy-v1'
};
const csrf = 'synthetic-csrf-token-long-enough';
const keyID = 'c9eb01e9-899f-4edb-87fd-66e585ec9f80';
const secret = `cvk_${'a'.repeat(64)}`;

type APIKeyFixture = {
  id: string; account_id: string; name: string; prefix: string; permissions: string[]; model_ids: string[];
  daily_spend_cap_piconero: string; created_at: string; last_used_at: string | null; revoked_at: string | null;
};

function makeKey(overrides: Partial<APIKeyFixture> = {}): APIKeyFixture {
  return {
    id: keyID, account_id: account.id, name: 'local development', prefix: 'cvk_12345678', permissions: ['chat:write'],
    model_ids: ['cordova-chat-small-v1'], daily_spend_cap_piconero: '10000000000',
    created_at: '2026-10-09T10:00:00Z', last_used_at: null, revoked_at: null, ...overrides
  };
}

async function openKeys(page: Page, options: { createStatus?: number; holdCreate?: boolean } = {}) {
  let records: ReturnType<typeof makeKey>[] = [];
  let createStatus = options.createStatus ?? 201;
  let holdCreate = options.holdCreate ?? false;
  let releaseCreate!: () => void;
  let createStarted!: () => void;
  let createCount = 0;
  let revokeCount = 0;
  const createStartedPromise = new Promise<void>((resolve) => { createStarted = resolve; });
  const blocked = new Promise<void>((resolve) => { releaseCreate = resolve; });

  await page.route((url) => url.pathname.startsWith('/api/'), async (route: Route) => {
    const request = route.request();
    const path = new URL(request.url()).pathname;
    if (path === '/api/session') {
      await route.fulfill({ status: 200, json: { account, csrf_token: csrf, expires_at: '2026-10-10T10:00:00Z' } });
    } else if (path === '/api/keys' && request.method() === 'GET') {
      await route.fulfill({ status: 200, json: { data: records, next_cursor: null } });
    } else if (path === '/api/keys' && request.method() === 'POST') {
      createCount++;
      createStarted();
      const settings = request.postDataJSON() as { name: string; permissions: string[]; model_ids: string[]; daily_spend_cap_piconero: string };
      expect(request.headers()['x-csrf-token']).toBe(csrf);
      if (holdCreate) await blocked;
      if (createStatus !== 201) {
        await route.fulfill({ status: createStatus,
          headers: createStatus === 429 ? { 'Retry-After': '12' } : {},
          json: { error: { message: 'Safe server error.', type: createStatus === 429 ? 'rate_limit_error' : 'permission_error', param: null,
            code: createStatus === 429 ? 'rate_limited' : 'verification_required' }, request_id: account.id }
        });
      } else {
        records = [makeKey({ name: settings.name, permissions: settings.permissions, model_ids: settings.model_ids,
          daily_spend_cap_piconero: settings.daily_spend_cap_piconero })];
        await route.fulfill({ status: 201, json: { key: records[0], secret } });
      }
    } else if (path === `/api/keys/${keyID}` && request.method() === 'DELETE') {
      revokeCount++;
      expect(request.headers()['x-csrf-token']).toBe(csrf);
      records = records.map((record) => ({ ...record, revoked_at: new Date().toISOString() }));
      await route.fulfill({ status: 204, body: '' });
    } else {
      await route.fulfill({ status: 404, json: { error: { message: 'Not found.', type: 'invalid_request_error', param: null, code: 'not_found' }, request_id: account.id } });
    }
  });

  await page.goto('/dashboard/keys');
  await expect(page.getByRole('heading', { name: 'API keys', exact: true })).toBeVisible();
  await expect(page.getByText('No API keys yet')).toBeVisible();
  return {
    createCalls: () => createCount,
    revokeCalls: () => revokeCount,
    setCreateStatus: (status: number) => { createStatus = status; },
    waitForCreate: () => createStartedPromise,
    releaseCreate: () => releaseCreate()
  };
}

async function fillKeyForm(page: Page, cap = '0.010000000009') {
  await page.getByLabel('Name', { exact: true }).fill('local development');
  await page.getByLabel('Daily spend cap · XMR').fill(cap);
  await page.getByLabel(/Create chat completions/).check();
  await page.getByLabel(/cordova-chat-small-v1/).check();
  await page.getByLabel(/I have read and accept the current/).check();
}

test('[keys] create uses exact XMR units, reveals once, copies only on click and clears on close', async ({ page, context }) => {
  const api = await openKeys(page);
  await context.grantPermissions(['clipboard-read', 'clipboard-write']);
  await fillKeyForm(page);
  await page.getByRole('button', { name: 'Create key' }).click();
  await expect(page.getByRole('heading', { name: 'Store this key securely' })).toBeVisible();
  await expect(page.getByLabel('New API key')).toHaveValue(secret);
  await expect(page.getByText('local development', { exact: true })).toBeVisible();
  await expect(page.getByText('10,000,000,009 piconero', { exact: false })).toHaveCount(0);
  expect(api.createCalls()).toBe(1);

  await page.getByRole('button', { name: 'Select manually' }).click();
  const selection = await page.getByLabel('New API key').evaluate((element: HTMLInputElement) => [element.selectionStart, element.selectionEnd]);
  expect(selection).toEqual([0, secret.length]);
  await page.getByRole('button', { name: 'Copy', exact: true }).click();
  await expect.poll(async () => page.evaluate(() => navigator.clipboard.readText())).toBe(secret);
  await page.getByRole('button', { name: 'Close and clear' }).click();
  await expect(page.getByLabel('New API key')).toHaveCount(0);
  const browserResidue = await page.evaluate(async () => ({
    body: document.body.innerText,
    local: JSON.stringify(localStorage),
    session: JSON.stringify(sessionStorage),
    caches: JSON.stringify(await caches.keys())
  }));
  expect(JSON.stringify(browserResidue)).not.toContain(secret);

  const row = page.getByRole('row').filter({ hasText: 'local development' });
  await row.getByRole('button', { name: 'Revoke' }).click();
  await expect(page.getByRole('group', { name: 'Confirm revocation of local development' })).toContainText('Active requests using this key will be cancelled.');
  await page.getByRole('button', { name: 'Confirm revoke' }).click();
  await expect(page.getByText('Revoked', { exact: true })).toBeVisible();
  expect(api.revokeCalls()).toBe(1);
  await page.setViewportSize({ width: 390, height: 844 });
  const pageWidth = await page.evaluate(() => document.documentElement.scrollWidth);
  expect(pageWidth).toBeLessThanOrEqual(390);
});

test('[keys] rejects invalid decimal caps locally and shows server privacy and rate-limit errors', async ({ page }) => {
  const api = await openKeys(page, { createStatus: 403 });
  await fillKeyForm(page, '1e2');
  await page.getByRole('button', { name: 'Create key' }).click();
  await expect(page.getByRole('alert')).toContainText('up to 12 decimal places');
  expect(api.createCalls()).toBe(0);

  await page.getByLabel('Daily spend cap · XMR').fill('0.01');
  await page.getByRole('button', { name: 'Create key' }).click();
  await expect(page.getByRole('alert')).toContainText('Accept the current privacy notice');
  expect(api.createCalls()).toBe(1);

  api.setCreateStatus(429);
  await page.getByRole('button', { name: 'Create key' }).click();
  await expect(page.getByRole('alert')).toContainText('Retry later (in 12s)');
  expect(api.createCalls()).toBe(2);
});

test('[keys] prevents duplicate submits while a create request is pending', async ({ page }) => {
  const api = await openKeys(page, { holdCreate: true });
  await fillKeyForm(page);
  await page.getByRole('button', { name: 'Create key' }).click();
  await api.waitForCreate();
  await page.locator('form').dispatchEvent('submit', { bubbles: true, cancelable: true });
  expect(api.createCalls()).toBe(1);
  api.releaseCreate();
  await expect(page.getByRole('heading', { name: 'Store this key securely' })).toBeVisible();
  expect(api.createCalls()).toBe(1);
});
