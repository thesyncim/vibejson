# simdjson benchmarks

This separate module measures the repository's release contract: strict
correctness, safe default ownership, fast ordinary paths, and zero hidden
tuning requirements. Comparison-only dependencies never enter the root module
graph.

## Publication record

<!-- benchpublish:go-publication:start -->
Every table in this document is generated from one clean publication record:

| Component | Revision |
|---|---|
| simdjson | `b05b7ce145bb9a3c53301beb2619241180c786ce` (`dirty=false`) |
| Go | `go1.27-devel_03845e30 Fri Jul 10 12:31:49 2026 -0700 darwin/arm64`, commit `03845e30f7b73d1703bd8c21017297f6eecb76d6` |
| Machine | Apple M4 Max, `darwin/arm64`, one CPU |
| Samples | six approximately 300 ms samples, median reported |

Each `valid`, `dynamic-owned`, `dom`, `typed-reused`, and `encode`
contract runs in a fresh process. Compilation, plan creation, fixture decode,
capacity preparation, and correctness checks happen before the timer.

## Headline geomeans

| Operation | vs `encoding/json` | vs fastest compatible rival | SIMD vs pure Go |
|---|---:|---:|---:|
| Strict validation | **3.094x** | **2.785x** | **1.804x** |
| Typed owned decode | **4.169x** | **1.808x** | **1.124x** |
| Dynamic owned decode | **3.653x** | **1.878x** | **1.068x** |
| Owned encode | **2.553x** | **1.458x** | **1.318x** |
| Compiled encode reuse | **4.634x** | **2.647x** | **1.497x** |
| Parse + complete walk | **6.293x** | — | **1.230x** |

![Absolute time for one complete corpus pass](charts/headline.svg)

The chart sums the seven per-file median times and shows the absolute time to
complete one full corpus pass; lower bars are faster. The table uses
geometric-mean ratios so every payload has equal aggregate weight.

The rival is the fastest compatible per-payload result from go-json, Segment,
jsoniter, or fastjson. Aggregate leads do not imply a win on every payload.

## Per-corpus results

### Strict validation

| Corpus | `encoding/json` | simdjson | Rival | Rival time | vs stdlib | vs rival |
|---|---:|---:|---|---:|---:|---:|
| Canada geometry | 215.8 us | **115.3 us** | fastjson | 196.1 us | **1.87x** | **1.70x** |
| CITM catalog | 718.4 us | **335.2 us** | fastjson | 811.6 us | **2.14x** | **2.42x** |
| Go source | 1.249 ms | **907.0 us** | Segment | 1.149 ms | **1.38x** | **1.27x** |
| Escaped strings | 51.9 us | **4.2 us** | Segment | 52.6 us | **12.22x** | **12.41x** |
| Unicode strings | 17.7 us | **3.1 us** | fastjson | 6.6 us | **5.65x** | **2.11x** |
| Synthea FHIR | 975.1 us | **343.4 us** | fastjson | 1.209 ms | **2.84x** | **3.52x** |
| Twitter status | 347.6 us | **138.7 us** | fastjson | 375.3 us | **2.51x** | **2.70x** |

![Absolute strict-validation time by corpus](charts/validation-times.svg)

Each vertical pair is one measured payload with its own scale; the labels are
absolute median times and lower bars are faster. Valid input allocates zero
bytes and zero objects.

### Typed owned decode

| Corpus | `encoding/json` | simdjson | Rival | Rival time | vs stdlib | vs rival |
|---|---:|---:|---|---:|---:|---:|
| Canada geometry | 1.202 ms | **180.4 us** | go-json | 741.9 us | **6.66x** | **4.11x** |
| CITM catalog | 2.473 ms | **749.4 us** | go-json | 1.208 ms | **3.30x** | **1.61x** |
| Go source | 6.058 ms | **1.298 ms** | Segment | 2.150 ms | **4.67x** | **1.66x** |
| Escaped strings | 190.9 us | **33.7 us** | go-json | 62.9 us | **5.67x** | **1.87x** |
| Unicode strings | 39.7 us | **7.5 us** | go-json | 12.6 us | **5.32x** | **1.69x** |
| Synthea FHIR | 3.644 ms | **1.561 ms** | go-json | 1.830 ms | **2.33x** | **1.17x** |
| Twitter status | 1.314 ms | **433.8 us** | go-json | 672.2 us | **3.03x** | **1.55x** |

