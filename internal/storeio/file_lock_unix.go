//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package storeio

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func LockWriter(file *os.File) error {
	if file == nil {
		return ErrInvalidWrite
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return fmt.Errorf("%w: %v", ErrWriterLocked, err)
		}
		return err
	}
	return nil
}

func UnlockWriter(file *os.File) error {
	if file == nil {
		return nil
	}
	return unix.Flock(int(file.Fd()), unix.LOCK_UN)
}
