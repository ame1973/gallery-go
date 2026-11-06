import { spawn, spawnSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { mkdirSync, mkdtempSync, readFileSync, rmSync, statSync, writeFileSync } from 'node:fs';
import { request as proxyRequest } from 'node:http';
import { createServer } from 'node:https';
import { tmpdir } from 'node:os';
import { dirname, join, resolve } from 'node:path';
import { DatabaseSync } from 'node:sqlite';

// Only this process's freshly allocated test directory is removed on shutdown.
const directory = mkdtempSync(join(tmpdir(), 'gallerygo-e2e-'));
const binary = join(directory, 'gallerygo');
const config = join(directory, 'config.yaml');
const key = join(directory, 'tls.key');
const certificate = join(directory, 'tls.pem');
const password = process.env.GALLERYGO_TEST_PASSWORD;
const fixtureSources = ['chromium', 'mobile-chromium'].flatMap((project) =>
  ['media', 'outage', 'duplicates', 'people', 'album-metadata', 'person-metadata'].map((kind) => ({
    name: `${kind}-${project}`,
    root: join(directory, 'sources', `${kind}-${project}`),
    marker: '.marker',
  })),
);
const managementRoots = ['chromium', 'mobile-chromium'].flatMap((project) =>
  ['managed', 'unmounted'].map((kind) => ({
    name: `${kind}-${project}`,
    root: join(directory, 'sources', `${kind}-${project}`),
    marker: '.marker',
  })),
);
if (!password) throw new Error('The Playwright harness must supply its generated test password');
writeFileSync(
  config,
  JSON.stringify({
    listen: '127.0.0.1:4319',
    base_url: 'https://127.0.0.1:4318',
    database: join(directory, 'data/gallerygo.db'),
    cache_root: join(directory, 'cache'),
    web_root: resolve('web/dist'),
    sources: [...fixtureSources, ...managementRoots],
    // These control/seeded-content cases deliberately do not consume their jobs.
    // Actual ingestion uses the separate compiled CLI integration test.
    worker: { embedded: false, required: false },
    scheduler: { enabled: false },
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

// Seed only this newly migrated temporary DB, before Go starts. Each project's
// ACL case owns separate resources; no production seed command or route exists.
let fixtureDB;
let fixturesReady = false;
try {
  // These roots are registered through real administrative APIs by the test.
  // Intentionally missing roots must remain missing; Go must not create them.
  for (const root of managementRoots.filter((root) => root.name.startsWith('managed-'))) {
    mkdirSync(root.root, { recursive: true, mode: 0o700 });
    writeFileSync(join(root.root, root.marker), 'isolated management marker', { mode: 0o400 });
  }
  fixtureDB = new DatabaseSync(join(directory, 'data/gallerygo.db'));
  fixtureDB.exec('PRAGMA foreign_keys=ON; PRAGMA busy_timeout=5000; BEGIN IMMEDIATE');
  for (const project of ['chromium', 'mobile-chromium']) {
    // Synthetic queued jobs exercise HTTPS controls only; no scanner or worker
    // execution is implied by these fresh, project-isolated setup records.
    for (const suffix of ['queued', 'stable']) {
      const id = `jobs-${project}-${suffix}`;
      const now = Date.now();
      fixtureDB
        .prepare(
          "INSERT INTO jobs(id,kind,resource_key,dedupe_key,dedupe_token,payload_json,status,max_attempts,next_run_at,created_at,updated_at) VALUES(?,'scan',?,?,?,?,'queued',3,?,?,?)",
        )
        .run(
          id,
          `source:${id}`,
          `scan:${id}`,
          `scan:${id}`,
          JSON.stringify({ private_path: directory }),
          now,
          now,
          now,
        );
    }
    const library = `acl-${project}-library`;
    fixtureDB
      .prepare('INSERT INTO libraries(id,name,created_at,updated_at) VALUES(?,?,?,?)')
      .run(library, 'Isolated ACL fixture', Date.now(), Date.now());
    for (const name of ['open', 'restricted']) {
      fixtureDB
        .prepare(
          'INSERT INTO albums(id,library_id,source_name,created_at,updated_at) VALUES(?,?,?,?,?)',
        )
        .run(`acl-${project}-${name}`, library, `Fixture ${name}`, Date.now(), Date.now());
    }
    fixtureDB
      .prepare('INSERT INTO tags(id,name) VALUES(?,?)')
      .run(`tags-${project}-assigned`, `Private assigned tag-${project}`);
    fixtureDB
      .prepare('INSERT INTO album_tags(album_id,tag_id) VALUES(?,?)')
      .run(`acl-${project}-restricted`, `tags-${project}-assigned`);
  }
  // A real one-pixel PNG encoded with Go's image/png, not a codec mock. These
  // ready records are test setup only; the production scanner/publisher is separate.
  const png = Buffer.from(
    'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAAEElEQVR4nGKSq9gCCAAA//8CCgFNa0v7mgAAAABJRU5ErkJggg==',
    'base64',
  );
  const hash = createHash('sha256').update(png).digest('hex');
  for (const source of fixtureSources) {
    const prefix = source.name;
    const library = `${prefix}-library`;
    const album = `${prefix}-album`;
    mkdirSync(source.root, { recursive: true, mode: 0o700 });
    if (!prefix.startsWith('outage-')) {
      writeFileSync(join(source.root, source.marker), 'isolated test mount', { mode: 0o400 });
    }
    const original = join(source.root, 'fixture.png');
    writeFileSync(original, png, { mode: 0o400 });
    fixtureDB
      .prepare('INSERT INTO libraries(id,name,created_at,updated_at) VALUES(?,?,?,?)')
      .run(library, 'Isolated media fixture', Date.now(), Date.now());
    fixtureDB
      .prepare(
        "INSERT INTO sources(id,library_id,name,root_path,root_config_sha256,mapping_json,state,created_at) VALUES(?,?,?,?,?,'{}','healthy',?)",
      )
      .run(
        `${prefix}-source`,
        library,
        source.name,
        source.root,
        createHash('sha256')
          .update(JSON.stringify([source.root, source.marker]))
          .digest('hex'),
        Date.now(),
      );
    fixtureDB
      .prepare(
        "INSERT INTO albums(id,library_id,source_id,source_name,relative_path,created_at,updated_at) VALUES(?,?,?,? ,'.',?,?)",
      )
      .run(album, library, `${prefix}-source`, 'Real PNG fixture', Date.now(), Date.now());
    if (prefix.startsWith('people-') || prefix.startsWith('person-metadata-')) {
      fixtureDB
        .prepare(
          'INSERT INTO people(id,source_id,relative_path,source_name,display_name,created_at) VALUES(?,?,?,?,?,?)',
        )
        .run(
          `${prefix}-person`,
          `${prefix}-source`,
          'Private/Creator',
          'Creator',
          'Visible creator',
          Date.now(),
        );
      fixtureDB.prepare('UPDATE albums SET person_id=? WHERE id=?').run(`${prefix}-person`, album);
    }
    for (const suffix of ['media', 'other-media']) {
      // Equal bytes at distinct paths remain distinct logical media, not two
      // objects sharing one indexed entry. Each original has its own stat epoch.
      const filename = suffix === 'media' ? 'fixture.png' : 'other.png';
      if (suffix !== 'media') {
        writeFileSync(join(source.root, filename), png, { mode: 0o400 });
      }
      const stat = statSync(join(source.root, filename), { bigint: true });
      const entry = suffix === 'media' ? `${prefix}-entry` : `${prefix}-other-entry`;
      fixtureDB
        .prepare(
          'INSERT INTO source_entries(id,source_id,relative_path,size_bytes,mtime_ns,sha256,source_config_version,updated_at) VALUES(?,?,?,?,?,?,1,?)',
        )
        .run(entry, `${prefix}-source`, filename, stat.size, stat.mtimeNs, hash, Date.now());
      const id = `${prefix}-${suffix}`;
      const revision = `${prefix}-${suffix}-revision`;
      fixtureDB
        .prepare(
          "INSERT INTO media(id,album_id,source_entry_id,kind,file_name,mime,width,height,current_revision_id,created_at,updated_at) VALUES(?,?,?,'image',?,'image/png',1,1,?,?,?)",
        )
        .run(id, album, entry, filename, revision, Date.now(), Date.now());
      for (const version of [1, 2]) {
        const revisionID = version === 2 ? revision : `${revision}-old`;
        const variantID =
          version === 2 ? `${prefix}-${suffix}-variant` : `${prefix}-${suffix}-old-variant`;
        const storage = `variants/${revisionID}/fixture_png/1/${hash}.png`;
        const target = join(directory, 'cache', ...storage.split('/'));
        mkdirSync(dirname(target), { recursive: true, mode: 0o700 });
        writeFileSync(target, png, { mode: 0o400 });
        fixtureDB
          .prepare(
            'INSERT INTO media_revisions(id,media_id,source_sha256,source_size,source_mtime_ns,version,created_at) VALUES(?,?,?,?,?,?,?)',
          )
          .run(revisionID, id, hash, stat.size, stat.mtimeNs, version, Date.now());
        fixtureDB
          .prepare(
            "INSERT INTO media_variants(id,revision_id,preset_key,preset_version,storage_path,mime,size_bytes,sha256,width,height,status,created_at) VALUES(?,?,'fixture_png',1,?,'image/png',?,?,1,1,'ready',?)",
          )
          .run(variantID, revisionID, storage, stat.size, hash, Date.now());
      }
    }
  }
  fixtureDB.exec('COMMIT');
  fixturesReady = true;
} finally {
  try {
    fixtureDB?.close();
  } finally {
    if (!fixturesReady) rmSync(directory, { recursive: true, force: true });
  }
}

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
