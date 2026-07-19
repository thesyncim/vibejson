package simdjson

import (
	"reflect"
	"unsafe"
)

// typedSliceState is the single boundary for slices whose concrete type is
// known only to the compiled plan. It uses public reflect operations for the
// slice value while caching its current data pointer and bounds for direct
// element addressing. It never interprets or manufactures a slice header.
type typedSliceState struct {
	value reflect.Value
	data  unsafe.Pointer
	len   int
	cap   int
}

func typedSliceAt(typ reflect.Type, ptr unsafe.Pointer) typedSliceState {
	value := reflect.NewAt(typ, ptr).Elem()
	return typedSliceState{
		value: value,
		data:  value.UnsafePointer(),
		len:   value.Len(),
		cap:   value.Cap(),
	}
}

func (slice *typedSliceState) refresh() {
	slice.data = slice.value.UnsafePointer()
	slice.len = slice.value.Len()
	slice.cap = slice.value.Cap()
}

func (slice *typedSliceState) setLen(length int) {
	slice.value.SetLen(length)
	slice.len = length
}

func (slice *typedSliceState) setZero() {
	slice.value.SetZero()
	slice.refresh()
}

func (slice *typedSliceState) setEmpty() {
	slice.value.Set(reflect.MakeSlice(slice.value.Type(), 0, 0))
	slice.refresh()
}

func (slice *typedSliceState) grow(capacity int) {
	next := reflect.MakeSlice(slice.value.Type(), slice.len, capacity)
	if slice.len != 0 {
		reflect.Copy(next, slice.value)
	}
	slice.value.Set(next)
	slice.refresh()
}

func isBuiltinScalarSlice(typ reflect.Type) bool {
	return typ == reflect.TypeFor[[]int64]() ||
		typ == reflect.TypeFor[[]uint64]() ||
		typ == reflect.TypeFor[[]float64]()
}
