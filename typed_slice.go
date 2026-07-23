package slopjson

import (
	"reflect"
	"unsafe"
)

// typedSliceState is the single boundary for slices whose concrete type is
// known only to the compiled plan. It splits the destination header by
// hazard class. Reads and length-only writes go through a []byte
// reinterpretation of the caller's slice value: every slice shares one
// header representation regardless of element type — the equivalent-layout
// rule the unsafe.Slice and unsafe.String builtins are defined against — so
// the compiler emits the loads, the bounds checks, and the stores, and no
// field offsets are assumed here. Length and capacity read in element
// counts under either view because reslicing never scales them. Mutations
// that install a new backing pointer (grow, the empty sentinel, zeroing)
// happen through public reflect operations, whose write barriers the
// collector requires when a pointer slot changes to a new object.
type typedSliceState struct {
	view *[]byte
	typ  reflect.Type
	data unsafe.Pointer
	len  int
	cap  int
}

func typedSliceAt(typ reflect.Type, ptr unsafe.Pointer) typedSliceState {
	view := (*[]byte)(ptr)
	return typedSliceState{
		view: view,
		typ:  typ,
		data: unsafe.Pointer(unsafe.SliceData(*view)),
		len:  len(*view),
		cap:  cap(*view),
	}
}

// lvalue rebuilds the reflect view of the destination for the pointer-word
// mutations. Only the cold paths below pay for it.
func (slice *typedSliceState) lvalue() reflect.Value {
	return reflect.NewAt(slice.typ, unsafe.Pointer(slice.view)).Elem()
}

func (slice *typedSliceState) refresh() {
	slice.data = unsafe.Pointer(unsafe.SliceData(*slice.view))
	slice.len = len(*slice.view)
	slice.cap = cap(*slice.view)
}

// setLen reslices the destination in place. The language checks the new
// length against the capacity — out of range panics exactly as
// reflect.Value.SetLen would.
func (slice *typedSliceState) setLen(length int) {
	*slice.view = (*slice.view)[:length]
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

// isNil reports whether the destination slice is nil, through the language's
// own nil test on the reinterpreted view; the data pointer is unsuitable for
// this because unsafe.SliceData is unspecified for non-nil empty slices.
func (s *typedSliceState) isNil() bool {
	return *s.view == nil
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
