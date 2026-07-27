package storeio

const (
	// PrimaryBucketIDLimit is the first value outside the durable 30-bit
	// primary-leaf identity namespace. The upper bits identify a tablet and
	// the lower bits identify a stable leaf inside that tablet.
	PrimaryBucketIDLimit = uint32(1 << 30)
)

// BucketID is the stable 30-bit identity carried by secondary posting tiles.
// It is logical: copy-on-write may move the leaf without changing this value.
type BucketID uint32

// BucketZone is the compact leaf summary carried by a primary router handle.
// Its interpretation belongs to the ordered-leaf layer.
type BucketZone [4]byte
