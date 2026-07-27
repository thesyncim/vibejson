//go:build go1.27 && !go1.28 && goexperiment.simd && arm64

package kernels

import (
	"bytes"
	"math/rand/v2"
	"testing"
)

// stage1RecWalker is the independent per-byte oracle for the batched
// kernel: explicit escape, in-string, and scalar-run state advanced one
// byte at a time, no bit tricks shared with any kernel.
type stage1RecWalker struct {
	escaped bool // current byte is the target of a backslash escape
	inStr   bool
	follows bool // previous byte was a scalar candidate
}

func (w *stage1RecWalker) block(b *[64]byte) Stage1Rec {
	var r Stage1Rec
	for i, c := range b {
		bit := uint64(1) << i
		isWs := c == ' ' || c == '\t' || c == '\n' || c == '\r'
		isStruct := c == '{' || c == '}' || c == '[' || c == ']' || c == ':' || c == ','
		isQuoteRaw := c == '"'
		isCtrl := c < 0x20
		if c >= 0x80 {
			r.NonASCII = true
		}

		esc := w.escaped
		w.escaped = false
		if c == '\\' && !esc {
			w.escaped = true
		}

		quote := isQuoteRaw && !esc
		if quote {
			w.inStr = !w.inStr
		}
		in := w.inStr             // opener bit set, closer bit clear
		closer := quote && !in    // closing quote
		outside := !in && !closer // strictly outside, excluding both quotes

		cand := !(isWs || isStruct || isQuoteRaw || in)
		start := cand && !w.follows
		w.follows = cand

		if isStruct && outside || quote && in || start && outside {
			r.Emit |= bit
		}
		if cand && outside {
			r.Scalar |= bit
		}
		if esc && in {
			r.EscInStr |= bit
		}
		if isCtrl && in || isCtrl && outside && !isWs {
			r.Bad = true
		}
		if isWs && outside {
			r.WsOut |= bit
		}
		if in {
			r.InStr |= bit
		}
	}
	return r
}

// checkStreamKernels runs the batched kernel and the portable per-mask
// reference over a block sequence against the walker oracle, with all
// three carry chains evolving independently.
func checkStreamKernels(t *testing.T, blocks []byte, w *stage1RecWalker,
	stGP, stRef *Stage1Stream, label string) {
	t.Helper()
	if len(blocks)%64 != 0 {
		t.Fatalf("%s: sequence length %d not a multiple of 64", label, len(blocks))
	}
	n := len(blocks) / 64
	var outGP [Stage1ChunkBlocks]Stage1Rec
	for off := 0; off < n; off += Stage1ChunkBlocks {
		cnt := min(Stage1ChunkBlocks, n-off)
		Stage1BlocksGP(&blocks[off*64], cnt, stGP, &outGP)
		for i := 0; i < cnt; i++ {
			blk := (*[64]byte)(blocks[(off+i)*64:])
			want := w.block(blk)

			var m Stage1Masks
			Stage1Block(blk, &m)
			var ref Stage1Rec
			Stage1RecFromMasks(&m, stRef, &ref)

			if ref != want {
				t.Fatalf("%s block %d: portable reference %+v, walker %+v\nblock: %q",
					label, off+i, ref, want, blk[:])
			}
			if outGP[i] != want {
				t.Fatalf("%s block %d: GP kernel %+v, walker %+v\nblock: %q",
					label, off+i, outGP[i], want, blk[:])
			}
		}
		if stGP.Carry != stRef.Carry || stGP.Follows != stRef.Follows {
			t.Fatalf("%s: GP carry %+v diverged from reference %+v", label, stGP, stRef)
		}
	}
}

