import { test, expect, type Page } from '@playwright/test';
import { randomUUID } from 'node:crypto';

const password = `M1-safe-${randomUUID()}-A9!`;

async function register(page: Page, username: string) {
  await page.goto('/register');
  const registered = page.waitForResponse((response) =>
    new URL(response.url()).pathname === '/api/auth/register' && response.request().method() === 'POST'
  );
  await page.getByLabel('I have read and accept the privacy notice.').check();
  await page.getByLabel('Username').fill(username);
  await page.getByLabel('Password', { exact: true }).fill(password);
  await page.getByLabel('Confirm password').fill(password);
  await page.getByRole('button', { name: 'Create account' }).click();

  const response = await registered;
  expect(response.status()).toBe(201);
  const result = await response.json() as { csrf_token?: string };
  expect(result.csrf_token).toBeTruthy();
  await expect(page.locator('.recovery-code-list code').first()).toBeVisible();
  await page.getByLabel('I saved my recovery codes somewhere safe').check();
  await page.getByRole('button', { name: 'Continue to dashboard' }).click();
  await expect(page).toHaveURL(/\/dashboard$/);
  await expect(page.getByText('Live updates connected')).toBeVisible();
  return result.csrf_token as string;
}

test('M1 account dashboard receives real usage invalidation and isolates accounts', async ({ page }) => {
  const firstUsername = `m1-${randomUUID().replaceAll('-', '').slice(0, 16)}`;
  const csrfToken = await register(page, firstUsername);
  expect(new URL(page.url()).protocol).toBe('https:');
  await expect(page.getByText('No requests yet')).toBeVisible();

  const prompt = `M1-browser-${randomUUID()}`;
  const result = await page.evaluate(async ({ csrfToken, prompt }) => {
    const keyResponse = await fetch('/api/keys', {
      method: 'POST',
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrfToken },
      body: JSON.stringify({
        name: 'M1 browser verification',
        permissions: ['chat:write', 'models:read', 'usage:read'],
        model_ids: ['cordova-chat-small-v1'],
        daily_spend_cap_piconero: '0'
      })
    });
    const key = await keyResponse.json() as { secret?: string };
    if (!keyResponse.ok || !key.secret) return { keyStatus: keyResponse.status, completionStatus: 0, requestId: '', body: '' };

    const completion = await fetch('/v1/chat/completions', {
      method: 'POST',
      credentials: 'same-origin',
      headers: {
        Authorization: `Bearer ${key.secret}`,
        'Content-Type': 'application/json',
        Accept: 'text/event-stream'
      },
      body: JSON.stringify({
        model: 'cordova-chat-small-v1',
        messages: [{ role: 'user', content: prompt }],
        stream: true,
        stream_options: { include_usage: true }
      })
    });
    const body = await completion.text();
    let completionErrorCode = '';
    if (completion.status >= 400) {
      try {
        const error = JSON.parse(body) as { error?: { code?: string } };
        completionErrorCode = error.error?.code ?? '';
      } catch {
        completionErrorCode = 'invalid_error_response';
      }
    }
    return {
      keyStatus: keyResponse.status,
      completionStatus: completion.status,
      completionErrorCode,
      requestId: completion.headers.get('X-Request-ID') ?? '',
      body
    };
  }, { csrfToken, prompt });

  expect(result.keyStatus).toBe(201);
  expect(result.completionStatus, `chat completion error code: ${result.completionErrorCode || 'missing'}`).toBe(200);
  expect(result.requestId).toMatch(/^[0-9a-f-]{36}$/i);
  expect(result.body).toContain('[DONE]');
  expect(result.body).toContain('"usage"');

  await expect.poll(async () => page.evaluate(async (requestId) => {
    const [usageResponse, requestResponse] = await Promise.all([
      fetch('/api/usage?limit=5', { credentials: 'same-origin' }),
      fetch(`/api/requests/${encodeURIComponent(requestId)}`, { credentials: 'same-origin' })
    ]);
    const usage = usageResponse.ok
      ? await usageResponse.json() as { data?: Array<{ request_id?: string }> }
      : null;
    const request = requestResponse.ok
      ? await requestResponse.json() as { status?: string }
      : null;
    const usageRequestIds = usage?.data?.map((row) => row.request_id).filter((id): id is string => Boolean(id)) ?? [];
    return {
      usageStatus: usageResponse.status,
      usageRequestIds,
      requestStatus: requestResponse.status,
      requestState: request?.status ?? null,
      hasRequest: usageRequestIds.includes(requestId)
    };
  }, result.requestId), {
    message: 'REST usage projection did not contain the completed request',
    timeout: 15_000,
    intervals: [250, 500, 1_000, 2_000]
  }).toMatchObject({ hasRequest: true });
  await expect(page.getByText(`#${result.requestId.slice(0, 8)}`)).toBeVisible();
  await expect(page.locator('body')).not.toContainText(prompt);

  await page.getByRole('button', { name: 'Sign out' }).click();
  await expect(page).toHaveURL(/\/login$/);
  await register(page, `m1-${randomUUID().replaceAll('-', '').slice(0, 16)}`);
  await expect(page.getByText('No requests yet')).toBeVisible();
  await expect(page.locator('body')).not.toContainText(`#${result.requestId.slice(0, 8)}`);
  await expect(page.locator('body')).not.toContainText(prompt);
});
