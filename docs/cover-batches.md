# Album-cover batch preferences

Administrators can enqueue bounded per-album cover preferences, inspect their
frozen media epochs, and let the existing embedded/standalone workers apply them.
This edits **metadata only**. It never renders pixels, creates a READY variant,
reads/writes source media or grants original downloads. The manager UI and codec
reprocessing remain separate work.

All paths below are relative to `/api/v2`. Use a current active administrator
session and the Origin/CSRF protections described in [authentication.md](authentication.md).
Responses are `private, no-store`. Ordinary accounts receive `403 ADMIN_REQUIRED`;
missing, expired, revoked or disabled sessions receive `401 UNAUTHENTICATED`.
The service rechecks authority inside its transaction, including receipt replay.

## Accept explicit preferences

```http
POST /api/v2/admin/batches/covers
Content-Type: application/json
Origin: https://gallery.example.test
X-CSRF-Token: <session-token>
Idempotency-Key: <new-unique-command-key>

{
  "targets": [
    {"album_id": "album-one", "cover_media_id": "media-one"},
    {"album_id": "album-two", "cover_media_id": ""}
  ]
}
```

- Provide 1 to 100 unique album targets, each with an explicit `cover_media_id`
  string. Duplicate albums are rejected even when their cover choices differ.
- Empty string clears the desired preference. Missing/null is **not** clear.
  Each opaque ID is at most 64 UTF-8 bytes; the entire strict JSON body is at most
  16 KiB. Unknown/case-folded fields, ambiguous JSON and invalid types are rejected.
- Album and selected media must satisfy the shared active/consistent visibility
  predicates even for administrators. A selected cover must belong to that album,
  be non-archived, and not have `metadata.status: "not_media"`. Foreign-album or
  source-inconsistent media is rejected. An unavailable mount does not invalidate
  indexed metadata; an inactive registration does.
- There is no implicit all-library selection, path selector or renderer preset.
  This operation captures current versions; it does not accept client resource
  versions, source configuration, actor identity, display names or policy fields.

Acceptance freezes album version, library/source identity, source configuration
epoch and the actual projected cover's media version/current revision. Projection
uses the [manual cover contract](album-metadata.md): eligible desired preference,
then valid previous current cover, then stable eligible media ID. On clear, the
previous current cover remains sticky when valid. Therefore clearing an override
can leave the same visible cover; it does not force another image.

The projected cover and its media version are both null when no eligible media
exists. A projected media object can have a null revision if only indexed metadata
exists. Neither case proves pixels, a READY variant or download authority.

`202` returns the safe [Job](jobs.md) DTO with `kind: "batch_covers"`, initially
queued, zero attempts/work and unknown total. `Location` identifies the current
session-protected job; `ETag` is its quoted identity/version. **Acceptance is not
completion.** Without a compatible worker, it stays queued without fake progress.

## Receipts and active coalescing

Keys use the existing 16-to-128-character ASCII grammar and 24-hour receipt window.
Reordered target pairs are canonicalized by album ID without changing pairings.
The same current administrator/key/body replays the original accepted job, even
after processing, cancellation or target/media removal. Its historical status and
ETag are not current state; GET the job to refresh. Reusing that actor/key with
another body returns `409 IDEMPOTENCY_CONFLICT`.

A new key can coalesce with the same actor's identical active frozen snapshot.
Different cover choices, album/source epochs or projected media version/revision
produce a distinct intent. Tag and cover operations never coalesce. Existing
snapshots are never expanded. Intent, targets, new-job album/source/media bindings,
job change/outbox, audits and receipt commit or roll back together.

## Frozen target pages

```http
GET /api/v2/admin/batches/<job-id>/covers?limit=30
```

Only `limit` and `cursor` are supported. Default limit is 30, maximum 100. Stable
album-ID keysets use the usual `items`, `has_more` and nullable `next_cursor`
envelope. Cursors bind current account, scope and job collection, not content
permission. Unknown/non-cover jobs return `404`.

Each target contains:

- `album_id`, `album_version`, `library_id`;
- nullable `source_id` and `source_config_version` (both null for source-less albums);
- `cover_media_id`: the explicit desired preference, including `""` for clear;
- nullable `projected_cover_media_id`, `media_version`, `revision_id`: the accepted
  projection and media epoch, not a current ETag or content capability.

Historical IDs/pins remain after media, source or original-actor removal. No root,
relative path, filename, raw metadata, password/session token, worker payload or
lease credential is returned. Current administrator authority is still required.

## Execution, results and safe retry

Both worker modes consume `batch_covers` on the same existing queue, concurrency,
lease, heartbeat and retry/cancel lifecycle as [tag batches](tag-batches.md).
Before every unresolved target, they recheck:

1. The accepted original account still exists, is active and is an administrator.
   The durable job can outlive logout or session revocation, not account
   disablement/demotion/removal. Another administrator's retry never replaces that
   original actor. No saved/forged web session participates.
2. Frozen album/library/source/configuration identity and version still match.
3. Recomputing the sticky projection yields exactly the accepted media ID/version/
   current revision, including clear-to-null. A rewrite, move, archive, changed
   eligibility or new clear fallback is rejected, never silently retargeted.
4. Current lease owner/attempt/expiry/cancel still authorize publication. Actor,
   resource and media fences are checked again before commit after journal writes.

Each genuine target change atomically commits desired/current cover, shared album
version, complete album event/outbox, audit, immutable result and job progress.
Unrelated display/source names, tags, creators, restrictions, grants and account
scopes remain unchanged. No-op records a real skipped unit without bumping album
version or publishing a false album mutation.

```http
GET /api/v2/admin/batches/<job-id>/results?limit=30
```

The existing generic result collection handles both tag and cover jobs. It has
stable result-ID pages across attempts and only `limit`/`cursor`, default 30/max100.
`applied` increments succeeded; `unchanged` and `already_applied` increment skipped;
`failed` increments failed. Resolved applied/no-op targets are permanent: later
attempts skip them without overwriting another later metadata edit or rewritten
media. A skip reports the original resolution version, not current resource state.

Cover diagnostics are fixed safe English messages selected by stable code:
`ACTOR_NOT_AUTHORIZED`, `TARGET_CHANGED`, `COVER_CHANGED` or fallback
`PROCESSING_FAILED`. The generic schema also includes tag-only `TAG_NOT_FOUND`.
No raw SQL, paths or subprocess errors are exposed.

Budgets/counters are cumulative attempt work, not unique albums. Interrupted
unfinished budget closes as cancelled; a retry adds a new bounded budget and
preserves prior resolutions/history. No result row means no committed target
outcome, not success. Queued cancellation can return an empty result page. A changed
frozen pin is not refreshed by retry; review current metadata and create a new
intent where appropriate. See [job controls](jobs.md#cancel-or-retry).

Running/source cancellation fences subsequent commits but does not undo already
committed targets. Normal native scans retain eligible desired cover IDs and
source outages retain indexed preferences. Serving any bytes still requires its
independent current authorization, revision and variant/original policy.

## Scope of verification

Isolated SQLite, strict HTTP, real compiled worker/HTTPS and killed-process recovery
checks exercise the metadata path; control browser fixtures intentionally disable
consumers and never claim applied work from queued responses. This is not real NAS,
physical-device, power-loss, restore, renderer/codec, full manager UI or GA acceptance.
