package slopjson

import (
	"reflect"
	"testing"
	"unsafe"
)

// TestPackedFieldMatchSurvivesFormatting pins the dispatch property used by
// pretty-printed object corpora: insignificant whitespace around members must
// not demote an otherwise declaration-ordered known struct to general key
// lookup for the rest of the document.
func TestPackedFieldMatchSurvivesFormatting(t *testing.T) {
	type document struct {
		Alpha         int    `json:"alpha"`
		LongFieldName string `json:"long_field_name"`
		OK            bool   `json:"ok"`
	}

	decoder, err := CompileDecoder[document](DecoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	src := []byte("{\n  \"alpha\": 7,\n  \"long_field_name\": \"value\",\n  \"ok\": true\n}")
	var got document
	cursor := newDecoderCursor(src, decoder.options)
	if err := cursor.decodeCompiledStruct(decoder.root, unsafe.Pointer(&got)); err != nil {
		t.Fatal(err)
	}
	if err := cursor.Finish(); err != nil {
		t.Fatal(err)
	}
	want := document{Alpha: 7, LongFieldName: "value", OK: true}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("decoded %#v, want %#v", got, want)
	}
	if cursor.flags&decoderExpectedSlow != 0 {
		t.Fatal("formatting whitespace disabled packed field matching")
	}
}
