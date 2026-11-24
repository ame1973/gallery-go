import { spawn, spawnSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import {
  mkdirSync,
  mkdtempSync,
  readFileSync,
  realpathSync,
  rmSync,
  statSync,
  writeFileSync,
} from 'node:fs';
import { request as proxyRequest } from 'node:http';
import { createServer } from 'node:https';
import { tmpdir } from 'node:os';
import { basename, dirname, join, resolve } from 'node:path';
import { createInterface } from 'node:readline';
import { DatabaseSync } from 'node:sqlite';

function loopbackPort(name, fallback) {
  const raw = process.env[name];
  if (raw === undefined) return fallback;
  if (!/^[1-9][0-9]{0,4}$/.test(raw) || Number(raw) > 65535) {
    throw new Error(`Invalid test port: ${name}`);
  }
  return Number(raw);
}

const httpsPort = loopbackPort('GALLERYGO_E2E_HTTPS_PORT', 4318);
const httpPort = loopbackPort('GALLERYGO_E2E_HTTP_PORT', 4319);
if (httpsPort === httpPort) throw new Error('Test HTTP and HTTPS ports must differ');

const requestedParent = process.env.GALLERYGO_E2E_TEMP_PARENT;
let temporaryParent = tmpdir();
if (requestedParent !== undefined) {
  temporaryParent = realpathSync(requestedParent);
  if (
    dirname(temporaryParent) !== realpathSync(tmpdir()) ||
    !basename(temporaryParent).startsWith('gallerygo-isolated-e2e-')
  ) {
    throw new Error('Invalid isolated test temporary parent');
  }
}

const password = process.env.GALLERYGO_TEST_PASSWORD;
if (!password) throw new Error('The Playwright harness must supply its generated test password');
const realScan = process.env.GALLERYGO_E2E_REAL_SCAN === '1';
if (realScan && requestedParent === undefined) {
  throw new Error('Real scan tests require an isolated temporary parent');
}

// Only this process's freshly allocated test directory is removed on shutdown.
const directory = mkdtempSync(join(temporaryParent, 'gallerygo-e2e-'));
const binary = join(directory, 'gallerygo');
const config = join(directory, 'config.yaml');
const key = join(directory, 'tls.key');
const certificate = join(directory, 'tls.pem');
const projects = realScan ? [] : ['chromium', 'mobile-chromium'];
const png = Buffer.from(
  'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAAEElEQVR4nGKSq9gCCAAA//8CCgFNa0v7mgAAAABJRU5ErkJggg==',
  'base64',
);
const fixtureSources = projects.flatMap((project) =>
  [
    'media',
    'media-selection',
    'album-selection',
    'browse-ui',
    'random-home',
    'media-detail-ui',
    'outage',
    'duplicates',
    'people',
    'people-ui',
    'album-metadata',
    'ratings',
    'rating-ui',
    'views',
    'favorites',
    'favorite-collections',
    'rating-order',
    'popular-order',
    'person-metadata',
    'album-tags',
    'tag-batches',
    'cover-batches',
    'creators',
  ].map((kind) => ({
    name: `${kind}-${project}`,
    root: join(directory, 'sources', `${kind}-${project}`),
    marker: '.marker',
  })),
);
const managementRoots = realScan
  ? [{ name: 'minimum-flow', root: join(directory, 'sources', 'minimum-flow'), marker: '.marker' }]
  : projects.flatMap((project) =>
      ['managed', 'unmounted'].map((kind) => ({
        name: `${kind}-${project}`,
        root: join(directory, 'sources', `${kind}-${project}`),
        marker: '.marker',
      })),
    );
writeFileSync(
  config,
  JSON.stringify({
    listen: `127.0.0.1:${httpPort}`,
    base_url: `https://127.0.0.1:${httpsPort}`,
    database: join(directory, 'data/gallerygo.db'),
    cache_root: join(directory, 'cache'),
    web_root: resolve('web/dist'),
    sources: [...fixtureSources, ...managementRoots],
    // Synthetic control fixtures must not consume jobs. The separate real-scan
    // mode uses an empty database and a private embedded worker.
    worker: realScan
      ? {
          embedded: true,
          concurrency: 1,
          poll: '100ms',
          max_attempts: 2,
          retry_base: '100ms',
          retry_max: '200ms',
        }
      : { embedded: false, required: false },
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
  for (const root of managementRoots.filter(
    (root) => realScan || root.name.startsWith('managed-'),
  )) {
    mkdirSync(root.root, { recursive: true, mode: 0o700 });
    writeFileSync(join(root.root, root.marker), 'isolated management marker', { mode: 0o400 });
    if (realScan) {
      mkdirSync(join(root.root, 'Album'), { mode: 0o700 });
      writeFileSync(join(root.root, 'Album', 'photo.png'), png, { mode: 0o400 });
    }
  }
  fixtureDB = new DatabaseSync(join(directory, 'data/gallerygo.db'));
  fixtureDB.exec('PRAGMA foreign_keys=ON; PRAGMA busy_timeout=5000; BEGIN IMMEDIATE');
  for (const project of projects) {
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
    // Immutable, project-namespaced history is test setup only. The worker is
    // disabled; these records do not claim actual scanning or lease execution.
    const historyTime = Date.parse('2026-11-10T01:02:03.000Z');
    const historyPrefix = `history-${project}`;
    const insertHistoryJob = fixtureDB.prepare(
      "INSERT INTO jobs(id,kind,resource_key,dedupe_token,payload_json,status,max_attempts,next_run_at,total_known,created_at,updated_at) VALUES(?,'scan',?,?,?,'dead',3,?,1,?,?)",
    );
    for (let index = 0; index < 33; index += 1) {
      const id = `${historyPrefix}-${String(index).padStart(2, '0')}`;
      insertHistoryJob.run(
        id,
        `source:${historyPrefix}`,
        `scan:${id}`,
        JSON.stringify({ private_path: directory }),
        historyTime,
        historyTime,
        historyTime,
      );
    }
    const historyJob = `${historyPrefix}-zz.with.dot`;
    fixtureDB
      .prepare(
        "INSERT INTO jobs(id,kind,resource_key,dedupe_token,payload_json,status,priority,attempts,max_attempts,next_run_at,total,done,succeeded,failed,skipped,cancelled,total_known,error_code,last_error,created_at,updated_at,started_at,finished_at) VALUES(?,'scan',?,?,?,'dead',2,31,31,?,93,93,31,31,31,0,1,'INVALID_MEDIA','The input media could not be validated.',?,?,?,?)",
      )
      .run(
        historyJob,
        `source:${historyPrefix}`,
        `scan:${historyJob}`,
        JSON.stringify({ private_path: directory }),
        historyTime,
        historyTime,
        historyTime + 120_000,
        historyTime,
        historyTime + 120_000,
      );
    const insertAttempt = fixtureDB.prepare(
      "INSERT INTO job_attempts(job_id,attempt,started_at,finished_at,status,error_code,message,base_counts_json,counts_json) VALUES(?,?,?,?,'failed','INVALID_MEDIA','The input media could not be validated.',?,?)",
    );
    for (let attempt = 1; attempt <= 31; attempt += 1) {
      const previous = attempt - 1;
      insertAttempt.run(
        historyJob,
        attempt,
        historyTime + attempt * 1_000,
        historyTime + attempt * 1_000 + 500,
        JSON.stringify({
          total: previous * 3,
          done: previous * 3,
          succeeded: previous,
          failed: previous,
          skipped: previous,
          cancelled: 0,
        }),
        JSON.stringify({ total: 3, done: 3, succeeded: 1, failed: 1, skipped: 1, cancelled: 0 }),
      );
    }
    const insertIssue = fixtureDB.prepare(
      "INSERT INTO job_issues(id,job_id,attempt,source_id,source_config_version,generation_id,relative_path,error_code,created_at) VALUES(?,?,?,?,5,?,?,'INVALID_MEDIA',?)",
    );
    for (let index = 0; index < 33; index += 1) {
      const number = String(index).padStart(2, '0');
      insertIssue.run(
        `${historyPrefix}-issue-${number}`,
        historyJob,
        (index % 31) + 1,
        `${historyPrefix}-source`,
        index % 2 ? '' : `${historyPrefix}-generation`,
        index === 32 ? '../outside-private-path' : `Album/entry-${number}.png`,
        historyTime + index * 1_000,
      );
    }
    const runningJob = `${historyPrefix}-running`;
    fixtureDB
      .prepare(
        "INSERT INTO jobs(id,kind,resource_key,dedupe_token,payload_json,status,cancel_requested,attempts,max_attempts,next_run_at,total,done,succeeded,failed,skipped,cancelled,total_known,created_at,updated_at,started_at) VALUES(?,'scan',?,?,?,'running',1,1,3,?,10,4,1,1,1,1,0,?,?,?)",
      )
      .run(
        runningJob,
        `source:${historyPrefix}`,
        `scan:${runningJob}`,
        JSON.stringify({ private_path: directory }),
        historyTime,
        historyTime,
        historyTime,
        historyTime,
      );
    fixtureDB
      .prepare(
        "INSERT INTO job_attempts(job_id,attempt,started_at,status,counts_json) VALUES(?,1,?,'running',?)",
      )
      .run(
        runningJob,
        historyTime,
        JSON.stringify({ total: 10, done: 4, succeeded: 1, failed: 1, skipped: 1, cancelled: 1 }),
      );
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
  // Synthetic ready records are test setup only; the real-scan mode creates none.
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
    if (prefix.startsWith('rating-order-') || prefix.startsWith('popular-order-')) {
      // Empty selection cards only, not native mapping or renderer/READY proof.
      // The primary card above owns actual isolated read-only PNG media.
      for (const [suffix, restricted] of [
        ['other-album', 0],
        ['unrated-album', 0],
        ['private-album', 1],
      ]) {
        fixtureDB
          .prepare(
            'INSERT INTO albums(id,library_id,source_id,source_name,access_restricted,created_at,updated_at) VALUES(?,?,?,?,?,?,?)',
          )
          .run(
            `${prefix}-${suffix}`,
            library,
            `${prefix}-source`,
            'Empty rank card',
            restricted,
            Date.now(),
            Date.now(),
          );
      }
    }
    if (prefix.startsWith('creators-')) {
      for (const name of ['mapped', 'manual']) {
        fixtureDB
          .prepare(
            'INSERT INTO people(id,source_id,relative_path,source_name,created_at) VALUES(?,?,?,?,?)',
          )
          .run(
            `${prefix}-${name}`,
            `${prefix}-source`,
            `Private/${name}`,
            `${name} creator`,
            Date.now(),
          );
      }
      fixtureDB
        .prepare('UPDATE albums SET person_id=?,mapped_person_id=? WHERE id=?')
        .run(`${prefix}-mapped`, `${prefix}-mapped`, album);
    }
    if (
      prefix.startsWith('people-') ||
      prefix.startsWith('person-metadata-') ||
      prefix.startsWith('album-selection-')
    ) {
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
    if (prefix.startsWith('media-detail-ui-')) {
      // An indexed original without a READY variant exercises the pre-rendering flow.
      const unprocessed = join(source.root, 'unprocessed.png');
      writeFileSync(unprocessed, png, { mode: 0o400 });
      const originalStat = statSync(unprocessed, { bigint: true });
      const originalEntry = `${prefix}-unprocessed-entry`;
      const originalMedia = `${prefix}-unprocessed-media`;
      const originalRevision = `${prefix}-unprocessed-revision`;
      fixtureDB
        .prepare(
          'INSERT INTO source_entries(id,source_id,relative_path,size_bytes,mtime_ns,sha256,source_config_version,updated_at) VALUES(?,?,?,?,?,?,1,?)',
        )
        .run(
          originalEntry,
          `${prefix}-source`,
          'unprocessed.png',
          originalStat.size,
          originalStat.mtimeNs,
          hash,
          Date.now(),
        );
      fixtureDB
        .prepare(
          "INSERT INTO media(id,album_id,source_entry_id,kind,file_name,mime,width,height,current_revision_id,created_at,updated_at) VALUES(?,?,?,'image','unprocessed.png','image/png',1,1,?,?,?)",
        )
        .run(originalMedia, album, originalEntry, originalRevision, Date.now(), Date.now());
      fixtureDB
        .prepare(
          'INSERT INTO media_revisions(id,media_id,source_sha256,source_size,source_mtime_ns,version,created_at) VALUES(?,?,?,?,?,1,?)',
        )
        .run(
          originalRevision,
          originalMedia,
          hash,
          originalStat.size,
          originalStat.mtimeNs,
          Date.now(),
        );
      // The shared grant is never mutated by tests; each test changes its own user.
      fixtureDB
        .prepare('INSERT INTO permission_groups(id,name,created_at) VALUES(?,?,?)')
        .run(`${prefix}-group`, `Media detail access ${prefix}`, Date.now());
      fixtureDB
        .prepare('INSERT INTO library_access(library_id,group_id) VALUES(?,?)')
        .run(library, `${prefix}-group`);
      // Metadata for the known static test PNG; this does not exercise the scanner.
      fixtureDB
        .prepare('UPDATE media SET metadata_json=? WHERE id=?')
        .run(
          JSON.stringify({ codec: 'png', animated: false, frame_count: 1, loop_count: 1 }),
          `${prefix}-media`,
        );
    }
    if (prefix.startsWith('random-home-')) {
      // The grant is immutable; each test changes only its own user membership.
      fixtureDB
        .prepare('INSERT INTO permission_groups(id,name,created_at) VALUES(?,?,?)')
        .run(`${prefix}-group`, `Random home access ${prefix}`, Date.now());
      fixtureDB
        .prepare('INSERT INTO library_access(library_id,group_id) VALUES(?,?)')
        .run(library, `${prefix}-group`);
      fixtureDB
        .prepare('UPDATE albums SET cover_media_id=? WHERE id=?')
        .run(`${prefix}-media`, album);
      for (let index = 0; index < 33; index += 1) {
        fixtureDB
          .prepare(
            'INSERT INTO albums(id,library_id,source_id,source_name,created_at,updated_at) VALUES(?,?,?,?,?,?)',
          )
          .run(
            `${prefix}-extra-${String(index).padStart(2, '0')}`,
            library,
            `${prefix}-source`,
            `Random album ${index}`,
            Date.now(),
            Date.now(),
          );
      }
      fixtureDB
        .prepare(
          'INSERT INTO albums(id,library_id,source_id,source_name,access_restricted,created_at,updated_at) VALUES(?,?,?,?,1,?,?)',
        )
        .run(
          `${prefix}-hidden`,
          library,
          `${prefix}-source`,
          'Hidden random album',
          Date.now(),
          Date.now(),
        );
    }
    if (prefix.startsWith('browse-ui-')) {
      // Pagination and permission fixtures only; no production renderer is enabled.
      fixtureDB
        .prepare('UPDATE albums SET cover_media_id=? WHERE id=?')
        .run(`${prefix}-media`, album);
      for (let index = 0; index < 32; index += 1) {
        fixtureDB
          .prepare(
            'INSERT INTO albums(id,library_id,source_id,source_name,created_at,updated_at) VALUES(?,?,?,?,?,?)',
          )
          .run(
            `${prefix}-extra-${String(index).padStart(2, '0')}`,
            library,
            `${prefix}-source`,
            `Paged album ${index}`,
            Date.now(),
            Date.now(),
          );
      }
      fixtureDB
        .prepare(
          'INSERT INTO albums(id,library_id,source_id,source_name,access_restricted,created_at,updated_at) VALUES(?,?,?,?,1,?,?)',
        )
        .run(
          `${prefix}-hidden`,
          library,
          `${prefix}-source`,
          'Hidden private album',
          Date.now(),
          Date.now(),
        );
    }
    if (prefix.startsWith('people-ui-')) {
      // Distinct people and empty albums exercise UI keysets and permissions only.
      // The primary person owns real test PNGs through the primary album above.
      fixtureDB
        .prepare('UPDATE albums SET cover_media_id=? WHERE id=?')
        .run(`${prefix}-media`, album);
      for (let index = 0; index < 32; index += 1) {
        const number = String(index).padStart(2, '0');
        fixtureDB
          .prepare(
            'INSERT INTO albums(id,library_id,source_id,person_id,source_name,created_at,updated_at) VALUES(?,?,?,?,?,?,?)',
          )
          .run(
            `${prefix}-album-extra-${number}`,
            library,
            `${prefix}-source`,
            `${prefix}-person`,
            `Creator album ${number}`,
            Date.now(),
            Date.now(),
          );
        const id = `${prefix}-zz-person-${number}`;
        fixtureDB
          .prepare(
            'INSERT INTO people(id,source_id,relative_path,source_name,created_at) VALUES(?,?,?,?,?)',
          )
          .run(
            id,
            `${prefix}-source`,
            `Private/Person-${number}`,
            index < 2 ? 'Same display name' : `Person ${number}`,
            Date.now(),
          );
        fixtureDB
          .prepare(
            'INSERT INTO albums(id,library_id,source_id,person_id,source_name,created_at,updated_at) VALUES(?,?,?,?,?,?,?)',
          )
          .run(
            `${prefix}-zz-album-${number}`,
            library,
            `${prefix}-source`,
            id,
            `Solo album ${number}`,
            Date.now(),
            Date.now(),
          );
      }
      fixtureDB
        .prepare(
          'INSERT INTO people(id,source_id,relative_path,source_name,created_at) VALUES(?,?,?,?,?)',
        )
        .run(
          `${prefix}-hidden-person`,
          `${prefix}-source`,
          'Private/Hidden',
          'Hidden private person',
          Date.now(),
        );
      fixtureDB
        .prepare(
          'INSERT INTO albums(id,library_id,source_id,person_id,source_name,access_restricted,created_at,updated_at) VALUES(?,?,?,?,?,1,?,?)',
        )
        .run(
          `${prefix}-hidden-album`,
          library,
          `${prefix}-source`,
          `${prefix}-hidden-person`,
          'Hidden private album',
          Date.now(),
          Date.now(),
        );
    }
    if (prefix.startsWith('album-selection-')) {
      // Selection-only empty/restricted cards and deterministic time ties, not
      // native mapping/index-time or renderer proof. The primary card owns real PNGs.
      const indexed = Date.parse('2026-11-07T00:00:00.123Z');
      fixtureDB
        .prepare('UPDATE albums SET mapped_person_id=?,cover_media_id=?,created_at=? WHERE id=?')
        .run(`${prefix}-person`, `${prefix}-media`, indexed, album);
      for (const [suffix, restricted] of [
        ['other-album', 0],
        ['private-album', 1],
      ]) {
        fixtureDB
          .prepare(
            'INSERT INTO albums(id,library_id,source_id,person_id,mapped_person_id,source_name,access_restricted,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?)',
          )
          .run(
            `${prefix}-${suffix}`,
            library,
            `${prefix}-source`,
            `${prefix}-person`,
            `${prefix}-person`,
            'Empty selection card',
            restricted,
            indexed + restricted,
            Date.now(),
          );
      }
    }
    if (prefix.startsWith('media-selection-')) {
      // Indexed selection setup only: processing rows have no source-entry,
      // revision, codec or READY bytes. This does not claim video/EXIF ingestion.
      const captured = Date.parse('2026-11-06T01:02:03.123Z');
      for (const [suffix, kind, offset] of [
        ['a', 'image', 0],
        ['b', 'video', 0],
        ['c', 'image', 1],
      ]) {
        fixtureDB
          .prepare(
            'INSERT INTO media(id,album_id,kind,file_name,capture_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?)',
          )
          .run(
            `${prefix}-${suffix}`,
            album,
            kind,
            kind === 'video' ? 'Processing.mp4' : 'Processing.png',
            captured + offset,
            Date.now(),
            Date.now(),
          );
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
        port: httpPort,
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
const child = spawn(binary, ['serve', '--config', config], {
  stdio: ['ignore', 'inherit', 'pipe'],
});
let stopping = false;
let proxyFailed = false;
let proxyStarted = false;
// Do not proxy to an unrelated process if the Go listener's port is occupied.
// The compiled server logs this line only after its own listener has bound.
createInterface({ input: child.stderr }).on('line', (line) => {
  console.error(line);
  let event;
  try {
    event = JSON.parse(line);
  } catch {
    return;
  }
  if (
    !stopping &&
    !proxyStarted &&
    event.msg === 'HTTP server listening' &&
    event.address === `127.0.0.1:${httpPort}`
  ) {
    proxyStarted = true;
    proxy.listen(httpsPort, '127.0.0.1');
  }
});
proxy.on('listening', () => {
  process.stdout.write(`GALLERYGO_E2E_READY https://127.0.0.1:${httpsPort}\n`);
});
proxy.on('error', (error) => {
  proxyFailed = true;
  console.error(error);
  child.kill('SIGTERM');
});
for (const signal of ['SIGINT', 'SIGTERM']) {
  process.on(signal, () => {
    stopping = true;
    proxy.close();
    child.kill(signal);
  });
}
child.on('error', (error) => {
  console.error(error);
  proxy.closeAllConnections();
  proxy.close();
  rmSync(directory, { recursive: true, force: true });
  process.exit(1);
});
child.on('exit', (code) => {
  proxy.closeAllConnections();
  proxy.close();
  rmSync(directory, { recursive: true, force: true });
  process.exit(proxyFailed ? 1 : stopping ? 0 : (code ?? 1));
});
