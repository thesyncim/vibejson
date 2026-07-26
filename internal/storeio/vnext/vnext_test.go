package vnext

import (
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"testing"
)

var testIdentity = Identity{
	StoreID:    [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
	Generation: 7,
	LogicalID:  19,
}

func TestKeyedFingerprintSipHashVectors(t *testing.T) {
	seed := [16]byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}
	vectors := []uint64{
		0x726fdb47dd0e0e31,
		0x74f839c593dc67fd,
		0x0d6c8009d9a94f5a,
		0x85676696d7fb7e2d,
	}
	var key string
	for length, want := range vectors {
		if got := KeyedFingerprint(seed, key); got != want {
			t.Fatalf("length %d = %016x, want %016x", length, got, want)
		}
		key += string(rune(length))
	}
}

func TestFingerprintPageExactCollisionVerification(t *testing.T) {
	const collision = uint64(0x9a11223344556677)
	entries := []FingerprintEntry{
		{Hash: 3, Location: Location{BlockID: 1, Slot: 2}},
		{Hash: collision, Location: Location{BlockID: 7, Slot: 1}},
		{Hash: collision, Location: Location{BlockID: 9, Slot: 63}},
		{Hash: ^uint64(0) - 1, Location: Location{BlockID: 11, Slot: 0}},
	}
	page, err := EncodeFingerprintPage(make([]byte, FingerprintPageSize), testIdentity, entries)
	if err != nil {
		t.Fatal(err)
	}
	view, err := OpenFingerprintPage(page)
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	location, found := view.Lookup(collision, func(candidate Location) bool {
		checked++
		return candidate.BlockID == 9
	})
	if !found || location != (Location{BlockID: 9, Slot: 63}) || checked != 2 {
		t.Fatalf("collision lookup = (%+v,%v), checked %d", location, found, checked)
	}
	if _, found := view.Lookup(collision, nil); found {
		t.Fatal("fingerprint-only lookup returned an unverified hit")
	}
	checked = 0
	if _, found := view.Lookup(4, func(Location) bool {
		checked++
		return true
	}); found || checked != 0 {
		t.Fatalf("miss = %v after %d verifications", found, checked)
	}
}

func TestFingerprintPageCapacitySpaceAndCorruption(t *testing.T) {
	entries := make([]FingerprintEntry, FingerprintPageCapacity)
	for i := range entries {
		entries[i] = FingerprintEntry{
			Hash: uint64(i) << 32,
			Location: Location{
				BlockID: uint32(i + 1),
				Slot:    uint8(i % RawBlockSlotCount),
			},
		}
	}
	page, err := EncodeFingerprintPage(make([]byte, FingerprintPageSize), testIdentity, entries)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenFingerprintPage(page); err != nil {
		t.Fatal(err)
	}
	seventyPercent := FingerprintPageCapacity * 7 / 10
	if bytesPerKey := float64(FingerprintPageSize) / float64(seventyPercent); bytesPerKey > 24 {
		t.Fatalf("70%% occupancy = %.2f bytes/key, want <= 24", bytesPerKey)
	}
	tooMany := append(entries, FingerprintEntry{
		Hash: ^uint64(0), Location: Location{BlockID: 1 << 20},
	})
	if _, err := EncodeFingerprintPage(
		make([]byte, FingerprintPageSize),
		testIdentity,
		tooMany,
	); !errors.Is(err, ErrInvalidFrame) {
		t.Fatalf("over-capacity encode = %v", err)
	}
	for i := range page {
		corruptPage := slices.Clone(page)
		corruptPage[i] ^= 1
		if _, err := OpenFingerprintPage(corruptPage); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("byte %d corruption = %v", i, err)
		}
	}
}

