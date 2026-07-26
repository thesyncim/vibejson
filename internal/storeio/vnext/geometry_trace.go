package vnext

import (
	"fmt"
	"math/bits"
	"slices"
)

// TraceKind is one logical mutation for the block-geometry laboratory.
type TraceKind uint8

const (
	TracePut TraceKind = iota + 1
	TraceDelete
)

// TraceOperation addresses one logical key. RecordBytes is the exact key plus
// JSON byte count after a put; framing is accounted by the raw-block codec.
type TraceOperation struct {
	Kind        TraceKind
	Key         uint64
	RecordBytes int
}

// TraceConfig fixes hot mutation geometry and cold canonical maintenance.
type TraceConfig struct {
	HotMaxSpan  int
	ColdMaxSpan int
	Maintain    bool
}

// TraceMetrics compares exact-quantum spans with the current power-of-two
// policy for the same logical block rewrites.
type TraceMetrics struct {
	Operations            uint64
	Documents             uint64
	LogicalBytes          uint64
	Blocks                uint64
	ExactSpanBytes        uint64
	PowerOfTwoSpanBytes   uint64
	ExactDeviceBytes      uint64
	PowerOfTwoDeviceBytes uint64
	Relocations           uint64
	FreshRebuildBytes     uint64
}

type traceRecord struct {
	key   uint64
	bytes int
}

type traceBlock struct {
	records []traceRecord
}

// SimulateTrace replays mutations into byte-sized stable-slot blocks. Splits
// are byte-balanced. Optional maintenance greedily merges the remaining live
// records into cold blocks, providing the fresh-rebuild comparison without
// embedding a page cache, allocator, or database in the laboratory.
func SimulateTrace(operations []TraceOperation, config TraceConfig) (TraceMetrics, error) {
	if config.HotMaxSpan == 0 {
		config.HotMaxSpan = 8 << 10
	}
	if config.ColdMaxSpan == 0 {
		config.ColdMaxSpan = 16 << 10
	}
	if !validSpan(config.HotMaxSpan) || !validSpan(config.ColdMaxSpan) ||
		config.HotMaxSpan > config.ColdMaxSpan {
		return TraceMetrics{}, fmt.Errorf("%w: trace geometry", ErrInvalidFrame)
	}
	blocks := make([]*traceBlock, 0)
	locations := make(map[uint64]*traceBlock)
	var metrics TraceMetrics
	for _, operation := range operations {
		metrics.Operations++
		switch operation.Kind {
		case TracePut:
			if operation.RecordBytes <= 0 ||
				operation.RecordBytes+RawBlockFixedBytes > MaxSpan {
				return TraceMetrics{}, fmt.Errorf("%w: trace record", ErrInvalidFrame)
			}
			block := locations[operation.Key]
			if block == nil {
				block = bestTraceBlock(blocks, operation.RecordBytes, config.HotMaxSpan)
				if block == nil {
					block = &traceBlock{}
					blocks = append(blocks, block)
				}
				block.records = append(block.records, traceRecord{
					key: operation.Key, bytes: operation.RecordBytes,
				})
				locations[operation.Key] = block
			} else {
				for i := range block.records {
					if block.records[i].key == operation.Key {
						block.records[i].bytes = operation.RecordBytes
						break
					}
				}
			}
			if traceBlockBytes(block) > config.HotMaxSpan && len(block.records) > 1 {
				left, right := splitTraceBlock(block)
				metrics.Relocations += uint64(len(right.records))
				metrics.accountWrite(traceBlockBytes(left))
				metrics.accountWrite(traceBlockBytes(right))
				block.records = left.records
				blocks = append(blocks, right)
				for _, record := range right.records {
					locations[record.key] = right
				}
			} else {
				metrics.accountWrite(traceBlockBytes(block))
			}
		case TraceDelete:
			block := locations[operation.Key]
			if block == nil {
				continue
			}
			for i := range block.records {
				if block.records[i].key != operation.Key {
					continue
				}
				copy(block.records[i:], block.records[i+1:])
				block.records = block.records[:len(block.records)-1]
				delete(locations, operation.Key)
				break
			}
			metrics.accountWrite(traceBlockBytes(block))
		default:
			return TraceMetrics{}, fmt.Errorf("%w: trace operation", ErrInvalidFrame)
		}
	}
	blocks = liveTraceBlocks(blocks)
	fresh := packTraceRecords(allTraceRecords(blocks), config.ColdMaxSpan)
	metrics.FreshRebuildBytes = traceBlocksExactBytes(fresh)
	if config.Maintain {
		before := make(map[uint64]*traceBlock, len(locations))
		for key, block := range locations {
			before[key] = block
		}
		blocks = fresh
		for _, block := range blocks {
			metrics.accountWrite(traceBlockBytes(block))
			for _, record := range block.records {
				if before[record.key] != block {
					metrics.Relocations++
				}
				locations[record.key] = block
			}
		}
	}
	for _, block := range blocks {
		encoded := traceBlockBytes(block)
		metrics.Blocks++
		metrics.ExactSpanBytes += uint64(exactTraceSpan(encoded))
		metrics.PowerOfTwoSpanBytes += uint64(powerOfTwoTraceSpan(encoded))
		for _, record := range block.records {
			metrics.Documents++
			metrics.LogicalBytes += uint64(record.bytes)
		}
	}
	return metrics, nil
}

