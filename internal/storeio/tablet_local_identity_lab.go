package storeio

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// The tablet-local identity experiment splits the 30-bit posting identity into
// an 18-bit macro-tablet and a 12-bit stable leaf identity. The dense inverse
// map is exactly 4096 uint16 values (8 KiB), so a posting can resolve its leaf
// without a radix descent or allocation.
//
// A live uint16 is a 12-bit stable anchor location: four bits of anchor-page ID
// and eight bits of row slot. FFFE is retired and FFFF is empty. Retirement
// generations deliberately do not use truncated bits in the uint16. They live
// in a sorted, root-bound exact-generation queue covered by the same checksum.
// This keeps snapshot lifetime unbounded while preserving the hot 8-KiB array.
const (
	TabletLocalIdentityLabTabletBits  = 18
	TabletLocalIdentityLabLocalBits   = 12
	TabletLocalIdentityLabImageBytes  = 1 << (TabletLocalIdentityLabLocalBits + 1)
	TabletLocalIdentityLabLocalCount  = 1 << TabletLocalIdentityLabLocalBits
	TabletLocalIdentityLabTabletCount = 1 << TabletLocalIdentityLabTabletBits
	TabletLocalIdentityLabBucketCount = 1 << (TabletLocalIdentityLabTabletBits + TabletLocalIdentityLabLocalBits)
	// DescriptorBytes is the canonical packed root charge; Go struct padding
	// is not part of the format. Each outstanding retirement adds ten bytes.
	TabletLocalIdentityLabDescriptorBytes = 36
	TabletLocalIdentityLabRetirementBytes = 10
	// A snapshot that pins every formerly live LocalID can retain one exact
	// retirement record per dense slot. This is a conservative logical-ID
	// queue bound, separate from physical extent reclamation.
	TabletLocalIdentityLabMaxRetirementBytes = TabletLocalIdentityLabLocalCount *
		TabletLocalIdentityLabRetirementBytes
	TabletLocalIdentityLabMaxEnvelopeBytes = TabletLocalIdentityLabImageBytes +
		TabletLocalIdentityLabDescriptorBytes +
		TabletLocalIdentityLabMaxRetirementBytes

	TabletLocalIdentityLabAnchorPageBits = 4
	TabletLocalIdentityLabRowSlotBits    = 8
	TabletLocalIdentityLabAnchorPages    = 1 << TabletLocalIdentityLabAnchorPageBits
	TabletLocalIdentityLabRowsPerPage    = 1 << TabletLocalIdentityLabRowSlotBits

	tabletLocalIdentityLabVersion = uint32(1)
	tabletLocalIdentityLabRetired = uint16(0xfffe)
	tabletLocalIdentityLabEmpty   = uint16(0xffff)
	tabletLocalIdentityLabLiveEnd = uint16(1 << TabletLocalIdentityLabLocalBits)
)

var (
	ErrTabletLocalIdentityLabCorrupt = errors.New(
		"vibejson: corrupt tablet-local identity lab image",
	)
	ErrTabletLocalIdentityLabScratch = errors.New(
		"vibejson: tablet-local identity lab retirement scratch is too small",
	)
	ErrTabletLocalIdentityLabReuse = errors.New(
		"vibejson: tablet-local identity lab identity is not safe to reuse",
	)
)

type TabletLocalIdentityLabState uint8

const (
	TabletLocalIdentityLabEmpty TabletLocalIdentityLabState = iota
	TabletLocalIdentityLabLive
	TabletLocalIdentityLabRetired
)

// TabletLocalIdentityLabLocation is stable within the tablet anchor layer.
// Physical PageRefs remain outside this image and may change on every COW.
type TabletLocalIdentityLabLocation struct {
	AnchorPageID uint8
	RowSlot      uint8
}

// TabletLocalIdentityLabAssignment seeds one live LocalID. Assignments must be
// in strictly increasing LocalID order.
type TabletLocalIdentityLabAssignment struct {
	LocalID  uint16
	Location TabletLocalIdentityLabLocation
}

// TabletLocalIdentityLabRetirement is the exact generation at which LocalID's
// previous meaning was last reachable. It is safe to recycle only when
// RetiredGeneration < the active snapshot floor. Records are sorted by LocalID
// and encoded as ten bytes when persisted by the owning tablet root.
type TabletLocalIdentityLabRetirement struct {
	LocalID           uint16
	RetiredGeneration uint64
}

