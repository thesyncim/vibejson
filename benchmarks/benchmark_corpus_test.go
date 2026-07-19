package benchmarks

import (
	"encoding/json"
	"math/bits"
	"strings"
	"testing"
	"unsafe"

	simdkernels "github.com/thesyncim/simdjson/internal/kernels"
	stdlibcorpus "github.com/thesyncim/simdjson/tests/stdlib"
)

// benchmarkCorpus holds the stage-1 products reused by the production stage-2
// corpus checks and microbenchmarks. Corpus preparation is never timed.
type benchmarkCorpus struct {
	label     string
	src       []byte
	emit      []uint64
	positions []uint32
}

func loadBenchmarkCorpora(tb testing.TB) []benchmarkCorpus {
	tb.Helper()
	out := make([]benchmarkCorpus, 0, len(stdlibcorpus.Names))
	for _, name := range stdlibcorpus.Names {
		src, err := stdlibcorpus.Read(name)
		if err != nil {
			tb.Fatal(err)
		}
		if !json.Valid(src) {
			tb.Fatalf("%s: corpus document is not valid JSON", name)
		}
		emit := stage1EmitMasks(src)
		out = append(out, benchmarkCorpus{
			label:     strings.TrimSuffix(name, ".json.zst"),
			src:       src,
			emit:      emit,
			positions: positionsFromMasks(emit),
		})
	}
	return out
}

// stage1EmitMasks classifies the document in production-sized batches and
// space-pads the final block, matching the stage-2 walker's input contract.
func stage1EmitMasks(src []byte) []uint64 {
	if len(src) == 0 {
		return nil
	}
	base := unsafe.Pointer(unsafe.SliceData(src))
	fullBlocks := len(src) / 64
	emit := make([]uint64, (len(src)+63)/64)

	var stream simdkernels.Stage1Stream
	var records [simdkernels.Stage1ChunkBlocks]simdkernels.Stage1Rec
	for chunk := 0; chunk < fullBlocks; chunk += simdkernels.Stage1ChunkBlocks {
		count := min(simdkernels.Stage1ChunkBlocks, fullBlocks-chunk)
		simdkernels.Stage1BlocksGP((*byte)(unsafe.Add(base, chunk*64)), count, &stream, &records)
		for i := 0; i < count; i++ {
			emit[chunk+i] = records[i].Emit
		}
	}
	if fullBlocks < len(emit) {
		var tail [64]byte
		for i := range tail {
			tail[i] = ' '
		}
		copy(tail[:], src[fullBlocks*64:])
		simdkernels.Stage1BlocksGP(&tail[0], 1, &stream, &records)
		emit[fullBlocks] = records[0].Emit
	}
	return emit
}

func positionsFromMasks(masks []uint64) []uint32 {
	count := 0
	for _, mask := range masks {
		count += bits.OnesCount64(mask)
	}
	positions := make([]uint32, 0, count)
	for block, mask := range masks {
		base := uint32(block * 64)
		for mask != 0 {
			positions = append(positions, base+uint32(bits.TrailingZeros64(mask)))
			mask &= mask - 1
		}
	}
	return positions
}

func reportPerPosition(b *testing.B, positions int) {
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/float64(positions), "ns/pos")
}
