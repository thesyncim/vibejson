# Test contracts

This matrix is the contract-first index for test consolidation. It describes
what must remain observable while files, fixtures, and fuzz entry points are
merged. A test is retained because it protects a distinct invariant, not
because it records the implementation incident that created it.

## Contract matrix

| ID | Contract | Oracle | Deterministic suite | Property campaign | Safety modes | CI tier |
| --- | --- | --- | --- | --- | --- | --- |
| `SYN` | Complete JSON syntax, depth, and exact-document acceptance | `encoding/json`, JSONTestSuite, scalar grammar | Valid/invalid grammar families, truncations, depth boundaries, corpus | Consolidated syntax/API parity | Portable/SIMD, race, checkptr | PR and weekly scheduled |
| `STR` | UTF-8, escapes, strings, keys, and boundary handling | `encoding/json`, Unicode rules, scalar scanner | Escape alignments, exhaustive UTF-8, key matching | String/scanner parity | Portable/SIMD, checkptr | PR and weekly scheduled |
| `NUM` | Number grammar, exact conversion, and formatting | `strconv`, `encoding/json` | Boundary spellings, Eisel-Lemire fallbacks, digit widths | Number parse/format parity | Portable/SIMD | PR and weekly scheduled |
| `DEC` | Typed decode selection, merge/replace, fields, and destination reuse | `encoding/json` | Type/field matrix and reuse sequences | Typed decode parity | Race, checkptr, portable/SIMD | PR |
| `ENC` | Typed and dynamic encode acceptance, bytes, tags, ordering, and errors | `encoding/json` | Value/type matrix and error paths | Encode parity and round-trip | Race, checkptr, hook validation | PR |
| `HOOK` | Standard and native method dispatch, output integrity, and receiver semantics | `encoding/json` plus documented native-hook contract | Addressability, error, panic, and retention matrix | Hook integrity/operation sequence | Race, checkptr, forced GC | PR and weekly scheduled |
| `DOC` | `RawValue`, `Value`, `Index`, `Node`, iterators, duplicate keys, and JSON Pointer | RFC 6901, generic traversal, `encoding/json` materialization | Navigation/accessor/iteration matrix | Pointer/index/navigation parity | Portable/SIMD, checkptr | PR and weekly scheduled |
| `STREAM` | Framing, fragmentation, reader state, cursors, offsets, limits, and writer state | `json.Decoder`, `json.Encoder`, scalar framer, state model | Fragmented fixtures and operation sequences | Stream state-machine parity | Race, portable/SIMD | PR and weekly scheduled |
| `XFORM` | Compact, indent, canonicalize, and token-writer output | `json.Compact`, `json.Indent`, documented ordering | Formatting and writer-state matrix | Transform parity | Portable/SIMD | PR |
| `OWN` | Borrowing, mutation, GC visibility, concurrency, and invalidation | Documented ownership model | Lifetime/mutation/concurrency matrix | Cross-GC operation sequences | Race, checkptr, `GOGC=1` | PR and release |
| `RES` | Allocation ceilings, cache growth, scratch clearing, and retained-byte budgets | Documented byte/allocation budgets | Huge-then-small and poison matrices | Resource operation sequences | Forced GC, race exclusions for allocation assertions | Weekly scheduled and release |
| `ROUTE` | Generic/specialized and portable/SIMD route equivalence | Generic portable implementation | Forced-route corpus and malformed cases | SIMD/scalar/route parity | Portable/SIMD, checkptr | PR and weekly scheduled |
| `API` | Exported construction, zero values, errors, and examples | Package documentation and Go API conventions | External-package examples and contract tests | Covered through domain campaigns | Normal PR matrix | PR |
| `PERF` | Latency, throughput, allocations, compile cost, binary size, and retained memory | Fixed baseline and interleaved `benchstat` | Named benchmark families | Not fuzzed | Hosted ARM64/amd64 comparisons; dedicated ARM64 hard gate | PR signal; manual hard gate |
| `TOOL` | Generators, publishers, corpus provenance, and CI helpers | Reproducible checked-in output | Tool unit tests and clean-tree checks | Not fuzzed | Pinned toolchain | PR |

