package kernels

import "testing"

func TestStage1BlockProducersRejectInvalidCounts(t *testing.T) {
	var src [64 * (Stage1ChunkBlocks + 1)]byte
	var records [Stage1ChunkBlocks]Stage1Rec
	packed := make([]uint32, 64*(Stage1ChunkBlocks+1)+64)

	producers := []struct {
		name string
		call func(int)
	}{
		{
			name: "records",
			call: func(nblocks int) {
				var stream Stage1Stream
				Stage1BlocksGP(src[:], nblocks, &stream, &records)
			},
		},
		{
			name: "index",
			call: func(nblocks int) {
				var stream Stage1IndexStream
				Stage1IndexBlocks(src[:], nblocks, 0, &stream, packed)
			},
		},
		{
			name: "index-meta",
			call: func(nblocks int) {
				var stream Stage1IndexStream
				var meta Stage1IndexMeta
				Stage1IndexBlocksMeta(src[:], nblocks, 0, &stream, packed, &meta)
			},
		},
		{
			name: "cursor",
			call: func(nblocks int) {
				var stream Stage1IndexStream
				Stage1CursorBlocks(src[:], nblocks, 0, &stream, packed)
			},
		},
		{
			name: "cursor-meta",
			call: func(nblocks int) {
				var stream Stage1IndexStream
				var meta Stage1CursorMeta
				Stage1CursorBlocksMeta(src[:], nblocks, 0, &stream, packed, &meta)
			},
		},
		{
			name: "valid",
			call: func(nblocks int) {
				var stream Stage1IndexStream
				var meta Stage1ValidMeta
				Stage1ValidBlocks(src[:], nblocks, 0, &stream, packed, &meta)
			},
		},
		{
			name: "valid-coarse",
			call: func(nblocks int) {
				var stream Stage1IndexStream
				var meta Stage1ValidMeta
				Stage1ValidBlocksCoarse(src[:], nblocks, 0, &stream, packed, &meta)
			},
		},
	}
	invalidCounts := []struct {
		name   string
		blocks int
	}{
		{name: "negative", blocks: -1},
		{name: "zero", blocks: 0},
		{name: "over-limit", blocks: Stage1ChunkBlocks + 1},
	}

	for _, producer := range producers {
		for _, count := range invalidCounts {
			t.Run(producer.name+"/"+count.name, func(t *testing.T) {
				defer func() {
					if recover() == nil {
						t.Fatalf("accepted invalid block count %d", count.blocks)
					}
				}()
				producer.call(count.blocks)
			})
		}
	}
}

func TestStage1BlockProducersRejectShortSource(t *testing.T) {
	short := make([]byte, 63)
	var records [Stage1ChunkBlocks]Stage1Rec
	packed := make([]uint32, 128)

	cases := []struct {
		name string
		call func()
	}{
		{
			name: "records",
			call: func() {
				var stream Stage1Stream
				Stage1BlocksGP(short, 1, &stream, &records)
			},
		},
		{
			name: "index",
			call: func() {
				var stream Stage1IndexStream
				Stage1IndexBlocks(short, 1, 0, &stream, packed)
			},
		},
		{
			name: "index-meta",
			call: func() {
				var stream Stage1IndexStream
				var meta Stage1IndexMeta
				Stage1IndexBlocksMeta(short, 1, 0, &stream, packed, &meta)
			},
		},
		{
			name: "cursor",
			call: func() {
				var stream Stage1IndexStream
				Stage1CursorBlocks(short, 1, 0, &stream, packed)
			},
		},
		{
			name: "cursor-meta",
			call: func() {
				var stream Stage1IndexStream
				var meta Stage1CursorMeta
				Stage1CursorBlocksMeta(short, 1, 0, &stream, packed, &meta)
			},
		},
		{
			name: "valid",
			call: func() {
				var stream Stage1IndexStream
				var meta Stage1ValidMeta
				Stage1ValidBlocks(short, 1, 0, &stream, packed, &meta)
			},
		},
		{
			name: "valid-coarse",
			call: func() {
				var stream Stage1IndexStream
				var meta Stage1ValidMeta
				Stage1ValidBlocksCoarse(short, 1, 0, &stream, packed, &meta)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("accepted short source slice")
				}
			}()
			tc.call()
		})
	}
}
