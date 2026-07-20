// Package simd exposes the pre-v1 numeric and time kernels used by simdjson,
// together with diagnostic CPU and backend reporting. Functions that append
// to caller-provided slices can avoid output allocation when the destination
// has enough capacity; they may grow it otherwise.
//
// Validated Go 1.27 builds using GOEXPERIMENT=simd compile architecture
// implementations on arm64 and amd64. The arm64 scanner calls NEON directly;
// amd64 GOAMD64 v1/v2 builds choose scalar or AVX2 once during initialization,
// while v3 and newer builds call AVX2 directly. AVX-512 and PMULL capabilities
// are reported for diagnostics but do not select production scanner kernels;
// there are no DotProd, SVE, or SVE2 scanner backends. Other compiler releases
// and builds use byte-exact portable fallbacks.
//
// The package includes fixed-width decimal parsing and formatting, JSON float
// and RFC3339 time formatting, and runtime CPU feature reporting. Structural
// classification, byte scanning, and grammar machines are internal.
package simd
