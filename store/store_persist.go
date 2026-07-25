package store

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/maphash"
	"io"
	"math"
	"math/bits"
	"runtime"
	"slices"

	"github.com/thesyncim/vibejson"
	"github.com/thesyncim/vibejson/document"
	"github.com/thesyncim/vibejson/internal/byteview"
)

// Store persistence is a Store-native container around the existing bounded
// DocSet page image. Each materialized Store chunk is written as one complete
// mmap-friendly DocSet image. A checksummed tail manifest owns the keyed layer:
// stable slots, key spellings, options, ready index definitions, TTL deadlines,
// and reusable empty chunk ids. OpenStore views the immutable document bytes
// and structural tapes directly from the caller's image, rebuilds the seeded
// compact in-memory base key directory, and reuses the ordinary exact-index
// bulk builder.
// There is one document/tape format and one query engine.
//
// The format is intentionally unstable before v1. All integers are
// little-endian. The image has this shape:
//
//	header | DocSet page 0 | ... | DocSet page N | manifest | footer
//
// The fixed footer locates and checksums the variable manifest. Page offsets
// and lengths are covered by that checksum; each nested DocSet image performs
// its own framing and manifest validation. Key bytes reside in the Store
// manifest so constructing the key directory never faults document payload
// pages.

const (
	storePersistVersion = 2

	storePersistHeaderMagic   = "SJSTORE1"
	storePersistManifestMagic = "SJSTMAN1"
	storePersistFooterMagic   = "SJSTFTR1"

	storePersistHeaderLen  = 16
	PersistManifestFixed   = 72
	storePersistChunkFixed = 32
	PersistFooterLen       = 40
)

const (
	storePersistFlagShapeTapes = 1 << iota
	storePersistFlagPostings
	storePersistFlagValueDict
	storePersistFlagHashKeys
)

const storePersistKnownFlags = storePersistFlagShapeTapes |
	storePersistFlagPostings |
	storePersistFlagValueDict |
	storePersistFlagHashKeys

var (
	// ErrCheckpointMagic reports data that is not a Store image.
	ErrCheckpointMagic = errors.New("vibejson: not a Store image")
	// ErrCheckpointVersion reports an image from an unsupported format
	// version. The pre-v1 representation intentionally makes no compatibility
	// promise across versions.
	ErrCheckpointVersion = errors.New("vibejson: unsupported Store image version")
	// ErrCheckpointCorrupt is the fail-closed result for malformed framing,
	// bounds, keys, slots, pages, indexes, TTL records, or checksums.
	ErrCheckpointCorrupt = errors.New("vibejson: corrupt Store image")
	// ErrCheckpointIndexBuilding requires callers to finish bounded online
	// backfill before taking a persistent snapshot. This prevents an image from
	// silently changing a Building index's coverage or latency contract.
	ErrCheckpointIndexBuilding = errors.New("vibejson: Store persistence requires ready indexes")
	// ErrCheckpointTooLarge reports metadata that exceeds the format's 32-bit
	// counts or lengths. Document payload bounds remain those of DocSet images.
	ErrCheckpointTooLarge = errors.New("vibejson: Store image metadata exceeds format bounds")
)

type storePersistChunkRef struct {
	id     uint32
	offset uint64
	length uint64
	chunk  *Chunk
}

type storePersistSnapshot struct {
	state     *State
	schema    *Schema
	deadlines []storeDeadline
	freeEmpty []uint32
}

// WriteTo writes one full immutable Store checkpoint to w. It is an export and
// restart primitive, not an incremental durability protocol: every live chunk
// is streamed on each call, and later mutations do not modify the image. The
// operation snapshots state and writer-side TTL/free metadata under the writer
// lock, then releases the lock before streaming any document bytes. Concurrent
// later mutations are not included and cannot change the captured graph.
//
// All declared indexes must be Ready. Open reconstructs exact roots with
// a fresh process-local hash seed and restores wildcard posting consumers over
// the page-local postings embedded in each chunk image.
func (s *Store) WriteTo(w io.Writer) (int64, error) {
	snapshot, err := s.storePersistSnapshot()
	if err != nil {
		return 0, err
	}
	state := snapshot.state
	pw := &persistWriter{w: w}

	var header [storePersistHeaderLen]byte
	copy(header[0:8], storePersistHeaderMagic)
	binary.LittleEndian.PutUint32(header[8:12], storePersistVersion)
	pw.writeSmall(header[:])

	refs := make([]storePersistChunkRef, 0, state.ChunkCount)
	state.Chunks.Each(func(id uint32, chunk *Chunk) bool {
		pw.pad8()
		start := pw.off
		_, writeErr := chunk.Docs.writeToNested(pw)
		if writeErr != nil && pw.err == nil {
			pw.err = writeErr
		}
		refs = append(refs, storePersistChunkRef{
			id: id, offset: uint64(start), length: uint64(pw.off - start), chunk: chunk,
		})
		return pw.err == nil
	})
	if pw.err != nil {
		return pw.off, pw.err
	}

	manifest, err := buildStorePersistManifest(
		state, snapshot.schema, refs,
		snapshot.deadlines, snapshot.freeEmpty,
	)
	if err != nil {
		return pw.off, err
	}
	pw.pad8()
	manifestOffset := uint64(pw.off)
	pw.write(manifest)

	var footer [PersistFooterLen]byte
	copy(footer[0:8], storePersistFooterMagic)
	binary.LittleEndian.PutUint64(footer[8:16], manifestOffset)
	binary.LittleEndian.PutUint64(footer[16:24], uint64(len(manifest)))
	binary.LittleEndian.PutUint64(footer[24:32], PersistChecksum(manifest))
	binary.LittleEndian.PutUint32(footer[32:36], storePersistVersion)
	pw.writeSmall(footer[:])
	runtime.KeepAlive(state)
	return pw.off, pw.err
}

