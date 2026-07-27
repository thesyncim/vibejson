package storeio

import (
	"errors"
	"os"
	"testing"
	"time"
)

func TestCommitterManualCheckpointIssuesNoDeviceIOBeforeFlush(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "manual-checkpoint-*")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	pageSize := os.Getpagesize()
	device := newRecordingDevice(8, pageSize)
	committer, err := newCommitter(file, DeviceOptions{
		Backend: BackendPortable, BufferCount: 8, BufferSize: pageSize,
	}, CommitterOptions{
		QueueSlots: 4, MaxPagesPerBatch: 1, ManualCheckpoint: true,
	}, func(*os.File, DeviceOptions) (Device, error) { return device, nil })
	if err != nil {
		t.Fatal(err)
	}

	publishTestGeneration(t, committer, 1,
		[]testPage{{offset: int64(pageSize), data: []byte("page-1")}},
		0, []byte("root-1"))
	publishTestGeneration(t, committer, 2,
		[]testPage{{offset: int64(2 * pageSize), data: []byte("page-2")}},
		0, []byte("root-2"))

	select {
	case <-device.firstStarted:
		close(device.releaseFirst)
		t.Fatal("manual committer touched the device before Flush")
	case <-time.After(20 * time.Millisecond):
	}
	if got := committer.DurableGeneration(); got != 0 {
		t.Fatalf("durable generation before Flush = %d, want 0", got)
	}

	close(device.releaseFirst)
	if err := committer.Flush(); err != nil {
		t.Fatal(err)
	}
	commits := device.snapshot()
	if len(commits) != 1 {
		t.Fatalf("device commits after Flush = %d, want 1", len(commits))
	}
	if commits[0].root != "root-2" || len(commits[0].pages) != 2 {
		t.Fatalf("checkpoint commit = %+v, want both pages under root-2", commits[0])
	}
	if got := committer.DurableGeneration(); got != 2 {
		t.Fatalf("durable generation after Flush = %d, want 2", got)
	}

	publishTestGeneration(t, committer, 3,
		[]testPage{{offset: int64(3 * pageSize), data: []byte("page-3")}},
		0, []byte("root-3"))
	if len(device.snapshot()) != 1 {
		t.Fatal("manual committer touched the device after Flush without a new checkpoint")
	}
	if err := committer.Close(); err != nil {
		t.Fatal(err)
	}
	commits = device.snapshot()
	if len(commits) != 2 || commits[1].root != "root-3" {
		t.Fatalf("Close checkpoint commits = %+v, want root-3", commits)
	}
}

func TestCommitterManualCheckpointReturnsBoundedPressure(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "manual-checkpoint-pressure-*")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	pageSize := os.Getpagesize()
	device := newRecordingDevice(2, pageSize)
	close(device.releaseFirst)
	committer, err := newCommitter(file, DeviceOptions{
		Backend: BackendPortable, BufferCount: 2, BufferSize: pageSize,
	}, CommitterOptions{
		QueueSlots: 2, ManualCheckpoint: true,
	}, func(*os.File, DeviceOptions) (Device, error) { return device, nil })
	if err != nil {
		t.Fatal(err)
	}
	defer committer.Close()

	publishTestGeneration(t, committer, 1, nil, 0, []byte("root-1"))
	publishTestGeneration(t, committer, 2, nil, 0, []byte("root-2"))
	published := committer.PublishedGeneration()
	if _, err := committer.Begin(0); !errors.Is(err, ErrCheckpointRequired) {
		t.Fatalf("Begin at bounded dirty capacity = %v, want %v", err, ErrCheckpointRequired)
	}
	if got := committer.PublishedGeneration(); got != published {
		t.Fatalf("rejected admission published generation %d, want %d", got, published)
	}
	if len(device.snapshot()) != 0 {
		t.Fatal("staging pressure caused an implicit device checkpoint")
	}
	if err := committer.Flush(); err != nil {
		t.Fatal(err)
	}
	if len(device.snapshot()) != 1 {
		t.Fatalf("device commits after pressure Flush = %d, want 1", len(device.snapshot()))
	}
	if committer.NeedsCheckpointFor(1) {
		t.Fatal("Flush returned before checkpoint staging was reusable")
	}
}

func TestCommitterManualCheckpointCapacityPreflight(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "manual-checkpoint-capacity-*")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	pageSize := os.Getpagesize()
	device := newRecordingDevice(4, pageSize)
	close(device.releaseFirst)
	committer, err := newCommitter(file, DeviceOptions{
		Backend: BackendPortable, BufferCount: 4, BufferSize: pageSize,
	}, CommitterOptions{
		QueueSlots: 4, MaxPagesPerBatch: 3, ManualCheckpoint: true,
	}, func(*os.File, DeviceOptions) (Device, error) { return device, nil })
	if err != nil {
		t.Fatal(err)
	}
	defer committer.Close()

	if committer.NeedsCheckpointFor(3) {
		t.Fatal("empty manual committer reported checkpoint pressure")
	}
	held, err := committer.Begin(0)
	if err != nil {
		t.Fatal(err)
	}
	if !committer.NeedsCheckpointFor(3) {
		t.Fatal("preflight admitted a worst-case transaction without enough buffers")
	}
	if err := held.Abort(); err != nil {
		t.Fatal(err)
	}
	if committer.NeedsCheckpointFor(3) {
		t.Fatal("preflight did not observe recycled buffers")
	}
}

func TestCommitterManualCheckpointExcludesPublicationsAfterCut(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "manual-checkpoint-cut-*")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	pageSize := os.Getpagesize()
	device := newRecordingDevice(8, pageSize)
	committer, err := newCommitter(file, DeviceOptions{
		Backend: BackendPortable, BufferCount: 8, BufferSize: pageSize,
	}, CommitterOptions{
		QueueSlots: 4, MaxPagesPerBatch: 1, ManualCheckpoint: true,
	}, func(*os.File, DeviceOptions) (Device, error) { return device, nil })
	if err != nil {
		t.Fatal(err)
	}
	defer committer.Close()

	publishTestGeneration(t, committer, 1,
		[]testPage{{offset: int64(pageSize), data: []byte("page-1")}},
		0, []byte("root-1"))
	publishTestGeneration(t, committer, 2,
		[]testPage{{offset: int64(2 * pageSize), data: []byte("page-2")}},
		0, []byte("root-2"))

	flushed := make(chan error, 1)
	go func() { flushed <- committer.Flush() }()
	select {
	case <-device.firstStarted:
	case <-time.After(time.Second):
		t.Fatal("checkpoint did not reach the device")
	}
	// Flush captured generation two before the worker entered this blocked
	// commit. Generation three is accepted while that cut is still in flight
	// and must remain entirely outside it.
	publishTestGeneration(t, committer, 3,
		[]testPage{{offset: int64(3 * pageSize), data: []byte("page-3")}},
		0, []byte("root-3"))
	close(device.releaseFirst)
	if err := <-flushed; err != nil {
		t.Fatal(err)
	}
	commits := device.snapshot()
	if len(commits) != 1 || commits[0].root != "root-2" {
		t.Fatalf("first checkpoint commits = %+v, want only cut through root-2", commits)
	}
	if got := committer.DurableGeneration(); got != 2 {
		t.Fatalf("durable generation after first cut = %d, want 2", got)
	}
	if err := committer.Flush(); err != nil {
		t.Fatal(err)
	}
	commits = device.snapshot()
	if len(commits) != 2 || commits[1].root != "root-3" {
		t.Fatalf("second checkpoint commits = %+v, want root-3", commits)
	}
}
