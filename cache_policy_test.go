package slopjson

import (
	"fmt"
	"reflect"
	"testing"
)

func TestDynamicEncodePlanCacheHighTypeCardinality(t *testing.T) {
	const typeCount = 128

	escaped, err := CompileEncoder[any](EncoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	unescaped, err := CompileEncoder[any](EncoderOptions{DisableHTMLEscaping: true})
	if err != nil {
		t.Fatal(err)
	}

	types := make(map[reflect.Type]bool, typeCount)
	for i := range typeCount {
		name := fmt.Sprintf("field_%03d", i)
		typ := reflect.StructOf([]reflect.StructField{{
			Name: "Value",
			Type: reflect.TypeFor[string](),
			Tag:  reflect.StructTag(`json:"` + name + `"`),
		}})
		types[typ] = true
		value := reflect.New(typ).Elem()
		value.Field(0).SetString("<")
		input := value.Interface()

		got, err := escaped.AppendJSON(nil, &input)
		if err != nil {
			t.Fatalf("type %d escaped encode: %v", i, err)
		}
		want := `{"` + name + `":"\u003c"}`
		if string(got) != want {
			t.Fatalf("type %d escaped encode = %s, want %s", i, got, want)
		}

		got, err = unescaped.AppendJSON(nil, &input)
		if err != nil {
			t.Fatalf("type %d unescaped encode: %v", i, err)
		}
		want = `{"` + name + `":"<"}`
		if string(got) != want {
			t.Fatalf("type %d unescaped encode = %s, want %s", i, got, want)
		}

		firstEscaped, err := dynamicEncodeNode(typ, true)
		if err != nil {
			t.Fatalf("type %d escaped plan: %v", i, err)
		}
		secondEscaped, err := dynamicEncodeNode(typ, true)
		if err != nil {
			t.Fatalf("type %d second escaped plan: %v", i, err)
		}
		if firstEscaped != secondEscaped {
			t.Fatalf("type %d escaped plan was recompiled", i)
		}

		firstUnescaped, err := dynamicEncodeNode(typ, false)
		if err != nil {
			t.Fatalf("type %d unescaped plan: %v", i, err)
		}
		secondUnescaped, err := dynamicEncodeNode(typ, false)
		if err != nil {
			t.Fatalf("type %d second unescaped plan: %v", i, err)
		}
		if firstUnescaped != secondUnescaped {
			t.Fatalf("type %d unescaped plan was recompiled", i)
		}
		if firstEscaped == firstUnescaped {
			t.Fatalf("type %d HTML modes shared a plan", i)
		}
	}

	entries := 0
	dynamicEncodeNodes.Range(func(key, _ any) bool {
		if types[key.(dynamicEncodeKey).typ] {
			entries++
		}
		return true
	})
	if want := typeCount * 2; entries != want {
		t.Fatalf("cache entries for generated types = %d, want %d", entries, want)
	}
}