## Invariant naming rule

Every retained deterministic test must be expressible as:

```text
Given <public or internal precondition>, when <operation>, then <one observable
postcondition> agrees with <oracle>, under <safety modes>.
```

Table-driven subtests may cover many inputs for one invariant. Two tests that
have the same precondition, operation, postcondition, oracle, and safety modes
are duplicates even if they were introduced for different bugs.

Performance assertions are not semantic postconditions. `AllocsPerRun`, exact
object/cache-line sizes, retained bytes, compile time, and latency belong to
`RES` or `PERF`, not to `SYN`, `DEC`, or `ENC` tests.

## Primary file map

This is the initial file-level map. Each tracked or unignored test file appears
once under its primary contract, including new files before they are staged.
Files marked as mixed below must be split or have their non-primary tests
migrated before line-reduction work claims completion.

### `SYN`

```text
parser_test.go
truncation_test.go
valid_bitmap_test.go
valid_differential_test.go
validation_corpus_test.go
tests/stdlib/corpus_test.go
```

### `STR`

```text
boundary_escape_test.go
utf8_exhaustive_test.go
valid_bitmap_utf8_test.go
internal/scanner/scan_test.go
```

### `NUM`

```text
float_eisel_test.go
float_fuzz_extra_test.go
number_corpus_test.go
number_digits_test.go
number_exactness_test.go
number_float_contract_test.go
number_float_differential_test.go
number_rejection_contract_test.go
simd/digits_test.go
simd/float_test.go
simd/time_test.go
```

### `DEC`

```text
decoder_replace_null_test.go
decoder_trust_test.go
field_matching_contract_test.go
typed_packed_whitespace_test.go
typed_slice_contract_test.go
typed_test.go
internal/typedtest/model_test.go
```

### `ENC`

```text
encoder_addressable_test.go
encoder_compatibility_test.go
encoder_cycle_pre_go127_test.go
encoder_hardening_test.go
encoder_test.go
inline_test.go
```

### `HOOK`

```text
marshaler_test.go
typed_hook_alloc_test.go
typed_hook_corruption_test.go
typed_hook_fuzz_test.go
typed_hook_retention_test.go
typed_hook_safety_test.go
typed_hook_test.go
```

### `DOC`

```text
any_test.go
docset_test.go
duplicate_keys_contract_test.go
field_cursor_test.go
index_bitmap_test.go
index_contract_helpers_test.go
index_flat_test.go
index_keyhash_test.go
index_probe_test.go
index_tapescan_test.go
intern_test.go
lazy_navigation_contract_test.go
lazy_scalar_contract_test.go
lazy_test.go
pointer_rfc6901_test.go
raw_trusted_test.go
shape_column_test.go
shape_column_typed_test.go
shape_test.go
value_accessors_contract_test.go
value_iteration_contract_test.go
value_spans_contract_test.go
```

### `STREAM`

```text
docset_stream_test.go
reader_differential_test.go
reader_io_contract_test.go
reader_lifecycle_test.go
stream_cursor_test.go
stream_frame_boundary_test.go
stream_frame_test.go
stream_fuzz_test.go
stream_test.go
writer_contract_test.go
```

### `XFORM`

```text
formatting_contract_test.go
```

### `OWN`

```text
any_box_corruption_test.go
concurrency_corruption_test.go
encoder_lifetime_test.go
gc_corruption_test.go
gc_lifetime_test.go
internal/byteview/byteview_test.go
ownership_lifetime_test.go
typed_slice_words_test.go
```

### `RES`

```text
cache_policy_test.go
decoder_arena_test.go
decoder_scratch_test.go
decoder_structural_retention_simd_test.go
decoder_structural_retention_test.go
encoder_heterogeneous_scratch_test.go
encoder_scratch_poison_test.go
encoder_scratch_test.go
encoder_sequence_fuzz_test.go
marshal_hint_test.go
resource_retention_test.go
```

### `ROUTE`

