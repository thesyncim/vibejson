# Competitive results

**Every number this harness publishes lives in this file and nowhere else.** No
prose in this repository quotes a figure from it; prose points here. That is not
a style preference — several of these engines, `store/durable` included, are
under active change, and a number pasted into a paragraph goes stale silently
while the paragraph keeps asserting it.

Regenerate the whole file with the commands in each section. Every section
states what produced it.

## Conditions

| | |
| --- | --- |
| Machine | Apple M4 Max, darwin/arm64, macOS 26.3.1 |
| Go | 1.26.5 |
| Corpus | 100,000 documents, 229-268 bytes each, 23.73 MiB total |
| Repetitions | `-count=6`, medians reported |
| Read benchmarks | one process per engine (isolated); see section 7 |

**These figures were taken on a contended machine, and it shows in exactly one
place.** Another tenant was running on this host throughout; load average moved
between 5 and 18 over the session. The effect was not uniform across the
workloads:

- **Read benchmarks: sound.** Every row's six repetitions agree within 10%, and
  every scan, filter, and indexed-filter row reproduced across three separate
  runs hours apart. Two point-read rows did not and are given as ranges.
- **Bulk-load benchmarks: contaminated, and not usable as a cross-engine
  comparison.** These are disk-bound and shared a disk with the other tenant.
  Four rows have a max/min spread over 3x and one over 20x. Section 4 publishes
  them with their spreads and says which two rows are readable.
- **Point-write benchmarks: mostly sound**, spreads under 1.3x for most rows,
  but still disk-bound and still worth re-taking.

Where a figure below disagrees with one quoted in a `Tuning()` string or a code
comment, the figure here is the one this revision of the harness measured, and
the disagreement is called out where it matters.

**`store` and `store/durable` were mid-change while this ran.** Several
concurrent changes to `store/`, `store/durable/`, and `query/` were in the
working tree. The vibejson rows are a snapshot of an in-flight tree, not of a
release.

## 1. Disk footprint, durable engines

Produced by:

```sh
go build -o /tmp/footprint ./cmd/footprint
/tmp/footprint -engine=baseline -header
for c in low high; do
  for e in bbolt badger pebble sqlite vibejson-durable; do
    /tmp/footprint -engine="$e" -cardinality="$c"
  done
  /tmp/footprint -engine=vibejson-durable -putloop -cardinality="$c"
done
```

**Read the allocated column.** Every comparison in this table is made on
allocated blocks. Apparent size is `st_size` summed over the directory, and
two of these engines deliberately create files far larger than the blocks they
occupy: Badger truncates its value log and memtable file to twice
`ValueLogFileSize` and mmaps them, and bbolt grows its file by doubling and
leaves the tail unwritten. Summing `st_size` reported Badger at 257 MiB for a
corpus it stores in 26.6 MiB of real blocks — a **9.7x error** — and it was the
number this harness published. Apparent size is still reported, because a sparse
file does occupy address space and can be filled in later, but it is not a
footprint.

**Read the cardinality columns together.** The shipped corpus is ~92% redundant
(`gzip -9` compresses its 23.73 MiB to 1.84 MiB). Exactly one engine here
exploits that: `store/durable`'s bulk writer builds a shape template and a value
dictionary. The high-cardinality corpus is document-for-document the same shape
and the same byte length with per-document random string values, and it is the
control. A reader must not be able to mistake corpus redundancy for engine
compression, so both are always published.

| Engine | Corpus | Apparent MiB | **Allocated MiB** | High-card. apparent | **High-card. allocated** | Change |
| --- | --- | ---: | ---: | ---: | ---: | ---: |
| vibejson-durable, **bulk** | low / high | 15.9 | **15.9** | 28.2 | **28.2** | **+77%** |
| badger | low / high | 257.0 | **26.6** | 257.0 | **26.6** | 0% |
| sqlite | low / high | 28.1 | **28.1** | 28.1 | **28.1** | 0% |
| bbolt | low / high | 45.8 | **29.7** | 45.8 | **29.7** | 0% |
| pebble | low / high | 50.6 | **54.3** | 50.6 | **56.9** | +4.8% |
| vibejson-durable, **Put loop** | low / high | 60.4 | **61.4** | 60.4 | **61.2** | -0.3% |

