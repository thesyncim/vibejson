package simdjson_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"testing"

	"github.com/thesyncim/simdjson"
	"github.com/thesyncim/simdjson/document"
)

type externalRootRecord struct {
	ID    int      `json:"id"`
	Name  string   `json:"name"`
	Tags  []string `json:"tags,omitempty"`
	Note  string   `json:"note"`
	Empty string   `json:"empty,omitempty"`
}

var (
	_ document.Kind          = simdjson.Invalid
	_ simdjson.Kind          = document.Invalid
	_ *document.PointerError = (*simdjson.PointerError)(nil)
	_ *simdjson.PointerError = (*document.PointerError)(nil)
)

func TestDocumentKindMigrationContract(t *testing.T) {
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
		var root simdjson.Kind = test.kind
		if got := root.String(); got != test.name {
			t.Errorf("Kind(%d).String() = %q, want %q", test.kind, got, test.name)
		}
	}

	rootType := reflect.TypeOf(simdjson.Invalid)
	documentType := reflect.TypeOf(document.Invalid)
	if rootType != documentType {
		t.Fatalf("root Kind type = %v, document Kind type = %v", rootType, documentType)
	}
	if got, want := rootType.PkgPath(), "github.com/thesyncim/simdjson/document"; got != want {
		t.Fatalf("root Kind package path = %q, want %q", got, want)
	}
}

func TestDocumentIndexOptionsMigrationContract(t *testing.T) {
	opts := document.IndexOptions{MaxDepth: 1}
	if _, err := simdjson.BuildIndexOptions([]byte(`[]`), make([]simdjson.IndexEntry, 1), opts); err != nil {
		t.Fatalf("BuildIndexOptions with document.IndexOptions: %v", err)
	}
	if _, err := simdjson.BuildIndexOptions([]byte(`[[]]`), make([]simdjson.IndexEntry, 3), opts); err == nil {
		t.Fatal("BuildIndexOptions with MaxDepth=1 accepted depth 2")
	}
}

func TestDocumentIndexErrorMigrationContract(t *testing.T) {
	src := []byte(`[1]`)
	count, err := simdjson.RequiredIndexEntries(src)
	if err != nil {
		t.Fatal(err)
	}
	_, err = simdjson.BuildIndex(src, make([]simdjson.IndexEntry, count-1))
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
		{document.ErrIndexFull, "simdjson: index entry buffer is full"},
		{document.ErrIndexTooLarge, "simdjson: indexed input exceeds 32-bit offsets"},
	} {
		if got := test.err.Error(); got != test.want {
			t.Errorf("%T.Error() = %q, want %q", test.err, got, test.want)
		}
	}
}

func TestDocumentPointerErrorMigrationContract(t *testing.T) {
	src := []byte(`[1]`)
	index, err := simdjson.BuildIndex(src, make([]simdjson.IndexEntry, 2))
	if err != nil {
		t.Fatal(err)
	}
	value, err := simdjson.Parse(src)
	if err != nil {
		t.Fatal(err)
	}
	_, compileErr := simdjson.CompilePointer("/~2")
	_, _, rawErr := simdjson.GetRaw(src, "not-a-pointer")
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
			var rootErr *simdjson.PointerError
			if !errors.As(test.err, &rootErr) {
				t.Fatalf("error = %T %v, want *simdjson.PointerError", test.err, test.err)
			}
			var documentErr *document.PointerError
			if !errors.As(test.err, &documentErr) {
				t.Fatalf("error = %T %v, want *document.PointerError", test.err, test.err)
			}
			if rootErr != documentErr {
				t.Fatalf("root error = %p, document error = %p", rootErr, documentErr)
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

	rootType := reflect.TypeOf((*simdjson.PointerError)(nil)).Elem()
	documentType := reflect.TypeOf((*document.PointerError)(nil)).Elem()
	if rootType != documentType {
		t.Fatalf("root PointerError type = %v, document PointerError type = %v", rootType, documentType)
	}
	if got, want := rootType.PkgPath(), "github.com/thesyncim/simdjson/document"; got != want {
		t.Fatalf("root PointerError package path = %q, want %q", got, want)
	}
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