```text
decoder_structural_test.go
route_differential_test.go
stage2_scalar_differential_test.go
simd/features_simd_test.go
internal/scanner/scan_policy_amd64_test.go
internal/scanner/scan_simd_test.go
internal/kernels/stage1_index_portable_test.go
internal/kernels/stage1_index_test.go
internal/kernels/stage1_portable_test.go
internal/kernels/stage1_stream_test.go
internal/kernels/stage1_test.go
internal/kernels/stage2_index_test.go
```

### `API`

```text
api_external_contract_test.go
example_test.go
parity_test.go
race_off_test.go
race_on_test.go
stream_decode_test.go
```

### `PERF`

```text
docset_stream_bench_test.go
index_tapescan_bench_test.go
multidoc_bench_test.go
parser_bench_test.go
portable_backend_bench_test.go
shape_bench_test.go
shape_column_bench_test.go
shape_column_typed_bench_test.go
typed_bench_test.go
typed_hook_bench_test.go
benchmarks/bench_test.go
benchmarks/benchmark_corpus_test.go
benchmarks/legacy/bench_test.go
benchmarks/legacy/stdlib_corpus_bench_test.go
benchmarks/legacy/stdlib_models_test.go
benchmarks/lookup_competitors_bench_test.go
benchmarks/native_corpus_bench_test.go
benchmarks/stage2_machine_bench_test.go
benchmarks/stdlib_corpus_bench_test.go
benchmarks/stdlib_corpus_jsonv2_bench_test.go
benchmarks/typed_bench_test.go
benchmarks/typed_jsonv2_bench_test.go
internal/scanner/scan_backend_bench_test.go
tests/stdlib/corpus_bench_test.go
```

### `TOOL`

```text
internal/cmd/benchpublish/charts_test.go
internal/cmd/benchpublish/main_test.go
internal/cmd/testcontracts/main_test.go
internal/cmd/unsafeinventory/main_test.go
benchmarks/crosslang/go_contract/main_test.go
simd/release_window_test.go
test_budget_test.go
```

## Mixed files to separate

The primary map does not bless mixed concerns. These are the first split or
migration candidates:

| File | Primary | Other contracts currently mixed in |
| --- | --- | --- |
| `parser_test.go` | `SYN` | `DOC`, `OWN`, `RES`, `XFORM` |
| `typed_test.go` | `DEC` | `OWN`, `RES`, `PERF` |
| `encoder_test.go` | `ENC` | `DEC`, `NUM`, `RES`, `PERF` |
| `inline_test.go` | `ENC` | `DEC`, `OWN`, `RES`, `PERF` |
| `stream_test.go` | `STREAM` | `OWN`, `RES`, `PERF` |
| `ownership_lifetime_test.go` | `OWN` | `DEC`, `DOC`, `STREAM`, `RES` |
| `concurrency_corruption_test.go` | `OWN` | `ENC`, `DEC`, `STREAM`, `HOOK` |
| `benchmarks/bench_test.go` | `PERF` | fixture correctness checks |

When a mixed file is touched, move allocation/layout checks to an
`invariant_*_test.go` or benchmark file and keep semantic behavior in a
`*_contract_test.go` file.

## Target property campaigns

The 34 baseline fuzz targets consolidate toward these ten campaigns:

1. Syntax, validation, parser, index, and exact-document parity.
2. Typed decode, merge/replace, and route parity.
3. Encode parity, round-trip, inline behavior, and malformed-output rejection.
4. Number grammar, parse, format, and digit-kernel parity.
5. UTF-8, escape, string decoding, field matching, and scanner parity.
6. JSON Pointer, index storage, accessor, and navigation parity.
7. Stream fragmentation, framing, cursor, reader-lifecycle, and operation state.
8. Hook integrity, dispatch, recovery, and receiver lifetime.
9. Scratch, cache, and retained-resource operation sequences.
10. Portable versus SIMD kernel and forced-route parity.

Consolidation is complete only when every removed target's source `f.Add`
values, checked-in `testdata/fuzz` files, and known crash artifacts are migrated
to a retained campaign.

## Fuzz target ownership

This table makes the current target set and its destination campaign explicit.
The verifier discovers targets from tracked or unignored Go test files, so
adding, deleting, or renaming a target requires an ownership update in the
same change.

