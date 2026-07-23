package slopjson

import "unsafe"

// --- plan dispatch ---------------------------------------------------------

// decodeViaSimdHook uses the ordinary addressable destination receiver. The
// runtime-built interface keeps the destination storage visible to the GC;
// dispatchDecodeHook transfers cursor state by value.
func (cursor *decoderCursor) decodeViaSimdHook(node *typedNode, dst unsafe.Pointer) error {
	boxed := pointerInterfaceAt(node.typ, dst)
	hook, ok := boxed.(UnmarshalerSimd)
	if !ok {
		return &DecodeError{Offset: cursor.i, Type: node.typ, Reason: "invalid compiled operation"}
	}
	return dispatchDecodeHook(hook, cursor)
}

// encodeViaSimdHook uses ordinary Go receiver ownership. Addressable values
// expose their real *T through a runtime-built interface, so retaining the
// receiver safely retains (and aliases) the caller's value without allocating
// a detached shadow. Non-addressable map/interface values can reach this point
// only when T itself implements the hook; they receive the normal value copy a
// Go value-receiver call specifies. Compact output is trusted and spliced
// without the validation and re-escape passes of json.Marshaler.
func (e *encodeState) encodeViaSimdHook(node *typedNode, src unsafe.Pointer) error {
	var boxed any
	if e.nonAddr {
		boxed = valueInterfaceAt(node.typ, src)
	} else {
		boxed = pointerInterfaceAt(node.typ, src)
	}
	hook, ok := boxed.(MarshalerSimd)
	if !ok {
		return &EncodeError{Reason: "invalid compiled operation"}
	}
	start := len(e.dst)
	w := dispatchEncodeHook(hook, TrustedAppender{dst: e.dst, escapeHTML: e.escapeHTML})
	e.dst = w.dst
	if w.bad {
		return &EncodeError{Reason: "MarshalSimdJSON: unsupported value"}
	}
	if validateSimdHookOutput && !Valid(e.dst[start:]) {
		return &EncodeError{Reason: "MarshalSimdJSON produced invalid JSON"}
	}
	return nil
}
