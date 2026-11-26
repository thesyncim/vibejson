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
		{1, 16},
		{16, 16},
		{17, 32},
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
		if len(block) != 0 || cap(block) != test.want {
			t.Fatalf("need %d: len=%d cap=%d, want 0/%d",
				test.need, len(block), cap(block), test.want)
		}
		data := block[:cap(block)]
		for i := range data {
			data[i] = byte(i*31 + 7)
		}
	}
}

//go:noinline
func decoderStringBlockRetainedText(capacity int) string {
	block := newDecoderStringBlock(capacity)
	block = append(block, "retained through collection"...)
	return OwnedBytesString(block)
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