// TestStage1StreamAdversarial drives carry-hostile fixed patterns across
// block boundaries: backslash runs of every phase ending at the
// boundary, strings spanning many blocks, quote/backslash alternations,
// and control bytes inside and outside strings.
func TestStage1StreamAdversarial(t *testing.T) {
	patterns := [][]byte{
		bytes.Repeat([]byte{'\\'}, 64*4),
		bytes.Repeat([]byte{'"'}, 64*4),
		bytes.Repeat([]byte{'\\', '"'}, 64*2),
		bytes.Repeat([]byte{'"', '\\'}, 64*2),
		bytes.Repeat([]byte{'\\', '\\', '"'}, 64),
		bytes.Repeat([]byte(`{"a":"b A\\", "c\n":[1,2]}   `), 64)[:64*8],
		bytes.Repeat([]byte{0x00}, 64*2),
		bytes.Repeat([]byte{'"', 0x1f}, 64),
		bytes.Repeat([]byte{' ', '\t', '\n', '\r'}, 64),
		bytes.Repeat([]byte{'"', 0xc3, 0xa9, '"'}, 64), // non-ASCII inside strings
		bytes.Repeat([]byte{0x80, 0xff, 'a', ' '}, 64), // non-ASCII sprinkled outside
	}
	// Backslash runs of length 1..17 ending exactly at a block boundary,
	// followed by a quote at the start of the next block: the sharpest
	// escape-carry edge.
	for run := 1; run <= 17; run++ {
		p := bytes.Repeat([]byte{'x'}, 128)
		for i := 0; i < run; i++ {
			p[63-i] = '\\'
		}
		p[64] = '"'
		p[65] = '"'
		patterns = append(patterns, p)
	}
	// A string opened in block 0 that stays open across several blocks,
	// with escapes and controls inside.
	long := bytes.Repeat([]byte{'a'}, 64*6)
	long[3] = '"'
	long[70] = '\\'
	long[71] = 'u'
	long[130] = 0x07
	long[64*5] = '"'
	patterns = append(patterns, long)

	for pi, p := range patterns {
		var w stage1RecWalker
		var stGP, stRef Stage1Stream
		checkStreamKernels(t, p, &w, &stGP, &stRef, "adversarial")
		_ = pi
	}
}

// TestStage1StreamRandom chains random block sequences through the kernel
// and the reference: over half a million blocks across three alphabets,
// with carries running uninterrupted within each sequence.
func TestStage1StreamRandom(t *testing.T) {
	hostile := []byte{'"', '\\', '{', '}', '[', ']', ':', ',', ' ', '\t', '\n', '\r', 0x00, 0x1f, 0x7f, 0x80, 0xff, 'a', '0'}
	stringy := []byte{'"', '\\', '\\', 'u', 'n', 'a', 'b', ' '}
	rng := rand.New(rand.NewPCG(0x5eed, 0xfeed))

	gen := func(seq []byte, mode int) {
		switch mode {
		case 0: // uniform random bytes
			for i := range seq {
				seq[i] = byte(rng.Uint64())
			}
		case 1: // hostile classification alphabet
			for i := range seq {
				seq[i] = hostile[rng.IntN(len(hostile))]
			}
		case 2: // escape/quote dense
			for i := range seq {
				seq[i] = stringy[rng.IntN(len(stringy))]
			}
		}
	}

	const seqBlocks = 48 // exercises the 32-block chunk boundary
	seq := make([]byte, seqBlocks*64)
	rounds := 12000 // 576k blocks
	if testing.Short() {
		rounds = 400
	}
	for round := 0; round < rounds; round++ {
		gen(seq, round%3)
		var w stage1RecWalker
		var stGP, stRef Stage1Stream
		checkStreamKernels(t, seq, &w, &stGP, &stRef, "random")
	}
}

var stage1StreamBenchSink Stage1Rec

// The isolation microbenchmark classifies the same 2 KiB (32 blocks) per
// iteration: the per-block baseline against the batched kernel.
func benchStreamInput() []byte {
	doc := bytes.Repeat([]byte(`    {"key": "value with words", "n": 12345, "ok": true},`+"\n"), 40)
	return doc[:32*64]
}

func BenchmarkStage1Chunk32(b *testing.B) {
	src := benchStreamInput()
	b.Run("current", func(b *testing.B) {
		var m Stage1Masks
		var st Stage1Stream
		var rec Stage1Rec
		b.SetBytes(int64(len(src)))
		for i := 0; i < b.N; i++ {
			for blk := 0; blk < 32; blk++ {
				Stage1Block((*[64]byte)(src[blk*64:]), &m)
				Stage1RecFromMasks(&m, &st, &rec)
			}
		}
		stage1StreamBenchSink = rec
	})
	b.Run("batchedGP", func(b *testing.B) {
		var st Stage1Stream
		var out [Stage1ChunkBlocks]Stage1Rec
		b.SetBytes(int64(len(src)))
		for i := 0; i < b.N; i++ {
			Stage1BlocksGP(&src[0], 32, &st, &out)
		}
		stage1StreamBenchSink = out[31]
	})
}
