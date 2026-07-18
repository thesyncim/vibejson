//go:build !go1.27

package simdjson

// encoding/json v1 has no encoder nesting limit. Recursive reference-bearing
// values are protected independently by delayed identity-based cycle checks.
const encoderHasDepthLimit = false
