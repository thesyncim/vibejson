package vibejson

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
)

func wideFieldMetadataType(count int) reflect.Type {
	fields := make([]reflect.StructField, count)
	for i := range fields {
		fields[i] = reflect.StructField{Name: fmt.Sprintf("F%05d", i), Type: reflect.TypeFor[uint8]()}
	}
	return reflect.StructOf(fields)
}

func TestDecoderFieldTableBeyondInt16(t *testing.T) {
	const count = 1<<15 + 1
	typ := wideFieldMetadataType(count)
	compiler := newTypedCompiler(typedCompileDecode)
	// Use a short diagnostic path: repeating this anonymous type's enormous
	// spelling for every field would test path construction, not lookup.
	node, err := compiler.compile(typ, "wide")
	if err != nil {
		t.Fatal(err)
	}
	dst := reflect.New(typ)
	input := []byte(`{"F32768":7,"F32767":8,"F32766":9,"F00000":10,"f00001":11}`)
	if err := decodeTypedDocument(input, DecoderOptions{MaxDepth: DefaultMaxDepth}, node, dst.UnsafePointer(), nil); err != nil {
		t.Fatal(err)
	}
	expected := map[int]int64{32768: 7, 32767: 8, 32766: 9, 0: 10, 1: 11}
	for i := 0; i < count; i++ {
		if got := int64(dst.Elem().Field(i).Uint()); got != expected[i] {
			t.Fatalf("field %d = %d, want %d", i, got, expected[i])
		}
	}
}

func TestCompilerRejectsUnrepresentableFieldHops(t *testing.T) {
	for _, count := range []int{1 << 15, 1<<15 + 1} {
		// One embedded pointer promotes all fields, giving each selected field
		// its own hop path without constructing thousands of nested types.
		typ := reflect.StructOf([]reflect.StructField{{
			Name: "Embedded", Type: reflect.PointerTo(wideFieldMetadataType(count)), Anonymous: true,
		}})
		for _, mode := range []typedCompileMode{typedCompileDecode, typedCompileEncode} {
			t.Run(fmt.Sprintf("fields%d/mode%d", count, mode), func(t *testing.T) {
				compiler := newTypedCompiler(mode)
				node, err := compiler.compile(typ, "embedded")
				if count > 1<<15 {
					var unsupported *UnsupportedTypeError
					if !errors.As(err, &unsupported) || unsupported.Type != typ {
						t.Fatalf("compile error type = %T, want UnsupportedTypeError for embedded record", err)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if got := len(node.fieldHops); got != count {
					t.Fatalf("hop paths = %d, want %d", got, count)
				}
				if mode == typedCompileDecode {
					dst := reflect.New(typ)
					if err := decodeTypedDocument([]byte(`{"F32767":7}`), DecoderOptions{MaxDepth: DefaultMaxDepth}, node, dst.UnsafePointer(), nil); err != nil {
						t.Fatal(err)
					}
					if got := dst.Elem().Field(0).Elem().Field(count - 1).Uint(); got != 7 {
						t.Fatalf("last representable hop decoded %d, want 7", got)
					}
				}
			})
		}
	}
}
