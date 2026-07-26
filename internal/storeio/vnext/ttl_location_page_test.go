package vnext

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
	"slices"
	"sync"
	"testing"

	"github.com/thesyncim/vibejson/internal/storeio"
)

func ttlLocationIdentity(generation, logicalID uint64) Identity {
	return Identity{
		StoreID:    testIdentity.StoreID,
		Generation: generation,
		LogicalID:  logicalID,
	}
}

func ttlLocationRef(identity Identity, offset uint64) storeio.PageRef {
	return storeio.PageRef{
		Offset:     offset,
		LogicalID:  identity.LogicalID,
		Generation: identity.Generation,
		Length:     TTLLocationPageSize,
		Kind:       storeio.PageTTLDirectory,
	}
}

func forceTTLLocationChecksum(page []byte) {
	trailer := len(page) - FrameTrailerSize
	checksum := crc32.Checksum(page[:trailer], frameCRC)
	binary.LittleEndian.PutUint32(page[trailer:trailer+4], checksum)
	binary.LittleEndian.PutUint32(page[trailer+4:], ^checksum)
}

func mustTTLLocationLeaf(
	t testing.TB,
	identity Identity,
	entries []TTLLocationEntry,
) ([]byte, TTLLocationLeafView) {
	t.Helper()
	page, err := EncodeTTLLocationLeaf(
		make([]byte, TTLLocationPageSize), identity, entries,
	)
	if err != nil {
		t.Fatal(err)
	}
	view, err := OpenTTLLocationLeaf(page)
	if err != nil {
		t.Fatal(err)
	}
	return page, view
}

func mustTTLLocationBranch(
	t testing.TB,
	identity Identity,
	level uint8,
	children []TTLLocationChild,
) ([]byte, TTLLocationBranchView) {
	t.Helper()
	page, err := EncodeTTLLocationBranch(
		make([]byte, TTLLocationPageSize), identity, level, children,
	)
	if err != nil {
		t.Fatal(err)
	}
	view, err := OpenTTLLocationBranch(page)
	if err != nil {
		t.Fatal(err)
	}
	return page, view
}

func TestTTLLocationLeafRoundTripLookupAndFirstDue(t *testing.T) {
	identity := ttlLocationIdentity(7, 20)
	entries := []TTLLocationEntry{
		{Row: 11, Deadline: 900},
		{Row: 42, Deadline: 300},
		{Row: 91, Deadline: 300},
		{Row: 1 << 48, Deadline: 700},
	}
	page, view := mustTTLLocationLeaf(t, identity, entries)

	if len(page) != TTLLocationPageSize {
		t.Fatalf("image length = %d, want %d", len(page), TTLLocationPageSize)
	}
	if view.Identity() != identity || view.Len() != len(entries) ||
		view.LowerRow() != entries[0].Row || view.MinDeadline() != 300 {
		t.Fatalf("summary = (%+v,%d,%d,%d)", view.Identity(), view.Len(),
			view.LowerRow(), view.MinDeadline())
	}
	for rank, want := range entries {
		got, ok := view.EntryAt(rank)
		if !ok || got != want {
			t.Fatalf("EntryAt(%d) = (%+v,%v), want %+v", rank, got, ok, want)
		}
		deadline, ok := view.Lookup(want.Row)
		if !ok || deadline != want.Deadline {
			t.Fatalf("Lookup(%d) = (%d,%v), want %d", want.Row, deadline, ok,
				want.Deadline)
		}
	}
	for _, row := range []uint64{0, 10, 12, 43, 1 << 63, ^uint64(0)} {
		if deadline, ok := view.Lookup(row); ok {
			t.Fatalf("Lookup(%d) = (%d,true), want miss", row, deadline)
		}
	}
	if _, ok := view.EntryAt(-1); ok {
		t.Fatal("EntryAt(-1) succeeded")
	}
	if _, ok := view.EntryAt(len(entries)); ok {
		t.Fatal("EntryAt(len) succeeded")
	}
	if got, ok := view.FirstDue(299); ok {
		t.Fatalf("FirstDue(299) = %+v", got)
	}
	if got, ok := view.FirstDue(300); !ok || got != entries[1] {
		t.Fatalf("FirstDue(300) = (%+v,%v), want first minimum %+v",
			got, ok, entries[1])
	}

	ref := ttlLocationRef(identity, 2*Quantum)
	child, err := view.AsChild(ref)
	if err != nil {
		t.Fatal(err)
	}
	if child.LowerRow() != entries[0].Row ||
		child.MinDeadline() != 300 || child.Ref() != ref {
		t.Fatalf("child = (%d,%d,%+v)", child.LowerRow(),
			child.MinDeadline(), child.Ref())
	}
}

