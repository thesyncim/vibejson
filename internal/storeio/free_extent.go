package storeio

import (
	"encoding/binary"
	"fmt"
)

// FreeExtent is one page-aligned physical range retired by a copy-on-write
// publication. RetiredGeneration is the last Store generation that may reach
// it; reuse additionally waits for every read lease and protected recovery
// root to advance beyond that generation.
//
// Coalescing two adjacent extents keeps the greater RetiredGeneration, never
// the lesser: the merged range is reachable from every generation either input
// was reachable from, so taking the minimum would advertise the older half for
// reuse while a recovery root could still dereference the newer one.
type FreeExtent struct {
	Offset            uint64
	Length            uint64
	RetiredGeneration uint64
}

// MeanAdjacentExtentDistance reports the mean absolute distance in bytes
// between the starting offsets of adjacent extents in the supplied logical
// order. Callers measuring ordered leaves pass them in lexical order; the
// allocator's offset-sorted free set is not an appropriate input. Fewer than
// two extents have zero distance.
//
// The online mean avoids overflowing an integer sum at hundred-million-extent
// scale while retaining the individual uint64 distances exactly until their
// conversion to float64.
func MeanAdjacentExtentDistance(extents []FreeExtent) float64 {
	if len(extents) < 2 {
		return 0
	}
	mean := 0.0
	for i := 1; i < len(extents); i++ {
		left, right := extents[i-1].Offset, extents[i].Offset
		distance := left - right
		if right > left {
			distance = right - left
		}
		mean += (float64(distance) - mean) / float64(i)
	}
	return mean
}

func decodeFreeExtent(src []byte) FreeExtent {
	return FreeExtent{
		Offset:            binary.LittleEndian.Uint64(src[0:8]),
		Length:            binary.LittleEndian.Uint64(src[8:16]),
		RetiredGeneration: binary.LittleEndian.Uint64(src[16:24]),
	}
}

func validateFreeExtent(extent FreeExtent, pageSize uint32, fileEnd uint64) error {
	quantum := uint64(pageSize)
	layout, err := MutableStoreLayout(pageSize)
	if err != nil {
		return err
	}
	if extent.Length == 0 || extent.Offset < layout.DataStart ||
		extent.Offset%quantum != 0 || extent.Length%quantum != 0 ||
		extent.Offset > maxSuperblockFileOffset || extent.Length > fileEnd || extent.Offset > fileEnd-extent.Length ||
		extent.RetiredGeneration == 0 {
		return fmt.Errorf("%w: invalid free extent", ErrInvalidWrite)
	}
	return nil
}
