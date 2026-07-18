# Structural decoding and route selection

There are three related representations with different construction costs:

- the raw typed cursor validates and decodes in one forward pass;
- the structural typed route performs stage 1 once, then advances a compact
  position tape while decoding; and
- `Index` validates the complete document and writes persistent navigation
  entries for repeated or out-of-order access.

An index is fast after construction, but construction is not free. It scans the
whole input, records structure, and completes container metadata. A one-shot
typed decode can finish during its first pass, stop without materializing a
navigation structure, and reuse the caller's destination. That is why every
internal decode does not first build an `Index`.

## Typed structural route

The typed structural route is considered only for eligible plans and inputs of
at least 4 KiB. The pinned implementation also requires a line feed in the
first 2 KiB. A raw line feed cannot occur inside valid JSON
text, so it is a cheap signal for indentation dense enough to repay stage-1
setup. Compact or small documents stay on the cursor route.

The tape is forward-only. It contains delimiter and string positions but omits
colons. The executor must validate the colon gap whenever it cannot prove a
packed expected field. Escaped or shuffled keys fall back to the raw key parser
and then realign the tape at the value start. A malformed tape or unsupported
shape fails closed or returns to the generic cursor; it never relaxes grammar.

The structural tape is transient and pooled only below the bound documented in
[`pooling.md`](pooling.md). `Index` storage, by contrast, is caller-owned and
persists for navigation.

## Validation routing

The bitmap validator samples the leading blocks before selecting its packed
position consumer. Stage 1 is available on every supported build: SIMD kernels
classify supported architectures and portable SWAR handles the rest. The
current thresholds separate indentation-heavy and string-heavy corpora from
compact record-shaped documents where setup and per-position work cost more
than raw recursive descent. Threshold changes must name the whole-document
benchmarks, preserve at least a 10% classification margin on the maintained
route fixtures, and pass scalar/SIMD differential tests.

The current tuning record is:

- the 2 KiB sample commits its whitespace leg at 25% whitespace and a 3.5:1
  whitespace-to-emit ratio; the ratio moved from 4.5 after cheaper mask
  reduction made CITM-shaped inputs profitable;
- the string leg starts at 9/16 in-string bytes, requires skipped whitespace
  plus string bytes to exceed emitted positions 6:1, and rejects escape density
  above 1/16;
- twitter-shaped fixtures sample near 66% in-string bytes, while compact
  source-in-string fixtures sit near 50%, leaving more than 10% classification
  margin around the current boundary;
- production classifies the 32-block sample in one call, then groups four
  `Stage1ChunkBlocks` batches before each packed-position stage-2 consumption;
  the per-block and record-streamed walkers remain differential references; and
- adjacent non-ASCII UTF-8 regions coalesce across at most two ASCII blocks.

These figures explain the constants; they are not API promises. Replacing them
requires interleaved whole-document measurements on the pinned compiler.

## Index iterator code shape

Index iterators expose both pointer-receiver `Next` loops and by-value
`Valid`/`Current`/`Advance` loops. The by-value form keeps state in registers.
On the benchmark revision that established the API it was about 3.3x faster for
a general kind-only array loop, 6.4x for a fixed-stride flat array, 1.3x for a
record-shaped object, and 2.9x for a flat scalar object. These ratios are tuning
history, not compatibility guarantees; `BenchmarkIterShape` is the current
evidence source.

Route coverage lives in `route_differential_test.go`,
`valid_bitmap_stream_test.go`, `decoder_structural_test.go`, and the SIMD stage-1
tests. Maintained benchmarks include `BenchmarkDecodeLargeReused`,
`BenchmarkDecodeLargeIndentedReused`, `BenchmarkBuildIndex`, and the bitmap
corpus benchmarks.