// TabletLocalIdentityLabDescriptor belongs in the checksummed tablet root next
// to the raw 8-KiB image reference and exact retirement queue. Checksum binds
// all descriptor fields, the complete dense image, and every retirement record.
// The struct itself is pointer-free.
type TabletLocalIdentityLabDescriptor struct {
	TabletID           uint32
	Format             uint32
	Generation         uint64
	ReuseFloor         uint64
	LiveCount          uint16
	RetiredCount       uint16
	Checksum           uint32
	ChecksumComplement uint32
}

// TabletLocalIdentityLabView borrows a checksum-admitted dense image and its
// root-bound retirement queue. Resolve and live iteration touch only the image.
type TabletLocalIdentityLabView struct {
	image       []byte
	retirements []TabletLocalIdentityLabRetirement
	descriptor  TabletLocalIdentityLabDescriptor
}

type TabletLocalIdentityLabOperation uint8

const (
	TabletLocalIdentityLabAssign TabletLocalIdentityLabOperation = iota + 1
	TabletLocalIdentityLabMove
	TabletLocalIdentityLabRetire
)

// TabletLocalIdentityLabEdit performs one simultaneous COW batch operation.
// Assign accepts an ID that was never used or whose exact retirement generation
// is below ReuseFloor. Move preserves identity while changing its anchor row.
type TabletLocalIdentityLabEdit struct {
	LocalID   uint16
	Operation TabletLocalIdentityLabOperation
	Location  TabletLocalIdentityLabLocation
}

// TabletLocalIdentityLabCursor scans live mappings in LocalID order without
// allocating. Retired and empty identities are skipped.
type TabletLocalIdentityLabCursor struct {
	image []byte
	next  uint16
}

// MakeTabletLocalIdentityLabBucket combines an 18-bit tablet and 12-bit local
// identity. The largest valid pair produces the largest 30-bit BucketID.
func MakeTabletLocalIdentityLabBucket(
	tabletID uint32, localID uint32,
) (uint32, bool) {
	if tabletID >= TabletLocalIdentityLabTabletCount ||
		localID >= TabletLocalIdentityLabLocalCount {
		return 0, false
	}
	return tabletID<<TabletLocalIdentityLabLocalBits | localID, true
}

func SplitTabletLocalIdentityLabBucket(
	bucketID uint32,
) (tabletID uint32, localID uint16, ok bool) {
	if bucketID >= TabletLocalIdentityLabBucketCount {
		return 0, 0, false
	}
	return bucketID >> TabletLocalIdentityLabLocalBits,
		uint16(bucketID & (TabletLocalIdentityLabLocalCount - 1)), true
}

// TabletLocalIdentityLabDocumentCapacity is the exact namespace capacity at a
// chosen average rows per leaf. A zero density is invalid.
func TabletLocalIdentityLabDocumentCapacity(rowsPerLeaf uint64) (uint64, bool) {
	if rowsPerLeaf == 0 ||
		rowsPerLeaf > ^uint64(0)/TabletLocalIdentityLabBucketCount {
		return 0, false
	}
	return rowsPerLeaf * TabletLocalIdentityLabBucketCount, true
}

