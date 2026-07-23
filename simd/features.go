package simd

import "github.com/thesyncim/slopjson/internal/scanner"

// Info describes the effective implementations for this build and process.
// Some implementations are fixed at compile time; an amd64 scanner in a
// GOAMD64 v1/v2 build may instead be chosen at package initialization.
type Info struct {
	Enabled           bool   // kernels compiled in and selected
	StringBackend     string // string scanning implementation name
	FormatBackend     string // digit formatting implementation name
	StringVectorBytes int    // string kernel vector width, 0 when scalar
	FormatVectorBytes int    // format kernel vector width, 0 when scalar
	StringMinBytes    int    // shortest input the string kernels accept
}

// Current reports the effective string and decimal-format implementations.
// The backend names identify the kernels that execute; CPU capability checks
// remain behind the internal selection boundary.
func Current() Info {
	scan := scanner.Current()
	format := formatBackend()
	return Info{
		Enabled:           scan.Enabled || format != "scalar",
		StringBackend:     scan.Backend,
		FormatBackend:     format,
		StringVectorBytes: scan.VectorBytes,
		FormatVectorBytes: formatVectorBytes(),
		StringMinBytes:    scan.MinBytes,
	}
}
