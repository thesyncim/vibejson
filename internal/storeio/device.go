package storeio

import (
	"errors"
	"fmt"
	"os"
)

const (
	defaultBufferCount  = 64
	defaultBufferSize   = 64 << 10
	maxDeviceQueueDepth = 32768
	maxDeviceBuffers    = maxDeviceQueueDepth
)

var (
	// ErrInvalidWrite reports a buffer index, byte length, file offset, order,
	// or batch size outside the Device contract.
	ErrInvalidWrite = errors.New("simdjson: invalid Store page write")
	// ErrOverlappingWrite reports physical ranges that could overwrite one
	// another or the root descriptor in a single commit.
	ErrOverlappingWrite = errors.New("simdjson: overlapping Store page writes")
	// ErrDuplicateBuffer reports one staging buffer submitted by two concurrent
	// data-page writes. The root may reuse a data buffer because the data phase
	// completes before the root phase begins.
	ErrDuplicateBuffer = errors.New("simdjson: duplicate Store page buffer")
)

// Backend selects the internal Store page-I/O implementation.
type Backend uint8

const (
	// BackendAuto tries the native asynchronous backend and falls back to the
	// portable correctness implementation when the OS or sandbox rejects it.
	BackendAuto Backend = iota
	// BackendPortable uses positional file writes and the platform's data-
	// integrity barrier (fdatasync on Linux, File.Sync elsewhere).
	BackendPortable
	// BackendIOUring requires the pure-Go Linux io_uring backend.
	BackendIOUring
)

func (b Backend) String() string {
	switch b {
	case BackendAuto:
		return "auto"
	case BackendPortable:
		return "portable"
	case BackendIOUring:
		return "io_uring"
	default:
		return "unknown"
	}
}

// DeviceOptions fixes the bounded staging and submission storage of a Device.
// Zero selects 64 buffers of 64 KiB each and an implementation-sized queue.
type DeviceOptions struct {
	Backend Backend
	// BufferCount is the number of reusable page staging buffers.
	BufferCount int
	// BufferSize is the equal byte size of every staging buffer. It must be a
	// multiple of the host page size so native fixed-buffer I/O stays aligned.
	BufferSize int
	// QueueDepth is the requested native submission depth. Zero selects at
	// least BufferCount. The native backend may round it to a power of two.
	QueueDepth int
	// SingleIssuer enables native same-thread optimizations. The owner must
	// lock its goroutine to one OS thread before OpenDevice and keep it locked
	// through every operation and Close. Store's background writer does this.
	SingleIssuer bool
}

func (o DeviceOptions) normalized() (DeviceOptions, error) {
	if o.Backend > BackendIOUring {
		return DeviceOptions{}, fmt.Errorf("%w: unknown backend %d", ErrInvalidWrite, o.Backend)
	}
	if o.BufferCount == 0 {
		o.BufferCount = defaultBufferCount
	}
	if o.BufferSize == 0 {
		o.BufferSize = defaultBufferSize
	}
	pageSize := os.Getpagesize()
	if o.BufferCount < 1 || o.BufferCount > maxDeviceBuffers || o.BufferSize < pageSize ||
		o.BufferSize%pageSize != 0 || uint64(o.BufferSize) > uint64(^uint32(0)) {
		return DeviceOptions{}, fmt.Errorf("%w: buffers=%d size=%d", ErrInvalidWrite, o.BufferCount, o.BufferSize)
	}
	if o.QueueDepth == 0 {
		o.QueueDepth = o.BufferCount
	}
	if o.QueueDepth < o.BufferCount || o.QueueDepth > maxDeviceQueueDepth {
		return DeviceOptions{}, fmt.Errorf("%w: queue depth %d for %d buffers", ErrInvalidWrite, o.QueueDepth, o.BufferCount)
	}
	return o, nil
}

// Write names one initialized prefix of a Device staging buffer and its
// physical file offset. Data-page writes in a commit must be ordered by Offset,
// non-overlapping, and use distinct buffers.
type Write struct {
	Offset int64
	Length uint32
	Buffer uint16
	kind   PageKind
}

// Device is the internal, single-owner durable page-I/O boundary. Buffer
// storage is fixed at construction and is reused for every commit. Commit is
// synchronous at this layer: it returns only after data pages, a data barrier,
// the alternate root, and its final barrier have completed in that order.
// Store's background writer supplies asynchronous application semantics above
// this boundary.
//
// Device does not own the file passed to OpenDevice. The file and Device must
// remain open until all operations finish.
type Device interface {
	Backend() Backend
	Buffer(index int) ([]byte, error)
	Commit(pages []Write, root Write) error
	Close() error
}

// OpenDevice constructs a bounded page-I/O backend over file.
func OpenDevice(file *os.File, options DeviceOptions) (Device, error) {
	if file == nil {
		return nil, fmt.Errorf("%w: nil file", ErrInvalidWrite)
	}
	normalized, err := options.normalized()
	if err != nil {
		return nil, err
	}
	if normalized.Backend != BackendPortable {
		device, ringErr := openRingDevice(file, normalized)
		if ringErr == nil {
			return device, nil
		}
		if normalized.Backend == BackendIOUring || !ringFallbackError(ringErr) {
			return nil, ringErr
		}
	}
	return openPortableDevice(file, normalized)
}

func ringFallbackError(err error) bool {
	return errors.Is(err, ErrUnavailable) || errors.Is(err, ErrUnsupported)
}

func validateCommit(bufferCount, bufferSize int, seen []uint64, pages []Write, root Write) error {
	clear(seen)
	var previousEnd int64
	for i, write := range pages {
		end, err := validateWrite(bufferCount, bufferSize, write)
		if err != nil {
			return err
		}
		word, bit := int(write.Buffer)>>6, uint(write.Buffer)&63
		mask := uint64(1) << bit
		if seen[word]&mask != 0 {
			return ErrDuplicateBuffer
		}
		seen[word] |= mask
		if i != 0 && write.Offset < previousEnd {
			return ErrOverlappingWrite
		}
		previousEnd = end
	}
	rootEnd, err := validateWrite(bufferCount, bufferSize, root)
	if err != nil {
		return err
	}
	for _, write := range pages {
		writeEnd := write.Offset + int64(write.Length)
		if write.Offset < rootEnd && root.Offset < writeEnd {
			return ErrOverlappingWrite
		}
	}
	return nil
}

func validateWrite(bufferCount, bufferSize int, write Write) (int64, error) {
	if int(write.Buffer) >= bufferCount || write.Length == 0 || uint64(write.Length) > uint64(bufferSize) ||
		write.Offset < 0 || int64(write.Length) > int64(^uint64(0)>>1)-write.Offset {
		return 0, ErrInvalidWrite
	}
	return write.Offset + int64(write.Length), nil
}
