package vibejson

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

type decodeCyclePointer *decodeCyclePointer

type decodeCyclePointerA *decodeCyclePointerB
type decodeCyclePointerB *decodeCyclePointerA

type decodeCycleMarker interface{ decodeCycleMarker() }

type decodeCycleObject struct {
	Value any
}

func (*decodeCycleObject) decodeCycleMarker() {}

type decodeCycleJSONHook struct {
	Self  *decodeCycleJSONHook
	Value int
}

func (v *decodeCycleJSONHook) UnmarshalJSON(src []byte) error {
	return json.Unmarshal(src, &v.Value)
}

type decodeCycleNativeHook struct {
	Self  *decodeCycleNativeHook
	Value int
}

func (v *decodeCycleNativeHook) UnmarshalVibeJSON(cursor DecodeCursor) (DecodeCursor, error) {
	err := cursor.Int(&v.Value)
	return cursor, err
}

func TestDecoderRejectsExistingPointerCycles(t *testing.T) {
	for _, inline := range []bool{false, true} {
		name := "ordinary"
		if inline {
			name = "inline"
		}
		t.Run(name, func(t *testing.T) {
			decoder := mustCompileTestDecoder[any](t, DecoderOptions{InlineFields: inline})
			t.Run("direct", func(t *testing.T) {
				var dst any
				dst = &dst
				err := decoder.Decode([]byte(`1`), &dst)
				assertDecodePointerCycle(t, err, 0)
				if dst != &dst {
					t.Fatal("cycle rejection changed the destination")
				}
			})
			t.Run("two-interfaces", func(t *testing.T) {
				var first, second any
				first, second = &second, &first
				err := decoder.Decode([]byte(`[1]`), &first)
				assertDecodePointerCycle(t, err, 0)
				if first != &second || second != &first {
					t.Fatal("cycle rejection changed an interface")
				}
			})
			t.Run("pointer-only", func(t *testing.T) {
				var pointer decodeCyclePointer
				pointer = &pointer
				var dst any = pointer
				assertDecodePointerCycle(t, decoder.Decode([]byte(`1`), &dst), 0)
			})
			t.Run("prefix", func(t *testing.T) {
				var dst any
				dst = &dst
				src := []byte(" \t1 trailing")
				consumed, err := decoder.DecodePrefix(src, &dst)
				assertDecodePointerCycle(t, err, 2)
				if consumed != 2 || string(src) != " \t1 trailing" {
					t.Fatalf("cycle consumed input: consumed=%d src=%q", consumed, src)
				}
			})
			t.Run("null", func(t *testing.T) {
				var dst any
				dst = &dst
				requireNoTestError(t, decoder.Decode([]byte(`null`), &dst))
				if dst != nil {
					t.Fatalf("null destination = %#v", dst)
				}
			})
			t.Run("nonempty-interface-container", func(t *testing.T) {
				object := &decodeCycleObject{}
				object.Value = &object.Value
				var dst decodeCycleMarker = object
				markerDecoder := mustCompileTestDecoder[decodeCycleMarker](t, DecoderOptions{InlineFields: inline})
				assertDecodePointerCycle(t, markerDecoder.Decode([]byte(`{"Value":1}`), &dst), 9)
			})
		})
	}
}

func assertDecodePointerCycle(t *testing.T, err error, offset int) {
	t.Helper()
	var decodeErr *DecodeError
	if !errors.As(err, &decodeErr) || decodeErr.Offset != offset || decodeErr.Reason != "cyclic pointer chain in decode destination" {
		t.Fatalf("cycle error = %v, want DecodeError at %d", err, offset)
	}
}

func TestDecoderRejectsRecursivePointerTypes(t *testing.T) {
	for _, replace := range []bool{false, true} {
		name := "merge"
		if replace {
			name = "replace"
		}
		t.Run(name, func(t *testing.T) {
			opts := DecoderOptions{Replace: replace, MaxDepth: 1}
			decoder := mustCompileTestDecoder[decodeCyclePointer](t, opts)
			for _, initialized := range []bool{false, true} {
				var dst decodeCyclePointer
				if initialized {
					dst = &dst
				}
				assertDecodePointerCycle(t, decoder.Decode([]byte(`1`), &dst), 0)
				requireNoTestError(t, decoder.Decode([]byte(`null`), &dst))
				if dst != nil {
					t.Fatal("null did not clear a recursive pointer")
				}
			}
			pairDecoder := mustCompileTestDecoder[decodeCyclePointerA](t, opts)
			for _, initialized := range []bool{false, true} {
				var first decodeCyclePointerA
				var second decodeCyclePointerB
				if initialized {
					first, second = &second, &first
				}
				assertDecodePointerCycle(t, pairDecoder.Decode([]byte(`1`), &first), 0)
				requireNoTestError(t, pairDecoder.Decode([]byte(`null`), &first))
				if first != nil {
					t.Fatal("null did not clear a mutually recursive pointer")
				}
			}
			finiteDecoder := mustCompileTestDecoder[***int](t, opts)
			var finite ***int
			requireNoTestError(t, finiteDecoder.Decode([]byte(`7`), &finite))
			if finite == nil || *finite == nil || **finite == nil || ***finite != 7 {
				t.Fatal("finite nil pointer chain did not allocate its destination")
			}
		})
	}
	for _, inline := range []bool{false, true} {
		decoder := mustCompileTestDecoder[any](t, DecoderOptions{InlineFields: inline})
		var pointer decodeCyclePointer
		var dst any = &pointer
		assertDecodePointerCycle(t, decoder.Decode([]byte(`1`), &dst), 0)
	}
}

