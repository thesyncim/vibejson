package vibejson

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
)

type compilePathLeaf struct {
	Repeated int      `json:"repeated"`
	Bad      chan int `json:"bad"`
}

func checkCompileErrorPath[T any](t *testing.T, suffix string, inline bool) {
	t.Helper()
	_, decodeErr := CompileDecoder[T](DecoderOptions{InlineFields: inline})
	_, encodeErr := CompileEncoder[T](EncoderOptions{InlineFields: inline})
	for name, err := range map[string]error{"decode": decodeErr, "encode": encodeErr} {
		t.Run(name, func(t *testing.T) {
			var unsupported *UnsupportedTypeError
			if !errors.As(err, &unsupported) {
				t.Fatalf("compile error = %v, want UnsupportedTypeError", err)
			}
			want := reflect.TypeFor[T]().String() + suffix
			if unsupported.Path != want || unsupported.Type != reflect.TypeFor[chan int]() {
				t.Fatalf("unsupported type/path = %v at %q, want chan int at %q", unsupported.Type, unsupported.Path, want)
			}
		})
	}
}

func TestCompilerDiagnosticPaths(t *testing.T) {
	// Repeated scalar fields exercise cache hits before a later unsupported
	// leaf. The reported path must still identify the first failing occurrence.
	t.Run("struct", func(t *testing.T) {
		checkCompileErrorPath[struct {
			First int             `json:"first"`
			Leaf  compilePathLeaf `json:"leaf"`
		}](t, ".leaf.bad", false)
	})
	t.Run("pointer", func(t *testing.T) {
		checkCompileErrorPath[*compilePathLeaf](t, "*.bad", false)
	})
	t.Run("slice", func(t *testing.T) {
		checkCompileErrorPath[[]compilePathLeaf](t, "[].bad", false)
	})
	t.Run("array", func(t *testing.T) {
		checkCompileErrorPath[[2]compilePathLeaf](t, "[].bad", false)
	})
	t.Run("map", func(t *testing.T) {
		checkCompileErrorPath[map[string]compilePathLeaf](t, "[key].bad", false)
	})
	t.Run("inline", func(t *testing.T) {
		checkCompileErrorPath[struct {
			First int                        `json:"first"`
			Extra map[string]compilePathLeaf `json:",inline"`
		}](t, "[inline].bad", true)
	})
}

var benchAnonymousCompileSink *typedNode

func BenchmarkCompileAnonymousFieldGraph(b *testing.B) {
	for _, count := range []int{16, 128, 512} {
		typ := wideFieldMetadataType(count)
		path := typ.String()
		for _, mode := range []typedCompileMode{typedCompileDecode, typedCompileEncode} {
			name := "decode"
			if mode == typedCompileEncode {
				name = "encode"
			}
			b.Run(fmt.Sprintf("%s/fields%d", name, count), func(b *testing.B) {
				// Measure graph construction with the real anonymous root name;
				// runtime reset/scratch preparation is covered by plan benchmarks.
				warm := newTypedCompiler(mode)
				if _, err := warm.compile(typ, path); err != nil {
					b.Fatal(err)
				}
				b.ReportAllocs()
				b.ResetTimer()
				for b.Loop() {
					compiler := newTypedCompiler(mode)
					node, err := compiler.compile(typ, path)
					if err != nil {
						b.Fatal(err)
					}
					benchAnonymousCompileSink = node
				}
			})
		}
	}
}
