package slopjson

import (
	"bytes"
	"runtime"
	"strconv"
	"strings"
	"testing"

	simdkernels "github.com/thesyncim/slopjson/internal/kernels"
)

func TestDecoderStructuralTapeDropsOversizedBacking(t *testing.T) {
	tape := decoderStructuralTape{
		positions: make([]uint32, 1, decoderStructuralTapeRetentionPositions+1),
		index:     1,
		bad:       true,
		nonASCII:  true,
		escaped:   true,
	}

	tape.resetForPool()
	runtime.GC()
	if tape.positions != nil {
		t.Fatalf("oversized tape retained capacity %d after release", cap(tape.positions))
	}
	if tape.index != 0 || tape.bad || tape.nonASCII || tape.escaped {
		t.Fatalf("released tape retained state: %+v", tape)
	}
	if tape.stringWindows != [decoderStructuralWindowWords]uint64{} {
		t.Fatal("released tape retained string-window metadata")
	}
}

func TestDecoderStructuralTapeRetainsBoundedBacking(t *testing.T) {
	tape := decoderStructuralTape{positions: make([]uint32, 1, 1024)}
	warmCap := cap(tape.positions)
	tape.resetForPool()
	if cap(tape.positions) != warmCap || len(tape.positions) != 0 {
		t.Fatalf("bounded tape release cap=%d len=%d, want cap=%d len=0", cap(tape.positions), len(tape.positions), warmCap)
	}
}

func TestDecoderStructuralTapeTracksDirtyStringWindows(t *testing.T) {
	if got, want := 1<<decoderStructuralWindowShift, simdkernels.Stage1ChunkBlocks*64; got != want {
		t.Fatalf("structural window is %d bytes, stage-1 chunk is %d", got, want)
	}

	src := []byte(`{"escaped":"` + strings.Repeat("a", 100) + `\n","padding":"` +
		strings.Repeat("b", 5000) + `","unicode":"β","tail":"` + strings.Repeat("c", 5000) + `"}`)
	var tape decoderStructuralTape
	tape.build(src)

	escape := bytes.Index(src, []byte(`\n`))
	nonASCII := bytes.Index(src, []byte("β"))
	tail := bytes.Index(src, []byte(strings.Repeat("c", 64)))
	clean := tail + 3000
	if escape < 0 || nonASCII < 0 || tail < 0 {
		t.Fatal("test fixture lost its marker spans")
	}
	if !tape.stringRangeDirty(escape, escape+2, false) {
		t.Fatal("escape window was not marked")
	}
	if tape.stringRangeDirty(escape, escape+2, true) {
		t.Fatal("escape-only window was marked non-ASCII")
	}
	if !tape.stringRangeDirty(nonASCII, nonASCII+2, true) {
		t.Fatal("non-ASCII window was not marked")
	}
	if tape.stringRangeDirty(nonASCII, nonASCII+2, false) {
		t.Fatal("non-ASCII-only window was marked escaped")
	}
	if tape.stringRangeDirty(clean, clean+64, false) || tape.stringRangeDirty(clean, clean+64, true) {
		t.Fatal("clean window inherited a distant string fact")
	}
	for _, offset := range []int{2047, 2048} {
		for _, tc := range []struct {
			name     string
			marker   string
			nonASCII bool
		}{
			{name: "escape", marker: `\n`},
			{name: "utf8", marker: "β", nonASCII: true},
		} {
			t.Run(tc.name+"-at-"+strconv.Itoa(offset), func(t *testing.T) {
				prefix := `{"value":"`
				boundarySrc := []byte(prefix + strings.Repeat("x", offset-len(prefix)) + tc.marker + `"}` + strings.Repeat(" ", 4096))
				if got := bytes.Index(boundarySrc, []byte(tc.marker)); got != offset {
					t.Fatalf("marker offset=%d, want %d", got, offset)
				}
				var boundaryTape decoderStructuralTape
				boundaryTape.build(boundarySrc)
				if !boundaryTape.stringRangeDirty(offset, offset+len(tc.marker), tc.nonASCII) {
					t.Fatal("boundary window was not marked dirty")
				}
				if boundaryTape.stringRangeDirty(offset, offset+len(tc.marker), !tc.nonASCII) {
					t.Fatal("boundary window was marked for the wrong fact")
				}
			})
		}
	}
	beyondInline := decoderStructuralTapeRetentionBytes + 1
	if !tape.stringRangeDirty(beyondInline, beyondInline+1, false) ||
		!tape.stringRangeDirty(beyondInline, beyondInline+1, true) {
		t.Fatal("range beyond inline metadata was treated as clean")
	}
}