// EncodeTabletLocalIdentityLab creates an initial exact 8-KiB dense image.
// dst is caller-owned and assignments must be canonical and location-unique.
func EncodeTabletLocalIdentityLab(
	dst []byte,
	tabletID uint32,
	generation uint64,
	reuseFloor uint64,
	assignments []TabletLocalIdentityLabAssignment,
) (
	[]byte,
	TabletLocalIdentityLabDescriptor,
	error,
) {
	if len(dst) < TabletLocalIdentityLabImageBytes ||
		tabletID >= TabletLocalIdentityLabTabletCount ||
		generation == 0 ||
		!tabletLocalIdentityLabValidFloor(generation, reuseFloor) ||
		len(assignments) > TabletLocalIdentityLabLocalCount {
		return nil, TabletLocalIdentityLabDescriptor{},
			fmt.Errorf("%w: initial identity or geometry", ErrInvalidWrite)
	}
	image := dst[:TabletLocalIdentityLabImageBytes]
	for at := range image {
		image[at] = 0xff
	}
	var occupied [TabletLocalIdentityLabLocalCount / 64]uint64
	var previous uint16
	for rank, assignment := range assignments {
		if uint32(assignment.LocalID) >= TabletLocalIdentityLabLocalCount ||
			rank != 0 && assignment.LocalID <= previous {
			return nil, TabletLocalIdentityLabDescriptor{},
				fmt.Errorf("%w: non-canonical assignment order", ErrInvalidWrite)
		}
		code, ok := tabletLocalIdentityLabEncodeLocation(assignment.Location)
		if !ok || tabletLocalIdentityLabLocationSeen(&occupied, code) {
			return nil, TabletLocalIdentityLabDescriptor{},
				fmt.Errorf("%w: invalid or duplicate anchor location", ErrInvalidWrite)
		}
		binary.LittleEndian.PutUint16(
			image[int(assignment.LocalID)*2:], code,
		)
		previous = assignment.LocalID
	}
	descriptor := TabletLocalIdentityLabDescriptor{
		TabletID:   tabletID,
		Format:     tabletLocalIdentityLabVersion,
		Generation: generation,
		ReuseFloor: reuseFloor,
		LiveCount:  uint16(len(assignments)),
	}
	tabletLocalIdentityLabSeal(&descriptor, image, nil)
	return image, descriptor, nil
}

// OpenTabletLocalIdentityLab verifies the root binding, checksum, state
// encoding, live-location uniqueness, retirement ordering, and exact state to
// retirement-record correspondence. It allocates nothing on success or error.
func OpenTabletLocalIdentityLab(
	image []byte,
	descriptor TabletLocalIdentityLabDescriptor,
	retirements []TabletLocalIdentityLabRetirement,
) (TabletLocalIdentityLabView, error) {
	if len(image) != TabletLocalIdentityLabImageBytes ||
		descriptor.Format != tabletLocalIdentityLabVersion ||
		descriptor.TabletID >= TabletLocalIdentityLabTabletCount ||
		descriptor.Generation == 0 ||
		!tabletLocalIdentityLabValidFloor(
			descriptor.Generation, descriptor.ReuseFloor,
		) ||
		int(descriptor.RetiredCount) != len(retirements) ||
		descriptor.ChecksumComplement != ^descriptor.Checksum ||
		tabletLocalIdentityLabChecksum(
			descriptor, image, retirements,
		) != descriptor.Checksum {
		return TabletLocalIdentityLabView{},
			fmt.Errorf("%w: descriptor or checksum", ErrTabletLocalIdentityLabCorrupt)
	}

	var occupied [TabletLocalIdentityLabLocalCount / 64]uint64
	live := 0
	retired := 0
	retirementAt := 0
	for localID := 0; localID < TabletLocalIdentityLabLocalCount; localID++ {
		code := binary.LittleEndian.Uint16(image[localID*2:])
		switch {
		case code < tabletLocalIdentityLabLiveEnd:
			if tabletLocalIdentityLabLocationSeen(&occupied, code) {
				return TabletLocalIdentityLabView{},
					fmt.Errorf("%w: duplicate live location", ErrTabletLocalIdentityLabCorrupt)
			}
			live++
		case code == tabletLocalIdentityLabRetired:
			if retirementAt >= len(retirements) ||
				int(retirements[retirementAt].LocalID) != localID ||
				retirements[retirementAt].RetiredGeneration == 0 ||
				retirements[retirementAt].RetiredGeneration >
					descriptor.Generation ||
				retirements[retirementAt].RetiredGeneration <
					descriptor.ReuseFloor {
				return TabletLocalIdentityLabView{},
					fmt.Errorf("%w: retirement binding or floor", ErrTabletLocalIdentityLabCorrupt)
			}
			retirementAt++
			retired++
		case code == tabletLocalIdentityLabEmpty:
		default:
			return TabletLocalIdentityLabView{},
				fmt.Errorf("%w: reserved locator code", ErrTabletLocalIdentityLabCorrupt)
		}
	}
	if live != int(descriptor.LiveCount) ||
		retired != int(descriptor.RetiredCount) ||
		retirementAt != len(retirements) {
		return TabletLocalIdentityLabView{},
			fmt.Errorf("%w: state cardinality", ErrTabletLocalIdentityLabCorrupt)
	}
	return TabletLocalIdentityLabView{
		image:       image,
		retirements: retirements,
		descriptor:  descriptor,
	}, nil
}