Corpus reference line, from `/tmp/footprint -corpus-stats -cardinality=…`:

| Corpus | Raw | `gzip -9` | Ratio |
| --- | ---: | ---: | ---: |
| low (shipped) | 23.73 MiB | 1.84 MiB | 7.7% |
| high | 23.73 MiB | 8.04 MiB | 33.9% |

What this table actually says:

- **`store/durable` has two disk footprints and they differ by 3.9x.** Only the
  bulk path (`durable.CreateFrom`) emits the compact shape-template/value-
  dictionary representation; `EncodeDocumentGroup` has exactly one non-test
  caller, `store/durable/store_file_bulk.go`. A collection built by replaying
  `Put` is 61.4 MiB — **larger than every competitor in this table**. Both rows
  are published, always, and neither may be described as "vibejson's footprint".
- **Most of the bulk path's advantage on the shipped corpus is the corpus.**
  15.9 -> 28.2 MiB when only value entropy changes, +77%, while every competitor
  moves by 0%. On the high-cardinality corpus the bulk file is *larger* than
  Badger's 26.6 MiB and level with SQLite's 28.1 MiB.
- **Pebble's allocated size exceeds its apparent size** (54.3-56.9 against
  50.6). It is the only engine here whose allocated figure is the larger one,
  and the reason is in the per-file breakdown below.

### Which file is each figure, from `-files`

`/tmp/footprint -engine=<name> -files`. MiB, apparent then allocated.

| Engine | File | Apparent | Allocated | |
| --- | --- | ---: | ---: | --- |
| badger | `000001.vlog` | 128.0 | **0.0** | **8192x sparse — entirely address space** |
| badger | `00001.mem` | 128.0 | 26.6 | 4.8x sparse; holds all the real blocks |
| badger | everything else | 1.0 | 0.0 | |
| bbolt | `bolt.db` | 45.8 | 29.7 | 1.5x sparse; grown by doubling, tail unwritten |
| pebble | `000002.log` (WAL) | 25.3 | **26.0** | **a live log, fully allocated, not sparse** |
| pebble | 14 `.sst` files | 25.1 | 30.0 | allocated exceeds apparent on every one |
| sqlite | `sqlite.db` (+shm, wal) | 28.1 | 28.1 | dense; WAL checkpointed out |
| vibejson-durable | `vibejson.db` | 15.9 | 15.9 | dense, one file |

Two things this makes unambiguous, and the second was mischaracterised in the
audit that prompted this correction:

- **Badger's 257 MiB is one 128 MiB file with zero blocks and one with 26.6.**
  It is not storing 257 MiB by any definition. This is the 9.7x error.
- **Pebble is not holding a sparse or recycled log slot.** Its WAL is a live
  file, fully allocated, holding a second copy of the records that
  `DiskBytes`' `Flush()` has already written into the SSTs — and Pebble offers
  no call to retire it. So half of its figure is a genuine second copy on disk,
  and the allocated column charges every byte. **This is an asymmetry against
  Pebble**: SQLite is given `PRAGMA wal_checkpoint(TRUNCATE)` and its log leaves
  its figure entirely, while Pebble's is counted twice. Pebble's stored-data
  component is ~25.1 MiB of SSTs, which is level with SQLite, bbolt, and Badger;
  its 50.6/54.3 total is that plus an unretired log. Do not compare Pebble's
  total against another engine's without saying so.

### Retracted claims

Two comparative claims were derived from the apparent-size column and are
withdrawn. Neither is present in the checked-in tree as of this correction — a
repository-wide search for both phrasings and for their component numbers found
nothing — so this table is not a diff of a document, it is a record of two
conclusions the broken measurement produces, published so that nobody re-derives
either by running it. Both ratios fall straight out of the old
`dirBytes`-only column: 45.8/15.9 = 2.9 and 257.0/15.9 = 16.2.

