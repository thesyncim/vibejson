package storeio

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestPortableCommitOrderingAndValidation(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "portable-pages")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	pageSize := os.Getpagesize()
	device, err := OpenDevice(file, DeviceOptions{
		Backend: BackendPortable, BufferCount: 3, BufferSize: pageSize,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := device.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()
	if device.Backend() != BackendPortable {
		t.Fatalf("backend = %v, want portable", device.Backend())
	}

	page0 := []byte("page-zero")
	page1 := []byte("page-one")
	root := []byte("root-generation-two")
	copy(deviceBuffer(t, device, 0), page0)
	copy(deviceBuffer(t, device, 1), page1)
	copy(deviceBuffer(t, device, 2), root)
	writes := [...]Write{
		{Offset: int64(pageSize), Length: uint32(len(page0)), Buffer: 0},
		{Offset: int64(2 * pageSize), Length: uint32(len(page1)), Buffer: 1},
	}
	rootWrite := Write{Offset: 0, Length: uint32(len(root)), Buffer: 2}
	if err := device.Commit(writes[:], rootWrite); err != nil {
		t.Fatal(err)
	}
	assertFileBytes(t, file, int64(pageSize), page0)
	assertFileBytes(t, file, int64(2*pageSize), page1)
	assertFileBytes(t, file, 0, root)

	tests := []struct {
		name  string
		pages []Write
		root  Write
		want  error
	}{
		{name: "unsorted", pages: []Write{writes[1], writes[0]}, root: rootWrite, want: ErrOverlappingWrite},
		{name: "data overlap", pages: []Write{{Offset: 10, Length: 4}, {Offset: 12, Length: 4, Buffer: 1}}, root: rootWrite, want: ErrOverlappingWrite},
		{name: "root overlap", pages: writes[:1], root: Write{Offset: int64(pageSize) + 1, Length: 2, Buffer: 2}, want: ErrOverlappingWrite},
		{name: "duplicate buffer", pages: []Write{{Offset: int64(pageSize), Length: 1}, {Offset: int64(2 * pageSize), Length: 1}}, root: rootWrite, want: ErrDuplicateBuffer},
		{name: "bad buffer", root: Write{Offset: 0, Length: 1, Buffer: 3}, want: ErrInvalidWrite},
		{name: "oversize length", root: Write{Offset: 0, Length: ^uint32(0), Buffer: 2}, want: ErrInvalidWrite},
		{name: "zero length", root: Write{Offset: 0, Buffer: 2}, want: ErrInvalidWrite},
		{name: "negative offset", root: Write{Offset: -1, Length: 1, Buffer: 2}, want: ErrInvalidWrite},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := device.Commit(test.pages, test.root); !errors.Is(err, test.want) {
				t.Fatalf("Commit error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestPortableRootOnlyCommitAndClose(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "portable-root")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	device, err := OpenDevice(file, DeviceOptions{
		Backend: BackendPortable, BufferCount: 1, BufferSize: os.Getpagesize(),
	})
	if err != nil {
		t.Fatal(err)
	}
	root := []byte("root-only")
	copy(deviceBuffer(t, device, 0), root)
	if err := device.Commit(nil, Write{Length: uint32(len(root))}); err != nil {
		t.Fatal(err)
	}
	assertFileBytes(t, file, 0, root)
	if err := device.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := device.Buffer(0); !errors.Is(err, ErrClosed) {
		t.Fatalf("Buffer after Close = %v, want %v", err, ErrClosed)
	}
	if err := device.Commit(nil, Write{Length: 1}); !errors.Is(err, ErrClosed) {
		t.Fatalf("Commit after Close = %v, want %v", err, ErrClosed)
	}
}

func TestDeviceOptionsValidation(t *testing.T) {
	if _, err := OpenDevice(nil, DeviceOptions{}); !errors.Is(err, ErrInvalidWrite) {
		t.Fatalf("nil file error = %v, want %v", err, ErrInvalidWrite)
	}
	file, err := os.CreateTemp(t.TempDir(), "device-options")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	pageSize := os.Getpagesize()
	tests := []struct {
		name    string
		options DeviceOptions
	}{
		{name: "backend", options: DeviceOptions{Backend: BackendIOUring + 1}},
		{name: "buffers", options: DeviceOptions{Backend: BackendPortable, BufferCount: maxDeviceBuffers + 1}},
		{name: "short buffer", options: DeviceOptions{Backend: BackendPortable, BufferSize: pageSize - 1}},
		{name: "unaligned buffer", options: DeviceOptions{Backend: BackendPortable, BufferSize: pageSize + 1}},
		{name: "short queue", options: DeviceOptions{Backend: BackendPortable, BufferCount: 2, QueueDepth: 1}},
		{name: "large queue", options: DeviceOptions{Backend: BackendPortable, QueueDepth: maxDeviceQueueDepth + 1}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := OpenDevice(file, test.options); !errors.Is(err, ErrInvalidWrite) {
				t.Fatalf("OpenDevice error = %v, want %v", err, ErrInvalidWrite)
			}
		})
	}
}

func TestPortableDataFailureDoesNotWriteRoot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "read-only-store")
	wantRoot := []byte("old-root")
	if err := os.WriteFile(path, wantRoot, 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	pageSize := os.Getpagesize()
	device, err := OpenDevice(file, DeviceOptions{
		Backend: BackendPortable, BufferCount: 2, BufferSize: pageSize,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer device.Close()
	copy(deviceBuffer(t, device, 0), "new-page")
	copy(deviceBuffer(t, device, 1), "new-root")
	if err := device.Commit(
		[]Write{{Offset: int64(pageSize), Length: 8, Buffer: 0}},
		Write{Offset: 0, Length: 8, Buffer: 1},
	); err == nil {
		t.Fatal("Commit to read-only file succeeded")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(wantRoot) {
		t.Fatalf("root changed after data failure: %q", got)
	}
}

func deviceBuffer(t *testing.T, device Device, index int) []byte {
	t.Helper()
	buffer, err := device.Buffer(index)
	if err != nil {
		t.Fatal(err)
	}
	return buffer
}

func assertFileBytes(t *testing.T, file *os.File, offset int64, want []byte) {
	t.Helper()
	got := make([]byte, len(want))
	if _, err := file.ReadAt(got, offset); err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("bytes at %d = %q, want %q", offset, got, want)
	}
}
