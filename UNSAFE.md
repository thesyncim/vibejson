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
| Borrowed and lazy views: `node.go`, `raw.go`, `string_views.go` | Construct zero-copy strings and slices and navigate compact index entries without reflection or allocation. | Every source range comes from a validated token or checked index entry. `IndexEntry` has compile-time size and offset assertions. Empty inputs never dereference a nil base. | Value-derived `Node` pointers keep owned source and entry arrays visible to the collector. Index-derived nodes, `RawValue`, zero-copy results, and stream values borrow documented caller or reader storage. Go pointers remain typed pointers; none are stored as `uintptr`. | `ownership_lifetime_test.go`, `gc_lifetime_test.go`, `lazy_navigation_contract_test.go`, `parser_test.go`, `reader_lifecycle_test.go`, `route_differential_test.go` | `BenchmarkParse`, `BenchmarkBuildIndex`, `BenchmarkGetRaw`, `BenchmarkStreamReadNDJSON`, `BenchmarkStreamDynamicWalk` |
| Compiled decoding and hooks: `decoder_cursor.go`, `decoder_structural.go`, `typed.go`, `typed_compiled*.go`, `typed_hook*.go`, `typed_reset.go`, decode paths in `marshaler.go` | Execute a reflect-compiled type plan directly against typed destinations and call user methods with ordinary receiver semantics. | Field offsets and element strides come from `reflect.Type`; slice growth occurs before element addressing. Structural positions are validated before typed loads, and scalar fallbacks preserve the same grammar. | Destination pointers stay visible to the runtime. Native hook cursors cross user code by value; receivers are heap-backed or caller-owned according to normal Go method rules. Temporary decoded strings follow the documented owned or zero-copy mode. | `typed_test.go`, `typed_hook_safety_test.go`, `typed_hook_retention_test.go`, `gc_corruption_test.go`, `route_differential_test.go`, `decoder_structural_test.go` | `BenchmarkDecodeLargeReused`, `BenchmarkDecodeLargeIndentedReused`, `BenchmarkHookDecodeSmall`, `BenchmarkFieldSetLookup` |
| Compiled encoding: `encoder_execute*.go`, `encoder_int.go`, `encoder_scratch.go`, `encoder_string.go`, encode paths in `marshaler.go` | Walk a compiled type plan with reflection confined to dynamic storage and type boundaries. SWAR stores format short integers and strings. | Addresses use reflect-derived sizes and live slice or array bounds. Fixed-width loads and stores are guarded by remaining-length checks. Scratch slots retain their concrete pointer-bearing type and oversized backing is discarded. | Source pointers remain GC-visible for the complete call. User methods receive stable, legally retainable receivers. Pooled scratch does not retain caller values or reinterpret heterogeneous pointer layouts. | `encoder_lifetime_test.go`, `encoder_scratch_poison_test.go`, `encoder_heterogeneous_scratch_test.go`, `concurrency_corruption_test.go`, `marshaler_test.go` | `BenchmarkEncodeLarge`, `BenchmarkEncodeMap`, `BenchmarkHookEncodeSmall`, `BenchmarkEncodeTinyAfterHuge` |
| Validation, numbers, dynamic values, and index construction: `any.go`, `index.go`, `index_bitmap.go`, `index_positions.go`, `number_digits.go`, `number_float*.go`, `valid_bitmap*.go`, `valid_fast.go`, `valid_positions.go`, `walk_number_swar.go` | Use checked fixed-width loads, SWAR digit classification, and compact structural buffers in the parser's hottest loops. | Each fixed-width load is dominated by an explicit remaining-byte check. Bitmap, structural, container, and scalar output capacities are proved before stores. Numeric text views stay within the validated token. | Temporary strings and slices do not outlive the source call. Dynamic interface values are constructed through typed Go storage, and index results preserve their documented source lifetime. | `valid_differential_test.go`, `valid_bitmap_test.go`, `number_float_differential_test.go`, `number_rejection_contract_test.go`, `any_box_corruption_test.go`, `index_bitmap_test.go` | `BenchmarkValid`, `BenchmarkValidLarge`, `BenchmarkNumberCorpusParse`, `BenchmarkUnmarshalAnyLarge`, `BenchmarkBuildIndexBitmapIndent4` |
| Internal structural kernels: production files under `internal/kernels/` listed below | Load vector-width blocks and exchange compact Stage 1 and Stage 2 buffers through direct typed calls. | Full vector loads require a complete block; tail handling selects only complete in-range blocks. Output writes are capacity-checked by the caller or function precondition. Stage 2 constants and root-package entry layouts have compile-time agreement checks. | Kernels retain no source or output pointers after return, and all buffers remain ordinary Go allocations. | `internal/kernels/stage1_test.go`, `internal/kernels/stage1_index_test.go`, `internal/kernels/stage1_stream_test.go`, `valid_bitmap_test.go`, `index_bitmap_test.go` | `BenchmarkStage1Block`, `BenchmarkStage1Chunk32`, `BenchmarkStage2PositionsGo`, `BenchmarkValidLarge`, `BenchmarkBuildIndexBitmapIndent4` |
| Internal SIMD scanners: production files under `internal/scanner/` listed below | Load and store vector-width string spans behind direct root calls and checked public scanner wrappers. | Full vector loads and stores are dominated by remaining-length checks. Copy entry points reject short or overlapping destinations before vector stores. | Scanners retain no source or output pointers after return. Buffers remain ordinary Go allocations and overlapping copies are rejected. | `simd/scan_api_test.go`, `internal/scanner/scan_simd_test.go` | `BenchmarkStringScannerASCII`, `BenchmarkCopyHTMLStringPrefixASCII` |

