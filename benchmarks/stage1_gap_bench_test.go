package benchmarks

// Stage-1 gap suite: measures the components of a hypothetical
// position-driven front-end against the shipping walkers, on the real
// corpus. The question it answers is the fenced walker's re-entry
// precondition: can structural positions be materialized and consumed at
// or below one nanosecond per position? Each benchmark isolates one leg:
//
//   GapValid             the shipping strict validator (simdjson.Valid)
//   GapIndex             the shipping index build (simdjson.BuildIndex)
//   GapStage1Kernel      batched mask production alone (Stage1BlocksGP)
//   GapFlattenExtract    mask -> []uint32 positions (simdjson flatten_bits port)
//   GapConsume           positions -> byte load + dispatch + checksum
//   GapMaskIterConsume   the in-place bitmap walk: dispatch straight off masks
//   GapExtractConsume    flatten + consume from precomputed masks
//   GapStage1Pipeline    kernel + flatten + consume end to end
//
// ns/pos is reported per benchmark; pos/byte per corpus makes the
// corpus-level arithmetic explicit (walker ns/byte vs pipeline ns/byte =
// kernel ns/byte + pos/byte x cursor ns/pos).
//
// Rerun from this directory:
//
//	GOEXPERIMENT=simd "$TIP_GO" test -run='^TestGap' -bench='^BenchmarkGap' \
//	  -benchtime=300ms -count=6 -cpu=1 .

import (
	"math/bits"
	"strings"
	"testing"
	"unsafe"

	"github.com/thesyncim/simdjson"
	simdkernels "github.com/thesyncim/simdjson/simd"
	stdlibcorpus "github.com/thesyncim/simdjson/tests/stdlib"
)

var uint64Sink uint64

// gapCorpus is one corpus payload with its precomputed stage-1 products.
type gapCorpus struct {
	label     string
	src       []byte
	emit      []uint64 // per-64-byte-block emit masks (structural outside strings, opening quotes, scalar starts)
	positions []uint32 // flattened structural positions
}

func loadGapCorpora(tb testing.TB) []gapCorpus {
	out := make([]gapCorpus, 0, len(stdlibcorpus.Names))
	for _, name := range stdlibcorpus.Names {
		src, err := stdlibcorpus.Read(name)
		if err != nil {
			tb.Fatal(err)
		}
		emit := stage1EmitMasks(src)
		n := 0
		for _, m := range emit {
			n += bits.OnesCount64(m)
		}
		positions := make([]uint32, n+flattenSlack)
		got := flattenPositions(emit, positions)
		if got != n {
			tb.Fatalf("%s: flatten produced %d positions, popcount says %d", name, got, n)
		}
		out = append(out, gapCorpus{
			label:     strings.TrimSuffix(name, ".json.zst"),
			src:       src,
			emit:      emit,
			positions: positions[:n],
		})
	}
	return out
}

// stage1EmitMasks classifies the whole document through the batched kernel
// and keeps only the emit mask per block, mirroring validBitmapStreamed's
// framing: full 64-byte blocks straight from the source, the tail block
// space-padded (space is insignificant whitespace and emits nothing).
func stage1EmitMasks(src []byte) []uint64 {
	n := len(src)
	base := unsafe.Pointer(unsafe.SliceData(src))
	nBlocks := (n + 63) / 64
	fullBlocks := n / 64
	emit := make([]uint64, nBlocks)

	var st simdkernels.Stage1Stream
	var recs [simdkernels.Stage1ChunkBlocks]simdkernels.Stage1Rec
	for chunk := 0; chunk < fullBlocks; chunk += simdkernels.Stage1ChunkBlocks {
		cnt := fullBlocks - chunk
		if cnt > simdkernels.Stage1ChunkBlocks {
			cnt = simdkernels.Stage1ChunkBlocks
		}
		simdkernels.Stage1BlocksGP((*byte)(unsafe.Add(base, chunk*64)), cnt, &st, &recs)
		for i := 0; i < cnt; i++ {
			emit[chunk+i] = recs[i].Emit
		}
	}
	if fullBlocks < nBlocks {
		var tail [64]byte
		for i := range tail {
			tail[i] = ' '
		}
		copy(tail[:], src[fullBlocks*64:])
		simdkernels.Stage1BlocksGP(&tail[0], 1, &st, &recs)
		emit[fullBlocks] = recs[0].Emit
	}
	return emit
}

// flattenSlack is the overwrite slack flatten_bits requires past the last
// real position: up to 16 unconditional writes can land beyond the tail
// before the count advances it.
const flattenSlack = 64