func TestFingerprintPageFullCollisionRunIsExplicitAndExact(t *testing.T) {
	const hash = uint64(0xfeedfacecafebeef)
	entries := make([]FingerprintEntry, FingerprintPageCapacity)
	for i := range entries {
		entries[i] = FingerprintEntry{
			Hash: hash,
			Location: Location{
				BlockID: uint32(i + 1),
				Slot:    uint8(i % RawBlockSlotCount),
			},
		}
	}
	page, err := EncodeFingerprintPage(make([]byte, FingerprintPageSize), testIdentity, entries)
	if err != nil {
		t.Fatal(err)
	}
	view, err := OpenFingerprintPage(page)
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	location, ok := view.Lookup(hash, func(candidate Location) bool {
		checked++
		return candidate == entries[len(entries)-1].Location
	})
	if !ok || location != entries[len(entries)-1].Location ||
		checked != FingerprintPageCapacity {
		t.Fatalf("full collision lookup = (%+v,%v), checked %d", location, ok, checked)
	}
}

func TestFingerprintPageRejectsResealedStructureCorruption(t *testing.T) {
	entries := []FingerprintEntry{
		{Hash: 1, Location: Location{BlockID: 1}},
		{Hash: 2, Location: Location{BlockID: 2}},
	}
	page, err := EncodeFingerprintPage(make([]byte, FingerprintPageSize), testIdentity, entries)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		mutate func([]byte)
	}{
		{"bucket order", func(p []byte) {
			at := FrameHeaderSize + FingerprintPayloadHeaderSize + 2
			binary.LittleEndian.PutUint16(p[at:at+2], 0)
		}},
		{"location", func(p []byte) {
			at := FrameHeaderSize + FingerprintPayloadHeaderSize +
				FingerprintBucketBytes + 7
			binary.LittleEndian.PutUint32(p[at:at+4], 0)
		}},
		{"range", func(p []byte) {
			binary.LittleEndian.PutUint64(p[FrameHeaderSize+12:], 99)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			corruptPage := slices.Clone(page)
			test.mutate(corruptPage)
			if err := sealFrame(corruptPage); err != nil {
				t.Fatal(err)
			}
			if _, err := OpenFingerprintPage(corruptPage); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("OpenFingerprintPage = %v", err)
			}
		})
	}
}

func TestRawBlockSparseRoundTripAndExactLookup(t *testing.T) {
	rows := []RawRow{
		{Slot: 0, Key: []byte("alpha"), JSON: []byte(`{"v":1}`)},
		{Slot: 7, Key: []byte("beta"), JSON: []byte(`{"v":2}`)},
		{Slot: 63, Key: []byte("omega"), JSON: []byte(`{"v":3}`)},
	}
	used, err := RawBlockEncodedBytes(rows)
	if err != nil {
		t.Fatal(err)
	}
	span, err := (GeometryPolicy{TargetFillPermille: 1000}).SelectSpan(used)
	if err != nil || span != Quantum {
		t.Fatalf("SelectSpan = (%d,%v), want (%d,nil)", span, err, Quantum)
	}
	page, err := EncodeRawBlock(make([]byte, span), testIdentity, 91, rows)
	if err != nil {
		t.Fatal(err)
	}
	view, err := OpenRawBlock(page)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		key, json, ok := view.Lookup(row.Slot)
		if !ok || !slices.Equal(key, row.Key) || !slices.Equal(json, row.JSON) {
			t.Fatalf("Lookup(%d) = (%q,%q,%v)", row.Slot, key, json, ok)
		}
		if exact, ok := view.LookupKey(row.Slot, string(row.Key)); !ok ||
			!slices.Equal(exact, row.JSON) {
			t.Fatalf("LookupKey(%d) = (%q,%v)", row.Slot, exact, ok)
		}
	}
	if _, _, ok := view.Lookup(8); ok {
		t.Fatal("vacant slot returned a row")
	}
	if _, ok := view.LookupKey(7, "foreign"); ok {
		t.Fatal("foreign key passed exact verification")
	}
}

