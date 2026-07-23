package slopjson

import (
	"encoding"
	"encoding/json"
	"reflect"
	"runtime"
	"time"
	"unsafe"
)

// valueInterfaceAt copies T into an interface. Calling a value-receiver method
// on that interface cannot expose p to user code.
func valueInterfaceAt(typ reflect.Type, p unsafe.Pointer) any {
	return reflect.NewAt(typ, p).Elem().Interface()
}

// pointerInterfaceAt exposes an addressable caller-owned value through its
// ordinary *T interface. No pointer is hidden from escape analysis: if user
// code retains the receiver, the runtime keeps the source storage alive just
// as it would for a direct Go method call.
func pointerInterfaceAt(typ reflect.Type, p unsafe.Pointer) any {
	return reflect.NewAt(typ, p).Interface()
}

func copyMethodReceiverBack(typ reflect.Type, dst unsafe.Pointer, shadow reflect.Value) {
	if !shadow.IsValid() {
		return
	}
	if typ.Kind() == reflect.Pointer {
		pointer := *(*unsafe.Pointer)(dst)
		if pointer != nil {
			reflect.NewAt(typ.Elem(), pointer).Elem().Set(shadow.Elem())
		}
		return
	}
	reflect.NewAt(typ, dst).Elem().Set(shadow.Elem())
}

// decodeViaUnmarshaler feeds the raw bytes of the next JSON value to the
// destination's UnmarshalJSON, exactly one validated value, null included.
func (cursor *decoderCursor) decodeViaUnmarshaler(node *typedNode, dst unsafe.Pointer) error {
	start := cursor.i
	if err := cursor.Skip(); err != nil {
		return err
	}
	raw := cursor.src[start:cursor.i]
	if node.typ == timeReflectType {
		if err := (*time.Time)(dst).UnmarshalJSON(raw); err != nil {
			return &DecodeError{Offset: start, Type: node.typ, Reason: err.Error()}
		}
		return nil
	}

	// A pointer-kind receiver treats null as assignment like encoding/json.
	if node.typ.Kind() == reflect.Pointer && len(raw) > 0 && raw[0] == 'n' {
		*(*unsafe.Pointer)(dst) = nil
		return nil
	}
	receiver, shadow := cursor.receiverAt(node, dst)
	unmarshaler, ok := receiver.(json.Unmarshaler)
	if !ok {
		return &DecodeError{Offset: start, Type: node.typ, Reason: "invalid compiled operation"}
	}
	err := unmarshaler.UnmarshalJSON(raw)
	copyMethodReceiverBack(node.typ, dst, shadow)
	if err != nil {
		return &DecodeError{Offset: start, Type: node.typ, Reason: err.Error()}
	}
	return nil
}

// receiverAt boxes a detached un/marshaler receiver for the value at dst.
// Repeated container elements draw distinct receivers from a GC-scanned typed
// array; singleton calls keep the one-object shadow. Pointer kinds are loaded
// and allocated on demand before either path copies their pointee.
func (cursor *decoderCursor) receiverAt(node *typedNode, dst unsafe.Pointer) (any, reflect.Value) {
	typ := node.typ
	var source reflect.Value
	if typ.Kind() != reflect.Pointer {
		source = reflect.NewAt(typ, dst).Elem()
	} else {
		pointer := *(*unsafe.Pointer)(dst)
		if pointer == nil {
			value := reflect.New(typ.Elem())
			pointer = value.UnsafePointer()
			*(*unsafe.Pointer)(dst) = pointer
			runtime.KeepAlive(value)
		}
		source = reflect.NewAt(typ.Elem(), pointer).Elem()
	}
	if shadow, ok := cursor.nextReceiverShadow(node, source); ok {
		return shadow.Interface(), shadow
	}
	shadow := reflect.New(source.Type())
	shadow.Elem().Set(source)
	return shadow.Interface(), shadow
}

