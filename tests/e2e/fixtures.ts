import { test as base, expect } from '@playwright/test';
import type { APIRequestContext, Page } from '@playwright/test';
import { randomBytes, randomUUID } from 'node:crypto';
import type { components } from '../../web/src/lib/api/schema';

export type Account = {
  user: components['schemas']['User'];
  password: string;
};

type Administrator = {
  api: APIRequestContext;
  headers: { Origin: string; 'X-CSRF-Token': string };
};

export const test = base.extend<{ account: Account }, { administrator: Administrator }>({
  administrator: [
    async ({ playwright }, use) => {
      const password = process.env.GALLERYGO_TEST_PASSWORD;
      if (!password) throw new Error('Missing generated test bootstrap password');
      const origin = 'https://127.0.0.1:4318';
      const api = await playwright.request.newContext({
        baseURL: origin,
        ignoreHTTPSErrors: true,
        extraHTTPHeaders: { Origin: origin },
      });
      try {
        const login = await api.post('/api/v2/auth/login', {
          data: { username: 'e2e-owner', password },
        });
        expect(login.status()).toBe(200);
        const response = await api.get('/api/v2/auth/csrf');
        expect(response.status()).toBe(200);
        const { token } = (await response.json()) as { token: string };
        const headers = { Origin: origin, 'X-CSRF-Token': token };
        await use({ api, headers });
        expect((await api.post('/api/v2/auth/logout', { headers })).status()).toBe(204);
      } finally {
        await api.dispose();
      }
    },
    { scope: 'worker' },
  ],
  account: async ({ administrator }, use) => {
    const password = randomBytes(24).toString('base64url');
    const response = await administrator.api.post('/api/v2/admin/users', {
      headers: { ...administrator.headers, 'Idempotency-Key': randomUUID() },
      data: { username: `reader-${randomUUID()}`, password },
    });
    expect(response.status()).toBe(201);
    const user = (await response.json()) as Account['user'];
    const path = `/api/v2/admin/users/${user.id}`;
    try {
      await use({ user, password });
    } finally {
      // Cleanup uses the same real, versioned API as a user, never a test backdoor.
      const current = await administrator.api.get(path);
      if (current.status() !== 404) {
        expect(current.status()).toBe(200);
        const removed = await administrator.api.delete(path, {
          headers: { ...administrator.headers, 'If-Match': current.headers().etag! },
        });
        expect(removed.status()).toBe(204);
      }
    }
  },
});

export async function signIn(page: Page, account: Account, password = account.password) {
  await page.goto('/login');
  await page.getByLabel('Username', { exact: true }).fill(account.user.username);
  await page.getByLabel('Password', { exact: true }).fill(password);
  await page.getByRole('button', { name: 'Sign in', exact: true }).click();
  await expect(page.getByRole('heading', { name: 'Account security' })).toBeVisible();
}

export { expect };
