import { test, expect, type Page, type Route } from '@playwright/test';

const account = {
  id: 'c9eb01e9-899f-4edb-87fd-66e585ec9f79', username: 'node-owner',
  created_at: '2026-10-09T10:00:00Z', totp_enabled: false, privacy_notice_version: 'privacy-v1'
};
const csrf = 'synthetic-csrf-token-long-enough';

async function openApproval(page: Page, options: { status?: number; hold?: boolean } = {}) {
  let responseStatus = options.status ?? 204;
  let hold = options.hold ?? false;
  let requestCount = 0;
  let capturedCode = '';
  let release!: () => void;
  let started!: () => void;
  const blocked = new Promise<void>((resolve) => { release = resolve; });
  const requestStarted = new Promise<void>((resolve) => { started = resolve; });
  await page.route((url) => url.pathname.startsWith('/api/'), async (route: Route) => {
    const request = route.request();
    const path = new URL(request.url()).pathname;
    if (path === '/api/session') {
      await route.fulfill({ status: 200, json: { account, csrf_token: csrf, expires_at: '2026-10-10T10:00:00Z' } });
    } else if (path === '/api/node/enrollments/approve' && request.method() === 'POST') {
      requestCount++;
      started();
      capturedCode = String((request.postDataJSON() as { user_code?: string }).user_code ?? '');
      expect(request.headers()['x-csrf-token']).toBe(csrf);
      if (hold) await blocked;
      if (responseStatus === 204) {
        await route.fulfill({ status: 204, body: '' });
      } else {
        const code = responseStatus === 409 ? 'conflict' : responseStatus === 429 ? 'rate_limited' : 'not_found';
        const type = responseStatus === 409 ? 'invalid_request_error' : responseStatus === 429 ? 'rate_limit_error' : 'invalid_request_error';
        await route.fulfill({ status: responseStatus,
          headers: responseStatus === 429 ? { 'Retry-After': '8' } : {},
          json: { error: { message: 'Safe enrollment error.', type, param: null, code }, request_id: account.id }
        });
      }
    } else {
      await route.fulfill({ status: 404, json: { error: { message: 'Not found.', type: 'invalid_request_error', param: null, code: 'not_found' }, request_id: account.id } });
    }
  });
  await page.goto('/dashboard/nodes');
  await expect(page.getByRole('heading', { name: 'Approve node sign-in' })).toBeVisible();
  return {
    count: () => requestCount,
    code: () => capturedCode,
    status: (value: number) => { responseStatus = value; },
    hold: (value: boolean) => { hold = value; },
    waitStarted: () => requestStarted,
    release: () => release()
  };
}

async function fillAndConfirm(page: Page, code = 'ABCDE-FGHIJ-KLM') {
  await page.getByLabel('Code shown by the node CLI').fill(code);
  await page.getByLabel('I checked the CLI code and approve a node on a device I control.').check();
}

test('[nodes] approves a one-time code with owner confirmation, then clears it', async ({ page }) => {
  const api = await openApproval(page);
  await expect(page.getByRole('button', { name: 'Approve sign-in' })).toBeDisabled();
  expect(api.count()).toBe(0);
  await fillAndConfirm(page, 'abcde-fghij-klm');
  const codeValidity = await page.getByLabel('Code shown by the node CLI').evaluate((input: HTMLInputElement) => ({
    value: input.value, pattern: input.pattern, patternMismatch: input.validity.patternMismatch
  }));
  expect(codeValidity).toEqual({ value: 'ABCDE-FGHIJ-KLM', pattern: '[A-Z2-7]{5}-[A-Z2-7]{5}-[A-Z2-7]{3}', patternMismatch: false });
  await page.getByRole('button', { name: 'Approve sign-in' }).click();
  await expect.poll(api.count).toBe(1);
  await expect(page.getByRole('status')).toContainText('Sign-in approved');
  expect(api.count()).toBe(1);
  expect(api.code()).toBe('ABCDE-FGHIJ-KLM');
  await expect(page.getByLabel('Code shown by the node CLI')).toHaveValue('');
  expect(new URL(page.url()).search).toBe('');
  const stored = await page.evaluate(() => `${JSON.stringify(localStorage)} ${JSON.stringify(sessionStorage)}`);
  expect(stored).not.toContain('ABCDE-FGHIJ-KLM');
  await page.setViewportSize({ width: 390, height: 844 });
  expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(390);
});

test('[nodes] distinguishes expired, reused and disabled-enrollment responses', async ({ page }) => {
  const api = await openApproval(page, { status: 410 });
  await fillAndConfirm(page);
  await page.getByRole('button', { name: 'Approve sign-in' }).click();
  await expect(page.getByRole('alert')).toContainText('This code expired');

  api.status(409);
  await page.getByLabel('Code shown by the node CLI').fill('UVWXY-23456-AAA');
  await page.getByRole('button', { name: 'Approve sign-in' }).click();
  await expect(page.getByRole('alert')).toContainText('already approved');

  api.status(404);
  await page.getByRole('button', { name: 'Approve sign-in' }).click();
  await expect(page.getByRole('alert')).toContainText('has not enabled C3 enrollment');
  expect(api.count()).toBe(3);
});

test('[nodes] blocks duplicate approval while the server is processing the code', async ({ page }) => {
  const api = await openApproval(page, { hold: true });
  await fillAndConfirm(page);
  await page.getByRole('button', { name: 'Approve sign-in' }).click();
  await api.waitStarted();
  await page.locator('form').dispatchEvent('submit', { bubbles: true, cancelable: true });
  expect(api.count()).toBe(1);
  api.release();
  await expect(page.getByRole('status')).toContainText('Sign-in approved');
  expect(api.count()).toBe(1);
});
