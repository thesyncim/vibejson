package storeio

import (
	"errors"
	"fmt"
	"io"
	"os"
)

// DiscoverMutableInlinePageSize returns the allocation quantum carried by a
// mutable Store's fixed inline roots. It reads at most one 4 KiB record at
// offset zero plus one record at each power-of-two candidate offset. It never
// scans data pages.
//
// The first inline root always starts at offset zero. Its alternate starts at
// PageSize, while the encoded record itself is fixed at InlineSuperblockSize.
// If offset zero is torn, probing the finite uint32 power-of-two page-size
// domain finds the alternate without trusting caller configuration. Two valid
// roots that imply different page sizes fail closed.
func DiscoverMutableInlinePageSize(file *os.File) (uint32, error) {
	if file == nil {
		return 0, fmt.Errorf("%w: nil inline discovery file", ErrInvalidWrite)
	}
	info, err := file.Stat()
	if err != nil {
		return 0, err
	}
	if info.Size() < InlineSuperblockSize {
		return 0, ErrSuperblockNotFound
	}

	var record [InlineSuperblockSize]byte
	var discovered uint32
	consider := func(offset uint64, expected uint32) error {
		n, readErr := file.ReadAt(record[:], int64(offset))
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return readErr
		}
		if n != len(record) {
			return nil
		}
		root, decodeErr := DecodeInlineSuperblock(record[:])
		if decodeErr != nil || expected != 0 && root.PageSize != expected {
			return nil
		}
		if discovered != 0 && discovered != root.PageSize {
			return ErrSuperblockConflict
		}
		discovered = root.PageSize
		return nil
	}

	if err := consider(0, 0); err != nil {
		return 0, err
	}

	fileSize := uint64(info.Size())
	for candidate := uint64(physicalPageQuantum); candidate <= uint64(^uint32(0)) && candidate <= fileSize-uint64(InlineSuperblockSize); candidate <<= 1 {
		if err := consider(candidate, uint32(candidate)); err != nil {
			return 0, err
		}
		if candidate > uint64(^uint32(0))>>1 {
			break
		}
	}
	if discovered == 0 {
		return 0, ErrSuperblockNotFound
	}
	return discovered, nil
}
