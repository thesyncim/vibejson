package simdjson

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/thesyncim/simdjson/document"
)

// SchemaType is a set of JSON types accepted by one collection constraint.
// SchemaInteger is the subset of numbers written without a fraction or
// exponent. Combine constants with | to describe a union such as a nullable
// string.
type SchemaType uint16

const (
	SchemaNull SchemaType = 1 << iota
	SchemaBool
	SchemaNumber
	SchemaInteger
	SchemaString
	SchemaArray
	SchemaObject

	schemaKnownTypes = SchemaNull | SchemaBool | SchemaNumber |
		SchemaInteger | SchemaString | SchemaArray | SchemaObject

	// SchemaAny accepts every JSON value. Integer is already a subset of
	// number, so the canonical all-types mask does not need both bits.
	SchemaAny = SchemaNull | SchemaBool | SchemaNumber |
		SchemaString | SchemaArray | SchemaObject
)

var (
	// ErrStoreSchemaDefinition reports an invalid root type, field type, path,
	// or duplicate path while compiling a schema.
	ErrStoreSchemaDefinition = errors.New("simdjson: invalid Store schema")
	// ErrStoreSchemaViolation reports a valid JSON document that does not
	// satisfy its Store's optional compiled schema.
	ErrStoreSchemaViolation = errors.New("simdjson: Store schema violation")
)

// StoreSchemaField constrains one RFC 6901 path. Required distinguishes an
// absent path from a present null: accept null explicitly with SchemaNull.
// Paths may address nested object members or array elements.
type StoreSchemaField struct {
	Path     string
	Types    SchemaType
	Required bool
}

// StoreSchemaDefinition is the declarative input to CompileStoreSchema. Root
// zero means SchemaAny. Fields constrain named paths but deliberately allow
// unspecified JSON fields, keeping evolving document payloads compatible.
type StoreSchemaDefinition struct {
	Root   SchemaType
	Fields []StoreSchemaField
}

type compiledStoreSchemaField struct {
	path     string
	pointer  CompiledPointer
	types    SchemaType
	required bool
}

// StoreSchema is an immutable compiled schema safe for concurrent validation
// and reuse by any number of Store, StoreBuilder, or FileStore instances.
// Validation walks the document's existing structural index and allocates
// nothing on success.
type StoreSchema struct {
	root   SchemaType
	fields []compiledStoreSchemaField
	hash   uint64
}

// SchemaViolationError identifies one failed constraint. Path is empty for a
// root-type mismatch. Missing distinguishes an absent required path from a
// present value of the wrong type.
type SchemaViolationError struct {
	Path     string
	Expected SchemaType
	Actual   document.Kind
	Missing  bool
}

func (e *SchemaViolationError) Error() string {
	if e == nil {
		return ErrStoreSchemaViolation.Error()
	}
	if e.Missing {
		return fmt.Sprintf(
			"%v: required path %q is absent",
			ErrStoreSchemaViolation, e.Path,
		)
	}
	path := e.Path
	if path == "" {
		path = "<root>"
	}
	return fmt.Sprintf(
		"%v: path %q has type %s, expected %s",
		ErrStoreSchemaViolation, path, e.Actual, e.Expected,
	)
}

// Unwrap lets errors.Is classify every constraint failure uniformly.
func (e *SchemaViolationError) Unwrap() error {
	return ErrStoreSchemaViolation
}

