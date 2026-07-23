//go:build linux

package storeio

import (
	"os"
	"syscall"
)

func dataSync(file *os.File) error {
	_, _, errno := syscall.Syscall(syscall.SYS_FDATASYNC, file.Fd(), 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}
