package slopjson

import (
	"encoding/base64"
	"reflect"
	"strings"
	"time"
	"unsafe"

	simdkernels "github.com/thesyncim/slopjson/simd"
)

// encodeTypedSource is the typed source-to-executor boundary for AppendJSON.
//
// Preconditions: plan.root is the immutable root compiled for exactly T; src
// is non-nil and remains live for this synchronous call; e is call-local and
// its output does not overlap storage reachable from src.
// Ownership: the library does not retain src. A user marshaler may retain its
// ordinary receiver, so escape analysis must remain authoritative.
// Postconditions: the result and e.dst are exactly those of e.encode; the raw
// pointer is neither returned, stored, converted to uintptr, nor hidden from
// escape analysis.
// Callers: Encoder.AppendJSON.
func (plan Encoder[T]) encodeTypedSource(e *encodeState, src *T) error {
	node := plan.root
	return e.encodeKind(node, unsafe.Pointer(src), node.encKind)
}

func (e *encodeState) encode(node *typedNode, src unsafe.Pointer) error {
	return e.encodeKind(node, src, node.encKind)
}

func (e *encodeState) encodeKind(node *typedNode, src unsafe.Pointer, kind typedKind) error {
	switch kind {
	case typedBool:
		if *(*bool)(src) {
			e.dst = append(e.dst, "true"...)
		} else {
			e.dst = append(e.dst, "false"...)
		}
	case typedString:
		e.dst = appendEncodedJSONString(e.dst, *(*string)(src), e.escapeHTML)
	case typedNumber:
		return e.encodeNumberLiteral(*(*string)(src))
	case typedInt:
		switch node.bits {
		case 8:
			e.dst = appendCompactInt(e.dst, int64(*(*int8)(src)))
		case 16:
			e.dst = appendCompactInt(e.dst, int64(*(*int16)(src)))
		case 32:
			e.dst = appendCompactInt(e.dst, int64(*(*int32)(src)))
		case 64:
			e.dst = appendCompactInt(e.dst, *(*int64)(src))
		}
	case typedUint:
		switch node.bits {
		case 8:
			e.dst = appendCompactUint(e.dst, uint64(*(*uint8)(src)))
		case 16:
			e.dst = appendCompactUint(e.dst, uint64(*(*uint16)(src)))
		case 32:
			e.dst = appendCompactUint(e.dst, uint64(*(*uint32)(src)))
		case 64:
			e.dst = appendCompactUint(e.dst, *(*uint64)(src))
		}
	case typedFloat:
		if node.bits == 32 {
			return e.encodeFloat(float64(*(*float32)(src)), 32)
		}
		return e.encodeFloat(*(*float64)(src), 64)
	case typedStruct:
		return e.encodeStruct(node, src)
	case typedSlice:
		return e.encodeSlice(node, src)
	case typedArray:
		return e.encodeArray(node, src)
	case typedMap:
		return e.encodeMap(node, src)
	case typedAny:
		return e.encodeAny(src)
	case typedMarshalerJSON, typedMarshalerText:
		return e.encodeMarshalerKind(node, src, kind)
	case typedMarshalerSimd:
		return e.encodeMarshaler(node, src)
	case typedTime:
		return e.encodeTime(src)
	case typedIface:
		value := reflect.NewAt(node.typ, src).Elem()
		if value.IsNil() {
			e.dst = append(e.dst, "null"...)
			return nil
		}
		return e.encodeDynamicValue(value.Elem())
	case typedBytes:
		value := reflect.NewAt(node.typ, src).Elem()
		if value.IsNil() {
			e.dst = append(e.dst, "null"...)
			return nil
		}
		raw := value.Bytes()
		e.dst = append(e.dst, '"')
		e.dst = base64.StdEncoding.AppendEncode(e.dst, raw)
		e.dst = append(e.dst, '"')
		return nil
	case typedPointer:
		pointer := *(*unsafe.Pointer)(src)
		if pointer == nil {
			e.dst = append(e.dst, "null"...)
			return nil
		}
		if encoderDetectCycles {
			key := encoderCycleKey{typ: node.typ, ptr: pointer, kind: encoderCyclePointer}
			if err := e.enterReference(key); err != nil {
				return err
			}
			defer e.leaveReference(key)
			if e.nonAddr {
				e.nonAddr = false
				err := e.encode(node.elem, pointer)
				e.nonAddr = true
				return err
			}
			return e.encode(node.elem, pointer)
		}
		// Pointers indirect without nesting: the decoder counts only
		// containers toward the depth limit, and matching it here keeps
		// documents this package decodes re-encodable.
		run := e.ptrRun
		if run >= defaultMaxDepth {
			return &EncodeError{Reason: "maximum nesting depth exceeded"}
		}
		e.ptrRun = run + 1
		if e.nonAddr {
			e.nonAddr = false
			err := e.encode(node.elem, pointer)
			e.nonAddr = true
			e.ptrRun = run
			return err
		}
		err := e.encode(node.elem, pointer)
		e.ptrRun = run
		return err
	default:
		return &EncodeError{Reason: "invalid compiled operation"}
	}
	return nil
}

func (e *encodeState) encodeTime(src unsafe.Pointer) error {
	var err error
	e.dst, err = simdkernels.AppendTimeCached(e.dst, *(*time.Time)(src), &e.timeCache)
	if err != nil {
		return &EncodeError{Reason: "MarshalJSON: Time.MarshalJSON: " + strings.TrimPrefix(err.Error(), "Time.AppendText: ")}
	}
	return nil
}
