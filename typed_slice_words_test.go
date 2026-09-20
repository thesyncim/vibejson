package vibejson

import (
	"fmt"
	"runtime"
	"testing"
)

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
