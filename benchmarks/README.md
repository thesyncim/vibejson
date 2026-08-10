# Benchmark snapshot

This directory publishes a machine-specific, reproducible comparison of public
JSON operations. The charts show absolute measurements; they are not normalized
scores, kernel-only throughput, or a blend of unlike ownership contracts.

The current snapshot measures commit
`bb2b4f03486ea1b97c7209a04dc89e43203835ff` on an Apple M4 Max
(`darwin/arm64`) with the pinned
`go1.27-devel_03845e30f7` compiler, one CPU, six samples per row, and a 300 ms
target per sample. The seven pinned Go standard-library corpus files contain
6,638,273 input bytes in total.

## Absolute results

![Absolute time for one seven-file corpus pass](charts/go-times.svg)

Each bar is the sum of the six-sample median for every corpus file: the absolute
time to apply that operation serially to all seven files once. Every panel starts
at zero and has its own scale.

The per-row guard passed all 56 vibejson mode/file/operation rows. The narrowest
latency lead is portable `citm_catalog` validation at 657.01 µs versus
759.75 µs; portable `synthea_fhir` validation is 776.09 µs versus 1.02 ms.
No vibejson row uses more bytes or allocations than its
`encoding/json` reference.

The string-rich validation lane is the real workload where SIMD has its largest
end-to-end effect:

![Strict whole-document validation](charts/simd-validation-times.svg)

| Public workload | vibejson SIMD | Fastest compatible peer | Difference |
| --- | ---: | ---: | ---: |
| Strict validation, all seven files | 1.93 ms | 3.79 ms (`encoding/json`) | 2.0× faster |
| Strict validation, escaped + Unicode files | 7.4 µs | 67.3 µs (segmentio) | 9.1× faster |
| Typed owned decode, all seven files | 3.54 ms | 7.61 ms (go-json) | 2.2× faster |
| Dynamic owned decode, all seven files | 18.85 ms | 26.31 ms (jsoniter) | 1.4× faster |
| Typed owned encode, all seven files | 5.12 ms | 5.16 ms (segmentio) | 1.01× faster |

The focused validation result is intentionally described as string-rich, not
universal. Portable builds always use the recursive validator. Accelerated
builds sample eligible large inputs, but number-dense `canada_geometry` and
`golang_source` remain on the recursive route because forcing the positions
pipeline was slower: position extraction and stage 2 cost more than they save
when punctuation and scalar starts are dense.

## Numeric SIMD results

The focused numeric chart measures complete public `Decode` calls into reused
typed slices. It compares the same JSON bytes under vibejson portable,
vibejson SIMD, and `encoding/json`; input generation, decoder compilation, and
destination allocation stay outside the timer.

![Absolute numeric-array decode time](charts/simd-numeric-times.svg)

| Reused typed decode | vibejson portable | vibejson SIMD | `encoding/json` | SIMD vs portable |
| --- | ---: | ---: | ---: | ---: |
| 1,024 positive 16-digit identifiers | 5.32 µs | 1.07 µs | 33.39 µs | 4.96× faster |
| 32,768 fixed-precision telemetry samples | 782.03 µs | 679.89 µs | 1.61 ms | 1.15× faster |
| 32,768 long geographic coordinates | 579.37 µs | 343.17 µs | 2.39 ms | 1.69× faster |

The three workloads are deliberately concrete rather than synthetic kernel
loops: 1,024 fixed-width identifiers, 32,768 fixed-precision telemetry values,
and 32,768 long geographic coordinates. The identifiers exercise batched digit
conversion; the float workloads exercise SIMD structural discovery while
retaining the shared exact scalar conversion path. Each row records its input
bytes and value count in [`results/numeric.json`](results/numeric.json).
The identifier row uses `DecoderOptions{Replace: true}`; for a flat scalar
slice that has the same reset-and-reuse result as `encoding/json.Unmarshal`.
The float rows use default decoder options.

Time is not the only cost:

![Absolute heap bytes for one seven-file corpus pass](charts/go-allocations.svg)

Owned typed decoding copies only retained text into compact append-only blocks;
it does not retain a private copy of the complete source. The allocation chart
is published beside the time chart. In this snapshot the seven-file typed pass
uses 210.1 KiB versus 916.7 KiB for `encoding/json`; the dynamic pass uses
20.83 MiB versus 23.51 MiB. The publisher also rejects any individual row where
vibejson exceeds `encoding/json` in time, bytes, or allocation count, so a
favorable aggregate cannot conceal a local loss.

## Fairness contract

The comparison harness enforces these rules before starting any timer:

- all libraries receive the exact same decompressed source bytes;
- typed decoders use the same concrete Go model and reuse one destination;
- typed and dynamic owned decoders must equal the `encoding/json` result;
- encoders receive the same typed value, return newly owned output, and must
  round-trip to the same JSON value;
- validators must accept the corpus input and reject the same late-invalid
  variant;
- type caches, output-size hints, corpus loading, and correctness checks remain
  outside timed regions; and
- each timed row records `ns/op`, `B/op`, and `allocs/op`; and
- publication fails if a vibejson row is slower or allocates more than its
  `encoding/json` reference row.

The numeric comparison additionally requires complete-document decoding into
the same concrete slice element type, one pre-sized reused destination, the
same input bytes and value count, and zero allocation for every published row.
`encoding/json.Unmarshal` is the compatible standard-library peer; the
vibejson SIMD row changes only `GOEXPERIMENT`.

The portable comparison includes `encoding/json`, go-json v0.10.6, segmentio
v0.5.4, and jsoniter v1.1.12. The vibejson SIMD rows use the same pinned compiler,
machine, corpus, API contract, and single-CPU setting with
`GOEXPERIMENT=simd`.

jsoniter participates in decode and encode panels, but not strict validation:
its public `Valid` API accepts trailing non-space bytes after one valid value.
Including it in that panel would compare different contracts.

The owned peer chart does not mix in vibejson's compiled zero-copy decode or
caller-buffer encode APIs. Those are useful application choices, but comparing
them against newly owned peer results would make the chart look better by
changing the work.

## Reproduce

From a clean repository root:

```sh
TIP_GO="$HOME/sdk/vibejson-gotip/bin/go" \
  ./benchmarks/publish-comparison.sh
```

`BENCHTIME`, `COUNT`, and `BENCH_MACHINE` may be set explicitly for another
controlled machine. The publisher isolates both repository modules with
`GOWORK=off`, compiles each portable/SIMD mode once, alternates their process
order by sample round, refuses a dirty worktree, captures raw logs in a
temporary directory, and rewrites:

- [`results/comparison.json`](results/comparison.json), containing corpus
  metadata, commands, per-file medians, and aggregates;
- [`results/numeric.json`](results/numeric.json), containing numeric workload
  shape and medians;
- [`charts/go-times.svg`](charts/go-times.svg);
- [`charts/go-allocations.svg`](charts/go-allocations.svg);
- [`charts/simd-validation-times.svg`](charts/simd-validation-times.svg); and
- [`charts/simd-numeric-times.svg`](charts/simd-numeric-times.svg).

Transient raw logs are not committed. Use
[`docs/benchmarking.md`](../docs/benchmarking.md) for the complete benchmark
coverage matrix and the interleaved baseline-versus-candidate regression gate.
