package vibejson

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

type compileRecoverySliceHook struct {
	Unsupported []chan int
}

func (*compileRecoverySliceHook) UnmarshalJSON([]byte) error  { return nil }
func (compileRecoverySliceHook) MarshalJSON() ([]byte, error) { return []byte(`null`), nil }

type compileRecoveryRecursive struct {
	Self        *compileRecoveryRecursive `json:"self"`
	Unsupported chan int                  `json:"unsupported"`
}

type compileRecoveryRecursiveHook struct {
	Child compileRecoveryRecursive
}

func (*compileRecoveryRecursiveHook) UnmarshalJSON([]byte) error  { return nil }
func (compileRecoveryRecursiveHook) MarshalJSON() ([]byte, error) { return []byte(`null`), nil }

func requireCompileRecoveryRejection[T any](t *testing.T, suffix string) {
	t.Helper()
	wantPath := reflect.TypeFor[T]().String() + suffix
	check := func(t *testing.T, err error) {
		t.Helper()
		var unsupported *UnsupportedTypeError
		if !errors.As(err, &unsupported) {
			t.Fatalf("compile error = %v, want UnsupportedTypeError", err)
		}
		if unsupported.Type != reflect.TypeFor[chan int]() || unsupported.Path != wantPath {
			t.Fatalf("compile error = %#v, want chan int at %s", unsupported, wantPath)
		}
	}
	for _, replace := range []bool{false, true} {
		decoder, err := CompileDecoder[T](DecoderOptions{Replace: replace})
		check(t, err)
		if decoder.root != nil {
			t.Fatal("failed compilation published a decoder")
		}
	}
	encoder, err := CompileEncoder[T](EncoderOptions{})
	check(t, err)
	if encoder.root != nil {
		t.Fatal("failed compilation published an encoder")
	}
}

func TestCompileRecoveryRejectsFailedChild(t *testing.T) {
	type document struct {
		Hook compileRecoverySliceHook `json:"hook"`
		Bad  []chan int               `json:"bad"`
	}
	requireCompileRecoveryRejection[document](t, ".bad[]")
}

func TestCompileRecoveryRejectsFailedRecursiveDescendant(t *testing.T) {
	type document struct {
		Hook compileRecoveryRecursiveHook `json:"hook"`
		Bad  *compileRecoveryRecursive    `json:"bad"`
	}
	// The recursive pointer node completes before its pointee fails. Removing
	// only nodes whose compilation returned an error leaves that pointer cached.
	requireCompileRecoveryRejection[document](t, ".bad*.unsupported")
}

type compileRecoveryResourceHook struct {
	Values      map[int]string
	Unsupported []chan int
	Seen        bool `json:"-"`
}

func (h *compileRecoveryResourceHook) UnmarshalJSON([]byte) error {
	h.Seen = true
	return nil
}

func (compileRecoveryResourceHook) MarshalJSON() ([]byte, error) {
	return []byte(`"hook"`), nil
}

func TestCompileRecoveryPreservesEarlierNodesAndRebuildsMapResources(t *testing.T) {
	type document struct {
		Before map[string]int              `json:"before"`
		Hook   compileRecoveryResourceHook `json:"hook"`
		After  map[int]string              `json:"after"`
		Again  map[string]int              `json:"again"`
	}
	src := []byte(`{"before":{"a":1},"hook":"hook","after":{"2":"two"},"again":{"b":3}}`)
	var want document
	requireNoTestError(t, json.Unmarshal(src, &want))
	for _, replace := range []bool{false, true} {
		decoder := mustCompileTestDecoder[document](t, DecoderOptions{Replace: replace})
		var got document
		requireNoTestError(t, decoder.Decode(src, &got))
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("recovered hook graph decoded %#v, want %#v", got, want)
		}
	}
	encoder, err := CompileEncoder[document](EncoderOptions{})
	requireNoTestError(t, err)
	for range 2 {
		got, err := encoder.AppendJSON(nil, &want)
		requireNoTestError(t, err)
		if string(got) != string(src) {
			t.Fatalf("recovered map resources encoded %s, want %s", got, src)
		}
	}
}

func TestCompileRecoveryAcceptsHooksWithUnsupportedLayouts(t *testing.T) {
	type document struct {
		First  compileRecoveryRecursiveHook `json:"first"`
		Second compileRecoveryRecursiveHook `json:"second"`
	}
	decoder := mustCompileTestDecoder[document](t, DecoderOptions{Replace: true})
	var value document
	requireNoTestError(t, decoder.Decode([]byte(`{"first":null,"second":null}`), &value))
	encoder, err := CompileEncoder[document](EncoderOptions{})
	requireNoTestError(t, err)
	got, err := encoder.AppendJSON(nil, &value)
	requireNoTestError(t, err)
	if string(got) != `{"first":null,"second":null}` {
		t.Fatalf("recovered hooks encoded %s", got)
	}
}
