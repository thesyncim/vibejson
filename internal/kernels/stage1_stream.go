package kernels

// Provenance: CPP-STAGE1-001. The pipeline shape follows C++ simdjson 4.6.4
// json_scanner and structural indexer at commit
// 1bcf71bd85059ab6574ea1159de9298dcc1212c5; Apache-2.0, see
// LICENSE-SIMDJSON. Batching, record layout, metadata, and specializations are
// local changes.
//
// Streamed stage-1: the batched kernel classifies a run of 64-byte blocks
// per call and emits one Stage1Rec per block, keeping the carry state
// internal to the call. The kernel is Stage1BlocksGP: classification in
// NEON, per-mask movemask to general-purpose registers, escape chain and
// prefix-XOR in scalar code. This is the C++ simdjson shape
// (json_scanner::next), batched so the vector constants load once per
// chunk instead of once per block.
//
// The record carries exactly the masks the bitmap validator consumes;
// everything else (whitespace skipping, string interiors) dies inside
// the kernel.

// Stage1ChunkBlocks is the maximum number of blocks per kernel call. It
// matches the validator's sampling window so the first chunk decides
// engine commitment, and 32 blocks of records stay comfortably inside
// L1.
const Stage1ChunkBlocks = 32

// Stage1Rec is the per-block output of the batched kernel. Bit i of each
// mask describes byte i of the block.
//
// Scalar keeps the complete scalar-byte runs that produced Emit's scalar
// starts. The Go grammar consumer uses those runs to recover exact token ends
// with one trailing-zero operation, avoiding delimiter reloads and bytewise
// scans for the overwhelmingly common short integers and literals.
type Stage1Rec struct {
	Emit     uint64 // structural bytes outside strings, opening quotes, scalar starts
	Scalar   uint64 // every byte in scalar runs outside strings
	EscInStr uint64 // escape-target bytes inside strings (byte after a backslash)
	WsOut    uint64 // whitespace outside strings (density sampling)
	InStr    uint64 // in-string bytes, opening quote included, closing quote excluded (density sampling)
	Bad      bool   // any control-byte violation (raw control in a string, non-ws control outside)
	NonASCII bool   // any byte at or above 0x80 in this block (drives per-run UTF-8 checking)
}

// Stage1Stream threads carry state between chunks. The zero value is the
// document-start state.
type Stage1Stream struct {
	Carry   Stage1Carry
	Follows uint64 // bit 0: last byte of the previous block was a scalar candidate
}

// Stage1IndexStream carries the state for the packed structural-index
// producer. The zero value is the document-start state.
type Stage1IndexStream struct {
	Carry      Stage1Carry
	Follows    uint64
	PreviousIn uint64
	Bad        bool
	NonASCII   bool
	Escaped    bool
}

// Stage1ValidMeta carries the per-block checks that cannot be reduced to a
// document-wide sticky bit. Stage1ValidBlocks overwrites entries for the
// blocks in its current call.
type Stage1ValidMeta struct {
	EscInStr [Stage1ChunkBlocks]uint64
	NonASCII uint32
}

// Stage1IndexMeta carries the per-block facts needed by a direct index
// consumer. Counts cover the current call and let the first 2 KiB double as
// the density sample instead of classifying it twice.
type Stage1IndexMeta struct {
	EscInStr   [Stage1ChunkBlocks]uint64
	InStr      [Stage1ChunkBlocks]uint64
	NonASCII   uint32
	Sample     bool
	WsCount    uint32
	EmitCount  uint32
	InStrCount uint32
	EscCount   uint32
}

// Stage1RecFromMasks derives one record from a block's classification masks
// using the portable carry kernels. It is the scalar reference for the batched
// kernel and works on every build.
func Stage1RecFromMasks(m *Stage1Masks, st *Stage1Stream, r *Stage1Rec) {
	escaped := Stage1Escaped(m.Backslash, &st.Carry)
	quotes := m.Quote &^ escaped
	inString := Stage1PrefixXOR(quotes, &st.Carry)

	// inString includes opening quotes and excludes closing quotes. Combining
	// it with every unescaped quote therefore excludes both quote boundaries
	// and the entire string body in one mask.
	outside := ^(inString | quotes)
	openers := quotes & inString

	// Raw quotes are excluded rather than only unescaped quotes: an escaped
	// quote inside a string must not become a scalar candidate. Since cand also
	// excludes inString, it is already a strict subset of outside.
	cand := ^(m.Whitespace | m.Structural | m.Quote | inString)
	starts := cand &^ (cand<<1 | st.Follows)
	st.Follows = cand >> 63

	r.Emit = (m.Structural|starts)&outside | openers
	r.Scalar = cand
	r.EscInStr = escaped & inString
	r.Bad = m.Control&(inString|outside&^m.Whitespace) != 0
	r.WsOut = m.Whitespace & outside
	r.InStr = inString
	r.NonASCII = m.NonASCII
}
