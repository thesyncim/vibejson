// Package simd provides pre-v1 numeric and time helpers and reports the active
// implementation backend.
//
// GOEXPERIMENT=simd builds select arm64 NEON or amd64 AVX2 implementations
// when supported by the toolchain and CPU; other builds use byte-equivalent
// scalar implementations. Low-level scanners and grammar machines live in the
// unstable x/kernels and x/scanner packages.
package simd
