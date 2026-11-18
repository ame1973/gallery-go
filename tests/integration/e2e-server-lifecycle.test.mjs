import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { randomBytes } from 'node:crypto';
import { mkdtemp, readFile, readdir, rm, writeFile } from 'node:fs/promises';
import { get } from 'node:https';
import { createServer } from 'node:net';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { setTimeout as delay } from 'node:timers/promises';
import test from 'node:test';

async function reserve() {
  let connections = 0;
  const server = createServer((socket) => {
    connections += 1;
    socket.destroy();
  });
  await new Promise((resolve, reject) => {
    server.once('error', reject);
    server.listen(0, '127.0.0.1', resolve);
  });
  return {
    port: server.address().port,
    connections: () => connections,
    close: () =>
      new Promise((resolve, reject) =>
        server.close((error) => (error ? reject(error) : resolve())),
      ),
  };
}

async function fixture(t) {
  const parent = await mkdtemp(join(tmpdir(), 'gallerygo-isolated-e2e-'));
  await writeFile(join(parent, 'unrelated-canary'), 'preserve');
  const servers = [];
  let run;
  t.after(async () => {
    if (run && run.result === undefined) {
      if (process.platform === 'win32') run.child.kill('SIGKILL');
      else {
        try {
          process.kill(-run.child.pid, 'SIGKILL');
        } catch (error) {
          if (error.code !== 'ESRCH') throw error;
        }
      }
      await run.stopped;
    }
    for (const server of servers) await server.close();
    await rm(parent, { recursive: true, force: true });
  });
  async function allocate() {
    const server = await reserve();
    servers.push(server);
    return server;
  }
  async function unused() {
    const server = await reserve();
    await server.close();
    return server.port;
  }
  function start(httpsPort, httpPort, extra = {}) {
    const child = spawn(process.execPath, ['tests/e2e/server.mjs'], {
      detached: process.platform !== 'win32',
      env: {
        ...process.env,
        GALLERYGO_TEST_PASSWORD: randomBytes(24).toString('base64url'),
        GALLERYGO_E2E_HTTPS_PORT: String(httpsPort),
        GALLERYGO_E2E_HTTP_PORT: String(httpPort),
        GALLERYGO_E2E_TEMP_PARENT: parent,
        GOTOOLCHAIN: 'local',
        ...extra,
      },
      stdio: ['ignore', 'pipe', 'pipe'],
    });
    run = { child, stdout: '', stderr: '', result: undefined, stopped: undefined };
    child.stdout.on('data', (chunk) => {
      run.stdout += chunk.toString();
    });
    child.stderr.on('data', (chunk) => {
      run.stderr += chunk.toString();
    });
    run.stopped = new Promise((resolve, reject) => {
      child.once('error', reject);
      child.once('exit', (code, signal) => {
        run.result = { code, signal };
        resolve(run.result);
      });
    });
    return run;
  }
  async function waitStopped() {
    return Promise.race([
      run.stopped,
      delay(120_000, undefined, { ref: false }).then(() => {
        throw new Error('test server did not exit');
      }),
    ]);
  }
  async function waitReady(origin) {
    const deadline = Date.now() + 120_000;
    while (!run.stdout.split('\n').includes(`GALLERYGO_E2E_READY ${origin}`)) {
      if (run.result !== undefined) throw new Error(`test server exited: ${run.stderr}`);
      if (Date.now() >= deadline)
        throw new Error(`test server did not become ready: ${run.stderr}`);
      await delay(50);
    }
  }
  return { allocate, unused, start, waitReady, waitStopped, parent };
}

async function health(origin, ca) {
  return new Promise((resolve, reject) => {
    const request = get(`${origin}/api/v2/health/ready`, { ca }, (response) => {
      response.resume();
      response.on('end', () => resolve(response.statusCode));
    });
    request.on('error', reject);
    request.setTimeout(5_000, () => request.destroy(new Error('health request timed out')));
  });
}

test(
  'occupied HTTP port never receives proxy traffic or a ready marker',
  { timeout: 150_000 },
  async (t) => {
    const f = await fixture(t);
    const occupied = await f.allocate();
    const httpsPort = await f.unused();
    const run = f.start(httpsPort, occupied.port);
    const result = await f.waitStopped();
    assert.notEqual(result.code, 0);
    assert.match(run.stderr, /address already in use/i);
    assert.doesNotMatch(run.stdout, /GALLERYGO_E2E_READY/);
    assert.equal(occupied.connections(), 0);
    assert.deepEqual(await readdir(f.parent), ['unrelated-canary']);
  },
);

test(
  'occupied HTTPS port stops the owned Go server without reporting ready',
  { timeout: 150_000 },
  async (t) => {
    const f = await fixture(t);
    const occupied = await f.allocate();
    const httpPort = await f.unused();
    const run = f.start(occupied.port, httpPort);
    const result = await f.waitStopped();
    assert.notEqual(result.code, 0);
    assert.match(run.stderr, /EADDRINUSE/);
    assert.doesNotMatch(run.stdout, /GALLERYGO_E2E_READY/);
    assert.equal(occupied.connections(), 0);
    assert.deepEqual(await readdir(f.parent), ['unrelated-canary']);
  },
);

test(
  'ready marker identifies owned listeners and shutdown removes only owned files',
  { timeout: 150_000 },
  async (t) => {
    const f = await fixture(t);
    const httpsPort = await f.unused();
    let httpPort = await f.unused();
    while (httpPort === httpsPort) httpPort = await f.unused();
    const origin = `https://127.0.0.1:${httpsPort}`;
    const run = f.start(httpsPort, httpPort);
    await f.waitReady(origin);
    const entries = await readdir(f.parent);
    assert.equal(entries.length, 2);
    const ownDirectory = entries.find((entry) => entry.startsWith('gallerygo-e2e-'));
    assert.ok(ownDirectory);
    const ca = await readFile(join(f.parent, ownDirectory, 'tls.pem'));
    assert.equal(await health(origin, ca), 200);
    run.child.kill('SIGTERM');
    const result = await f.waitStopped();
    assert.equal(result.code, 0);
    assert.deepEqual(await readdir(f.parent), ['unrelated-canary']);
  },
);

test(
  'invalid configuration exits without allocating a test database',
  { timeout: 30_000 },
  async (t) => {
    const f = await fixture(t);
    const run = f.start(4420, 4421, { GALLERYGO_E2E_HTTP_PORT: '0' });
    const result = await f.waitStopped();
    assert.notEqual(result.code, 0);
    assert.match(run.stderr, /Invalid test port/);
    assert.doesNotMatch(run.stdout, /GALLERYGO_E2E_READY/);
    assert.deepEqual(await readdir(f.parent), ['unrelated-canary']);
  },
);