func TestDecoderExistingPointerChainsMatchStdlib(t *testing.T) {
	for _, inline := range []bool{false, true} {
		decoder := mustCompileTestDecoder[any](t, DecoderOptions{MaxDepth: 1, InlineFields: inline})
		for _, src := range []string{`7`, `null`} {
			for _, stdlib := range []bool{false, true} {
				number := 3
				pointer := &number
				var inner any = &pointer
				var dst any = &inner
				var err error
				if stdlib {
					err = json.Unmarshal([]byte(src), &dst)
				} else {
					err = decoder.Decode([]byte(src), &dst)
				}
				requireNoTestError(t, err)
				if src == `null` {
					if dst != nil || number != 3 {
						t.Fatalf("null merge: stdlib=%v dst=%#v number=%d", stdlib, dst, number)
					}
				} else if dst != &inner || inner != &pointer || pointer != &number || number != 7 {
					t.Fatalf("pointer merge: stdlib=%v dst=%#v number=%d", stdlib, dst, number)
				}
			}
		}
		number := 3
		chain := make([]any, 1024)
		chain[len(chain)-1] = &number
		for i := len(chain) - 2; i >= 0; i-- {
			chain[i] = &chain[i+1]
		}
		var dst any = &chain[0]
		requireNoTestError(t, decoder.Decode([]byte(`7`), &dst))
		if dst != &chain[0] || number != 7 {
			t.Fatal("long pointer chain did not update its original destination")
		}
	}
}

func TestDecoderExistingPointerCustomMethodBoundary(t *testing.T) {
	for _, inline := range []bool{false, true} {
		decoder := mustCompileTestDecoder[any](t, DecoderOptions{InlineFields: inline})
		standard := &decodeCycleJSONHook{}
		standard.Self = standard
		native := &decodeCycleNativeHook{}
		native.Self = native
		for _, target := range []any{standard, native} {
			var dst any = target
			src := []byte("7 trailing")
			consumed, err := decoder.DecodePrefix(src, &dst)
			requireNoTestError(t, err)
			if consumed != 1 || dst != target || string(src) != "7 trailing" {
				t.Fatalf("custom pointer merge: consumed=%d dst=%#v src=%q", consumed, dst, src)
			}
		}
		if standard.Value != 7 || standard.Self != standard || native.Value != 7 || native.Self != native {
			t.Fatal("custom method did not preserve its cyclic object graph")
		}
		var expected any = &decodeCycleJSONHook{}
		requireNoTestError(t, json.Unmarshal([]byte(`7`), &expected))
		if standard.Value != expected.(*decodeCycleJSONHook).Value {
			t.Fatal("standard custom method differed from encoding/json")
		}
	}
}

func TestDecoderLongFinitePointerChains(t *testing.T) {
	t.Run("32", func(t *testing.T) { checkLongFinitePointerChain[pointerBenchChain32](t) })
	t.Run("128", func(t *testing.T) { checkLongFinitePointerChain[pointerBenchChain128](t) })
}

func checkLongFinitePointerChain[T any](t *testing.T) {
	t.Helper()
	for _, replace := range []bool{false, true} {
		decoder := mustCompileTestDecoder[T](t, DecoderOptions{MaxDepth: 1, Replace: replace})
		var dst, want T
		requireNoTestError(t, decoder.Decode([]byte(`7`), &dst))
		pointers := decodedPointerChainAddresses(&dst)
		if err := decoder.Decode([]byte(`false`), &dst); err == nil {
			t.Fatal("pointer chain accepted a boolean into an integer")
		}
		requireNoTestError(t, decoder.Decode([]byte(`9`), &dst))
		requireNoTestError(t, json.Unmarshal([]byte(`9`), &want))
		if !reflect.DeepEqual(dst, want) {
			t.Fatalf("replace=%v: pointer chain differed from encoding/json", replace)
		}
		if !reflect.DeepEqual(pointers, decodedPointerChainAddresses(&dst)) {
			t.Fatalf("replace=%v: a later decode replaced unique pointer storage", replace)
		}
		requireNoTestError(t, decoder.Decode([]byte(`null`), &dst))
		if !reflect.ValueOf(dst).IsNil() {
			t.Fatalf("replace=%v: null did not clear the pointer chain", replace)
		}
	}
}

func decodedPointerChainAddresses[T any](dst *T) []uintptr {
	var pointers []uintptr
	for value := reflect.ValueOf(dst).Elem(); value.Kind() == reflect.Pointer && !value.IsNil(); value = value.Elem() {
		pointers = append(pointers, value.Pointer())
	}
	return pointers
}