// CompileStoreSchema validates and canonically compiles definition. Field
// order does not affect the resulting schema identity.
func CompileStoreSchema(
	definition StoreSchemaDefinition,
) (*StoreSchema, error) {
	root := definition.Root
	if root == 0 {
		root = SchemaAny
	}
	if !validSchemaTypes(root) {
		return nil, fmt.Errorf(
			"%w: invalid root types %#x",
			ErrStoreSchemaDefinition, uint16(root),
		)
	}
	root = canonicalSchemaTypes(root)
	fields := make([]compiledStoreSchemaField, len(definition.Fields))
	for i, field := range definition.Fields {
		if field.Path == "" {
			return nil, fmt.Errorf(
				"%w: field %d uses the root path; use Root",
				ErrStoreSchemaDefinition, i,
			)
		}
		if !utf8.ValidString(field.Path) {
			return nil, fmt.Errorf(
				"%w: field %d path is not valid UTF-8",
				ErrStoreSchemaDefinition, i,
			)
		}
		if !validSchemaTypes(field.Types) {
			return nil, fmt.Errorf(
				"%w: field %q has invalid types %#x",
				ErrStoreSchemaDefinition, field.Path,
				uint16(field.Types),
			)
		}
		field.Types = canonicalSchemaTypes(field.Types)
		path := strings.Clone(field.Path)
		pointer, err := CompilePointer(path)
		if err != nil {
			return nil, fmt.Errorf(
				"%w: field %q: %v",
				ErrStoreSchemaDefinition, path, err,
			)
		}
		fields[i] = compiledStoreSchemaField{
			path: path, pointer: pointer,
			types: field.Types, required: field.Required,
		}
	}
	slices.SortFunc(fields, func(a, b compiledStoreSchemaField) int {
		return strings.Compare(a.path, b.path)
	})
	for i := 1; i < len(fields); i++ {
		if fields[i-1].path == fields[i].path {
			return nil, fmt.Errorf(
				"%w: duplicate path %q",
				ErrStoreSchemaDefinition, fields[i].path,
			)
		}
	}
	schema := &StoreSchema{root: root, fields: fields}
	schema.hash = schema.identityHash()
	return schema, nil
}

func validSchemaTypes(types SchemaType) bool {
	return types != 0 && types&^schemaKnownTypes == 0
}

func canonicalSchemaTypes(types SchemaType) SchemaType {
	if types&SchemaNumber != 0 {
		types &^= SchemaInteger
	}
	return types
}

func (s *StoreSchema) valid() bool {
	if s == nil || !validSchemaTypes(s.root) ||
		s.root != canonicalSchemaTypes(s.root) ||
		s.hash == 0 || s.hash != s.identityHash() {
		return false
	}
	for i, field := range s.fields {
		if field.path == "" || !validSchemaTypes(field.types) ||
			field.types != canonicalSchemaTypes(field.types) ||
			i != 0 && s.fields[i-1].path >= field.path {
			return false
		}
	}
	return true
}

// Definition returns an independent canonical declarative copy of s.
func (s *StoreSchema) Definition() StoreSchemaDefinition {
	if s == nil {
		return StoreSchemaDefinition{}
	}
	definition := StoreSchemaDefinition{
		Root:   s.root,
		Fields: make([]StoreSchemaField, len(s.fields)),
	}
	for i, field := range s.fields {
		definition.Fields[i] = StoreSchemaField{
			Path: field.path, Types: field.types,
			Required: field.required,
		}
	}
	return definition
}

// ValidateIndex checks an already-built document index. Success is
// allocation-free; failures return a typed SchemaViolationError.
func (s *StoreSchema) ValidateIndex(index Index) error {
	if s == nil {
		return nil
	}
	root := index.Root()
	if !schemaAcceptsNode(s.root, root) {
		return &SchemaViolationError{
			Expected: s.root, Actual: root.Kind(),
		}
	}
	for _, field := range s.fields {
		node, found, err := index.PointerCompiled(field.pointer)
		if err != nil {
			return fmt.Errorf(
				"%w: compiled path %q: %v",
				ErrStoreSchemaDefinition, field.path, err,
			)
		}
		if !found {
			if field.required {
				return &SchemaViolationError{
					Path: field.path, Expected: field.types,
					Missing: true,
				}
			}
			continue
		}
		if !schemaAcceptsNode(field.types, node) {
			return &SchemaViolationError{
				Path: field.path, Expected: field.types,
				Actual: node.Kind(),
			}
		}
	}
	return nil
}

func schemaAcceptsNode(types SchemaType, node Node) bool {
	kind := node.Kind()
	if kind == document.Number {
		return types&SchemaNumber != 0 ||
			types&SchemaInteger != 0 && node.IsInteger()
	}
	return schemaAcceptsKind(types, kind)
}

func schemaAcceptsRaw(types SchemaType, raw RawValue) bool {
	kind := raw.Kind()
	if kind == document.Number {
		return types&SchemaNumber != 0 ||
			types&SchemaInteger != 0 &&
				storeSchemaIntegerSpelling(raw.Bytes())
	}
	return schemaAcceptsKind(types, kind)
}

