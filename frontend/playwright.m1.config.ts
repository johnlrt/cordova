import { defineConfig } from '@playwright/test';

const baseURL = process.env.CORDOVA_M1_ORIGIN;
if (!baseURL) throw new Error('CORDOVA_M1_ORIGIN must point to a running M1 HTTPS edge.');

export default defineConfig({
  testDir: './tests/m1',
  timeout: 90_000,
  expect: { timeout: 30_000 },
  fullyParallel: false,
  workers: 1,
  reporter: 'list',
  use: {
    baseURL,
    ignoreHTTPSErrors: true,
    browserName: 'chromium',
    headless: true,
    viewport: { width: 1280, height: 900 }
  }
});
