package benchmarks

// Production stage-2 study: the resumable Go grammar machines measured over
// real corpus masks and packed positions, whole-document and chunked the way
// the Valid engine feeds them.

import (
	"testing"
	"unsafe"

	simdkernels "github.com/thesyncim/simdjson/simd"
)

// stage2CorpusScalars classifies a corpus's flattened positions: the
// scalar starts the machine must record, in order.
func stage2CorpusScalars(c gapCorpus) []uint32 {
	var out []uint32
	for _, p := range c.positions {
		switch c.src[p] {
		case '{', '[', '}', ']', ':', ',', '"':
		default:
			out = append(out, p)
		}
	}
	return out
}

func stage1CursorPositions(src []byte) ([]uint32, simdkernels.Stage1IndexStream) {
	n := len(src)
	base := unsafe.Pointer(unsafe.SliceData(src))
	fullBlocks := n / 64
	out := make([]uint32, n+128)
	written := 0
	var st simdkernels.Stage1IndexStream
	for block := 0; block < fullBlocks; block += simdkernels.Stage1ChunkBlocks {
		count := min(simdkernels.Stage1ChunkBlocks, fullBlocks-block)
		written += simdkernels.Stage1CursorBlocks((*byte)(unsafe.Add(base, block*64)), count, uint32(block*64), &st, out[written:])
	}
	if fullBlocks*64 != n {
		var tail [64]byte
		for i := range tail {
			tail[i] = ' '
		}
		copy(tail[:], src[fullBlocks*64:])
		written += simdkernels.Stage1CursorBlocks(&tail[0], 1, uint32(fullBlocks*64), &st, out[written:])
	}
	return out[:written], st
}

func stage1ValidPositions(src []byte) ([]uint32, simdkernels.Stage1IndexStream) {
	n := len(src)
	base := unsafe.Pointer(unsafe.SliceData(src))
	fullBlocks := n / 64
	out := make([]uint32, n+128)
	written := 0
	var st simdkernels.Stage1IndexStream
	var meta simdkernels.Stage1ValidMeta
	for block := 0; block < fullBlocks; block += simdkernels.Stage1ChunkBlocks {
		count := min(simdkernels.Stage1ChunkBlocks, fullBlocks-block)
		written += simdkernels.Stage1ValidBlocks((*byte)(unsafe.Add(base, block*64)), count, uint32(block*64), &st, out[written:], &meta)
	}
	if fullBlocks*64 != n {
		var tail [64]byte
		for i := range tail {
			tail[i] = ' '
		}
		copy(tail[:], src[fullBlocks*64:])
		written += simdkernels.Stage1ValidBlocks(&tail[0], 1, uint32(fullBlocks*64), &st, out[written:], &meta)
	}
	return out[:written], st
}

// stage2RunChunked drives the machine over the corpus masks in runs of
// chunkWords, collecting scalar positions, and returns the final verdict.
func stage2RunChunked(src []byte, emit []uint64, chunkWords int, collect *[]uint32) bool {
	base := unsafe.Pointer(unsafe.SliceData(src))
	var st simdkernels.Stage2State
	simdkernels.Stage2Reset(&st)
	kinds := new([simdkernels.Stage2KindsLen]byte)
	scalars := make([]uint32, 64*chunkWords)
	for w := 0; w < len(emit); w += chunkWords {
		end := min(w+chunkWords, len(emit))
		ns := simdkernels.Stage2Walk((*byte)(unsafe.Add(base, w*64)), emit[w:end], kinds, scalars, &st)
		if collect != nil {
			for _, rel := range scalars[:ns] {
				*collect = append(*collect, uint32(w*64)+rel)
			}
		}
	}
	return simdkernels.Stage2Finish(&st)
}

