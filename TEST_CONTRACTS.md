# Test contracts

This is a machine-checked maintenance ledger. Every tracked test file has one
primary contract, every fuzz target has one campaign, and every checked-in fuzz
seed is reconciled with `testdata/FUZZ_CORPUS.json`.

Run:

```sh
go run ./internal/cmd/testcontracts -check
```

## Contract matrix

| ID | Primary contract |
| --- | --- |
| `SYN` | JSON syntax, depth, and exact-document acceptance |
| `STR` | UTF-8, escaping, keys, and string boundaries |
| `NUM` | Number grammar, conversion, and formatting |
| `DEC` | Typed decoding and destination semantics |
| `ENC` | Typed and dynamic encoding |
| `HOOK` | Standard and native method dispatch |
| `DOC` | Documents, pointers, stores, persistence, TTL, and indexes |
| `STREAM` | Framing, cursors, reader lifecycle, and writer state |
| `XFORM` | Compact, indent, canonicalize, and token output |
| `OWN` | Borrowing, GC visibility, invalidation, and concurrency |
| `RES` | Allocation, scratch, cache, and retained resources |
| `ROUTE` | Portable, specialized, and SIMD route parity |
| `API` | Exported construction, errors, zero values, and examples |
| `PERF` | Benchmarks and generated-code performance evidence |
| `TOOL` | Generators, repository checks, and build helpers |

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
contains_contract_test.go
docset_persist_test.go
docset_postings_test.go
docset_test.go
docset_valuedict_test.go
duplicate_keys_contract_test.go
internal/orderedkey/key_test.go
internal/storeio/committer_test.go
internal/storeio/chunk_directory_test.go
internal/storeio/chunk_tree_test.go
internal/storeio/device_test.go
internal/storeio/document_group_test.go
internal/storeio/document_page_test.go
internal/storeio/float64_group_test.go
internal/storeio/float64_scan_test.go
internal/storeio/free_directory_test.go
internal/storeio/free_tree_test.go
internal/storeio/generation_leases_test.go
internal/storeio/index_group_catalog_test.go
internal/storeio/index_directory_test.go
internal/storeio/index_pool_test.go
internal/storeio/key_directory_test.go
internal/storeio/key_tree_test.go
internal/storeio/overflow_page_test.go
internal/storeio/page_test.go
internal/storeio/page_cache_test.go
internal/storeio/page_key_directory_test.go
internal/storeio/posting_page_test.go
internal/storeio/state_root_test.go
internal/storeio/superblock_test.go
internal/storeio/ttl_directory_test.go
internal/storeio/write_transaction_test.go
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
query/parse_test.go
query/file_execute_test.go
query/plan_test.go
query/postings_test.go
query/query_test.go
query/store_test.go
raw_trusted_test.go
shape_column_test.go
shape_column_typed_test.go
shape_test.go
store_test.go
store_builder_test.go
store_file_bulk_test.go
store_file_group_test.go
store_file_linux_test.go
store_file_reliability_test.go
store_file_test.go
store_float64_reduce_test.go
store_bitmap_test.go
store_index_exact_test.go
store_index_packed_test.go
store_persist_test.go
store_schema_test.go
store_page_db_test.go
store_page_file_test.go
internal/storeio/ring_linux_test.go
value_accessors_contract_test.go
value_iteration_contract_test.go
value_spans_contract_test.go
verify_exhaustive_test.go
verify_invariants_test.go
```

### `STREAM`

```text
docset_shape_test.go
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
internal/storemem/block_test.go
ownership_lifetime_test.go
store_mapped_keys_test.go
store_owned_documents_test.go
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
query/workspace_test.go
resource_retention_test.go
internal/storeio/committer_alloc_test.go
internal/storeio/chunk_directory_alloc_test.go
internal/storeio/ring_alloc_linux_test.go
internal/storeio/device_alloc_test.go
internal/storeio/document_page_alloc_test.go
internal/storeio/key_directory_alloc_test.go
internal/storeio/posting_page_alloc_test.go
internal/storeio/state_root_alloc_test.go
internal/storeio/superblock_alloc_test.go
store_file_physical_linux_test.go
store_file_scale_smoke_test.go
```

### `ROUTE`

```text
decoder_structural_test.go
route_differential_test.go
stage2_scalar_differential_test.go
simd/features_simd_test.go
internal/scanner/scan_policy_amd64_test.go
internal/scanner/scan_simd_test.go
internal/storeio/page_checksum_simd_amd64_test.go
internal/storeio/page_checksum_simd_arm64_test.go
internal/kernels/stage1_index_portable_test.go
internal/kernels/stage1_index_test.go
internal/kernels/stage1_portable_test.go
internal/kernels/stage1_stream_test.go
internal/kernels/stage1_test.go
internal/kernels/stage2_index_test.go
internal/bitset/ops_dispatch_amd64_test.go
internal/bitset/ops_test.go
```

### `API`

```text
api_external_contract_test.go
example_test.go
parity_test.go
race_off_test.go
race_on_test.go
stream_decode_test.go
store_example_test.go
query/value_type_test.go
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
benchmarks/native_corpus_bench_test.go
benchmarks/stage2_machine_bench_test.go
benchmarks/stdlib_corpus_bench_test.go
benchmarks/typed_bench_test.go
internal/scanner/scan_backend_bench_test.go
tests/stdlib/corpus_bench_test.go
```

### `TOOL`

```text
internal/cmd/testcontracts/main_test.go
internal/cmd/unsafeinventory/main_test.go
simd/release_window_test.go
test_budget_test.go
```

## Mixed files to separate

This heading is the parser boundary for the primary map. Files may exercise
secondary contracts; each still has exactly one owner above.

## Fuzz target ownership

The verifier discovers these targets from Go test files. Adding, deleting, or
renaming one requires updating the table in the same change.

| Package | Target | Campaign |
| --- | --- | ---: |
| `./` | `FuzzContains` | 6 |
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

This generated block must match `testdata/FUZZ_CORPUS.json`.

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