| Retracted | Where it came from | Correct statement |
| --- | --- | --- |
| "3x smaller than bbolt" | 45.8 apparent / 15.9 bulk | On allocated blocks the bulk file is 1.87x smaller than bbolt on the redundant corpus and 1.05x smaller on the high-cardinality one. A `Put`-built collection is **2.1x larger** than bbolt. |
| "16x smaller than badger" | 257.0 apparent / 15.9 bulk | Badger's 257 MiB is 26.6 MiB of allocated blocks in two sparse mmapped files. The bulk file is 1.67x smaller on the redundant corpus and **6% larger** on the high-cardinality one. A `Put`-built collection is **2.3x larger** than Badger. |

## 2. Memory footprint

Same command as section 1. One engine per process, because Go-heap residency and
process RSS are process-global.

| Engine | HeapAlloc | HeapSys | RuntimeResident | MaxRSS |
| --- | ---: | ---: | ---: | ---: |
| baseline (no engine) | 2.5 | 43.4 | 11.7 | 54.2 |
| vibejson-heap | 3.8 | 119.4 | 15.9 | 164.9 |
| vibejson-durable (bulk) | 7.4 | 127.2 | 18.5 | 171.0 |
| vibejson-durable (Put) | 7.4 | 83.3 | 17.8 | 190.1 |
| bbolt | 2.5 | 79.4 | 12.6 | 94.1 |
| badger | 86.1 | 167.3 | 97.0 | 152.0 |
| pebble | 36.3 | 123.3 | 46.0 | 131.2 |
| sqlite | 2.5 | 71.4 | 13.0 | 152.6 |

MiB. `HeapAlloc` is not a database size and no row here may be quoted as one.
bbolt's working set is an mmap the Go heap never sees; `modernc.org/sqlite`
allocates through its own page allocator; Pebble's block cache is manually
managed off-heap. All three read far below what they cost. `MaxRSS` sees them
but is a high-water mark that includes the load's transient peak.

## 3. Read path

Medians of `-count=6`, one process per engine. Produced by the loop in the
appendix; section 7 explains why the mode matters.

### Durable engines

| Engine | Point read | Scan, **iteration only** | Scan, **all bytes** | Filter, no index | Indexed filter |
| --- | ---: | ---: | ---: | ---: | ---: |
| bbolt | 405 ns | 8.6 ns/doc | 80 ns/doc | 66 ns/doc | no index |
| pebble | 630 ns | 25.4 ns/doc | 109 ns/doc | 95 ns/doc | no index |
| badger | 1,004 ns | 96.5 ns/doc | 192 ns/doc | 176 ns/doc | no index |
| **vibejson-durable** | 1,337-1,696 ns | 162 ns/doc | 234 ns/doc | 182 ns/doc | **37.2 µs** |
| sqlite | 3,009 ns | 177 ns/doc | 247 ns/doc | 834 ns/doc | 563 µs |

Point read is given as a range where three separate runs disagreed by more than
10%; see section 7.

Allocation per operation, same runs:

| Engine | Point read | Scan (iteration only) | Filter |
| --- | ---: | ---: | ---: |
| vibejson-durable | 64 B / 1 | 0 B / 0 | **25.2 MB / 70** |
| bbolt | 168 B / 3 | 576 B / 9 | 576 B / 9 |
| pebble | 192 B / 2 | 433 B / 2 | 137 B / 1 |
| badger | 804 B / 8 | 8.45 kB / 42 | 8.55 kB / 44 |
| sqlite | 1,092 B / 22 | **27.4 MB / 200,017** | 568 B / 20 |

### In-memory — an upper bound, not a competitor

`store` is process memory with no file, no fsync, and no recovery. It is in this
harness to bound what removing durability from the question is worth. It is
never a row in the table above and its numbers are never placed in a column
beside a durable engine's.

| Engine | Point read | Scan, **iteration only** | Scan, **all bytes** | Filter, no index | Indexed filter |
| --- | ---: | ---: | ---: | ---: | ---: |
| vibejson-heap | 152-234 ns | 6.4 ns/doc | 79 ns/doc | 79 ns/doc | 64.6 µs |

All five vibejson-heap columns are 0 B / 0 allocs per operation.

### Reading this table