func (s *Store) storePersistSnapshot() (storePersistSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.initLocked()
	if err != nil {
		return storePersistSnapshot{}, err
	}
	for _, info := range state.Indexes {
		if info.State != IndexReady {
			return storePersistSnapshot{}, fmt.Errorf("%w: %q", ErrCheckpointIndexBuilding, info.Name)
		}
	}
	if uint64(len(s.ttl.Heap)) > math.MaxUint32 || uint64(len(state.Indexes)) > math.MaxUint32 {
		return storePersistSnapshot{}, ErrCheckpointTooLarge
	}
	deadlines := make([]storeDeadline, len(s.ttl.Heap))
	for i, item := range s.ttl.Heap {
		loc := item.key.location()
		chunk := state.Chunks.Get(loc.Chunk)
		if chunk == nil || chunk.Live&(uint64(1)<<loc.Slot) == 0 {
			return storePersistSnapshot{}, fmt.Errorf("%w: TTL references absent row", ErrCheckpointCorrupt)
		}
		deadlines[i] = storeDeadline{key: chunk.Key(int(loc.Slot)), Deadline: item.Deadline}
	}
	slices.SortFunc(deadlines, func(a, b storeDeadline) int {
		if a.key < b.key {
			return -1
		}
		if a.key > b.key {
			return 1
		}
		return 0
	})
	freeEmpty := make([]uint32, 0, len(s.free.ids))
	for _, id := range s.free.ids {
		if state.Chunks.Get(id) == nil {
			freeEmpty = append(freeEmpty, id)
		}
	}
	slices.Sort(freeEmpty)
	return storePersistSnapshot{
		state: state, schema: s.options.Schema,
		deadlines: deadlines, freeEmpty: freeEmpty,
	}, nil
}

