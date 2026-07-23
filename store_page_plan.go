package slopjson

import (
	"io"
	"os"

	"github.com/thesyncim/slopjson/internal/storeio"
)

// The planner assigns immutable physical extents before the writer touches
// the file. Plans contain values and borrowed Store chunks only; none becomes
// part of the durable representation or the read path.
type storeDocumentPagePlan struct {
	chunkID uint32
	chunk   *storeChunk
	ref     storeio.PageRef
}

type storeChunkDirectoryPlan struct {
	prefix   uint32
	shift    uint8
	bitmap   uint64
	children []storeio.PageRef
	ref      storeio.PageRef
}

type storeChunkDirectoryItem struct {
	id  uint32
	ref storeio.PageRef
}

type storeKeyPagePlan struct {
	level    uint8
	minHash  uint64
	maxHash  uint64
	leaf     []storeio.PageKeyLocation
	branches []storeio.PageKeyBranch
	next     storeio.PageRef
	ref      storeio.PageRef
}

func storePageExtent(required uint64, maximum uint32) (uint32, bool) {
	if required > uint64(maximum) {
		return 0, false
	}
	size := uint64(storePageQuantum)
	for size < required {
		size <<= 1
	}
	return uint32(size), size <= uint64(maximum)
}

func planStoreChunkDirectories(items []storeChunkDirectoryItem, generation uint64, pageSize uint32, nextLogical, offset *uint64) ([]storeChunkDirectoryPlan, storeio.PageRef) {
	if len(items) == 0 {
		return nil, storeio.PageRef{}
	}
	all := make([]storeChunkDirectoryPlan, 0, (len(items)+62)/63)
	shift := uint8(0)
	for len(items) > 1 || shift == 0 {
		next := make([]storeChunkDirectoryItem, 0, (len(items)+63)/64)
		for start := 0; start < len(items); {
			covered := uint(shift) + 6
			group := items[start].id
			if covered < 32 {
				group >>= covered
			} else {
				group = 0
			}
			end := start + 1
			for end < len(items) {
				other := items[end].id
				if covered < 32 {
					other >>= covered
				} else {
					other = 0
				}
				if other != group {
					break
				}
				end++
			}
			children := make([]storeio.PageRef, end-start)
			var bitmap uint64
			for i := start; i < end; i++ {
				bitmap |= uint64(1) << uint(items[i].id>>shift&63)
				children[i-start] = items[i].ref
			}
			prefix := items[start].id
			if covered < 32 {
				prefix &^= uint32(1)<<covered - 1
			} else {
				prefix = 0
			}
			ref := storeio.PageRef{
				Offset: *offset, LogicalID: *nextLogical, Generation: generation,
				Length: pageSize, Kind: storeio.PageChunkDirectory,
			}
			*offset += uint64(pageSize)
			*nextLogical++
			all = append(all, storeChunkDirectoryPlan{
				prefix: prefix, shift: shift, bitmap: bitmap, children: children, ref: ref,
			})
			next = append(next, storeChunkDirectoryItem{id: prefix, ref: ref})
			start = end
		}
		items = next
		if len(items) == 1 {
			return all, items[0].ref
		}
		shift += 6
	}
	panic("slopjson: unreachable chunk-directory planner state")
}

func planStoreKeyDirectories(entries []storeio.PageKeyLocation, generation uint64, nextLogical, offset *uint64) ([]storeKeyPagePlan, storeio.PageRef) {
	if len(entries) == 0 {
		return nil, storeio.PageRef{}
	}
	leafCapacity := (int(storePageQuantum) - storeio.PageHeaderSize - storeio.PageTrailerSize -
		storeio.PageKeyDirectoryPayloadHeaderSize) / storeio.PageKeyLeafEntrySize
	plans := make([]storeKeyPagePlan, 0, (len(entries)+leafCapacity-1)/leafCapacity)
	levelStart := 0
	for start := 0; start < len(entries); start += leafCapacity {
		end := min(start+leafCapacity, len(entries))
		ref := storeio.PageRef{
			Offset: *offset, LogicalID: *nextLogical, Generation: generation,
			Length: storePageQuantum, Kind: storeio.PageKeyDirectory,
		}
		*offset += uint64(storePageQuantum)
		*nextLogical++
		plans = append(plans, storeKeyPagePlan{
			minHash: entries[start].Hash, maxHash: entries[end-1].Hash,
			leaf: entries[start:end], ref: ref,
		})
	}
	for i := levelStart; i+1 < len(plans); i++ {
		plans[i].next = plans[i+1].ref
	}
	levelEnd := len(plans)
	branchCapacity := (int(storePageQuantum) - storeio.PageHeaderSize - storeio.PageTrailerSize -
		storeio.PageKeyDirectoryPayloadHeaderSize) / storeio.PageKeyBranchEntrySize
	level := uint8(1)
	for levelEnd-levelStart > 1 {
		nextStart := len(plans)
		for start := levelStart; start < levelEnd; start += branchCapacity {
			end := min(start+branchCapacity, levelEnd)
			branches := make([]storeio.PageKeyBranch, end-start)
			for i := start; i < end; i++ {
				branches[i-start] = storeio.PageKeyBranch{MaxHash: plans[i].maxHash, Child: plans[i].ref}
			}
			ref := storeio.PageRef{
				Offset: *offset, LogicalID: *nextLogical, Generation: generation,
				Length: storePageQuantum, Kind: storeio.PageKeyDirectory,
			}
			*offset += uint64(storePageQuantum)
			*nextLogical++
			plans = append(plans, storeKeyPagePlan{
				level: level, minHash: plans[start].minHash, maxHash: plans[end-1].maxHash,
				branches: branches, ref: ref,
			})
		}
		levelStart, levelEnd = nextStart, len(plans)
		level++
	}
	return plans, plans[levelStart].ref
}

func storePageOptionFlags(options storeStateOptions) uint32 {
	var flags uint32
	if options.ShapeTapes {
		flags |= storeio.StateOptionShapeTapes
	}
	if options.Postings {
		flags |= storeio.StateOptionPostings
	}
	if options.ValueDict {
		flags |= storeio.StateOptionValueDict
	}
	if options.IndexOptions.HashKeys {
		flags |= storeio.StateOptionHashKeys
	}
	return flags
}

func writeStorePageAt(file *os.File, page []byte, offset uint64) error {
	n, err := file.WriteAt(page, int64(offset))
	if err == nil && n != len(page) {
		err = io.ErrShortWrite
	}
	return err
}