The race build, `-d=checkptr=2`, aggressive-GC lifetime tests, scalar/SIMD
differential tests, and corpus tests jointly enforce these invariants. See
[`CONTRIBUTING.md`](CONTRIBUTING.md) for the required commands.

## Complete production scope list

<!-- BEGIN GENERATED UNSAFE SCOPES -->
<!-- Generated by internal/cmd/unsafeinventory; do not edit this block. -->
- `any.go` — `(*parser).parseAnyArray`
- `any.go` — `(*parser).parseAnyNumber`
- `any.go` — `(*parser).parseAnyNumberArray`
- `any.go` — `scanAnyNumberFast`
- `decoder_cursor.go` — `(*decoderCursor).NextObjectField`
- `decoder_cursor.go` — `(*decoderCursor).nextObjectFieldSlow`
- `decoder_cursor.go` — `(*decoderCursor).stringStructural`
- `decoder_cursor.go` — `(*decoderCursor).stringStructuralExactSlow`
- `decoder_cursor.go` — `(*decoderCursor).stringToken`
- `decoder_cursor.go` — `(*decoderCursor).typedKey`
- `decoder_cursor.go` — `(*parser).typedKey`
- `decoder_cursor.go` — `shortTypedFloatAt`
- `decoder_cursor.go` — `typedNumberEnd`
- `decoder_cursor_go127.go` — `(*decoderCursor).Float`
- `decoder_cursor_go127.go` — `(*decoderCursor).Int`
- `decoder_cursor_go127.go` — `(*decoderCursor).Number`
- `decoder_cursor_go127.go` — `(*decoderCursor).String`
- `decoder_cursor_go127.go` — `(*decoderCursor).Uint`
- `decoder_cursor_go127.go` — `(*decoderCursor).floatSlow`
- `decoder_cursor_go127.go` — `(*decoderCursor).numberToken`
- `decoder_cursor_go127.go` — `(*decoderCursor).stringSlow`
- `decoder_cursor_pre_go127.go` — `(*decoderCursor).Number`
- `decoder_cursor_pre_go127.go` — `(*decoderCursor).String`
- `decoder_cursor_pre_go127.go` — `(*decoderCursor).numberToken`
- `decoder_cursor_pre_go127.go` — `(*decoderCursor).stringSlow`
- `decoder_cursor_pre_go127.go` — `decoderCursorFloat`
- `decoder_cursor_pre_go127.go` — `decoderCursorFloatSlow`
- `decoder_cursor_pre_go127.go` — `decoderCursorInt`
- `decoder_cursor_pre_go127.go` — `decoderCursorUint`
- `decoder_structural.go` — `(*decoderCursor).finishObjectStructural`
- `decoder_structural.go` — `(*decoderCursor).matchFirstObjectFieldStructuralExpected`
- `decoder_structural.go` — `(*decoderCursor).matchFirstObjectFieldStructuralShape`
- `decoder_structural.go` — `(*decoderCursor).matchNextObjectFieldStructuralExpected`
- `decoder_structural.go` — `(*decoderCursor).matchNextObjectFieldStructuralShape`
- `decoder_structural.go` — `(*decoderCursor).matchObjectFieldStructural`
- `decoder_structural.go` — `(*decoderCursor).nextArrayElementExact`
- `decoder_structural.go` — `(*decoderCursor).nextObjectFieldStructural`
- `decoder_structural.go` — `(*decoderCursor).structuralFirstValueGapOK`
- `decoder_structural.go` — `(*decoderStructuralTape).build`
- `decoder_structural.go` — `structuralColonGap`
- `decoder_structural.go` — `structuralPackedColonTail`
- `encoder_cycle_go127.go` — `package scope`
- `encoder_cycle_pre_go127.go` — `package scope`
- `encoder_execute.go` — `(*encodeState).encode`
- `encoder_execute.go` — `(*encodeState).encodeKind`
- `encoder_execute.go` — `(*encodeState).encodeTime`
- `encoder_execute.go` — `(Encoder[T]).AppendJSON`
- `encoder_execute_record.go` — `(*encodeState).encodeInlineMembers`
- `encoder_execute_record.go` — `(*encodeState).encodeInlineMembersOneShot`
- `encoder_execute_record.go` — `(*encodeState).encodeStruct`
- `encoder_execute_record_specialized.go` — `(*encodeState).encodeSimpleStructPairs`
- `encoder_execute_record_specialized.go` — `(*encodeState).encodeStructFieldValue`
- `encoder_execute_record_specialized.go` — `appendSimpleFieldName`
- `encoder_execute_sequence.go` — `(*encodeState).encodeArray`
- `encoder_execute_sequence.go` — `(*encodeState).encodeFloat64Array`
- `encoder_execute_sequence.go` — `(*encodeState).encodeSlice`
- `encoder_execute_sequence.go` — `(*encodeState).encodeStructSlice`
- `encoder_execute_value.go` — `(*encodeState).encodeAny`
- `encoder_execute_value.go` — `(*encodeState).encodeMap`
- `encoder_execute_value.go` — `(*encodeState).encodeMapValue`
- `encoder_execute_value.go` — `(*encodeState).encodeNonAddressable`
- `encoder_execute_value.go` — `(*encodeState).encodeNonAddressableMarshaler`
- `encoder_execute_value.go` — `(*encodeState).encodeQuoted`
- `encoder_execute_value.go` — `typedValueIsEmpty`
- `encoder_int.go` — `appendCompactUint`
- `encoder_int.go` — `appendCompactUint10`
- `encoder_int.go` — `storeCompactDigitPair`
- `encoder_scratch.go` — `encoderMapScratchLimit`
- `encoder_string.go` — `appendEncodedJSONString`
- `encoder_string.go` — `appendShortCleanJSONString`
- `index.go` — `(*tapeBuilder).stringFast`
- `index.go` — `(*tapeBuilder).walkFast`
- `index.go` — `buildIndexOptions`
- `index.go` — `package scope`
- `index_bitmap.go` — `package scope`
- `index_positions.go` — `buildIndexPositions`
- `index_positions.go` — `indexFallbackNumberMode`
- `index_positions.go` — `indexPositionsFallbackNumberMode`
- `internal/kernels/stage1_amd64.go` — `Stage1Block`
- `internal/kernels/stage1_arm64.go` — `Stage1Block`
- `internal/kernels/stage1_index_arm64.go` — `stage1IndexBlocks`
- `internal/kernels/stage1_stream_arm64.go` — `Stage1BlocksGP`
- `internal/kernels/stage1_stream_default.go` — `Stage1BlocksGP`
- `internal/kernels/stage1_stream_default.go` — `stage1IndexBlocksPortable`
- `internal/kernels/stage2_grammar_go.go` — `Stage2PositionsTrusted`
- `internal/kernels/stage2_index_go.go` — `Stage2IndexPositionsFused`
- `internal/kernels/structural_cursor_arm64.go` — `stage1CursorBlocksSpecialized`
- `internal/kernels/structural_index_meta_arm64.go` — `stage1IndexBlocksMetaNoSample`
- `internal/kernels/structural_valid_arm64.go` — `stage1ValidBlocks`
- `internal/kernels/structural_valid_coarse_arm64.go` — `stage1ValidBlocksCoarse`
- `internal/scanner/api.go` — `slicesOverlap`
- `internal/scanner/scan_simd.go` — `copyHTMLStringPrefix`
- `internal/scanner/scan_simd.go` — `copyStringPrefix`
- `internal/scanner/scan_simd.go` — `hasJSONLineSeparatorFast`
- `internal/scanner/scan_simd.go` — `scanEncodedHTMLSpecialSIMD`
- `internal/scanner/scan_simd.go` — `scanEncodedHTMLSyntaxSIMD`
- `internal/scanner/scan_simd.go` — `scanStringSpecialSIMD`
- `internal/scanner/scan_simd.go` — `scanStringSyntaxSIMD`
- `internal/scanner/scan_simd.go` — `scanUnicodeEscapeRun`
- `internal/scanner/scan_simd.go` — `validUTF8NoLineSeparatorGeneric`
- `internal/scanner/scan_simd_amd64.go` — `scanEncodedHTMLSpecialAVX2`
- `internal/scanner/scan_simd_amd64.go` — `scanEncodedHTMLSpecialAVX512`
- `internal/scanner/scan_simd_amd64.go` — `scanEncodedHTMLSyntaxAVX2`
- `internal/scanner/scan_simd_amd64.go` — `scanEncodedHTMLSyntaxAVX512`
- `internal/scanner/scan_simd_amd64.go` — `scanStringSpecialAVX2`
- `internal/scanner/scan_simd_amd64.go` — `scanStringSpecialAVX512`
- `internal/scanner/scan_simd_amd64.go` — `scanStringSyntaxAVX2`
- `internal/scanner/scan_simd_amd64.go` — `scanStringSyntaxAVX512`
- `internal/scanner/scan_simd_amd64.go` — `validUTF8Runtime`
- `internal/scanner/scan_simd_arm64.go` — `validUTF8NoLineSeparatorRuntime`
- `internal/scanner/scan_simd_arm64.go` — `validUTF8Runtime`
- `marshaler.go` — `(*decoderCursor).decodeViaTextUnmarshaler`
- `marshaler.go` — `(*decoderCursor).decodeViaUnmarshaler`
- `marshaler.go` — `(*decoderCursor).receiverAt`
- `marshaler.go` — `(*encodeState).encodeMarshaler`
- `marshaler.go` — `(*encodeState).encodeMarshalerKind`
- `marshaler.go` — `copyMethodReceiverBack`
- `marshaler.go` — `pointerInterfaceAt`
- `marshaler.go` — `valueInterfaceAt`
- `node.go` — `(Node).Bool`
- `node.go` — `(Node).Float64`
- `node.go` — `(Node).Uint64`
- `node.go` — `tapeEntryOffset`
- `node.go` — `tapeInt64`
- `node.go` — `tapeSourceBytes`
- `node.go` — `tapeUint64`
- `number_digits.go` — `all16Digits`
- `number_digits.go` — `all8Digits`
- `number_digits.go` — `loadUint16LE`
- `number_digits.go` — `loadUint32LE`
- `number_digits.go` — `loadUint64LE`
- `number_digits.go` — `parse16Digits`
- `number_digits.go` — `parse8Digits`
- `number_digits.go` — `parseTapeDigitsUint64`
- `number_digits.go` — `scanDigitsFast`
- `number_digits.go` — `scanDigitsLong`
- `number_digits.go` — `storeUint64LE`
- `number_float.go` — `exactJSONFloat64`
- `number_float.go` — `parseFloat64`
- `number_float.go` — `scanJSONNumber`
- `number_float.go` — `tapeFixedDecimalFloat64`
- `number_float.go` — `tapeFloat64`
- `number_float_typed.go` — `scanTypedFloat64`
- `number_float_typed.go` — `scanTypedFloat64Number`
- `number_float_typed.go` — `scanTypedLeadingZeroFloat64`
- `number_float_typed.go` — `scanTypedSimpleFloat64`
- `number_float_typed.go` — `scanTypedSimpleFloat64Number`
- `number_float_typed.go` — `scanTypedThreeDigitFloat64`
- `number_float_typed.go` — `scanTypedTwoDigitFloat64`
- `raw.go` — `(RawValue).Float64`
- `raw.go` — `(RawValue).Int64`
- `raw.go` — `(RawValue).Uint64`
- `string_views.go` — `(*parser).string`
- `string_views.go` — `appendJSONString`
- `string_views.go` — `ownedBytesString`
- `typed.go` — `(Decoder[T]).Decode`
- `typed.go` — `(Decoder[T]).DecodePrefix`
- `typed.go` — `(Decoder[T]).decodeStructural`
- `typed.go` — `decodeTypedDocument`
- `typed.go` — `decodeTypedDocumentScratch`
- `typed_compiled.go` — `(*decoderCursor).decodeCompiled`
- `typed_compiled_record.go` — `(*decoderCursor).decodeCompiledStruct`
- `typed_compiled_record_structural.go` — `(*decoderCursor).decodeCompiledStructStructural`
- `typed_compiled_record_structural.go` — `(*decoderCursor).decodeCompiledStructStructuralExpected`
- `typed_compiled_record_structural.go` — `(*decoderCursor).decodeCompiledStructStructuralInt64String`
- `typed_compiled_record_structural.go` — `(*decoderCursor).decodeCompiledStructStructuralRecord`
- `typed_compiled_record_structural.go` — `(*decoderCursor).decodeCompiledStructStructuralRecordFloat64x3`
- `typed_compiled_record_structural.go` — `(*decoderCursor).decodeCompiledStructStructuralSliceStruct`
- `typed_compiled_record_structural.go` — `(*decoderCursor).decodeCompiledStructStructuralSlow`
- `typed_compiled_record_structural.go` — `(*decoderCursor).tryCompiledStructStructuralRecordFloat64x3`
- `typed_compiled_record_structural.go` — `structuralPackedFieldAt`
- `typed_compiled_record_structural.go` — `structuralTapePosition`
- `typed_compiled_record_structural_fields.go` — `(*decoderCursor).matchObjectFieldExpected`
- `typed_compiled_record_structural_fields.go` — `(*decoderCursor).nextObjectFieldExpectedSlow`
- `typed_compiled_record_structural_fields.go` — `resetMissingTypedFields`
- `typed_compiled_root_slice.go` — `decodeCompiledRootFloat64Slice`
- `typed_compiled_root_slice.go` — `decodeCompiledRootInt64Slice`
- `typed_compiled_root_slice.go` — `decodeCompiledRootSlice`
- `typed_compiled_root_slice.go` — `decodeCompiledRootUint64Slice`
- `typed_compiled_sequence.go` — `(*decoderCursor).decodeCompiledSlice`
- `typed_compiled_sequence.go` — `(*decoderCursor).decodeCompiledSliceReceivers`
- `typed_compiled_sequence.go` — `(*decoderCursor).decodeCompiledSliceStructural`
- `typed_compiled_sequence.go` — `decodeCompiledFloat64Slice`
- `typed_compiled_sequence.go` — `decodeCompiledInt64Slice`
- `typed_compiled_sequence.go` — `decodeCompiledUint64Slice`
- `typed_compiled_sequence_array.go` — `(*decoderCursor).decodeCompiledArray`
- `typed_compiled_sequence_array.go` — `(*decoderCursor).decodeCompiledArrayReceivers`
- `typed_compiled_sequence_array.go` — `(*decoderCursor).decodeCompiledArrayStructural`
- `typed_compiled_sequence_float.go` — `decodeCompiledFloat64Array3Structural`
- `typed_compiled_sequence_float.go` — `decodeCompiledFloat64ArrayStructural`
- `typed_compiled_sequence_float.go` — `decodeCompiledFloatArray`
- `typed_compiled_sequence_float.go` — `decodeCompiledFloatArrayStructural`
- `typed_compiled_sequence_float.go` — `shortStructuralFloatAt`
- `typed_compiled_sequence_float.go` — `zeroTypedArrayTail`
- `typed_compiled_value.go` — `(*decoderCursor).decodeBytesArray`
- `typed_compiled_value.go` — `(*decoderCursor).decodeCompiledAny`
- `typed_compiled_value.go` — `(*decoderCursor).decodeCompiledBytes`
- `typed_compiled_value.go` — `(*decoderCursor).decodeCompiledIface`
- `typed_compiled_value.go` — `(*decoderCursor).decodeCompiledMap`
- `typed_compiled_value.go` — `(*decoderCursor).decodeCompiledPointer`
- `typed_compiled_value.go` — `(*decoderCursor).decodeQuotedField`
- `typed_compiled_value.go` — `(*decoderMapScratch).decodeInlineEntry`
- `typed_compiled_value.go` — `allocateTypedPointer`
- `typed_compiled_value.go` — `decodeQuotedNumber`
- `typed_compiled_value.go` — `resolveDecodeHops`
- `typed_compiled_value.go` — `resolveResetHops`
- `typed_compiled_value.go` — `setTypedEmptySlice`
- `typed_compiled_value.go` — `setTypedSliceZero`
- `typed_hook_bridge.go` — `(*decoderCursor).decodeViaSimdHook`
- `typed_hook_bridge.go` — `(*encodeState).encodeViaSimdHook`
- `typed_reset.go` — `applyTypedReset`
- `typed_reset.go` — `resetTyped`
- `typed_slice.go` — `package scope`
- `typed_slice.go` — `typedSliceAt`
- `valid_bitmap.go` — `validBitmapEscapes`
- `valid_fast.go` — `fastByteAt`
- `valid_fast.go` — `nextSignificantFast`
- `valid_fast.go` — `scanJSONStringFast`
- `valid_fast.go` — `scanJSONStringFastFrom`
- `valid_fast.go` — `scanJSONStringFastLong`
- `valid_fast.go` — `scanNumberFast`
- `valid_fast.go` — `scanNumberFastTagged`
- `valid_fast.go` — `scanNumberFastTaggedSWAR`
- `valid_fast.go` — `scanShortJSONString`
- `valid_fast.go` — `skipSpaceFast`
- `valid_fast.go` — `validFast`
- `valid_fast.go` — `validRootValueFast`
- `valid_fast.go` — `validStringFast`
- `valid_fast.go` — `validValueFast`
- `valid_positions.go` — `sparseNonASCIIMask`
- `valid_positions.go` — `validPositionsCommitted`
- `valid_positions.go` — `validPositionsSample`
- `valid_positions.go` — `validScalarTokenAt`
- `valid_positions.go` — `validScalarTokenAtMode`
- `walk_number_swar.go` — `(*tapeBuilder).walkFastSWAR`
<!-- END GENERATED UNSAFE SCOPES -->
