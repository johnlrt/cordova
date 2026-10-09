import { defineConfig } from '@playwright/test';

export default defineConfig({
  testDir: './tests/browser',
  timeout: 20_000,
  expect: { timeout: 5_000 },
  fullyParallel: false,
  workers: 1,
  reporter: 'list',
  use: {
    baseURL: 'http://127.0.0.1:5173',
    browserName: 'chromium',
    headless: true,
    viewport: { width: 1280, height: 900 }
  },
  webServer: {
    command: 'npm run dev',
    url: 'http://127.0.0.1:5173/login',
    reuseExistingServer: false,
    timeout: 60_000
  }
});
