package vibejson

import "reflect"

// takeValueBacking borrows one compile-time-assigned, fixed-element-type slot
// with room for at least n values. Different map types use different slots, so
// a heterogeneous struct does not discard and reallocate one polymorphic
// backing on every encode. The returned slice keeps its full length, avoiding
// a reflect.Value.Slice header allocation. Recursive use of the same map node
// sees its slot taken and falls back to a fresh backing for that depth.
func (e *encodeState) takeValueBacking(slot encoderBackingSlot, elemType reflect.Type, n int) reflect.Value {
	scratch := e.scratch
	if scratch != nil && slot < 0 {
		if scratch.dynamicValueBackingElem == elemType &&
			scratch.dynamicValueBacking.IsValid() && scratch.dynamicValueBacking.Len() >= n {
			backing := scratch.dynamicValueBacking
			scratch.dynamicValueBacking = reflect.Value{}
			scratch.dynamicValueBackingElem = nil
			return backing
		}
		// A different dynamic element type may replace only an already-cleared
		// backing. Drop both fields together so no stale type/value pairing can
		// be observed by recursive interface encoding.
		scratch.dynamicValueBacking = reflect.Value{}
		scratch.dynamicValueBackingElem = nil
	}
	if scratch != nil && slot >= 0 {
		var backing reflect.Value
		if scratch.valueBackings == nil {
			backing = scratch.valueBacking
			scratch.valueBacking = reflect.Value{}
		} else {
			backing = scratch.valueBackings[slot]
			scratch.valueBackings[slot] = reflect.Value{}
		}
		if backing.IsValid() && backing.Len() >= n {
			return backing
		}
	}
	return reflect.MakeSlice(reflect.SliceOf(elemType), n, n)
}

// releaseValueBackingPrefix clears the populated values and returns the backing
// to its fixed-type slot. Cleanup follows this operation's usage, including
// partial collection on an error, rather than a previous map's retained length.
// A recursive child may restore a backing first; keep that slot and drop ours
// only after clearing its values.
func (e *encodeState) releaseValueBackingPrefix(slot encoderBackingSlot, backing reflect.Value, elemType reflect.Type, used int) {
	scratch := e.scratch
	if scratch == nil {
		return
	}
	clearEncoderValueBacking(backing, used)
	if slot < 0 {
		if scratch.dynamicValueBackingElem != nil {
			return
		}
		scratch.dynamicValueBacking = backing
		scratch.dynamicValueBackingElem = elemType
		return
	}
	if scratch.valueBackings == nil {
		if scratch.valueBacking.IsValid() {
			return
		}
		scratch.valueBacking = backing
		return
	}
	if scratch.valueBackings[slot].IsValid() {
		return
	}
	scratch.valueBackings[slot] = backing
}
