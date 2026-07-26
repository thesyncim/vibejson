# vNext layout laboratory

This package tests the highest-risk parts of the next durable Store format
without changing a production read or write path.

The design is not an LSM and has no reader-visible journal, tombstone layer,
forwarding page, or delta overlay. One immutable root continues to name one
canonical state.

## Candidate layout

- A keyed fingerprint directory stores no key bytes. Every candidate is
  verified against the complete key in its record block.
- Fingerprint leaves store 316 entries in one 4 KiB frame. An entry is an
  implied eight-bit hash prefix, a 56-bit hash suffix, and a packed 26-bit
  stable block ID plus six-bit slot.
- A stable block map replaces the current chunk directory; it must never become
  a third lookup layer.
- Raw blocks retain at most 64 stable slots. Each slot uses a direct four-byte
  `(row start, key end)` record, and JSON ends at the next live slot.
- Selective packed blocks store one shared JSON prefix and suffix plus one
  contiguous independently decodable middle per row. They retain the same
  four-byte stable-slot directory as raw blocks. A block stays raw unless the
  packed candidate saves at least one 4 KiB quantum and 12.5% of the raw
  physical span. Packed promotion is disabled unless the integrated reader has
  separately cleared the read-latency gate for the production build and CPU.
- Data spans may be any 4 KiB multiple from 4 through 64 KiB. Metadata remains
  fixed at one quantum until the cache and allocator support exact spans.

## Invariants

- Fingerprints route only; exact key bytes decide identity.
- Stable block IDs never encode physical offsets.
- An existing-key replacement does not rewrite the fingerprint directory unless
  its stable location changes.
- Pages and blocks are immutable after publication.
- No allocator may reuse an extent reachable from an active snapshot or either
  validated recovery root.
- A block map replaces the current chunk radix and must match or improve its
  depth, cache footprint, and zone-summary behavior.

## Gates

- Warm raw-block exact lookup: no slower than the current document page by more
  than 2%.
- Warm fingerprint leaf probe: no slower than the existing exact-rechecked
  fingerprint leaf.
- Packed `AppendRaw`: within 2% of raw lookup plus copy, with no shared decode
  buffer and no fragmented reconstruction program.
- Zero allocations for encode, open, hit, miss, and exact collision handling.
- Raw physical bytes at most 1.20 times key plus JSON bytes on the representative
  small-document corpus.
- Packed physical bytes at most 0.75 times key plus JSON bytes on repetitive
  blocks.
- Incompressible or weakly compressible blocks have exactly one canonical raw
  representation; readers never probe a packed alternative.
- Fingerprint leaf at most 24 physical bytes per key at 70% occupancy.
- No extra point-read page or cache miss after integration.
- A small isolated replacement normally rewrites one 4–8 KiB data extent.
- A 90% delete trace returns to within 10% of a fresh rebuild after explicitly
  bounded local canonical-maintenance steps.

The trace uses a bounded recent-block placement window and reports its probes.
Its write-byte counters cover data extents only; fingerprint-directory,
block-map, allocator, retirement, and publication-root traffic must be measured
by the integrated Store before promotion. Resident codec benchmarks likewise
qualify codecs, not the complete point-read path.

The laboratory is intentionally not wired into collection open, recovery, or
the page cache. Promotion requires full span-aware cache allocation, crash
injection across every 4 KiB-multiple extent, and an integrated point-read
comparison against the current key-tree and chunk-radix path.

The encoder deterministically emits maximal common JSON edges. The decoder
accepts any structurally valid edge decomposition; canonicality is therefore a
writer/publication invariant, while the checksum and structural validation are
the corruption boundary.
