package storeio

import (
	"errors"
	"fmt"
	"os"
	"sync"
)

var (
	// ErrWriterLocked reports that another mutable page-file owner already
	// holds the process/filesystem advisory writer lease.
	ErrWriterLocked = errors.New("vibejson: Store page file already has a writer")
	// ErrWriterLockUnsupported rejects mutable open on a platform where this
	// package cannot enforce the single-writer invariant.
	ErrWriterLockUnsupported = errors.New("vibejson: Store page writer locking is unsupported")
)

var writerLockRegistry struct {
	sync.Mutex
	entries []writerLockIdentity
}

type writerLockIdentity struct {
	descriptor uintptr
	info       os.FileInfo
}

// LockWriter acquires the process and operating-system exclusive writer lease
// for file. The in-process file-identity registry closes a hole in advisory
// locks: reacquiring through the same or a duplicated descriptor may otherwise
// succeed and let two durable Store values mutate one generation stream.
//
// Identity, not descriptor number, is what makes an entry a conflict. Both the
// duplicate-descriptor and the reopened-file cases are the same file, so
// os.SameFile decides them, and it is the only test that can: a descriptor
// number is unique only while its descriptor is open, and the kernel hands the
// lowest free number to the next open. An owner that never unlocked — a Store
// dropped without Close, or a test that failed before its cleanup — leaves an
// entry behind whose descriptor the process then reuses for an unrelated file,
// and matching on the number alone refused that file forever after. In this
// repository's own suite one test failing before Close made every later durable
// test in the package fail with "already has a writer: descriptor 4", turning a
// single timing-sensitive failure into forty unattributable ones.
//
// A stale entry is therefore dropped rather than honoured. If a live descriptor
// carries this number and does not name the entry's file, that entry's
// descriptor was closed — which also released its flock, so there is nothing
// left to hold.
func LockWriter(file *os.File) error {
	if file == nil {
		return ErrInvalidWrite
	}
	fd := file.Fd()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	writerLockRegistry.Lock()
	defer writerLockRegistry.Unlock()
	live := writerLockRegistry.entries[:0]
	conflict := false
	for _, entry := range writerLockRegistry.entries {
		if os.SameFile(entry.info, info) {
			conflict = true
		} else if entry.descriptor == fd {
			// The number was recycled onto a different file, so the entry that
			// claimed it no longer has an open descriptor at all.
			continue
		}
		live = append(live, entry)
	}
	// Clearing the tail releases the dropped entries' retained os.FileInfo.
	clear(writerLockRegistry.entries[len(live):])
	writerLockRegistry.entries = live
	if conflict {
		return fmt.Errorf("%w: descriptor %d", ErrWriterLocked, fd)
	}
	if err := lockWriterPlatform(file); err != nil {
		return err
	}
	writerLockRegistry.entries = append(writerLockRegistry.entries, writerLockIdentity{
		descriptor: fd,
		info:       info,
	})
	return nil
}

// UnlockWriter releases a lease acquired by LockWriter. Unknown descriptors
// are ignored so partially constructed stores can use one cleanup path.
func UnlockWriter(file *os.File) error {
	if file == nil {
		return nil
	}
	fd := file.Fd()
	writerLockRegistry.Lock()
	defer writerLockRegistry.Unlock()
	found := -1
	for i := range writerLockRegistry.entries {
		if writerLockRegistry.entries[i].descriptor == fd {
			found = i
			break
		}
	}
	if found < 0 {
		return nil
	}
	if err := unlockWriterPlatform(file); err != nil {
		return err
	}
	last := len(writerLockRegistry.entries) - 1
	writerLockRegistry.entries[found] = writerLockRegistry.entries[last]
	writerLockRegistry.entries[last] = writerLockIdentity{}
	writerLockRegistry.entries = writerLockRegistry.entries[:last]
	return nil
}
