package vibejson

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
)

type fusionDepthBox[T any] struct {
	Value T `json:"v"`
}

type fusionDepthPointerNode struct {
	Inner struct {
		Next *fusionDepthPointerNode `json:"n"`
	} `json:"i"`
}

type fusionDepthSliceNode struct {
	Inner struct {
		Next []fusionDepthSliceNode `json:"n"`
	} `json:"i"`
}

type fusionDepthMapNode struct {
	Inner struct {
		Next map[string]fusionDepthMapNode `json:"n"`
	} `json:"i"`
}

func TestEncoderFusionPreservesRecursiveDepth(t *testing.T) {
	t.Run("pointer", func(t *testing.T) {
		const links = DefaultMaxDepth / 2
		var value fusionDepthPointerNode
		current := &value
		for i := 1; i < links; i++ {
			current.Inner.Next = &fusionDepthPointerNode{}
			current = current.Inner.Next
		}
		checkFusionDepthBoundary(t, &value)
	})
	t.Run("slice", func(t *testing.T) {
		const links = DefaultMaxDepth / 3
		var value fusionDepthSliceNode
		for i := 1; i < links; i++ {
			var parent fusionDepthSliceNode
			parent.Inner.Next = []fusionDepthSliceNode{value}
			value = parent
		}
		// Each link contributes two structs and, except at the nil tail,
		// one slice. Two boxes bring the depth from 9998 to exactly 10000.
		boxed := fusionDepthBox[fusionDepthBox[*fusionDepthSliceNode]]{
			Value: fusionDepthBox[*fusionDepthSliceNode]{Value: &value},
		}
		checkFusionDepthBoundary(t, &boxed)
	})
	t.Run("map", func(t *testing.T) {
		const links = DefaultMaxDepth / 3
		var value fusionDepthMapNode
		for i := 1; i < links; i++ {
			var parent fusionDepthMapNode
			parent.Inner.Next = map[string]fusionDepthMapNode{"v": value}
			value = parent
		}
		boxed := fusionDepthBox[fusionDepthBox[*fusionDepthMapNode]]{
			Value: fusionDepthBox[*fusionDepthMapNode]{Value: &value},
		}
		checkFusionDepthBoundary(t, &boxed)
	})
}

func TestEncoderFusionPreservesDeepStaticDepth(t *testing.T) {
	// More than 255 nested structs exercises fused-depth bookkeeping beyond
	// a byte while keeping the reflected type itself reasonably small.
	const depth = 257
	typ := reflect.TypeFor[int]()
	for range depth {
		typ = reflect.StructOf([]reflect.StructField{{
			Name: "V", Type: typ, Tag: `json:"v"`,
		}})
	}
	compiler := newTypedCompiler(typedCompileEncode)
	node, err := compiler.compile(typ, typ.String())
	if err != nil {
		t.Fatal(err)
	}
	value := reflect.New(typ)
	for _, nesting := range []int{DefaultMaxDepth, DefaultMaxDepth + 1} {
		t.Run(fmt.Sprint(nesting), func(t *testing.T) {
			parentDepth := nesting - depth
			prefix := bytes.Repeat([]byte{'['}, parentDepth)
			state := encodeState{dst: prefix, depth: parentDepth, escapeHTML: true}
			// Enter the reflected plan at the depth of its enclosing arrays.
			// This avoids depending on interface-dispatch depth accounting.
			err := state.encode(node, value.UnsafePointer())
			var source any = value.Interface()
			for range parentDepth {
				source = []any{source}
			}
			want, stdErr := json.Marshal(source)
			wantOK := nesting <= DefaultMaxDepth
			if (err == nil) != wantOK || (stdErr == nil) != wantOK {
				t.Fatalf("depth %d: encoder error = %.200v, encoding/json error = %.200v; want success = %v", nesting, err, stdErr, wantOK)
			}
			if err == nil {
				got := append(state.dst, bytes.Repeat([]byte{']'}, parentDepth)...)
				if !bytes.Equal(got, want) {
					t.Fatal("encoded output differs from encoding/json")
				}
				if err := Validate(got); err != nil {
					t.Fatalf("encoded output failed validation: %v", err)
				}
			}
		})
	}
}

func checkFusionDepthBoundary[T any](t *testing.T, value *T) {
	t.Helper()
	t.Run(fmt.Sprint(DefaultMaxDepth), func(t *testing.T) {
		checkFusionDepthResult(t, value, true)
	})
	t.Run(fmt.Sprint(DefaultMaxDepth+1), func(t *testing.T) {
		boxed := fusionDepthBox[*T]{Value: value}
		checkFusionDepthResult(t, &boxed, false)
	})
}

func checkFusionDepthResult[T any](t *testing.T, value *T, wantOK bool) {
	t.Helper()
	got, gotErr := Marshal(value)
	want, wantErr := json.Marshal(value)
	if (gotErr == nil) != wantOK || (wantErr == nil) != wantOK {
		t.Fatalf("vibejson error = %.200v, encoding/json error = %.200v; want success = %v", gotErr, wantErr, wantOK)
	}
	if gotErr == nil {
		if !bytes.Equal(got, want) {
			t.Fatal("encoded output differs from encoding/json")
		}
		if err := Validate(got); err != nil {
			t.Fatalf("Marshal emitted JSON that Validate rejects: %v", err)
		}
	}
}
