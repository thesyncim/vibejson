package slopjson_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"testing"

	"github.com/thesyncim/slopjson"
	"github.com/thesyncim/slopjson/document"
)

type externalRootRecord struct {
	ID    int      `json:"id"`
	Name  string   `json:"name"`
	Tags  []string `json:"tags,omitempty"`
	Note  string   `json:"note"`
	Empty string   `json:"empty,omitempty"`
}

var (
	_ document.Kind = (slopjson.Value{}).Kind()
	_ document.Kind = (slopjson.Node{}).Kind()
	_ document.Kind = (slopjson.RawValue{}).Kind()
	_ document.Kind = new(slopjson.ValueCursor).Kind()
	_ document.Kind = new(slopjson.IndexEntry).Kind()
)

func TestDocumentKindContract(t *testing.T) {
	for _, test := range []struct {
		kind document.Kind
		name string
	}{
		{document.Invalid, "invalid"},
		{document.Null, "null"},
		{document.Bool, "bool"},
		{document.Number, "number"},
		{document.String, "string"},
		{document.Array, "array"},
		{document.Object, "object"},
		{document.Kind(255), "invalid"},
	} {
		if got := test.kind.String(); got != test.name {
			t.Errorf("Kind(%d).String() = %q, want %q", test.kind, got, test.name)
		}
	}
}

func TestDocumentIndexOptionsMigrationContract(t *testing.T) {
	opts := document.IndexOptions{MaxDepth: 1}
	if _, err := slopjson.BuildIndexOptions([]byte(`[]`), make([]slopjson.IndexEntry, 1), opts); err != nil {
		t.Fatalf("BuildIndexOptions with document.IndexOptions: %v", err)
	}
	if _, err := slopjson.BuildIndexOptions([]byte(`[[]]`), make([]slopjson.IndexEntry, 3), opts); err == nil {
		t.Fatal("BuildIndexOptions with MaxDepth=1 accepted depth 2")
	}
}

func TestDocumentIndexErrorMigrationContract(t *testing.T) {
	src := []byte(`[1]`)
	count, err := slopjson.RequiredIndexEntries(src)
	if err != nil {
		t.Fatal(err)
	}
	_, err = slopjson.BuildIndex(src, make([]slopjson.IndexEntry, count-1))
	if err != document.ErrIndexFull || !errors.Is(err, document.ErrIndexFull) {
		t.Fatalf("BuildIndex error = %T %v, want exact document.ErrIndexFull", err, err)
	}

	if document.ErrIndexFull == document.ErrIndexTooLarge {
		t.Fatal("document index sentinels have the same identity")
	}
	for _, test := range []struct {
		err  error
		want string
	}{
		{document.ErrIndexFull, "slopjson: index entry buffer is full"},
		{document.ErrIndexTooLarge, "slopjson: indexed input exceeds 32-bit offsets"},
	} {
		if got := test.err.Error(); got != test.want {
			t.Errorf("%T.Error() = %q, want %q", test.err, got, test.want)
		}
	}
}

func TestDocumentPointerErrorMigrationContract(t *testing.T) {
	src := []byte(`[1]`)
	index, err := slopjson.BuildIndex(src, make([]slopjson.IndexEntry, 2))
	if err != nil {
		t.Fatal(err)
	}
	value, err := slopjson.Parse(src)
	if err != nil {
		t.Fatal(err)
	}
	_, compileErr := slopjson.CompilePointer("/~2")
	_, _, rawErr := slopjson.GetRaw(src, "not-a-pointer")
	_, _, indexErr := index.Pointer("/01")
	_, _, valueErr := value.Pointer("/01")

	for _, test := range []struct {
		name        string
		err         error
		wantPointer string
		wantMessage string
	}{
		{
			name:        "CompilePointer",
			err:         compileErr,
			wantPointer: "/~2",
			wantMessage: "unknown tilde escape",
		},
		{
			name:        "GetRaw",
			err:         rawErr,
			wantPointer: "not-a-pointer",
			wantMessage: "pointer must be empty or start with slash",
		},
		{
			name:        "Index.Pointer",
			err:         indexErr,
			wantPointer: "01",
			wantMessage: "array index has leading zero",
		},
		{
			name:        "Value.Pointer",
			err:         valueErr,
			wantPointer: "01",
			wantMessage: "array index has leading zero",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var documentErr *document.PointerError
			if !errors.As(test.err, &documentErr) {
				t.Fatalf("error = %T %v, want *document.PointerError", test.err, test.err)
			}
			if documentErr.Pointer != test.wantPointer || documentErr.Message != test.wantMessage {
				t.Fatalf("PointerError = %#v, want pointer %q and message %q", documentErr, test.wantPointer, test.wantMessage)
			}
			want := "invalid JSON pointer " + strconv.Quote(documentErr.Pointer) + ": " + documentErr.Message
			if got := documentErr.Error(); got != want {
				t.Fatalf("PointerError.Error() = %q, want %q", got, want)
			}
		})
	}
}

func TestRootTypedJSONContractMatchesEncodingJSON(t *testing.T) {
	src := []byte(`{"id":7,"name":"caf\u00e9","tags":["go","simd"],"note":"<tag>\u2028","unknown":true}`)
	var want externalRootRecord
	if err := json.Unmarshal(src, &want); err != nil {
		t.Fatal(err)
	}

	var got externalRootRecord
	if err := slopjson.Unmarshal(src, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Unmarshal = %#v, encoding/json = %#v", got, want)
	}

	decoder, err := slopjson.CompileDecoder[externalRootRecord](slopjson.DecoderOptions{})
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
	gotJSON, err := slopjson.Marshal(&want)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotJSON, wantJSON) {
		t.Fatalf("Marshal = %s, encoding/json = %s", gotJSON, wantJSON)
	}

	encoder, err := slopjson.CompileEncoder[externalRootRecord](slopjson.EncoderOptions{})
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
		var decodeErr *slopjson.DecodeError
		if !errors.As(err, &decodeErr) {
			t.Fatalf("%s error = %T %v, want *slopjson.DecodeError", label, err, err)
		}
		if decodeErr.Path != "id" {
			t.Fatalf("%s DecodeError.Path = %q, want id", label, decodeErr.Path)
		}
	}
	var rejectedConvenience externalRootRecord
	assertDecodePath("Unmarshal", slopjson.Unmarshal(rejected, &rejectedConvenience))
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
			if got := slopjson.Valid(test.src); got != want {
				t.Fatalf("Valid = %v, encoding/json = %v", got, want)
			}
			if got := slopjson.Validate(test.src) == nil; got != want {
				t.Fatalf("Validate success = %v, encoding/json = %v", got, want)
			}
		})
	}

	err := slopjson.Validate(invalid)
	var syntaxErr *slopjson.SyntaxError
	if !errors.As(err, &syntaxErr) {
		t.Fatalf("Validate error = %T %v, want *slopjson.SyntaxError", err, err)
	}
	if syntaxErr.Offset != 8 || syntaxErr.Line != 1 || syntaxErr.Column != 9 {
		t.Fatalf("SyntaxError coordinates = byte %d line %d column %d, want byte 8 line 1 column 9",
			syntaxErr.Offset, syntaxErr.Line, syntaxErr.Column)
	}
}