// decodeViaTextUnmarshaler decodes a JSON string through UnmarshalText.
// Null leaves non-pointer values untouched and nils pointers, like
// encoding/json.
func (cursor *decoderCursor) decodeViaTextUnmarshaler(node *typedNode, dst unsafe.Pointer) error {
	null, err := cursor.TryNull()
	if err != nil {
		return err
	}
	if null {
		if node.typ.Kind() == reflect.Pointer {
			*(*unsafe.Pointer)(dst) = nil
		}
		return nil
	}
	start := cursor.i
	if start >= len(cursor.src) || cursor.src[start] != '"' {
		return &DecodeError{Offset: start, Type: node.typ, Reason: "expected string for TextUnmarshaler"}
	}
	text, err := cursor.stringToken()
	if err != nil {
		return err
	}

	receiver, shadow := cursor.receiverAt(node, dst)
	unmarshaler, ok := receiver.(encoding.TextUnmarshaler)
	if !ok {
		return &DecodeError{Offset: start, Type: node.typ, Reason: "invalid compiled operation"}
	}
	err = unmarshaler.UnmarshalText(text)
	copyMethodReceiverBack(node.typ, dst, shadow)
	if err != nil {
		return &DecodeError{Offset: start, Type: node.typ, Reason: err.Error()}
	}
	return nil
}

// encodeMarshaler dispatches MarshalJSON or MarshalText, compacting and
// re-escaping custom JSON exactly like encoding/json.
func (e *encodeState) encodeMarshaler(node *typedNode, src unsafe.Pointer) error {
	if node.encKind == typedTime {
		return e.encodeTime(src)
	}
	if node.encKind == typedMarshalerSimd {
		if e.nonAddr && node.encNonAddrKind != typedMarshalerSimd {
			return e.encodeKind(node, src, node.encNonAddrKind)
		}
		return e.encodeViaSimdHook(node, src)
	}
	return e.encodeMarshalerKind(node, src, node.encKind)
}

func (e *encodeState) encodeMarshalerKind(node *typedNode, src unsafe.Pointer, kind typedKind) error {
	var boxed any
	if node.typ.Kind() == reflect.Pointer {
		pointer := *(*unsafe.Pointer)(src)
		if pointer == nil {
			e.dst = append(e.dst, "null"...)
			return nil
		}
		// T is itself a pointer. Copying that pointer into an ordinary
		// interface preserves the caller's pointee identity and makes any
		// retention visible to the GC.
		boxed = valueInterfaceAt(node.typ, src)
	} else if node.encNonAddrKind == kind {
		// The compiler set encNonAddrKind to this marshaler kind exactly
		// when the value type itself implements the interface, so the
		// per-call reflect.Implements probes compile down to this compare.
		if e.scratch == nil || node.encScratch < 0 {
			boxed = valueInterfaceAt(node.typ, src)
		} else {
			slot := &e.scratch.marshalers[node.encScratch]
			slot.value.Set(reflect.NewAt(node.typ, src).Elem())
			boxed = slot.boxed
		}
	} else if e.nonAddr {
		// The value type does not implement the interface and the value is
		// not addressable, so encoding/json cannot call the pointer-receiver
		// method: it falls back to the default encoding.
		return e.encodeKind(node, src, node.encNonAddrKind)
	} else {
		// Addressable pointer-receiver marshalers follow direct Go method-call
		// semantics. The runtime-built interface keeps caller storage alive if
		// user code retains the receiver; no detached shadow is needed.
		boxed = pointerInterfaceAt(node.typ, src)
	}

	if kind == typedMarshalerJSON {
		marshaler, ok := boxed.(json.Marshaler)
		if !ok {
			return &EncodeError{Reason: "invalid compiled operation"}
		}
		data, err := marshaler.MarshalJSON()
		if err != nil {
			return &EncodeError{Reason: "MarshalJSON: " + err.Error()}
		}
		if out, ok := appendCleanMarshalerOutput(e.dst, data); ok {
			e.dst = out
			return nil
		}
		compacted, err := AppendCompact(e.dst, data)
		if err != nil {
			return &EncodeError{Reason: "MarshalJSON produced invalid JSON: " + err.Error()}
		}
		if e.escapeHTML {
			compacted = escapeHTMLInPlaceTail(compacted, len(e.dst))
		}
		e.dst = compacted
		return nil
	}
	marshaler, ok := boxed.(encoding.TextMarshaler)
	if !ok {
		return &EncodeError{Reason: "invalid compiled operation"}
	}
	text, err := marshaler.MarshalText()
	if err != nil {
		return &EncodeError{Reason: "MarshalText: " + err.Error()}
	}
	e.dst = appendEncodedJSONString(e.dst, string(text), e.escapeHTML)
	return nil
}