func (v TabletLocalIdentityLabView) Descriptor() TabletLocalIdentityLabDescriptor {
	return v.descriptor
}

func (v TabletLocalIdentityLabView) PersistentBytes() []byte {
	return v.image
}

// Resolve is the posting-driven hot path. It performs one bounds check, one
// uint16 load, and two shifts; empty and retired IDs fail closed.
func (v TabletLocalIdentityLabView) Resolve(
	localID uint16,
) (TabletLocalIdentityLabLocation, bool) {
	if uint32(localID) >= TabletLocalIdentityLabLocalCount {
		return TabletLocalIdentityLabLocation{}, false
	}
	code := binary.LittleEndian.Uint16(v.image[int(localID)*2:])
	if code >= tabletLocalIdentityLabLiveEnd {
		return TabletLocalIdentityLabLocation{}, false
	}
	return tabletLocalIdentityLabDecodeLocation(code), true
}

// Entry exposes all three states. Retirement lookup is binary search over the
// root-bound exact-generation queue; the live Resolve path never pays for it.
func (v TabletLocalIdentityLabView) Entry(
	localID uint16,
) (
	TabletLocalIdentityLabState,
	TabletLocalIdentityLabLocation,
	uint64,
	bool,
) {
	if uint32(localID) >= TabletLocalIdentityLabLocalCount {
		return TabletLocalIdentityLabEmpty,
			TabletLocalIdentityLabLocation{}, 0, false
	}
	code := binary.LittleEndian.Uint16(v.image[int(localID)*2:])
	switch {
	case code < tabletLocalIdentityLabLiveEnd:
		return TabletLocalIdentityLabLive,
			tabletLocalIdentityLabDecodeLocation(code), 0, true
	case code == tabletLocalIdentityLabEmpty:
		return TabletLocalIdentityLabEmpty,
			TabletLocalIdentityLabLocation{}, 0, true
	case code == tabletLocalIdentityLabRetired:
		at := tabletLocalIdentityLabFindRetirement(v.retirements, localID)
		if at < len(v.retirements) && v.retirements[at].LocalID == localID {
			return TabletLocalIdentityLabRetired,
				TabletLocalIdentityLabLocation{},
				v.retirements[at].RetiredGeneration, true
		}
	}
	return TabletLocalIdentityLabEmpty,
		TabletLocalIdentityLabLocation{}, 0, false
}

func (v TabletLocalIdentityLabView) Cursor() TabletLocalIdentityLabCursor {
	return TabletLocalIdentityLabCursor{image: v.image}
}

func (c *TabletLocalIdentityLabCursor) Next() (
	uint16,
	TabletLocalIdentityLabLocation,
	bool,
) {
	for uint32(c.next) < TabletLocalIdentityLabLocalCount {
		localID := c.next
		c.next++
		code := binary.LittleEndian.Uint16(c.image[int(localID)*2:])
		if code < tabletLocalIdentityLabLiveEnd {
			return localID, tabletLocalIdentityLabDecodeLocation(code), true
		}
	}
	return 0, TabletLocalIdentityLabLocation{}, false
}