func buildStorePersistManifest(
	state *State,
	schema *Schema,
	refs []storePersistChunkRef,
	deadlines []storeDeadline,
	freeEmpty []uint32,
) ([]byte, error) {
	if state.Count < 0 || uint64(len(refs)) > math.MaxUint32 || uint64(len(freeEmpty)) > math.MaxUint32 {
		return nil, ErrCheckpointTooLarge
	}
	if uint32(len(refs)) != state.ChunkCount ||
		uint64(len(freeEmpty)) != uint64(state.Chunks.Count)-uint64(state.ChunkCount) {
		return nil, fmt.Errorf("%w: inconsistent chunk/free directory", ErrCheckpointCorrupt)
	}
	manifestSize, err := storePersistManifestSize(
		refs, state.Indexes, deadlines, freeEmpty, schema,
	)
	if err != nil {
		return nil, err
	}
	buf := make([]byte, PersistManifestFixed, manifestSize)
	copy(buf[0:8], storePersistManifestMagic)
	binary.LittleEndian.PutUint32(buf[8:12], storePersistVersion)
	binary.LittleEndian.PutUint32(buf[12:16], storeOptionsPersistFlags(state.StateOptions))
	binary.LittleEndian.PutUint64(buf[16:24], uint64(state.StateOptions.IndexOptions.MaxDepth))
	binary.LittleEndian.PutUint64(buf[24:32], state.Generation)
	binary.LittleEndian.PutUint64(buf[32:40], uint64(state.Count))
	binary.LittleEndian.PutUint32(buf[40:44], state.Chunks.Count)
	binary.LittleEndian.PutUint32(buf[44:48], uint32(len(refs)))
	binary.LittleEndian.PutUint32(buf[48:52], uint32(state.StateOptions.ChunkDocuments))
	binary.LittleEndian.PutUint32(buf[52:56], uint32(len(state.Indexes)))
	binary.LittleEndian.PutUint32(buf[56:60], uint32(len(deadlines)))
	binary.LittleEndian.PutUint32(buf[60:64], uint32(len(freeEmpty)))
	if schema != nil {
		binary.LittleEndian.PutUint32(buf[64:68], uint32(len(schema.fields)))
		binary.LittleEndian.PutUint16(buf[68:70], uint16(schema.root))
		for _, field := range schema.fields {
			buf = binary.LittleEndian.AppendUint32(
				buf, uint32(len(field.path)),
			)
			buf = binary.LittleEndian.AppendUint16(
				buf, uint16(field.types),
			)
			if field.required {
				buf = append(buf, 1)
			} else {
				buf = append(buf, 0)
			}
			buf = append(buf, 0)
			buf = append(buf, field.path...)
		}
	}

	for _, id := range freeEmpty {
		buf = binary.LittleEndian.AppendUint32(buf, id)
	}
	for _, ref := range refs {
		chunk := ref.chunk
		if chunk == nil || int(chunk.Count) != bits.OnesCount64(chunk.Live) {
			return nil, fmt.Errorf("%w: inconsistent chunk %d", ErrCheckpointCorrupt, ref.id)
		}
		buf = binary.LittleEndian.AppendUint32(buf, ref.id)
		buf = binary.LittleEndian.AppendUint32(buf, uint32(chunk.Count))
		buf = binary.LittleEndian.AppendUint64(buf, chunk.Live)
		buf = binary.LittleEndian.AppendUint64(buf, ref.offset)
		buf = binary.LittleEndian.AppendUint64(buf, ref.length)
		for live := chunk.Live; live != 0; live &= live - 1 {
			slot := bits.TrailingZeros64(live)
			key := chunk.Key(slot)
			if uint64(len(key)) > math.MaxUint32 {
				return nil, ErrCheckpointTooLarge
			}
			buf = append(buf, byte(slot), chunk.Ord[slot], 0, 0)
			buf = binary.LittleEndian.AppendUint32(buf, uint32(len(key)))
			buf = append(buf, key...)
		}
	}
	for _, info := range state.Indexes {
		if uint64(len(info.Name)) > math.MaxUint32 {
			return nil, ErrCheckpointTooLarge
		}
		buf = binary.LittleEndian.AppendUint32(buf, uint32(len(info.Name)))
		buf = append(buf, byte(info.Kind), info.ColumnCount, 0, 0)
		buf = append(buf, info.Name...)
		for i := 0; i < int(info.ColumnCount); i++ {
			path := info.Columns[i]
			if uint64(len(path)) > math.MaxUint32 {
				return nil, ErrCheckpointTooLarge
			}
			buf = binary.LittleEndian.AppendUint32(buf, uint32(len(path)))
			buf = append(buf, path...)
		}
	}
	for _, deadline := range deadlines {
		if uint64(len(deadline.key)) > math.MaxUint32 {
			return nil, ErrCheckpointTooLarge
		}
		buf = binary.LittleEndian.AppendUint32(buf, uint32(len(deadline.key)))
		buf = binary.LittleEndian.AppendUint32(buf, uint32(deadline.Deadline.nsec))
		buf = binary.LittleEndian.AppendUint64(buf, uint64(deadline.Deadline.sec))
		buf = append(buf, deadline.key...)
	}
	return buf, nil
}

// storePersistManifestSize computes one checked allocation for the manifest.
// Besides avoiding growth copies, this keeps WriteTo allocation proportional
// to the final metadata rather than to geometric capacity overshoot.
func storePersistManifestSize(
	refs []storePersistChunkRef,
	indexes []IndexInfo,
	deadlines []storeDeadline,
	freeEmpty []uint32,
	schema *Schema,
) (int, error) {
	size := uint64(PersistManifestFixed) + uint64(len(freeEmpty))*4
	add := func(n uint64) bool {
		if n > uint64(MaxInt())-size {
			return false
		}
		size += n
		return true
	}
	if schema != nil {
		if uint64(len(schema.fields)) > math.MaxUint32 {
			return 0, ErrCheckpointTooLarge
		}
		for _, field := range schema.fields {
			pathLen := uint64(len(field.path))
			if pathLen > math.MaxUint32 || !add(8+pathLen) {
				return 0, ErrCheckpointTooLarge
			}
		}
	}
	for _, ref := range refs {
		if ref.chunk == nil || !add(storePersistChunkFixed+uint64(ref.chunk.Count)*8) {
			return 0, ErrCheckpointTooLarge
		}
		for live := ref.chunk.Live; live != 0; live &= live - 1 {
			slot := bits.TrailingZeros64(live)
			keyLen := uint64(len(ref.chunk.Key(slot)))
			if keyLen > math.MaxUint32 || !add(keyLen) {
				return 0, ErrCheckpointTooLarge
			}
		}
	}
	for _, info := range indexes {
		nameLen := uint64(len(info.Name))
		if nameLen > math.MaxUint32 || !add(8+nameLen) {
			return 0, ErrCheckpointTooLarge
		}
		for i := 0; i < int(info.ColumnCount); i++ {
			pathLen := uint64(len(info.Columns[i]))
			if pathLen > math.MaxUint32 || !add(4+pathLen) {
				return 0, ErrCheckpointTooLarge
			}
		}
	}
	for _, deadline := range deadlines {
		keyLen := uint64(len(deadline.key))
		if keyLen > math.MaxUint32 || !add(16+keyLen) {
			return 0, ErrCheckpointTooLarge
		}
	}
	return int(size), nil
}

