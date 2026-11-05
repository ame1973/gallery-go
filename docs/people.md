# Indexed people and creators

GalleryGo builds Person identities from each source's configurable path mapping.
A person is bound to its source and full mapped relative path, not to a globally
unique display label. Equal names can legitimately return different opaque IDs;
a proven whole-folder move can retain the identity and display override.

The current API is a read-only indexed view. It supports person listing/detail and
same-source album pages. Person management/association UI, richer filters/sorting,
search and offline snapshots remain separate work; these endpoints support only
`limit` and `cursor` on list pages, not an ignored search/filter parameter.

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
