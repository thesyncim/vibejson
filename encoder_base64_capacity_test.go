package vibejson

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"
	"unsafe"
)

type encoderBase64Byte uint8
type encoderBase64NamedBytes []encoderBase64Byte

func TestEncoderBase64CapacityAndBounds(t *testing.T) {
	values := [][]byte{nil, {}}
	for _, size := range []int{1, 2, 3, 6, 7, 15, 16, 17, 23, 24, 31, 32, 33, 63, 64, 65, 96, 1024, 4096} {
		values = append(values, base64CapacityPayload(size))
	}
	checkEncoderBase64Bounds(t, values)
	named := make([]encoderBase64NamedBytes, len(values))
	for i, value := range values {
		if value != nil {
			named[i] = make(encoderBase64NamedBytes, len(value))
			for j, b := range value {
				named[i][j] = encoderBase64Byte(b)
			}
		}
	}
	checkEncoderBase64Bounds(t, named)
}

func checkEncoderBase64Bounds[T any](t *testing.T, values []T) {
	t.Helper()
	encoder, err := CompileEncoder[T](EncoderOptions{})
	requireNoTestError(t, err)
	for index, value := range values {
		encoded, err := json.Marshal(value)
		requireNoTestError(t, err)
		for _, prefix := range []string{"", "p", "pre", "\x00\xff"} {
			for _, capacity := range []int{len(prefix), len(prefix) + len(encoded) - 1, len(prefix) + len(encoded), len(prefix) + len(encoded) + 1, len(prefix) + len(encoded) + 32} {
				t.Run(fmt.Sprintf("%T/case=%d/prefix=%d/cap=%d", value, index, len(prefix), capacity), func(t *testing.T) {
					storage := bytes.Repeat([]byte{0xa5}, capacity+16)
					dst := storage[8 : 8+len(prefix) : 8+capacity]
					copy(dst, prefix)
					before := unsafe.SliceData(dst)
					got, err := encoder.AppendJSON(dst, &value)
					want := append([]byte(prefix), encoded...)
					if err != nil || !bytes.Equal(got, want) {
						t.Fatalf("AppendJSON = %q, %v; want %q", got, err, want)
					}
					if capacity >= len(want) && unsafe.SliceData(got) != before {
						t.Fatal("AppendJSON did not reuse sufficient destination capacity")
					}
					if !bytes.Equal(storage[8:8+len(prefix)], []byte(prefix)) {
						t.Fatal("AppendJSON changed the visible destination prefix")
					}
					tail := 8 + capacity
					if unsafe.SliceData(got) == before {
						tail = 8 + len(got)
					}
					if !bytes.Equal(storage[:8], bytes.Repeat([]byte{0xa5}, 8)) || !bytes.Equal(storage[tail:], bytes.Repeat([]byte{0xa5}, len(storage)-tail)) {
						t.Fatal("AppendJSON wrote outside the result or destination capacity")
					}
					after, err := json.Marshal(value)
					if err != nil || !bytes.Equal(after, encoded) {
						t.Fatal("AppendJSON changed the source")
					}
				})
			}
		}
	}
}

func TestEncoderBase64CapacityAllocations(t *testing.T) {
	encoder, err := CompileEncoder[[]byte](EncoderOptions{})
	requireNoTestError(t, err)
	for _, size := range []int{0, 1, 32, 1024} {
		value := base64CapacityPayload(size)
		encoded, err := json.Marshal(value)
		requireNoTestError(t, err)
		var dst []byte
		if allocs := testing.AllocsPerRun(100, func() {
			var err error
			dst, err = encoder.AppendJSON(nil, &value)
			if err != nil {
				panic(err)
			}
		}); allocs != 1 {
			t.Fatalf("size=%d: fresh output allocated %v times, want 1", size, allocs)
		}
		storage := make([]byte, 0, len(encoded))
		if allocs := testing.AllocsPerRun(100, func() {
			var err error
			dst, err = encoder.AppendJSON(storage, &value)
			if err != nil {
				panic(err)
			}
		}); allocs != 0 {
			t.Fatalf("size=%d: exact-capacity output allocated %v times, want 0", size, allocs)
		}
		if !bytes.Equal(dst, encoded) {
			t.Fatal("allocation test produced incorrect output")
		}
		prefix := []byte("pre")
		prefix = prefix[:len(prefix):len(prefix)]
		if allocs := testing.AllocsPerRun(100, func() {
			var err error
			dst, err = encoder.AppendJSON(prefix, &value)
			if err != nil {
				panic(err)
			}
		}); allocs != 1 {
			t.Fatalf("size=%d: tiny-prefix output allocated %v times, want 1", size, allocs)
		}
	}
}
