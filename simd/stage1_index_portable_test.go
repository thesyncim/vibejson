//go:build !go1.27 || go1.28 || !goexperiment.simd || !arm64

package simd

import (
	"math/bits"
	"math/rand"
	"reflect"
	"testing"
)

func TestStage1PortablePackedProducers(t *testing.T) {
	rng := rand.New(rand.NewSource(0x504f525441424c45))
	alphabet := []byte("{}[],:\"\\ truefalsenull0123456789\n\t\rabcdefghijklmnopqrstuvwxyz")
	for trial := 0; trial < 100; trial++ {
		blocks := 1 + rng.Intn(40)
		src := make([]byte, blocks*64)
		for i := range src {
			if rng.Intn(4) == 0 {
				src[i] = byte(rng.Intn(256))
			} else {
				src[i] = alphabet[rng.Intn(len(alphabet))]
			}
		}

		var fullState, validState, cursorState Stage1IndexStream
		var recordState Stage1Stream
		var previousIn uint64
		var wantBad, wantNonASCII, wantEscapes bool
		var gotFull, gotValid, gotCursor []uint32
		var wantFull, wantValid, wantCursor []uint32

		for block := 0; block < blocks; {
			count := 1 + rng.Intn(Stage1ChunkBlocks)
			if count > blocks-block {
				count = blocks - block
			}
			base := uint32(block * 64)

			fullOut := make([]uint32, count*64+64)
			validOut := make([]uint32, count*64+64)
			cursorOut := make([]uint32, count*64+64)
			var validMeta, coarseMeta Stage1ValidMeta
			fullN := Stage1IndexBlocks(&src[block*64], count, base, &fullState, fullOut)
			validN := Stage1ValidBlocks(&src[block*64], count, base, &validState, validOut, &validMeta)
			cursorN := Stage1CursorBlocks(&src[block*64], count, base, &cursorState, cursorOut)
			coarseOut := make([]uint32, count*64+64)
			var coarseState Stage1IndexStream
			coarseState = validState
			// Recreate the pre-chunk state for the coarse comparison.
			coarseState = Stage1IndexStream{}
			for prior := 0; prior < block; {
				step := min(Stage1ChunkBlocks, block-prior)
				tmp := make([]uint32, step*64+64)
				var tmpMeta Stage1ValidMeta
				Stage1ValidBlocks(&src[prior*64], step, uint32(prior*64), &coarseState, tmp, &tmpMeta)
				prior += step
			}
			coarseN := Stage1ValidBlocksCoarse(&src[block*64], count, base, &coarseState, coarseOut, &coarseMeta)
			if coarseN != validN || !reflect.DeepEqual(coarseOut[:coarseN], validOut[:validN]) {
				t.Fatalf("trial %d block %d: coarse stream differs", trial, block)
			}
			if coarseState != validState {
				t.Fatalf("trial %d block %d: coarse state %+v, exact %+v", trial, block, coarseState, validState)
			}
			wantCoarse := uint32(0)
			if validMeta.NonASCII != 0 {
				wantCoarse = ^uint32(0) >> uint(Stage1ChunkBlocks-count)
			}
			if coarseMeta.NonASCII != wantCoarse {
				t.Fatalf("trial %d block %d: coarse non-ASCII %#x, want %#x", trial, block, coarseMeta.NonASCII, wantCoarse)
			}

			gotFull = append(gotFull, fullOut[:fullN]...)
			gotValid = append(gotValid, validOut[:validN]...)
			gotCursor = append(gotCursor, cursorOut[:cursorN]...)

			var recs [Stage1ChunkBlocks]Stage1Rec
			Stage1BlocksGP(&src[block*64], count, &recordState, &recs)
			var nonASCII uint32
			for i := 0; i < count; i++ {
				rec := &recs[i]
				wantBad = wantBad || rec.Bad
				wantNonASCII = wantNonASCII || rec.NonASCII
				wantEscapes = wantEscapes || rec.EscInStr != 0
				closers := (rec.InStr<<1 | previousIn) &^ rec.InStr
				fullMask := rec.Emit | closers
				positionBase := uint32((block + i) * 64)
				for mask := rec.Emit; mask != 0; mask &= mask - 1 {
					pos := positionBase + uint32(bits.TrailingZeros64(mask))
					wantValid = append(wantValid, pos)
				}
				for mask := fullMask; mask != 0; mask &= mask - 1 {
					pos := positionBase + uint32(bits.TrailingZeros64(mask))
					wantFull = append(wantFull, pos)
					if src[pos] != ':' {
						wantCursor = append(wantCursor, pos)
					}
				}
				if validMeta.EscInStr[i] != rec.EscInStr {
					t.Fatalf("trial %d block %d lane %d: escape metadata differs", trial, block, i)
				}
				if rec.NonASCII {
					nonASCII |= 1 << i
				}
				previousIn = rec.InStr >> 63
			}
			if validMeta.NonASCII != nonASCII {
				t.Fatalf("trial %d block %d: non-ASCII metadata %#x, want %#x", trial, block, validMeta.NonASCII, nonASCII)
			}
			block += count
		}

		if !reflect.DeepEqual(gotFull, wantFull) || !reflect.DeepEqual(gotValid, wantValid) || !reflect.DeepEqual(gotCursor, wantCursor) {
			t.Fatalf("trial %d: packed output mismatch", trial)
		}
		for name, state := range map[string]Stage1IndexStream{"full": fullState, "valid": validState, "cursor": cursorState} {
			if state.Carry != recordState.Carry || state.Follows != recordState.Follows || state.PreviousIn != previousIn ||
				state.Bad != wantBad || state.NonASCII != wantNonASCII || state.Escaped != wantEscapes {
				t.Fatalf("trial %d %s state=%+v record=%+v previous=%x", trial, name, state, recordState, previousIn)
			}
		}
	}
}
