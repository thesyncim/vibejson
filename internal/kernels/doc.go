// Package kernels contains the internal structural classification and grammar
// machines used by slopjson. Stage 1 produces compact structural buffers and
// Stage 2 consumes them through typed, allocation-free state owned by callers.
// Architecture-specific implementations and portable fallbacks share the same
// direct-call contracts; this package does not expose a public compatibility
// surface.
package kernels