// RewriteTabletLocalIdentityLab applies one simultaneous COW batch. The
// snapshot floor is monotonic. Eligible retirements become empty automatically;
// a Retire records base.Generation, the last root that could reach the old
// meaning. retireDst is caller-owned exact-generation scratch.
// retireDst must not alias base.retirements.
//
// Correct identity reuse depends on one publication invariant: StateRoot must
// atomically select this descriptor/image, every anchor root, and every exact
// index root. A background index builder may carry a BucketID across roots only
// while pinning the source generation; otherwise it must re-resolve under the
// destination root. The conservative retirement floor protects that boundary.
//
// Ordinary root-bound snapshots do not intrinsically require this logical-ID
// queue: an old snapshot resolves a reused number through its old locator and
// old index roots, while the new root resolves it through the new set. Production
// can reclaim LocalIDs immediately—even with old snapshots—if and only if:
//
//   - one failure-atomic StateRoot selects locator, anchors, and every index;
//   - every cache key includes the physical page generation;
//   - no background build, cursor, posting batch, or change feed carries a bare
//     BucketID across roots (it pins the source generation or re-resolves);
//   - recovery never assembles component roots from different generations.
//
// Under those invariants the exact queue can disappear. Physical PageRefs and
// extents still require the existing snapshot-floor reclaimer; only numeric
// LocalID reuse becomes immediate.
func RewriteTabletLocalIdentityLab(
	dst []byte,
	retireDst []TabletLocalIdentityLabRetirement,
	base TabletLocalIdentityLabView,
	generation uint64,
	reuseFloor uint64,
	edits []TabletLocalIdentityLabEdit,
) (
	[]byte,
	TabletLocalIdentityLabDescriptor,
	[]TabletLocalIdentityLabRetirement,
	error,
) {
	if len(dst) < TabletLocalIdentityLabImageBytes ||
		len(base.image) != TabletLocalIdentityLabImageBytes ||
		generation <= base.descriptor.Generation ||
		reuseFloor < base.descriptor.ReuseFloor ||
		// The lease floor describes the currently published base root, not
		// the generation being prepared (which may legitimately skip).
		!tabletLocalIdentityLabValidFloor(
			base.descriptor.Generation, reuseFloor,
		) ||
		len(edits) > TabletLocalIdentityLabLocalCount {
		return nil, TabletLocalIdentityLabDescriptor{}, retireDst[:0],
			fmt.Errorf("%w: rewrite identity or geometry", ErrInvalidWrite)
	}
	for rank, edit := range edits {
		if uint32(edit.LocalID) >= TabletLocalIdentityLabLocalCount ||
			rank != 0 && edit.LocalID <= edits[rank-1].LocalID ||
			edit.Operation < TabletLocalIdentityLabAssign ||
			edit.Operation > TabletLocalIdentityLabRetire {
			return nil, TabletLocalIdentityLabDescriptor{}, retireDst[:0],
				fmt.Errorf("%w: non-canonical edit", ErrInvalidWrite)
		}
		if edit.Operation != TabletLocalIdentityLabRetire {
			if _, ok := tabletLocalIdentityLabEncodeLocation(edit.Location); !ok {
				return nil, TabletLocalIdentityLabDescriptor{}, retireDst[:0],
					fmt.Errorf("%w: edit anchor location", ErrInvalidWrite)
			}
		}
	}

	image := dst[:TabletLocalIdentityLabImageBytes]
	copy(image, base.image)
	retirements := retireDst[:0]
	var occupied [TabletLocalIdentityLabLocalCount / 64]uint64
	editAt := 0
	retirementAt := 0
	liveCount := 0
	for local := 0; local < TabletLocalIdentityLabLocalCount; local++ {
		localID := uint16(local)
		code := binary.LittleEndian.Uint16(base.image[local*2:])
		state := TabletLocalIdentityLabEmpty
		var location TabletLocalIdentityLabLocation
		var retiredGeneration uint64
		switch {
		case code < tabletLocalIdentityLabLiveEnd:
			state = TabletLocalIdentityLabLive
			location = tabletLocalIdentityLabDecodeLocation(code)
		case code == tabletLocalIdentityLabRetired:
			if retirementAt >= len(base.retirements) ||
				base.retirements[retirementAt].LocalID != localID {
				return nil, TabletLocalIdentityLabDescriptor{}, retirements,
					fmt.Errorf("%w: base retirement binding", ErrTabletLocalIdentityLabCorrupt)
			}
			retiredGeneration =
				base.retirements[retirementAt].RetiredGeneration
			retirementAt++
			if retiredGeneration >= reuseFloor {
				state = TabletLocalIdentityLabRetired
			}
		case code == tabletLocalIdentityLabEmpty:
		default:
			return nil, TabletLocalIdentityLabDescriptor{}, retirements,
				fmt.Errorf("%w: base locator code", ErrTabletLocalIdentityLabCorrupt)
		}

		if editAt < len(edits) && edits[editAt].LocalID == localID {
			edit := edits[editAt]
			editAt++
			switch edit.Operation {
			case TabletLocalIdentityLabAssign:
				if state != TabletLocalIdentityLabEmpty {
					return nil, TabletLocalIdentityLabDescriptor{}, retirements,
						fmt.Errorf("%w: LocalID %d", ErrTabletLocalIdentityLabReuse, localID)
				}
				state = TabletLocalIdentityLabLive
				location = edit.Location
			case TabletLocalIdentityLabMove:
				if state != TabletLocalIdentityLabLive {
					return nil, TabletLocalIdentityLabDescriptor{}, retirements,
						fmt.Errorf("%w: move of non-live LocalID %d", ErrInvalidWrite, localID)
				}
				location = edit.Location
			case TabletLocalIdentityLabRetire:
				if state != TabletLocalIdentityLabLive {
					return nil, TabletLocalIdentityLabDescriptor{}, retirements,
						fmt.Errorf("%w: retirement of non-live LocalID %d", ErrInvalidWrite, localID)
				}
				if base.descriptor.Generation < reuseFloor {
					state = TabletLocalIdentityLabEmpty
				} else {
					state = TabletLocalIdentityLabRetired
					retiredGeneration = base.descriptor.Generation
				}
			}
		}

		switch state {
		case TabletLocalIdentityLabEmpty:
			binary.LittleEndian.PutUint16(image[local*2:], tabletLocalIdentityLabEmpty)
		case TabletLocalIdentityLabLive:
			liveCode, _ := tabletLocalIdentityLabEncodeLocation(location)
			if tabletLocalIdentityLabLocationSeen(&occupied, liveCode) {
				return nil, TabletLocalIdentityLabDescriptor{}, retirements,
					fmt.Errorf("%w: duplicate post-edit anchor location", ErrInvalidWrite)
			}
			binary.LittleEndian.PutUint16(image[local*2:], liveCode)
			liveCount++
		case TabletLocalIdentityLabRetired:
			if len(retirements) == cap(retirements) {
				return nil, TabletLocalIdentityLabDescriptor{}, retirements,
					ErrTabletLocalIdentityLabScratch
			}
			retirements = append(retirements, TabletLocalIdentityLabRetirement{
				LocalID: localID, RetiredGeneration: retiredGeneration,
			})
			binary.LittleEndian.PutUint16(image[local*2:], tabletLocalIdentityLabRetired)
		}
	}
	if editAt != len(edits) || retirementAt != len(base.retirements) {
		return nil, TabletLocalIdentityLabDescriptor{}, retirements,
			fmt.Errorf("%w: batch merge cardinality", ErrInvalidWrite)
	}
	descriptor := TabletLocalIdentityLabDescriptor{
		TabletID:     base.descriptor.TabletID,
		Format:       tabletLocalIdentityLabVersion,
		Generation:   generation,
		ReuseFloor:   reuseFloor,
		LiveCount:    uint16(liveCount),
		RetiredCount: uint16(len(retirements)),
	}
	tabletLocalIdentityLabSeal(&descriptor, image, retirements)
	return image, descriptor, retirements, nil
}

