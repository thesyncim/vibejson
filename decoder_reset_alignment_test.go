package vibejson

import (
	"encoding/json"
	"reflect"
	"testing"
	"unsafe"
)

func TestDecoderResetAlignment(t *testing.T) {
	t.Run("bytes2", testDecoderResetAlignment[[2]byte])
	t.Run("bytes4", testDecoderResetAlignment[[4]byte])
	t.Run("bytes8", testDecoderResetAlignment[[8]byte])
	t.Run("uint16s2", testDecoderResetAlignment[[2]uint16])
	t.Run("uint16s4", testDecoderResetAlignment[[4]uint16])
	t.Run("uint32s2", testDecoderResetAlignment[[2]uint32])
	t.Run("uint16", testDecoderResetAlignment[uint16])
	t.Run("uint32", testDecoderResetAlignment[uint32])
	t.Run("uint64", testDecoderResetAlignment[uint64])
	t.Run("nested", testDecoderResetAlignment[struct {
		Prefix byte
		Data   [8]byte
		Suffix byte
	}])
	t.Run("array_tail", func(t *testing.T) {
		// Merge decoding also resets array elements absent from the input.
		testDecoderResetAlignmentInputs[[2][8]byte](t, false, []string{"null", "[]", "[[]]", "[[1]]"})
	})
}

func testDecoderResetAlignment[T any](t *testing.T) {
	inputs := []string{"null"}
	switch reflect.TypeFor[T]().Kind() {
	case reflect.Array:
		inputs = append(inputs, "[]", "[1]", "[1,2]")
	case reflect.Struct:
		inputs = append(inputs, `{}`, `{"Data":[]}`, `{"Data":[7]}`)
	default:
		inputs = append(inputs, "1")
	}
	testDecoderResetAlignmentInputs[T](t, true, inputs)
}

// All instantiations are pointer-free numeric layouts. Byte backing lets the
// destination have only the alignment its type promises, including arrays
// inside byte-aligned records rather than accidentally aligned heap objects.
func testDecoderResetAlignmentInputs[T any](t *testing.T, replace bool, inputs []string) {
	typ := reflect.TypeFor[T]()
	size, alignment := int(typ.Size()), uintptr(typ.Align())
	backing := make([]byte, size+32)
	offset := 1
	for uintptr(unsafe.Pointer(&backing[offset]))%8 != alignment%8 {
		offset++
	}
	dst := (*T)(unsafe.Pointer(&backing[offset]))
	decoder := mustCompileTestDecoder[T](t, DecoderOptions{Replace: replace})

	// Native hosts may tolerate unaligned stores. Check the compiled writes'
	// alignment requirements too, so the regression is observable without a
	// strict-alignment CPU; the public decode below checks their behavior.
	for node := decoder.root; node != nil; node = node.elem {
		for _, op := range node.reset {
			var required uintptr
			switch op.kind {
			case typedResetUint16:
				required = unsafe.Alignof(uint16(0))
			case typedResetUint32:
				required = unsafe.Alignof(uint32(0))
			case typedResetUint64:
				required = unsafe.Alignof(uint64(0))
			default:
				continue
			}
			if address := uintptr(unsafe.Pointer(dst)) + op.offset; address%required != 0 {
				t.Fatalf("reset store at offset %d requires alignment %d", op.offset, required)
			}
		}
	}

	for _, input := range inputs {
		for i := range backing {
			backing[i] = 0xa5
		}
		var want T
		if !replace {
			want = *dst
		}
		if err := json.Unmarshal([]byte(input), &want); err != nil {
			t.Fatal(err)
		}
		if err := decoder.Decode([]byte(input), dst); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(*dst, want) {
			t.Fatalf("%s: got %#v, want %#v", input, *dst, want)
		}
		for i, b := range backing {
			if (i < offset || i >= offset+size) && b != 0xa5 {
				t.Fatalf("%s: reset changed byte %d outside destination", input, i)
			}
		}
	}
}