- **Two scan columns, and they disagree by more than 10x.** bbolt walks its
  B+tree at 8.6 ns/doc and hands over the bytes at 80 ns/doc; `store`'s figures
  are 6.4 and 79. Quoting the iteration column as scan throughput states a
  figure above this machine's memory bandwidth. Quote them together or quote the
  all-bytes one.
- **`store/durable` currently loses the read row.** Against bbolt it is 3.3-4.2x
  slower on a point read, 19x on iteration, and 2.9x on an all-bytes scan. It
  wins one column outright — the indexed filter, where it is 15x faster than
  SQLite's index and 1.7x faster than `store`'s — and that is the column the
  key/value stores cannot enter at all.
- **`store/durable`'s filter allocates 25 MB per query** across 70 allocations,
  against 576 bytes for bbolt, while its scan path allocates nothing. The
  reusable `query.Exec` is not being reused across executions on the file
  backend in this revision of the tree. That is a bug-shaped number, not a
  design cost.
- **SQLite's scan allocates 27 MB and 200,017 objects per pass** — two
  allocations per row, from the driver, even through `sql.RawBytes`. Its
  indexed-filter figure is a `count(*)` over an index and is a real 563 µs.
- Badger's scan and filter numbers are only this good because
  `PrefetchValues` is off; see section 5.

### The filter row is not what it looks like

`BenchmarkParse`, medians of `-count=6`, no storage engine underneath:

| Extraction | Per document |
| --- | ---: |
| vibejson compiled pointer, trusted, early-exit | 56 ns, 0 allocs |
| `encoding/json` into a one-field struct | 1,254 ns, 256 B, 7 allocs |

The key/value stores cannot see inside a value, so the filter workload hands
them vibejson's own fastest extraction — a precompiled RFC 6901 pointer, an
early-exit scan, the non-revalidating trusted parser. That is 22x faster than
what an application reaching for the standard library would get. Substituting
`encoding/json` adds roughly 1.2 µs per document to every key/value engine's
filter figure and to nothing else, which moves bbolt's 66 ns/doc to
approximately 1.26 µs/doc and inverts the filter column outright. The row as
printed measures storage, not what filtering through these engines costs an
application.

Note the 56 ns above is an *early-exit* extraction of one field, not a full
parse. Building the whole structural tape for the same document costs 134.5 ns,
against 1,454 ns for `encoding/json` into `any` — a 10.8x gap, not 22x. Where
that gap comes from, measured by removing one piece at a time:

| Step | Per document |
| --- | ---: |
| `vibejson.Valid` — scan only, no tape built | 120.0 ns |
| `vibejson.BuildIndex` — full 26-entry tape | 134.5 ns |
| `encoding/json.Valid` — scan only | 474.0 ns |
| `encoding/json.Unmarshal` into `any` | 1,454 ns, 1,240 B, 40 allocs |

So **3.9x is the scanner**, measured with no tape in the picture at all, and
roughly **2.8x is not allocating**. The tape costs 10.8% of a parse — it is a
cost the scanner pays for, not the source of the advantage. It earns that cost
back on the second query against the same document (one compiled-pointer
lookup on a built tape is 14.1 ns, so the break-even against re-parsing is
about 1.1 queries), which is why it exists. But an optimisation that trades any
scanner throughput for a smaller or cheaper tape is trading 290% for 10%.

### Stability

Every read row above has a tight spread *within* its six repetitions: the widest
is `BenchmarkPointRead/vibejson-heap` at 180-249 ns, and every other row's
maximum is within 10% of its minimum. Every scan, filter, and indexed-filter row
also reproduced *across* the three separate runs this file draws on. Point read
did not, for two engines, which is why those two are given as ranges — see
section 7.

Absolute latency is another matter. The CPU-bound read benchmarks fit in cache
and were largely insulated from the co-tenant on this host; the I/O-bound load
and write benchmarks were not, and section 4 says so in detail. A point-read
figure here is a defensible comparison and an unreliable absolute — re-measure
on a quiet machine before quoting one as a latency.

## 4. Write path

`BenchmarkBulkLoad` and `BenchmarkPointWrite`, medians of `-count=6`. Each
`-count` repetition of the point-write rows builds its own store and throws it
away; the shared fixture this used to use accumulated every earlier
repetition's writes, and SQLite's `sync=true` row drifted +64% across three
repetitions of a single `-count=3` run. The median across repetitions hid that,
and it penalised whichever engine got through the most writes per repetition —
that is, the fastest ones — hardest.

