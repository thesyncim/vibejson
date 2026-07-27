package storeio

import (
	"fmt"
	"io"
	"os"
)

type portableDevice struct {
	file                     *os.File
	arena                    []byte
	bufferSize               int
	buffers                  int
	seen                     []uint64
	checkpointSync           CheckpointSync
	checkpointBarrier        func(*os.File) error
	checkpointFinalSync      func(*os.File) error
	materializationBarrier   func(*os.File) error
	materializationFinalSync func(*os.File) error
	closed                   bool
}

func openPortableDevice(file *os.File, options DeviceOptions) (*portableDevice, error) {
	if options.BufferCount > int(^uint(0)>>1)/options.BufferSize {
		return nil, fmt.Errorf("%w: staging arena overflow", ErrInvalidWrite)
	}
	arena, err := allocateArena(options.BufferCount * options.BufferSize)
	if err != nil {
		return nil, fmt.Errorf("allocate Store page arena: %w", err)
	}
	checkpointBarrier := dataBarrier
	checkpointFinalSync := dataSync
	if options.CheckpointSync == CheckpointSyncFilesystem {
		// The alternate root must never become filesystem-stable before every
		// page it names. Ordinary mode weakens only the primitive, not the
		// two-phase ordering: plain fsync data, write the alternate root, then
		// plain fsync the root.
		checkpointBarrier = filesystemSync
		checkpointFinalSync = filesystemSync
	}
	return &portableDevice{
		file:                     file,
		arena:                    arena,
		bufferSize:               options.BufferSize,
		buffers:                  options.BufferCount,
		seen:                     make([]uint64, (options.BufferCount+63)/64),
		checkpointSync:           options.CheckpointSync,
		checkpointBarrier:        checkpointBarrier,
		checkpointFinalSync:      checkpointFinalSync,
		materializationBarrier:   materializationPhaseBarrier,
		materializationFinalSync: materializationSync,
	}, nil
}

func (*portableDevice) Backend() Backend { return BackendPortable }

func (d *portableDevice) Buffer(index int) ([]byte, error) {
	if d.closed {
		return nil, ErrClosed
	}
	if index < 0 || index >= d.buffers {
		return nil, ErrInvalidWrite
	}
	start := index * d.bufferSize
	return d.arena[start : start+d.bufferSize], nil
}

func (d *portableDevice) Commit(pages []Write, root Write) error {
	if d.closed {
		return ErrClosed
	}
	if err := validateCommit(d.buffers, d.bufferSize, d.seen, pages, root); err != nil {
		return err
	}
	if err := writeDataPages(d.file, d.arena, d.bufferSize, pages); err != nil {
		return err
	}
	if len(pages) != 0 {
		barrier := d.checkpointBarrier
		if barrier == nil {
			barrier = dataBarrier
		}
		if err := barrier(d.file); err != nil {
			return err
		}
	}
	if err := d.write(root); err != nil {
		return commitOutcomeUnknown(err)
	}
	finalSync := d.checkpointFinalSync
	if finalSync == nil {
		finalSync = dataSync
	}
	return commitOutcomeUnknown(finalSync(d.file))
}

func (d *portableDevice) CommitMaterialized(
	journal Write,
	targets []Write,
	root Write,
	mode materializationCommitMode,
) (materializationCommitResult, error) {
	if d.closed {
		return materializationCommitResult{}, ErrClosed
	}
	if mode != materializationPatchOnly && mode != materializationHybrid {
		return materializationCommitResult{}, ErrInvalidWrite
	}
	if _, err := validateWrite(
		d.buffers, d.bufferSize, journal,
	); err != nil {
		return materializationCommitResult{}, err
	}
	if err := validateCommit(
		d.buffers, d.bufferSize, d.seen, targets, root,
	); err != nil {
		return materializationCommitResult{}, err
	}
	if err := d.write(journal); err != nil {
		return materializationCommitResult{}, err
	}
	barrier := d.materializationBarrier
	if barrier == nil {
		barrier = materializationPhaseBarrier
	}
	if err := barrier(d.file); err != nil {
		return materializationCommitResult{}, err
	}
	if err := writeDataPages(
		d.file, d.arena, d.bufferSize, targets,
	); err != nil {
		return materializationCommitResult{
			CompletedPhases: 1, CompletedBarriers: 1,
		}, err
	}
	result := materializationCommitResult{
		CompletedPhases: 1, CompletedBarriers: 1,
	}
	if mode == materializationHybrid {
		if err := barrier(d.file); err != nil {
			return result, err
		}
		result.CompletedPhases = 2
		result.CompletedBarriers = 2
	} else {
		result.CompletedPhases = 2
	}
	if err := d.write(root); err != nil {
		result.RootAttempted = true
		return result, err
	}
	result.RootAttempted = true
	finalSync := d.materializationFinalSync
	if finalSync == nil {
		finalSync = materializationSync
	}
	if err := finalSync(d.file); err != nil {
		return result, err
	}
	result.CompletedPhases = 3
	result.CompletedBarriers++
	return result, nil
}

func (d *portableDevice) write(write Write) error {
	return writeArenaAt(d.file, d.arena, d.bufferSize, write)
}

func writeArenaAt(file *os.File, arena []byte, bufferSize int, write Write) error {
	start := int(write.Buffer) * bufferSize
	data := arena[start : start+int(write.Length)]
	n, err := file.WriteAt(data, write.Offset)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	if err != nil {
		return err
	}
	return nil
}

func (d *portableDevice) Close() error {
	if d == nil || d.closed {
		return nil
	}
	d.closed = true
	err := releaseArena(d.arena)
	d.arena = nil
	d.seen = nil
	if err != nil {
		return fmt.Errorf("release Store page arena: %w", err)
	}
	return nil
}