func TestTTLLocationBranchSelectAndFirstDue(t *testing.T) {
	type leafSpec struct {
		identity Identity
		offset   uint64
		entries  []TTLLocationEntry
	}
	specs := []leafSpec{
		{ttlLocationIdentity(5, 21), 2 * Quantum, []TTLLocationEntry{
			{Row: 10, Deadline: 800}, {Row: 19, Deadline: 900},
		}},
		{ttlLocationIdentity(6, 22), 3 * Quantum, []TTLLocationEntry{
			{Row: 20, Deadline: 200}, {Row: 29, Deadline: 700},
		}},
		{ttlLocationIdentity(7, 23), 4 * Quantum, []TTLLocationEntry{
			{Row: 50, Deadline: 200}, {Row: 99, Deadline: 600},
		}},
	}
	children := make([]TTLLocationChild, 0, len(specs))
	for _, spec := range specs {
		_, leaf := mustTTLLocationLeaf(t, spec.identity, spec.entries)
		child, err := leaf.AsChild(ttlLocationRef(spec.identity, spec.offset))
		if err != nil {
			t.Fatal(err)
		}
		children = append(children, child)
	}

	parentIdentity := ttlLocationIdentity(7, 90)
	page, branch := mustTTLLocationBranch(t, parentIdentity, 1, children)
	if len(page) != TTLLocationPageSize || branch.Identity() != parentIdentity ||
		branch.Level() != 1 || branch.Len() != 3 ||
		branch.LowerRow() != 10 || branch.MinDeadline() != 200 {
		t.Fatalf("branch summary = (%d,%+v,%d,%d,%d,%d)", len(page),
			branch.Identity(), branch.Level(), branch.Len(), branch.LowerRow(),
			branch.MinDeadline())
	}

	for _, test := range []struct {
		row  uint64
		ok   bool
		want int
	}{
		{0, false, 0},
		{9, false, 0},
		{10, true, 0},
		{19, true, 0},
		{20, true, 1},
		{49, true, 1},
		{50, true, 2},
		{^uint64(0), true, 2},
	} {
		got, ok := branch.Select(test.row)
		if ok != test.ok {
			t.Fatalf("Select(%d) ok = %v, want %v", test.row, ok, test.ok)
		}
		if ok && got != children[test.want] {
			t.Fatalf("Select(%d) = %+v, want %+v", test.row, got,
				children[test.want])
		}
	}
	if _, ok := branch.ChildAt(-1); ok {
		t.Fatal("ChildAt(-1) succeeded")
	}
	for rank, want := range children {
		got, ok := branch.ChildAt(rank)
		if !ok || got != want {
			t.Fatalf("ChildAt(%d) = (%+v,%v), want %+v", rank, got, ok, want)
		}
	}
	if _, ok := branch.ChildAt(len(children)); ok {
		t.Fatal("ChildAt(len) succeeded")
	}
	if got, ok := branch.FirstDue(199); ok {
		t.Fatalf("FirstDue(199) = %+v", got)
	}
	if got, ok := branch.FirstDue(200); !ok || got != children[1] {
		t.Fatalf("FirstDue(200) = (%+v,%v), want first minimum %+v",
			got, ok, children[1])
	}

	parentRef := ttlLocationRef(parentIdentity, 5*Quantum)
	child, err := branch.AsChild(parentRef)
	if err != nil {
		t.Fatal(err)
	}
	if child.LowerRow() != branch.LowerRow() ||
		child.MinDeadline() != branch.MinDeadline() ||
		child.Ref() != parentRef {
		t.Fatalf("parent child = %+v", child)
	}
}

