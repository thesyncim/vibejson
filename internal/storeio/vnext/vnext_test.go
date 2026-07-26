package vnext

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
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

func TestRawBlockEveryQuantumSpan(t *testing.T) {
	for span := Quantum; span <= MaxSpan; span += Quantum {
		jsonBytes := span - RawBlockFixedBytes - 1
		rows := []RawRow{{
			Slot: 63, Key: []byte("k"), JSON: bytes.Repeat([]byte{'x'}, jsonBytes),
		}}
		page, err := EncodeRawBlock(make([]byte, span), testIdentity, uint32(span), rows)
		if err != nil {
			t.Fatalf("EncodeRawBlock(%d) = %v", span, err)
		}
		view, err := OpenRawBlock(page)
		if err != nil {
			t.Fatalf("OpenRawBlock(%d) = %v", span, err)
		}
		if got, ok := view.LookupKey(63, "k"); !ok || len(got) != jsonBytes {
			t.Fatalf("LookupKey(%d) = %d,%v, want %d,true", span, len(got), ok, jsonBytes)
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

func TestPackedBlockSpaceExactnessAndAllocations(t *testing.T) {
	rows := repetitivePackedRows()
	used, err := PackedBlockEncodedBytes(rows)
	if err != nil {
		t.Fatal(err)
	}
	span, err := (GeometryPolicy{TargetFillPermille: 1000}).SelectSpan(used)
	if err != nil {
		t.Fatal(err)
	}
	logical := 0
	for _, row := range rows {
		logical += len(row.Key) + len(row.JSON)
	}
	if ratio := float64(span) / float64(logical); ratio > 0.75 {
		t.Fatalf("packed physical ratio = %.3fx, want <= 0.75x", ratio)
	}
	t.Logf("packed block physical ratio: %d/%d = %.3fx",
		span, logical, float64(span)/float64(logical))
	page, err := EncodePackedBlock(make([]byte, span), testIdentity, 77, rows)
	if err != nil {
		t.Fatal(err)
	}
	view, err := OpenPackedBlock(page)
	if err != nil {
		t.Fatal(err)
	}
	dst := make([]byte, 0, len(rows[37].JSON))
	got, ok := view.AppendJSON(dst, 37, string(rows[37].Key))
	if !ok || !slices.Equal(got, rows[37].JSON) {
		t.Fatalf("AppendJSON = (%q,%v)", got, ok)
	}
	if got, ok := view.AppendJSON(dst[:0], 37, "foreign"); ok || len(got) != 0 {
		t.Fatalf("foreign AppendJSON = (%q,%v)", got, ok)
	}
	if allocations := testing.AllocsPerRun(1000, func() {
		got, ok := view.AppendJSON(dst[:0], 37, "key-000000037")
		if !ok || len(got) != len(rows[37].JSON) {
			panic("lookup")
		}
	}); allocations != 0 {
		t.Fatalf("packed AppendJSON allocations = %g, want 0", allocations)
	}
}

func TestCanonicalBlockPlanRejectsWeakPacking(t *testing.T) {
	repetitive := repetitivePackedRows()
	plan, err := PlanCanonicalBlock(
		repetitive,
		GeometryPolicy{TargetFillPermille: 1000},
		0,
	)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Encoding != BlockEncodingPacked || plan.PackedSpan != Quantum ||
		plan.RawSpan <= plan.PackedSpan {
		t.Fatalf("repetitive plan = %+v", plan)
	}

	incompressible := make([]RawRow, RawBlockSlotCount)
	for i := range incompressible {
		json := bytes.Repeat([]byte{byte(i + 1)}, 257)
		json[0] = byte(i + 1)
		json[len(json)-1] = byte(255 - i)
		incompressible[i] = RawRow{
			Slot: uint8(i),
			Key:  []byte(fmt.Sprintf("key-%09d", i)),
			JSON: json,
		}
	}
	plan, err = PlanCanonicalBlock(
		incompressible,
		GeometryPolicy{TargetFillPermille: 1000},
		0,
	)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Encoding != BlockEncodingRaw || plan.Span != plan.RawSpan ||
		plan.PackedSpan != plan.RawSpan {
		t.Fatalf("incompressible plan = %+v", plan)
	}
}

func TestPackedFourByteDirectoryAvoidsExtentGrowth(t *testing.T) {
	rows := make([]RawRow, RawBlockSlotCount)
	json := bytes.Repeat([]byte{'x'}, 3300)
	for i := range rows {
		rows[i] = RawRow{
			Slot: uint8(i),
			Key:  []byte(fmt.Sprintf("k%04d", i)),
			JSON: json,
		}
	}
	used, err := PackedBlockEncodedBytes(rows)
	if err != nil {
		t.Fatal(err)
	}
	span, err := (GeometryPolicy{TargetFillPermille: 1000}).SelectSpan(used)
	if err != nil {
		t.Fatal(err)
	}
	if PackedBlockSlotRecordSize != 4 || span != Quantum || used+4*RawBlockSlotCount <= Quantum {
		t.Fatalf("packed directory: record=%d used=%d span=%d",
			PackedBlockSlotRecordSize, used, span)
	}
	page, err := EncodePackedBlock(make([]byte, span), testIdentity, 78, rows)
	if err != nil {
		t.Fatal(err)
	}
	view, err := OpenPackedBlock(page)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := view.AppendJSON(nil, 63, "k0063"); !ok || !slices.Equal(got, json) {
		t.Fatalf("last row = (%d,%v)", len(got), ok)
	}
}

func TestPackedBlockCorruption(t *testing.T) {
	rows := repetitivePackedRows()
	used, _ := PackedBlockEncodedBytes(rows)
	span, _ := (GeometryPolicy{TargetFillPermille: 1000}).SelectSpan(used)
	page, err := EncodePackedBlock(make([]byte, span), testIdentity, 77, rows)
	if err != nil {
		t.Fatal(err)
	}
	for i := range page {
		corruptPage := slices.Clone(page)
		corruptPage[i] ^= 1
		if _, err := OpenPackedBlock(corruptPage); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("byte %d corruption = %v", i, err)
		}
	}
	corruptPage := slices.Clone(page)
	first := FrameHeaderSize + PackedBlockPayloadHeaderSize
	record := binary.LittleEndian.Uint32(corruptPage[first:])
	record &^= uint32(0xffff) << 16
	record |= uint32(0xffff) << 16
	binary.LittleEndian.PutUint32(corruptPage[first:], record)
	if err := sealFrame(corruptPage); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenPackedBlock(corruptPage); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("resealed packed corruption = %v", err)
	}

	corruptPage = slices.Clone(page)
	header, ok := decodeFrameHeader(corruptPage)
	if !ok {
		t.Fatal("decode frame")
	}
	padding := FrameHeaderSize + int(header.payloadLength)
	corruptPage[padding] = 1
	trailer := len(corruptPage) - FrameTrailerSize
	checksum := crc32.Checksum(corruptPage[:trailer], frameCRC)
	binary.LittleEndian.PutUint32(corruptPage[trailer:], checksum)
	binary.LittleEndian.PutUint32(corruptPage[trailer+4:], ^checksum)
	if _, err := OpenPackedBlock(corruptPage); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("resealed non-canonical padding = %v", err)
	}
}