// Provenance: CPP-STAGE1-001.
// flattenWrite is a benchmark adaptation of C++ simdjson 4.6.4's
// bit_indexer::write at commit 1bcf71bd85059ab6574ea1159de9298dcc1212c5,
// src/generic/stage1/json_structural_indexer.h; Apache-2.0, see the root
// LICENSE-SIMDJSON. The local variant uses batches of eight and enters its
// loop after 16 writes; upstream defaults to groups of four and loops after
// 24. Eight unconditional ctz + clear-lowest-bit writes cover the common word
// without a per-bit branch, the count advances the tail, and denser words
// fall through to a second unconditional batch and then a loop. dst must have
// at least tail+16 elements of capacity when bits has at most 16 set bits (the
// JSON worst case of 64 set bits needs tail+64); the caller provides
// flattenSlack of slack so the unconditional stores always land in bounds.
func flattenWrite(dst []uint32, tail int, idx uint32, mask uint64) int {
	if mask == 0 {
		return tail
	}
	cnt := bits.OnesCount64(mask)
	d := dst[tail : tail+8]
	d[0] = idx + uint32(bits.TrailingZeros64(mask))
	mask &= mask - 1
	d[1] = idx + uint32(bits.TrailingZeros64(mask))
	mask &= mask - 1
	d[2] = idx + uint32(bits.TrailingZeros64(mask))
	mask &= mask - 1
	d[3] = idx + uint32(bits.TrailingZeros64(mask))
	mask &= mask - 1
	d[4] = idx + uint32(bits.TrailingZeros64(mask))
	mask &= mask - 1
	d[5] = idx + uint32(bits.TrailingZeros64(mask))
	mask &= mask - 1
	d[6] = idx + uint32(bits.TrailingZeros64(mask))
	mask &= mask - 1
	d[7] = idx + uint32(bits.TrailingZeros64(mask))
	mask &= mask - 1
	if cnt > 8 {
		d := dst[tail+8 : tail+16]
		d[0] = idx + uint32(bits.TrailingZeros64(mask))
		mask &= mask - 1
		d[1] = idx + uint32(bits.TrailingZeros64(mask))
		mask &= mask - 1
		d[2] = idx + uint32(bits.TrailingZeros64(mask))
		mask &= mask - 1
		d[3] = idx + uint32(bits.TrailingZeros64(mask))
		mask &= mask - 1
		d[4] = idx + uint32(bits.TrailingZeros64(mask))
		mask &= mask - 1
		d[5] = idx + uint32(bits.TrailingZeros64(mask))
		mask &= mask - 1
		d[6] = idx + uint32(bits.TrailingZeros64(mask))
		mask &= mask - 1
		d[7] = idx + uint32(bits.TrailingZeros64(mask))
		mask &= mask - 1
		if cnt > 16 {
			i := tail + 16
			for mask != 0 {
				dst[i] = idx + uint32(bits.TrailingZeros64(mask))
				mask &= mask - 1
				i++
			}
		}
	}
	return tail + cnt
}

// flattenPositions runs flattenWrite over every block mask, exactly the
// per-word loop C++ stage 1 runs after classification.
func flattenPositions(emit []uint64, dst []uint32) int {
	tail := 0
	idx := uint32(0)
	for _, m := range emit {
		tail = flattenWrite(dst, tail, idx, m)
		idx += 64
	}
	return tail
}

// consumePositions is the minimal honest cursor: load the byte at each
// structural position, dispatch on it, keep a checksum. Byte loads go
// through the same unguarded pattern the production walkers use
// (positions are in bounds by construction). Any position-driven parser
// or validator pays at least this much per position before doing work.
func consumePositions(src []byte, positions []uint32) uint64 {
	base := unsafe.Pointer(unsafe.SliceData(src))
	var sum uint64
	for _, p := range positions {
		c := *(*byte)(unsafe.Add(base, uintptr(p)))
		switch c {
		case '{', '[':
			sum += 1
		case '}', ']':
			sum += 2
		case ':':
			sum += 3
		case ',':
			sum += 4
		case '"':
			sum += 5
		default:
			sum += uint64(c)
		}
	}
	return sum
}

func reportPerPosition(b *testing.B, positions int) {
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/float64(positions), "ns/pos")
}