func TestTTLLocationPageCapacityAndExactSpaceAccounting(t *testing.T) {
	if TTLLocationLeafCapacity != 249 || TTLLocationBranchCapacity != 83 {
		t.Fatalf("capacities = (%d,%d), want (249,83)",
			TTLLocationLeafCapacity, TTLLocationBranchCapacity)
	}

	entries := make([]TTLLocationEntry, TTLLocationLeafCapacity)
	for i := range entries {
		entries[i] = TTLLocationEntry{
			Row: uint64(i) * 7, Deadline: int64(10_000 + i),
		}
	}
	_, leaf := mustTTLLocationLeaf(
		t, ttlLocationIdentity(7, 30), entries,
	)
	assertTTLLocationSpace(t, leaf.Space(), TTLLocationLeafRecordSize)
	if got := float64(leaf.Space().ImageBytes) / float64(leaf.Len()); got > 16.45 {
		t.Fatalf("full leaf physical bytes/expiry = %.3f, want <= 16.45", got)
	}

	children := make([]TTLLocationChild, TTLLocationBranchCapacity)
	parent := ttlLocationIdentity(7, 200)
	for i := range children {
		children[i] = TTLLocationChild{
			lowerRow:    uint64(i) * 1_000,
			minDeadline: int64(i + 1),
			ref: storeio.PageRef{
				Offset:     uint64(i+2) * Quantum,
				LogicalID:  uint64(i + 2),
				Generation: 7,
				Length:     TTLLocationPageSize,
				Kind:       storeio.PageTTLDirectory,
			},
		}
	}
	_, branch := mustTTLLocationBranch(t, parent, 1, children)
	assertTTLLocationSpace(t, branch.Space(), TTLLocationBranchRecordSize)
}

func assertTTLLocationSpace(
	t testing.TB,
	space TTLLocationPageSpace,
	recordSize int,
) {
	t.Helper()
	total := space.FrameHeaderBytes + space.PayloadHeaderBytes +
		space.RecordBytes + space.PaddingBytes + space.FrameTrailerBytes
	if total != space.ImageBytes || space.ImageBytes != TTLLocationPageSize {
		t.Fatalf("space sum = %d, image = %d", total, space.ImageBytes)
	}
	if space.RecordBytes != space.Live*recordSize ||
		space.PaddingBytes !=
			space.ImageBytes-FrameHeaderSize-FrameTrailerSize-
				TTLLocationPayloadHeaderSize-space.RecordBytes {
		t.Fatalf("space = %+v", space)
	}
}

