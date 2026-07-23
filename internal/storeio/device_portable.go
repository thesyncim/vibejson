package storeio

import (
	"fmt"
	"io"
	"os"
)

type portableDevice struct {
	file       *os.File
	arena      []byte
	bufferSize int
	buffers    int
	seen       []uint64
	closed     bool
}

func openPortableDevice(file *os.File, options DeviceOptions) (*portableDevice, error) {
	if options.BufferCount > int(^uint(0)>>1)/options.BufferSize {
		return nil, fmt.Errorf("%w: staging arena overflow", ErrInvalidWrite)
	}
	arena, err := allocateArena(options.BufferCount * options.BufferSize)
	if err != nil {
		return nil, fmt.Errorf("allocate Store page arena: %w", err)
	}
	return &portableDevice{
		file:       file,
		arena:      arena,
		bufferSize: options.BufferSize,
		buffers:    options.BufferCount,
		seen:       make([]uint64, (options.BufferCount+63)/64),
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
	for _, write := range pages {
		if err := d.write(write); err != nil {
			return err
		}
	}
	if len(pages) != 0 {
		if err := dataSync(d.file); err != nil {
			return err
		}
	}
	if err := d.write(root); err != nil {
		return err
	}
	return dataSync(d.file)
}

func (d *portableDevice) write(write Write) error {
	start := int(write.Buffer) * d.bufferSize
	data := d.arena[start : start+int(write.Length)]
	n, err := d.file.WriteAt(data, write.Offset)
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