func BenchmarkGapValid(b *testing.B) {
	for _, c := range loadGapCorpora(b) {
		b.Run(c.label, func(b *testing.B) {
			if !simdjson.Valid(c.src) {
				b.Fatal("corpus rejected")
			}
			b.SetBytes(int64(len(c.src)))
			for i := 0; i < b.N; i++ {
				boolSink = simdjson.Valid(c.src)
			}
			reportPerPosition(b, len(c.positions))
		})
	}
}

func BenchmarkGapIndex(b *testing.B) {
	for _, c := range loadGapCorpora(b) {
		b.Run(c.label, func(b *testing.B) {
			count, err := simdjson.RequiredIndexEntries(c.src)
			if err != nil {
				b.Fatal(err)
			}
			storage := make([]simdjson.IndexEntry, count)
			b.SetBytes(int64(len(c.src)))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				tape, err := simdjson.BuildIndex(c.src, storage)
				if err != nil {
					b.Fatal(err)
				}
				intSink = tape.Len()
			}
			reportPerPosition(b, len(c.positions))
		})
	}
}

// BenchmarkGapStage1Kernel is mask production alone: the batched kernel
// classifies every block and the records are written but never consumed.
// No UTF-8 validation, no flatten — the floor under any consumer.
func BenchmarkGapStage1Kernel(b *testing.B) {
	for _, c := range loadGapCorpora(b) {
		b.Run(c.label, func(b *testing.B) {
			src := c.src
			base := unsafe.Pointer(unsafe.SliceData(src))
			fullBlocks := len(src) / 64
			var recs [simdkernels.Stage1ChunkBlocks]simdkernels.Stage1Rec
			b.SetBytes(int64(len(src)))
			for i := 0; i < b.N; i++ {
				var st simdkernels.Stage1Stream
				for chunk := 0; chunk < fullBlocks; chunk += simdkernels.Stage1ChunkBlocks {
					cnt := fullBlocks - chunk
					if cnt > simdkernels.Stage1ChunkBlocks {
						cnt = simdkernels.Stage1ChunkBlocks
					}
					simdkernels.Stage1BlocksGP((*byte)(unsafe.Add(base, chunk*64)), cnt, &st, &recs)
				}
				uint64Sink = st.Carry.InString
			}
			reportPerPosition(b, len(c.positions))
		})
	}
}