![Absolute typed-decode time by corpus](charts/typed-decode-times.svg)

Each corpus keeps its measured time rather than converting the bars to a
ratio.

### Dynamic owned decode

| Corpus | `encoding/json` | simdjson | Rival | Rival time | vs stdlib | vs rival |
|---|---:|---:|---|---:|---:|---:|
| Canada geometry | 2.817 ms | **874.2 us** | go-json | 1.771 ms | **3.22x** | **2.03x** |
| CITM catalog | 7.251 ms | **2.305 ms** | jsoniter | 4.152 ms | **3.15x** | **1.80x** |
| Go source | 16.898 ms | **4.333 ms** | go-json | 9.185 ms | **3.90x** | **2.12x** |
| Escaped strings | 200.6 us | **30.0 us** | go-json | 71.1 us | **6.68x** | **2.37x** |
| Unicode strings | 50.1 us | **11.9 us** | go-json | 19.6 us | **4.22x** | **1.65x** |
| Synthea FHIR | 10.882 ms | **3.840 ms** | jsoniter | 6.466 ms | **2.83x** | **1.68x** |
| Twitter status | 3.296 ms | **1.199 ms** | jsoniter | 1.943 ms | **2.75x** | **1.62x** |

![Absolute dynamic-decode time by corpus](charts/dynamic-decode-times.svg)

Each corpus keeps its measured time rather than converting the bars to a
ratio.

Dynamic `any` values use ordinary Go interface construction. The current
allocation profile is:

| Corpus | Bytes/op | Allocs/op |
|---|---:|---:|
| Canada geometry | 1,440,704 | 22,228 |
| CITM catalog | 6,193,280 | 41,187 |
| Go source | 7,611,600 | 103,805 |
| Escaped strings | 41,112 | 77 |
| Unicode strings | 34,968 | 76 |
| Synthea FHIR | 8,630,136 | 64,558 |
| Twitter status | 2,404,688 | 11,209 |

### Encode

| Corpus | stdlib | Owned | Compiled reuse | Rival | Rival time |
|---|---:|---:|---:|---|---:|
| Canada geometry | 612.7 us | **381.9 us** | **298.8 us** | Segment | 496.6 us |
| CITM catalog | 1.056 ms | 389.7 us | **200.2 us** | go-json | **373.7 us** |
| Go source | 2.935 ms | **1.056 ms** | **673.3 us** | Segment | 1.328 ms |
| Escaped strings | 20.7 us | **7.0 us** | **3.3 us** | jsoniter | 21.0 us |
| Unicode strings | 20.8 us | **7.0 us** | **3.3 us** | jsoniter | 21.1 us |
| Synthea FHIR | 6.082 ms | 1.980 ms | **1.012 ms** | Segment | **1.906 ms** |
| Twitter status | 711.5 us | **326.2 us** | **169.6 us** | Segment | 336.8 us |

![Absolute owned-encode time by corpus](charts/owned-encode-times.svg)

![Absolute compiled-encode-reuse time by corpus](charts/compiled-encode-times.svg)

The two charts separate owned output from caller-buffer reuse. Every corpus
has its own vertical scale and retains the absolute median labels.


### Parse and complete walk

| Corpus | stdlib `any` + walk | simdjson parse + walk | Lead |
|---|---:|---:|---:|
| Canada geometry | 2.807 ms | **546.5 us** | **5.14x** |
| CITM catalog | 7.818 ms | **987.4 us** | **7.92x** |
| Go source | 17.913 ms | **3.777 ms** | **4.74x** |
| Escaped strings | 197.5 us | **42.4 us** | **4.66x** |
| Unicode strings | 49.8 us | **8.2 us** | **6.06x** |
| Synthea FHIR | 11.741 ms | **1.169 ms** | **10.05x** |
| Twitter status | 3.445 ms | **482.8 us** | **7.14x** |

![Absolute parse-and-complete-walk time by corpus](charts/walk-times.svg)

Every vertical pair shows the absolute time to complete the same
parse-and-walk task; lower bars are faster.

### Reusable structural index

`BuildIndex` validates the input and builds a caller-owned navigable tape.
Correctly sized storage is reused; every row allocates zero bytes and objects.

