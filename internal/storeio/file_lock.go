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
	for i := range writerLockRegistry.entries {
		entry := &writerLockRegistry.entries[i]
		if entry.descriptor == fd || os.SameFile(entry.info, info) {
			return fmt.Errorf("%w: descriptor %d", ErrWriterLocked, fd)
		}
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