func storeOptionsPersistFlags(options StateOptions) uint32 {
	var flags uint32
	if options.ShapeTapes {
		flags |= storePersistFlagShapeTapes
	}
	if options.Postings {
		flags |= storePersistFlagPostings
	}
	if options.ValueDict {
		flags |= storePersistFlagValueDict
	}
	if options.IndexOptions.HashKeys {
		flags |= storePersistFlagHashKeys
	}
	return flags
}

// Open reconstructs a mutable Store over an immutable image produced by
// [Store.WriteTo]. Source bytes and native structural tapes borrow data; the
// caller must keep it immutable and, for mmap-backed data, mapped until the
// Store and every Snapshot or derived value are unreachable. The returned
// Store may immediately be updated, deleted from, assigned TTLs, or queried.
// Those mutations are heap publications and are not written back into data;
// call WriteTo for a later full checkpoint. Old mapped pages remain shared into
// later snapshots under the same lifetime contract.
//
// Opening validates all framing, manifest metadata, stable slots, nested page
// images, key uniqueness, index definitions, and TTL references before
// publication. Exact indexes are rebuilt through the same bulk constructor
// used by Builder, never a persistence-specific query structure.
func Open(data []byte) (*Store, error) {
	manifest, err := openStorePersistManifest(data)
	if err != nil {
		return nil, err
	}
	return manifest.open(data)
}

type storePersistManifest struct {
	bytes          []byte
	generation     uint64
	count          uint64
	chunkHighWater uint32
	liveChunks     uint32
	indexCount     uint32
	ttlCount       uint32
	freeCount      uint32
	schemaCount    uint32
	schemaRoot     SchemaType
	options        Options
	manifestOffset uint64
}

