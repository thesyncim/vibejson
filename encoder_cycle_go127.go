//go:build go1.27

package simdjson

import (
	"reflect"
	"unsafe"
)

const (
	encoderDetectCycles       = false
	encoderCyclePointer uint8 = iota
	encoderCycleMap
	encoderCycleSlice
)

// The Go 1.27 encoder retains the existing 10,000-container guard. The
// compiler removes calls guarded by encoderDetectCycles, so these definitions
// add no work to that release's encoding path.
type encoderCycleKey struct {
	typ    reflect.Type
	ptr    unsafe.Pointer
	length int
	kind   uint8
}

func (e *encodeState) enterReference(encoderCycleKey) error { return nil }
func (e *encodeState) leaveReference(encoderCycleKey)       {}
