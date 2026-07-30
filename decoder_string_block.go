package vibejson

import "unsafe"

// decoderStringBlock is the header shared by fixed-size owned-string blocks.
// The common path allocates one concrete header+data object, so the cursor can
// retain its arena in one pointer without growing beyond one cache line.
type decoderStringBlock struct {
	used     int
	capacity int
	external []byte
}

type decoderStringBlock64 struct {
	decoderStringBlock
	data [64]byte
}

type decoderStringBlock128 struct {
	decoderStringBlock
	data [128]byte
}

type decoderStringBlock256 struct {
	decoderStringBlock
	data [256]byte
}

type decoderStringBlock512 struct {
	decoderStringBlock
	data [512]byte
}

type decoderStringBlock1K struct {
	decoderStringBlock
	data [1 << 10]byte
}

type decoderStringBlock2K struct {
	decoderStringBlock
	data [2 << 10]byte
}

type decoderStringBlock4K struct {
	decoderStringBlock
	data [4 << 10]byte
}

type decoderStringBlock8K struct {
	decoderStringBlock
	data [8 << 10]byte
}

type decoderStringBlock16K struct {
	decoderStringBlock
	data [16 << 10]byte
}

type decoderStringBlock32K struct {
	decoderStringBlock
	data [32 << 10]byte
}

type decoderStringBlock64K struct {
	decoderStringBlock
	data [64 << 10]byte
}

type decoderStringBlock128K struct {
	decoderStringBlock
	data [128 << 10]byte
}

type decoderStringBlock256K struct {
	decoderStringBlock
	data [256 << 10]byte
}

type decoderStringBlock512K struct {
	decoderStringBlock
	data [512 << 10]byte
}

type decoderStringBlock1M struct {
	decoderStringBlock
	data [1 << 20]byte
}

type decoderStringBlock2M struct {
	decoderStringBlock
	data [2 << 20]byte
}

type decoderStringBlock4M struct {
	decoderStringBlock
	data [4 << 20]byte
}

func newDecoderStringBlock(capacity int) *decoderStringBlock {
	switch {
	case capacity <= 64:
		block := new(decoderStringBlock64)
		block.capacity = len(block.data)
		return &block.decoderStringBlock
	case capacity <= 128:
		block := new(decoderStringBlock128)
		block.capacity = len(block.data)
		return &block.decoderStringBlock
	case capacity <= 256:
		block := new(decoderStringBlock256)
		block.capacity = len(block.data)
		return &block.decoderStringBlock
	case capacity <= 512:
		block := new(decoderStringBlock512)
		block.capacity = len(block.data)
		return &block.decoderStringBlock
	case capacity <= 1<<10:
		block := new(decoderStringBlock1K)
		block.capacity = len(block.data)
		return &block.decoderStringBlock
	case capacity <= 2<<10:
		block := new(decoderStringBlock2K)
		block.capacity = len(block.data)
		return &block.decoderStringBlock
	case capacity <= 4<<10:
		block := new(decoderStringBlock4K)
		block.capacity = len(block.data)
		return &block.decoderStringBlock
	case capacity <= 8<<10:
		block := new(decoderStringBlock8K)
		block.capacity = len(block.data)
		return &block.decoderStringBlock
	case capacity <= 16<<10:
		block := new(decoderStringBlock16K)
		block.capacity = len(block.data)
		return &block.decoderStringBlock
	case capacity <= 32<<10:
		block := new(decoderStringBlock32K)
		block.capacity = len(block.data)
		return &block.decoderStringBlock
	case capacity <= 64<<10:
		block := new(decoderStringBlock64K)
		block.capacity = len(block.data)
		return &block.decoderStringBlock
	case capacity <= 128<<10:
		block := new(decoderStringBlock128K)
		block.capacity = len(block.data)
		return &block.decoderStringBlock
	case capacity <= 256<<10:
		block := new(decoderStringBlock256K)
		block.capacity = len(block.data)
		return &block.decoderStringBlock
	case capacity <= 512<<10:
		block := new(decoderStringBlock512K)
		block.capacity = len(block.data)
		return &block.decoderStringBlock
	case capacity <= 1<<20:
		block := new(decoderStringBlock1M)
		block.capacity = len(block.data)
		return &block.decoderStringBlock
	case capacity <= 2<<20:
		block := new(decoderStringBlock2M)
		block.capacity = len(block.data)
		return &block.decoderStringBlock
	case capacity <= 4<<20:
		block := new(decoderStringBlock4M)
		block.capacity = len(block.data)
		return &block.decoderStringBlock
	default:
		block := new(decoderStringBlock)
		block.external = make([]byte, capacity)
		block.capacity = capacity
		return block
	}
}

func (block *decoderStringBlock) bytes() []byte {
	if block.external != nil {
		return block.external
	}
	data := unsafe.Add(unsafe.Pointer(block), unsafe.Sizeof(*block))
	return unsafe.Slice((*byte)(data), block.capacity)
}