func openStorePersistManifest(data []byte) (storePersistManifest, error) {
	if uint64(len(data)) < storePersistHeaderLen+PersistFooterLen {
		return storePersistManifest{}, fmt.Errorf("%w: image shorter than framing", ErrCheckpointCorrupt)
	}
	if string(data[:8]) != storePersistHeaderMagic {
		return storePersistManifest{}, fmt.Errorf("%w: header magic", ErrCheckpointMagic)
	}
	if version := binary.LittleEndian.Uint32(data[8:12]); version != storePersistVersion {
		return storePersistManifest{}, fmt.Errorf("%w: header version %d != %d", ErrCheckpointVersion, version, storePersistVersion)
	}
	if binary.LittleEndian.Uint32(data[12:16]) != 0 {
		return storePersistManifest{}, fmt.Errorf("%w: header reserved field", ErrCheckpointCorrupt)
	}
	footer := data[len(data)-PersistFooterLen:]
	if string(footer[:8]) != storePersistFooterMagic {
		return storePersistManifest{}, fmt.Errorf("%w: footer magic", ErrCheckpointMagic)
	}
	if version := binary.LittleEndian.Uint32(footer[32:36]); version != storePersistVersion {
		return storePersistManifest{}, fmt.Errorf("%w: footer version %d != %d", ErrCheckpointVersion, version, storePersistVersion)
	}
	if binary.LittleEndian.Uint32(footer[36:40]) != 0 {
		return storePersistManifest{}, fmt.Errorf("%w: footer reserved field", ErrCheckpointCorrupt)
	}
	offset := binary.LittleEndian.Uint64(footer[8:16])
	length := binary.LittleEndian.Uint64(footer[16:24])
	checksum := binary.LittleEndian.Uint64(footer[24:32])
	limit := uint64(len(data) - PersistFooterLen)
	if offset < storePersistHeaderLen || length < PersistManifestFixed ||
		offset > limit || length > limit-offset {
		return storePersistManifest{}, fmt.Errorf("%w: manifest span", ErrCheckpointCorrupt)
	}
	manifest := data[offset : offset+length]
	if PersistChecksum(manifest) != checksum {
		return storePersistManifest{}, fmt.Errorf("%w: manifest checksum", ErrCheckpointCorrupt)
	}
	if string(manifest[:8]) != storePersistManifestMagic {
		return storePersistManifest{}, fmt.Errorf("%w: manifest magic", ErrCheckpointCorrupt)
	}
	if version := binary.LittleEndian.Uint32(manifest[8:12]); version != storePersistVersion {
		return storePersistManifest{}, fmt.Errorf("%w: manifest version", ErrCheckpointCorrupt)
	}
	flags := binary.LittleEndian.Uint32(manifest[12:16])
	if flags&^uint32(storePersistKnownFlags) != 0 {
		return storePersistManifest{}, fmt.Errorf("%w: unknown option flags", ErrCheckpointCorrupt)
	}
	schemaCount := binary.LittleEndian.Uint32(manifest[64:68])
	schemaRoot := SchemaType(binary.LittleEndian.Uint16(manifest[68:70]))
	if binary.LittleEndian.Uint16(manifest[70:72]) != 0 ||
		schemaRoot == 0 && schemaCount != 0 ||
		schemaRoot != 0 && (!validSchemaTypes(schemaRoot) ||
			schemaRoot != canonicalSchemaTypes(schemaRoot)) {
		return storePersistManifest{}, fmt.Errorf(
			"%w: manifest schema header", ErrCheckpointCorrupt,
		)
	}
	maxDepth64 := int64(binary.LittleEndian.Uint64(manifest[16:24]))
	maxDepth := int(maxDepth64)
	if int64(maxDepth) != maxDepth64 {
		return storePersistManifest{}, fmt.Errorf("%w: MaxDepth overflows int", ErrCheckpointCorrupt)
	}
	chunkDocuments := binary.LittleEndian.Uint32(manifest[48:52])
	options := Options{
		ChunkDocuments: int(chunkDocuments),
		IndexOptions: document.IndexOptions{
			MaxDepth: maxDepth, HashKeys: flags&storePersistFlagHashKeys != 0,
		},
		ShapeTapes: flags&storePersistFlagShapeTapes != 0,
		Postings:   flags&storePersistFlagPostings != 0,
		ValueDict:  flags&storePersistFlagValueDict != 0,
	}
	if _, err := options.Normalized(); err != nil {
		return storePersistManifest{}, fmt.Errorf("%w: %v", ErrCheckpointCorrupt, err)
	}
	return storePersistManifest{
		bytes:          manifest,
		generation:     binary.LittleEndian.Uint64(manifest[24:32]),
		count:          binary.LittleEndian.Uint64(manifest[32:40]),
		chunkHighWater: binary.LittleEndian.Uint32(manifest[40:44]),
		liveChunks:     binary.LittleEndian.Uint32(manifest[44:48]),
		options:        options,
		indexCount:     binary.LittleEndian.Uint32(manifest[52:56]),
		ttlCount:       binary.LittleEndian.Uint32(manifest[56:60]),
		freeCount:      binary.LittleEndian.Uint32(manifest[60:64]),
		schemaCount:    schemaCount,
		schemaRoot:     schemaRoot,
		manifestOffset: offset,
	}, nil
}