// TabletLocalIdentityLabBytesPerLive reports the dense locator charge. The
// root descriptor and ten-byte exact retirement records are reported
// separately because they are shared/temporary rather than charged per live ID.
func TabletLocalIdentityLabBytesPerLive(live int) float64 {
	if live <= 0 {
		return 0
	}
	return float64(TabletLocalIdentityLabImageBytes) / float64(live)
}

// TabletLocalIdentityLabEnvelopeBytes reports the exact packed durable charge
// for the root-bound descriptor, dense locator, and outstanding retirement
// records. Go slice/struct padding and physical allocator rounding are excluded.
func TabletLocalIdentityLabEnvelopeBytes(retired int) (int, bool) {
	if retired < 0 || retired > TabletLocalIdentityLabLocalCount {
		return 0, false
	}
	return TabletLocalIdentityLabImageBytes +
		TabletLocalIdentityLabDescriptorBytes +
		retired*TabletLocalIdentityLabRetirementBytes, true
}

func tabletLocalIdentityLabEncodeLocation(
	location TabletLocalIdentityLabLocation,
) (uint16, bool) {
	if location.AnchorPageID >= TabletLocalIdentityLabAnchorPages {
		return 0, false
	}
	return uint16(location.AnchorPageID)<<TabletLocalIdentityLabRowSlotBits |
		uint16(location.RowSlot), true
}

func tabletLocalIdentityLabDecodeLocation(
	code uint16,
) TabletLocalIdentityLabLocation {
	return TabletLocalIdentityLabLocation{
		AnchorPageID: uint8(code >> TabletLocalIdentityLabRowSlotBits),
		RowSlot:      uint8(code),
	}
}

