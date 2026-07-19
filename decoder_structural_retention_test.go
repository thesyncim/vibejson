package simdjson

import (
	"bytes"
	"runtime"
	"strings"
	"testing"
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
}
