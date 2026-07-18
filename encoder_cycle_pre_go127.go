//go:build !go1.27

package simdjson

import (
	"reflect"
	"unsafe"
)

const (
	encoderDetectCycles                    = true
	encoderStartDetectingCyclesAfter       = 1000
	encoderCyclePointer              uint8 = iota
	encoderCycleMap
	encoderCycleSlice
)

type encoderCycleKey struct {
	typ    reflect.Type
	ptr    unsafe.Pointer
	length int
	kind   uint8
}

// enterReference mirrors encoding/json v1's delayed cycle detection. The map
// stays nil for ordinary values, avoiding allocation and hashing until a path
// crosses 1,000 pointer-, map-, or slice-bearing values.
func (e *encodeState) enterReference(key encoderCycleKey) error {
	e.ptrRun++
	if e.ptrRun <= encoderStartDetectingCyclesAfter {
		return nil
	}
	if e.ptrSeen == nil {
		e.ptrSeen = make(map[encoderCycleKey]struct{})
	}
	if _, ok := e.ptrSeen[key]; ok {
		e.ptrRun--
		return &EncodeError{Reason: "encountered a cycle via " + key.typ.String()}
	}
	e.ptrSeen[key] = struct{}{}
	return nil
}

func (e *encodeState) leaveReference(key encoderCycleKey) {
	if e.ptrRun > encoderStartDetectingCyclesAfter {
		delete(e.ptrSeen, key)
	}
	e.ptrRun--
}
