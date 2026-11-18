import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { mkdtemp, readdir, rm } from 'node:fs/promises';
import { createServer } from 'node:net';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { setTimeout as delay } from 'node:timers/promises';
import test from 'node:test';

for (const blockedPort of [4420, 4421]) {
  test(
    `isolated browser fixture cleans up when port ${blockedPort} is occupied`,
    { timeout: 180_000 },
    async (t) => {
      const directory = await mkdtemp(join(tmpdir(), 'gallerygo-fixture-failure-'));
      let connections = 0;
      const blocker = createServer((socket) => {
        connections += 1;
        socket.destroy();
      });
      let child;
      let exited = false;
      let listening = false;
      t.after(async () => {
        if (child && !exited) {
          if (process.platform === 'win32') child.kill('SIGKILL');
          else {
            try {
              process.kill(-child.pid, 'SIGKILL');
            } catch (error) {
              if (error.code !== 'ESRCH') throw error;
            }
          }
        }
        if (listening)
          await new Promise((resolve, reject) =>
            blocker.close((error) => (error ? reject(error) : resolve())),
          );
        await rm(directory, { recursive: true, force: true });
      });
      await new Promise((resolve, reject) => {
        blocker.once('error', reject);
        blocker.listen(blockedPort, '127.0.0.1', resolve);
      });
      listening = true;
      const env = { ...process.env, TMPDIR: directory, GOTOOLCHAIN: 'local' };
      delete env.GALLERYGO_TEST_PASSWORD;
      delete env.GALLERYGO_E2E_HTTPS_PORT;
      delete env.GALLERYGO_E2E_HTTP_PORT;
      delete env.GALLERYGO_E2E_TEMP_PARENT;
      child = spawn(
        'pnpm',
        [
          'exec',
          'playwright',
          'test',
          'tests/e2e/source-scope-conflict.spec.ts',
          '--project=chromium',
          '--workers=1',
          '--reporter=list',
          '--output',
          join(directory, 'results'),
        ],
        { detached: process.platform !== 'win32', env, stdio: ['ignore', 'pipe', 'pipe'] },
      );
      let output = '';
      for (const stream of [child.stdout, child.stderr]) {
        stream.on('data', (chunk) => {
          output += chunk.toString();
        });
      }
      const stopped = new Promise((resolve, reject) => {
        child.once('error', reject);
        child.once('exit', (code, signal) => {
          exited = true;
          resolve({ code, signal });
        });
      });
      const result = await Promise.race([
        stopped,
        delay(150_000, undefined, { ref: false }).then(() => {
          throw new Error('nested browser test timed out');
        }),
      ]);
      assert.equal(result.code, 1, output);
      assert.match(output, /Isolated test server exited before readiness/);
      assert.match(output, /EADDRINUSE|address already in use/i);
      assert.equal(
        connections,
        0,
        'the occupied port must not receive health checks or API requests',
      );
      const remaining = await readdir(directory);
      assert.equal(
        remaining.some((entry) => entry.startsWith('gallerygo-isolated-e2e-')),
        false,
        remaining.join(', '),
      );
      assert.equal(
        remaining.some((entry) => entry.startsWith('gallerygo-e2e-')),
        false,
        remaining.join(', '),
      );
    },
  );
}