func TestTTLLocationEncodeRejectsNonCanonicalInput(t *testing.T) {
	identity := ttlLocationIdentity(7, 50)
	validEntry := TTLLocationEntry{Row: 1, Deadline: 1}
	for _, test := range []struct {
		name    string
		dst     []byte
		entries []TTLLocationEntry
	}{
		{"short image", make([]byte, TTLLocationPageSize-1), []TTLLocationEntry{validEntry}},
		{"empty", make([]byte, TTLLocationPageSize), nil},
		{"duplicate row", make([]byte, TTLLocationPageSize), []TTLLocationEntry{
			validEntry, validEntry,
		}},
		{"descending row", make([]byte, TTLLocationPageSize), []TTLLocationEntry{
			{Row: 2, Deadline: 1}, {Row: 1, Deadline: 2},
		}},
		{"zero deadline", make([]byte, TTLLocationPageSize), []TTLLocationEntry{
			{Row: 1, Deadline: 0},
		}},
	} {
		t.Run("leaf/"+test.name, func(t *testing.T) {
			if _, err := EncodeTTLLocationLeaf(
				test.dst, identity, test.entries,
			); !errors.Is(err, ErrInvalidFrame) {
				t.Fatalf("EncodeTTLLocationLeaf = %v", err)
			}
		})
	}
	tooManyEntries := make([]TTLLocationEntry, TTLLocationLeafCapacity+1)
	for i := range tooManyEntries {
		tooManyEntries[i] = TTLLocationEntry{Row: uint64(i), Deadline: 1}
	}
	if _, err := EncodeTTLLocationLeaf(
		make([]byte, TTLLocationPageSize), identity, tooManyEntries,
	); !errors.Is(err, ErrInvalidFrame) {
		t.Fatalf("over-capacity leaf = %v", err)
	}

	leafIdentity := ttlLocationIdentity(7, 51)
	_, leaf := mustTTLLocationLeaf(t, leafIdentity, []TTLLocationEntry{validEntry})
	validChild, err := leaf.AsChild(ttlLocationRef(leafIdentity, 2*Quantum))
	if err != nil {
		t.Fatal(err)
	}
	otherIdentity := ttlLocationIdentity(7, 52)
	_, otherLeaf := mustTTLLocationLeaf(t, otherIdentity, []TTLLocationEntry{
		{Row: 2, Deadline: 2},
	})
	otherChild, err := otherLeaf.AsChild(ttlLocationRef(otherIdentity, 3*Quantum))
	if err != nil {
		t.Fatal(err)
	}
	parent := ttlLocationIdentity(7, 60)
	for _, test := range []struct {
		name     string
		level    uint8
		children []TTLLocationChild
	}{
		{"empty", 1, nil},
		{"leaf level", 0, []TTLLocationChild{validChild}},
		{"excess level", ttlLocationMaxLevel + 1, []TTLLocationChild{validChild}},
		{"invented child", 1, []TTLLocationChild{{}}},
		{"descending lower row", 1, []TTLLocationChild{otherChild, validChild}},
		{"duplicate child", 1, []TTLLocationChild{validChild, {
			lowerRow:    2,
			minDeadline: validChild.minDeadline,
			ref:         validChild.ref,
		}}},
	} {
		t.Run("branch/"+test.name, func(t *testing.T) {
			if _, err := EncodeTTLLocationBranch(
				make([]byte, TTLLocationPageSize), parent,
				test.level, test.children,
			); !errors.Is(err, ErrInvalidFrame) {
				t.Fatalf("EncodeTTLLocationBranch = %v", err)
			}
		})
	}

	tooManyChildren := make([]TTLLocationChild, TTLLocationBranchCapacity+1)
	for i := range tooManyChildren {
		tooManyChildren[i] = TTLLocationChild{
			lowerRow: uint64(i), minDeadline: int64(i + 1),
			ref: storeio.PageRef{
				Offset: uint64(i+2) * Quantum, LogicalID: uint64(i + 2),
				Generation: 7, Length: TTLLocationPageSize,
				Kind: storeio.PageTTLDirectory,
			},
		}
	}
	if _, err := EncodeTTLLocationBranch(
		make([]byte, TTLLocationPageSize), parent, 1, tooManyChildren,
	); !errors.Is(err, ErrInvalidFrame) {
		t.Fatalf("over-capacity branch = %v", err)
	}

	for _, ref := range []storeio.PageRef{
		{},
		ttlLocationRef(leafIdentity, Quantum),
		ttlLocationRef(ttlLocationIdentity(7, storeio.StateRootLogicalID), 2*Quantum),
		ttlLocationRef(ttlLocationIdentity(6, leafIdentity.LogicalID), 2*Quantum),
		ttlLocationRef(ttlLocationIdentity(7, leafIdentity.LogicalID+1), 2*Quantum),
	} {
		if _, err := leaf.AsChild(ref); !errors.Is(err, ErrInvalidFrame) {
			t.Fatalf("AsChild(%+v) = %v", ref, err)
		}
	}
}