// marshalerPassthroughDeny flags bytes whose presence anywhere in a
// MarshalJSON result forces the general compact-and-escape path: JSON
// whitespace (the output might not be compact), the HTML escape set, and
// the U+2028/U+2029 lead byte. The test is string-blind, so results whose
// string values contain these bytes just take the general path.
var marshalerPassthroughDeny = [4]uint64{
	1<<'\t' | 1<<'\n' | 1<<'\r' | 1<<' ' | 1<<'&' | 1<<'<' | 1<<'>',
	0,
	0,
	1 << (0xE2 - 192),
}

// appendCleanMarshalerOutput appends a MarshalJSON result verbatim when a
// byte-set scan plus strict validation prove that the general path would
// reproduce it unchanged. ok=false leaves dst untouched.
func appendCleanMarshalerOutput(dst []byte, data []byte) ([]byte, bool) {
	for _, c := range data {
		if marshalerPassthroughDeny[c>>6]&(1<<(c&63)) != 0 {
			return dst, false
		}
	}
	if !Valid(data) {
		return dst, false
	}
	return append(dst, data...), true
}

// escapeHTMLInPlaceTail rewrites buf[from:] escaping <, >, &, U+2028, and
// U+2029, which in valid compact JSON only occur inside strings, matching
// encoding/json's compact(escape=true).
// Provenance: GO-STRING-001. The scalar escape behavior is conservatively
// treated as adapted from Go encoding/json appendHTMLEscape at pinned commit
// 03845e30f7b73d1703bd8c21017297f6eecb76d6; BSD-3-Clause, see LICENSE-GO.
// In-place tail rewriting and SIMD prefix scanning are local changes.
func escapeHTMLInPlaceTail(buf []byte, from int) []byte {
	tail := buf[from:]
	needsEscape := false
	for i := 0; i < len(tail); i++ {
		c := tail[i]
		if c == '<' || c == '>' || c == '&' {
			needsEscape = true
			break
		}
		if c == 0xE2 && i+2 < len(tail) && tail[i+1] == 0x80 && (tail[i+2] == 0xA8 || tail[i+2] == 0xA9) {
			needsEscape = true
			break
		}
	}
	if !needsEscape {
		return buf
	}
	escaped := make([]byte, 0, len(tail)+16)
	for i := 0; i < len(tail); i++ {
		c := tail[i]
		switch {
		case c == '<' || c == '>' || c == '&':
			escaped = append(escaped, '\\', 'u', '0', '0', encodeHexDigits[c>>4], encodeHexDigits[c&0xF])
		case c == 0xE2 && i+2 < len(tail) && tail[i+1] == 0x80 && (tail[i+2] == 0xA8 || tail[i+2] == 0xA9):
			escaped = append(escaped, '\\', 'u', '2', '0', '2', encodeHexDigits[tail[i+2]&0xF])
			i += 2
		default:
			escaped = append(escaped, c)
		}
	}
	return append(buf[:from], escaped...)
}

// computeEncPtrMarshaler marks encHasPtrMarshaler bottom-up. A pointer-only
// marshaler leaf qualifies; a struct or array qualifies when it reaches one
// through fields or elements without crossing a pointer, slice, or map, which
// restore or independently handle addressability. Every reachable node is
// visited so its own subtree is marked; cycles resolve to false because a
// cycle must pass through a pointer.
func computeEncPtrMarshaler(node *typedNode, active map[*typedNode]bool) bool {
	if node == nil || active[node] {
		return false
	}
	active[node] = true
	defer delete(active, node)

	has := false
	switch node.encKind {
	case typedMarshalerJSON, typedMarshalerText, typedMarshalerSimd:
		// encNonAddrKind holds the non-marshaler fallback only when the
		// value type does not implement the interface itself.
		has = node.encNonAddrKind != node.encKind
	case typedStruct:
		program := node.encodeProgram
		for i := range program.encFields {
			child := computeEncPtrMarshaler(program.encFields[i].node, active)
			switch program.encFields[i].encOp {
			case typedOpMarshaler, typedOpStruct, typedOpArray:
				has = has || child
			}
		}
	case typedArray:
		has = computeEncPtrMarshaler(node.elem, active)
	case typedPointer, typedSlice, typedMap:
		computeEncPtrMarshaler(node.elem, active)
	}
	node.encHasPtrMarshaler = has
	return has
}
