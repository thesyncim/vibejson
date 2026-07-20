//go:build go1.27 && !go1.28 && goexperiment.simd && arm64

package kernels

import (
	"math/bits"
	"math/rand"
	"reflect"
	"testing"
)

const stage1IndexTestAlphabet = "{}[],:\"\\ truefalsenull0123456789\n\t\rabcdefghijklmnopqrstuvwxyz"

func randomStage1IndexFixture(t *testing.T, rng *rand.Rand) (int, []byte) {
	t.Helper()
	blocks := 1 + rng.Intn(67)
	src := make([]byte, blocks*64)
	if _, err := rng.Read(src); err != nil {
		t.Fatal(err)
	}
	// Bias the random stream toward JSON syntax, strings, escapes, and
	// whitespace so every carried state is exercised frequently.
	for i := range src {
		if rng.Intn(4) != 0 {
			src[i] = stage1IndexTestAlphabet[rng.Intn(len(stage1IndexTestAlphabet))]
		}
	}
	return blocks, src
}

func TestStage1IndexBlocksMatchesRecordPipeline(t *testing.T) {
	rng := rand.New(rand.NewSource(0x51a9e1))
	for trial := 0; trial < 200; trial++ {
		blocks, src := randomStage1IndexFixture(t, rng)

		var direct, directValid Stage1IndexStream
		var records Stage1Stream
		var previousIn uint64
		var bad bool
		var nonASCII bool
		var hasEscapes bool
		var got, want, gotValid, wantValid []uint32
		for block := 0; block < blocks; {
			count := 1 + rng.Intn(Stage1ChunkBlocks)
			if count > blocks-block {
				count = blocks - block
			}
			out := make([]uint32, count*64+64)
			written := Stage1IndexBlocks(&src[block*64], count, uint32(block*64), &direct, out)
			got = append(got, out[:written]...)
			validOut := make([]uint32, count*64+64)
			var validMeta Stage1ValidMeta
			validWritten := Stage1ValidBlocks(&src[block*64], count, uint32(block*64), &directValid, validOut, &validMeta)
			gotValid = append(gotValid, validOut[:validWritten]...)

			for recordBlock := 0; recordBlock < count; {
				recordCount := count - recordBlock
				if recordCount > Stage1ChunkBlocks {
					recordCount = Stage1ChunkBlocks
				}
				var recs [Stage1ChunkBlocks]Stage1Rec
				Stage1BlocksGP(&src[(block+recordBlock)*64], recordCount, &records, &recs)
				for i := 0; i < recordCount; i++ {
					rec := &recs[i]
					bad = bad || rec.Bad
					nonASCII = nonASCII || rec.NonASCII
					closers := (rec.InStr<<1 | previousIn) &^ rec.InStr
					if rec.EscInStr != 0 {
						hasEscapes = true
					}
					mask := rec.Emit | closers
					base := uint32((block + recordBlock + i) * 64)
					for validMask := rec.Emit; validMask != 0; validMask &= validMask - 1 {
						wantValid = append(wantValid, base+uint32(bits.TrailingZeros64(validMask)))
					}
					for mask != 0 {
						position := bits.TrailingZeros64(mask)
						entry := base + uint32(position)
						want = append(want, entry)
						mask &= mask - 1
					}
					previousIn = rec.InStr >> 63
				}
				recordBlock += recordCount
			}
			block += count
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("trial %d index mismatch\ngot  %v\nwant %v", trial, got, want)
		}
		if !reflect.DeepEqual(gotValid, wantValid) {
			t.Fatalf("trial %d validation stream mismatch\ngot  %v\nwant %v", trial, gotValid, wantValid)
		}
		if direct.Bad != bad || direct.Carry != records.Carry || direct.Follows != records.Follows ||
			direct.PreviousIn != previousIn || direct.NonASCII != nonASCII || direct.Escaped != hasEscapes {
			t.Fatalf("trial %d state mismatch: direct=%+v records=%+v previous=%x escaped=%v bad=%v",
				trial, direct, records, previousIn, hasEscapes, bad)
		}
		if directValid.Bad != bad || directValid.Carry != records.Carry || directValid.Follows != records.Follows ||
			directValid.PreviousIn != previousIn || directValid.NonASCII != nonASCII || directValid.Escaped != hasEscapes {
			t.Fatalf("trial %d validation state mismatch: direct=%+v records=%+v previous=%x escaped=%v bad=%v",
				trial, directValid, records, previousIn, hasEscapes, bad)
		}
	}
}

