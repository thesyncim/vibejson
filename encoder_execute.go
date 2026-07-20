package simdjson

import (
	"encoding/base64"
	"fmt"
	"reflect"
	"strings"
	"time"
	"unsafe"

	simdkernels "github.com/thesyncim/simdjson/simd"
)

// AppendJSON appends src encoded as compact JSON to dst. The backing storage of
// dst must not overlap storage reachable from src. Ordinary compiled sources
// remain stack eligible unless an addressable custom method can retain its
// receiver; in that case ordinary escape analysis keeps caller storage alive.
// On error AppendJSON returns dst unchanged in length, but its unused capacity
// may contain partial output.
func (plan Encoder[T]) AppendJSON(dst []byte, src *T) ([]byte, error) {
	if plan.root == nil {
		return dst, fmt.Errorf("simdjson: zero Encoder")
	}
	if src == nil {
		return dst, fmt.Errorf("simdjson: encode source is nil")
	}
	e := encodeState{dst: dst, escapeHTML: plan.escapeHTML}
	if plan.scratch != nil {
		e.scratch = plan.scratch.Get().(*encoderScratch)
	}
	err := e.encode(plan.root, unsafe.Pointer(src))
	if plan.scratch != nil {
		e.scratch.reset()
		plan.scratch.Put(e.scratch)
	}
	if err != nil {
		return dst, err
	}
	return e.dst, nil
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
