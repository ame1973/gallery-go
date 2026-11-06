# Indexed people and creators

GalleryGo builds Person identities from each source's configurable path mapping.
A person is bound to its source and full mapped relative path, not to a globally
unique display label. Equal names can legitimately return different opaque IDs;
a proven whole-folder move can retain the identity and display override.

The indexed read API supports person listing/detail and same-source album pages.
Separate administrative commands below manage display-name overrides without
changing source identities. Versioned [album creator classification](album-creators.md)
separately supports mapped fallback, same-source manual selection and explicit none.
Full management UI, richer filters/sorting, search and offline snapshots remain
separate work; read pages support only
`limit` and `cursor`, not an ignored search/filter parameter.

## Endpoints and fields

All routes below are relative to `/api/v2` and require a current HTTPS session:

- `GET /people` lists visible people in stable person-ID order.
- `GET /people/{person_id}` returns one current visible person.
- `GET /people/{person_id}/albums` lists authorized same-source albums in stable
  album-ID order using the existing [album DTO](access.md).

A person contains:

```json
{
  "id": "opaque-person-id",
  "name": "Display name",
  "source_id": "opaque-source-id",
  "album_count": 1,
  "cover_media_id": null,
  "version": 1
}
```

`name` uses a nonempty display override or the indexed folder label. The complete
source-relative path, absolute root, mount marker and runtime/configuration pins
are never returned. `source_id` is an opaque identifier, not filesystem access.
`version` is the indexed person's metadata version; visibility-dependent counts
and covers can change without a person-name update and are not an offline snapshot.

## Visibility, counts and covers

Every query rechecks the session/account in its database transaction and applies
the shared [library and additive album policy](access.md). A person is visible only
when at least one same-source album is visible. A cross-source album link cannot
reveal another source's creator name or contribute to its counts/cover. Inactive
libraries/sources and inconsistent source ownership fail closed, including for
administrators. People without a concrete mapped path or with no visible album are hidden
by this indexed read surface, not advertised as public/unclassified creators.

- `album_count` includes only those same-source, currently authorized albums.
- `cover_media_id` can reference only authorized album-owned, non-archived media
  with a consistent indexed source entry. Valid album cover choices are preferred;
  otherwise a stable media-ID fallback is used. A hidden album's media, foreign
  album cover or mismatched source entry cannot become a public representative.
- A visible empty album still contributes to the count. If no visible media exists,
  the cover is explicitly null rather than an inferred path or original fallback.
- A cover ID does not prove a READY variant, grant original download, or provide an
  offline capability. The normal protected variant/original handlers still apply;
  originals require the additional account policy even for administrators.

An unauthenticated/expired/disabled/revoked session receives 401. An unknown, hidden,
empty or unclassified person receives 404 on detail/album routes. A caller with no
visible people receives an explicit empty page, never a private creator name with
a zero count. Removing the last album grant or its permission group cannot clear
the album's persistent restriction latch or broaden a person aggregate.

## Pagination and source availability

Both lists use default 30/max 100 keysets with
`{items,next_cursor,has_more}`. Cursors bind the current account, authorization scope
and collection; a person-album cursor cannot be reused for another person or the
person list. Membership/policy changes invalidate old scope-bound cursors.

These are live indexed metadata pages, not fixed offline snapshots. Reads perform
no source filesystem I/O and never create a root/marker. Mount loss preserves
committed identities/names/counts; it does not prove current source reachability or
change original-download availability. Responses are `private, no-store`, never a
public mount or globally cached private API response.

## Administrative display-name overrides

`GET /admin/people/{person_id}` requires a current administrator and the same
source-bound visibility rule. It returns a strong ETag such as `"person:{id}:7"`
and only `id`, opaque `source_id`, indexed `source_name`, nullable `display_name`
and `version`. The indexed source label is not a full path and is not editable here.
Ordinary users receive 403 and anonymous/expired/disabled/revoked sessions receive
401 before resource existence is revealed. Empty/unmapped/hidden or inconsistent
people remain 404, including for administrators. Mount loss alone does not hide
committed metadata or prevent editing it.

`PATCH /admin/people/{person_id}` requires the exact resource `If-Match`, an explicit
unique `Idempotency-Key` of 16 to 128 ASCII letters/digits/underscores/hyphens, and the
normal same-origin `Origin` plus session-bound `X-CSRF-Token`. For example:

```json
{
  "display_name": "A creator display override"
}
```

`display_name` is the only accepted field and must be provided. An explicit empty
string clears the override; missing or **null is not an implicit clear**. Nonempty
values use the existing display-label policy: trim whitespace, then 1 to 100 Unicode
characters/at most 400 UTF-8 bytes without control characters. This is not NFKC/full
case-fold search normalization. Source names/paths/IDs, album associations, covers
and permissions cannot be mass-assigned through this command.

Successful changes atomically commit the label/version, complete person change,
outbox, audit and actor-bound result receipt. Identical preferences preserve the
version and business events but still save their receipt. New stale commands receive
409; missing strong preconditions receive 428 and invalid ones receive 400. A label
edit does not rotate account scopes, clear album restrictions, reassign source/path
or album identities, or grant original/variant/offline access. Source files are never
read or written by these endpoints.

Retry the same actor/key/canonical body/original precondition to retrieve the
historical result and ETag, even after later edits or scans. Reusing the key for
another target/body/precondition conflicts. Receipt replay still requires a current
administrator and is never an authorization capability. GET refreshes current state.
Healthy scans preserve manual display overrides; provable whole-folder moves may
update the indexed label/version while retaining logical identities and the override.
Clearing afterward uses the current indexed label, not an old folder name. Album
[creator associations](album-creators.md) and [tags](tags.md) use separate versioned
commands; full search and management UI remain separate work.