func schemaAcceptsKind(types SchemaType, kind document.Kind) bool {
	var want SchemaType
	switch kind {
	case document.Null:
		want = SchemaNull
	case document.Bool:
		want = SchemaBool
	case document.Number:
		want = SchemaNumber
	case document.String:
		want = SchemaString
	case document.Array:
		want = SchemaArray
	case document.Object:
		want = SchemaObject
	default:
		return false
	}
	return types&want != 0
}

func storeSchemaIntegerSpelling(src []byte) bool {
	if len(src) == 0 {
		return false
	}
	at := 0
	if src[0] == '-' {
		at = 1
	}
	if at == len(src) {
		return false
	}
	for ; at < len(src); at++ {
		if src[at] < '0' || src[at] > '9' {
			return false
		}
	}
	return true
}

// validateDocSetRows checks one bounded micro-page batch. It gathers each
// compiled path once across all selected ordinals, preserving shape/template
// fast paths and amortizing selector setup across as many as 64 stable slots.
// failed is the position in rows of a constraint failure, or -1 when err is a
// page/path error not attributable to one row.
func (s *StoreSchema) validateDocSetRows(
	set *DocSet,
	rows []int,
	values []RawValue,
) (failed int, err error) {
	if s == nil {
		return -1, nil
	}
	for at, row := range rows {
		root := storeSchemaDocumentRaw(set.rawAt(row))
		if !schemaAcceptsRaw(s.root, root) {
			return at, &SchemaViolationError{
				Expected: s.root, Actual: root.Kind(),
			}
		}
	}
	for _, field := range s.fields {
		result, err := set.AppendPointerRows(
			values[:0], rows, field.pointer,
		)
		if err != nil {
			return -1, err
		}
		if len(result) != len(rows) {
			return -1, errors.New(
				"simdjson: schema gather length invariant",
			)
		}
		for at, value := range result {
			if len(value.Bytes()) == 0 {
				if !field.required {
					continue
				}
				return at, &SchemaViolationError{
					Path: field.path, Expected: field.types,
					Missing: true,
				}
			}
			if !schemaAcceptsRaw(field.types, value) {
				return at, &SchemaViolationError{
					Path: field.path, Expected: field.types,
					Actual: value.Kind(),
				}
			}
		}
	}
	return -1, nil
}

func storeSchemaDocumentRaw(src []byte) RawValue {
	start, end := 0, len(src)
	for start < end && storeSchemaJSONSpace(src[start]) {
		start++
	}
	for end > start && storeSchemaJSONSpace(src[end-1]) {
		end--
	}
	return RawValue{src: src[start:end]}
}

func storeSchemaJSONSpace(value byte) bool {
	return value == ' ' || value == '\t' ||
		value == '\r' || value == '\n'
}

func (s *StoreSchema) identityHash() uint64 {
	hash := uint64(14695981039346656037)
	appendByte := func(value byte) {
		hash = (hash ^ uint64(value)) * 1099511628211
	}
	appendUint32 := func(value uint32) {
		appendByte(byte(value))
		appendByte(byte(value >> 8))
		appendByte(byte(value >> 16))
		appendByte(byte(value >> 24))
	}
	appendByte(0x53)
	appendByte(byte(s.root))
	appendByte(byte(s.root >> 8))
	appendUint32(uint32(len(s.fields)))
	for _, field := range s.fields {
		appendUint32(uint32(len(field.path)))
		for i := range field.path {
			appendByte(field.path[i])
		}
		appendByte(byte(field.types))
		appendByte(byte(field.types >> 8))
		if field.required {
			appendByte(1)
		} else {
			appendByte(0)
		}
	}
	if hash == 0 {
		// Zero is reserved as the durable "no catalog" identity.
		return 1
	}
	return hash
}

// String returns a stable human-readable union.
func (t SchemaType) String() string {
	if t == SchemaAny {
		return "any"
	}
	names := [...]struct {
		bit  SchemaType
		name string
	}{
		{SchemaNull, "null"},
		{SchemaBool, "bool"},
		{SchemaNumber, "number"},
		{SchemaInteger, "integer"},
		{SchemaString, "string"},
		{SchemaArray, "array"},
		{SchemaObject, "object"},
	}
	var builder strings.Builder
	for _, item := range names {
		if t&item.bit == 0 {
			continue
		}
		if builder.Len() != 0 {
			builder.WriteByte('|')
		}
		builder.WriteString(item.name)
	}
	if builder.Len() == 0 {
		return "invalid"
	}
	return builder.String()
}
