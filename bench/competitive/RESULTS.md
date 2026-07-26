# Competitive results

This is the current reproducible baseline. It is deliberately blunt: a row is
included only when the harness drove equivalent operations and the result has
multiple isolated samples. Historical tables remain available in Git history;
they are not mixed with current numbers.

## Conditions

| | |
| --- | --- |
| vibejson commit | `1a11b02233a125dd743bab22ce0612b0faee2abf` |
| Machine | Apple M4 Max, 16 cores, 64 GiB |
| OS | macOS 26.3.1, darwin/arm64 |
| Go | 1.26.0 |
| Competitors | bbolt 1.5.0, Badger 4.9.5, Pebble 1.1.5, modernc SQLite 1.54.0 (`go list -m -u` reported no updates on 2026-07-26) |
| Mixed corpus | 10,000 documents |
| Mixed samples | six isolated process-level runs |
| Async mixed run | 2,000 warmup + 20,000 measured operations |
| Sync mixed run | 200 warmup + 2,000 measured operations |
| Read/space corpus | 100,000 documents, 23.73 MiB raw JSON |
| Read/space samples | three isolated process-level runs |

`TestFullEquivalence` and `TestCorpusVariantsAreShapeMatched` passed before the
measurements. Medians below are the middle-pair average for six samples and the
middle observation for three samples.

At this commit the default and measured `CreateFrom` representation is
**verbatim**. Older benchmark prose incorrectly called that path compact. The
harness now labels explicit compact and verbatim bulk artifacts separately.
No compact footprint appears below because it was not measured in this run.

## Dedicated reads and ordered scans

The scan corpus has 100,000 documents. `iteration` touches one byte per value;
`all bytes` reads every value byte and is the ordered-scan throughput result.

| Engine | Random point | Iteration | Ordered all bytes | Point allocation | Scan allocation |
| --- | ---: | ---: | ---: | ---: | ---: |
| bbolt | **376.3 ns** | 8.479 ns/doc | **79.39 ns/doc** | 168 B / 3 | 576 B / 9 |
| Badger | 796.7 ns | 84.20 ns/doc | 203.7 ns/doc | 804 B / 8 | ~8.6 KiB / 44 |
| **vibejson** | 1,162 ns | **7.546 ns/doc** | 79.64 ns/doc | **0 B / 0** | **0 B / 0** |
| Pebble | 1,235 ns | 43.00 ns/doc | 112.4 ns/doc | 80 B / 2 | ~85 B / 1 |
| SQLite | 2,838 ns | 170.4 ns/doc | 244.5 ns/doc | 1,092 B / 22 | 27.4 MiB / 200,017 |

Interpretation:

- vibejson has the fastest iteration scan and is within 0.3% of bbolt on the
  stronger all-byte scan.
- vibejson all-byte scan is 1.41x faster than Pebble, 2.56x faster than Badger,
  and 3.07x faster than SQLite.
- vibejson point lookup beats Pebble by about 6% and SQLite by 2.44x, but is
  1.46x behind Badger and 3.09x behind bbolt. This is an open performance gap.
- all three vibejson paths allocate nothing.

## Realistic asynchronous mixed workloads

`sync=false` means every engine may acknowledge buffered work. vibejson uses
the explicit `DurabilityAsyncVisible` mode; Pebble uses `NoSync`, bbolt uses
`NoSync`, Badger uses `SyncWrites=false`, and SQLite uses WAL with
`synchronous=OFF`.

| Workload | vibejson | bbolt | Badger | Pebble | SQLite |
| --- | ---: | ---: | ---: | ---: | ---: |
| YCSB-B, 95% read / 5% update | 92,452 | 1,124,422 | 1,097,863 | **2,330,037** | 269,596 |
| YCSB-A, 50% read / 50% update | 13,854 | 161,408 | 342,737 | **1,793,875** | 181,830 |
| YCSB-F, 50% read / 50% RMW | 13,984 | 156,337 | 296,647 | **1,298,406** | 151,250 |
| Churn, read/update/delete+restore | 12,065 | 216,202 | 440,672 | **1,858,683** | 208,790 |
| Ordered-scan mix | 18,824 | 243,540 | 298,466 | **653,656** | 154,555 |

Values are total user operations per second. The decisive weakness is mutation
materialization and commit-buffer backpressure, not ordered reads.

### Mutation latency

| Operation | vibejson p50 / p95 / p99 | Best competitor p50 / p95 / p99 |
| --- | ---: | ---: |
| YCSB-B update | 35.146 / 45.250 / 7,818.667 µs | Pebble 0.583 / 0.917 / 1.667 µs |
| YCSB-A update | 40.499 / 53.688 / 5,027.291 µs | Pebble 0.563 / 0.959 / 1.583 µs |
| YCSB-F read-modify-write | 38.270 / 55.209 / 4,992.083 µs | Pebble 0.959 / 1.500 / 2.063 µs |
| Delete + restore | 118.979 / 4,334.938 / 10,454.354 µs | Pebble 0.959 / 1.396 / 2.042 µs |
| Ordered all-byte scan, 10k docs | 988.188 / 1,120.625 / 1,151.854 µs | bbolt 811.000 / 861.688 / 899.208 µs |

Pebble is about 60–72x lower at update p50 and about 124x lower at
delete+restore p50. vibejson's multi-millisecond p99 stalls are the main
throughput limiter to remove. A proposed write optimization does not pass the
default-path gate unless it removes those stalls without changing the read
path.

## Historical fsync-class mixed workloads

This table is retained to make the measured history reproducible, but it is
**not the current power-loss durability table**. At the pinned commit,
vibejson, bbolt, and Pebble used Darwin `fsync`, which Apple documents as not
draining a drive's volatile cache. Badger used `msync(MS_SYNC)`. Only the
SQLite row enabled `fullfsync=1`.

