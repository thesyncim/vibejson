// Package jsoncmp provides allocation-free comparison and streaming decoding
// primitives for validated RFC 8259 JSON scalar tokens.
//
// Callers are expected to pass already-validated JSON tokens (for example spans
// produced by vibejson's structural index or validator). Malformed or truncated
// inputs never panic, but semantic equality is defined only for valid RFC 8259
// tokens.
//
// This package is exported for sibling engines such as vibedb and is explicitly
// unstable. Applications should prefer the high-level APIs in the root vibejson
// package.
package jsoncmp