```sh
go test -run '^$' -bench='BenchmarkBulkLoad$|BenchmarkPointWrite' -count=6 -timeout=180m .
```

### Bulk load — mostly contaminated, and reported as such

> **Do not quote a cross-engine comparison from this table.** These rows are
> disk-bound, and this host had another tenant doing I/O throughout. Four of the
> nine engine/configuration rows below have a max/min spread across their six
> repetitions of more than 3x, one of more than 20x, and several are
> non-monotonic in ways that are physically impossible (SQLite's
> `sync=true, indexed=true` row is ten times faster than its
> `sync=true, indexed=false` row; `store/durable`'s indexed rows beat its
> unindexed ones). What the ordering actually tracks is when in the run a row
> executed, because the host's load fell over the session. This section is
> published as evidence of what was measured, not as a result.

Milliseconds for one full 100,000-document load. min / **median** / max over
`-count=6`, with the spread stated so no row can be quoted without it.

| Engine | sync | indexed | min | **median** | max | spread |
| --- | --- | --- | ---: | ---: | ---: | ---: |
| vibejson-heap | n/a | no | 49.5 | **49.9** | 51.2 | 1.03x |
| vibejson-heap | n/a | **yes** | 59.5 | **60.4** | 61.5 | 1.03x |
| badger | false | no | 74.9 | **76.0** | 78.9 | 1.05x |
| badger | true | no | 107.4 | **114.3** | 126.6 | 1.18x |
| bbolt | false | no | 107.4 | **238.2** | 388.3 | 3.6x |
| bbolt | true | no | 248.6 | **1481** | 2632.6 | 10.6x |
| pebble | false | no | 81.3 | **958.3** | 2273.9 | 28x |
| pebble | true | no | 1476.8 | **1844** | 2393.2 | 1.6x |
| vibejson-durable | false | no | 703.8 | **1046** | 2069.4 | 2.9x |
| vibejson-durable | false | **yes** | 467.4 | **907.6** | 1611.6 | 3.4x |
| vibejson-durable | true | no | 1154.9 | **2091** | 2510.4 | 2.2x |
| vibejson-durable | true | **yes** | 541.5 | **580.4** | 624.7 | 1.15x |
| sqlite | false | no | 1001.5 | **12,682** | 23,668 | 24x |
| sqlite | false | **yes** | 3530.1 | **15,159** | 24,753 | 7.0x |
| sqlite | true | no | 1000.6 | **10,761** | 19,186 | 19x |
| sqlite | true | **yes** | 1017.4 | **1083** | 1667.7 | 1.6x |

The only rows stable enough to read are `vibejson-heap`'s and `badger`'s.

### Index build cost — this one is solid

`vibejson-heap`'s four rows have a 3% spread and are the tightest measurement in
this file, so the index-build cost is readable straight off them:

| | Unindexed | Indexed | Cost |
| --- | ---: | ---: | ---: |
| vibejson-heap bulk load | 49.9 ms | 60.4 ms | **+21%** |

That is the number that used to be charged to nobody: `BenchmarkBulkLoad` always
passed `Indexed:false`, so the index whose benefit `BenchmarkIndexedFilter`
reports — a 64.6 µs indexed count against a 7.9 ms unindexed filter, a 122x win
— had no build cost anywhere in the report. It is not free, and 21% of a bulk
load is the honest price on this corpus. The equivalent figures for
`store/durable` and SQLite cannot be read off this run; their rows are in the
contaminated set.

### Point write

One replacement of one existing document. Medians of `-count=6`, each
repetition on its own freshly built store. These rows are far more stable than
the bulk-load rows above — most have a max/min spread under 1.3x — but they are
still disk-bound and were still taken alongside another tenant.

