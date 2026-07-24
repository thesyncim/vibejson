//go:build darwin

package storeio

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func dataSync(file *os.File) error { return file.Sync() }

// dataBarrier orders the data-page phase before the following alternate-root
// write. F_BARRIERFSYNC performs an fsync and issues a device barrier, which is
// the Darwin primitive intended to order two phases without draining the
// device queue. The final root phase still uses file.Sync, so this changes
// neither Commit's return point nor its existing durability guarantee.
//
// HFS and APFS implement the command. Preserve portable behavior on other
// filesystems that reject it, but propagate descriptor and I/O failures.
func dataBarrier(file *os.File) error {
	_, err := unix.FcntlInt(file.Fd(), unix.F_BARRIERFSYNC, 0)
	if err == nil {
		return nil
	}
	if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOTSUP) ||
		errors.Is(err, unix.ENOTTY) {
		return file.Sync()
	}
	return err
}
