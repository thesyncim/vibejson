// Package simd exposes the allocation-free byte kernels used by simdjson.
//
// Validated Go 1.27 builds using GOEXPERIMENT=simd select an architecture
// implementation once at package initialization. Other compiler releases and
// builds use byte-exact portable fallbacks.
// The package includes JSON string classification and prefix copying, UTF-8
// and line-separator checks, Unicode escape scanning, fixed-width decimal
// parsing, JSON float and RFC3339 time formatting, stage-1 structural block
// classification, and runtime CPU feature reporting.
package simd
