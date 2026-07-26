package storeio

import (
	"fmt"
	"io"
	"os"
)

const (
	ringDataSyncTag = ^uint64(0) - iota
	ringRootTag
	ringRootSyncTag
)

type ringDevice struct {
	ring       *Ring
	bufferSize int
	buffers    int
	seen       []uint64
	closed     bool
}

func openRingDevice(file *os.File, options DeviceOptions) (*ringDevice, error) {
	ring, err := Open(Config{Entries: uint32(options.QueueDepth), SingleIssuer: options.SingleIssuer})
	if err != nil {
		return nil, err
	}
	if err := ring.RegisterFiles([]int{int(file.Fd())}); err != nil {
		_ = ring.Close()
		return nil, classifyRingSetupError("register file", err)
	}
	if err := ring.RegisterBuffers(options.BufferCount, options.BufferSize); err != nil {
		_ = ring.Close()
		return nil, classifyRingSetupError("register buffers", err)
	}
	return &ringDevice{
		ring:       ring,
		bufferSize: options.BufferSize,
		buffers:    options.BufferCount,
		seen:       make([]uint64, (options.BufferCount+63)/64),
	}, nil
}

func classifyRingSetupError(operation string, err error) error {
	if ringSetupUnavailable(err) {
		return fmt.Errorf("%w: io_uring %s: %v", ErrUnavailable, operation, err)
	}
	return err
}

func (*ringDevice) Backend() Backend { return BackendIOUring }

func (d *ringDevice) Buffer(index int) ([]byte, error) { return d.ring.Buffer(index) }

func (d *ringDevice) Commit(pages []Write, root Write) error {
	if d.closed {
		return ErrClosed
	}
	if err := validateCommit(d.buffers, d.bufferSize, d.seen, pages, root); err != nil {
		return err
	}
	for i, write := range pages {
		if err := d.ring.PrepareWriteFixed(0, int(write.Buffer), int(write.Length), write.Offset, uint64(i), false); err != nil {
			return err
		}
	}
	if len(pages) != 0 {
		if err := d.ring.SubmitAndWait(uint32(len(pages))); err != nil {
			return err
		}
		var first error
		for range pages {
			completion, ok, err := d.ring.Pop()
			if err != nil {
				return err
			}
			if !ok || completion.UserData >= uint64(len(pages)) {
				return ErrOverflow
			}
			write := pages[completion.UserData]
			if err := completionResult(completion, write.Length); first == nil && err != nil {
				first = err
			}
		}
		if first != nil {
			return first
		}
	}

	if len(pages) != 0 {
		if err := d.ring.PrepareDataSync(0, ringDataSyncTag, true); err != nil {
			return err
		}
	}
	if err := d.ring.PrepareWriteFixed(0, int(root.Buffer), int(root.Length), root.Offset, ringRootTag, true); err != nil {
		return err
	}
	if err := d.ring.PrepareDataSync(0, ringRootSyncTag, false); err != nil {
		return err
	}
	want := uint32(2)
	if len(pages) != 0 {
		want++
	}
	if err := d.ring.SubmitAndWait(want); err != nil {
		return commitOutcomeUnknown(err)
	}
	var first error
	for range want {
		completion, ok, err := d.ring.Pop()
		if err != nil {
			return commitOutcomeUnknown(err)
		}
		if !ok {
			return commitOutcomeUnknown(ErrOverflow)
		}
		var expected uint32
		switch completion.UserData {
		case ringDataSyncTag, ringRootSyncTag:
		case ringRootTag:
			expected = root.Length
		default:
			return commitOutcomeUnknown(ErrOverflow)
		}
		if err := completionResult(completion, expected); first == nil && err != nil {
			first = err
		}
	}
	return commitOutcomeUnknown(first)
}

func (d *ringDevice) CommitMaterialized(
	journal Write,
	targets []Write,
	root Write,
) (uint32, error) {
	if d.closed {
		return 0, ErrClosed
	}
	if _, err := validateWrite(
		d.buffers, d.bufferSize, journal,
	); err != nil {
		return 0, err
	}
	if err := validateCommit(
		d.buffers, d.bufferSize, d.seen, targets, root,
	); err != nil {
		return 0, err
	}
	journalPhase := [1]Write{journal}
	if err := d.materializationPhase(journalPhase[:]); err != nil {
		return 0, err
	}
	if err := d.materializationPhase(targets); err != nil {
		return 1, err
	}
	rootPhase := [1]Write{root}
	if err := d.materializationPhase(rootPhase[:]); err != nil {
		return 2, commitOutcomeUnknown(err)
	}
	return 3, nil
}

func (d *ringDevice) materializationPhase(writes []Write) error {
	if len(writes) == 0 {
		return ErrInvalidWrite
	}
	for rank, write := range writes {
		if err := d.ring.PrepareWriteFixed(
			0, int(write.Buffer), int(write.Length), write.Offset,
			uint64(rank), false,
		); err != nil {
			return err
		}
	}
	if err := d.ring.SubmitAndWait(uint32(len(writes))); err != nil {
		return err
	}
	var first error
	for range writes {
		completion, ok, err := d.ring.Pop()
		if err != nil {
			return err
		}
		if !ok || completion.UserData >= uint64(len(writes)) {
			return ErrOverflow
		}
		write := writes[completion.UserData]
		if err := completionResult(
			completion, write.Length,
		); first == nil && err != nil {
			first = err
		}
	}
	if first != nil {
		return first
	}
	if err := d.ring.PrepareDataSync(
		0, ringDataSyncTag, false,
	); err != nil {
		return err
	}
	if err := d.ring.SubmitAndWait(1); err != nil {
		return err
	}
	completion, ok, err := d.ring.Pop()
	if err != nil {
		return err
	}
	if !ok || completion.UserData != ringDataSyncTag {
		return ErrOverflow
	}
	return completionResult(completion, 0)
}

func completionResult(completion Completion, expected uint32) error {
	if err := completion.Err(); err != nil {
		return err
	}
	if uint32(completion.Result) != expected {
		return io.ErrShortWrite
	}
	return nil
}

func (d *ringDevice) Close() error {
	if d == nil || d.closed {
		return nil
	}
	d.closed = true
	d.seen = nil
	return d.ring.Close()
}