| Engine | sync=false | sync=true | Durability actually bought at sync=true |
| --- | ---: | ---: | --- |
| badger | 5.2 µs | 223 µs | `msync(MS_SYNC)` only — **not** `F_FULLFSYNC` |
| pebble | 14.9 µs | 12.24 ms | `F_FULLFSYNC` |
| sqlite | 15.0 µs | 8.09 ms | `F_FULLFSYNC` (`fullfsync=1`) |
| bbolt | 18.8 µs | 19.59 ms | `F_FULLFSYNC` |
| **vibejson-durable** | **320 µs** | **18.31 ms** | `F_FULLFSYNC` |

In-memory, for the separate table it belongs in:

| Engine | Put |
| --- | ---: |
| vibejson-heap | 2.9 µs |

- **Badger's `sync=true` figure is not comparable and must never be quoted
  beside the others.** It is 37x faster than the next engine because it is doing
  something weaker: its log files are mmapped and its sync is
  `unix.Msync(MS_SYNC)`, which pushes dirty pages to the filesystem without
  forcing the drive to flush its write cache. It exposes no way to request
  `F_FULLFSYNC`.
- **The four `F_FULLFSYNC` engines do not cluster.** They span 8.09 to 19.59 ms
  — a 2.4x range, with SQLite at one end and bbolt at the other, and
  `store/durable` next to bbolt. A previous version of the harness's README
  described bbolt, Pebble, and `store/durable` as "all in the 4-9 ms range"
  when two of them are the ends of the distribution. There is no range to quote
  here; quote the rows.
- **`store/durable`'s buffered write is 17x slower than bbolt's and 61x slower
  than Badger's**, at 320 µs against 18.8 and 5.2. That is the largest single
  gap anywhere in this report and it is not a durability difference — every
  engine in that column is buffered.

### store/durable's own tuning, measured

`BenchmarkPointWriteDurableDefaults`, medians of `-count=6`:

