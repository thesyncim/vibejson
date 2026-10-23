package vibejson

import (
	"encoding/json"
	"testing"
)

type compileGraph0 struct{ Value int }
type compileGraph1 struct{ Left, Right *compileGraph0 }
type compileGraph2 struct{ Left, Right *compileGraph1 }
type compileGraph3 struct{ Left, Right *compileGraph2 }
type compileGraph4 struct{ Left, Right *compileGraph3 }
type compileGraph5 struct{ Left, Right *compileGraph4 }
type compileGraph6 struct{ Left, Right *compileGraph5 }
type compileGraph7 struct{ Left, Right *compileGraph6 }
type compileGraph8 struct{ Left, Right *compileGraph7 }
type compileGraph9 struct{ Left, Right *compileGraph8 }
type compileGraph10 struct{ Left, Right *compileGraph9 }
type compileGraph11 struct{ Left, Right *compileGraph10 }
type compileGraph12 struct{ Left, Right *compileGraph11 }
type compileGraph13 struct{ Left, Right *compileGraph12 }
type compileGraph14 struct{ Left, Right *compileGraph13 }
type compileGraph15 struct{ Left, Right *compileGraph14 }
type compileGraph16 struct{ Left, Right *compileGraph15 }

type inlineGraphValue struct {
	Extra map[string]holdsPointerOnly `json:",inline"`
}

type pointerOnlyOuter struct{ Nested holdsPointerOnly }

func (*pointerOnlyOuter) MarshalJSON() ([]byte, error) { return []byte(`"outer"`), nil }

type recursiveGraphA struct {
	Kids map[string]recursiveGraphB `json:"kids,omitempty"`
	M    pointerOnlyJSON            `json:"m"`
}

type recursiveGraphB struct{ Parent recursiveGraphA }

type graphEmbeddedFields struct{ Promoted pointerOnlyJSON }
type graphEmbeddedValue struct {
	*graphEmbeddedFields
	Direct pointerOnlyJSON
}

type graphEmbeddedRecursive struct {
	*graphEmbeddedRecursiveFields
	Direct pointerOnlyJSON
}
type graphEmbeddedRecursiveFields struct{ Next graphEmbeddedRecursive }

func TestEncoderAddressabilityAcrossPlanGraph(t *testing.T) {
	outer := pointerOnlyOuter{Nested: holdsPointerOnly{M: pointerOnlyJSON{V: 7}}}
	recursive := recursiveGraphA{Kids: map[string]recursiveGraphB{
		"key": {Parent: recursiveGraphA{M: pointerOnlyJSON{V: 7}}},
	}}
	embedded := graphEmbeddedValue{graphEmbeddedFields: &graphEmbeddedFields{Promoted: pointerOnlyJSON{V: 7}}}
	embeddedRecursive := graphEmbeddedRecursive{graphEmbeddedRecursiveFields: &graphEmbeddedRecursiveFields{
		Next: graphEmbeddedRecursive{Direct: pointerOnlyJSON{V: 8}},
	}}
	runAddressabilityCases(t, []struct {
		name  string
		value any
	}{
		{"custom fallback in map", map[string]pointerOnlyOuter{"key": outer}},
		{"custom fallback in interface", outer},
		{"recursive map graph", &recursive},
		{"embedded pointer in map", map[string]graphEmbeddedValue{"key": embedded}},
		{"embedded pointer in interface", embedded},
		{"recursive embedded pointer in map", map[string]graphEmbeddedRecursive{"key": embeddedRecursive}},
		{"recursive embedded pointer in interface", embeddedRecursive},
	})
}

func TestInlineEncoderMarksNonAddressableValues(t *testing.T) {
	value := inlineGraphValue{Extra: map[string]holdsPointerOnly{
		"key": {M: pointerOnlyJSON{V: 7}},
	}}
	encoder := mustCompileTestEncoder[inlineGraphValue](t, EncoderOptions{InlineFields: true})
	got, err := encoder.AppendJSON(nil, &value)
	if err != nil {
		t.Fatal(err)
	}
	want, err := json.Marshal(value.Extra)
	if err != nil || string(got) != string(want) {
		t.Fatalf("inline encoding = %s; ordinary map encoding = %s, %v", got, want, err)
	}
}

func benchmarkCompileGraph[T any](b *testing.B) {
	// Warm the shared field-resolution cache equally in both versions.
	if _, err := CompileEncoder[T](EncoderOptions{}); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := CompileEncoder[T](EncoderOptions{}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCompileEncoderSharedGraph(b *testing.B) {
	b.Run("depth0", benchmarkCompileGraph[compileGraph0])
	b.Run("depth8", benchmarkCompileGraph[compileGraph8])
	b.Run("depth16", benchmarkCompileGraph[compileGraph16])
}
