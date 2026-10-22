package vibejson

import (
	"encoding"
	"encoding/json"
	"errors"
	"testing"
)

type nilMapKeyText struct{ Name string }

func (key *nilMapKeyText) MarshalText() ([]byte, error) {
	if key == nil {
		return []byte("nil-receiver"), nil
	}
	return []byte(key.Name), nil
}

func TestEncoderNilTextMarshalerMapKey(t *testing.T) {
	for _, inline := range []bool{false, true} {
		opts := EncoderOptions{InlineFields: inline}
		static := mustCompileTestEncoder[map[encoding.TextMarshaler]int](t, opts)
		dynamic := mustCompileTestEncoder[any](t, opts)
		for _, mode := range []string{"static", "dynamic"} {
			name := mode + "/ordinary"
			if inline {
				name = mode + "/inline"
			}
			t.Run(name, func(t *testing.T) {
				for attempt := 0; attempt < 2; attempt++ {
					value := map[encoding.TextMarshaler]int{nil: 1}
					if _, err := json.Marshal(value); err == nil {
						t.Fatal("encoding/json accepted a nil interface member name")
					}
					dst := append(make([]byte, 0, 128), "prefix"...)
					var got []byte
					var err error
					if mode == "static" {
						got, err = static.AppendJSON(dst, &value)
					} else {
						var input any = value
						got, err = dynamic.AppendJSON(dst, &input)
					}
					var encodeErr *EncodeError
					if !errors.As(err, &encodeErr) || string(got) != "prefix" || string(dst) != "prefix" {
						t.Fatalf("attempt %d: output=%q, error=%v; want unchanged prefix and EncodeError", attempt, got, err)
					}

					// Reuse the same map plan after an error. An interface holding
					// a typed nil pointer still has a callable method receiver.
					value = map[encoding.TextMarshaler]int{
						(*nilMapKeyText)(nil):         1,
						&nilMapKeyText{Name: "valid"}: 2,
					}
					want, wantErr := json.Marshal(value)
					requireNoTestError(t, wantErr)
					if mode == "static" {
						got, err = static.AppendJSON(nil, &value)
					} else {
						var input any = value
						got, err = dynamic.AppendJSON(nil, &input)
					}
					if err != nil || string(got) != string(want) {
						t.Fatalf("after error: output=%s, error=%v; want %s", got, err, want)
					}
				}
			})
		}
	}
}

func TestEncoderNilPointerTextMarshalerMapKey(t *testing.T) {
	// A statically pointer-typed nil key keeps encoding/json's empty-name
	// behavior; it differs from a typed nil pointer stored in an interface.
	value := map[*nilMapKeyText]int{nil: 1}
	want, err := json.Marshal(value)
	requireNoTestError(t, err)
	got, err := Marshal(&value)
	if err != nil || string(got) != string(want) {
		t.Fatalf("output=%s, error=%v; want %s", got, err, want)
	}
}
