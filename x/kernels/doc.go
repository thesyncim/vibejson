// Package kernels exposes the unstable structural classification and grammar
// machines shared by vibejson engines. Stage 1 produces structural buffers;
// Stage 2 consumes them through caller-owned state. Architecture-specific
// implementations and portable fallbacks share the same call contracts.
// Callers must satisfy each function's pointer, length, capacity, and state
// preconditions.
package kernels
