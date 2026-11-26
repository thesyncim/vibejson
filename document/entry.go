package document

// Flags apply only to their associated entry kinds.
const (
	TapeFlagEscaped = 1 << iota // string contains at least one escape sequence
	TapeFlagKey                 // string is an object key
	TapeFlagInt                 // number is a plain integer: optional minus, then digits only
)

// TapeFlagObjectKeysHashed marks an Object header whose key entries carry
// precomputed hashes. It reuses the string-only escaped bit.
const TapeFlagObjectKeysHashed = TapeFlagEscaped

// The info word packs count, kind, and flags into one uint32, keeping entries
// at four words (16 bytes). Count uses 26 bits, kind 3, and flags 3:
//
//	 31     29 28    26 25                        0
//	+---------+--------+--------------------------+
//	|  flags  |  kind  |          count           |
//	+---------+--------+--------------------------+
//
// Count is meaningful only for containers; builders reject overflow.
const (
	InfoCountBits         = 26
	InfoKindBits          = 3
	InfoCountMask  uint32 = 1<<InfoCountBits - 1
	InfoKindShift         = InfoCountBits
	InfoKindMask   uint32 = (1<<InfoKindBits - 1) << InfoKindShift
	InfoFlagsShift        = InfoCountBits + InfoKindBits
	InfoMaxCount   uint32 = InfoCountMask
)

// IndexEntry is one compact structural entry. Its offsets and metadata must not
// be modified while the Index or a derived Node is in use.
type IndexEntry struct {
	Start uint32
	End   uint32
	Next  uint32
	Info  uint32
}

// KeysHashed reports whether this Object header was enriched with per-key
// hashes. It is meaningful only on an Object entry.
func (e *IndexEntry) KeysHashed() bool {
	return e.Flags()&TapeFlagObjectKeysHashed != 0
}

// Kind returns the entry's JSON kind.
func (e *IndexEntry) Kind() Kind {
	return Kind((e.Info & InfoKindMask) >> InfoKindShift)
}

// Flags returns the entry's tape Flags (escaped, key, integer).
func (e *IndexEntry) Flags() uint8 {
	return uint8(e.Info >> InfoFlagsShift)
}

// Count returns a container's direct element count. It is meaningful only for
// arrays and objects; other kinds report zero.
func (e *IndexEntry) Count() uint32 {
	return e.Info & InfoCountMask
}

// PackInfo composes an info word from its parts. The caller guarantees count
// fits in InfoCountBits; the builders check this before an entry is written.
func PackInfo(count uint32, kind Kind, flags uint8) uint32 {
	return count&InfoCountMask | uint32(kind)<<InfoKindShift | uint32(flags)<<InfoFlagsShift
}

// SetCount replaces the entry's element count, preserving kind and Flags.
func (e *IndexEntry) SetCount(count uint32) {
	e.Info = e.Info&^InfoCountMask | count&InfoCountMask
}

// BumpCount adds one to the entry's element count in place. count occupies the
// low bits of Info, so an increment cannot disturb kind or Flags unless it
// overflows the count field, which the builders prevent.
func (e *IndexEntry) BumpCount() {
	e.Info++
}