func (m *TraceMetrics) accountWrite(encoded int) {
	if encoded == 0 {
		return
	}
	m.ExactDeviceBytes += uint64(exactTraceSpan(encoded))
	m.PowerOfTwoDeviceBytes += uint64(powerOfTwoTraceSpan(encoded))
}

func bestTraceBlock(blocks []*traceBlock, recordBytes, maxSpan int) *traceBlock {
	var best *traceBlock
	bestBytes := -1
	for _, block := range blocks {
		if len(block.records) >= RawBlockSlotCount {
			continue
		}
		encoded := traceBlockBytes(block) + recordBytes
		if encoded <= maxSpan && encoded > bestBytes {
			best, bestBytes = block, encoded
		}
	}
	return best
}

func splitTraceBlock(block *traceBlock) (*traceBlock, *traceBlock) {
	total := 0
	for _, record := range block.records {
		total += record.bytes
	}
	leftBytes, split := 0, 1
	for i, record := range block.records[:len(block.records)-1] {
		leftBytes += record.bytes
		split = i + 1
		if leftBytes*2 >= total {
			break
		}
	}
	left := &traceBlock{records: slices.Clone(block.records[:split])}
	right := &traceBlock{records: slices.Clone(block.records[split:])}
	return left, right
}

func liveTraceBlocks(blocks []*traceBlock) []*traceBlock {
	out := blocks[:0]
	for _, block := range blocks {
		if len(block.records) != 0 {
			out = append(out, block)
		}
	}
	return out
}

func allTraceRecords(blocks []*traceBlock) []traceRecord {
	count := 0
	for _, block := range blocks {
		count += len(block.records)
	}
	records := make([]traceRecord, 0, count)
	for _, block := range blocks {
		records = append(records, block.records...)
	}
	slices.SortFunc(records, func(a, b traceRecord) int {
		switch {
		case a.key < b.key:
			return -1
		case a.key > b.key:
			return 1
		default:
			return 0
		}
	})
	return records
}

func packTraceRecords(records []traceRecord, maxSpan int) []*traceBlock {
	blocks := make([]*traceBlock, 0)
	for _, record := range records {
		var block *traceBlock
		if len(blocks) != 0 {
			candidate := blocks[len(blocks)-1]
			if len(candidate.records) < RawBlockSlotCount &&
				traceBlockBytes(candidate)+record.bytes <= maxSpan {
				block = candidate
			}
		}
		if block == nil {
			block = &traceBlock{}
			blocks = append(blocks, block)
		}
		block.records = append(block.records, record)
	}
	return blocks
}

func traceBlockBytes(block *traceBlock) int {
	if block == nil || len(block.records) == 0 {
		return 0
	}
	bytes := RawBlockFixedBytes
	for _, record := range block.records {
		bytes += record.bytes
	}
	return bytes
}

func traceBlocksExactBytes(blocks []*traceBlock) uint64 {
	var bytes uint64
	for _, block := range blocks {
		bytes += uint64(exactTraceSpan(traceBlockBytes(block)))
	}
	return bytes
}

func exactTraceSpan(encoded int) int {
	if encoded == 0 {
		return 0
	}
	return (encoded + Quantum - 1) / Quantum * Quantum
}

func powerOfTwoTraceSpan(encoded int) int {
	if encoded <= Quantum {
		return Quantum
	}
	return 1 << bits.Len(uint(encoded-1))
}
