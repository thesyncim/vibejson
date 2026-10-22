package vibejson

import (
	"encoding/json"
	"reflect"
	"testing"
)

type recursiveJSONFieldA struct {
	B     *recursiveJSONFieldB
	Value string `json:"-"`
}
type recursiveJSONFieldB struct {
	A      recursiveJSONFieldA `json:"a"`
	Quoted int                 `json:"q,string"`
}
type recursiveJSONFieldDocument struct {
	A recursiveJSONFieldA `json:"a"`
	B recursiveJSONFieldB `json:"b"`
}

func (v *recursiveJSONFieldA) UnmarshalJSON(src []byte) error {
	return json.Unmarshal(src, &v.Value)
}

type recursiveTextFieldA struct {
	B     *recursiveTextFieldB
	Value string `json:"-"`
}
type recursiveTextFieldB struct {
	A      recursiveTextFieldA `json:"a"`
	Quoted int                 `json:"q,string"`
}
type recursiveTextFieldDocument struct {
	A recursiveTextFieldA `json:"a"`
	B recursiveTextFieldB `json:"b"`
}

func (v *recursiveTextFieldA) UnmarshalText(src []byte) error {
	v.Value = string(src)
	return nil
}

type recursiveNativeFieldA struct {
	B     *recursiveNativeFieldB
	Value string `json:"-"`
}
type recursiveNativeFieldB struct {
	A      recursiveNativeFieldA `json:"a"`
	Quoted int                   `json:"q,string"`
}
type recursiveNativeFieldDocument struct {
	A recursiveNativeFieldA `json:"a"`
	B recursiveNativeFieldB `json:"b"`
}

func (v *recursiveNativeFieldA) UnmarshalVibeJSON(c DecodeCursor) (DecodeCursor, error) {
	err := c.String(&v.Value)
	return c, err
}
func (v *recursiveNativeFieldA) UnmarshalJSON(src []byte) error {
	return json.Unmarshal(src, &v.Value)
}

func TestDecoderRecursiveFieldMethods(t *testing.T) {
	t.Run("JSON", testDecoderRecursiveFieldMethods[recursiveJSONFieldDocument])
	t.Run("text", testDecoderRecursiveFieldMethods[recursiveTextFieldDocument])
	t.Run("native", testDecoderRecursiveFieldMethods[recursiveNativeFieldDocument])
}

func testDecoderRecursiveFieldMethods[T any](t *testing.T) {
	t.Helper()
	src := []byte(`{"b":{"a":"handled","q":"7"}}`)
	var want T
	if err := json.Unmarshal(src, &want); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name    string
		options DecoderOptions
	}{
		{name: "ordinary"},
		{name: "replace", options: DecoderOptions{Replace: true}},
		{name: "inline", options: DecoderOptions{InlineFields: true}},
	} {
		t.Run(test.name, func(t *testing.T) {
			decoder := mustCompileTestDecoder[T](t, test.options)
			var got T
			if err := decoder.Decode(src, &got); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("compiled=%+v, encoding/json=%+v", got, want)
			}
		})
	}
	for _, inline := range []bool{false, true} {
		name := "dynamic"
		if inline {
			name = "dynamic-inline"
		}
		t.Run(name, func(t *testing.T) {
			var got any = new(T)
			var err error
			if inline {
				decoder := mustCompileTestDecoder[any](t, DecoderOptions{InlineFields: true})
				err = decoder.Decode(src, &got)
			} else {
				err = Unmarshal(src, &got)
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, &want) {
				t.Fatalf("dynamic=%+v, encoding/json=%+v", got, &want)
			}
		})
	}
}

type recursiveReplaceFieldNode struct {
	Value int
	Next  *recursiveReplaceFieldNode
}

func TestReplaceRecursiveFieldSeparatesRootCycle(t *testing.T) {
	src := []byte(`{"Value":1,"Next":{"Value":2}}`)
	var want *recursiveReplaceFieldNode
	if err := json.Unmarshal(src, &want); err != nil {
		t.Fatal(err)
	}
	decoder := mustCompileTestDecoder[*recursiveReplaceFieldNode](t, DecoderOptions{Replace: true})
	got := new(recursiveReplaceFieldNode)
	got.Next = got
	if err := decoder.Decode(src, &got); err != nil {
		t.Fatal(err)
	}
	if got == got.Next || !reflect.DeepEqual(got, want) {
		t.Fatalf("replaced root=%+v, fresh decode=%+v", got, want)
	}
}
