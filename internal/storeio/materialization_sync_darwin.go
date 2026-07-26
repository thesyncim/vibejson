//go:build darwin

package storeio

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// materializationPhaseBarrier orders journal before targets and targets before
// the alternate root without draining the device queue. APFS and HFS implement
// F_BARRIERFSYNC; an unsupported filesystem falls back to the stronger full
// flush rather than weakening canonical-overwrite recovery.
func materializationPhaseBarrier(file *os.File) error {
	_, err := unix.FcntlInt(file.Fd(), unix.F_BARRIERFSYNC, 0)
	if err == nil {
		return nil
	}
	if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOTSUP) ||
		errors.Is(err, unix.ENOTTY) {
		return materializationSync(file)
	}
	return err
}

// materializationSync is the final commit/recovery durability boundary on
// Darwin. F_FULLFSYNC drains volatile drive caches before acknowledgement.
func materializationSync(file *os.File) error {
	_, err := unix.FcntlInt(file.Fd(), unix.F_FULLFSYNC, 0)
	return err
}
