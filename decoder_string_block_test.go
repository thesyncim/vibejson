package vibejson

import (
	"runtime"
	"strings"
	"testing"
	"unsafe"
)

func TestDecoderStringBlockCapacityClasses(t *testing.T) {
	tests := []struct {
		need int
		want int
	}{
		{1, 64},
		{64, 64},
		{65, 128},
		{511, 512},
		{513, 1 << 10},
		{(1 << 10) + 1, 2 << 10},
		{(16 << 10) + 1, 32 << 10},
		{(1 << 20) + 1, 2 << 20},
		{(2 << 20) + 1, 4 << 20},
		{(4 << 20) + 1, (4 << 20) + 1},
	}
	for _, test := range tests {
		block := newDecoderStringBlock(test.need)
		data := block.bytes()
		if block.capacity != test.want || len(data) != test.want {
			t.Fatalf("need %d: capacity=%d len=%d, want %d",
				test.need, block.capacity, len(data), test.want)
		}
		for i := range data {
			data[i] = byte(i*31 + 7)
		}
		if test.need <= 4<<20 {
			header := uintptr(unsafe.Pointer(block))
			payload := uintptr(unsafe.Pointer(unsafe.SliceData(data)))
			if payload-header != unsafe.Sizeof(*block) {
				t.Fatalf("need %d: fixed payload offset=%d, want %d",
					test.need, payload-header, unsafe.Sizeof(*block))
			}
		} else if block.external == nil {
			t.Fatalf("need %d: oversized block did not use external storage", test.need)
		}
	}
}

// decoderStringBlockRetainedText returns the only live reference to a block's
// payload. The GC must recognize the interior pointer held by the string after
// the concrete block header itself becomes unreachable.
//
//go:noinline
func decoderStringBlockRetainedText(capacity int) string {
	block := newDecoderStringBlock(capacity)
	copy(block.bytes(), "retained through collection")
	block.used = len("retained through collection")
	return OwnedBytesString(block.bytes()[:block.used])
}

func TestDecoderStringBlockStringKeepsStorageAlive(t *testing.T) {
	for _, capacity := range []int{64, 16 << 10, (4 << 20) + 1} {
		text := decoderStringBlockRetainedText(capacity)
		for range 3 {
			runtime.GC()
		}
		if text != "retained through collection" {
			t.Fatalf("capacity %d: retained text = %q", capacity, text)
		}
	}
}

func TestDecoderStringBlocksDoNotRetainWholeSource(t *testing.T) {
	type document struct {
		Text string `json:"text"`
	}
	decoder, err := CompileDecoder[document](DecoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	padding := strings.Repeat(" ", 1<<20)
	src := []byte(`{"text":"small"}` + padding)
	var dst document
	if err := decoder.Decode(src, &dst); err != nil {
		t.Fatal(err)
	}
	if dst.Text != "small" {
		t.Fatalf("text = %q", dst.Text)
	}
	data := uintptr(unsafe.Pointer(unsafe.StringData(dst.Text)))
	source := uintptr(unsafe.Pointer(unsafe.SliceData(src)))
	if data >= source && data-source < uintptr(len(src)) {
		t.Fatal("owned result aliases the source document")
	}
}
