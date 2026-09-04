package simd

import (
	"runtime"

	"github.com/thesyncim/vibejson/x/kernels"
	"github.com/thesyncim/vibejson/x/scanner"
)

// Info describes the effective implementations for this build and process.
// Some implementations are fixed at compile time; an amd64 scanner in a
// GOAMD64 v1/v2 build may instead be chosen at package initialization.
type Info struct {
	StructuralBackend     string // structural classifier actually selected for this CPU
	StructuralVectorBytes int    // bytes per structural vector, 0 when scalar
	Enabled               bool   // kernels compiled in and selected
	StringBackend         string // string scanning implementation name
	FormatBackend         string // digit formatting implementation name
	StringVectorBytes     int    // string kernel vector width, 0 when scalar
	FormatVectorBytes     int    // format kernel vector width, 0 when scalar
	StringMinBytes        int    // shortest input the string kernels accept
}

// Current reports the effective structural, string, and decimal-format implementations.
// The backend names identify the kernels that execute; CPU capability checks
// remain behind the internal selection boundary.
func Current() Info {
	scan := scanner.Current()
	format := formatBackend()
	return Info{
		Enabled:               scan.Enabled || format != "scalar" || kernels.Stage1SIMDEnabled(),
		StructuralBackend:     kernels.CurrentStage1Backend(),
		StructuralVectorBytes: structuralVectorBytes(),
		StringBackend:         scan.Backend,
		FormatBackend:         format,
		StringVectorBytes:     scan.VectorBytes,
		FormatVectorBytes:     formatVectorBytes(),
		StringMinBytes:        scan.MinBytes,
	}
}

func structuralVectorBytes() int {
	if kernels.Stage1SIMDEnabled() {
		if runtime.GOARCH == "amd64" {
			return 32
		}
		return 16
	}
	return 0
}
