# Player ordering foundation

GalleryGo does not yet provide a player screen or playback controls. The internal
`web/src/features/player/order.ts` module is a building block for a **fully acquired,
fixed** sequence of media IDs; it is not an authorization check or a playback API.
No route, video player, timer, gestures, progress persistence, or offline support is
enabled by this module.

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
incremental paging, revision/availability changes, timing, video events, visibility,
accessibility, data-aware preloading, and the device/browser acceptance in the product
specification.
