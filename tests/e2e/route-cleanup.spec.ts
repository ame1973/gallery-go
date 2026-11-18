import { expect, test } from './fixtures';

test('cleanup waits for a held route after an earlier failure', async ({ page }) => {
  let release!: () => void;
  let entered!: () => void;
  const held = new Promise<void>((resolve) => {
    release = resolve;
  });
  const started = new Promise<void>((resolve) => {
    entered = resolve;
  });
  await page.route('**/api/v2/health/ready', async (route) => {
    entered();
    await held;
    await route.continue();
  });
  const navigation = page.goto('/api/v2/health/ready');
  void navigation.catch(() => {});
  const failure = new Error('intentional preceding failure');
  try {
    await started;
    throw failure;
  } catch (error) {
    expect(error).toBe(failure);
  } finally {
    release();
    await page.unrouteAll({ behavior: 'wait' });
  }
  expect((await navigation)?.status()).toBe(200);
});

test('cleanup handles a route that was never entered', async ({ page }) => {
  let entered = false;
  await page.route('**/api/v2/health/ready', async (route) => {
    entered = true;
    await route.continue();
  });
  await page.unrouteAll({ behavior: 'wait' });
  expect((await page.goto('/api/v2/health/ready'))?.status()).toBe(200);
  expect(entered).toBe(false);
});
