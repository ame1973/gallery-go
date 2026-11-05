# Indexed exact duplicates

The current administrative API groups **indexed byte signatures**, not perceptual
similarity. It compares the full SHA-256 and byte length recorded for each media's
current owned revision. A historical revision, an invalid fingerprint, a singleton,
an archived item, an inactive library/source, or an inconsistent source relationship
cannot contribute to a group. No source hash, filesystem path or marker is returned.

This is a read-only management API. The group/member screens and perceptual-similarity
workflow remain under development. There is no merge or delete action, automatic
original removal, original-copy fallback, or new download permission.

## Authentication and selection

Both endpoints below are relative to `/api/v2` and require a current administrator
HTTPS session. Anonymous/expired/disabled/revoked sessions receive 401; ordinary users
receive 403 before resource existence is revealed. Every service query rechecks the
session/account in the same transaction and uses the shared SQL visibility rules.

- `GET /admin/duplicates/exact` lists groups by opaque representative revision ID.
- `GET /admin/duplicates/exact/{group_id}/media` lists members by stable media ID,
  using the same safe [media DTO](media.md). Metadata does not grant original bytes.

Optional `library_id` and `source_id` filters select the indexed corpus **before**
grouping or counting. When both are provided they intersect. Each accepts one
nonempty opaque ID of at most 64 UTF-8 bytes; unknown, repeated, empty, unsafe or
unsupported filter parameters are rejected. A valid selection with no duplicates
returns `{ "items": [], "next_cursor": null, "has_more": false }`.

For example:

```http
GET /api/v2/admin/duplicates/exact?source_id={source_id}&limit=30
GET /api/v2/admin/duplicates/exact/{group_id}/media?source_id={source_id}&limit=30
```

Keep the same filters when opening a group or following its cursor. A group row is:

```json
{
  "id": "opaque-representative-revision-id",
  "source_size": 1024,
  "media_count": 2
}
```

`source_size` is the indexed original byte length, not a generated variant's size.
`media_count` counts current visible logical media in this selected corpus, not
historical revisions, all filesystem paths or a global library statistic. Equal
bytes at distinct indexed paths may legitimately belong to separate media/records.

## Pagination, rewrites and safety

Both lists use default 30/max 100 ID keysets with
`{items,next_cursor,has_more}`. Cursors bind the current account, authorization scope,
collection/group and canonical filters; they cannot be reused for a different group,
user, filter or endpoint. Group cursors are applied after aggregation so later pages
do not accidentally drop a group's earlier members or change its count.

Group IDs are immutable representative **revision** identifiers, not source hashes
or mutable media IDs. Rewriting a file can preserve the media ID while creating a
new revision and changing group membership. If the representative is no longer a
current member, another representative becomes necessary, or fewer than two members
remain, the old member endpoint returns 404. It never retargets an old cursor to a
different content signature. Refresh the group list rather than treating an old
ID as a permanent content/download capability.

These are live database pages, not immutable offline snapshots. Ingestion, source
activation, permissions, rewrites or archival can change the set between requests.
Each individual request has consistent transactional membership and counts.

The API performs no source filesystem I/O. An unavailable mount does not erase its
last committed indexed fingerprints, and duplicate metadata does not establish that
an original is still reachable or unchanged since ingestion. Do not infer semantic
identity, full decoder validity, current filesystem equality or safe deletion from
a group. Strong indexed hashes and equal lengths are useful evidence, not authority
to delete files. Original GET/HEAD/Range retains the extra `download_original` policy,
including for administrators; private variants and all existing access rules remain
unchanged. Responses are `private, no-store`, never a global service-worker cache.
