import { test, expect, type Page, type Route, type WebSocketRoute } from '@playwright/test';

const accountA = {
  id: 'c9eb01e9-899f-4edb-87fd-66e585ec9f79', username: 'alpha-user',
  created_at: '2026-10-09T10:00:00Z', totp_enabled: false, privacy_notice_version: 'privacy-v1'
};
const accountB = {
  id: '4e345523-1a8c-4f4b-9d48-0593cf3e7eca', username: 'beta-user',
  created_at: '2026-10-09T10:00:00Z', totp_enabled: false, privacy_notice_version: 'privacy-v1'
};
const csrf = 'synthetic-csrf-token-long-enough';
const requestA = '706134d8-0590-437c-ab24-bb9c396c815b';
const requestB = '7c081ea0-a8f5-4635-9c3d-a6bb2eaad5f2';

function session(account: typeof accountA) {
  return { account, csrf_token: csrf, expires_at: '2026-10-10T10:00:00Z' };
}

function usage(requestId: string) {
  return {
    data: [{
      request_id: requestId, api_key_id: 'c9eb01e9-899f-4edb-87fd-66e585ec9f80', model_id: 'cordova-chat-small-v1', status: 'completed', input_tokens: 4, output_tokens: 5,
      charged_piconero: '0', latency_ms: 18, created_at: '2026-10-09T10:00:00Z', finished_at: '2026-10-09T10:00:01Z'
    }],
    next_cursor: null
  };
}

async function mockApi(page: Page) {
  let current: ReturnType<typeof session> | null = null;
  let usageForAccount = usage(requestA);
  let socketCloses = 0;
  let liveSocket: WebSocketRoute | undefined;
  let liveAccountId = '';
  let liveSequence = 0;
  let holdNextUsageResponse: (() => Promise<void>) | undefined;
  await page.routeWebSocket('ws://127.0.0.1:5173/api/live', (socket) => {
    liveSocket = socket;
    socket.onMessage((raw) => {
      const message = JSON.parse(typeof raw === 'string' ? raw : raw.toString());
      if (message.type !== 'live.auth') return;
      liveAccountId = String(message.payload.ticket).slice(-36);
      liveSequence = 1;
      socket.send(JSON.stringify({
        v: 1, type: 'live.ready', message_id: liveAccountId, request_id: message.request_id,
        session_id: liveAccountId, seq: 1, sent_at: new Date().toISOString(),
        payload: { session_id: liveAccountId, account_id: liveAccountId, heartbeat_interval_ms: 15000 }, error: null
      }));
    });
    socket.onClose(() => { socketCloses++; });
  });

  await page.route((url) => url.pathname.startsWith('/api/'), async (route: Route) => {
    const request = route.request();
    const path = new URL(request.url()).pathname;
    if (path === '/api/auth/register') {
      current = session(accountA);
      usageForAccount = usage(requestA);
      await route.fulfill({ status: 201, json: { ...current, recovery_codes: ['synthetic-recovery-1', 'synthetic-recovery-2'] } });
    } else if (path === '/api/auth/login') {
      const body = request.postDataJSON() as { username?: string };
      const account = body.username === accountB.username ? accountB : accountA;
      current = session(account);
      usageForAccount = account === accountA ? usage(requestA) : usage(requestB);
      await route.fulfill({ status: 200, json: current });
    } else if (path === '/api/auth/recover') {
      current = session(accountA);
      await route.fulfill({ status: 200, json: current });
    } else if (path === '/api/auth/logout') {
      current = null;
      await route.fulfill({ status: 204, body: '' });
    } else if (path === '/api/session') {
      if (current) await route.fulfill({ status: 200, json: current });
      else await route.fulfill({ status: 401, json: {
        error: { message: 'Authentication required.', type: 'authentication_error', param: null, code: 'unauthenticated' },
        request_id: accountA.id
      } });
    } else if (path === '/api/usage') {
      if (!current) {
        await route.fulfill({ status: 401, json: {
          error: { message: 'Authentication required.', type: 'authentication_error', param: null, code: 'unauthenticated' },
          request_id: accountA.id
        } });
      } else {
        const hold = holdNextUsageResponse;
        holdNextUsageResponse = undefined;
        if (hold) await hold();
        await route.fulfill({ status: 200, json: usageForAccount });
      }
    } else if (path === '/api/live-ticket') {
      await route.fulfill({ status: 200, json: {
        ticket: `synthetic-one-use-live-ticket-${current?.account.id ?? accountA.id}`,
        expires_at: new Date(Date.now() + 10_000).toISOString()
      } });
    } else {
      await route.fulfill({ status: 404, json: {
        error: { message: 'Not found.', type: 'invalid_request_error', param: null, code: 'not_found' },
        request_id: accountA.id
      } });
    }
  });
  return {
    socketCloseCount: () => socketCloses,
    holdNextUsage(value: ReturnType<typeof usage>) {
      usageForAccount = value;
      let notifyStarted!: () => void;
      let releaseResponse!: () => void;
      const started = new Promise<void>((resolve) => { notifyStarted = resolve; });
      const blocked = new Promise<void>((resolve) => { releaseResponse = resolve; });
      holdNextUsageResponse = async () => { notifyStarted(); await blocked; };
      return { started, release: () => releaseResponse() };
    },
    publishUsageInvalidation() {
      if (!liveSocket || !liveAccountId) throw new Error('live_socket_not_ready');
      liveSequence++;
      liveSocket.send(JSON.stringify({
        v: 1, type: 'live.event', message_id: liveAccountId, request_id: null,
        session_id: liveAccountId, seq: liveSequence, sent_at: new Date().toISOString(),
        payload: { event_id: 'synthetic-usage-event', topic: 'usage', resource_id: requestA, revision: 1, action: 'invalidate' },
        error: null
      }));
    }
  };
}

