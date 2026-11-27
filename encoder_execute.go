package vibejson

import (
	"encoding/base64"
	"reflect"
	"slices"
	"strings"
	"time"
	"unsafe"

	simdkernels "github.com/thesyncim/vibejson/simd"
)

// encodeTypedSource is the typed source-to-executor boundary for AppendJSON.
// plan.root matches T, src is non-nil and live for this synchronous call, and
// e.dst does not overlap storage reachable from src. The encoder does not
// retain src; the raw pointer is used synchronously and is never stored or
// converted to uintptr. Custom marshalers may retain their ordinary receiver
// under normal Go escape semantics.
func (plan Encoder[T]) encodeTypedSource(e *encodeState, src *T) error {
	node := plan.root
	return e.encodeKind(node, unsafe.Pointer(src), node.encKind)
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
		return e.encodeAnyValue(*(*any)(src), false)
	case typedMarshalerJSON, typedMarshalerText:
		return e.encodeMarshalerKind(node, src, kind)
	case typedMarshalerSimd:
		if e.nonAddr && node.encNonAddrKind != typedMarshalerSimd {
			return e.encodeKind(node, src, node.encNonAddrKind)
		}
		return e.encodeViaSimdHook(node, src)
	case typedTime:
		return e.encodeTime(*(*time.Time)(src))
	case typedIface, typedIfaceInline:
		value := reflect.NewAt(node.typ, src).Elem()
		if value.IsNil() {
			e.dst = append(e.dst, "null"...)
			return nil
		}
		concrete := value.Elem()
		if concrete.Kind() == reflect.Pointer && concrete.IsNil() {
			if node.typ.Implements(jsonMarshalerReflectType) {
				return e.encodeMarshalerKind(node, src, typedMarshalerJSON)
			}
			if node.typ.Implements(textMarshalerReflectType) {
				return e.encodeMarshalerKind(node, src, typedMarshalerText)
			}
			e.dst = append(e.dst, "null"...)
			return nil
		}
		if kind == typedIfaceInline {
			return e.encodeDynamicValue(concrete, &dynamicEncodeInlineNodes)
		}
		return e.encodeDynamicValue(concrete, &dynamicEncodeNodes)
	case typedAnyInline:
		return e.encodeAnyValue(*(*any)(src), true)
	case typedBytes:
		value := reflect.NewAt(node.typ, src).Elem()
		if value.IsNil() {
			e.dst = append(e.dst, "null"...)
			return nil
		}
		raw := value.Bytes()
		n := base64.StdEncoding.EncodedLen(len(raw))
		start := len(e.dst)
		e.dst = slices.Grow(e.dst, n+2)
		e.dst = e.dst[:start+n+2]
		e.dst[start] = '"'
		base64.StdEncoding.Encode(e.dst[start+1:start+1+n], raw)
		e.dst[start+1+n] = '"'
		return nil
	case typedPointer:
		pointer := *(*unsafe.Pointer)(src)
		if pointer == nil {
			e.dst = append(e.dst, "null"...)
			return nil
		}
		// Pointers indirect without nesting: the decoder counts only
		// containers toward the depth limit, and matching it here keeps
		// documents this package decodes re-encodable.
		run := e.ptrRun
		if run >= DefaultMaxDepth {
			return &EncodeError{Reason: "maximum nesting depth exceeded"}
		}
		e.ptrRun = run + 1
		if e.nonAddr {
			e.nonAddr = false
			err := e.encodeKind(node.elem, pointer, node.elem.encKind)
			e.nonAddr = true
			e.ptrRun = run
			return err
		}
		err := e.encodeKind(node.elem, pointer, node.elem.encKind)
		e.ptrRun = run
		return err
	default:
		return &EncodeError{Reason: "invalid compiled operation"}
	}
	return nil
}

func (e *encodeState) encodeTime(t time.Time) error {
	var err error
	e.dst, err = simdkernels.AppendTimeCached(e.dst, t, &e.timeCache)
	if err != nil {
		return &EncodeError{Reason: "MarshalJSON: Time.MarshalJSON: " + strings.TrimPrefix(err.Error(), "Time.AppendText: ")}
	}
	return nil
}
