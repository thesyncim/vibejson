package simdjson

import (
	"fmt"
	"reflect"
	"runtime"
	"testing"
	"unsafe"
)

// TestTypedSliceWordsLayout pins the header layout typedSliceState reads: the
// data pointer word followed by the length and capacity integer words, the
// ABI the unsafe.Slice builtin is defined against. If this ever fails, the
// in-place view is wrong and every typed slice decode is unsound — fail
// loudly here rather than subtly there.
func TestTypedSliceWordsLayout(t *testing.T) {
	backing := make([]int64, 3, 7)
	view := (*sliceWords)(unsafe.Pointer(&backing))
	if view.data != unsafe.Pointer(unsafe.SliceData(backing)) {
		t.Fatal("sliceWords data word does not match unsafe.SliceData")
	}
	if view.len != 3 || view.cap != 7 {
		t.Fatalf("sliceWords integer words = (%d, %d), want (3, 7)", view.len, view.cap)
	}
	if unsafe.Sizeof(sliceWords{}) != unsafe.Sizeof(backing) {
		t.Fatalf("sliceWords size = %d, slice header size = %d", unsafe.Sizeof(sliceWords{}), unsafe.Sizeof(backing))
	}

	// The state's split boundary: direct length writes must be exactly what
	// reflect performs, and pointer-word mutations must still go through
	// reflect (verified by behavior: grow preserves elements and installs a
	// larger backing array).
	state := typedSliceAt(reflect.TypeOf(backing), unsafe.Pointer(&backing))
	state.setLen(2)
	if len(backing) != 2 || cap(backing) != 7 {
		t.Fatalf("after setLen(2): len=%d cap=%d", len(backing), cap(backing))
	}
	state.grow(32)
	if cap(backing) < 32 || len(backing) != 2 {
		t.Fatalf("after grow(32): len=%d cap=%d", len(backing), cap(backing))
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("setLen beyond capacity did not panic")
			}
		}()
		state.setLen(cap(backing) + 1)
	}()
}

// TestGCCorruptionTypedSliceWords decodes pointer-rich nested slices under an
// aggressive collector while forcing stack movement between operations. The
// direct length-word stores in typedSliceState carry no write barriers; this
// stress proves the pointer words the collector does care about only ever
// change under reflect, so no reachable element is ever hidden from a scan.
// As with the sibling corruption tests, -race masks the class; the stress
// invocation is GOGC=1 without -race, and the plain form is CI-safe.
func TestGCCorruptionTypedSliceWords(t *testing.T) {
	type leaf struct {
		Name string   `json:"name"`
		Tags []string `json:"tags"`
	}
	type branch struct {
		Leaves []leaf  `json:"leaves"`
		Nums   []int64 `json:"nums"`
	}
	type root struct {
		Branches []branch `json:"branches"`
	}

	decoder, err := CompileDecoder[root](DecoderOptions{})
	if err != nil {
		t.Fatal(err)
	}

	build := func(branches, leaves int) []byte {
		doc := `{"branches":[`
		for b := 0; b < branches; b++ {
			if b > 0 {
				doc += ","
			}
			doc += `{"leaves":[`
			for l := 0; l < leaves; l++ {
				if l > 0 {
					doc += ","
				}
				doc += fmt.Sprintf(`{"name":"leaf-%d-%d","tags":["a-%d","b-%d","c-%d"]}`, b, l, b, l, b+l)
			}
			doc += `],"nums":[`
			for n := 0; n < leaves; n++ {
				if n > 0 {
					doc += ","
				}
				doc += fmt.Sprintf("%d", b*1000+n)
			}
			doc += `]}`
		}
		doc += `]}`
		return []byte(doc)
	}

	// Alternate shapes so reused destinations exercise shrink, growth, and
	// the empty sentinel across collections.
	shapes := [][2]int{{8, 6}, {2, 1}, {12, 9}, {1, 0}, {6, 12}}
	var dst root
	for round := 0; round < 30; round++ {
		for _, shape := range shapes {
			src := build(shape[0], shape[1])
			if err := decoder.Decode(src, &dst); err != nil {
				t.Fatal(err)
			}
			sink := forceStackMovement(64, round)
			runtime.GC()
			if len(dst.Branches) != shape[0] {
				t.Fatalf("round %d: branches = %d, want %d", round, len(dst.Branches), shape[0])
			}
			for b, br := range dst.Branches {
				if len(br.Leaves) != shape[1] || len(br.Nums) != shape[1] {
					t.Fatalf("round %d branch %d: leaves=%d nums=%d want %d", round, b, len(br.Leaves), len(br.Nums), shape[1])
				}
				for l, lf := range br.Leaves {
					want := fmt.Sprintf("leaf-%d-%d", b, l)
					if lf.Name != want || len(lf.Tags) != 3 {
						t.Fatalf("round %d: leaf %d/%d = %q tags=%d", round, b, l, lf.Name, len(lf.Tags))
					}
					if lf.Tags[0] != fmt.Sprintf("a-%d", b) {
						t.Fatalf("round %d: tag corruption at %d/%d: %q", round, b, l, lf.Tags[0])
					}
				}
				for n, v := range br.Nums {
					if v != int64(b*1000+n) {
						t.Fatalf("round %d: number corruption at %d/%d: %d", round, b, n, v)
					}
				}
			}
			_ = sink
		}
	}
}
