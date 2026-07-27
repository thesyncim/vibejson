package durable

import (
	"os"
	"strings"
	"testing"

	"github.com/thesyncim/vibejson/internal/storeio"
	"github.com/thesyncim/vibejson/store"
)

// TestSingleDocumentTransactionFitsExactReservation leaves exactly the point
// transaction reservation free in the manual committer, then writes the
// largest accepted document and key with index maintenance enabled. Any missed
// overflow, directory, free-log, or retirement consumer fails admission or
// exhausts the transaction before publication.
func TestSingleDocumentTransactionFitsExactReservation(t *testing.T) {
	options := Options{
		Collection: store.Options{ChunkDocuments: 64},
		Indexes: []store.IndexDefinition{
			{Name: "indexed", Paths: []string{"/indexed"}},
		},
		PageSize: 4096, MaxPageSize: 64 << 10,
		ResidentBytes:            64 << 20,
		MaxKeyBytes:              128,
		InlineValueBytes:         512,
		MaxDocumentBytes:         64 << 10,
		MaxBatchDocuments:        64,
		Backend:                  BackendPortable,
		Durability:               DurabilityBufferedVisible,
		DisableMutationCombining: true,
	}
	normalized, err := options.normalized()
	if err != nil {
		t.Fatal(err)
	}
	if normalized.singleDocumentTransactionPages >= normalized.maxTransactionPages {
		t.Fatalf(
			"single-document pages = %d, batch pages = %d; test needs a narrower point reservation",
			normalized.singleDocumentTransactionPages,
			normalized.maxTransactionPages,
		)
	}

	file, err := os.CreateTemp(t.TempDir(), "single-document-bound-*")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	collection, err := Create(file, options)
	if err != nil {
		t.Fatal(err)
	}
	defer collection.Close()
	if err := collection.Flush(); err != nil {
		t.Fatal(err)
	}

	// Consume every buffer except the point operation's data-page reservation
	// and alternate root. Reservations may span several batch descriptors, but
	// their total is exact.
	consume := collection.options.BufferCount -
		(collection.options.singleDocumentTransactionPages + 1)
	held := make([]*storeio.Batch, 0, 4)
	for consume > 0 {
		reservation := min(
			consume, collection.options.maxTransactionPages+1,
		)
		batch, beginErr := collection.committer.Begin(reservation - 1)
		if beginErr != nil {
			t.Fatalf(
				"reserve %d of %d exact-bound buffers: %v",
				reservation, consume, beginErr,
			)
		}
		held = append(held, batch)
		consume -= reservation
	}
	releaseHeld := func() {
		t.Helper()
		for i := len(held) - 1; i >= 0; i-- {
			if abortErr := held[i].Abort(); abortErr != nil {
				t.Fatalf("release held reservation %d: %v", i, abortErr)
			}
		}
		held = held[:0]
	}
	defer func() {
		if len(held) != 0 {
			releaseHeld()
		}
	}()

	prefix := `{"indexed":"maximum","padding":"`
	suffix := `"}`
	document := []byte(prefix + strings.Repeat(
		"x", collection.options.MaxDocumentBytes-len(prefix)-len(suffix),
	) + suffix)
	if len(document) != collection.options.MaxDocumentBytes {
		t.Fatalf(
			"maximum document length = %d, want %d",
			len(document), collection.options.MaxDocumentBytes,
		)
	}
	key := strings.Repeat("k", collection.options.MaxKeyBytes)
	checkpoints := collection.automaticCheckpoints.Load()
	created, err := collection.Put(key, document)
	if err != nil || !created {
		t.Fatalf("maximum indexed Put = (%v,%v), want (true,nil)", created, err)
	}
	if got := collection.automaticCheckpoints.Load(); got != checkpoints {
		t.Fatalf(
			"point preflight checkpointed with its exact reservation free: %d -> %d",
			checkpoints, got,
		)
	}
	releaseHeld()
	if err := collection.Flush(); err != nil {
		t.Fatal(err)
	}
}
