package simdjson

import (
	"reflect"
	"slices"
	"sort"
	"strings"
)

// resolvedField is one JSON-visible field after anonymous struct flattening,
// following encoding/json's visibility and dominance rules.
type resolvedField struct {
	name      string
	tagged    bool
	omitEmpty bool
	quoted    bool
	inline    bool // ",inline" map[string]T: the unknown-member catch-all
	index     []int
	typ       reflect.Type
}

// typedFieldHop is one embedded-pointer traversal on the way to a flattened
// field: dereference the pointer at offset, allocating pointee on decode.
type typedFieldHop struct {
	offset     uintptr
	pointee    reflect.Type
	unexported bool
}

// Provenance: GO-FIELDS-001.
// resolveStructFields is adapted from encoding/json typeFields at Go commit
// d468ad3648be469ffc4090e4586c29709182d6b6,
// src/encoding/json/encode.go. Copyright The Go Authors; BSD-3-Clause, see
// LICENSE-GO. Local changes use the compiled-field representation, pointer-hop
// metadata, and the opt-in ",inline" extension.
//
// resolveStructFields ports encoding/json's typeFields semantics: breadth
// first traversal of anonymous struct fields, JSON tag handling, and
// shallowest-then-tagged dominance with same-level conflicts dropped.
func resolveStructFields(root reflect.Type) []resolvedField {
	current := []resolvedField{}
	next := []resolvedField{{typ: root}}
	var count, nextCount map[reflect.Type]int
	visited := map[reflect.Type]bool{}
	var fields []resolvedField

	for len(next) > 0 {
		current, next = next, current[:0]
		count, nextCount = nextCount, map[reflect.Type]int{}

		for _, scan := range current {
			if visited[scan.typ] {
				continue
			}
			visited[scan.typ] = true

			for i := 0; i < scan.typ.NumField(); i++ {
				structField := scan.typ.Field(i)
				if structField.Anonymous {
					embedded := structField.Type
					if embedded.Kind() == reflect.Pointer {
						embedded = embedded.Elem()
					}
					if !structField.IsExported() && embedded.Kind() != reflect.Struct {
						continue
					}
				} else if !structField.IsExported() {
					continue
				}
				tag := structField.Tag.Get("json")
				if tag == "-" {
					continue
				}
				name, options, _ := strings.Cut(tag, ",")
				if !validTypedTag(name) {
					name = ""
				}
				index := slices.Clone(scan.index)
				index = append(index, i)

				fieldType := structField.Type
				if fieldType.Name() == "" && fieldType.Kind() == reflect.Pointer {
					fieldType = fieldType.Elem()
				}

				if name != "" || !structField.Anonymous || fieldType.Kind() != reflect.Struct {
					tagged := name != ""
					if name == "" {
						name = structField.Name
					}
					field := resolvedField{
						name:      name,
						tagged:    tagged,
						omitEmpty: tagOptionsContain(options, "omitempty"),
						inline:    tagOptionsContain(options, "inline"),
						index:     index,
						typ:       structField.Type,
					}
					if tagOptionsContain(options, "string") {
						switch fieldType.Kind() {
						case reflect.Bool,
							reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
							reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
							reflect.Float32, reflect.Float64,
							reflect.String:
							field.quoted = true
						}
					}
					fields = append(fields, field)
					if count[scan.typ] > 1 {
						// The enclosing type appeared multiple times at the
						// previous level, so its fields conflict with
						// themselves and must annihilate during dominance.
						fields = append(fields, field)
					}
					continue
				}

				nextCount[fieldType]++
				if nextCount[fieldType] == 1 {
					next = append(next, resolvedField{index: index, typ: fieldType})
				}
			}
		}
	}

	sort.Slice(fields, func(i, j int) bool {
		a, b := &fields[i], &fields[j]
		if a.name != b.name {
			return a.name < b.name
		}
		if len(a.index) != len(b.index) {
			return len(a.index) < len(b.index)
		}
		if a.tagged != b.tagged {
			return a.tagged
		}
		return byIndexLess(a.index, b.index)
	})

	out := fields[:0]
	for start := 0; start < len(fields); {
		end := start + 1
		for end < len(fields) && fields[end].name == fields[start].name {
			end++
		}
		group := fields[start:end]
		if len(group) == 1 {
			out = append(out, group[0])
		} else if len(group[0].index) != len(group[1].index) || group[0].tagged != group[1].tagged {
			// A unique shallowest or uniquely tagged field dominates;
			// otherwise the whole group is silently dropped like stdlib.
			out = append(out, group[0])
		}
		start = end
	}
	fields = out

	sort.Slice(fields, func(i, j int) bool {
		return byIndexLess(fields[i].index, fields[j].index)
	})
	return fields
}

func tagOptionsContain(options, option string) bool {
	for options != "" {
		var current string
		current, options, _ = strings.Cut(options, ",")
		if current == option {
			return true
		}
	}
	return false
}

func byIndexLess(a, b []int) bool {
	for position, value := range a {
		if position >= len(b) {
			return false
		}
		if value != b[position] {
			return value < b[position]
		}
	}
	return len(a) < len(b)
}
