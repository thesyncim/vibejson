//go:build windows

package storeio

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

func LockWriter(file *os.File) error {
	if file == nil {
		return ErrInvalidWrite
	}
	var overlapped windows.Overlapped
	err := windows.LockFileEx(windows.Handle(file.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, ^uint32(0), ^uint32(0), &overlapped)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
		return fmt.Errorf("%w: %v", ErrWriterLocked, err)
	}
	return err
}

func UnlockWriter(file *os.File) error {
	if file == nil {
		return nil
	}
	var overlapped windows.Overlapped
	return windows.UnlockFileEx(windows.Handle(file.Fd()), 0, ^uint32(0), ^uint32(0), &overlapped)
}