| Corpus | Time | Throughput |
|---|---:|---:|
| Canada geometry | **119.8 us** | **2.26 GB/s** |
| CITM catalog | **390.2 us** | **4.43 GB/s** |
| Go source | **841.7 us** | **2.31 GB/s** |
| Escaped strings | **4.6 us** | **9.15 GB/s** |
| Unicode strings | **3.4 us** | **5.36 GB/s** |
| Synthea FHIR | **419.0 us** | **4.79 GB/s** |
| Twitter status | **152.1 us** | **4.15 GB/s** |

## Native hook cost

Hooks keep the public API composable without weakening default ownership.
Decode uses retainable receiver state; encode passes ordinary GC-visible
receivers.

| Case | Interpreter | Native hook | Hook / interpreter | Bytes/op | Allocs/op |
|---|---:|---:|---:|---:|---:|
| Decode small | 46.3 ns | 50.6 ns | 1.09x | 0 | 0 |
| Decode 1,024 records | 72.7 us | 78.0 us | 1.07x | 0 | 0 |
| Encode small | 34.6 ns | 31.3 ns | 0.90x | 0 | 0 |
| Encode 1,024 records | 37.9 us | 38.0 us | 1.00x | 12 | 0 |

## SIMD controls

Both binaries use the same candidate, compiler, corpus, isolated-process
contract, and one CPU.

| Path | SIMD wins | Geomean uplift |
|---|---:|---:|
| Validation | 6/7 | **1.804x** |
| Dynamic owned | 4/7 | **1.068x** |
| Dynamic zero-copy | 5/7 | **1.082x** |
| Parse + complete walk | 7/7 | **1.230x** |
| Typed owned | 7/7 | **1.124x** |
| Typed zero-copy | 5/7 | **1.152x** |
| Encode owned | 7/7 | **1.318x** |
| Encode compiled reuse | 7/7 | **1.497x** |
| Reusable structural index | 7/7 | **1.809x** |

![Absolute SIMD and portable-Go completion time](charts/simd-uplift.svg)

Each pair is the absolute time for one pass over all seven payloads. Pairs use
independent vertical scales so small paths remain legible; labels preserve the
measured time and lower bars are faster.

## Additional Go context

`encoding/json/v2` is built from the pinned Go tip. Its time divided by
simdjson time is 3.347x for typed owned decode, 2.032x for dynamic owned decode,
and 2.394x for owned encode.

Sonic is measured with `go1.26.4 darwin/arm64`. Its native path does not support the pinned Go
tip. Sonic time divided by simdjson time is 1.692x for typed owned decode,
1.143x for dynamic owned decode, and 2.613x for owned encode. Its syntax-only
validation result (1.374x) is context, not a strict-UTF-8 peer.
<!-- benchpublish:go-publication:end -->

## Reproduce

Build the pinned toolchain and run the default publication path:

```sh
./scripts/bootstrap-gotip.sh "$HOME/sdk/simdjson-gotip"
TIP_GO="$HOME/sdk/simdjson-gotip/bin/go" ./benchmarks/publish.sh
```

The publisher refuses a dirty tree, records the repository and toolchain
revisions, uses six 300 ms one-CPU samples, starts every corpus contract in a
fresh process, runs pure-Go, jsonv2, native Sonic, hook, and C++ controls,
verifies cross-language digests, and generates every table and chart from the
same normalized result file. Use `run-comparison.sh` directly for exploratory
`BENCH`, `BENCHTIME`, and `COUNT` overrides.

The equivalent C++ command and current results are documented under
[crosslang](crosslang/). That runner fails unless every semantic digest matches
and the repository is clean.

The interleaved before/after gate used for hot-path changes is:

```sh
GOTIP="$HOME/sdk/simdjson-gotip/bin/go" ./scripts/bench-gate.sh -b HEAD~1 -c 63
```

Its default pattern covers validation, reusable structural indexing, typed and
dynamic decode, and encode. It exits non-zero for any statistically significant
`sec/op` regression above 2% and for any significant `B/op` or `allocs/op`
increase; `-r` changes the time threshold. `-c` pins the exact number of rows
the selector must emit on every round; maintained targeted gates must set it.
The gate uses one logical processor, matching the publisher and avoiding
multi-P scheduler and pool-migration noise in sequential benchmarks. Use
`-d .` with an explicit root-package selector and row count for resource and
hook contracts. Pull requests run these checks on the dedicated
`simdjson-performance` runner rather than a noisy shared host. The nested
module pins every comparison dependency in `go.mod` and `go.sum`.
