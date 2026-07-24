package stdlibcorpus

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"testing"

	"github.com/thesyncim/vibejson"
)

func TestHighLevelCorpus(t *testing.T) {
	for _, name := range Names {
		name := name
		t.Run(name, func(t *testing.T) {
			src, err := Read(name)
			if err != nil {
				t.Fatal(err)
			}
			checkValidation(t, src)
			checkDynamicDecode(t, src)
			checkNumberDecode(t, src)
			checkIndexRoundTrip(t, src)
			checkTypedCorpus(t, name, src)
		})
	}
}

func checkTypedCorpus(t *testing.T, name string, src []byte) {
	t.Helper()
	switch name {
	case "canada_geometry.json.zst":
		checkTyped[canadaRoot](t, src)
	case "citm_catalog.json.zst":
		checkTyped[citmRoot](t, src)
	case "golang_source.json.zst":
		checkTyped[golangRoot](t, src)
	case "string_escaped.json.zst", "string_unicode.json.zst":
		checkTyped[stringRoot](t, src)
	case "synthea_fhir.json.zst":
		checkTyped[syntheaRoot](t, src)
	case "twitter_status.json.zst":
		checkTyped[twitterRoot](t, src)
	default:
		t.Fatalf("stdlib corpus has no concrete model: %s", name)
	}
}

func checkTyped[T any](t *testing.T, src []byte) {
	t.Helper()
	var want T
	if err := json.Unmarshal(src, &want); err != nil {
		t.Fatalf("encoding/json typed decode: %v", err)
	}

	decoder, err := vibejson.CompileDecoder[T](vibejson.DecoderOptions{})
	if err != nil {
		t.Fatalf("vibejson.CompileDecoder: %v", err)
	}
	var got T
	if err := decoder.Decode(src, &got); err != nil {
		t.Fatalf("vibejson typed decode: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatal("vibejson typed decode result differs from encoding/json")
	}
	zeroCopyDecoder, err := vibejson.CompileDecoder[T](vibejson.DecoderOptions{ZeroCopy: true})
	if err != nil {
		t.Fatalf("vibejson.CompileDecoder zero copy: %v", err)
	}
	var zeroCopy T
	if err := zeroCopyDecoder.Decode(src, &zeroCopy); err != nil {
		t.Fatalf("vibejson typed zero-copy decode: %v", err)
	}
	if !reflect.DeepEqual(zeroCopy, want) {
		t.Fatal("vibejson typed zero-copy decode result differs from encoding/json")
	}

	wantJSON, err := json.Marshal(&want)
	if err != nil {
		t.Fatalf("encoding/json typed encode: %v", err)
	}
	encoder, err := vibejson.CompileEncoder[T](vibejson.EncoderOptions{})
	if err != nil {
		t.Fatalf("vibejson.CompileEncoder: %v", err)
	}
	gotJSON, err := encoder.AppendJSON(nil, &got)
	if err != nil {
		t.Fatalf("vibejson typed encode: %v", err)
	}
	if !bytes.Equal(gotJSON, wantJSON) {
		t.Fatalf("vibejson typed encode differs from encoding/json: got %d bytes, want %d", len(gotJSON), len(wantJSON))
	}
}

func checkValidation(t *testing.T, src []byte) {
	t.Helper()
	if !json.Valid(src) {
		t.Fatal("Go stdlib corpus entry is not valid JSON")
	}
	if !vibejson.Valid(src) {
		t.Fatal("vibejson.Valid rejected valid Go stdlib corpus entry")
	}
	if err := vibejson.Validate(src); err != nil {
		t.Fatalf("vibejson.Validate rejected valid Go stdlib corpus entry: %v", err)
	}
}

func checkDynamicDecode(t *testing.T, src []byte) {
	t.Helper()
	var want any
	if err := json.Unmarshal(src, &want); err != nil {
		t.Fatalf("encoding/json.Unmarshal: %v", err)
	}
	var got any
	if err := vibejson.Unmarshal(src, &got); err != nil {
		t.Fatalf("vibejson.Unmarshal into any: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatal("vibejson dynamic result differs from encoding/json")
	}
	zeroCopyDecoder, err := vibejson.CompileDecoder[any](vibejson.DecoderOptions{ZeroCopy: true})
	if err != nil {
		t.Fatal(err)
	}
	var zeroCopy any
	if err := zeroCopyDecoder.Decode(src, &zeroCopy); err != nil {
		t.Fatalf("vibejson zero-copy dynamic decode: %v", err)
	}
	if !reflect.DeepEqual(zeroCopy, want) {
		t.Fatal("vibejson zero-copy dynamic result differs from encoding/json")
	}

	wantJSON, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("encoding/json.Marshal: %v", err)
	}
	gotJSON, err := vibejson.Marshal(&got)
	if err != nil {
		t.Fatalf("vibejson.Marshal: %v", err)
	}
	if !bytes.Equal(gotJSON, wantJSON) {
		t.Fatal("vibejson.Marshal output differs from encoding/json")
	}
}

func checkNumberDecode(t *testing.T, src []byte) {
	t.Helper()
	stdlibDecoder := json.NewDecoder(bytes.NewReader(src))
	stdlibDecoder.UseNumber()
	var want any
	if err := stdlibDecoder.Decode(&want); err != nil {
		t.Fatalf("encoding/json.Decoder.Decode with UseNumber: %v", err)
	}
	if err := requireEOF(stdlibDecoder); err != nil {
		t.Fatalf("encoding/json.Decoder trailing input: %v", err)
	}

	useNumberDecoder, err := vibejson.CompileDecoder[any](vibejson.DecoderOptions{UseNumber: true})
	if err != nil {
		t.Fatal(err)
	}
	var got any
	if err := useNumberDecoder.Decode(src, &got); err != nil {
		t.Fatalf("vibejson dynamic decode with UseNumber: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatal("vibejson UseNumber result differs from encoding/json")
	}
}

func requireEOF(decoder *json.Decoder) error {
	var trailing any
	err := decoder.Decode(&trailing)
	if err == io.EOF {
		return nil
	}
	if err == nil {
		return errors.New("unexpected second JSON value")
	}
	return err
}

func checkIndexRoundTrip(t *testing.T, src []byte) {
	t.Helper()
	root, err := vibejson.Parse(src)
	if err != nil {
		t.Fatalf("vibejson.Parse: %v", err)
	}
	got := root.AppendJSON(nil)
	if !json.Valid(got) {
		t.Fatal("Value.AppendJSON produced invalid JSON")
	}

	var wantValue, gotValue any
	if err := json.Unmarshal(src, &wantValue); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(got, &gotValue); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Fatal("Value.AppendJSON changed the decoded value")
	}
}
