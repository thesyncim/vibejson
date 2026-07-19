package simdjson

import "reflect"

// typedShape is the immutable, direction-neutral type metadata shared by the
// compiled decoder and encoder programs. It is embedded by value so hot plan
// walks retain direct field loads without a pointer chase or allocation.
type typedShape struct {
	typ    reflect.Type
	name   string
	size   uintptr
	bits   int
	length int
}
