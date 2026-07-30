// Package document defines dependency-neutral types shared by vibejson's
// document APIs: JSON kinds, structural-index options and errors, and JSON
// Pointer errors. The package owns no parser or source storage and remains
// pre-v1.
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
