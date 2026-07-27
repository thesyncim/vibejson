package storeio

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
	"math"
	"testing"
)

func tabletLocalIdentityLabAssignments(
	count int,
) []TabletLocalIdentityLabAssignment {
	assignments := make([]TabletLocalIdentityLabAssignment, count)
	for id := range assignments {
		assignments[id] = TabletLocalIdentityLabAssignment{
			LocalID: uint16(id),
			Location: TabletLocalIdentityLabLocation{
				AnchorPageID: uint8(id >> 8),
				RowSlot:      uint8(id),
			},
		}
	}
	return assignments
}

func tabletLocalIdentityLabTestView(
	t testing.TB, count int,
) TabletLocalIdentityLabView {
	t.Helper()
	image, descriptor, err := EncodeTabletLocalIdentityLab(
		make([]byte, TabletLocalIdentityLabImageBytes),
		42, 7, 1, tabletLocalIdentityLabAssignments(count),
	)
	if err != nil {
		t.Fatal(err)
	}
	view, err := OpenTabletLocalIdentityLab(image, descriptor, nil)
	if err != nil {
		t.Fatal(err)
	}
	return view
}

func TestTabletLocalIdentityLabBucketRoundTripAndCapacity(t *testing.T) {
	tests := []struct {
		tablet uint32
		local  uint16
	}{
		{0, 0},
		{42, 17},
		{TabletLocalIdentityLabTabletCount - 1,
			TabletLocalIdentityLabLocalCount - 1},
	}
	for _, test := range tests {
		bucket, ok := MakeTabletLocalIdentityLabBucket(
			test.tablet, uint32(test.local),
		)
		if !ok {
			t.Fatalf("Make(%d,%d) failed", test.tablet, test.local)
		}
		tablet, local, ok := SplitTabletLocalIdentityLabBucket(bucket)
		if !ok || tablet != test.tablet || local != test.local {
			t.Fatalf(
				"Split(%d) = %d,%d,%v", bucket, tablet, local, ok,
			)
		}
	}
	maxBucket, ok := MakeTabletLocalIdentityLabBucket(
		TabletLocalIdentityLabTabletCount-1,
		TabletLocalIdentityLabLocalCount-1,
	)
	if !ok || maxBucket != TabletLocalIdentityLabBucketCount-1 {
		t.Fatalf("max bucket = %d,%v", maxBucket, ok)
	}
	if _, ok := MakeTabletLocalIdentityLabBucket(
		TabletLocalIdentityLabTabletCount, 0,
	); ok {
		t.Fatal("overflow tablet accepted")
	}
	if _, ok := MakeTabletLocalIdentityLabBucket(
		0, TabletLocalIdentityLabLocalCount,
	); ok {
		t.Fatal("overflow local ID accepted")
	}
	if _, _, ok := SplitTabletLocalIdentityLabBucket(
		TabletLocalIdentityLabBucketCount,
	); ok {
		t.Fatal("overflow bucket accepted")
	}

	const documents = uint64(100_000_000_000)
	for _, rows := range []uint64{187, 230} {
		capacity, ok := TabletLocalIdentityLabDocumentCapacity(rows)
		if !ok || capacity < documents {
			t.Fatalf("capacity(%d) = %d,%v", rows, capacity, ok)
		}
		leaves := (documents + rows - 1) / rows
		tablets := (leaves + TabletLocalIdentityLabLocalCount - 1) /
			TabletLocalIdentityLabLocalCount
		if tablets > TabletLocalIdentityLabTabletCount {
			t.Fatalf(
				"100B at %d rows needs %d tablets", rows, tablets,
			)
		}
	}
	if _, ok := TabletLocalIdentityLabDocumentCapacity(0); ok {
		t.Fatal("zero rows accepted")
	}
	if _, ok := TabletLocalIdentityLabDocumentCapacity(math.MaxUint64); ok {
		t.Fatal("overflow capacity accepted")
	}
	if got, ok := TabletLocalIdentityLabEnvelopeBytes(0); !ok ||
		got != TabletLocalIdentityLabImageBytes+
			TabletLocalIdentityLabDescriptorBytes {
		t.Fatalf("empty envelope = %d,%v", got, ok)
	}
	if got, ok := TabletLocalIdentityLabEnvelopeBytes(
		TabletLocalIdentityLabLocalCount,
	); !ok || got != TabletLocalIdentityLabMaxEnvelopeBytes ||
		TabletLocalIdentityLabMaxRetirementBytes != 40960 {
		t.Fatalf(
			"worst envelope = %d,%v retirement=%d",
			got, ok, TabletLocalIdentityLabMaxRetirementBytes,
		)
	}
	if _, ok := TabletLocalIdentityLabEnvelopeBytes(
		TabletLocalIdentityLabLocalCount + 1,
	); ok {
		t.Fatal("overflow retirement envelope accepted")
	}
}