func TestGeometryTraceExactSpansAndDeleteMaintenance(t *testing.T) {
	operations := make([]TraceOperation, 0, 20_000)
	for key := range uint64(10_000) {
		operations = append(operations, TraceOperation{
			Kind: TracePut, Key: key, RecordBytes: 299 + int(key%5)*173,
		})
	}
	for key := range uint64(9_000) {
		operations = append(operations, TraceOperation{Kind: TraceDelete, Key: key})
	}
	metrics, err := SimulateTrace(operations, TraceConfig{
		HotMaxSpan: 12 << 10, ColdMaxSpan: 24 << 10, Maintain: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if metrics.Documents != 1_000 || metrics.ExactSpanBytes > metrics.FreshRebuildBytes*110/100 {
		t.Fatalf("delete maintenance metrics = %+v", metrics)
	}
	if metrics.ExactSpanBytes >= metrics.PowerOfTwoSpanBytes ||
		metrics.ExactDeviceBytes >= metrics.PowerOfTwoDeviceBytes {
		t.Fatalf("exact spans did not save space and writes: %+v", metrics)
	}
	if metrics.Relocations*100 > metrics.Operations*10 {
		t.Fatalf("relocations = %d across %d operations, want <= 10%%",
			metrics.Relocations, metrics.Operations)
	}
	t.Logf("90%% delete trace: exact=%d power2=%d fresh=%d writes=%d/%d relocations=%d",
		metrics.ExactSpanBytes, metrics.PowerOfTwoSpanBytes, metrics.FreshRebuildBytes,
		metrics.ExactDeviceBytes, metrics.PowerOfTwoDeviceBytes, metrics.Relocations)
}

func TestGeometryTraceReplacementUsesSubPowerOfTwoSpans(t *testing.T) {
	operations := make([]TraceOperation, 0, 4_000)
	for key := range uint64(2_000) {
		operations = append(operations, TraceOperation{
			Kind: TracePut, Key: key, RecordBytes: 700 + int(key%7)*113,
		})
	}
	for key := range uint64(2_000) {
		operations = append(operations, TraceOperation{
			Kind: TracePut, Key: key, RecordBytes: 900 + int(key%11)*97,
		})
	}
	metrics, err := SimulateTrace(operations, TraceConfig{
		HotMaxSpan: 12 << 10, ColdMaxSpan: 24 << 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if metrics.ExactDeviceBytes*100 > metrics.PowerOfTwoDeviceBytes*88 {
		t.Fatalf("exact-span write bytes = %d, power-of-two = %d, want >=12%% saving",
			metrics.ExactDeviceBytes, metrics.PowerOfTwoDeviceBytes)
	}
	t.Logf("replacement trace write bytes: exact=%d power2=%d",
		metrics.ExactDeviceBytes, metrics.PowerOfTwoDeviceBytes)
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

func BenchmarkPackedBlockAppendExact(b *testing.B) {
	rows := repetitivePackedRows()
	used, _ := PackedBlockEncodedBytes(rows)
	span, _ := (GeometryPolicy{TargetFillPermille: 1000}).SelectSpan(used)
	page, _ := EncodePackedBlock(make([]byte, span), testIdentity, 77, rows)
	view, _ := OpenPackedBlock(page)
	dst := make([]byte, 0, len(rows[37].JSON))
	b.ReportAllocs()
	b.SetBytes(int64(len(rows[37].JSON)))
	for b.Loop() {
		got, ok := view.AppendJSON(dst[:0], 37, "key-000000037")
		if !ok || len(got) != len(rows[37].JSON) {
			b.Fatal("lookup")
		}
	}
}

func repetitivePackedRows() []RawRow {
	rows := make([]RawRow, RawBlockSlotCount)
	padding := string(bytes.Repeat([]byte{'x'}, 256))
	for i := range rows {
		rows[i] = RawRow{
			Slot: uint8(i),
			Key:  []byte(fmt.Sprintf("key-%09d", i)),
			JSON: []byte(fmt.Sprintf(
				`{"id":"%09d","payload":"%s","active":true}`,
				i,
				padding,
			)),
		}
	}
	return rows
}
