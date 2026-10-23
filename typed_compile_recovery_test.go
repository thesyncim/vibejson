package vibejson

import (
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
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

type compileRecoveryJSONScalar int64

func (v compileRecoveryJSONScalar) MarshalJSON() ([]byte, error) {
	return json.Marshal([]int64{int64(v)})
}

type compileRecoveryTextScalar int64

func (v compileRecoveryTextScalar) MarshalText() ([]byte, error) {
	return strconv.AppendInt([]byte("text:"), int64(v), 10), nil
}

type compileRecoveryScalarHook[T any] struct {
	Value       T
	Unsupported chan int
}

func (compileRecoveryScalarHook[T]) MarshalJSON() ([]byte, error) {
	return []byte(`"hook"`), nil
}

func TestCompileRecoveryScalarMarshalerResources(t *testing.T) {
	t.Run("JSON", testCompileRecoveryScalarResources[compileRecoveryJSONScalar])
	t.Run("text", testCompileRecoveryScalarResources[compileRecoveryTextScalar])
}

func testCompileRecoveryScalarResources[T ~int64](t *testing.T) {
	t.Helper()
	// Value's type first appears under a failed structural branch. Even though
	// it has no child nodes, its value method owns an encoder scratch slot that
	// must be rebuilt after the enclosing hook rolls back that reservation.
	type document struct {
		Hook  compileRecoveryScalarHook[T] `json:"hook"`
		Value T                            `json:"value"`
	}
	type existingDocument struct {
		Before T                            `json:"before"`
		Hook   compileRecoveryScalarHook[T] `json:"hook"`
		After  T                            `json:"after"`
	}
	encoder := mustCompileTestEncoder[document](t, EncoderOptions{})
	existingEncoder := mustCompileTestEncoder[existingDocument](t, EncoderOptions{})
	for _, n := range []T{7, -19, 1 << 40} {
		value := document{Value: n}
		got, err := encoder.AppendJSON(nil, &value)
		requireNoTestError(t, err)
		want, err := json.Marshal(value)
		requireNoTestError(t, err)
		if string(got) != string(want) {
			t.Fatalf("rebuilt scalar scratch encoded %s, want %s", got, want)
		}
		// A node created before the failed branch must keep its original slot.
		existing := existingDocument{Before: n, After: n + 1}
		got, err = existingEncoder.AppendJSON(nil, &existing)
		requireNoTestError(t, err)
		want, err = json.Marshal(existing)
		requireNoTestError(t, err)
		if string(got) != string(want) {
			t.Fatalf("preserved scalar scratch encoded %s, want %s", got, want)
		}
	}
}