func TestTTLLocationPagesRejectCorruption(t *testing.T) {
	leafIdentity := ttlLocationIdentity(7, 70)
	leafPage, leaf := mustTTLLocationLeaf(t, leafIdentity, []TTLLocationEntry{
		{Row: 10, Deadline: 500},
		{Row: 20, Deadline: 100},
		{Row: 30, Deadline: 300},
	})
	leafChild, err := leaf.AsChild(ttlLocationRef(leafIdentity, 2*Quantum))
	if err != nil {
		t.Fatal(err)
	}
	otherIdentity := ttlLocationIdentity(7, 71)
	_, otherLeaf := mustTTLLocationLeaf(t, otherIdentity, []TTLLocationEntry{
		{Row: 40, Deadline: 600},
	})
	otherChild, err := otherLeaf.AsChild(ttlLocationRef(otherIdentity, 3*Quantum))
	if err != nil {
		t.Fatal(err)
	}
	branchPage, _ := mustTTLLocationBranch(
		t, ttlLocationIdentity(7, 80), 1,
		[]TTLLocationChild{leafChild, otherChild},
	)

	for _, test := range []struct {
		name string
		page []byte
		open func([]byte) error
	}{
		{"leaf data checksum", leafPage, func(p []byte) error {
			p[FrameHeaderSize+TTLLocationPayloadHeaderSize] ^= 1
			_, err := OpenTTLLocationLeaf(p)
			return err
		}},
		{"leaf trailer checksum", leafPage, func(p []byte) error {
			p[len(p)-1] ^= 1
			_, err := OpenTTLLocationLeaf(p)
			return err
		}},
		{"branch data checksum", branchPage, func(p []byte) error {
			p[FrameHeaderSize+TTLLocationPayloadHeaderSize] ^= 1
			_, err := OpenTTLLocationBranch(p)
			return err
		}},
		{"dirty leaf padding", leafPage, func(p []byte) error {
			payloadLength := int(binary.LittleEndian.Uint32(p[20:24]))
			p[FrameHeaderSize+payloadLength] = 1
			forceTTLLocationChecksum(p)
			_, err := OpenTTLLocationLeaf(p)
			return err
		}},
		{"dirty frame reserved byte", leafPage, func(p []byte) error {
			p[13] = 1
			forceTTLLocationChecksum(p)
			_, err := OpenTTLLocationLeaf(p)
			return err
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			page := slices.Clone(test.page)
			if err := test.open(page); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("open = %v", err)
			}
		})
	}

	leafRecord0 := FrameHeaderSize + TTLLocationPayloadHeaderSize
	for _, test := range []struct {
		name   string
		mutate func([]byte)
	}{
		{"level", func(p []byte) { p[FrameHeaderSize+4] = 1 }},
		{"payload reserved", func(p []byte) { p[FrameHeaderSize+28] = 1 }},
		{"record size", func(p []byte) {
			binary.LittleEndian.PutUint16(p[FrameHeaderSize+26:], 15)
		}},
		{"unordered rows", func(p []byte) {
			binary.LittleEndian.PutUint64(p[leafRecord0+TTLLocationLeafRecordSize:], 9)
		}},
		{"zero deadline", func(p []byte) {
			binary.LittleEndian.PutUint64(p[leafRecord0+8:], 0)
		}},
		{"wrong lower", func(p []byte) {
			binary.LittleEndian.PutUint64(p[FrameHeaderSize+16:], 11)
		}},
		{"wrong minimum", func(p []byte) {
			binary.LittleEndian.PutUint64(p[FrameHeaderSize+8:], 101)
		}},
		{"wrong minimum rank", func(p []byte) {
			binary.LittleEndian.PutUint16(p[FrameHeaderSize+24:], 0)
		}},
	} {
		t.Run("resealed leaf/"+test.name, func(t *testing.T) {
			page := slices.Clone(leafPage)
			test.mutate(page)
			if err := sealTTLLocationPage(page); err != nil {
				t.Fatal(err)
			}
			if _, err := OpenTTLLocationLeaf(page); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("OpenTTLLocationLeaf = %v", err)
			}
		})
	}

	branchRecord0 := FrameHeaderSize + TTLLocationPayloadHeaderSize
	branchRecord1 := branchRecord0 + TTLLocationBranchRecordSize
	for _, test := range []struct {
		name   string
		mutate func([]byte)
	}{
		{"zero level", func(p []byte) { p[FrameHeaderSize+4] = 0 }},
		{"unordered lower rows", func(p []byte) {
			binary.LittleEndian.PutUint64(p[branchRecord1:], 9)
		}},
		{"zero child minimum", func(p []byte) {
			binary.LittleEndian.PutUint64(p[branchRecord0+8:], 0)
		}},
		{"wrong summary lower", func(p []byte) {
			binary.LittleEndian.PutUint64(p[FrameHeaderSize+16:], 11)
		}},
		{"wrong summary minimum", func(p []byte) {
			binary.LittleEndian.PutUint64(p[FrameHeaderSize+8:], 99)
		}},
		{"wrong minimum rank", func(p []byte) {
			binary.LittleEndian.PutUint16(p[FrameHeaderSize+24:], 1)
		}},
		{"bad reference length", func(p []byte) {
			binary.LittleEndian.PutUint32(p[branchRecord0+16+24:], 1)
		}},
		{"duplicate reference logical ID", func(p []byte) {
			copy(p[branchRecord1+16+8:branchRecord1+16+16],
				p[branchRecord0+16+8:branchRecord0+16+16])
		}},
		{"duplicate reference offset", func(p []byte) {
			copy(p[branchRecord1+16:branchRecord1+16+8],
				p[branchRecord0+16:branchRecord0+16+8])
		}},
	} {
		t.Run("resealed branch/"+test.name, func(t *testing.T) {
			page := slices.Clone(branchPage)
			test.mutate(page)
			if err := sealTTLLocationPage(page); err != nil {
				t.Fatal(err)
			}
			if _, err := OpenTTLLocationBranch(page); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("OpenTTLLocationBranch = %v", err)
			}
		})
	}

	if _, err := OpenTTLLocationBranch(leafPage); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("leaf opened as branch = %v", err)
	}
	if _, err := OpenTTLLocationLeaf(branchPage); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("branch opened as leaf = %v", err)
	}
}