async function fillCredentials(page: Page, username: string) {
  await page.getByLabel('Username').fill(username);
  await page.getByLabel('Password', { exact: true }).fill('synthetic-password-long-enough');
}

test('[auth] registration reveals recovery codes once and then loads account usage', async ({ page }) => {
  await mockApi(page);
  await page.goto('/register');
  await page.waitForLoadState('networkidle');
  await fillCredentials(page, accountA.username);
  await page.getByLabel('Confirm password').fill('synthetic-password-long-enough');
  await page.getByLabel('I have read and accept the privacy notice.').check();
  await page.getByRole('button', { name: 'Create account' }).click();

  await expect(page.getByText('synthetic-recovery-1')).toBeVisible();
  expect(await page.evaluate(() => JSON.stringify(localStorage))).not.toContain('synthetic-recovery');
  await page.getByLabel('I saved my recovery codes somewhere safe').check();
  await page.getByRole('button', { name: 'Continue to dashboard' }).click();
  await expect(page).toHaveURL(/\/dashboard$/);
  await expect(page.getByText(`#${requestA.slice(0, 8)}`)).toBeVisible();
  expect(await page.locator('body').innerText()).not.toContain('synthetic-password-long-enough');
});

test('[auth] direct dashboard URL redirects signed-out users to sign-in with a safe return path', async ({ page }) => {
  await mockApi(page);
  await page.goto('/dashboard?tab=usage');
  await page.waitForLoadState('networkidle');
  await expect(page).toHaveURL(/\/login\?returnTo=/);
  await expect(page.getByRole('heading', { name: 'Sign in to Cordova' })).toBeVisible();
  expect(new URL(page.url()).searchParams.get('returnTo')).toBe('/dashboard?tab=usage');
});

test('[auth] logout clears account content and a following account sees only its own usage', async ({ page }) => {
  const api = await mockApi(page);
  await page.goto('/login');
  await page.waitForLoadState('networkidle');
  await page.getByRole('button', { name: 'RU' }).click();
  await expect(page.getByRole('heading', { name: 'Войти в Cordova' })).toBeVisible();
  await page.getByRole('button', { name: 'EN' }).click();
  await expect(page.getByRole('heading', { name: 'Sign in to Cordova' })).toBeVisible();
  await fillCredentials(page, accountA.username);
  await page.getByRole('button', { name: 'Sign in' }).click();
  await expect(page.getByText('Live updates connected')).toBeVisible();

  await page.getByRole('button', { name: 'Sign out' }).click();
  await expect(page).toHaveURL(/\/login$/);
  await expect.poll(api.socketCloseCount).toBe(1);

  await fillCredentials(page, accountB.username);
  await page.getByRole('button', { name: 'Sign in' }).click();
  await expect(page.getByText(`#${requestB.slice(0, 8)}`)).toBeVisible();
  await expect(page.getByText(`#${requestA.slice(0, 8)}`)).toHaveCount(0);
});

test('[auth] recovery confirms the post-recovery payout cooldown before opening the dashboard', async ({ page }) => {
  await mockApi(page);
  await page.goto('/recover');
  await page.waitForLoadState('networkidle');
  await page.getByLabel('Username').fill(accountA.username);
  await page.getByLabel('One-time recovery code').fill('synthetic-recovery-code');
  await page.getByLabel('New password').fill('synthetic-new-password-long-enough');
  await page.getByLabel('Confirm password').fill('synthetic-new-password-long-enough');
  await page.getByRole('button', { name: 'Recover account' }).click();
  await expect(page.getByRole('status')).toContainText('Payouts are locked for 24 hours');
  await page.getByRole('link', { name: 'Continue to dashboard' }).click();
});

test('[live] invalidation refreshes usage only after the authoritative REST response', async ({ page }) => {
  const api = await mockApi(page);
  await page.goto('/login');
  await page.waitForLoadState('networkidle');
  await fillCredentials(page, accountA.username);
  await page.getByRole('button', { name: 'Sign in' }).click();
  await expect(page.getByText('cordova-chat-small-v1', { exact: true })).toBeVisible();
  await expect(page.getByText('Live updates connected')).toBeVisible();

  const held = api.holdNextUsage(usage(requestB));
  api.publishUsageInvalidation();
  await held.started;
  await expect(page.getByText(`#${requestA.slice(0, 8)}`)).toBeVisible();
  await expect(page.getByText(`#${requestB.slice(0, 8)}`)).toHaveCount(0);
  held.release();
  await expect(page.getByText(`#${requestB.slice(0, 8)}`)).toBeVisible();
  await expect(page.getByText(`#${requestA.slice(0, 8)}`)).toHaveCount(0);
});