| Configuration | Put |
| --- | ---: |
| `BufferCount=1024` (this harness's tuning) | 239 µs |
| `store/durable` defaults | 537 µs |
| | **2.25x** |

**This does not reproduce the justification it was given.** The tuning is
documented — in the harness and in `store/durable`'s `Tuning()` string — as
buying roughly a 25-35x faster `Put`, and that is why it is considered worth
about 30 MiB of extra off-heap write buffers. Measured here, on this tree, it
buys 2.25x. Either the default changed under the in-flight `store/durable` work,
or the original figure was taken under conditions this run does not reproduce.
Until that is resolved, `BufferCount=1024` is 30 MiB spent for a 2.25x return
and the harness is giving vibejson a tuning whose stated payoff it cannot
demonstrate. It is smaller than the tuning correction owed to Badger (3.81x).

## 5. Tuning corrections

`BenchmarkTuning`, medians of `-count=6`. Each pair is the same engine, the same
fixture, the same workload, with `Config.Untuned` reverting one call-shape
choice this harness makes on that engine's behalf.

vibejson's own tuning is the reason this section has to exist:
`store/durable` is given `BufferCount=1024` — about 30 MiB of extra off-heap
write buffers — because its default was 25x slower on a serial writer. Two
competitors had symmetric pathologies sitting on their defaults and were being
published with them.

| Engine | Workload | Defaults | Tuned | Effect |
| --- | --- | ---: | ---: | ---: |
| badger | scan | 403.1 ns/doc | 105.8 ns/doc | **3.81x faster** |
| bbolt | point read | 645 ns | 416 ns | **-35%** |
| badger | point read | 1,298 ns | 983 ns | **-24%** |
| sqlite | scan | 222.2 ns/doc | 185.9 ns/doc | **-16%** |

Allocation, same pairs:

| Engine | Workload | Defaults | Tuned |
| --- | --- | ---: | ---: |
| badger | scan | 2.33 MB / 103,100 | 8.45 kB / 41 |
| bbolt | point read | 576 B / 9 | 168 B / 3 |
| badger | point read | 932 B / 9 | 804 B / 8 |
| sqlite | scan | 53.3 MB / 300,015 | 27.4 MB / 200,017 |

What each correction is:

- **Badger `IteratorOptions.PrefetchValues`.** Default true. Its prefetch worker
  pool resolves each item's value ahead of the cursor, which pays for large
  values living in the value log and does not pay for ~250-byte values already
  inline in the LSM tables. On the default, Badger was the slowest engine in the
  scan row. It is not. This was the single largest competitor-side distortion in
  the harness.
- **bbolt and Badger point reads open a transaction per `Get`.** Both engines'
  idiomatic spelling is a `db.View`/`db.Update` closure. `store/durable`'s
  `AppendRaw` takes no transaction at all, so charging them per-`Get`
  transaction setup measured a difference the two APIs do not have. The harness
  now holds one read transaction (and, for bbolt, resolves the bucket handle
  once) and drops it before any write, which both engines require. **The
  Badger half of this was not in the audit's list.**
- **SQLite `sql.RawBytes`.** Scanning into `[]byte` makes `database/sql` copy
  every row before the loop body sees it. `sql.RawBytes` borrows the driver's
  buffer. Two of the three allocations per row were the harness's fault; the
  remaining two per row are the driver's and cannot be removed from here.

### Checked and rejected

Recorded so nobody re-litigates them as obvious wins. Both are also in the
relevant `Tuning()` strings.

| Hypothesis | Result |
| --- | --- |
| Pebble bloom filter (`bloom.FilterPolicy(10)`, all levels) | 1284 -> 1441 ns. No help, a small loss. A bloom filter can only save reading a table that does not contain the key, and every key this harness probes exists. |
| SQLite `PRAGMA mmap_size(268435456)` | 3795 -> 3949 ns. No help. `modernc.org/sqlite`'s VFS does not implement the memory-mapped read path the pragma exists to enable. |

SQLite is competently configured. `EXPLAIN QUERY PLAN` was checked and reports
`SEARCH docs USING INDEX idx_country` for the indexed query and `SCAN docs` for
the `json_extract` one, so neither row is measuring the wrong plan.

## 6. Bulk path against mutation replay

`BenchmarkBulkLoadVariants`, ns per document, medians of `-count=6`.

```sh
go test -run '^$' -bench=BenchmarkBulkLoadVariants -count=6 -timeout=180m .
```

| Variant | n=1,000 | n=5,000 | n=100,000 |
| --- | ---: | ---: | ---: |
| bulk, tuned | 40,621 | 17,580 | **11,027** |
| Put loop, tuned | 1,093,490 | 410,373 | **430,682** |
| Put loop, `store/durable` defaults | 1,261,700 | — | — |
| **replay penalty** | **26.9x** | **23.3x** | **39.1x** |

### ns/doc was asserted to be size-stable at 5,000. It is not.

This benchmark used to run a single 5,000-document build and carry a comment
saying "ns/doc is the comparable figure and it does not need the full corpus to
be stable". Measured:

- The bulk builder's ns/doc **falls 1.59x** between n=5,000 and n=100,000, and
  3.7x between n=1,000 and n=100,000. It has large per-build fixed costs — a
  shape template, a value dictionary, a key directory — and they amortise across
  the corpus. A 5,000-document ns/doc figure overstates the bulk path's cost by
  more than half.
- The **replay penalty measured at 5,000 understates the real one by 1.7x**
  (23.3x against 39.1x), because the two paths amortise differently. That is the
  headline number of this benchmark, and the size it was measured at was the
  size that made it look smallest.

The Put loop's own ns/doc is size-stable between 5,000 and 100,000 (within 5%),
which is presumably where the original assumption came from — it happens to hold
for the path it was not the interesting measurement of.

### The untuned Put loop

`putloop-defaults` at n=1,000 is 1.15x slower than `putloop-tuned` at the same
size. Read that together with section 4's 2.25x: the `BufferCount=1024` tuning
does not reproduce the 25-35x it is documented as buying. See section 4.

## 7. Run mode

Read figures are taken **one engine per process**. This section measures why.

`BenchmarkPointRead`, medians of `-count=6`, two runs back to back in the same
conditions. `-coresident` is the old behaviour: every engine's fixture
accumulates in `Factories()` order and none is ever released. The default now
releases each engine's fixture before loading the next.

```sh
go test -run '^$' -bench=BenchmarkPointRead -coresident -count=6 .
go test -run '^$' -bench=BenchmarkPointRead              -count=6 .
```

| Engine | all fixtures resident | fixtures released | change | working set lives |
| --- | ---: | ---: | ---: | --- |
| vibejson-heap | 163.6 ns | 152.2 ns | **-7%** | Go heap |
| vibejson-durable | 1,405 ns | 1,337 ns | **-5%** | Go heap + page cache |
| bbolt | 376.4 ns | 410.9 ns | **+9%** | mmap |
| badger | 958 ns | 1,027 ns | **+7%** | mmap |
| pebble | 564.9 ns | 631.1 ns | **+12%** | off-heap block cache |
| sqlite | 2,870 ns | 2,980 ns | **+4%** | own page allocator |

**The bias does not cancel, because it has opposite signs for the two sides of
the comparison.** Releasing foreign fixtures makes both vibejson engines faster
and all four competitors slower, and the split is exactly along whether the
engine's working set is on the Go heap. The vibejson engines are the ones the
garbage collector has to scan past, so they gain when the other five megabytes
of retained fixtures go away; the competitors keep their data in mmaps and
off-heap arenas that the GC never touches, so they gain nothing from the
release and pay the reload churn that comes with it. Choosing a run mode
therefore moves vibejson and its competitors in opposite directions, which is
the worst possible shape for a measurement artefact in a comparison.

The magnitudes here (5-12%) are smaller than the 31% the audit that prompted
this work reported for `vibejson-heap`. `vibejson-heap`'s point read is the
least reproducible figure in this whole file — see below — so both are probably
true readings of different machine states, and the sign split is the durable
finding rather than the size.

### The one figure that would not settle

`BenchmarkPointRead/vibejson-heap` measured 234 ns in the per-engine isolated
run, 164 ns co-resident, and 152 ns with fixtures released — across runs whose
internal spreads were 3-10%. Every other engine reproduced across all three
runs to within 10%:

| Engine | isolated | co-resident | released |
| --- | ---: | ---: | ---: |
| **vibejson-heap** | **234** | **164** | **152** |
| bbolt | 405 | 376 | 411 |
| pebble | 630 | 565 | 631 |
| badger | 1,004 | 958 | 1,027 |
| sqlite | 3,009 | 2,870 | 2,980 |
| vibejson-durable | 1,696 | 1,405 | 1,337 |

It is the smallest number in the table, it probes 100,000 documents in a random
permutation, and it is the one figure whose working set is small enough to live
or die by what else is in the last-level cache. Quote it as **150-235 ns** or
re-measure it on a quiet machine; do not quote a point estimate.

### Reproducing the isolated read figures

```sh
for e in $(/tmp/footprint -list); do
  go test -run '^$' -count=6 -timeout=180m \
    -bench="BenchmarkPointRead/$e\$|BenchmarkScan/$e\$|BenchmarkScanAllBytes/$e\$|BenchmarkFilter/$e\$|BenchmarkIndexedFilter/$e\$" .
done
```

A full `-bench=.` run in one process is still supported and is much closer to
the isolated figures than it used to be, because the harness now releases every
engine's fixture before loading the next. `-coresident` restores the old
accumulate-everything behaviour if you want to measure the effect itself.

Write figures come from a single run; each `-count` repetition builds its own
store, so they do not need process isolation.

```sh
go test -run '^$' -bench='BenchmarkBulkLoad$|BenchmarkPointWrite|BenchmarkBulkLoadVariants|BenchmarkTuning' \
  -count=6 -timeout=180m . | tee bench.txt
```

Collapse any run to per-benchmark medians with:

```sh
awk '/^Benchmark/ {
  name = $1; sub(/-[0-9]+$/, "", name)
  for (i = 3; i < NF; i += 2) { k = name "\t" $(i+1); n = ++c[k]; v[k, n] = $i + 0 }
}
END {
  for (k in c) {
    n = c[k]
    for (a = 1; a <= n; a++) for (b = a+1; b <= n; b++)
      if (v[k,a] > v[k,b]) { t = v[k,a]; v[k,a] = v[k,b]; v[k,b] = t }
    printf "%s\t%.4g\n", k, (n % 2) ? v[k,(n+1)/2] : (v[k,n/2] + v[k,n/2+1]) / 2
  }
}' bench.txt | sort
```
