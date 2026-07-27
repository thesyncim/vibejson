package durable

import (
	"bytes"
	"errors"
	"os"
	"testing"
	"time"
)

func TestFileStoreBufferedVisibleCrashBoundary(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "buffered-visible-*")
	if err != nil {
		t.Fatal(err)
	}
	options := testFileStoreOptions()
	options.Durability = DurabilityBufferedVisible
	options.DisableMutationCombining = true
	collection, err := Create(file, options)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = collection.Close()
		_ = file.Close()
	}()

	before, err := os.ReadFile(file.Name())
	if err != nil {
		t.Fatal(err)
	}
	value := []byte(`{"value":"buffered"}`)
	if created, err := collection.Put("key", value); err != nil || !created {
		t.Fatalf("Put = (%v, %v), want created", created, err)
	}
	if got, want := collection.Generation(), uint64(2); got != want {
		t.Fatalf("visible generation = %d, want %d", got, want)
	}
	if got, want := collection.DurableGeneration(), uint64(1); got != want {
		t.Fatalf("durable generation = %d, want %d", got, want)
	}
	got, found, err := collection.AppendRaw(nil, "key")
	if err != nil || !found || !bytes.Equal(got, value) {
		t.Fatalf("visible read = (%q, %v, %v), want %q", got, found, err, value)
	}
	during, err := os.ReadFile(file.Name())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(during, before) {
		t.Fatal("acknowledged buffered Put changed the file before a checkpoint")
	}

	beforeCrash := openBufferedImage(t, before, options)
	if got, found, err := beforeCrash.AppendRaw(nil, "key"); err != nil || found {
		t.Fatalf("recovery before checkpoint = (%q, %v, %v), want absent", got, found, err)
	}

	if err := collection.Flush(); err != nil {
		t.Fatal(err)
	}
	if got, want := collection.DurableGeneration(), collection.Generation(); got != want {
		t.Fatalf("checkpoint generations = durable %d, visible %d", got, want)
	}
	after, err := os.ReadFile(file.Name())
	if err != nil {
		t.Fatal(err)
	}
	afterCrash := openBufferedImage(t, after, options)
	got, found, err = afterCrash.AppendRaw(nil, "key")
	if err != nil || !found || !bytes.Equal(got, value) {
		t.Fatalf("recovery after checkpoint = (%q, %v, %v), want %q", got, found, err, value)
	}
}

func TestFileStoreBufferedVisibleCloseCheckpoints(t *testing.T) {
	path := t.TempDir() + "/buffered-close.db"
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	options := testFileStoreOptions()
	options.Durability = DurabilityBufferedVisible
	options.DisableMutationCombining = true
	collection, err := Create(file, options)
	if err != nil {
		t.Fatal(err)
	}
	value := []byte(`{"value":"close"}`)
	if _, err := collection.Put("key", value); err != nil {
		t.Fatal(err)
	}
	if err := collection.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	reopenedFile, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer reopenedFile.Close()
	reopened, err := Open(reopenedFile, options)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got, found, err := reopened.AppendRaw(nil, "key")
	if err != nil || !found || !bytes.Equal(got, value) {
		t.Fatalf("read after Close checkpoint = (%q, %v, %v), want %q", got, found, err, value)
	}
}

func TestFileStoreBufferedVisibleFlushTakesWriterCut(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "buffered-flush-lock-*")
	if err != nil {
		t.Fatal(err)
	}
	options := testFileStoreOptions()
	options.Durability = DurabilityBufferedVisible
	options.DisableMutationCombining = true
	collection, err := Create(file, options)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = collection.Close()
		_ = file.Close()
	}()

	collection.writer.Lock()
	flushed := make(chan error, 1)
	go func() { flushed <- collection.Flush() }()
	select {
	case err := <-flushed:
		collection.writer.Unlock()
		t.Fatalf("Flush bypassed writer cut: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	collection.writer.Unlock()
	select {
	case err := <-flushed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Flush did not complete after writer cut was released")
	}
}

func TestFileStoreBufferedVisibleCheckpointFailureIsSticky(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "buffered-failure-*")
	if err != nil {
		t.Fatal(err)
	}
	options := testFileStoreOptions()
	options.Durability = DurabilityBufferedVisible
	options.DisableMutationCombining = true
	collection, err := Create(file, options)
	if err != nil {
		t.Fatal(err)
	}
	value := []byte(`{"value":"visible"}`)
	if _, err := collection.Put("key", value); err != nil {
		t.Fatal(err)
	}

	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	checkpointErr := collection.Flush()
	if checkpointErr == nil {
		t.Fatal("checkpoint on a closed device succeeded")
	}
	if persistErr := collection.PersistenceError(); !errors.Is(checkpointErr, persistErr) {
		t.Fatalf("PersistenceError = %v, checkpoint error = %v", persistErr, checkpointErr)
	}
	got, found, err := collection.AppendRaw(nil, "key")
	if err != nil || !found || !bytes.Equal(got, value) {
		t.Fatalf("read after failed checkpoint = (%q, %v, %v), want %q", got, found, err, value)
	}
	if _, err := collection.Put("later", []byte(`{"value":2}`)); !errors.Is(err, checkpointErr) {
		t.Fatalf("Put after failed checkpoint = %v, want sticky %v", err, checkpointErr)
	}
	if err := collection.Flush(); !errors.Is(err, checkpointErr) {
		t.Fatalf("second Flush = %v, want sticky %v", err, checkpointErr)
	}
	if err := collection.Close(); !errors.Is(err, checkpointErr) {
		t.Fatalf("Close = %v, want sticky %v", err, checkpointErr)
	}
}

func TestFileStoreBufferedVisibleRejectsCanonicalMaterialization(t *testing.T) {
	options := testFileStoreOptions()
	options.Durability = DurabilityBufferedVisible
	options.MaterializationDamageGranule = 512
	if _, err := options.normalized(); err == nil {
		t.Fatal("buffered-visible canonical materialization was accepted")
	}
}

func openBufferedImage(t *testing.T, image []byte, options Options) *Collection {
	t.Helper()
	path := t.TempDir() + "/crash-image.db"
	if err := os.WriteFile(path, image, 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	collection, err := Open(file, options)
	if err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = collection.Close()
		_ = file.Close()
	})
	return collection
}
