import { defineConfig, devices } from '@playwright/test';

// Search conformance is isolated from the application server and user data.
export default defineConfig({
  testDir: './tests/search-browser',
  timeout: 120_000,
  fullyParallel: true,
  workers: 2,
  retries: 0,
  reporter: [['list']],
  use: { baseURL: 'http://127.0.0.1:4327', trace: 'retain-on-failure' },
  projects: [
    { name: 'chromium', use: { ...devices['Desktop Chrome'] } },
    { name: 'mobile-chromium', use: { ...devices['Pixel 7'] } },
  ],
  webServer: {
    command: 'pnpm --dir web exec vite --host 127.0.0.1 --port 4327 --strictPort',
    url: 'http://127.0.0.1:4327/',
    reuseExistingServer: false,
    timeout: 120_000,
    gracefulShutdown: { signal: 'SIGTERM', timeout: 15_000 },
  },
});
