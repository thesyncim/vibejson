package simdjson

import (
	"reflect"
	"unsafe"
)

// sliceWords mirrors the runtime representation of a slice value: one data
// pointer word followed by two untyped integer words. The layout is the ABI
// the unsafe.Slice and unsafe.String builtins are defined against;
// TestTypedSliceWordsLayout pins it.
type sliceWords struct {
	data unsafe.Pointer
	len  int
	cap  int
}

// typedSliceState is the single boundary for slices whose concrete type is
// known only to the compiled plan. It splits the header by hazard class:
// reads of all three words and writes of the integer length word go through
// the in-place view — plain loads and stores of caller-owned memory that
// carry no garbage-collector obligations — while every write of the pointer
// word happens through public reflect operations, whose write barriers the
// collector requires for pointer slots. The state never assembles a slice
// header from parts.
type typedSliceState struct {
	words *sliceWords
	typ   reflect.Type
	data  unsafe.Pointer
	len   int
	cap   int
}

func typedSliceAt(typ reflect.Type, ptr unsafe.Pointer) typedSliceState {
	words := (*sliceWords)(ptr)
	return typedSliceState{
		words: words,
		typ:   typ,
		data:  words.data,
		len:   words.len,
		cap:   words.cap,
	}
}

// lvalue rebuilds the reflect view of the destination for the pointer-word
// mutations. Only the cold paths below pay for it.
func (slice *typedSliceState) lvalue() reflect.Value {
	return reflect.NewAt(slice.typ, unsafe.Pointer(slice.words)).Elem()
}

func (slice *typedSliceState) refresh() {
	slice.data = slice.words.data
	slice.len = slice.words.len
	slice.cap = slice.words.cap
}

// setLen stores the length word directly. Bounds stay a hard invariant: the
// stored length must not exceed the capacity the backing array really has,
// exactly the check reflect.Value.SetLen enforces.
func (slice *typedSliceState) setLen(length int) {
	if uint(length) > uint(slice.cap) {
		panic("simdjson: slice length out of range")
	}
	slice.words.len = length
	slice.len = length
}

func (slice *typedSliceState) setZero() {
	slice.lvalue().SetZero()
	slice.refresh()
}

func (slice *typedSliceState) setEmpty() {
	slice.lvalue().Set(reflect.MakeSlice(slice.typ, 0, 0))
	slice.refresh()
}

// elementAt returns the address of element index in the slice's backing
// array. The caller must already hold index < s.len — every call site runs
// setLen(index+1), growing first when index == cap, before taking the
// address — and size must be the slice's element size.
func (s typedSliceState) elementAt(index int, size uintptr) unsafe.Pointer {
	return unsafe.Add(s.data, uintptr(index)*size)
}

func (slice *typedSliceState) grow(capacity int) {
	value := slice.lvalue()
	next := reflect.MakeSlice(slice.typ, slice.len, capacity)
	if slice.len != 0 {
		reflect.Copy(next, value)
	}
	value.Set(next)
	slice.refresh()
}

func isBuiltinScalarSlice(typ reflect.Type) bool {
	return typ == reflect.TypeFor[[]int64]() ||
		typ == reflect.TypeFor[[]uint64]() ||
		typ == reflect.TypeFor[[]float64]()
}
