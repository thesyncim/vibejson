package query

import (
	"strings"

	"github.com/thesyncim/simdjson"
)

// A compiledPath is a projection path compiled once for reuse across every
// document a query touches. A single dotted segment ("price") keeps the
// top-level field name so extraction can take the fused columnar fast path
// (ShapeCache.AppendField and the typed AppendField* variants); every other
// path — nested, or written in JSON Pointer syntax — resolves through a
// compiled RFC 6901 pointer (DocSet.AppendPointer). Both spellings honor
// Node.Get semantics: names match by decoded content and duplicate keys
// resolve to the last occurrence.
type compiledPath struct {
	single  bool                     // a single top-level object field
	name    string                   // the field name when single
	key     simdjson.CompiledKey     // compiled name when single
	pointer simdjson.CompiledPointer // the full pointer otherwise
}

// compilePath compiles a path spec. A leading slash (or the empty string)
// selects JSON Pointer syntax; anything else is a dotted path whose segments
// are object keys. Invalid pointer syntax returns the core's PointerError.
func compilePath(spec string) (compiledPath, error) {
	if spec == "" || spec[0] == '/' {
		pointer, err := simdjson.CompilePointer(spec)
		if err != nil {
			return compiledPath{}, err
		}
		return compiledPath{pointer: pointer}, nil
	}
	segments := strings.Split(spec, ".")
	if len(segments) == 1 {
		pointer, err := simdjson.CompilePointer("/" + escapePointerSegment(spec))
		if err != nil {
			return compiledPath{}, err
		}
		return compiledPath{
			single:  true,
			name:    spec,
			key:     simdjson.CompileKey(spec),
			pointer: pointer,
		}, nil
	}
	pointer, err := simdjson.CompilePointer(pointerFromSegments(segments))
	if err != nil {
		return compiledPath{}, err
	}
	return compiledPath{pointer: pointer}, nil
}

func (p compiledPath) pointerForStore() simdjson.CompiledPointer { return p.pointer }

// indexPath returns the canonical RFC 6901 spelling used by declared Store
// indexes. Query's dotted syntax is a front-end convenience; index matching is
// performed only on this exact compiled form.
func (p compiledPath) indexPath() string {
	return p.pointer.String()
}

// pointerFromSegments renders dotted segments as an RFC 6901 pointer, escaping
// each segment's tildes and slashes so a key containing them round-trips.
func pointerFromSegments(segments []string) string {
	var b strings.Builder
	for _, seg := range segments {
		b.WriteByte('/')
		b.WriteString(escapePointerSegment(seg))
	}
	return b.String()
}

// escapePointerSegment applies RFC 6901's token escapes: ~ becomes ~0 and /
// becomes ~1.
func escapePointerSegment(seg string) string {
	if !strings.ContainsAny(seg, "~/") {
		return seg
	}
	var b strings.Builder
	for i := 0; i < len(seg); i++ {
		switch seg[i] {
		case '~':
			b.WriteString("~0")
		case '/':
			b.WriteString("~1")
		default:
			b.WriteByte(seg[i])
		}
	}
	return b.String()
}