func TestTabletLocalIdentityLabResolveAndWalk(t *testing.T) {
	view := tabletLocalIdentityLabTestView(t, 3072)
	for _, id := range []uint16{0, 1, 255, 256, 2047, 3071} {
		location, ok := view.Resolve(id)
		if !ok || location != (TabletLocalIdentityLabLocation{
			AnchorPageID: uint8(id >> 8),
			RowSlot:      uint8(id),
		}) {
			t.Fatalf("Resolve(%d) = %+v,%v", id, location, ok)
		}
	}
	if _, ok := view.Resolve(3072); ok {
		t.Fatal("empty ID resolved")
	}
	cursor := view.Cursor()
	count := 0
	for {
		id, location, ok := cursor.Next()
		if !ok {
			break
		}
		if int(id) != count ||
			location.AnchorPageID != uint8(id>>8) ||
			location.RowSlot != uint8(id) {
			t.Fatalf("walk %d = %d,%+v", count, id, location)
		}
		count++
	}
	if count != 3072 {
		t.Fatalf("walked %d live IDs", count)
	}
}

func TestTabletLocalIdentityLabSnapshotSafeRetireAndReuse(t *testing.T) {
	base := tabletLocalIdentityLabTestView(t, 32)
	image8, descriptor8, retirements8, err := RewriteTabletLocalIdentityLab(
		make([]byte, TabletLocalIdentityLabImageBytes),
		make([]TabletLocalIdentityLabRetirement, 0, 4),
		base, 8, 7,
		[]TabletLocalIdentityLabEdit{{
			LocalID: 9, Operation: TabletLocalIdentityLabRetire,
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(retirements8) != 1 ||
		retirements8[0] != (TabletLocalIdentityLabRetirement{
			LocalID: 9, RetiredGeneration: 7,
		}) {
		t.Fatalf("retirements = %+v", retirements8)
	}
	view8, err := OpenTabletLocalIdentityLab(
		image8, descriptor8, retirements8,
	)
	if err != nil {
		t.Fatal(err)
	}
	state, _, retiredGeneration, ok := view8.Entry(9)
	if !ok || state != TabletLocalIdentityLabRetired ||
		retiredGeneration != 7 {
		t.Fatalf("retired entry = %d,%d,%v", state, retiredGeneration, ok)
	}
	if _, ok := view8.Resolve(9); ok {
		t.Fatal("retired ID resolved")
	}

	_, _, _, err = RewriteTabletLocalIdentityLab(
		make([]byte, TabletLocalIdentityLabImageBytes),
		make([]TabletLocalIdentityLabRetirement, 0, 4),
		view8, 9, 7,
		[]TabletLocalIdentityLabEdit{{
			LocalID: 9, Operation: TabletLocalIdentityLabAssign,
			Location: TabletLocalIdentityLabLocation{
				AnchorPageID: 3, RowSlot: 9,
			},
		}},
	)
	if !errors.Is(err, ErrTabletLocalIdentityLabReuse) {
		t.Fatalf("premature reuse = %v", err)
	}

	image10, descriptor10, retirements10, err :=
		RewriteTabletLocalIdentityLab(
			make([]byte, TabletLocalIdentityLabImageBytes),
			make([]TabletLocalIdentityLabRetirement, 0, 4),
			view8, 10, 8,
			[]TabletLocalIdentityLabEdit{{
				LocalID: 9, Operation: TabletLocalIdentityLabAssign,
				Location: TabletLocalIdentityLabLocation{
					AnchorPageID: 3, RowSlot: 9,
				},
			}},
		)
	if err != nil {
		t.Fatal(err)
	}
	if len(retirements10) != 0 {
		t.Fatalf("eligible retirement retained: %+v", retirements10)
	}
	view10, err := OpenTabletLocalIdentityLab(
		image10, descriptor10, retirements10,
	)
	if err != nil {
		t.Fatal(err)
	}
	location, ok := view10.Resolve(9)
	if !ok || location != (TabletLocalIdentityLabLocation{
		AnchorPageID: 3, RowSlot: 9,
	}) {
		t.Fatalf("reused Resolve = %+v,%v", location, ok)
	}

	// The old snapshot retains the old COW image and therefore still sees the
	// pre-retirement identity even after the new root recycles its number.
	oldLocation, ok := base.Resolve(9)
	if !ok || oldLocation == location {
		t.Fatalf("old snapshot mapping = %+v,%v", oldLocation, ok)
	}
}

func TestTabletLocalIdentityLabRetirementGenerationIsNotTruncated(t *testing.T) {
	const oldGeneration = uint64(1)<<48 + 12345
	image, descriptor, err := EncodeTabletLocalIdentityLab(
		make([]byte, TabletLocalIdentityLabImageBytes),
		7, oldGeneration, 1, tabletLocalIdentityLabAssignments(2),
	)
	if err != nil {
		t.Fatal(err)
	}
	base, err := OpenTabletLocalIdentityLab(image, descriptor, nil)
	if err != nil {
		t.Fatal(err)
	}
	retiredImage, retiredDescriptor, retirements, err :=
		RewriteTabletLocalIdentityLab(
			make([]byte, TabletLocalIdentityLabImageBytes),
			make([]TabletLocalIdentityLabRetirement, 0, 1),
			base, oldGeneration+1, oldGeneration,
			[]TabletLocalIdentityLabEdit{{
				LocalID: 1, Operation: TabletLocalIdentityLabRetire,
			}},
		)
	if err != nil {
		t.Fatal(err)
	}
	if len(retirements) != 1 ||
		retirements[0].RetiredGeneration != oldGeneration {
		t.Fatalf("exact retirement = %+v", retirements)
	}
	retiredView, err := OpenTabletLocalIdentityLab(
		retiredImage, retiredDescriptor, retirements,
	)
	if err != nil {
		t.Fatal(err)
	}
	reusedImage, reusedDescriptor, reusedRetirements, err :=
		RewriteTabletLocalIdentityLab(
			make([]byte, TabletLocalIdentityLabImageBytes),
			make([]TabletLocalIdentityLabRetirement, 0, 1),
			retiredView, oldGeneration+2, oldGeneration+1,
			[]TabletLocalIdentityLabEdit{{
				LocalID: 1, Operation: TabletLocalIdentityLabAssign,
				Location: TabletLocalIdentityLabLocation{
					AnchorPageID: 15, RowSlot: 255,
				},
			}},
		)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenTabletLocalIdentityLab(
		reusedImage, reusedDescriptor, reusedRetirements,
	); err != nil {
		t.Fatal(err)
	}
}

func TestTabletLocalIdentityLabBatchMoveRetireAndImmediateReclaim(t *testing.T) {
	base := tabletLocalIdentityLabTestView(t, 128)
	image, descriptor, retirements, err := RewriteTabletLocalIdentityLab(
		make([]byte, TabletLocalIdentityLabImageBytes),
		make([]TabletLocalIdentityLabRetirement, 0, 4),
		base, 8, 7,
		[]TabletLocalIdentityLabEdit{
			{
				LocalID: 3, Operation: TabletLocalIdentityLabMove,
				Location: TabletLocalIdentityLabLocation{
					AnchorPageID: 2, RowSlot: 200,
				},
			},
			{LocalID: 7, Operation: TabletLocalIdentityLabRetire},
			{
				LocalID: 200, Operation: TabletLocalIdentityLabAssign,
				Location: TabletLocalIdentityLabLocation{
					AnchorPageID: 3, RowSlot: 201,
				},
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(retirements) != 1 ||
		retirements[0].RetiredGeneration != 7 {
		t.Fatalf("retirements = %+v", retirements)
	}
	view, err := OpenTabletLocalIdentityLab(image, descriptor, retirements)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := view.Resolve(3); !ok ||
		got != (TabletLocalIdentityLabLocation{
			AnchorPageID: 2, RowSlot: 200,
		}) {
		t.Fatalf("moved = %+v,%v", got, ok)
	}
	if _, ok := view.Resolve(7); ok {
		t.Fatal("retired identity resolved")
	}
	if got, ok := view.Resolve(200); !ok ||
		got != (TabletLocalIdentityLabLocation{
			AnchorPageID: 3, RowSlot: 201,
		}) {
		t.Fatalf("assigned = %+v,%v", got, ok)
	}

	// With no reader at generation 8 or below, retiring under root 8 does not
	// need a queue entry: the old identity is unreachable immediately.
	image9, descriptor9, retirements9, err :=
		RewriteTabletLocalIdentityLab(
			make([]byte, TabletLocalIdentityLabImageBytes),
			make([]TabletLocalIdentityLabRetirement, 0, 4),
			view, 9, 9,
			[]TabletLocalIdentityLabEdit{{
				LocalID: 11, Operation: TabletLocalIdentityLabRetire,
			}},
		)
	if err != nil {
		t.Fatal(err)
	}
	if len(retirements9) != 0 {
		t.Fatalf("immediately safe retirement = %+v", retirements9)
	}
	view9, err := OpenTabletLocalIdentityLab(
		image9, descriptor9, retirements9,
	)
	if err != nil {
		t.Fatal(err)
	}
	state, _, _, ok := view9.Entry(11)
	if !ok || state != TabletLocalIdentityLabEmpty {
		t.Fatalf("reclaimed state = %d,%v", state, ok)
	}
}

func TestTabletLocalIdentityLabRejectsCorruptionAndNonCanonicalInput(t *testing.T) {
	view := tabletLocalIdentityLabTestView(t, 1024)
	corrupt := append([]byte(nil), view.PersistentBytes()...)
	corrupt[777] ^= 0x40
	if _, err := OpenTabletLocalIdentityLab(
		corrupt, view.Descriptor(), nil,
	); !errors.Is(err, ErrTabletLocalIdentityLabCorrupt) {
		t.Fatalf("corruption = %v", err)
	}

	descriptor := view.Descriptor()
	descriptor.TabletID++
	if _, err := OpenTabletLocalIdentityLab(
		view.PersistentBytes(), descriptor, nil,
	); !errors.Is(err, ErrTabletLocalIdentityLabCorrupt) {
		t.Fatalf("descriptor graft = %v", err)
	}

	assignments := tabletLocalIdentityLabAssignments(2)
	assignments[1].Location = assignments[0].Location
	if _, _, err := EncodeTabletLocalIdentityLab(
		make([]byte, TabletLocalIdentityLabImageBytes),
		1, 1, 1, assignments,
	); err == nil {
		t.Fatal("duplicate live location accepted")
	}

	base := tabletLocalIdentityLabTestView(t, 2)
	if _, _, _, err := RewriteTabletLocalIdentityLab(
		make([]byte, TabletLocalIdentityLabImageBytes),
		make([]TabletLocalIdentityLabRetirement, 0, 1),
		base, 8, 1,
		[]TabletLocalIdentityLabEdit{{
			LocalID: 1, Operation: TabletLocalIdentityLabMove,
			Location: TabletLocalIdentityLabLocation{},
		}},
	); err == nil {
		t.Fatal("post-edit duplicate location accepted")
	}
}

func TestTabletLocalIdentityLabRetirementQueueIsChecksummed(t *testing.T) {
	base := tabletLocalIdentityLabTestView(t, 16)
	image, descriptor, retirements, err := RewriteTabletLocalIdentityLab(
		make([]byte, TabletLocalIdentityLabImageBytes),
		make([]TabletLocalIdentityLabRetirement, 0, 2),
		base, 8, 7,
		[]TabletLocalIdentityLabEdit{{
			LocalID: 4, Operation: TabletLocalIdentityLabRetire,
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	tampered := append([]TabletLocalIdentityLabRetirement(nil), retirements...)
	tampered[0].RetiredGeneration--
	if _, err := OpenTabletLocalIdentityLab(
		image, descriptor, tampered,
	); !errors.Is(err, ErrTabletLocalIdentityLabCorrupt) {
		t.Fatalf("retirement corruption = %v", err)
	}
	if _, err := OpenTabletLocalIdentityLab(
		image, descriptor, nil,
	); !errors.Is(err, ErrTabletLocalIdentityLabCorrupt) {
		t.Fatalf("missing retirement queue = %v", err)
	}
}

func TestTabletLocalIdentityLabRewriteScratchBound(t *testing.T) {
	base := tabletLocalIdentityLabTestView(t, 4)
	_, _, _, err := RewriteTabletLocalIdentityLab(
		make([]byte, TabletLocalIdentityLabImageBytes),
		nil, base, 8, 7,
		[]TabletLocalIdentityLabEdit{{
			LocalID: 2, Operation: TabletLocalIdentityLabRetire,
		}},
	)
	if !errors.Is(err, ErrTabletLocalIdentityLabScratch) {
		t.Fatalf("zero retirement scratch = %v", err)
	}
}

func TestTabletLocalIdentityLabResolveAndWalkAllocateZero(t *testing.T) {
	view := tabletLocalIdentityLabTestView(t, 3686)
	if got := testing.AllocsPerRun(100, func() {
		if _, err := OpenTabletLocalIdentityLab(
			view.PersistentBytes(), view.Descriptor(), nil,
		); err != nil {
			panic(err)
		}
	}); got != 0 {
		t.Fatalf("Open allocations = %f", got)
	}
	if got := testing.AllocsPerRun(1000, func() {
		_, _ = view.Resolve(2048)
	}); got != 0 {
		t.Fatalf("Resolve allocations = %f", got)
	}
	if got := testing.AllocsPerRun(1000, func() {
		cursor := view.Cursor()
		for {
			_, _, ok := cursor.Next()
			if !ok {
				break
			}
		}
	}); got != 0 {
		t.Fatalf("walk allocations = %f", got)
	}
}

func TestTabletLocalIdentityLabScalarCRCMatchesCRC32C(t *testing.T) {
	prefix := []byte("root-bound-image")
	var suffix [14]byte
	binary.LittleEndian.PutUint16(suffix[0:2], 0x1234)
	binary.LittleEndian.PutUint32(suffix[2:6], 0x89abcdef)
	binary.LittleEndian.PutUint64(
		suffix[6:14], 0xfedcba9876543210,
	)
	got := PageChecksum(prefix)
	got = tabletLocalIdentityLabCRC16(got, 0x1234)
	got = tabletLocalIdentityLabCRC32(got, 0x89abcdef)
	got = tabletLocalIdentityLabCRC64(got, 0xfedcba9876543210)
	want := crc32.Update(
		PageChecksum(prefix), pageChecksumTable, suffix[:],
	)
	if got != want {
		t.Fatalf("scalar CRC = %08x, want %08x", got, want)
	}
}

func FuzzOpenTabletLocalIdentityLab(f *testing.F) {
	view := tabletLocalIdentityLabTestView(f, 512)
	f.Add(0, byte(1))
	f.Add(8191, byte(0x80))
	f.Fuzz(func(t *testing.T, at int, xor byte) {
		image := append([]byte(nil), view.PersistentBytes()...)
		if len(image) != 0 {
			at %= len(image)
			if at < 0 {
				at += len(image)
			}
			image[at] ^= xor
		}
		opened, err := OpenTabletLocalIdentityLab(
			image, view.Descriptor(), nil,
		)
		if xor == 0 {
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := opened.Resolve(17); !ok {
				t.Fatal("valid image lost live identity")
			}
		} else if err == nil {
			t.Fatal("mutated image admitted")
		}
	})
}
