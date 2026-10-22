package vibejson

import (
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"testing"
)

var errNativeQuotedHook = errors.New("quoted hook rejected input")

type nativeQuotedScalar string

func (v *nativeQuotedScalar) UnmarshalVibeJSON(c DecodeCursor) (DecodeCursor, error) {
	raw, err := c.Raw()
	if err != nil {
		return c, err
	}
	if string(raw.Bytes()) == `"reject"` {
		return c, errNativeQuotedHook
	}
	*v = nativeQuotedScalar(raw.Bytes())
	return c, nil
}

func (v *nativeQuotedScalar) MarshalVibeJSON(w TrustedAppender) TrustedAppender {
	if *v == "reject" {
		return w.Float64(math.NaN())
	}
	return w.String("hook:" + string(*v))
}

// The standard hook type is separate so JSON methods cannot hide the native
// pointer's dispatch through its element plan.
type standardQuotedScalar string

func (v *standardQuotedScalar) UnmarshalJSON(src []byte) error {
	if string(src) == `"reject"` {
		return errNativeQuotedHook
	}
	*v = standardQuotedScalar(src)
	return nil
}

func (v *standardQuotedScalar) MarshalJSON() ([]byte, error) {
	if *v == "reject" {
		return nil, errNativeQuotedHook
	}
	return json.Marshal("hook:" + string(*v))
}

type nativeQuotedDocument[T ~string] struct {
	Value   T   `json:"value,string"`
	Pointer *T  `json:"pointer,string"`
	Plain   int `json:"plain,string"`
}

type nativeQuotedEnvelope[T ~string] struct {
	Direct nativeQuotedDocument[T]             `json:"direct"`
	Array  [1]nativeQuotedDocument[T]          `json:"array"`
	Slice  []nativeQuotedDocument[T]           `json:"slice"`
	Map    map[string]*nativeQuotedDocument[T] `json:"map"`
}

func nativeQuotedSnapshot[T ~string](v nativeQuotedEnvelope[T]) []any {
	var result []any
	for _, doc := range []nativeQuotedDocument[T]{v.Direct, v.Array[0], v.Slice[0], *v.Map["item"]} {
		result = append(result, string(doc.Value), doc.Pointer == nil, doc.Plain)
		if doc.Pointer != nil {
			result = append(result, string(*doc.Pointer))
		}
	}
	return result
}

func TestNativeScalarHookQuotedDecode(t *testing.T) {
	for _, replace := range []bool{false, true} {
		decoder := mustCompileTestDecoder[nativeQuotedEnvelope[nativeQuotedScalar]](t, DecoderOptions{Replace: replace})
		var got nativeQuotedEnvelope[nativeQuotedScalar]
		for _, token := range []string{`"hello"`, `"123"`, `null`, `{"nested":[1]}`} {
			doc := `{"value":` + token + `,"pointer":` + token + `,"plain":"12"}`
			src := []byte(`{"direct":` + doc + `,"array":[` + doc + `],"slice":[` + doc + `],"map":{"item":` + doc + `}}`)
			var want nativeQuotedEnvelope[standardQuotedScalar]
			requireNoTestError(t, json.Unmarshal(src, &want))
			requireNoTestError(t, decoder.Decode(src, &got))
			if !reflect.DeepEqual(nativeQuotedSnapshot(got), nativeQuotedSnapshot(want)) {
				t.Fatalf("replace=%v token=%s: native=%v, encoding/json=%v", replace, token, nativeQuotedSnapshot(got), nativeQuotedSnapshot(want))
			}
		}
	}
}

func newNativeQuotedEnvelope[T ~string](nilPointer bool) nativeQuotedEnvelope[T] {
	value := T("hello")
	doc := nativeQuotedDocument[T]{Value: value, Pointer: &value, Plain: 12}
	if nilPointer {
		doc.Pointer = nil
	}
	return nativeQuotedEnvelope[T]{
		Direct: doc,
		Array:  [1]nativeQuotedDocument[T]{doc},
		Slice:  []nativeQuotedDocument[T]{doc},
		Map:    map[string]*nativeQuotedDocument[T]{"item": &doc},
	}
}

func TestNativeScalarHookQuotedEncode(t *testing.T) {
	encoder, err := CompileEncoder[nativeQuotedEnvelope[nativeQuotedScalar]](EncoderOptions{})
	requireNoTestError(t, err)
	for _, nilPointer := range []bool{false, true} {
		value := newNativeQuotedEnvelope[nativeQuotedScalar](nilPointer)
		standard := newNativeQuotedEnvelope[standardQuotedScalar](nilPointer)
		want, err := json.Marshal(&standard)
		requireNoTestError(t, err)
		for _, encode := range []func(*nativeQuotedEnvelope[nativeQuotedScalar]) ([]byte, error){
			func(src *nativeQuotedEnvelope[nativeQuotedScalar]) ([]byte, error) {
				return encoder.AppendJSON(nil, src)
			},
			Marshal[nativeQuotedEnvelope[nativeQuotedScalar]],
		} {
			got, err := encode(&value)
			requireNoTestError(t, err)
			if string(got) != string(want) {
				t.Fatalf("nilPointer=%v: native=%s, encoding/json=%s", nilPointer, got, want)
			}
		}
	}
}

func TestNativeScalarHookQuotedErrors(t *testing.T) {
	for _, replace := range []bool{false, true} {
		decoder := mustCompileTestDecoder[nativeQuotedDocument[nativeQuotedScalar]](t, DecoderOptions{Replace: replace})
		var got nativeQuotedDocument[nativeQuotedScalar]
		err := decoder.Decode([]byte(`{"value":"ok","pointer":"reject","plain":"12"}`), &got)
		if !errors.Is(err, errNativeQuotedHook) {
			t.Fatalf("replace=%v decode error=%v, want hook error", replace, err)
		}
		requireNoTestError(t, decoder.Decode([]byte(`{"value":"ok","pointer":"hello","plain":"12"}`), &got))
		if got.Pointer == nil || *got.Pointer != `"hello"` || got.Plain != 12 {
			t.Fatalf("decode after hook error = %+v", got)
		}
	}
	encoder, err := CompileEncoder[nativeQuotedDocument[nativeQuotedScalar]](EncoderOptions{})
	requireNoTestError(t, err)
	pointer := nativeQuotedScalar("reject")
	value := nativeQuotedDocument[nativeQuotedScalar]{Value: "ok", Pointer: &pointer, Plain: 12}
	if _, err := encoder.AppendJSON(nil, &value); err == nil {
		t.Fatal("encode skipped the rejecting pointer hook")
	}
	pointer = "hello"
	got, err := encoder.AppendJSON(nil, &value)
	requireNoTestError(t, err)
	if string(got) != `{"value":"hook:ok","pointer":"hook:hello","plain":"12"}` {
		t.Fatalf("encode after hook error = %s", got)
	}
}
