# Unsafe-code inventory

The default build contains bounded uses of `unsafe`; there is no alternate
safety mode. The uses fall into a small number of invariant families described
below. The generated scope list that follows is exhaustive for production Go
files and is checked in CI.

All unsafe changes require review by `@thesyncim`. A reviewer checks the
ordinary Go reference path first, then the bounds and layout proof, then
ownership and GC visibility, and finally the named correctness and performance
gates. Passing a benchmark is not a substitute for any earlier check.

## Invariant families

| Area and files | Why unsafe exists | Bounds and layout invariant | Ownership and lifetime invariant | Required tests | Performance contract |
| --- | --- | --- | --- | --- | --- |
| Borrowed and lazy views: `x/byteview/byteview.go`, `node.go`, `raw.go`, `string_views.go` | Centralize allocation-free read-only byte/string views and navigate compact index entries without reflection or allocation. | Every view preserves its source length, and every source range comes from a validated token or checked index entry. `IndexEntry` has compile-time size and offset assertions. Empty inputs never dereference a nil base. | Byte views of strings are read-only. Callers keep borrowed storage alive and immutable for the returned view's lifetime. Value-derived `Node` pointers keep owned source and entry arrays visible to the collector. Index-derived nodes, `RawValue`, zero-copy results, and stream values borrow documented caller or reader storage. Go pointers remain typed pointers; none are stored as `uintptr`. | `ownership_lifetime_test.go`, `gc_lifetime_test.go`, `lazy_navigation_contract_test.go`, `parser_test.go`, `reader_lifecycle_test.go`, `route_differential_test.go` | `BenchmarkParse`, `BenchmarkBuildIndex`, `BenchmarkGetRaw`, `BenchmarkStreamReadNDJSON`, `BenchmarkStreamDynamicWalk` |
| Compiled decoding and hooks: `decoder_cursor.go`, `decoder_structural.go`, `typed.go`, `typed_compiled*.go`, `typed_hook*.go`, `typed_reset.go`, decode paths in `marshaler.go` | Execute a reflect-compiled type plan directly against typed destinations and call user methods with ordinary receiver semantics. | Field offsets and element strides come from `reflect.Type`; slice growth occurs before element addressing. Structural positions are validated before typed loads, and scalar fallbacks preserve the same grammar. | Destination pointers stay visible to the runtime. Native hook cursors cross user code by value; receivers are heap-backed or caller-owned according to normal Go method rules. Temporary decoded strings follow the documented owned or zero-copy mode. | `typed_test.go`, `typed_hook_safety_test.go`, `typed_hook_retention_test.go`, `gc_corruption_test.go`, `route_differential_test.go`, `decoder_structural_test.go` | `BenchmarkDecodeLargeReused`, `BenchmarkDecodeLargeIndentedReused`, `BenchmarkHookDecodeSmall`, `BenchmarkFieldSetLookup` |
| Compiled encoding: `encoder_execute*.go`, `encoder_int.go`, `encoder_string.go`, encode paths in `marshaler.go` | Walk a compiled type plan with reflection confined to dynamic storage and type boundaries. SWAR stores format short integers and strings. | Addresses use reflect-derived sizes and live slice or array bounds. Fixed-width loads and stores are guarded by remaining-length checks. Scratch slots retain their concrete pointer-bearing type and oversized backing is discarded. | Source pointers remain GC-visible for the complete call. User methods receive stable, legally retainable receivers. Pooled scratch does not retain caller values or reinterpret heterogeneous pointer layouts. | `encoder_lifetime_test.go`, `encoder_scratch_poison_test.go`, `encoder_heterogeneous_scratch_test.go`, `concurrency_corruption_test.go`, `marshaler_test.go` | `BenchmarkEncodeLarge`, `BenchmarkEncodeMap`, `BenchmarkHookEncodeSmall`, `BenchmarkEncodeTinyAfterHuge` |
| Validation, numbers, dynamic values, and index construction: `any.go`, `index.go`, `index_bitmap.go`, `index_positions.go`, `number_array16*.go`, `number_digits.go`, `number_float*.go`, `valid_bitmap*.go`, `valid_fast.go`, `valid_positions.go`, `walk_number_swar.go` | Use checked fixed-width loads, SWAR digit classification, and compact structural buffers in the parser's hottest loops. | Each fixed-width load is dominated by an explicit remaining-byte check. Bitmap, structural, container, and scalar output capacities are proved before stores. Numeric text views stay within the validated token. Fixed-width array batches validate the complete shape before writing typed destinations. | Temporary strings and slices do not outlive the source call. Dynamic interface values are constructed through typed Go storage, and index results preserve their documented source lifetime. | `valid_differential_test.go`, `valid_bitmap_test.go`, `number_float_differential_test.go`, `number_rejection_contract_test.go`, `typed_slice_contract_test.go`, `route_differential_test.go`, `any_box_corruption_test.go`, `index_bitmap_test.go` | `BenchmarkValid`, `BenchmarkValidLarge`, `BenchmarkNumberCorpusParse`, `BenchmarkNumericDecodePublication`, `BenchmarkUnmarshalAnyLarge`, `BenchmarkBuildIndexBitmapIndent4` |
| Low-level structural kernels: production files under `x/kernels/` listed below | Load vector-width blocks and exchange compact Stage 1 and Stage 2 buffers through direct typed calls. | Full vector loads require a complete block; tail handling selects only complete in-range blocks. Output writes are capacity-checked by the caller or function precondition. Stage 2 constants and root-package entry layouts have compile-time agreement checks. | Kernels retain no source or output pointers after return, and all buffers remain ordinary Go allocations. | `x/kernels/stage1_test.go`, `x/kernels/stage1_index_test.go`, `x/kernels/stage1_stream_test.go`, `valid_bitmap_test.go`, `index_bitmap_test.go` | `BenchmarkStage1Block`, `BenchmarkStage1Chunk32`, `BenchmarkStage2PositionsGo`, `BenchmarkValidLarge`, `BenchmarkBuildIndexBitmapIndent4` |
| Low-level SIMD scanners: production files under `x/scanner/` listed below | Load and store vector-width string spans behind direct root calls. | Full vector loads and stores are dominated by remaining-length checks. Copy entry points reject short or overlapping destinations before vector stores. | Scanners retain no source or output pointers after return. Buffers remain ordinary Go allocations and overlapping copies are rejected. | `x/scanner/scan_test.go`, `x/scanner/scan_simd_test.go` | `BenchmarkStringScannerASCII`, `BenchmarkCopyHTMLStringPrefixASCII` |

