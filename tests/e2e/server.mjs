import { spawn, spawnSync } from 'node:child_process';
import { mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';

// Only this process's freshly allocated test directory is removed on shutdown.
const directory = mkdtempSync(join(tmpdir(), 'gallerygo-e2e-'));
const binary = join(directory, 'gallerygo');
const config = join(directory, 'config.yaml');
writeFileSync(
  config,
  JSON.stringify({
    listen: '127.0.0.1:4318',
    base_url: 'https://localhost:8443',
    database: join(directory, 'data/gallerygo.db'),
    cache_root: join(directory, 'cache'),
    web_root: resolve('web/dist'),
    sources: [],
  }),
  { mode: 0o600 },
);

for (const [command, args] of [
  ['go', ['build', '-o', binary, './cmd/gallerygo']],
  [binary, ['migrate', '--config', config]],
]) {
  const result = spawnSync(command, args, { stdio: 'inherit' });
  if (result.status !== 0) {
    rmSync(directory, { recursive: true, force: true });
    process.exit(result.status ?? 1);
  }
}

const child = spawn(binary, ['serve', '--config', config], { stdio: 'inherit' });
let stopping = false;
for (const signal of ['SIGINT', 'SIGTERM']) {
  process.on(signal, () => {
    stopping = true;
    child.kill(signal);
  });
}
child.on('error', (error) => {
  console.error(error);
  process.exitCode = 1;
});
child.on('exit', (code) => {
  rmSync(directory, { recursive: true, force: true });
  process.exit(stopping ? 0 : (code ?? 1));
});