func stage2RunChunkedGo(src []byte, emit []uint64, chunkWords int, collect *[]uint32) bool {
	base := unsafe.Pointer(unsafe.SliceData(src))
	var st simdkernels.Stage2State
	simdkernels.Stage2Reset(&st)
	kinds := new([simdkernels.Stage2KindsLen]byte)
	scalars := make([]uint32, 64*chunkWords)
	for w := 0; w < len(emit); w += chunkWords {
		end := min(w+chunkWords, len(emit))
		ns := simdkernels.Stage2WalkGo((*byte)(unsafe.Add(base, w*64)), emit[w:end], kinds, scalars, &st)
		if collect != nil {
			for _, rel := range scalars[:ns] {
				*collect = append(*collect, uint32(w*64)+rel)
			}
		}
	}
	return simdkernels.Stage2Finish(&st)
}

func stage2RunPositionsGo(src []byte) bool {
	positions, stream := stage1ValidPositions(src)
	if stream.Bad || stream.Carry.InString != 0 {
		return false
	}
	var st simdkernels.Stage2State
	simdkernels.Stage2Reset(&st)
	kinds := new([simdkernels.Stage2KindsLen]byte)
	scalars := make([]uint32, len(positions))
	simdkernels.Stage2PositionsGo(unsafe.SliceData(src), positions, kinds, scalars, &st)
	return simdkernels.Stage2Finish(&st)
}

// TestStage2MachineCorpora checks the production machine on every corpus:
// acceptance whole and chunked at the engine's granularities, scalar
// records equal to the classified positions, and — because Bad judges
// exactly the grammar the oracle walk judges — verdict agreement with
// consumerOracleWalk on mutated corpus prefixes.
func TestStage2MachineCorpora(t *testing.T) {
	for _, c := range loadGapCorpora(t) {
		wantScalars := stage2CorpusScalars(c)
		validPositions, validStream := stage1ValidPositions(c.src)
		if validStream.Bad || validStream.Carry.InString != 0 {
			t.Fatalf("%s: validation producer rejected corpus", c.label)
		}
		if len(validPositions) != len(c.positions) {
			t.Fatalf("%s: validation producer wrote %d positions, want %d", c.label, len(validPositions), len(c.positions))
		}
		for i := range c.positions {
			if validPositions[i] != c.positions[i] {
				t.Fatalf("%s: validation position %d = %d, want %d", c.label, i, validPositions[i], c.positions[i])
			}
		}
		positionScalars := make([]uint32, len(validPositions))
		positionKinds := new([simdkernels.Stage2KindsLen]byte)
		var positionState simdkernels.Stage2State
		simdkernels.Stage2Reset(&positionState)
		npositionScalars := simdkernels.Stage2PositionsGo(unsafe.SliceData(c.src), validPositions, positionKinds, positionScalars, &positionState)
		if !simdkernels.Stage2Finish(&positionState) {
			t.Fatalf("%s: position machine rejected corpus", c.label)
		}
		if npositionScalars != len(wantScalars) {
			t.Fatalf("%s position machine: %d scalar starts, want %d", c.label, npositionScalars, len(wantScalars))
		}
		for i := range wantScalars {
			if positionScalars[i] != wantScalars[i] {
				t.Fatalf("%s position machine: scalar %d = %d, want %d", c.label, i, positionScalars[i], wantScalars[i])
			}
		}
		cursorPositions, cursorStream := stage1CursorPositions(c.src)
		if cursorStream.Bad || cursorStream.Carry.InString != 0 {
			t.Fatalf("%s: cursor producer rejected corpus", c.label)
		}
		cursorScalars := make([]uint32, len(cursorPositions))
		cursorKinds := new([simdkernels.Stage2KindsLen]byte)
		var cursorState simdkernels.Stage2State
		simdkernels.Stage2Reset(&cursorState)
		ncursorScalars := simdkernels.Stage2CursorGo(unsafe.SliceData(c.src), len(c.src), cursorPositions, cursorKinds, cursorScalars, &cursorState)
		if !simdkernels.Stage2Finish(&cursorState) {
			t.Fatalf("%s: cursor machine rejected corpus (bad %#x, depth %d, prev %#x)", c.label, cursorState.Bad, cursorState.Depth, cursorState.PrevRowIO)
		}
		if ncursorScalars != len(wantScalars) {
			t.Fatalf("%s cursor: %d scalar starts, want %d", c.label, ncursorScalars, len(wantScalars))
		}
		for i := range wantScalars {
			if cursorScalars[i] != wantScalars[i] {
				t.Fatalf("%s cursor: scalar %d = %d, want %d", c.label, i, cursorScalars[i], wantScalars[i])
			}
		}
		for _, chunkWords := range []int{4, 8, 16, len(c.emit)} {
			machines := []struct {
				name string
				run  func([]byte, []uint64, int, *[]uint32) bool
			}{
				{"go-direct", stage2RunChunkedGo},
				{"public", stage2RunChunked},
			}
			for _, machine := range machines {
				var got []uint32
				if !machine.run(c.src, c.emit, chunkWords, &got) {
					t.Fatalf("%s: %s machine rejected the corpus at chunk %d", c.label, machine.name, chunkWords)
				}
				if len(got) != len(wantScalars) {
					t.Fatalf("%s %s (chunk %d): %d scalar starts, want %d", c.label, machine.name, chunkWords, len(got), len(wantScalars))
				}
				for i := range got {
					if got[i] != wantScalars[i] {
						t.Fatalf("%s %s (chunk %d): scalar %d = %d, want %d", c.label, machine.name, chunkWords, i, got[i], wantScalars[i])
					}
				}
			}
		}
		// Grammar differential on structural mutations: flip one emitted
		// byte to another token class and require oracle agreement.
		hostile := []byte(`{}[]:,"5t`)
		for i := 0; i < 400; i++ {
			p := c.positions[(i*2654435761)%len(c.positions)]
			mutated := append([]byte(nil), c.src...)
			mutated[p] = hostile[i%len(hostile)]
			emit := stage1EmitMasks(mutated)
			want := consumerOracleWalk(mutated, emit)
			if got := stage2RunChunked(mutated, emit, 4, nil); got != want {
				t.Fatalf("%s: mutant at %d (%q): public Stage2Walk = %v, oracle = %v", c.label, p, mutated[p], got, want)
			}
			if got := stage2RunChunkedGo(mutated, emit, 4, nil); got != want {
				t.Fatalf("%s: mutant at %d (%q): Go machine = %v, oracle = %v", c.label, p, mutated[p], got, want)
			}
			if got := stage2RunPositionsGo(mutated); got != want {
				t.Fatalf("%s: mutant at %d (%q): position machine = %v, oracle = %v", c.label, p, mutated[p], got, want)
			}
		}
	}
}