func TestRawBlockSpaceAndArbitraryQuantumGeometry(t *testing.T) {
	rows := make([]RawRow, RawBlockSlotCount)
	for i := range rows {
		rows[i] = RawRow{
			Slot: uint8(i),
			Key:  []byte(fmt.Sprintf("key-%09d", i)),
			JSON: []byte(fmt.Sprintf(
				`{"id":%d,"payload":"%0256d"}`,
				i,
				i,
			)),
		}
	}
	used, err := RawBlockEncodedBytes(rows)
	if err != nil {
		t.Fatal(err)
	}
	span, err := (GeometryPolicy{TargetFillPermille: 1000}).SelectSpan(used)
	if err != nil {
		t.Fatal(err)
	}
	payloadBytes := 0
	for _, row := range rows {
		payloadBytes += len(row.Key) + len(row.JSON)
	}
	if ratio := float64(span) / float64(payloadBytes); ratio > 1.20 {
		t.Fatalf("raw block physical ratio = %.3fx, want <= 1.20x", ratio)
	}
	if span%Quantum != 0 || span&(span-1) == 0 {
		t.Fatalf("span = %d, want a non-power-of-two 4 KiB multiple", span)
	}

	for _, encoded := range []int{8<<10 + 1, 12<<10 - 1, 12 << 10, 12<<10 + 1} {
		got, err := (GeometryPolicy{TargetFillPermille: 1000}).SelectSpan(encoded)
		if err != nil {
			t.Fatal(err)
		}
		want := ((encoded + Quantum - 1) / Quantum) * Quantum
		if got != want {
			t.Fatalf("SelectSpan(%d) = %d, want %d", encoded, got, want)
		}
	}
}

func TestRawBlockCorruptionAndSteadyAllocations(t *testing.T) {
	rows := []RawRow{{Slot: 5, Key: []byte("key"), JSON: []byte(`{"ok":true}`)}}
	page := make([]byte, Quantum)
	if _, err := EncodeRawBlock(page, testIdentity, 7, rows); err != nil {
		t.Fatal(err)
	}
	view, err := OpenRawBlock(page)
	if err != nil {
		t.Fatal(err)
	}
	if allocations := testing.AllocsPerRun(1000, func() {
		if _, err := EncodeRawBlock(page, testIdentity, 7, rows); err != nil {
			panic(err)
		}
		opened, err := OpenRawBlock(page)
		if err != nil {
			panic(err)
		}
		if _, ok := opened.LookupKey(5, "key"); !ok {
			panic("missing")
		}
	}); allocations != 0 {
		t.Fatalf("encode/open/lookup allocations = %g, want 0", allocations)
	}
	if _, ok := view.LookupKey(5, "key"); !ok {
		t.Fatal("baseline lookup failed")
	}
	for i := range page {
		corruptPage := slices.Clone(page)
		corruptPage[i] ^= 1
		if _, err := OpenRawBlock(corruptPage); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("byte %d corruption = %v", i, err)
		}
	}
}

func BenchmarkFingerprintLookupResident(b *testing.B) {
	entries := make([]FingerprintEntry, FingerprintPageCapacity)
	for i := range entries {
		entries[i] = FingerprintEntry{
			Hash:     uint64(i) << 32,
			Location: Location{BlockID: uint32(i + 1), Slot: uint8(i % 64)},
		}
	}
	page, _ := EncodeFingerprintPage(make([]byte, FingerprintPageSize), testIdentity, entries)
	view, _ := OpenFingerprintPage(page)
	hash := entries[len(entries)/2].Hash
	want := entries[len(entries)/2].Location
	b.ReportAllocs()
	b.SetBytes(FingerprintEntrySize)
	for b.Loop() {
		if got, ok := view.Lookup(hash, func(candidate Location) bool {
			return candidate == want
		}); !ok || got != want {
			b.Fatal("lookup")
		}
	}
}

func BenchmarkRawBlockLookupExact(b *testing.B) {
	rows := make([]RawRow, 64)
	for i := range rows {
		rows[i] = RawRow{
			Slot: uint8(i),
			Key:  []byte(fmt.Sprintf("key-%09d", i)),
			JSON: []byte(`{"payload":"abcdefghijklmnopqrstuvwxyz"}`),
		}
	}
	page, _ := EncodeRawBlock(make([]byte, Quantum), testIdentity, 7, rows)
	view, _ := OpenRawBlock(page)
	b.ReportAllocs()
	b.SetBytes(int64(len(rows[37].JSON)))
	for b.Loop() {
		if _, ok := view.LookupKey(37, "key-000000037"); !ok {
			b.Fatal("lookup")
		}
	}
}