| Package | Target | Campaign |
| --- | --- | ---: |
| `./` | `FuzzDecodeTrust` | 2 |
| `./` | `FuzzEncoderMatchesStdlib` | 3 |
| `./` | `FuzzEncoderScratchOperationSequence` | 9 |
| `./` | `FuzzFieldSetLookupParity` | 5 |
| `./` | `FuzzFloatExactness` | 4 |
| `./` | `FuzzFloatRoundTripMarshalDecode` | 4 |
| `./` | `FuzzHookContracts` | 8 |
| `./` | `FuzzIndexNavigation` | 6 |
| `./` | `FuzzReaderLifecycleOperations` | 7 |
| `./` | `FuzzScanFirstRawTrusted` | 6 |
| `./` | `FuzzStreamReaderChunkEquivalence` | 7 |
| `./` | `FuzzStructuralRouteParity` | 10 |
| `./internal/scanner` | `FuzzSIMDScannersMatchScalar` | 10 |

## Corpus migration ledger

The baseline disk corpus has six files. The manifest links every current seed
to one immutable baseline path; the verifier requires exactly one descendant
per baseline file. Retained entries remain byte-for-byte at their original path
and target, while migrated entries move to a different live target.

<!-- BEGIN GENERATED FUZZ CORPUS LEDGER -->
<!-- Generated from testdata/FUZZ_CORPUS.json by internal/cmd/testcontracts. -->
| Origin | Baseline corpus file | Current owner | Current corpus file | Bytes | SHA-256 | Status |
| --- | --- | --- | --- | ---: | --- | --- |
| `./::FuzzDecodeTrust` | `testdata/fuzz/FuzzDecodeTrust/33fbec441b3db369` | `./::FuzzDecodeTrust` | `testdata/fuzz/FuzzDecodeTrust/33fbec441b3db369` | 487 | `33fbec441b3db3690f33b0f7651d921697ecd5b26acf248d93495f210c70e7da` | retained |
| `./::FuzzDecodeTrust` | `testdata/fuzz/FuzzDecodeTrust/64a82bc7ef2bb22e` | `./::FuzzDecodeTrust` | `testdata/fuzz/FuzzDecodeTrust/64a82bc7ef2bb22e` | 37 | `64a82bc7ef2bb22e9a8e28169069f0867641ea40ded9bf751e4ae1ae6de69a6f` | retained |
| `./::FuzzDecodeTrust` | `testdata/fuzz/FuzzDecodeTrust/e26729a06ef9d1a0` | `./::FuzzDecodeTrust` | `testdata/fuzz/FuzzDecodeTrust/e26729a06ef9d1a0` | 41 | `e26729a06ef9d1a048d325eb4d8003e610d0b748cee0b211e7ae9154f85913f5` | retained |
| `./::FuzzAPIConsistency` | `testdata/fuzz/FuzzAPIConsistency/edde7e36d1440ed3` | `./::FuzzDecodeTrust` | `testdata/fuzz/FuzzDecodeTrust/edde7e36d1440ed3` | 45 | `edde7e36d1440ed3067f1be13205a9275c5a85a6d840d318d70012b03c044336` | migrated |
| `./::FuzzTransforms` | `testdata/fuzz/FuzzTransforms/225ded3f35fa5a00` | `./::FuzzEncoderMatchesStdlib` | `testdata/fuzz/FuzzEncoderMatchesStdlib/225ded3f35fa5a00` | 41 | `225ded3f35fa5a0027b8efdb4994befb05fa1ea1f17b7fe4f83e7fd5c82e6372` | migrated |
| `./::FuzzStreamFramerAdversarial` | `testdata/fuzz/FuzzStreamFramerAdversarial/26eccc8f27f3228a` | `./::FuzzStreamReaderChunkEquivalence` | `testdata/fuzz/FuzzStreamReaderChunkEquivalence/3c91f2efc37fbf50` | 86 | `3c91f2efc37fbf5087f8b19afc22e720ab7e4e80fb10f503c3abae27b60a36b4` | migrated |
<!-- END GENERATED FUZZ CORPUS LEDGER -->
