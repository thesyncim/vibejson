package vibejson

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"testing"
	"unsafe"
)

func base64CapacityPayload(size int) []byte {
	value := make([]byte, size)
	for i := range value {
		value[i] = byte(i*37 + 11)
	}
	return value
}

func TestDecodeBase64ExactCapacity(t *testing.T) {
	for _, replace := range []bool{false, true} {
		decoder := mustCompileTestDecoder[[]byte](t, DecoderOptions{Replace: replace})
		for _, size := range []int{1, 2, 3, 16, 31, 32, 33, 63, 64, 65, 96, 1024} {
			t.Run(fmt.Sprintf("replace=%v/size=%d", replace, size), func(t *testing.T) {
				want := base64CapacityPayload(size)
				src, err := json.Marshal(want)
				requireNoTestError(t, err)
				backing := bytes.Repeat([]byte{0xa5}, size+16)
				dst := backing[8 : 8+size : 8+size]
				before := unsafe.SliceData(dst)
				requireNoTestError(t, decoder.Decode(src, &dst))
				if !bytes.Equal(dst, want) || unsafe.SliceData(dst) != before || cap(dst) != size {
					t.Fatalf("decode did not reuse exact capacity: valueMatches=%v cap=%d reused=%v", bytes.Equal(dst, want), cap(dst), unsafe.SliceData(dst) == before)
				}
				if !bytes.Equal(backing[:8], bytes.Repeat([]byte{0xa5}, 8)) || !bytes.Equal(backing[8+size:], bytes.Repeat([]byte{0xa5}, 8)) {
					t.Fatal("decode wrote outside destination capacity")
				}
			})
		}
	}
}

func TestDecodeBase64ExactCapacityAllocations(t *testing.T) {
	for _, replace := range []bool{false, true} {
		decoder := mustCompileTestDecoder[[]byte](t, DecoderOptions{Replace: replace})
		for _, size := range []int{32, 64} {
			src, err := json.Marshal(base64CapacityPayload(size))
			requireNoTestError(t, err)
			storage := make([]byte, size)
			dst := storage[:0]
			if allocs := testing.AllocsPerRun(100, func() {
				dst = storage[:0]
				if err := decoder.Decode(src, &dst); err != nil {
					panic(err)
				}
			}); allocs != 0 {
				t.Fatalf("replace=%v size=%d: allocated %v times", replace, size, allocs)
			}
		}
	}
}

func TestDecodeBase64PaddingAndWriteBounds(t *testing.T) {
	cases := []string{"", "AA==", "AAA=", "AAAA", "AA", "A===", "AAAA==", "AAAAA===", "AA= =", "AQ==\r\n", "AAAA\r\n", "\r\nAA==\n", "\nAA=\n=\r\n", "AAAAAA==X", "AAAA==\r\n"}
	alphabet := []byte("A/=\r\n!")
	var add func([]byte, int)
	add = func(prefix []byte, remaining int) {
		cases = append(cases, string(prefix))
		if remaining > 0 {
			for _, ch := range alphabet {
				add(append(prefix, ch), remaining-1)
			}
		}
	}
	add(nil, 4)
	rng := rand.New(rand.NewSource(1))
	for range 512 {
		encoded := []byte(base64.StdEncoding.EncodeToString(base64CapacityPayload(rng.Intn(128))))
		for range 1 + rng.Intn(4) {
			if len(encoded) > 0 {
				encoded[rng.Intn(len(encoded))] = alphabet[rng.Intn(len(alphabet))]
			}
		}
		cases = append(cases, string(encoded))
	}
	decoder := mustCompileTestDecoder[[]byte](t, DecoderOptions{})
	for _, encoded := range cases {
		want, wantErr := base64.StdEncoding.DecodeString(encoded)
		src, err := json.Marshal(encoded)
		requireNoTestError(t, err)
		bound := base64.StdEncoding.DecodedLen(len(encoded))
		for _, capacity := range []int{0, 1, max(0, bound-2), max(0, bound-1), bound} {
			backing := bytes.Repeat([]byte{0xa5}, capacity+16)
			dst := backing[8 : 8+capacity : 8+capacity]
			before := unsafe.SliceData(dst)
			err := decoder.Decode(src, &dst)
			if wantErr == nil {
				if err != nil || !bytes.Equal(dst, want) || dst == nil {
					t.Fatalf("%q cap=%d: got %x, %v; want %x", encoded, capacity, dst, err, want)
				}
			} else {
				var decodeErr *DecodeError
				if !errors.As(err, &decodeErr) || decodeErr.Offset != 0 || decodeErr.Reason != "invalid base64: "+wantErr.Error() {
					t.Fatalf("%q cap=%d: got %v, want base64 error %v", encoded, capacity, err, wantErr)
				}
				if len(dst) != capacity || cap(dst) != capacity || unsafe.SliceData(dst) != before {
					t.Fatalf("%q: failed decode published a changed slice header", encoded)
				}
			}
			if !bytes.Equal(backing[:8], bytes.Repeat([]byte{0xa5}, 8)) || !bytes.Equal(backing[8+capacity:], bytes.Repeat([]byte{0xa5}, 8)) {
				t.Fatalf("%q cap=%d: decode wrote outside destination capacity", encoded, capacity)
			}
		}
	}
}

func TestDecodeBase64ReplaceAliases(t *testing.T) {
	type pair struct {
		First  []byte `json:"first"`
		Second []byte `json:"second"`
	}
	want := pair{First: base64CapacityPayload(32), Second: bytes.Repeat([]byte{0xf1}, 32)}
	src, err := json.Marshal(want)
	requireNoTestError(t, err)
	backing := make([]byte, 48)
	dst := pair{First: backing[:32:32], Second: backing[16:48:48]}
	decoder := mustCompileTestDecoder[pair](t, DecoderOptions{Replace: true})
	requireNoTestError(t, decoder.Decode(src, &dst))
	if !reflect.DeepEqual(dst, want) || unsafe.SliceData(dst.First) != unsafe.SliceData(backing) || unsafe.SliceData(dst.Second) == unsafe.SliceData(backing[16:]) {
		t.Fatalf("Replace did not reuse first and detach overlapping second: %#v", dst)
	}
}

func TestDecodeBase64NamedBytesAndEmpty(t *testing.T) {
	type element uint8
	type blob []element
	decoder := mustCompileTestDecoder[blob](t, DecoderOptions{})
	dst := make(blob, 2)
	before := unsafe.SliceData(dst)
	requireNoTestError(t, decoder.Decode([]byte(`"AQI="`), &dst))
	if !reflect.DeepEqual(dst, blob{1, 2}) || unsafe.SliceData(dst) != before {
		t.Fatalf("named byte slice = %#v, reused=%v", dst, unsafe.SliceData(dst) == before)
	}
	requireNoTestError(t, decoder.Decode([]byte(`""`), &dst))
	if dst == nil || len(dst) != 0 {
		t.Fatalf("empty base64 slice = %#v", dst)
	}
	requireNoTestError(t, decoder.Decode([]byte(`null`), &dst))
	if dst != nil {
		t.Fatalf("null base64 slice = %#v", dst)
	}
}