func tabletLocalIdentityLabLocationSeen(
	occupied *[TabletLocalIdentityLabLocalCount / 64]uint64,
	code uint16,
) bool {
	word := code >> 6
	bit := uint64(1) << (code & 63)
	seen := occupied[word]&bit != 0
	occupied[word] |= bit
	return seen
}

func tabletLocalIdentityLabFindRetirement(
	retirements []TabletLocalIdentityLabRetirement,
	localID uint16,
) int {
	low, high := 0, len(retirements)
	for low < high {
		middle := int(uint(low+high) >> 1)
		if retirements[middle].LocalID < localID {
			low = middle + 1
		} else {
			high = middle
		}
	}
	return low
}

func tabletLocalIdentityLabValidFloor(
	generation, reuseFloor uint64,
) bool {
	if generation == 0 || reuseFloor == 0 {
		return false
	}
	// With no active snapshot, the generation lease layer reports current+1.
	// Spell that without overflowing the last representable generation.
	return reuseFloor <= generation ||
		generation != ^uint64(0) && reuseFloor == generation+1
}

func tabletLocalIdentityLabSeal(
	descriptor *TabletLocalIdentityLabDescriptor,
	image []byte,
	retirements []TabletLocalIdentityLabRetirement,
) {
	descriptor.Checksum = tabletLocalIdentityLabChecksum(
		*descriptor, image, retirements,
	)
	descriptor.ChecksumComplement = ^descriptor.Checksum
}

func tabletLocalIdentityLabChecksum(
	descriptor TabletLocalIdentityLabDescriptor,
	image []byte,
	retirements []TabletLocalIdentityLabRetirement,
) uint32 {
	// CRC the large resident slice first, then append the canonical descriptor
	// and queue fields with scalar table updates. Small stack byte arrays passed
	// to hash/crc32 escape today; spelling these updates out keeps COW batches
	// allocation-free without changing the CRC32C polynomial.
	checksum := PageChecksum(image)
	checksum = tabletLocalIdentityLabCRC64(checksum, uint64(0x313042414c494c54)) // "TLILAB01"
	checksum = tabletLocalIdentityLabCRC32(checksum, descriptor.Format)
	checksum = tabletLocalIdentityLabCRC32(
		checksum, TabletLocalIdentityLabImageBytes,
	)
	checksum = tabletLocalIdentityLabCRC32(checksum, descriptor.TabletID)
	checksum = tabletLocalIdentityLabCRC32(checksum,
		TabletLocalIdentityLabTabletBits|
			TabletLocalIdentityLabLocalBits<<8|
			TabletLocalIdentityLabAnchorPageBits<<16|
			TabletLocalIdentityLabRowSlotBits<<24,
	)
	checksum = tabletLocalIdentityLabCRC64(checksum, descriptor.Generation)
	checksum = tabletLocalIdentityLabCRC64(checksum, descriptor.ReuseFloor)
	checksum = tabletLocalIdentityLabCRC16(checksum, descriptor.LiveCount)
	checksum = tabletLocalIdentityLabCRC16(checksum, descriptor.RetiredCount)
	for _, retirement := range retirements {
		checksum = tabletLocalIdentityLabCRC16(
			checksum, retirement.LocalID,
		)
		checksum = tabletLocalIdentityLabCRC64(
			checksum, retirement.RetiredGeneration,
		)
	}
	return checksum
}

func tabletLocalIdentityLabCRCByte(checksum uint32, value byte) uint32 {
	current := ^checksum
	current = pageChecksumTable[byte(current)^value] ^ current>>8
	return ^current
}

func tabletLocalIdentityLabCRC16(checksum uint32, value uint16) uint32 {
	checksum = tabletLocalIdentityLabCRCByte(checksum, byte(value))
	return tabletLocalIdentityLabCRCByte(checksum, byte(value>>8))
}

func tabletLocalIdentityLabCRC32(checksum uint32, value uint32) uint32 {
	checksum = tabletLocalIdentityLabCRC16(checksum, uint16(value))
	return tabletLocalIdentityLabCRC16(checksum, uint16(value>>16))
}

func tabletLocalIdentityLabCRC64(checksum uint32, value uint64) uint32 {
	checksum = tabletLocalIdentityLabCRC32(checksum, uint32(value))
	return tabletLocalIdentityLabCRC32(checksum, uint32(value>>32))
}
