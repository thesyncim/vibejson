package benchmarks

// Production stage-2 position kernel checks and microbenchmarks over the
// reference corpus. Corpus preparation stays outside timed regions.

import (
	"testing"
	"unsafe"

	simdkernels "github.com/thesyncim/simdjson/internal/kernels"
)

func stage2CorpusScalars(src []byte, positions []uint32) []uint32 {
	out := make([]uint32, 0, len(positions))
	for _, position := range positions {
		switch src[position] {
		case '{', '[', '}', ']', ':', ',', '"':
		default:
			out = append(out, position)
		}
	}
	return out
}

func stage1ValidPositions(src []byte) ([]uint32, simdkernels.Stage1IndexStream) {
	base := unsafe.Pointer(unsafe.SliceData(src))
	fullBlocks := len(src) / 64
	out := make([]uint32, len(src)+128)
	written := 0
	var stream simdkernels.Stage1IndexStream
	var meta simdkernels.Stage1ValidMeta
	for block := 0; block < fullBlocks; block += simdkernels.Stage1ChunkBlocks {
		count := min(simdkernels.Stage1ChunkBlocks, fullBlocks-block)
		written += simdkernels.Stage1ValidBlocks((*byte)(unsafe.Add(base, block*64)), count, uint32(block*64), &stream, out[written:], &meta)
	}
	if fullBlocks*64 != len(src) {
		var tail [64]byte
		for i := range tail {
			tail[i] = ' '
		}
		copy(tail[:], src[fullBlocks*64:])
		written += simdkernels.Stage1ValidBlocks(&tail[0], 1, uint32(fullBlocks*64), &stream, out[written:], &meta)
	}
	return out[:written], stream
}

// stage2Positions preserves the checked boundary historically measured by
// this comparison module without keeping a benchmark-only API in production.
func stage2Positions(base *byte, positions []uint32, kinds *[simdkernels.Stage2KindsLen]byte, scalars []uint32, state *simdkernels.Stage2State) int {
	if len(scalars) < len(positions) {
		panic("simdjson: stage2 scalar output shorter than positions")
	}
	return simdkernels.Stage2PositionsTrusted(base, positions, kinds, scalars, state)
}

func TestStage2MachineCorpora(t *testing.T) {
	for _, corpus := range loadBenchmarkCorpora(t) {
		positions, stream := stage1ValidPositions(corpus.src)
		if stream.Bad || stream.Carry.InString != 0 {
			t.Fatalf("%s: validation producer rejected corpus", corpus.label)
		}
		wantScalars := stage2CorpusScalars(corpus.src, positions)
		gotScalars := make([]uint32, len(positions))
		kinds := new([simdkernels.Stage2KindsLen]byte)
		var state simdkernels.Stage2State
		simdkernels.Stage2Reset(&state)
		count := stage2Positions(unsafe.SliceData(corpus.src), positions, kinds, gotScalars, &state)
		if !simdkernels.Stage2Finish(&state) {
			t.Fatalf("%s: position machine rejected corpus", corpus.label)
		}
		gotScalars = gotScalars[:count]
		if len(gotScalars) != len(wantScalars) {
			t.Fatalf("%s: %d scalar starts, want %d", corpus.label, len(gotScalars), len(wantScalars))
		}
		for i := range wantScalars {
			if gotScalars[i] != wantScalars[i] {
				t.Fatalf("%s: scalar %d = %d, want %d", corpus.label, i, gotScalars[i], wantScalars[i])
			}
		}
	}
}

// BenchmarkStage2PositionsGo retains its historical name so targeted before
// and after comparisons remain directly matchable.
func BenchmarkStage2PositionsGo(b *testing.B) {
	for _, corpus := range loadBenchmarkCorpora(b) {
		b.Run(corpus.label, func(b *testing.B) {
			positions, stream := stage1ValidPositions(corpus.src)
			if stream.Bad || stream.Carry.InString != 0 {
				b.Fatal("validation producer rejected corpus")
			}
			base := unsafe.SliceData(corpus.src)
			kinds := new([simdkernels.Stage2KindsLen]byte)
			scalars := make([]uint32, len(positions))
			var state simdkernels.Stage2State
			b.SetBytes(int64(len(corpus.src)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				simdkernels.Stage2Reset(&state)
				intSink = stage2Positions(base, positions, kinds, scalars, &state)
				boolSink = simdkernels.Stage2Finish(&state)
			}
			reportPerPosition(b, len(positions))
		})
	}
}

func BenchmarkStage1ValidDirect(b *testing.B) {
	for _, corpus := range loadBenchmarkCorpora(b) {
		b.Run(corpus.label, func(b *testing.B) {
			base := unsafe.Pointer(unsafe.SliceData(corpus.src))
			fullBlocks := len(corpus.src) / 64
			out := make([]uint32, len(corpus.src)+128)
			b.SetBytes(int64(len(corpus.src)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				var stream simdkernels.Stage1IndexStream
				var meta simdkernels.Stage1ValidMeta
				written := 0
				for block := 0; block < fullBlocks; block += simdkernels.Stage1ChunkBlocks {
					count := min(simdkernels.Stage1ChunkBlocks, fullBlocks-block)
					written += simdkernels.Stage1ValidBlocks((*byte)(unsafe.Add(base, block*64)), count, uint32(block*64), &stream, out[written:], &meta)
				}
				intSink = written
			}
		})
	}
}
