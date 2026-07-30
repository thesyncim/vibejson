// Package kernels exposes the low-level structural classification and grammar
// machines shared by vibejson engines. Stage 1 produces compact structural
// buffers and Stage 2 consumes them through typed, allocation-free state owned
// by callers. Architecture-specific implementations and portable fallbacks
// share the same direct-call contracts.
//
// These functions operate below the validated root API and include
// caller-proven pointer, length, capacity, and state preconditions. The package
// is explicitly unstable and is not intended as an application-facing JSON
// API.
package kernels
