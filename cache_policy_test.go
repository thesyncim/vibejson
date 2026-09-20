package vibejson

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

		firstEscaped, err := dynamicEncodeBoxFor(typ, true, &dynamicEncodeNodes)
		if err != nil {
			t.Fatalf("type %d escaped plan: %v", i, err)
		}
		secondEscaped, err := dynamicEncodeBoxFor(typ, true, &dynamicEncodeNodes)
		if err != nil {
			t.Fatalf("type %d second escaped plan: %v", i, err)
		}
		if firstEscaped != secondEscaped {
			t.Fatalf("type %d escaped plan was recompiled", i)
		}

		firstUnescaped, err := dynamicEncodeBoxFor(typ, false, &dynamicEncodeNodes)
		if err != nil {
			t.Fatalf("type %d unescaped plan: %v", i, err)
		}
		secondUnescaped, err := dynamicEncodeBoxFor(typ, false, &dynamicEncodeNodes)
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

func TestDynamicPlanCacheOptionIsolation(t *testing.T) {
	type record struct {
		ID    int               `json:"id"`
		Extra map[string]string `json:",inline"`
	}
	for round := 0; round < 2; round++ {
		for _, inline := range []bool{false, true} {
			for _, escape := range []bool{true, false} {
				encoder, err := CompileEncoder[any](EncoderOptions{InlineFields: inline, DisableHTMLEscaping: !escape})
				if err != nil {
					t.Fatal(err)
				}
				var value any = record{ID: 7, Extra: map[string]string{"x": "<"}}
				got, err := encoder.AppendJSON(nil, &value)
				if err != nil {
					t.Fatal(err)
				}
				text := "<"
				if escape {
					text = `\u003c`
				}
				want := `{"id":7,"Extra":{"x":"` + text + `"}}`
				if inline {
					want = `{"id":7,"x":"` + text + `"}`
				}
				if string(got) != want {
					t.Fatalf("inline=%v escape=%v: %s, want %s", inline, escape, got, want)
				}
				decoder, err := CompileDecoder[any](DecoderOptions{InlineFields: inline})
				if err != nil {
					t.Fatal(err)
				}
				target := &record{}
				value = target
				if err := decoder.Decode(got, &value); err != nil {
					t.Fatal(err)
				}
				if target.ID != 7 || target.Extra["x"] != "<" {
					t.Fatalf("inline=%v decoded %+v", inline, target)
				}
			}
		}
	}
}

func TestDynamicPlanCacheErrorsStayInTheirMode(t *testing.T) {
	type invalidInline struct {
		Extra map[int]int `json:",inline"`
	}
	for round := 0; round < 2; round++ {
		for _, inline := range []bool{false, true} {
			encoder, err := CompileEncoder[any](EncoderOptions{InlineFields: inline})
			if err != nil {
				t.Fatal(err)
			}
			var value any = &invalidInline{}
			_, err = encoder.AppendJSON(nil, &value)
			if (err != nil) != inline {
				t.Fatalf("encode inline=%v: %v", inline, err)
			}
			decoder, err := CompileDecoder[any](DecoderOptions{InlineFields: inline})
			if err != nil {
				t.Fatal(err)
			}
			err = decoder.Decode([]byte(`{}`), &value)
			if (err != nil) != inline {
				t.Fatalf("decode inline=%v: %v", inline, err)
			}
		}
	}
}