func TestTTLLocationWarmOperationsAllocateZero(t *testing.T) {
	entries := make([]TTLLocationEntry, TTLLocationLeafCapacity)
	for i := range entries {
		entries[i] = TTLLocationEntry{
			Row: uint64(i * 3), Deadline: int64(1_000 - i),
		}
	}
	_, leaf := mustTTLLocationLeaf(t, ttlLocationIdentity(7, 100), entries)

	children := make([]TTLLocationChild, TTLLocationBranchCapacity)
	for i := range children {
		children[i] = TTLLocationChild{
			lowerRow: uint64(i * 100), minDeadline: int64(1_000 - i),
			ref: storeio.PageRef{
				Offset: uint64(i+2) * Quantum, LogicalID: uint64(i + 2),
				Generation: 7, Length: TTLLocationPageSize,
				Kind: storeio.PageTTLDirectory,
			},
		}
	}
	_, branch := mustTTLLocationBranch(
		t, ttlLocationIdentity(7, 101), 1, children,
	)

	if got := testing.AllocsPerRun(1_000, func() {
		_, _ = leaf.Lookup(330)
		_, _ = leaf.Lookup(331)
		_, _ = leaf.FirstDue(1_000)
		_, _ = branch.Select(4_250)
		_, _ = branch.FirstDue(1_000)
	}); got != 0 {
		t.Fatalf("warm lookups/selects = %.2f allocs/run, want 0", got)
	}
}

func TestTTLLocationViewsSupportConcurrentReads(t *testing.T) {
	_, leaf := mustTTLLocationLeaf(t, ttlLocationIdentity(7, 110),
		[]TTLLocationEntry{
			{Row: 1, Deadline: 5}, {Row: 3, Deadline: 2},
		})
	var wait sync.WaitGroup
	wait.Add(8)
	for i := 0; i < 8; i++ {
		go func() {
			defer wait.Done()
			for j := 0; j < 1_000; j++ {
				if deadline, ok := leaf.Lookup(3); !ok || deadline != 2 {
					t.Errorf("Lookup(3) = (%d,%v)", deadline, ok)
					return
				}
				if entry, ok := leaf.FirstDue(2); !ok ||
					entry != (TTLLocationEntry{Row: 3, Deadline: 2}) {
					t.Errorf("FirstDue(2) = (%+v,%v)", entry, ok)
					return
				}
			}
		}()
	}
	wait.Wait()
}

