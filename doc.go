// Package vibejson implements strict JSON parsing, validation, indexing,
// transformation, and typed encoding and decoding.
//
// Use [Unmarshal] and [Marshal] for occasional typed operations. Compile a
// [Decoder] or [Encoder] when a type is reused. Their options and error types
// document ownership, depth, field matching, and merge behavior.
//
// [Valid] and [Validate] check syntax without building a representation.
// [BuildIndex] validates once and creates caller-owned navigation storage for
// [Node] and its iterators. [Parse] returns an owning, ordered [Value].
// [GetRaw] and [ScanFirstRaw] resolve one JSON Pointer without a persistent
// index; their trusted variants skip validation for already-validated input.
//
// [Unmarshal] into *any builds ordinary Go maps and slices. [AppendCompact],
// [AppendIndent], and [AppendCanonicalize] write transformed JSON to a caller
// buffer. [Reader], [DecodeNext], [ValueCursor], and [Writer] process streams
// without retaining a per-value index.
//
// The module requires Go 1.27.0. Builds with GOEXPERIMENT=simd use validated
// vector kernels on supported arm64 and amd64 toolchains; portable fallbacks
// remain available. The x/ subpackages are unstable low-level interfaces, and
// the simd package is pre-v1.
package vibejson