func (m storePersistManifest) open(data []byte) (*Store, error) {
	if m.count > uint64(MaxInt()) || m.liveChunks > m.chunkHighWater ||
		uint64(m.freeCount) != uint64(m.chunkHighWater)-uint64(m.liveChunks) {
		return nil, fmt.Errorf("%w: impossible Store counts", ErrCheckpointCorrupt)
	}
	if m.count < uint64(m.liveChunks) ||
		m.count > uint64(m.liveChunks)*uint64(m.options.ChunkDocuments) {
		return nil, fmt.Errorf("%w: document/chunk counts", ErrCheckpointCorrupt)
	}
	// Every variable record has a fixed minimum width. Prove all attacker-
	// controlled counts against bytes already present before using any count as
	// a slice or map capacity. Variable key/name/path bytes only increase the
	// requirement, so the later reader checks close the remaining bounds.
	variableBytes := uint64(len(m.bytes) - PersistManifestFixed)
	minimumBytes := uint64(m.schemaCount)*8 +
		uint64(m.freeCount)*4 +
		uint64(m.liveChunks)*storePersistChunkFixed +
		m.count*8 + uint64(m.indexCount)*8 +
		uint64(m.ttlCount)*16
	if minimumBytes > variableBytes {
		return nil, fmt.Errorf("%w: counts exceed manifest bytes", ErrCheckpointCorrupt)
	}
	r := persistReader{b: m.bytes, pos: PersistManifestFixed, ok: true}
	schema, err := m.openSchema(&r)
	if err != nil {
		return nil, err
	}
	m.options.Schema = schema
	freeEmpty := make([]uint32, m.freeCount)
	for i := range freeEmpty {
		freeEmpty[i] = r.u32()
		if !r.ok || freeEmpty[i] >= m.chunkHighWater || i > 0 && freeEmpty[i] <= freeEmpty[i-1] {
			return nil, fmt.Errorf("%w: empty chunk ids", ErrCheckpointCorrupt)
		}
	}

	seed := maphash.MakeSeed()
	state := &State{
		Generation:   m.generation,
		Count:        int(m.count),
		ChunkCount:   m.liveChunks,
		seed:         seed,
		StateOptions: m.options.stateOptions(),
		source:       data,
		Chunks:       storeChunkVectorWithHighWater(m.chunkHighWater),
	}
	baseKeys, err := newStoreMappedKeys(m.bytes, int(m.count), m.chunkHighWater >= storeMappedLocationMaxChunk)
	if err != nil {
		return nil, fmt.Errorf("vibejson: OpenStore key directory: %w", err)
	}
	state.baseKeys = baseKeys
	opened := false
	defer func() {
		if opened {
			return
		}
		baseKeys.release()
		state.mappedDocs.release()
	}()
	if persistNativeLittleEndian {
		mappedDocs, allocErr := newStoreMappedDocs(int(m.count))
		if allocErr != nil {
			return nil, fmt.Errorf("vibejson: OpenStore document directory: %w", allocErr)
		}
		state.mappedDocs = mappedDocs
		state.mappedDocChunks = m.liveChunks
	}
	store := &Store{Options: m.options, options: m.options}
	store.free.pos = make(map[uint32]int)
	store.postingChunks.pos = make(map[uint32]int)

	var seenKeys int
	var previousEnd uint64 = storePersistHeaderLen
	var previousID uint32
	for n := uint32(0); n < m.liveChunks; n++ {
		fixed := r.bytes(storePersistChunkFixed)
		if !r.ok {
			return nil, fmt.Errorf("%w: chunk %d metadata", ErrCheckpointCorrupt, n)
		}
		id := binary.LittleEndian.Uint32(fixed[0:4])
		count := binary.LittleEndian.Uint32(fixed[4:8])
		live := binary.LittleEndian.Uint64(fixed[8:16])
		offset := binary.LittleEndian.Uint64(fixed[16:24])
		length := binary.LittleEndian.Uint64(fixed[24:32])
		if id >= m.chunkHighWater || n > 0 && id <= previousID || count == 0 ||
			count > uint32(m.options.ChunkDocuments) || count != uint32(bits.OnesCount64(live)) {
			return nil, fmt.Errorf("%w: chunk %d identity/live mask", ErrCheckpointCorrupt, id)
		}
		if offset < previousEnd || offset&7 != 0 || offset > m.manifestOffset || length > m.manifestOffset-offset {
			return nil, fmt.Errorf("%w: chunk %d image span", ErrCheckpointCorrupt, id)
		}

		chunk := &Chunk{
			mappedKeys: baseKeys,
			mappedBase: uint64(seenKeys),
			Live:       live,
			Count:      uint8(count),
		}
		var slotsSeen, ordSeen uint64
		for i := uint32(0); i < count; i++ {
			keyHeader := r.bytes(8)
			if !r.ok {
				return nil, fmt.Errorf("%w: chunk %d key header", ErrCheckpointCorrupt, id)
			}
			slot, ord := keyHeader[0], keyHeader[1]
			keyLen := binary.LittleEndian.Uint32(keyHeader[4:8])
			keyOffset := r.pos
			keyBytes := r.bytes(uint64(keyLen))
			bit := uint64(1) << slot
			ordBit := uint64(1) << ord
			if !r.ok || keyHeader[2] != 0 || keyHeader[3] != 0 ||
				int(slot) >= m.options.ChunkDocuments || int(ord) >= int(count) ||
				live&bit == 0 || slotsSeen&bit != 0 || ordSeen&ordBit != 0 {
				return nil, fmt.Errorf("%w: chunk %d key slot/ordinal", ErrCheckpointCorrupt, id)
			}
			key := byteview.String(keyBytes)
			hash := maphash.String(seed, key)
			ref := uint64(seenKeys)
			baseKeys.setKeySpan(ref, keyOffset, keyLen)
			baseKeys.setLocation(ref, Location{Chunk: id, Slot: slot})
			if inserted := baseKeys.insert(hash, ref); !inserted {
				return nil, fmt.Errorf("%w: duplicate key %q", ErrCheckpointCorrupt, key)
			}
			chunk.Ord[slot] = ord
			slotsSeen |= bit
			ordSeen |= ordBit
			seenKeys++
		}
		if slotsSeen != live || ordSeen != lowBits64(count) {
			return nil, fmt.Errorf("%w: chunk %d incomplete slot map", ErrCheckpointCorrupt, id)
		}
		page := data[offset : offset+length]
		var openErr error
		if state.mappedDocs != nil {
			openErr = openDocSetIntoStore(&chunk.Docs, page, state.mappedDocs, uint64(seenKeys)-uint64(count))
		} else {
			openErr = openDocSetInto(&chunk.Docs, page)
		}
		if openErr != nil {
			return nil, fmt.Errorf("%w: chunk %d: %v", ErrCheckpointCorrupt, id, openErr)
		}
		if chunk.Docs.Len() != int(count) || chunk.Docs.ShapeTapes != m.options.ShapeTapes ||
			chunk.Docs.ValueDict != m.options.ValueDict || chunk.Docs.Options != m.options.IndexOptions ||
			m.options.Postings && !chunk.Docs.Postings {
			return nil, fmt.Errorf("%w: chunk %d option/document mismatch", ErrCheckpointCorrupt, id)
		}
		if schema := m.options.Schema; schema != nil {
			var rows [MaxChunkDocuments]int
			var values [MaxChunkDocuments]vibejson.RawValue
			for row := 0; row < int(count); row++ {
				rows[row] = row
			}
			failed, schemaErr := schema.ValidateDocSetRows(
				&chunk.Docs, rows[:count], values[:0],
			)
			if schemaErr != nil {
				return nil, fmt.Errorf(
					"%w: chunk %d row %d violates schema: %v",
					ErrCheckpointCorrupt, id, failed, schemaErr,
				)
			}
		}
		storeChunkSetTransient(&state.Chunks.root, state.Chunks.depth, id, chunk)
		if int(count) < m.options.ChunkDocuments {
			store.free.add(id)
		}
		if chunk.Docs.Postings {
			store.postingChunks.add(id)
		}
		previousID, previousEnd = id, offset+length
	}
	if uint64(seenKeys) != m.count {
		return nil, fmt.Errorf("%w: key count", ErrCheckpointCorrupt)
	}
	for _, id := range freeEmpty {
		if state.Chunks.Get(id) != nil {
			return nil, fmt.Errorf("%w: live chunk %d marked empty", ErrCheckpointCorrupt, id)
		}
		store.free.add(id)
	}

	if err := m.openIndexes(&r, store, state); err != nil {
		return nil, err
	}
	if err := m.openDeadlines(&r, store, state); err != nil {
		return nil, err
	}
	if !r.ok || r.pos != uint64(len(r.b)) {
		return nil, fmt.Errorf("%w: trailing or truncated manifest data", ErrCheckpointCorrupt)
	}
	if !m.options.Postings && !store.hasPostingsIndexLocked() && len(store.postingChunks.ids) != 0 {
		store.reclaim = &storeIndexReclaim{}
	}
	store.state.Store(state)
	opened = true
	return store, nil
}

