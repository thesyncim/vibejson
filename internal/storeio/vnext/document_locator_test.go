package vnext

import (
	"bytes"
	"testing"

	"github.com/thesyncim/vibejson/internal/storeio"
)

func TestDocumentLocatorExactRoundTripAndSpace(t *testing.T) {
	for _, test := range []storeio.PageRef{
		{
			Offset: 2 * Quantum, LogicalID: 2, Generation: 1,
			Length: Quantum, Kind: storeio.PageDocument,
		},
		{
			Offset: (1<<43 - 1) * Quantum, LogicalID: 1 << 40,
			Generation: 1<<48 - 1, Length: 16 * Quantum,
			Kind: storeio.PageDocument,
		},
		{
			Offset: 991 * Quantum, LogicalID: 81, Generation: 1 << 37,
			Length: 8 * Quantum, Kind: storeio.PageDocumentGroup,
		},
	} {
		image, ok := EncodeDocumentLocator(nil, test)
		if !ok || len(image) != DocumentLocatorBytes {
			t.Fatalf("encode %+v = (%d,%v)", test, len(image), ok)
		}
		got, ok := DecodeDocumentLocator(image)
		if !ok || got.Offset != test.Offset ||
			got.Generation != test.Generation || got.Length != test.Length ||
			got.Grouped != (test.Kind == storeio.PageDocumentGroup) {
			t.Fatalf("round trip %+v = (%+v,%v)", test, got, ok)
		}
	}

	const rowsPerBlock = 64
	if got := float64(DocumentLocatorBytes) / rowsPerBlock; got != 0.1875 {
		t.Fatalf("block-map bytes/current key = %.4f, want 0.1875", got)
	}
}

func TestDocumentLocatorRejectsExtendedAndInvalidReferences(t *testing.T) {
	valid := storeio.PageRef{
		Offset: 2 * Quantum, LogicalID: 2, Generation: 1,
		Length: Quantum, Kind: storeio.PageDocument,
	}
	tests := []storeio.PageRef{
		{},
		func() storeio.PageRef { ref := valid; ref.Offset = Quantum; return ref }(),
		func() storeio.PageRef { ref := valid; ref.Offset++; return ref }(),
		func() storeio.PageRef { ref := valid; ref.Offset = 1 << 43 * Quantum; return ref }(),
		func() storeio.PageRef { ref := valid; ref.LogicalID = storeio.StateRootLogicalID; return ref }(),
		func() storeio.PageRef { ref := valid; ref.Generation = 0; return ref }(),
		func() storeio.PageRef { ref := valid; ref.Generation = 1 << 48; return ref }(),
		func() storeio.PageRef { ref := valid; ref.Length = 17 * Quantum; return ref }(),
		func() storeio.PageRef { ref := valid; ref.Kind = storeio.PageOverflow; return ref }(),
		func() storeio.PageRef { ref := valid; ref.Flags = 1; return ref }(),
		func() storeio.PageRef { ref := valid; ref.Aux = 1; return ref }(),
		func() storeio.PageRef {
			ref := valid
			ref.Kind, ref.Length = storeio.PageDocumentGroup, 3*Quantum
			return ref
		}(),
	}
	prefix := []byte{1, 2, 3}
	for _, test := range tests {
		dst, ok := EncodeDocumentLocator(bytes.Clone(prefix), test)
		if ok || !bytes.Equal(dst, prefix) {
			t.Fatalf("invalid encode %+v = (%x,%v)", test, dst, ok)
		}
	}

	if _, ok := DecodeDocumentLocator(nil); ok {
		t.Fatal("empty locator decoded")
	}
	zero := make([]byte, DocumentLocatorBytes)
	if _, ok := DecodeDocumentLocator(zero); ok {
		t.Fatal("zero locator decoded")
	}
}

func TestDocumentLocatorZeroAllocation(t *testing.T) {
	ref := storeio.PageRef{
		Offset: 17 * Quantum, LogicalID: 99, Generation: 71,
		Length: 3 * Quantum, Kind: storeio.PageDocument,
	}
	var image [DocumentLocatorBytes]byte
	if allocs := testing.AllocsPerRun(1000, func() {
		dst, ok := EncodeDocumentLocator(image[:0], ref)
		if !ok {
			panic("encode")
		}
		got, ok := DecodeDocumentLocator(dst)
		if !ok || got.Offset != ref.Offset || got.Generation != ref.Generation {
			panic("decode")
		}
	}); allocs != 0 {
		t.Fatalf("locator allocations = %.2f, want 0", allocs)
	}
}

func BenchmarkDocumentLocator(b *testing.B) {
	ref := storeio.PageRef{
		Offset: 17 * Quantum, LogicalID: 99, Generation: 71,
		Length: 3 * Quantum, Kind: storeio.PageDocument,
	}
	var image [DocumentLocatorBytes]byte
	b.ReportAllocs()
	for b.Loop() {
		dst, ok := EncodeDocumentLocator(image[:0], ref)
		if !ok {
			b.Fatal("encode")
		}
		if _, ok := DecodeDocumentLocator(dst); !ok {
			b.Fatal("decode")
		}
	}
}
