import { spawn, spawnSync } from 'node:child_process';
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { request as proxyRequest } from 'node:http';
import { createServer } from 'node:https';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';

// Only this process's freshly allocated test directory is removed on shutdown.
const directory = mkdtempSync(join(tmpdir(), 'gallerygo-e2e-'));
const binary = join(directory, 'gallerygo');
const config = join(directory, 'config.yaml');
const key = join(directory, 'tls.key');
const certificate = join(directory, 'tls.pem');
const password = process.env.GALLERYGO_TEST_PASSWORD;
if (!password) throw new Error('The Playwright harness must supply its generated test password');
writeFileSync(
  config,
  JSON.stringify({
    listen: '127.0.0.1:4319',
    base_url: 'https://127.0.0.1:4318',
    database: join(directory, 'data/gallerygo.db'),
    cache_root: join(directory, 'cache'),
    web_root: resolve('web/dist'),
    sources: [],
  }),
  { mode: 0o600 },
);

function run(command, args, input) {
  const result = spawnSync(command, args, { encoding: 'utf8', input });
  if (result.status !== 0) {
    process.stderr.write(result.stderr ?? 'Command failed');
    rmSync(directory, { recursive: true, force: true });
    process.exit(result.status ?? 1);
  }
}
run('openssl', [
  'req',
  '-x509',
  '-newkey',
  'ec',
  '-pkeyopt',
  'ec_paramgen_curve:P-256',
  '-nodes',
  '-keyout',
  key,
  '-out',
  certificate,
  '-days',
  '1',
  '-subj',
  '/CN=localhost',
  '-addext',
  'subjectAltName=DNS:localhost,IP:127.0.0.1',
]);
run('go', ['build', '-o', binary, './cmd/gallerygo']);
run(binary, ['migrate', '--config', config]);
run(
  binary,
  ['bootstrap-admin', '--config', config, '--username', 'e2e-owner', '--password-stdin'],
  password + '\n',
);

// Real TLS termination keeps Secure __Host cookies and Origin checks enabled.
const proxy = createServer(
  { key: readFileSync(key), cert: readFileSync(certificate) },
  (request, response) => {
    const upstream = proxyRequest(
      {
        hostname: '127.0.0.1',
        port: 4319,
        path: request.url,
        method: request.method,
        headers: request.headers,
      },
      (result) => {
        response.writeHead(result.statusCode ?? 502, result.headers);
        result.pipe(response);
      },
    );
    upstream.on('error', () => {
      if (!response.headersSent) response.writeHead(502);
      response.end();
    });
    request.on('aborted', () => upstream.destroy());
    request.pipe(upstream);
  },
);
proxy.listen(4318, '127.0.0.1');
const child = spawn(binary, ['serve', '--config', config], { stdio: 'inherit' });
let stopping = false;
for (const signal of ['SIGINT', 'SIGTERM']) {
  process.on(signal, () => {
    stopping = true;
    proxy.close();
    child.kill(signal);
  });
}
child.on('error', (error) => {
  console.error(error);
  process.exitCode = 1;
});
child.on('exit', (code) => {
  proxy.closeAllConnections();
  proxy.close();
  rmSync(directory, { recursive: true, force: true });
  process.exit(stopping ? 0 : (code ?? 1));
});
