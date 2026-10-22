package vibejson

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unicode"
)

func TestFieldSetSimpleFoldCycles(t *testing.T) {
	for r := rune(0); r <= unicode.MaxRune; r++ {
		next := unicode.SimpleFold(r)
		if next == r {
			continue
		}
		name := "prefix" + string(r) + "suffix"
		set := MakeFieldSet(name)
		for folded := next; folded != r; folded = unicode.SimpleFold(folded) {
			key := "PREFIX" + string(folded) + "SUFFIX"
			if !strings.EqualFold(name, key) {
				t.Fatalf("invalid oracle pair %q, %q", name, key)
			}
			if got, ok := set.Lookup(key, false); !ok || got != 0 {
				t.Fatalf("Lookup(%q), name %q = (%d, %v)", key, name, got, ok)
			}
		}
	}
}

func TestFieldSetMalformedUTF8Precedence(t *testing.T) {
	names := []string{"\xff", "\xfe", "\ufffd", "\xffA", "\ufffda", "\xff\xfe", "\ufffd\ufffd", "", "Σ", "ς", "K", "K", "ſ", "S"}
	keys := append(append([]string(nil), names...), "\xfea", "\ufffdA", "σ", "k", "s", "\ufffd\xfe", "missing")
	for _, names := range [][]string{names, append([]string{"plain", "other"}, names...)} {
		set := MakeFieldSet(names...)
		for _, key := range keys {
			for _, sensitive := range []bool{false, true} {
				want := -1
				for i, name := range names {
					if name == key {
						want = i
						break
					}
				}
				if want < 0 && !sensitive {
					for i, name := range names {
						if strings.EqualFold(name, key) {
							want = i
							break
						}
					}
				}
				if got, ok := set.Lookup(key, sensitive); got != want || ok != (want >= 0) {
					t.Fatalf("Lookup(%q, %v) = (%d, %v), want %d", key, sensitive, got, ok, want)
				}
			}
		}
	}
}

func TestFieldSetLongKeys(t *testing.T) {
	for _, length := range []int{63, 64, 65, 255, 4096} {
		prefix := strings.Repeat("p", length)
		for _, suffix := range []string{"A", "KſΣ", "\xff\xfe"} {
			name := prefix + suffix
			key := strings.ToUpper(prefix) + suffix
			set := MakeFieldSet("other", name)
			if got, ok := set.Lookup(key, false); !ok || got != 1 {
				t.Fatalf("length %d, suffix %q: Lookup = (%d, %v)", length, suffix, got, ok)
			}
		}
	}
}

var fieldSetCompileSink FieldSet
var fieldFoldCompileSink *typedNode
var fieldFoldLookupSink int

func BenchmarkMakeFieldSet(b *testing.B) {
	for _, count := range []int{5, 32, 512, 4096} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			names := make([]string, count)
			for i := range names {
				names[i] = fmt.Sprintf("field%04d", i)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				fieldSetCompileSink = MakeFieldSet(names...)
			}
		})
	}
}

func BenchmarkFieldSetFoldedLookup(b *testing.B) {
	names := make([]string, 256)
	for i := range names {
		names[i] = fmt.Sprintf("field%04d", i)
	}
	names[len(names)-1] = "sssssss"
	set := MakeFieldSet(names...)
	for _, key := range []string{"field0000", "FIELD0000", "ſſſſſſſ", "missing"} {
		b.Run(key, func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				fieldFoldLookupSink, _ = set.Lookup(key, false)
			}
		})
	}
}

func BenchmarkCompileDecoderFields(b *testing.B) {
	for _, count := range []int{8, 64, 512} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			fields := make([]reflect.StructField, count)
			for i := range fields {
				fields[i] = reflect.StructField{Name: fmt.Sprintf("Field%04d", i), Type: reflect.TypeFor[int](), Tag: reflect.StructTag(fmt.Sprintf(`json:"field%04d"`, i))}
			}
			typ := reflect.StructOf(fields)
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				compiler := newTypedCompiler(typedCompileDecode)
				node, err := compiler.compile(typ, "benchmark")
				if err != nil {
					b.Fatal(err)
				}
				fieldFoldCompileSink = node
			}
		})
	}
}