func TestStage1IndexBlocksMetaMatchesGeneric(t *testing.T) {
	rng := rand.New(rand.NewSource(0x4d455441))
	for trial := 0; trial < 200; trial++ {
		blocks, src := randomStage1IndexFixture(t, rng)

		var generic, specialized Stage1IndexStream
		var records Stage1Stream
		for block, chunk := 0, 0; block < blocks; chunk++ {
			count := 1 + rng.Intn(Stage1ChunkBlocks)
			if count > blocks-block {
				count = blocks - block
			}
			base := uint32(block * 64)
			genericOut := make([]uint32, count*64+64)
			specializedOut := make([]uint32, count*64+64)

			meta := Stage1IndexMeta{
				NonASCII:   ^uint32(0),
				Sample:     chunk&1 == 0,
				WsCount:    ^uint32(0),
				EmitCount:  ^uint32(0),
				InStrCount: ^uint32(0),
				EscCount:   ^uint32(0),
			}
			for i := range meta.EscInStr {
				meta.EscInStr[i] = ^uint64(0)
				meta.InStr[i] = ^uint64(0)
			}
			genericMeta := meta
			genericN := stage1IndexBlocks(
				&src[block*64], count, base, &generic, genericOut, stage1IndexFull, nil, &genericMeta,
			)
			specializedN := Stage1IndexBlocksMeta(
				&src[block*64], count, base, &specialized, specializedOut, &meta,
			)
			if specializedN != genericN || !reflect.DeepEqual(specializedOut[:specializedN], genericOut[:genericN]) {
				t.Fatalf("trial %d chunk %d index mismatch\ngot  %v\nwant %v",
					trial, chunk, specializedOut[:specializedN], genericOut[:genericN])
			}
			if specialized != generic {
				t.Fatalf("trial %d chunk %d state mismatch: specialized=%+v generic=%+v",
					trial, chunk, specialized, generic)
			}

			var recs [Stage1ChunkBlocks]Stage1Rec
			Stage1BlocksGP(&src[block*64], count, &records, &recs)
			var nonASCII, wsCount, emitCount, inStrCount, escCount uint32
			for i := 0; i < count; i++ {
				rec := &recs[i]
				var masks Stage1Masks
				offset := (block + i) * 64
				Stage1Block((*[64]byte)(src[offset:offset+64]), &masks)
				if meta.EscInStr[i] != rec.EscInStr || meta.InStr[i] != rec.InStr {
					t.Fatalf("trial %d chunk %d block %d metadata mismatch: got esc=%016x str=%016x want esc=%016x str=%016x",
						trial, chunk, i, meta.EscInStr[i], meta.InStr[i], rec.EscInStr, rec.InStr)
				}
				if rec.NonASCII {
					nonASCII |= 1 << i
				}
				wsCount += uint32(bits.OnesCount64(masks.Whitespace))
				emitCount += uint32(bits.OnesCount64(rec.Emit))
				inStrCount += uint32(bits.OnesCount64(rec.InStr))
				escCount += uint32(bits.OnesCount64(rec.EscInStr))
			}
			if meta.NonASCII != nonASCII {
				t.Fatalf("trial %d chunk %d non-ASCII mismatch: got %032b want %032b",
					trial, chunk, meta.NonASCII, nonASCII)
			}
			if meta.NonASCII != genericMeta.NonASCII || meta.WsCount != genericMeta.WsCount ||
				meta.EmitCount != genericMeta.EmitCount || meta.InStrCount != genericMeta.InStrCount ||
				meta.EscCount != genericMeta.EscCount {
				t.Fatalf("trial %d chunk %d generic metadata mismatch: specialized=%+v generic=%+v",
					trial, chunk, meta, genericMeta)
			}
			if meta.Sample != (chunk&1 == 0) {
				t.Fatalf("trial %d chunk %d sample flag changed", trial, chunk)
			}
			if !meta.Sample {
				wsCount, emitCount, inStrCount, escCount = 0, 0, 0, 0
			}
			if meta.WsCount != wsCount || meta.EmitCount != emitCount ||
				meta.InStrCount != inStrCount || meta.EscCount != escCount {
				t.Fatalf("trial %d chunk %d density mismatch: got ws=%d emit=%d str=%d esc=%d want ws=%d emit=%d str=%d esc=%d",
					trial, chunk, meta.WsCount, meta.EmitCount, meta.InStrCount, meta.EscCount,
					wsCount, emitCount, inStrCount, escCount)
			}
			block += count
		}
	}
}

