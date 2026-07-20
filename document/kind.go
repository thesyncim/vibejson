// Package document contains advanced JSON document types. The pre-v1
// migration introduces this package one dependency-neutral type family at a
// time.
package document

// Kind identifies the JSON type stored in a document value. It is immutable,
// owns no storage, and is safe to copy or use concurrently.
type Kind uint8

const (
	// Invalid is the zero value or an absent lookup result.
	Invalid Kind = iota
	// Null is JSON null.
	Null
	// Bool is a JSON true or false value.
	Bool
	// Number is a JSON number whose original spelling is preserved.
	Number
	// String is a JSON string.
	String
	// Array is a JSON array.
	Array
	// Object is a JSON object.
	Object
)

// String returns the lowercase JSON kind name, or "invalid" for Invalid and
// unknown values.
func (k Kind) String() string {
	switch k {
	case Null:
		return "null"
	case Bool:
		return "bool"
	case Number:
		return "number"
	case String:
		return "string"
	case Array:
		return "array"
	case Object:
		return "object"
	default:
		return "invalid"
	}
}