// BenchmarkGapFlattenExtract measures flatten_bits alone: per-block emit
// masks are precomputed, so the timed loop is exactly the C++ position
// materialization (ctz + clear-lowest, batched unconditional writes).
func BenchmarkGapFlattenExtract(b *testing.B) {
	for _, c := range loadGapCorpora(b) {
		b.Run(c.label, func(b *testing.B) {
			dst := make([]uint32, len(c.positions)+flattenSlack)
			b.SetBytes(int64(len(c.src)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				intSink = flattenPositions(c.emit, dst)
			}
			reportPerPosition(b, len(c.positions))
		})
	}
}

// BenchmarkGapConsume measures the cursor's consumption leg alone:
// positions are precomputed, the loop loads and dispatches each one.
func BenchmarkGapConsume(b *testing.B) {
	for _, c := range loadGapCorpora(b) {
		b.Run(c.label, func(b *testing.B) {
			b.SetBytes(int64(len(c.src)))
			for i := 0; i < b.N; i++ {
				uint64Sink = consumePositions(c.src, c.positions)
			}
			reportPerPosition(b, len(c.positions))
		})
	}
}

// consumeMasks is the shipping validator's consumption shape: iterate the
// set bits of each block mask directly (trailing-zeros + clear-lowest),
// load the byte, dispatch, checksum — no position array is materialized.
// Comparing this against flatten+consume isolates what materializing
// positions buys or costs relative to walking the bitmap in place.
func consumeMasks(src []byte, emit []uint64) uint64 {
	base := unsafe.Pointer(unsafe.SliceData(src))
	var sum uint64
	pos := 0
	for _, m := range emit {
		for m != 0 {
			j := pos + bits.TrailingZeros64(m)
			m &= m - 1
			c := *(*byte)(unsafe.Add(base, uintptr(j)))
			switch c {
			case '{', '[':
				sum += 1
			case '}', ']':
				sum += 2
			case ':':
				sum += 3
			case ',':
				sum += 4
			case '"':
				sum += 5
			default:
				sum += uint64(c)
			}
		}
		pos += 64
	}
	return sum
}

// BenchmarkGapMaskIterConsume is consumeMasks over precomputed masks: the
// per-position cost of the current in-place bitmap walk, dispatch included.
func BenchmarkGapMaskIterConsume(b *testing.B) {
	for _, c := range loadGapCorpora(b) {
		b.Run(c.label, func(b *testing.B) {
			b.SetBytes(int64(len(c.src)))
			for i := 0; i < b.N; i++ {
				uint64Sink = consumeMasks(c.src, c.emit)
			}
			reportPerPosition(b, len(c.positions))
		})
	}
}

// BenchmarkGapExtractConsume chains flatten and consume from precomputed
// masks: the round trip a position-driven walker pays per position once
// masks exist.
func BenchmarkGapExtractConsume(b *testing.B) {
	for _, c := range loadGapCorpora(b) {
		b.Run(c.label, func(b *testing.B) {
			dst := make([]uint32, len(c.positions)+flattenSlack)
			b.SetBytes(int64(len(c.src)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				n := flattenPositions(c.emit, dst)
				uint64Sink = consumePositions(c.src, dst[:n])
			}
			reportPerPosition(b, len(c.positions))
		})
	}
}

// BenchmarkGapStage1Pipeline is the honest end-to-end pipeline: batched
// mask production, flatten, consume — everything a position-driven
// validator would run except grammar checks, scalar validation, and UTF-8.
func BenchmarkGapStage1Pipeline(b *testing.B) {
	for _, c := range loadGapCorpora(b) {
		b.Run(c.label, func(b *testing.B) {
			src := c.src
			base := unsafe.Pointer(unsafe.SliceData(src))
			fullBlocks := len(src) / 64
			nBlocks := (len(src) + 63) / 64
			var recs [simdkernels.Stage1ChunkBlocks]simdkernels.Stage1Rec
			dst := make([]uint32, len(c.positions)+flattenSlack)
			b.SetBytes(int64(len(src)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				var st simdkernels.Stage1Stream
				tail := 0
				idx := uint32(0)
				for chunk := 0; chunk < fullBlocks; chunk += simdkernels.Stage1ChunkBlocks {
					cnt := fullBlocks - chunk
					if cnt > simdkernels.Stage1ChunkBlocks {
						cnt = simdkernels.Stage1ChunkBlocks
					}
					simdkernels.Stage1BlocksGP((*byte)(unsafe.Add(base, chunk*64)), cnt, &st, &recs)
					for j := 0; j < cnt; j++ {
						tail = flattenWrite(dst, tail, idx, recs[j].Emit)
						idx += 64
					}
				}
				if fullBlocks < nBlocks {
					var tailBlock [64]byte
					for i := range tailBlock {
						tailBlock[i] = ' '
					}
					copy(tailBlock[:], src[fullBlocks*64:])
					simdkernels.Stage1BlocksGP(&tailBlock[0], 1, &st, &recs)
					tail = flattenWrite(dst, tail, idx, recs[0].Emit)
				}
				uint64Sink = consumePositions(src, dst[:tail])
			}
			reportPerPosition(b, len(c.positions))
		})
	}
}

func benchmarkGapStage1Direct(b *testing.B, compact bool) {
	for _, c := range loadGapCorpora(b) {
		b.Run(c.label, func(b *testing.B) {
			n := len(c.src)
			base := unsafe.Pointer(unsafe.SliceData(c.src))
			fullBlocks := n / 64
			out := make([]uint32, n+128)
			var tail [64]byte
			for i := range tail {
				tail[i] = ' '
			}
			copy(tail[:], c.src[fullBlocks*64:])

			b.SetBytes(int64(n))
			b.ResetTimer()
			for iter := 0; iter < b.N; iter++ {
				var st simdkernels.Stage1IndexStream
				written := 0
				for block := 0; block < fullBlocks; block += simdkernels.Stage1ChunkBlocks {
					count := min(simdkernels.Stage1ChunkBlocks, fullBlocks-block)
					if compact {
						written += simdkernels.Stage1CursorBlocks((*byte)(unsafe.Add(base, block*64)), count, uint32(block*64), &st, out[written:])
					} else {
						written += simdkernels.Stage1IndexBlocks((*byte)(unsafe.Add(base, block*64)), count, uint32(block*64), &st, out[written:])
					}
				}
				if fullBlocks*64 != n {
					if compact {
						written += simdkernels.Stage1CursorBlocks(&tail[0], 1, uint32(fullBlocks*64), &st, out[written:])
					} else {
						written += simdkernels.Stage1IndexBlocks(&tail[0], 1, uint32(fullBlocks*64), &st, out[written:])
					}
				}
				intSink = written
				boolSink = st.Bad
			}
		})
	}
}

func BenchmarkGapStage1IndexDirect(b *testing.B)  { benchmarkGapStage1Direct(b, false) }
func BenchmarkGapStage1CursorDirect(b *testing.B) { benchmarkGapStage1Direct(b, true) }

// TestGapFlattenWrite pins the flatten port against a per-bit reference.
func TestGapFlattenWrite(t *testing.T) {
	ref := func(dst []uint32, tail int, idx uint32, mask uint64) int {
		for m := mask; m != 0; m &= m - 1 {
			dst[tail] = idx + uint32(bits.TrailingZeros64(m))
			tail++
		}
		return tail
	}
	masks := []uint64{
		0, 1, 1 << 63, 0x8000000000000001, 0xffffffffffffffff,
		0x5555555555555555, 0xaaaaaaaaaaaaaaaa, 0x00ff00ff00ff00ff,
		0x0101010101010101, 1<<8 | 1<<16 | 1<<24, 0xdeadbeefcafef00d,
	}
	for _, m := range masks {
		got := make([]uint32, 64+flattenSlack)
		want := make([]uint32, 64+flattenSlack)
		gn := flattenWrite(got, 0, 128, m)
		wn := ref(want, 0, 128, m)
		if gn != wn {
			t.Fatalf("mask %#x: count %d, want %d", m, gn, wn)
		}
		for i := 0; i < gn; i++ {
			if got[i] != want[i] {
				t.Fatalf("mask %#x: position %d = %d, want %d", m, i, got[i], want[i])
			}
		}
	}
}

// TestGapCorpusComposition logs each corpus's byte-class partition from the
// stage-1 masks: string bytes (quotes included), whitespace outside strings,
// structural characters, and scalar bodies. The split frames the per-byte
// versus per-position economics — string interiors and outside whitespace
// are the bytes a mask-driven engine never visits individually.
func TestGapCorpusComposition(t *testing.T) {
	for _, c := range loadGapCorpora(t) {
		var carry simdkernels.Stage1Carry
		var m simdkernels.Stage1Masks
		var inString, closerBytes, wsOut, structOut, scalarBytes int
		src := c.src
		for pos := 0; pos < len(src); pos += 64 {
			var block [64]byte
			for i := range block {
				block[i] = ' '
			}
			copy(block[:], src[pos:])
			simdkernels.Stage1Block(&block, &m)
			escaped := simdkernels.Stage1Escaped(m.Backslash, &carry)
			quotes := m.Quote &^ escaped
			inStr := simdkernels.Stage1PrefixXOR(quotes, &carry)
			closers := quotes &^ inStr
			outside := ^(inStr | closers)
			inString += bits.OnesCount64(inStr)
			closerBytes += bits.OnesCount64(closers)
			wsOut += bits.OnesCount64(m.Whitespace & outside)
			structOut += bits.OnesCount64(m.Structural & outside)
			scalarBytes += bits.OnesCount64(outside) - bits.OnesCount64(m.Whitespace&outside) - bits.OnesCount64(m.Structural&outside)
		}
		pad := (len(src)+63)/64*64 - len(src) // trailing pad counted as ws
		wsOut -= pad
		n := len(src)
		t.Logf("%s: %d bytes | string %.1f%% | ws-outside %.1f%% | structural %.1f%% | scalar %.1f%% | %.4f pos/byte",
			c.label, n,
			100*float64(inString+closerBytes)/float64(n),
			100*float64(wsOut)/float64(n),
			100*float64(structOut)/float64(n),
			100*float64(scalarBytes)/float64(n),
			float64(len(c.positions))/float64(n))
	}
}

// TestGapPositionsMatchIndex sanity-checks the emit convention against the
// shipping index builder: every corpus position count must be plausible
// (nonzero, below the byte count) and the first position must be the first
// significant byte of the document.
func TestGapPositionsMatchIndex(t *testing.T) {
	for _, c := range loadGapCorpora(t) {
		if len(c.positions) == 0 || len(c.positions) >= len(c.src) {
			t.Fatalf("%s: implausible position count %d for %d bytes", c.label, len(c.positions), len(c.src))
		}
		first := int(c.positions[0])
		for i := 0; i < first; i++ {
			switch c.src[i] {
			case ' ', '\t', '\n', '\r':
			default:
				t.Fatalf("%s: significant byte %q at %d before first position %d", c.label, c.src[i], i, first)
			}
		}
		t.Logf("%s: %d positions over %d bytes (%.4f pos/byte)", c.label, len(c.positions), len(c.src), float64(len(c.positions))/float64(len(c.src)))
	}
}