func TestStage1ValidBlocksCoarseMatchesExact(t *testing.T) {
	rng := rand.New(rand.NewSource(0x434f41525345))
	for trial := 0; trial < 200; trial++ {
		blocks, src := randomStage1IndexFixture(t, rng)

		var exact, coarse Stage1IndexStream
		for block, chunk := 0, 0; block < blocks; chunk++ {
			count := 1 + rng.Intn(Stage1ChunkBlocks)
			if count > blocks-block {
				count = blocks - block
			}
			base := uint32(block * 64)
			exactOut := make([]uint32, count*64+64)
			coarseOut := make([]uint32, count*64+64)
			var exactMeta, coarseMeta Stage1ValidMeta
			exactN := Stage1ValidBlocks(&src[block*64], count, base, &exact, exactOut, &exactMeta)
			coarseN := Stage1ValidBlocksCoarse(&src[block*64], count, base, &coarse, coarseOut, &coarseMeta)
			if coarseN != exactN || !reflect.DeepEqual(coarseOut[:coarseN], exactOut[:exactN]) {
				t.Fatalf("trial %d chunk %d index mismatch\ngot  %v\nwant %v",
					trial, chunk, coarseOut[:coarseN], exactOut[:exactN])
			}
			if coarse != exact {
				t.Fatalf("trial %d chunk %d state mismatch: coarse=%+v exact=%+v",
					trial, chunk, coarse, exact)
			}
			for i := 0; i < count; i++ {
				if coarseMeta.EscInStr[i] != exactMeta.EscInStr[i] {
					t.Fatalf("trial %d chunk %d block %d escape mismatch: coarse=%016x exact=%016x",
						trial, chunk, i, coarseMeta.EscInStr[i], exactMeta.EscInStr[i])
				}
			}
			wantNonASCII := uint32(0)
			if exactMeta.NonASCII != 0 {
				wantNonASCII = ^uint32(0) >> (Stage1ChunkBlocks - count)
			}
			if coarseMeta.NonASCII != wantNonASCII {
				t.Fatalf("trial %d chunk %d coarse non-ASCII mismatch: got %032b want %032b (exact %032b)",
					trial, chunk, coarseMeta.NonASCII, wantNonASCII, exactMeta.NonASCII)
			}
			block += count
		}
	}
}

func TestStage1CursorBlocksOmitsOnlyColons(t *testing.T) {
	rng := rand.New(rand.NewSource(0xc01051))
	for trial := 0; trial < 200; trial++ {
		blocks, src := randomStage1IndexFixture(t, rng)

		var fullState, compactState Stage1IndexStream
		var full, compact []uint32
		for block := 0; block < blocks; {
			count := 1 + rng.Intn(Stage1ChunkBlocks)
			if count > blocks-block {
				count = blocks - block
			}
			fullOut := make([]uint32, count*64+64)
			compactOut := make([]uint32, count*64+64)
			base := uint32(block * 64)
			fullN := Stage1IndexBlocks(&src[block*64], count, base, &fullState, fullOut)
			compactN := Stage1CursorBlocks(&src[block*64], count, base, &compactState, compactOut)
			full = append(full, fullOut[:fullN]...)
			compact = append(compact, compactOut[:compactN]...)
			block += count
		}

		filtered := full[:0]
		for _, position := range full {
			if src[position] != ':' {
				filtered = append(filtered, position)
			}
		}
		if !reflect.DeepEqual(compact, filtered) {
			t.Fatalf("trial %d compact index mismatch\ngot  %v\nwant %v", trial, compact, filtered)
		}
		if compactState != fullState {
			t.Fatalf("trial %d state mismatch: compact=%+v full=%+v", trial, compactState, fullState)
		}
	}
}
