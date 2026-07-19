package simdjson_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/thesyncim/simdjson"
)

type externalRootRecord struct {
	ID    int      `json:"id"`
	Name  string   `json:"name"`
	Tags  []string `json:"tags,omitempty"`
	Note  string   `json:"note"`
	Empty string   `json:"empty,omitempty"`
}

func TestRootTypedJSONContractMatchesEncodingJSON(t *testing.T) {
	src := []byte(`{"id":7,"name":"caf\u00e9","tags":["go","simd"],"note":"<tag>\u2028","unknown":true}`)
	var want externalRootRecord
	if err := json.Unmarshal(src, &want); err != nil {
		t.Fatal(err)
	}

	var got externalRootRecord
	if err := simdjson.Unmarshal(src, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Unmarshal = %#v, encoding/json = %#v", got, want)
	}

	decoder, err := simdjson.CompileDecoder[externalRootRecord](simdjson.DecoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var compiled externalRootRecord
	if err := decoder.Decode(src, &compiled); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(compiled, want) {
		t.Fatalf("Decoder.Decode = %#v, encoding/json = %#v", compiled, want)
	}

	wantJSON, err := json.Marshal(&want)
	if err != nil {
		t.Fatal(err)
	}
	gotJSON, err := simdjson.Marshal(&want)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotJSON, wantJSON) {
		t.Fatalf("Marshal = %s, encoding/json = %s", gotJSON, wantJSON)
	}

	encoder, err := simdjson.CompileEncoder[externalRootRecord](simdjson.EncoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	compiledJSON, err := encoder.AppendJSON(nil, &want)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(compiledJSON, wantJSON) {
		t.Fatalf("Encoder.AppendJSON = %s, encoding/json = %s", compiledJSON, wantJSON)
	}
	prefix := []byte("prefix:")
	prefixed, err := encoder.AppendJSON(append([]byte(nil), prefix...), &want)
	if err != nil {
		t.Fatal(err)
	}
	wantPrefixed := append(append([]byte(nil), prefix...), wantJSON...)
	if !bytes.Equal(prefixed, wantPrefixed) {
		t.Fatalf("prefixed AppendJSON = %s, want %s", prefixed, wantPrefixed)
	}

	rejected := []byte(`{"id":"seven","name":"bad","tags":[],"note":""}`)
	if err := json.Unmarshal(rejected, new(externalRootRecord)); err == nil {
		t.Fatal("encoding/json accepted typed mismatch")
	}
	assertDecodePath := func(label string, err error) {
		t.Helper()
		var decodeErr *simdjson.DecodeError
		if !errors.As(err, &decodeErr) {
			t.Fatalf("%s error = %T %v, want *simdjson.DecodeError", label, err, err)
		}
		if decodeErr.Path != "id" {
			t.Fatalf("%s DecodeError.Path = %q, want id", label, decodeErr.Path)
		}
	}
	var rejectedConvenience externalRootRecord
	assertDecodePath("Unmarshal", simdjson.Unmarshal(rejected, &rejectedConvenience))
	var rejectedCompiled externalRootRecord
	assertDecodePath("Decoder.Decode", decoder.Decode(rejected, &rejectedCompiled))
}

func TestRootValidationContractMatchesEncodingJSON(t *testing.T) {
	valid := []byte(`{"id":7,"name":"caf\u00e9","tags":["go","simd"],"note":"<tag>\u2028","unknown":true}`)
	invalid := []byte(`{"id":7,}`)
	for _, test := range []struct {
		name string
		src  []byte
	}{
		{"valid", valid},
		{"trailing comma", invalid},
	} {
		t.Run(test.name, func(t *testing.T) {
			want := json.Valid(test.src)
			if got := simdjson.Valid(test.src); got != want {
				t.Fatalf("Valid = %v, encoding/json = %v", got, want)
			}
			if got := simdjson.Validate(test.src) == nil; got != want {
				t.Fatalf("Validate success = %v, encoding/json = %v", got, want)
			}
		})
	}

	err := simdjson.Validate(invalid)
	var syntaxErr *simdjson.SyntaxError
	if !errors.As(err, &syntaxErr) {
		t.Fatalf("Validate error = %T %v, want *simdjson.SyntaxError", err, err)
	}
	if syntaxErr.Offset != 8 || syntaxErr.Line != 1 || syntaxErr.Column != 9 {
		t.Fatalf("SyntaxError coordinates = byte %d line %d column %d, want byte 8 line 1 column 9",
			syntaxErr.Offset, syntaxErr.Line, syntaxErr.Column)
	}
}