func BenchmarkStage2Whole(b *testing.B) {
	for _, c := range loadGapCorpora(b) {
		b.Run(c.label, func(b *testing.B) {
			base := unsafe.SliceData(c.src)
			kinds := new([simdkernels.Stage2KindsLen]byte)
			scalars := make([]uint32, 64*len(c.emit))
			var st simdkernels.Stage2State
			b.SetBytes(int64(len(c.src)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				simdkernels.Stage2Reset(&st)
				intSink = simdkernels.Stage2Walk(base, c.emit, kinds, scalars, &st)
				boolSink = simdkernels.Stage2Finish(&st)
			}
			reportPerPosition(b, len(c.positions))
		})
	}
}

func benchmarkStage2Chunked(b *testing.B, chunkWords int) {
	for _, c := range loadGapCorpora(b) {
		b.Run(c.label, func(b *testing.B) {
			base := unsafe.Pointer(unsafe.SliceData(c.src))
			kinds := new([simdkernels.Stage2KindsLen]byte)
			scalars := make([]uint32, 64*chunkWords)
			var st simdkernels.Stage2State
			npos := 0
			b.SetBytes(int64(len(c.src)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				simdkernels.Stage2Reset(&st)
				npos = 0
				for w := 0; w < len(c.emit); w += chunkWords {
					end := min(w+chunkWords, len(c.emit))
					npos += simdkernels.Stage2Walk((*byte)(unsafe.Add(base, w*64)), c.emit[w:end], kinds, scalars, &st)
				}
				boolSink = simdkernels.Stage2Finish(&st)
				intSink = npos
			}
			reportPerPosition(b, len(c.positions))
		})
	}
}

func BenchmarkStage2Chunked4(b *testing.B)  { benchmarkStage2Chunked(b, 4) }
func BenchmarkStage2Chunked8(b *testing.B)  { benchmarkStage2Chunked(b, 8) }
func BenchmarkStage2Chunked16(b *testing.B) { benchmarkStage2Chunked(b, 16) }

func BenchmarkStage2GoChunked16(b *testing.B) {
	for _, c := range loadGapCorpora(b) {
		b.Run(c.label, func(b *testing.B) {
			base := unsafe.Pointer(unsafe.SliceData(c.src))
			kinds := new([simdkernels.Stage2KindsLen]byte)
			scalars := make([]uint32, 64*16)
			var st simdkernels.Stage2State
			npos := 0
			b.SetBytes(int64(len(c.src)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				simdkernels.Stage2Reset(&st)
				npos = 0
				for w := 0; w < len(c.emit); w += 16 {
					end := min(w+16, len(c.emit))
					npos += simdkernels.Stage2WalkGo((*byte)(unsafe.Add(base, w*64)), c.emit[w:end], kinds, scalars, &st)
				}
				boolSink = simdkernels.Stage2Finish(&st)
				intSink = npos
			}
			reportPerPosition(b, len(c.positions))
		})
	}
}

func BenchmarkStage2CursorGo(b *testing.B) {
	for _, c := range loadGapCorpora(b) {
		b.Run(c.label, func(b *testing.B) {
			positions, stream := stage1CursorPositions(c.src)
			if stream.Bad || stream.Carry.InString != 0 {
				b.Fatal("cursor producer rejected corpus")
			}
			base := unsafe.SliceData(c.src)
			kinds := new([simdkernels.Stage2KindsLen]byte)
			scalars := make([]uint32, len(positions))
			var st simdkernels.Stage2State
			b.SetBytes(int64(len(c.src)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				simdkernels.Stage2Reset(&st)
				intSink = simdkernels.Stage2CursorGo(base, len(c.src), positions, kinds, scalars, &st)
				boolSink = simdkernels.Stage2Finish(&st)
			}
			reportPerPosition(b, len(c.positions))
		})
	}
}

func BenchmarkStage2PositionsGo(b *testing.B) {
	for _, c := range loadGapCorpora(b) {
		b.Run(c.label, func(b *testing.B) {
			positions, stream := stage1ValidPositions(c.src)
			if stream.Bad || stream.Carry.InString != 0 {
				b.Fatal("validation producer rejected corpus")
			}
			base := unsafe.SliceData(c.src)
			kinds := new([simdkernels.Stage2KindsLen]byte)
			scalars := make([]uint32, len(positions))
			var st simdkernels.Stage2State
			b.SetBytes(int64(len(c.src)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				simdkernels.Stage2Reset(&st)
				intSink = simdkernels.Stage2PositionsGo(base, positions, kinds, scalars, &st)
				boolSink = simdkernels.Stage2Finish(&st)
			}
			reportPerPosition(b, len(c.positions))
		})
	}
}

func BenchmarkGapStage1ValidDirect(b *testing.B) {
	for _, c := range loadGapCorpora(b) {
		b.Run(c.label, func(b *testing.B) {
			src := c.src
			base := unsafe.Pointer(unsafe.SliceData(src))
			fullBlocks := len(src) / 64
			out := make([]uint32, len(src)+128)
			b.SetBytes(int64(len(src)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				var st simdkernels.Stage1IndexStream
				var meta simdkernels.Stage1ValidMeta
				written := 0
				for block := 0; block < fullBlocks; block += simdkernels.Stage1ChunkBlocks {
					count := min(simdkernels.Stage1ChunkBlocks, fullBlocks-block)
					written += simdkernels.Stage1ValidBlocks((*byte)(unsafe.Add(base, block*64)), count, uint32(block*64), &st, out[written:], &meta)
				}
				intSink = written
			}
		})
	}
}