The race build, `-d=checkptr=2`, aggressive-GC lifetime tests, scalar/SIMD
differential tests, and corpus tests jointly enforce these invariants. Run the
portable and SIMD tests, vet, the unsafe inventory check, and the relevant
architecture checks before changing an unsafe scope.

### Numeric slice storage

The shared root numeric slice adapters accept only pointer-free, eight-byte
`int64`, `uint64`, and defined `float64`-layout elements. Built-in `[]float64`
keeps its concrete decoder loop and does not pass through a shared adapter.
Dispatch checks the compiled kind, width, and size before an adapter
reinterprets the destination as the corresponding concrete slice. Growth uses
ordinary Go allocation and slice assignment, including its backing-pointer
write barrier. The returned slice is assigned on errors as well as success,
preserving partial results. Types with custom unmarshal methods take their
existing method paths.

`TestNumericSliceStorage` checks defined slice and element types, reused backing,
partial errors, error types, duplicate fields, null, and GC lifetime.
`BenchmarkNumericSliceStorage` covers reuse and fresh allocations for both
public slice entry points.

## Complete production scope list

<!-- BEGIN GENERATED UNSAFE SCOPES -->
<!-- Generated by internal/cmd/unsafeinventory; do not edit this block. -->
- `decoder_cursor.go` — `(*decoderCursor).ownedText`
- `decoder_cursor.go` — `(*decoderCursor).reuseOwnedString`
- `decoder_cursor.go` — `package scope`
- `decoder_receiver.go` — `package scope`
- `encoder_execute.go` — `(*encodeState).encode`
- `encoder_execute.go` — `(*encodeState).encodeKind`
- `encoder_execute.go` — `(Encoder[T]).encodeTypedSource`
- `encoder_execute_record.go` — `(*encodeState).encodeInlineMembers`
- `encoder_execute_record.go` — `(*encodeState).encodeNonAddressableStruct`
- `encoder_execute_record.go` — `(*encodeState).encodeStruct`
- `encoder_execute_record_specialized.go` — `(*encodeState).encodeSimpleStructPairs`
- `encoder_execute_record_specialized.go` — `(*encodeState).encodeStructFieldValue`
- `encoder_execute_sequence.go` — `(*encodeState).encodeArray`
- `encoder_execute_sequence.go` — `(*encodeState).encodeNonAddressableArray`
- `encoder_execute_sequence.go` — `(*encodeState).encodeSlice`
- `encoder_execute_sequence.go` — `(*encodeState).encodeStructSlice`
- `encoder_execute_value.go` — `(*encodeState).encodeMap`
- `encoder_execute_value.go` — `(*encodeState).encodeNonAddressableMarshaler`
- `encoder_execute_value.go` — `(*encodeState).encodeQuoted`
- `encoder_execute_value.go` — `typedValueIsEmpty`
- `encoder_execute_value.go` — `typedValueIsZero`
- `marshaler.go` — `(*decoderCursor).decodeViaTextUnmarshaler`
- `marshaler.go` — `(*decoderCursor).decodeViaUnmarshaler`
- `marshaler.go` — `(*decoderCursor).receiverAt`
- `marshaler.go` — `(*encodeState).encodeMarshaler`
- `marshaler.go` — `(*encodeState).encodeMarshalerKind`
- `node.go` — `entryAt`
- `typed.go` — `(Decoder[T]).Decode`
- `typed.go` — `(Decoder[T]).decodeArrayCursor`
- `typed.go` — `(Decoder[T]).decodePrefixState`
- `typed.go` — `(Decoder[T]).decodeStructural`
- `typed.go` — `decodeTypedDocument`
- `typed.go` — `decodeTypedDocumentReplace`
- `typed.go` — `decodeTypedDocumentScratch`
- `typed_compiled.go` — `(*decoderCursor).decodeCompiled`
- `typed_compiled_record.go` — `(*decoderCursor).decodeCompiledStruct`
- `typed_compiled_record_structural.go` — `(*decoderCursor).decodeCompiledStructStructural`
- `typed_compiled_record_structural.go` — `(*decoderCursor).decodeCompiledStructStructuralExpected`
- `typed_compiled_record_structural.go` — `(*decoderCursor).decodeCompiledStructStructuralRecord`
- `typed_compiled_record_structural.go` — `(*decoderCursor).decodeCompiledStructStructuralSlow`
- `typed_compiled_record_structural_fields.go` — `(*decoderCursor).resetMissingTypedFields`
- `typed_compiled_record_structural_fields.go` — `(*decoderCursor).resetMissingTypedFieldsWide`
- `typed_compiled_record_wide.go` — `(*decoderCursor).decodeCompiledStructWide`
- `typed_compiled_root_slice.go` — `decodeCompiledRootSlice`
- `typed_compiled_sequence.go` — `(*decoderCursor).decodeCompiledRecordSliceStructural`
- `typed_compiled_sequence.go` — `(*decoderCursor).decodeCompiledSlice`
- `typed_compiled_sequence.go` — `(*decoderCursor).decodeCompiledSliceReceivers`
- `typed_compiled_sequence.go` — `(*decoderCursor).decodeCompiledSliceStructural`
- `typed_compiled_sequence_array.go` — `(*decoderCursor).decodeCompiledArray`
- `typed_compiled_sequence_array.go` — `(*decoderCursor).decodeCompiledArrayReceivers`
- `typed_compiled_sequence_array.go` — `(*decoderCursor).decodeCompiledArrayStructural`
- `typed_compiled_sequence_array.go` — `zeroTypedArrayTail`
- `typed_compiled_value.go` — `(*decoderCursor).beginReplaceScope`
- `typed_compiled_value.go` — `(*decoderCursor).clearReplaceOwnerScope`
- `typed_compiled_value.go` — `(*decoderCursor).clearTypedReplaceReferences`
- `typed_compiled_value.go` — `(*decoderCursor).currentReplaceReference`
- `typed_compiled_value.go` — `(*decoderCursor).decodeCompiledBytes`
- `typed_compiled_value.go` — `(*decoderCursor).decodeCompiledMap`
- `typed_compiled_value.go` — `(*decoderCursor).decodeCompiledPointer`
- `typed_compiled_value.go` — `(*decoderCursor).decodeCompiledPointerReplace`
- `typed_compiled_value.go` — `(*decoderCursor).decodeCompiledReferenceReplace`
- `typed_compiled_value.go` — `(*decoderCursor).decodePointerChain`
- `typed_compiled_value.go` — `(*decoderCursor).decodeQuotedField`
- `typed_compiled_value.go` — `(*decoderCursor).pointerAliasesDestination`
- `typed_compiled_value.go` — `(*decoderCursor).prepareReplaceHops`
- `typed_compiled_value.go` — `(*decoderCursor).refreshReplaceReference`
- `typed_compiled_value.go` — `(*decoderCursor).resetMissingInlineMap`
- `typed_compiled_value.go` — `(*decoderCursor).setReplaceDestination`
- `typed_compiled_value.go` — `(*decoderMapScratch).decodeInlineEntry`
- `typed_compiled_value.go` — `allocateTypedPointer`
- `typed_compiled_value.go` — `resolveDecodeHops`
- `typed_compiled_value.go` — `resolveResetHops`
- `typed_hook_bridge.go` — `(*decoderCursor).decodeViaSimdHook`
- `typed_hook_bridge.go` — `(*encodeState).encodeViaSimdHook`
- `typed_reset.go` — `applyTypedResetOp`
- `typed_reset.go` — `resetTyped`
- `typed_slice.go` — `(*typedSliceState).lvalue`
- `typed_slice.go` — `(*typedSliceState).refresh`
- `typed_slice.go` — `(typedSliceState).elementAt`
- `typed_slice.go` — `package scope`
- `typed_slice.go` — `typedSliceAt`
- `x/byteview/byteview.go` — `ByteAt`
- `x/byteview/byteview.go` — `Bytes`
- `x/byteview/byteview.go` — `SliceRange`
- `x/byteview/byteview.go` — `String`
- `x/byteview/byteview.go` — `StringRange`
- `x/scanner/api.go` — `slicesOverlap`
<!-- END GENERATED UNSAFE SCOPES -->
