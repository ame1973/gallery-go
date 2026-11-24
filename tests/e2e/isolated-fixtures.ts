import { spawn } from 'node:child_process';
import { randomBytes } from 'node:crypto';
import { mkdtemp, readdir, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { createInterface } from 'node:readline';
import { setTimeout as delay } from 'node:timers/promises';
import type { APIRequestContext } from '@playwright/test';
import { expect, test as base } from './fixtures';

type IsolatedServer = { origin: string; password: string; scanSource: string | null };

// Only tests that need a private database use this worker-local server. Other
// projects keep their existing shared two-worker concurrency and default ports.
export const isolatedTest = base.extend<
  object,
  { isolatedServer: IsolatedServer; realScan: boolean }
>({
  realScan: [false, { scope: 'worker', option: true }],
  isolatedServer: [
    async ({ playwright, realScan }, use, workerInfo) => {
      const parent = await mkdtemp(join(tmpdir(), 'gallerygo-isolated-e2e-'));
      const httpsPort = 4420 + workerInfo.parallelIndex * 2;
      const httpPort = httpsPort + 1;
      const origin = `https://127.0.0.1:${httpsPort}`;
      const password = randomBytes(24).toString('base64url');
      const child = spawn(process.execPath, ['tests/e2e/server.mjs'], {
        cwd: process.cwd(),
        detached: process.platform !== 'win32',
        env: {
          ...process.env,
          GALLERYGO_TEST_PASSWORD: password,
          GALLERYGO_E2E_HTTPS_PORT: String(httpsPort),
          GALLERYGO_E2E_HTTP_PORT: String(httpPort),
          GALLERYGO_E2E_TEMP_PARENT: parent,
          GALLERYGO_E2E_REAL_SCAN: realScan ? '1' : '0',
          GOTOOLCHAIN: 'local',
        },
        stdio: ['ignore', 'pipe', 'pipe'],
      });
      let output = '';
      let ready = false;
      createInterface({ input: child.stdout }).on('line', (line) => {
        output = (output + line + '\n').slice(-16_000);
        if (line === `GALLERYGO_E2E_READY ${origin}`) ready = true;
      });
      child.stderr.on('data', (chunk: Buffer) => {
        output = (output + chunk.toString('utf8')).slice(-16_000);
      });
      let exited = false;
      let spawnError: Error | undefined;
      child.once('error', (error) => {
        spawnError = error;
      });
      const stopped = new Promise<void>((resolve) => {
        child.once('exit', () => {
          exited = true;
          resolve();
        });
        child.once('error', () => {
          exited = true;
          resolve();
        });
      });
      let probe: APIRequestContext | undefined;
      try {
        const activeProbe = await playwright.request.newContext({
          baseURL: origin,
          ignoreHTTPSErrors: true,
        });
        probe = activeProbe;
        await expect
          .poll(
            async () => {
              if (spawnError || exited) {
                throw new Error(
                  `Isolated test server exited before readiness: ${spawnError ?? output}`,
                );
              }
              // Only connect after the child confirms both listeners are its own.
              if (!ready) return 0;
              try {
                return (await activeProbe.get('/api/v2/health/ready', { timeout: 1000 })).status();
              } catch {
                return 0;
              }
            },
            { timeout: 120_000, intervals: [100, 250, 500] },
          )
          .toBe(200);
        let scanSource: string | null = null;
        if (realScan) {
          const directories = (await readdir(parent, { withFileTypes: true })).filter(
            (entry) => entry.isDirectory() && entry.name.startsWith('gallerygo-e2e-'),
          );
          if (directories.length !== 1) throw new Error('Expected one owned test directory');
          scanSource = join(
            parent,
            directories[0]!.name,
            'sources',
            'minimum-flow',
            'Album',
            'photo.png',
          );
        }
        await use({ origin, password, scanSource });
      } finally {
        try {
          await probe?.dispose();
        } finally {
          try {
            const groupExists = () => {
              if (process.platform === 'win32' || child.pid === undefined) return !exited;
              try {
                process.kill(-child.pid, 0);
                return true;
              } catch (error) {
                if ((error as NodeJS.ErrnoException).code === 'ESRCH') return false;
                throw error;
              }
            };
            const signalGroup = (signal: NodeJS.Signals) => {
              if (process.platform === 'win32' || child.pid === undefined) {
                child.kill(signal);
                return;
              }
              try {
                process.kill(-child.pid, signal);
              } catch (error) {
                if ((error as NodeJS.ErrnoException).code !== 'ESRCH') throw error;
              }
            };
            // Signal the whole owned group, including a build subprocess if setup failed.
            signalGroup('SIGTERM');
            const deadline = Date.now() + 15_000;
            while (groupExists() && Date.now() < deadline) await delay(100);
            if (groupExists()) signalGroup('SIGKILL');
            await Promise.race([
              stopped,
              delay(5_000, undefined, { ref: false }).then(() => {
                throw new Error('Isolated test server did not exit after shutdown');
              }),
            ]);
          } finally {
            await rm(parent, { recursive: true, force: true });
          }
        }
      }
    },
    { scope: 'worker', timeout: 150_000 },
  ],
  administrator: [
    async ({ playwright, isolatedServer }, use) => {
      const api = await playwright.request.newContext({
        baseURL: isolatedServer.origin,
        ignoreHTTPSErrors: true,
        extraHTTPHeaders: { Origin: isolatedServer.origin },
      });
      try {
        const login = await api.post('/api/v2/auth/login', {
          data: { username: 'e2e-owner', password: isolatedServer.password },
        });
        expect(login.status()).toBe(200);
        const csrf = await api.get('/api/v2/auth/csrf');
        expect(csrf.status()).toBe(200);
        const { token } = (await csrf.json()) as { token: string };
        const headers = { Origin: isolatedServer.origin, 'X-CSRF-Token': token };
        await use({ api, headers });
        expect((await api.post('/api/v2/auth/logout', { headers })).status()).toBe(204);
      } finally {
        await api.dispose();
      }
    },
    { scope: 'worker' },
  ],
  baseURL: async ({ isolatedServer }, use) => {
    await use(isolatedServer.origin);
  },
});