Current vibejson `DurabilitySync` uses `F_FULLFSYNC` on Darwin. Those stronger
numbers must be remeasured before a current crash-safe comparison is
published; do not quote the vibejson row below as its current sync result.

| Workload | vibejson | bbolt | Badger* | Pebble | SQLite |
| --- | ---: | ---: | ---: | ---: | ---: |
| YCSB-B | 2,446† | 1,591† | 403,200† | 3,031† | **2,892** |
| YCSB-A | 306† | 171† | 14,869† | 321† | **297** |
| Churn | 329† | 253† | 49,390† | 431† | **530** |
| Ordered-scan mix | 573† | 317† | 36,559† | 607† | **599** |

Values are total user operations per second. `†` means the row did not drain
the Darwin drive cache and is not power-loss comparable with SQLite or current
vibejson.

vibejson mutation latency:

| Operation | p50 | p95 | p99 |
| --- | ---: | ---: | ---: |
| YCSB-B update | 8.915 ms | 10.494 ms | 11.479 ms |
| YCSB-A update | 5.605 ms | 10.107 ms | 11.543 ms |
| Churn update | 9.421 ms | 10.946 ms | 12.107 ms |
| Delete + restore | 17.919 ms | 20.420 ms | 22.423 ms |
| Scan-mix update | 6.044 ms | 9.584 ms | 11.227 ms |

## Disk and retained memory

The corpus is 100,000 unindexed documents. Disk comparisons use allocated
blocks, not sparse apparent size. Low- and high-cardinality files were identical
for every row at this commit because the measured vibejson representation was
verbatim and competitor compression was disabled.

| Engine | Apparent / allocated | HeapAlloc | Runtime resident | Peak RSS |
| --- | ---: | ---: | ---: | ---: |
| Badger | 257.0 / **26.6 MiB** | 86.1 MiB | 97.0 MiB | 154.1 MiB |
| SQLite | 28.1 / 28.1 MiB | **2.5 MiB** | 13.0 MiB | 152.3 MiB |
| bbolt | 45.8 / 29.7 MiB | **2.5 MiB** | **12.9 MiB** | **91.4 MiB** |
| **vibejson bulk, verbatim** | 32.2 / 32.2 MiB | 16.6 MiB | 27.5 MiB | 174.4 MiB |
| vibejson Put replay | 35.9 / 36.5 MiB | 16.6 MiB | 28.8 MiB | 185.6 MiB |
| Pebble | 50.6 / 50.7 MiB | 36.3 MiB | 46.6 MiB | 114.9 MiB |

The raw corpus is 23.73 MiB. vibejson bulk is 21% larger than Badger, 15%
larger than SQLite, and 8% larger than bbolt, but 36% smaller than Pebble's
total including its live unretired WAL. The old 15.9 MiB vibejson claim is
withdrawn: the adapter did not actually enable compact documents in the run
that was being described as compact.

Production-compressed footprint is unmeasured. This harness intentionally
disabled Badger/Pebble compression and previously exposed no explicit compact
vibejson row. It now exposes `-compact`; publish the next space table with both
vibejson representations and matched production compression where each engine
supports it.

## Secondary index

An exact country index selecting 945 of 100,000 documents:

| Engine | Query | Allocations | Indexed file | Index bytes |
| --- | ---: | ---: | ---: | ---: |
| **vibejson** | **36.108 µs** | 1,328 B / 17 | 34.6 MiB | +2.4 MiB |
| SQLite | 539.705 µs | 568 B / 20 | 30.3 MiB | +2.2 MiB |

vibejson is 14.95x faster for this exact indexed filter. The key/value engines
do not expose native JSON secondary indexes. No current multi-index
deduplication footprint run exists, so this baseline makes no dedup ratio claim.

## Reproduction

From `bench/competitive`:

```sh
go test -run 'TestFullEquivalence|TestCorpusVariantsAreShapeMatched' -count=1 -timeout=60m .

go build -o /tmp/vibejson-mixed ./cmd/mixed
for rep in {1..6}; do
  for w in ycsb-b ycsb-a ycsb-f churn scan; do
    for e in vibejson-durable bbolt badger pebble sqlite; do
      /tmp/vibejson-mixed -engine="$e" -workload="$w" \
        -corpus=10000 -operations=20000 -warmup=2000 -sync=false
    done
  done
done

for rep in {1..6}; do
  for w in ycsb-b ycsb-a churn scan; do
    for e in vibejson-durable bbolt badger pebble sqlite; do
      /tmp/vibejson-mixed -engine="$e" -workload="$w" \
        -corpus=10000 -operations=2000 -warmup=200 -sync=true
    done
  done
done

go test -run '^$' \
  -bench='^Benchmark(PointRead|Scan|ScanAllBytes)/(vibejson-durable|bbolt|badger|pebble|sqlite)$' \
  -benchtime=2s -count=3 -timeout=30m .

go build -o /tmp/vibejson-footprint ./cmd/footprint
for rep in {1..3}; do
  for card in low high; do
    for e in vibejson-durable bbolt badger pebble sqlite; do
      /tmp/vibejson-footprint -engine="$e" -cardinality="$card"
    done
    /tmp/vibejson-footprint -engine=vibejson-durable -putloop -cardinality="$card"
    /tmp/vibejson-footprint -engine=vibejson-durable -compact -cardinality="$card"
  done
done

go test -run '^$' \
  -bench='^BenchmarkIndexedFilter/(vibejson-durable|sqlite)$' \
  -benchtime=2s -count=3 .
```

The `sync` scan workload contains only two ordered scans per run, so its
within-run percentiles collapse to one order statistic. Use the dedicated
100,000-document scan benchmark for ordered-scan comparisons.
