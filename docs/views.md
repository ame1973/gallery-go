# Album views

All paths use `/api/v2`. A view is an explicit, accepted online account event; merely
reading album metadata, playing media, issuing HEAD/Range requests or downloading
bytes does not automatically record one. Every request requires a live session and
current shared album visibility. Hidden and missing albums return the same `404`.
No view operation reads source files or grants original or offline access.

## Read and record

`GET /albums/{album_id}/views` returns the current account's private count and
last server UTC-millisecond instant, with an account-and-album-bound strong ETag:

```json
{
  "album_id": "album-id",
  "view_count": 0,
  "last_viewed_at": null,
  "version": 1
}
```

A never-viewed album has this zero/null/1 state. A first recorded view sets count 1
and version 2. Times never move backward if the server clock regresses. A different
account's views do not change your personal validator.

Send `POST /albums/{album_id}/views` with **no body**, the configured HTTPS `Origin`,
current CSRF token and a new `Idempotency-Key`. No account ID or client timestamp is
accepted. Each new accepted key records one view. The response is the current
personal state and ETag. An identical retry with the same key within 24 hours
returns its historical result without adding another view; GET refreshes current
state. A retry still requires current album visibility and a live account/session.
Reusing a key for another command or album returns `409 IDEMPOTENCY_CONFLICT`.

## Shared totals and deletion

Authorized album cards and detail expose `view_count`, the shared sum of explicit
views retained for current accounts. A real view atomically increments the personal
count, shared total and both revisions, and co-commits complete private/shared
change events, their outboxes and the actor receipt. The account's personal change
event is private; the shared album projection contains only the total. Routine views
do not add per-view entries to the administrator audit.
Account deletion removes that account's personal counts and updates every affected
shared total and event, including albums the deleted account could no longer see.
It does not alter original media or another account's personal view state. When
an account has both a vote and views on one album, deletion publishes one complete
final shared projection for that album. The current personal counter and command
receipts are removed, but prior private change/outbox events are not purged by
account deletion and can remain in the database or backups. Administrators and
backup operators with direct storage access must protect that history; absence from
the administrator audit API is not a promise of erasure from stored data.

Deletion is an atomic SQLite write, not a resumable background job. On a slow or
busy database, or with sufficiently large account history, it can exceed the 10-second
metadata deadline; the transaction rolls back rather than reporting a partial delete.
A retry is not guaranteed to finish if the workload exceeds available capacity.

Totals are not anonymous lifetime counters and can decrease after account deletion.
A view count is not proof that media bytes were fully displayed or downloaded.
Recent browsing, popularity ordering, download statistics, offline operation replay
and their UI remain separate required features; this API does not implement them.