// openSchema reconstructs the immutable collection constraint before any
// document or secondary-index state is published. The declarative records are
// canonical compiler input rather than a second persistence-only schema
// representation, so all entry points share identical path and type rules.
func (m storePersistManifest) openSchema(
	r *persistReader,
) (*Schema, error) {
	if m.schemaRoot == 0 {
		return nil, nil
	}
	fields := make([]SchemaField, m.schemaCount)
	var previousPath string
	for i := range fields {
		header := r.bytes(8)
		if !r.ok {
			return nil, fmt.Errorf(
				"%w: schema field %d header",
				ErrCheckpointCorrupt, i,
			)
		}
		pathLen := binary.LittleEndian.Uint32(header[0:4])
		types := SchemaType(binary.LittleEndian.Uint16(header[4:6]))
		required := header[6]
		path := r.bytes(uint64(pathLen))
		pathString := byteview.String(path)
		if !r.ok || !validSchemaTypes(types) ||
			types != canonicalSchemaTypes(types) ||
			required > 1 || header[7] != 0 || len(path) == 0 {
			return nil, fmt.Errorf(
				"%w: schema field %d",
				ErrCheckpointCorrupt, i,
			)
		}
		if i != 0 && previousPath >= pathString {
			return nil, fmt.Errorf(
				"%w: schema field %d order",
				ErrCheckpointCorrupt, i,
			)
		}
		fields[i] = SchemaField{
			Path:     pathString,
			Types:    types,
			Required: required != 0,
		}
		previousPath = pathString
	}
	schema, err := CompileSchema(SchemaDefinition{
		Root: m.schemaRoot, Fields: fields,
	})
	if err != nil {
		return nil, fmt.Errorf(
			"%w: schema: %v", ErrCheckpointCorrupt, err,
		)
	}
	return schema, nil
}

