import { defineConfig, devices } from '@playwright/test';
import { randomBytes } from 'node:crypto';

// The test-only bootstrap secret is generated per run and never written to source.
process.env.GALLERYGO_TEST_PASSWORD ??= randomBytes(24).toString('base64url');

export default defineConfig({
  testDir: './tests/e2e',
  // ponytail: one bootstrap account; use per-test accounts when admin creation lands.
  fullyParallel: false,
  forbidOnly: Boolean(process.env.CI),
  retries: 0,
  workers: 1,
  reporter: [['list'], ['html', { open: 'never' }]],
  use: {
    baseURL: 'https://127.0.0.1:4318',
    ignoreHTTPSErrors: true,
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
  },
  projects: [
    { name: 'chromium', use: { ...devices['Desktop Chrome'] } },
    { name: 'mobile-chromium', use: { ...devices['Pixel 7'] } },
  ],
  webServer: {
    command: 'node tests/e2e/server.mjs',
    url: 'https://127.0.0.1:4318/api/v2/health/ready',
    ignoreHTTPSErrors: true,
    reuseExistingServer: false,
    timeout: 120_000,
    gracefulShutdown: { signal: 'SIGTERM', timeout: 15_000 },
  },
});