func FuzzOpenTTLLocationPages(f *testing.F) {
	leafIdentity := ttlLocationIdentity(7, 120)
	leafPage, leaf := mustTTLLocationLeaf(f, leafIdentity,
		[]TTLLocationEntry{
			{Row: 1, Deadline: 20}, {Row: 2, Deadline: 10},
		})
	child, err := leaf.AsChild(ttlLocationRef(leafIdentity, 2*Quantum))
	if err != nil {
		f.Fatal(err)
	}
	branchPage, _ := mustTTLLocationBranch(
		f, ttlLocationIdentity(7, 121), 1, []TTLLocationChild{child},
	)
	f.Add(leafPage)
	f.Add(branchPage)
	f.Add([]byte(nil))
	f.Fuzz(func(t *testing.T, page []byte) {
		_, _ = OpenTTLLocationLeaf(page)
		_, _ = OpenTTLLocationBranch(page)
	})
}

func BenchmarkTTLLocationPage(b *testing.B) {
	entries := make([]TTLLocationEntry, TTLLocationLeafCapacity)
	for i := range entries {
		entries[i] = TTLLocationEntry{
			Row: uint64(i * 3), Deadline: int64(10_000 - i),
		}
	}
	_, leaf := mustTTLLocationLeaf(b, ttlLocationIdentity(7, 130), entries)

	children := make([]TTLLocationChild, TTLLocationBranchCapacity)
	for i := range children {
		children[i] = TTLLocationChild{
			lowerRow: uint64(i * 1_000), minDeadline: int64(10_000 - i),
			ref: storeio.PageRef{
				Offset: uint64(i+2) * Quantum, LogicalID: uint64(i + 2),
				Generation: 7, Length: TTLLocationPageSize,
				Kind: storeio.PageTTLDirectory,
			},
		}
	}
	_, branch := mustTTLLocationBranch(
		b, ttlLocationIdentity(7, 131), 1, children,
	)

	b.Run("leaf_lookup_hit", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			rank := i % TTLLocationLeafCapacity
			row := uint64(rank * 3)
			if deadline, ok := leaf.Lookup(row); !ok ||
				deadline != int64(10_000-rank) {
				b.Fatal("lookup hit")
			}
		}
		b.ReportMetric(float64(TTLLocationPageSize)/
			float64(TTLLocationLeafCapacity), "physical-B/expiry")
	})
	b.Run("leaf_lookup_miss", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, ok := leaf.Lookup(
				uint64((i%TTLLocationLeafCapacity)*3 + 1),
			); ok {
				b.Fatal("lookup miss")
			}
		}
	})
	b.Run("leaf_first_due", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			entry, ok := leaf.FirstDue(10_000)
			if !ok || entry.Row != uint64(TTLLocationLeafCapacity-1)*3 ||
				entry.Deadline != 10_000-TTLLocationLeafCapacity+1 {
				b.Fatal("leaf minimum")
			}
		}
	})
	b.Run("branch_select", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			rank := i % TTLLocationBranchCapacity
			child, ok := branch.Select(uint64(rank*1_000 + 1))
			if !ok || child.lowerRow != uint64(rank*1_000) {
				b.Fatal("branch select")
			}
		}
		b.ReportMetric(TTLLocationBranchCapacity, "fanout")
	})
	b.Run("branch_first_due", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			child, ok := branch.FirstDue(10_000)
			if !ok ||
				child.lowerRow != uint64(TTLLocationBranchCapacity-1)*1_000 {
				b.Fatal("branch minimum")
			}
		}
	})
}
