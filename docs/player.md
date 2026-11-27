# Player foundations

GalleryGo does not yet provide a player screen or playback controls. The internal
`web/src/features/player/order.ts` module is a building block for a **fully acquired,
fixed** sequence of media IDs; it is not an authorization check or a playback API.
No route, video player, automatic timer, gestures, progress persistence, or offline
support is enabled by these modules.

The caller must obtain a valid account- and scope-bound media snapshot, retain each
item's revision and availability separately, and reauthorize or rebuild it when
necessary. The ordering module neither fetches missing pages nor validates media
bytes. In particular, an ID in this sequence does not grant access, and unavailable
or unverified offline items must not be treated as playable just because they appear
in the sequence. Incremental queue acquisition and queue-revision handling remain
work for the full player.

## Current behavior

- Empty sequences have no current item. The initial source index is clamped to the
  existing sequence; duplicate or empty IDs and invalid settings are rejected.
- Ordered playback follows the supplied order or its reverse. Shuffle uses a seeded
  Fisher–Yates permutation and starts with the selected item. A seed is for
  reproducibility, not a security credential.
- A shuffle round visits each supplied ID once. With repeat-all, a new round avoids
  repeating the preceding item at the boundary when more than one item exists. For
  two items, avoiding that repeat necessarily keeps the same order in later rounds.
- Previous and next follow the same deterministic order. Shuffle history is
  reconstructed from the seed and round rather than retained without bound. Previous
  at the beginning does not invent earlier history. Ordered playback can move back
  through earlier items in the supplied sequence.
- Repeat-one applies to an automatic advance; an explicit next or previous still
  navigates. Without repeat-all, advancing at the end retains the last item and
  marks the order ended.

The supplied sequence and each permutation are held in memory. Normal steps are
constant-time, while creating or crossing a round takes work proportional to the
sequence size. Seed, direction, shuffle and repeat settings are fixed for this order;
changing settings or the sequence requires a later player integration to define how
to reconcile the current item and history. The full player must still implement
incremental paging, revision/availability changes, integrated scheduling, video events,
visibility handling, accessibility, data-aware preloading, and device/browser acceptance.

## Still-image clock foundation

`web/src/features/player/slide-clock.ts` models one still or animated image without
creating timers or touching the DOM. Intervals are 500–30,000 milliseconds. If the
caller requests respect for animation and supplies a known positive cycle duration,
the image duration is the longer of that cycle and the configured interval; an
unknown or zero cycle falls back to the interval.

The caller supplies nondecreasing monotonic milliseconds from one time origin, such
as `performance.now()`. Pause preserves elapsed time; resuming establishes a new
deadline without charging the paused interval. A delayed tick ends at most this one
clock. If a pause arrives after its deadline, the clock stays paused at the duration
and ends on the next tick after resumption. Seek clamps to the current duration; a
seek backward from an ended clock leaves it paused. Reset creates a new ready clock.

The integration must pause on background visibility or buffering, fence stale callbacks
against the current account, media and revision, and reinitialize timing after reload
or a different time origin. This helper does not do any of those tasks itself and does
not model video: video play/pause/ended events and clipped playback require separate
handling and real browser tests. It does not authorize or cache media.
