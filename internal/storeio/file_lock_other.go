//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !windows

package storeio

import "os"

func LockWriter(*os.File) error   { return ErrWriterLockUnsupported }
func UnlockWriter(*os.File) error { return nil }