func (m storePersistManifest) openIndexes(r *persistReader, store *Store, state *State) error {
	var exact map[string]*ExactIndex
	names := make(map[string]struct{}, m.indexCount)
	if m.indexCount != 0 {
		store.indexes = make(map[string]*storeIndexBuild, m.indexCount)
	}
	for i := uint32(0); i < m.indexCount; i++ {
		header := r.bytes(8)
		if !r.ok {
			return fmt.Errorf("%w: index %d header", ErrCheckpointCorrupt, i)
		}
		nameLen := binary.LittleEndian.Uint32(header[:4])
		kind := IndexKind(header[4])
		columns := int(header[5])
		nameBytes := r.bytes(uint64(nameLen))
		if !r.ok || header[6] != 0 || header[7] != 0 || len(nameBytes) == 0 {
			return fmt.Errorf("%w: index %d name", ErrCheckpointCorrupt, i)
		}
		name := string(nameBytes)
		if _, duplicate := names[name]; duplicate {
			return fmt.Errorf("%w: duplicate index %q", ErrCheckpointCorrupt, name)
		}
		names[name] = struct{}{}
		switch kind {
		case IndexPostings:
			if columns != 0 {
				return fmt.Errorf("%w: postings index %q has columns", ErrCheckpointCorrupt, name)
			}
			if uint32(len(store.postingChunks.ids)) != state.ChunkCount {
				return fmt.Errorf("%w: postings index %q lacks page coverage", ErrCheckpointCorrupt, name)
			}
			store.indexes[name] = &storeIndexBuild{info: IndexInfo{
				Name: name, Kind: kind, State: IndexReady,
				CoveredChunks: state.ChunkCount, TotalChunks: state.ChunkCount,
			}, all: true}
		case IndexExact:
			if columns < 1 || columns > MaxIndexColumns {
				return fmt.Errorf("%w: exact index %q arity", ErrCheckpointCorrupt, name)
			}
			paths := make([]string, columns)
			for column := range paths {
				pathLen := r.u32()
				path := r.bytes(uint64(pathLen))
				if !r.ok {
					return fmt.Errorf("%w: index %q path %d", ErrCheckpointCorrupt, name, column)
				}
				paths[column] = string(path)
			}
			compiled, err := CompileExactIndex(IndexDefinition{Name: name, Paths: paths})
			if err != nil {
				return fmt.Errorf("%w: index %q: %v", ErrCheckpointCorrupt, name, err)
			}
			compiled.seed = state.seed
			if exact == nil {
				exact = make(map[string]*ExactIndex)
			}
			exact[name] = compiled
		default:
			return fmt.Errorf("%w: index %q kind %d", ErrCheckpointCorrupt, name, kind)
		}
	}
	if len(exact) != 0 {
		builder := Builder{exact: exact}
		if err := builder.buildExactIndexes(store, state); err != nil {
			return err
		}
	} else {
		state.Indexes = store.indexInfosLocked()
		state.secondary = store.indexSnapshotsLocked()
	}
	return nil
}

func (m storePersistManifest) openDeadlines(r *persistReader, store *Store, state *State) error {
	var previous string
	for i := uint32(0); i < m.ttlCount; i++ {
		header := r.bytes(16)
		if !r.ok {
			return fmt.Errorf("%w: TTL %d header", ErrCheckpointCorrupt, i)
		}
		keyLen := binary.LittleEndian.Uint32(header[:4])
		nsec := binary.LittleEndian.Uint32(header[4:8])
		sec := int64(binary.LittleEndian.Uint64(header[8:16]))
		keyBytes := r.bytes(uint64(keyLen))
		if !r.ok || nsec >= 1_000_000_000 {
			return fmt.Errorf("%w: TTL %d value", ErrCheckpointCorrupt, i)
		}
		key := byteview.String(keyBytes)
		if i > 0 && key <= previous {
			return fmt.Errorf("%w: TTL keys not unique/sorted", ErrCheckpointCorrupt)
		}
		hash := maphash.String(state.seed, key)
		_, loc, ok := storeStateKeyLookupChunk(state, hash, key)
		if !ok {
			return fmt.Errorf("%w: TTL key %q missing", ErrCheckpointCorrupt, key)
		}
		store.ttl.upsert(TTLKeyOf(loc), Instant{sec: sec, nsec: int32(nsec)})
		previous = key
	}
	return nil
}

func storeChunkVectorWithHighWater(count uint32) storeChunkVector {
	v := storeChunkVector{Count: count}
	if count == 0 {
		return v
	}
	maxID := uint64(count - 1)
	for maxID >= uint64(32)<<(uint(v.depth)*5) {
		v.depth++
	}
	return v
}

func lowBits64(n uint32) uint64 {
	if n >= 64 {
		return ^uint64(0)
	}
	return uint64(1)<<n - 1
}

func MaxInt() int { return int(^uint(0) >> 1) }
