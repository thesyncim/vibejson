package durable

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"math/bits"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	vibejson "github.com/thesyncim/vibejson"
	"github.com/thesyncim/vibejson/document"
	"github.com/thesyncim/vibejson/internal/storeio"
	"github.com/thesyncim/vibejson/internal/storemem"
	"github.com/thesyncim/vibejson/store"
)

var (
	// ErrClosed reports use after Collection.Close has started.
	ErrClosed = errors.New("vibejson: collection is closed")
	// ErrNotEmpty requires Create to receive an empty file.
	ErrNotEmpty = errors.New("vibejson: collection create requires an empty file")
	// ErrKeyTooLarge reports a key beyond the configured durable page
	// bound.
	ErrKeyTooLarge = errors.New("vibejson: collection key exceeds configured bound")
	// ErrDocumentTooLarge reports a JSON value beyond the configured
	// transaction bound.
	ErrDocumentTooLarge = errors.New("vibejson: collection document exceeds configured bound")
	// ErrDeadlineRange reports a deadline outside the durable signed
	// Unix-nanosecond representation.
	ErrDeadlineRange = errors.New("vibejson: collection deadline is outside Unix-nanosecond range")
	// ErrWriterLocked reports that another mutable collection owns the
	// page file. A durable file has exactly one generation publisher.
	ErrWriterLocked = storeio.ErrWriterLocked
	// ErrWriterLockUnsupported rejects mutable durable collections on a
	// platform without a safe exclusive file-lock implementation.
	ErrWriterLockUnsupported = storeio.ErrWriterLockUnsupported
)

// Backend selects the durable commit and speculative-read engines.
type Backend uint8

const (
	BackendAuto Backend = iota
	BackendPortable
	BackendIOUring
)

// ReadMode selects how cache misses reach the file. Direct modes are
// Linux-only and leave the caller's descriptor untouched.
type ReadMode uint8

const (
	ReadBuffered ReadMode = iota
	// ReadDirectTry uses O_DIRECT when the platform and filesystem
	// accept it, otherwise Stats reports the observable buffered fallback.
	ReadDirectTry
	// ReadDirectRequire fails construction rather than falling back.
	ReadDirectRequire
)

// WriteMode selects how durable page commits reach the file. Direct
// modes are Linux-only and use an independently owned descriptor, so the
// caller's descriptor flags and file offset remain untouched.
type WriteMode uint8

const (
	WriteBuffered WriteMode = iota
	// WriteDirectTry uses O_DIRECT when the platform and filesystem
	// accept it, otherwise Stats reports the observable buffered fallback.
	WriteDirectTry
	// WriteDirectRequire fails construction rather than falling back.
	WriteDirectRequire
)

// Options fixes every collection-owned resident and in-flight memory
// bound. The zero value selects 4 KiB metadata pages, 64 KiB
// document/overflow extents, a 64 MiB read cache, and 4 MiB maximum documents.
type Options struct {
	Collection store.Options
	// Indexes are frozen exact scalar definitions maintained from the first
	// durable generation. Their order assigns stable on-disk index IDs.
	Indexes []store.IndexDefinition
	// Float64Columns are frozen RFC 6901 paths stored beside each document
	// micro-page as typed covering columns. Predicate-free numeric aggregates
	// can reduce these values without parsing JSON. Missing, non-numeric, and
	// non-finite values are omitted from the column.
	Float64Columns []string

	PageSize      int
	MaxPageSize   int
	ResidentBytes int64
	// ReadConcurrency bounds portable positional-read workers.
	ReadConcurrency int
	// ReadQueueDepth bounds one native asynchronous read submission.
	ReadQueueDepth int
	// PrefetchQueue bounds references waiting for either read engine.
	PrefetchQueue    int
	MaxKeyBytes      int
	InlineValueBytes int
	MaxDocumentBytes int
	// BufferCount is the number of equal, MaxPageSize-wide staging buffers the
	// durability device owns. It is not a throughput dial with a smooth curve;
	// it is how many generations may be in flight at once, and that quantity is
	// integer-valued and small.
	//
	// One transaction reserves the worst case for the configured
	// MaxDocumentBytes — every overflow page a maximum document could need,
	// plus the directory pages, plus one root — before it writes anything. Call
	// that R. The achievable depth is therefore BufferCount/R, and at the
	// default geometry (MaxDocumentBytes 4 MiB, MaxPageSize 64 KiB) R is 114.
	// Sizing the pool to merely exceed R, as this option used to do, buys depth
	// exactly one: a serialized writer cannot begin its next Put until the
	// previous one's buffers come back, which happens only after that Put's
	// durability fence. Every Put then pays a full fence even with
	// Synchronous=false, which is the single most expensive thing this package
	// can be misconfigured into.
	//
	// Zero selects a pool deep enough for four worst-case transactions, capped
	// at 32 MiB of staging. Measured on an Apple M4 Max, one serialized writer,
	// Synchronous=false, 300 Puts of a ~60 byte document into a growing store
	// (BenchmarkFileStorePutCommitBuffers):
	//
	//	buffers   staging    ns/Put    Puts per fsync
	//	    128      8 MiB   3.69 ms              1.42
	//	    256     16 MiB    512 µs              10.7
	//	    512     32 MiB    197 µs              25.0   <- zero selects this
	//	   1024     64 MiB    169 µs              27.3
	//	   2048    128 MiB    182 µs              27.3
	//
	// The knee is real: 512 is 19x faster than the old default for 24 MiB more,
	// and is where the achieved group size saturates against GroupLimit.
	// Doubling again buys 14% for another 32 MiB, and doubling a third time
	// buys nothing at all. A caller who is short of address space rather than
	// throughput should set this explicitly and expect the table above; a
	// caller with an unusually large MaxDocumentBytes already exceeds the
	// 32 MiB cap at depth one and keeps the frugal geometry unchanged.
	BufferCount int
	QueueSlots  int
	// GroupLimit caps how many adjacent generations share one durability
	// fence; zero selects 32. It is a ceiling, not a target, and measurement
	// shows it is almost never the binding constraint: how many generations are
	// queued when the worker picks one up is. Raising it from 2 to 64 changes
	// neither the achieved group size nor throughput on any writer shape tested.
	// Reach for CommitCoalesce instead.
	GroupLimit int
	// CommitCoalesce bounds how long the background durability worker waits
	// after taking one generation for adjacent ones to join it. Zero, the
	// default, commits each generation as soon as it is picked up.
	//
	// The window is only entered when another generation is already queued or a
	// producer is mid-transaction, so a lone synchronous writer — whose next Put
	// cannot start until this one is durable — never pays it. When grouping is
	// possible the cost is real and bounded: a Synchronous caller's
	// acknowledgement is delayed by up to this duration so that its fence can be
	// shared. On an Apple M4 Max, where one file.Sync costs several
	// milliseconds, a 1 ms window took roughly three generations per fence
	// instead of one and a half, cutting per-operation cost by about a third
	// with eight concurrent writers. It is left at zero by default because that
	// trade belongs to the caller: it buys throughput with acknowledged-commit
	// latency, and only a caller knows which it is short of.
	CommitCoalesce time.Duration
	// Backend selects both engines; Stats reports the actual read and write
	// choices independently after Auto fallback.
	Backend Backend
	// ReadMode controls cache-miss reads independently from durable writes.
	// DirectTry has observable fallback; DirectRequire fails when unavailable.
	ReadMode ReadMode
	// WriteMode controls durable data and root writes independently from cache
	// misses. Direct modes keep sustained ingestion out of the kernel page
	// cache while retaining the same ordered durability barriers.
	WriteMode         WriteMode
	Synchronous       bool
	MaxSnapshotLeases int
	// MaxRetiredExtents bounds the copy-on-write extents held back from reuse
	// because some reader might still dereference them. Zero selects 65,536.
	//
	// This is the bound that decides how long a Snapshot may be held open while
	// the collection is being written. An extent retired at generation G cannot
	// be reused until no lease sits at or below G, so one snapshot held across a
	// write loop pins everything retired after it was taken. A replacement Put
	// retires roughly three extents at the default geometry, so the default
	// bound is reached after roughly twenty thousand replacements, after which
	// writes fail with ErrRetiredExtentCapacity.
	//
	// That failure is bounded backpressure and is fully recoverable: closing the
	// snapshot lets the next commit drain the pending set and the writer
	// resumes with no restart and nothing lost. It is not a wedge. But a reader
	// that keeps one snapshot for the lifetime of a long-lived request handler
	// will meet it, so take snapshots per query rather than per connection, or
	// raise this bound and accept the proportional tracking memory.
	//
	// The bound never fails a write that no reader is responsible for. When the
	// table fills with nothing pinning it, the extents are unreachable and only
	// their bookkeeping is missing, so they are abandoned rather than tracked:
	// the file grows, Stats reports AbandonedExtents, and the writer continues.
	// That case used to fail too, and the only cure was restarting the process,
	// which discarded the same metadata anyway.
	MaxRetiredExtents int
	// MaxBatchDocuments bounds how many distinct keys one Update may mutate;
	// zero selects store.MaxChunkDocuments. It sizes the durable transaction's
	// worst-case page reservation, so raising it raises the staging arena's
	// address-space reservation (lazily backed on every Unix, eagerly allocated
	// elsewhere) and lowers nothing. Update reports ErrBatchTooLarge rather than
	// silently splitting: a batch that spans two commits is not the atomic unit
	// its caller asked for, and a crash between them would publish half of it.
	MaxBatchDocuments int
}

// batchMetadataPages is the worst-case non-overflow page reservation for one
// batched publication. Each term names the structure it pays for:
//
//   - one rebuilt document page per chunk the batch touches, plus one for a
//     chunk it creates;
//   - one root-to-leaf chunk-directory copy per touched chunk, applied one
//     chunk at a time because the radix directory has no batched descent;
//   - one batched key-directory descent over every mutated key;
//   - one batched index-directory descent per configured index, over at most
//     two routing edits per document, because a replaced value leaves one
//     posting and joins another;
//   - one batched TTL descent over every document the batch may delete, the
//     cost a batch of deletes over deadline-bearing rows pays;
//   - the free log's fold ceiling and the publication root.
//
// It is deliberately a reservation and not an invariant. A pathological tree
// shape can exceed it, in which case the transaction's allocator refuses and
// Update returns ErrBatchTooLarge with nothing published; the caller retries
// with a smaller batch. Making it exact would require reserving for a
// ten-level directory over every key, which is hundreds of times the pages any
// real store uses.
func batchMetadataPages(o Options, indexes int) int {
	documents := o.MaxBatchDocuments
	chunks := (documents+o.Collection.ChunkDocuments-1)/o.Collection.ChunkDocuments + 1
	pages := chunks
	pages += chunks * storeio.ChunkTreePages()
	pages += storeio.KeyTreeBatchPages(documents)
	if indexes != 0 {
		pages += indexes * storeio.IndexTreeBatchPages(2*documents)
	}
	pages += storeio.TTLTreeBatchRemovalPages(documents)
	pages += fileStoreMetadataReservePages
	return pages
}

// fileStoreMetadataReservePages is the fixed share of a transaction's page
// reservation that is not proportional to the batch: the state root, the
// superblock, and the free log's worst commit. The free log's part is
// FreeLogMaxIndexPages + FreeLogMaxFoldSegments + FreeLogMaxDeltaPages, so a
// fold that dirties the maximum number of segments still fits inside a
// reservation taken before the fold knows what it will touch. Both the
// single-document and batched geometries spend it, which is why it is named
// once rather than repeated as a literal in each.
const fileStoreMetadataReservePages = 56

type normalizedFileStoreOptions struct {
	Options
	maxTransactionPages int
	maxTransactionBytes uint64
	indexes             []*store.ExactIndex
	float64Columns      []fileStoreFloat64Column
	indexCatalogHash    uint64
}

const fileStoreMaxFloat64Columns = 256

type fileStoreFloat64Column struct {
	spec    string
	pointer vibejson.CompiledPointer
}

func (o Options) normalized() (normalizedFileStoreOptions, error) {
	storeOptions, err := o.Collection.Normalized()
	if err != nil {
		return normalizedFileStoreOptions{}, err
	}
	o.Collection = storeOptions
	if o.PageSize == 0 {
		o.PageSize = 4096
	}
	if o.MaxPageSize == 0 {
		o.MaxPageSize = 64 << 10
	}
	if o.ResidentBytes == 0 {
		o.ResidentBytes = 64 << 20
	}
	if o.ReadConcurrency == 0 {
		o.ReadConcurrency = 4
	}
	if o.PrefetchQueue == 0 {
		o.PrefetchQueue = 64
	}
	if o.ReadQueueDepth == 0 {
		o.ReadQueueDepth = o.PrefetchQueue
	}
	if o.MaxKeyBytes == 0 {
		o.MaxKeyBytes = 256
	}
	if o.InlineValueBytes == 0 {
		o.InlineValueBytes = 512
	}
	if o.MaxDocumentBytes == 0 {
		o.MaxDocumentBytes = 4 << 20
	}
	if o.MaxSnapshotLeases == 0 {
		o.MaxSnapshotLeases = 1024
	}
	if o.MaxRetiredExtents == 0 {
		o.MaxRetiredExtents = 1 << 16
	}
	if o.MaxBatchDocuments == 0 {
		o.MaxBatchDocuments = store.MaxChunkDocuments
	}
	if o.MaxBatchDocuments < 1 {
		return normalizedFileStoreOptions{}, fmt.Errorf("vibejson: collection MaxBatchDocuments must be positive")
	}
	if o.Backend > BackendIOUring || o.ReadMode > ReadDirectRequire ||
		o.WriteMode > WriteDirectRequire ||
		o.CommitCoalesce < 0 || o.CommitCoalesce > time.Second ||
		o.PageSize < 4096 || o.PageSize&(o.PageSize-1) != 0 ||
		o.MaxPageSize < o.PageSize || o.MaxPageSize&(o.MaxPageSize-1) != 0 || o.MaxPageSize%o.PageSize != 0 ||
		o.MaxKeyBytes < 1 || o.InlineValueBytes < 1 || o.MaxDocumentBytes < 1 ||
		o.InlineValueBytes > o.MaxDocumentBytes || uint64(o.MaxPageSize) > uint64(^uint32(0)) ||
		o.ReadConcurrency < 1 || o.ReadConcurrency > 32768 ||
		o.ReadQueueDepth < 1 || o.ReadQueueDepth > 32768 ||
		o.PrefetchQueue < 1 || o.PrefetchQueue > 32768 {
		return normalizedFileStoreOptions{}, fmt.Errorf("vibejson: invalid Store page, key, value, backend, or read option")
	}
	if len(o.Indexes) > 64 {
		return normalizedFileStoreOptions{}, fmt.Errorf("%w: collection supports at most 64 indexes", store.ErrIndexDefinition)
	}
	if len(o.Float64Columns) > fileStoreMaxFloat64Columns {
		return normalizedFileStoreOptions{}, fmt.Errorf(
			"vibejson: collection supports at most %d float64 columns", fileStoreMaxFloat64Columns,
		)
	}
	compiled := make([]*store.ExactIndex, len(o.Indexes))
	definitions := make([]store.IndexDefinition, len(o.Indexes))
	seenIndexes := make(map[string]struct{}, len(o.Indexes))
	catalogHash := uint64(14695981039346656037)
	for i, definition := range o.Indexes {
		if _, exists := seenIndexes[definition.Name]; exists {
			return normalizedFileStoreOptions{}, store.ErrIndexExists
		}
		exact, compileErr := store.CompileExactIndex(definition)
		if compileErr != nil {
			return normalizedFileStoreOptions{}, compileErr
		}
		seenIndexes[definition.Name] = struct{}{}
		compiled[i] = exact
		definitions[i] = store.IndexDefinition{Name: exactName(exact, definition.Name), Paths: make([]string, exact.N)}
		copy(definitions[i].Paths, exact.Specs[:exact.N])
		catalogHash = fileIndexHashBytes(catalogHash, []byte(definitions[i].Name))
		catalogHash = fileIndexHashBytes(catalogHash, []byte{0xff, byte(exact.N)})
		for _, path := range definitions[i].Paths {
			catalogHash = fileIndexHashBytes(catalogHash, []byte(path))
			catalogHash = fileIndexHashBytes(catalogHash, []byte{0})
		}
	}
	o.Indexes = definitions
	columns := make([]fileStoreFloat64Column, len(o.Float64Columns))
	columnSpecs := make([]string, len(o.Float64Columns))
	seenColumns := make(map[string]struct{}, len(o.Float64Columns))
	for i, spec := range o.Float64Columns {
		owned := strings.Clone(spec)
		if _, exists := seenColumns[owned]; exists {
			return normalizedFileStoreOptions{}, fmt.Errorf(
				"%w: duplicate float64 column %q", store.ErrIndexDefinition, owned,
			)
		}
		pointer, compileErr := vibejson.CompilePointer(owned)
		if compileErr != nil {
			return normalizedFileStoreOptions{}, fmt.Errorf(
				"%w: float64 column %d: %v", store.ErrIndexDefinition, i, compileErr,
			)
		}
		seenColumns[owned] = struct{}{}
		columns[i] = fileStoreFloat64Column{spec: owned, pointer: pointer}
		columnSpecs[i] = owned
	}
	o.Float64Columns = columnSpecs
	if len(columns) != 0 {
		catalogHash = fileIndexHashBytes(catalogHash, []byte{0xfc, 0x64})
		for _, column := range columns {
			catalogHash = fileIndexHashBytes(catalogHash, []byte(column.spec))
			catalogHash = fileIndexHashBytes(catalogHash, []byte{0})
		}
	}
	if o.Collection.Schema != nil {
		catalogHash = fileIndexHashBytes(
			catalogHash, []byte{0x53, 0x43, 0x48},
		)
		var identity [8]byte
		binary.LittleEndian.PutUint64(
			identity[:], o.Collection.Schema.Hash,
		)
		catalogHash = fileIndexHashBytes(
			catalogHash, identity[:],
		)
	}
	if len(compiled) == 0 && len(columns) == 0 &&
		o.Collection.Schema == nil {
		catalogHash = 0
	}
	maxRowBytes := o.MaxKeyBytes + max(o.InlineValueBytes, storeio.DocumentOverflowDescriptorSize)
	worstDocumentPage := storeio.PageHeaderSize + storeio.PageTrailerSize + storeio.DocumentPagePayloadHeaderSize +
		o.Collection.ChunkDocuments*storeio.DocumentPageRecordSize + o.Collection.ChunkDocuments*maxRowBytes +
		len(columns)*(8+o.Collection.ChunkDocuments*8)
	if worstDocumentPage > o.MaxPageSize {
		return normalizedFileStoreOptions{}, fmt.Errorf("vibejson: collection MaxPageSize cannot hold configured chunk/key/inline bounds")
	}
	overflowPayload := o.MaxPageSize - storeio.PageHeaderSize - storeio.PageTrailerSize - storeio.OverflowPagePayloadHeaderSize
	if overflowPayload <= 0 {
		return normalizedFileStoreOptions{}, fmt.Errorf("vibejson: collection overflow page has no payload")
	}
	overflowPages := 1 + (o.MaxDocumentBytes-1)/overflowPayload
	// The metadata reserve covers the trees plus the free log's worst commit. The
	// free log's share is FreeLogMaxIndexPages + FreeLogMaxFoldSegments +
	// FreeLogMaxDeltaPages, which is deliberately the same twenty-eight pages the
	// flat image's sixteen-plus-four-plus-eight reserved, so replacing the image
	// with a segment index bought a thirty-five-fold larger free set without
	// costing any store a larger buffer pool.
	metadataPageLimit := fileStoreMetadataReservePages + len(compiled)*24
	if o.MaxBatchDocuments > 1 {
		metadataPageLimit = batchMetadataPages(o, len(compiled))
	}
	// Buffer indexes are uint16 today and the configured device ceiling is
	// 32,768. Reject the transaction geometry before int addition or byte
	// multiplication can wrap on adversarial maximum-document options.
	if metadataPageLimit < 0 || overflowPages >= 32768-metadataPageLimit {
		return normalizedFileStoreOptions{}, fmt.Errorf("vibejson: collection MaxBatchDocuments or maximum document requires too many transaction pages")
	}
	maxTransactionPages := overflowPages + metadataPageLimit
	// One document and its overflow chain may use maximum-size extents. A
	// categorical cover can replace one packed catalog, while a numeric
	// projection replaces one packed stripe plus a bounded path of PageSize
	// directory nodes. Every tree/root page remains exactly PageSize. The slot
	// cache therefore reserves the actual worst-case dirty bytes instead of
	// charging MaxPageSize for every metadata descriptor.
	largePages := overflowPages + 1
	if len(compiled) != 0 {
		largePages++
	}
	if len(columns) != 0 {
		largePages++
	}
	metadataPages := maxTransactionPages - largePages
	maxTransactionBytes := uint64(largePages)*uint64(o.MaxPageSize) +
		uint64(metadataPages)*uint64(o.PageSize)
	if o.MaxRetiredExtents < maxTransactionPages {
		return normalizedFileStoreOptions{}, fmt.Errorf("vibejson: collection MaxRetiredExtents must retain one worst-case transaction")
	}
	if o.BufferCount == 0 {
		o.BufferCount = defaultBufferCount(maxTransactionPages, o.MaxPageSize)
	}
	if o.BufferCount <= maxTransactionPages || o.BufferCount > maxCollectionBuffers {
		return normalizedFileStoreOptions{}, fmt.Errorf("vibejson: collection BufferCount must exceed worst-case %d-page transaction", maxTransactionPages)
	}
	if o.ResidentBytes < 0 || uint64(o.ResidentBytes) < maxTransactionBytes {
		return normalizedFileStoreOptions{}, fmt.Errorf("vibejson: collection ResidentBytes cannot retain one worst-case dirty transaction")
	}
	return normalizedFileStoreOptions{
		Options: o, maxTransactionPages: maxTransactionPages, maxTransactionBytes: maxTransactionBytes,
		indexes: compiled, float64Columns: columns, indexCatalogHash: catalogHash,
	}, nil
}

const (
	// maxCollectionBuffers matches the durability device's own staging ceiling.
	// Buffer indexes are uint16 on the wire between the writer and the device.
	maxCollectionBuffers = 32768
	// defaultCommitDepth is how many worst-case transactions the shipped
	// staging pool holds at once. Depth is the quantity that matters, not the
	// buffer count: at depth one a serialized writer waits for its own
	// predecessor's durability fence before it may begin, so it pays one fence
	// per Put no matter what Synchronous or the group-commit knobs say.
	defaultCommitDepth = 4
	// defaultCommitStageBytes caps what that depth is allowed to cost. Without
	// it the pool would scale with MaxDocumentBytes, and a store configured for
	// 64 MiB documents would silently reserve half a gigabyte of staging. A
	// configuration whose single worst-case transaction already exceeds this
	// budget keeps the old depth-one geometry, which is the correct
	// degradation: it is the one that fits.
	defaultCommitStageBytes = 32 << 20
)

// defaultBufferCount sizes the commit-buffer pool when the caller leaves
// BufferCount zero. It grows the smallest legal pool — a power of two strictly
// greater than one worst-case transaction — toward defaultCommitDepth
// transactions, stopping at the staging budget or the device ceiling.
func defaultBufferCount(maxTransactionPages, maxPageSize int) int {
	count := 1
	for count <= maxTransactionPages {
		count <<= 1
	}
	// maxTransactionPages+1 counts the alternate root buffer a transaction
	// reserves alongside its pages; leaving it out would size the pool a hair
	// short of the depth it claims to provide.
	target := defaultCommitDepth * (maxTransactionPages + 1)
	for count < target && count*2 <= maxCollectionBuffers &&
		uint64(count*2)*uint64(maxPageSize) <= defaultCommitStageBytes {
		count <<= 1
	}
	return count
}

func exactName(_ *store.ExactIndex, name string) string {
	return string(append([]byte(nil), name...))
}

func fileIndexHashBytes(hash uint64, src []byte) uint64 {
	for _, value := range src {
		hash = (hash ^ uint64(value)) * 1099511628211
	}
	return hash
}

type fileStoreState struct {
	root      storeio.StateRoot
	super     storeio.Superblock
	stateRef  storeio.PageRef
	keyRoot   storeio.PageRef
	chunkRoot storeio.PageRef
	indexRoot storeio.PageRef
	ttlRoot   storeio.PageRef
	// freeHead is the newest delta page of the free log, or the zero reference
	// when the durable free set is empty. It is reached through the superblock
	// rather than the state root, so the whole free set is replaceable without
	// rewriting a directory.
	freeHead storeio.PageRef
}

// Collection is a bounded-residency, page-oriented JSON document store. It owns
// no caller file lifetime: file must remain open through Close. Mutations are
// copy-on-write and automatically persisted through a checksummed double root.
// Reads use explicit Snapshot leases and caller-owned copy-out buffers.
type Collection struct {
	file         *os.File
	writerLocked bool
	options      normalizedFileStoreOptions
	storeID      [16]byte

	writer         sync.Mutex
	durabilityWait sync.WaitGroup
	snapshotGate   sync.RWMutex
	closed         bool
	closeDone      bool
	state          atomic.Pointer[fileStoreState]

	committer     *storeio.Committer
	cache         *storeio.PageCache
	readFile      *os.File
	writeFile     *os.File
	directRead    bool
	directWrite   bool
	leases        *storeio.GenerationLeases
	reclaimer     *storeio.ExtentReclaimer
	pageValidator *fileStorePageValidator

	parseScratch            []vibejson.IndexEntry
	oldParseScratch         []vibejson.IndexEntry
	indexValueScratch       []byte
	indexNewCertificate     []byte
	indexCertificateScratch []byte
	indexGroupSource        []storeio.IndexGroupCatalogEntry
	indexGroupEntries       []storeio.IndexGroupCatalogEntry
	documentValueScratch    []byte
	rowScratch              []storeio.DocumentRecord
	retireScratch           []storeio.FreeExtent
	reusable                []storeio.FreeExtent
	reuseJournal            []storeio.ReuseEdit
	reusableBlock           *storemem.Block
	float64Masks            []uint64
	float64Values           []float64
	float64StripeBytes      []byte
	float64StripeColumns    []storeio.Float64StripeColumn

	// Durable free-set bookkeeping. freeSegments is the published segment index;
	// freeIndexPages and freeDeltaPages are the pages the published index and
	// chain occupy, kept so a fold can retire exactly what it supersedes.
	// freeDirty marks, per published segment, that its durable page no longer
	// matches memory, which is what lets a fold rewrite those and carry the rest
	// forward by reference instead of rewriting the whole image. freePending
	// holds free-set changes made outside a transaction — reclamation, which is
	// not rolled back by Abort — and so must survive an aborted commit or those
	// extents would never be written down. abandonedExtents/abandonedBytes count
	// space forgotten because retirement metadata was full with no reader
	// responsible. They are serialized-writer state, guarded by the same writer
	// mutex as every other field here.
	abandonedExtents atomic.Uint64
	abandonedBytes   atomic.Uint64

	freeSegments    []storeio.FreeSegment
	freeNewSegments []storeio.FreeSegment
	freeIndexPages  []storeio.PageRef
	freeNewIndex    []storeio.PageRef
	freeDeltaPages  []storeio.PageRef
	freeNewDelta    []storeio.PageRef
	freeDirty       []bool
	freeRetired     []bool
	freeDirtyCount  int
	freeDirtyAll    bool
	freeFoldRanges  [][2]int
	freeFoldOrder   []freeFoldSlot

	freePending      []storeio.FreeDelta
	freeDeltas       []storeio.FreeDelta
	freeReclaimed    []storeio.FreeExtent
	freeFenced       []storeio.FreeExtent
	freeImageScratch []storeio.FreeExtent
	freeAllocMark    []uint32
	freeAllocStamp   uint32
	freeSetLimit     int
	freeDeltaPerPage int
	freeImagePerPage int
	freeIndexPerPage int
	freeFoldRequired bool
	freeLoaded       bool

	appendChunk uint32
	appendLive  uint64

	// Batched-publication scratch. Every one of these is reset at the start of
	// an Update and reused across calls, so a batch's steady-state cost is the
	// pages it publishes rather than the slices it would otherwise allocate.
	batch           *WriteBatch
	batchMutations  []fileBatchMutation
	batchChunkEdits []fileChunkEdit
	batchKeyEdits   []storeio.KeyTreeEdit
	batchIndexEdits []fileIndexBatchEdit
	batchTreeEdits  []storeio.IndexTreeEdit
	batchTreeCerts  []byte
	batchTTLEdits   []storeio.TTLTreeEdit
	batchCertArena  []byte
	batchRetired    []storeio.PageRef
}

// Stats is a point-in-time resource and I/O accounting snapshot.
// Every byte and queue counter corresponds to a configured finite budget.
type Stats struct {
	CapacityBytes uint64
	ResidentBytes uint64
	// CommitCapacityBytes is the fixed reusable staging arena owned by the
	// durability device. On supported systems it is mmap-backed and invisible
	// to the Go heap; it is capacity, not a claim that every page is resident.
	CommitCapacityBytes uint64
	PinnedPages         uint64
	DirtyBytes          uint64
	PageReads           uint64
	ReadBytes           uint64
	CacheHits           uint64
	CacheMisses         uint64
	CoalescedReads      uint64
	ReadErrors          uint64
	PrefetchHits        uint64
	Evictions           uint64
	PrefetchQueued      uint64
	PrefetchDropped     uint64
	// PrefetchQueueDepth samples references waiting for either read engine.
	PrefetchQueueDepth uint64
	// ReadQueueDepth is the configured native submission bound.
	ReadQueueDepth uint32
	// AsyncReadBatches counts successful native submissions.
	AsyncReadBatches uint64
	// LargestReadBatch is the native submission high-water.
	LargestReadBatch uint32

	PublishedGeneration uint64
	DurableGeneration   uint64
	CommitQueueDepth    uint64
	DeviceCommits       uint64
	CommittedBatches    uint64
	LargestCommitGroup  uint32
	// SuppressedRootWrites/Bytes count intermediate state pages omitted when
	// several generations share one newest durable superblock.
	SuppressedRootWrites uint64
	SuppressedRootBytes  uint64
	// DeviceBytes counts payload bytes handed to the durability device since
	// open. Divided by CommittedBatches it is write amplification per
	// generation. FileEnd cannot answer that question: copy-on-write reuses
	// retired extents, so the file stops growing while amplification does not.
	DeviceBytes uint64
	// Backend reports the durable write engine.
	Backend Backend
	// ReadBackend reports the active speculative-read engine. Demand misses
	// remain correct through positional reads regardless of this value.
	ReadBackend Backend
	// DirectReads reports actual O_DIRECT cache-miss reads, not merely a
	// requested try-direct policy.
	DirectReads bool
	// DirectWrites reports actual O_DIRECT durable writes. It is independent
	// from DirectReads and the selected portable or io_uring commit backend.
	DirectWrites bool

	SnapshotCapacity         uint64
	ActiveSnapshots          uint64
	OldestSnapshotGeneration uint64
	RetiredExtentCapacity    uint64
	// ReusableCapacityBytes is the fixed pointer-free extent arena. Common
	// Unix platforms keep it outside the Go heap.
	ReusableCapacityBytes uint64
	// ReusableExternalBytes is the portion of ReusableCapacityBytes outside
	// the Go heap on this platform.
	ReusableExternalBytes uint64
	// Float64ScratchBytes is the fixed pointer-free writer scratch used to
	// rebuild typed covering columns during one chunk replacement.
	Float64ScratchBytes   uint64
	PendingRetiredExtents uint64
	PendingRetiredBytes   uint64
	// AbandonedExtents and AbandonedBytes count space that became unreachable
	// but could not be written down, because retirement metadata was full and
	// no reader pinned it. Those extents are leaked: the file grows instead of
	// reusing them. It is a deliberate trade — see Options.MaxRetiredExtents —
	// and a non-zero value here is the signal that the bound is too small for
	// the workload, not that anything is damaged.
	AbandonedExtents uint64
	AbandonedBytes   uint64
	ReusableExtents  uint64
	ReusableBytes    uint64
	DocumentCount    uint64
	LiveChunks       uint32
	FileEnd          uint64
}

// Create initializes an empty durable collection in file and fences its
// first root before returning.
func Create(file *os.File, options Options) (*Collection, error) {
	if file == nil {
		return nil, fmt.Errorf("vibejson: nil collection file")
	}
	if err := storeio.LockWriter(file); err != nil {
		return nil, err
	}
	locked := true
	defer func() {
		if locked {
			_ = storeio.UnlockWriter(file)
		}
	}()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() != 0 {
		return nil, ErrNotEmpty
	}
	normalized, err := options.normalized()
	if err != nil {
		return nil, err
	}
	var storeID [16]byte
	if _, err := rand.Read(storeID[:]); err != nil {
		return nil, fmt.Errorf("vibejson: create collection identity: %w", err)
	}
	collection, err := newCollectionResources(file, normalized, storeID)
	if err != nil {
		return nil, err
	}
	collection.writerLocked = true
	locked = false
	if err := collection.createInitialState(); err != nil {
		_ = collection.closeResources()
		return nil, err
	}
	return collection, nil
}

// Open performs bounded recovery: it reads the two superblocks, the
// selected state root, and its top-level directory pages, then starts with an
// empty read cache. It does not scan keys, documents, postings, or TTL leaves.
func Open(file *os.File, options Options) (*Collection, error) {
	if file == nil {
		return nil, fmt.Errorf("vibejson: nil collection file")
	}
	if err := storeio.LockWriter(file); err != nil {
		return nil, err
	}
	locked := true
	defer func() {
		if locked {
			_ = storeio.UnlockWriter(file)
		}
	}()
	normalized, err := options.normalized()
	if err != nil {
		return nil, err
	}
	scratch := make([]byte, normalized.PageSize)
	super, root, _, err := storeio.RecoverStateRoot(file, uint32(normalized.PageSize), scratch)
	if err != nil {
		return nil, err
	}
	rootHasSchema := root.Options&storeio.StateOptionSchema != 0
	if root.ChunkDocuments != uint32(normalized.Collection.ChunkDocuments) ||
		root.IndexCount != uint32(len(normalized.indexes)) ||
		root.IndexCatalogHash != normalized.indexCatalogHash ||
		rootHasSchema != (normalized.Collection.Schema != nil) {
		return nil, fmt.Errorf("vibejson: collection options or unsupported durable catalog mismatch")
	}
	collection, err := newCollectionResources(file, normalized, root.StoreID)
	if err != nil {
		return nil, err
	}
	collection.writerLocked = true
	locked = false
	if err := collection.committer.InitializeGeneration(root.Generation); err != nil {
		_ = collection.closeResources()
		return nil, err
	}
	stateRef := storeio.PageRef{
		Offset: super.StateOffset, LogicalID: storeio.StateRootLogicalID,
		Generation: root.Generation, Length: super.StateLength, Kind: storeio.PageStateRoot,
	}
	// Only the chain's newest page is read here, to recover the logical identity
	// the superblock does not carry. The chain behind it is replayed lazily on
	// the first mutation, which keeps Open's cost bounded by the roots it
	// already reads and keeps a read-only user from paying for it at all.
	var freeHead storeio.PageRef
	if super.FreeLength != 0 {
		page := scratch[:super.FreeLength]
		n, readErr := file.ReadAt(page, int64(super.FreeOffset))
		if readErr != nil || n != len(page) {
			_ = collection.closeResources()
			if readErr != nil {
				return nil, readErr
			}
			return nil, storeio.ErrFreeLogCorrupt
		}
		view, openErr := storeio.OpenFreeDeltaPage(page, super.FileEnd, root.NextLogicalID)
		if openErr != nil {
			_ = collection.closeResources()
			return nil, openErr
		}
		header := view.Header()
		freeHead = storeio.PageRef{
			Offset: super.FreeOffset, LogicalID: header.LogicalID, Generation: header.Generation,
			Length: super.FreeLength, Kind: storeio.PageFreeDelta,
		}
	}
	state := &fileStoreState{
		root: root, super: super, stateRef: stateRef,
		keyRoot: root.KeyDirectory, chunkRoot: root.ChunkDirectory,
		indexRoot: root.IndexDirectory, ttlRoot: root.TTLDirectory, freeHead: freeHead,
	}
	collection.pageValidator.update(state)
	collection.state.Store(state)
	collection.appendChunk = root.ChunkHighWater
	if err := collection.restoreAppendChunk(state); err != nil {
		_ = collection.closeResources()
		return nil, err
	}
	return collection, nil
}

func newCollectionResources(file *os.File, options normalizedFileStoreOptions, storeID [16]byte) (*Collection, error) {
	writeFile, directWrite, err := storeio.OpenPageCommitFile(file, storeio.DirectMode(options.WriteMode))
	if err != nil {
		return nil, err
	}
	committer, err := storeio.NewCommitter(writeFile, storeio.DeviceOptions{
		Backend: storeio.Backend(options.Backend), BufferCount: options.BufferCount,
		BufferSize: max(options.MaxPageSize, os.Getpagesize()), QueueDepth: options.BufferCount,
	}, storeio.CommitterOptions{
		QueueSlots: options.QueueSlots, MaxPagesPerBatch: options.maxTransactionPages,
		GroupLimit: options.GroupLimit, CoalesceDelay: options.CommitCoalesce,
	})
	if err != nil {
		if writeFile != file {
			_ = writeFile.Close()
		}
		return nil, err
	}
	readFile, directRead, err := storeio.OpenPageCacheFile(file, storeio.DirectMode(options.ReadMode))
	if err != nil {
		_ = committer.Close()
		if writeFile != file {
			_ = writeFile.Close()
		}
		return nil, err
	}
	pageValidator := newFileStorePageValidator(
		uint32(options.PageSize), uint32(len(options.indexes)),
		uint32(options.Collection.ChunkDocuments),
	)
	cache, err := storeio.NewPageCache(readFile, storeio.PageCacheOptions{
		PageSize: options.PageSize, MaxPageSize: options.MaxPageSize,
		ResidentBytes: options.ResidentBytes, StoreID: storeID,
		PrefetchQueue: options.PrefetchQueue, ReadConcurrency: options.ReadConcurrency,
		ReadQueueDepth: options.ReadQueueDepth,
		Backend:        storeio.Backend(options.Backend),
		Validate:       pageValidator.validate,
	})
	if err != nil {
		if readFile != file {
			_ = readFile.Close()
		}
		_ = committer.Close()
		if writeFile != file {
			_ = writeFile.Close()
		}
		return nil, err
	}
	leases, err := storeio.NewGenerationLeases(storeio.GenerationLeaseOptions{MaxLeases: options.MaxSnapshotLeases})
	if err != nil {
		_ = cache.Close()
		if readFile != file {
			_ = readFile.Close()
		}
		_ = committer.Close()
		if writeFile != file {
			_ = writeFile.Close()
		}
		return nil, err
	}
	reclaimer, err := storeio.NewExtentReclaimer(leases, storeio.ExtentReclaimerOptions{MaxRetiredExtents: options.MaxRetiredExtents})
	if err != nil {
		_ = leases.Close()
		_ = cache.Close()
		if readFile != file {
			_ = readFile.Close()
		}
		_ = committer.Close()
		if writeFile != file {
			_ = writeFile.Close()
		}
		return nil, err
	}
	extentSize := int(unsafe.Sizeof(storeio.FreeExtent{}))
	if options.MaxRetiredExtents > math.MaxInt/extentSize {
		_ = leases.Close()
		_ = cache.Close()
		if readFile != file {
			_ = readFile.Close()
		}
		_ = committer.Close()
		if writeFile != file {
			_ = writeFile.Close()
		}
		return nil, store.ErrCheckpointTooLarge
	}
	reusableBlock, err := storemem.Allocate(options.MaxRetiredExtents * extentSize)
	if err != nil {
		_ = leases.Close()
		_ = cache.Close()
		if readFile != file {
			_ = readFile.Close()
		}
		_ = committer.Close()
		if writeFile != file {
			_ = writeFile.Close()
		}
		return nil, err
	}
	reusableArena := unsafe.Slice(
		(*storeio.FreeExtent)(unsafe.Pointer(unsafe.SliceData(reusableBlock.Bytes()))),
		options.MaxRetiredExtents,
	)
	var ownedRead *os.File
	if readFile != file {
		ownedRead = readFile
	}
	var ownedWrite *os.File
	if writeFile != file {
		ownedWrite = writeFile
	}
	pageSize := uint32(options.PageSize)
	deltaPerPage := storeio.FreeDeltaRecordCapacity(pageSize)
	imagePerPage := storeio.FreeImageRecordCapacity(pageSize)
	indexPerPage := storeio.FreeIndexRecordCapacity(pageSize)
	// The free set is capped at what the segment index can name, not at what one
	// image rewrite can hold. That is the whole change: the old cap was
	// FreeLogMaxImagePages*imagePerPage — sixteen pages, about 2,700 extents,
	// roughly 11 MiB of trackable free space at the 4 KiB page size — because a
	// fold rewrote the entire linked image and had to fit in one transaction.
	// The index costs one page per 70 segments of 168 extents, so the same
	// twenty-page fold reserve now describes about 94,000 extents, and what must
	// fit inside a commit is a directory of the free set rather than the free set.
	//
	// A collection that fragments past this ceiling still stalls reclamation and
	// eventually fails writes with ErrRetiredExtentCapacity, exactly as before;
	// the ceiling is simply thirty-five times further away, and raising it again
	// is a policy edit against FreeLogMaxIndexPages rather than a redesign.
	// Half the index's capacity, because the durable set now carries the fenced
	// extents as well as the reusable ones: a retirement is written down by the
	// commit that makes it, so both halves have to fit the same image.
	freeSetLimit := min(options.MaxRetiredExtents,
		storeio.FreeLogMaxIndexPages*indexPerPage*imagePerPage/2)
	return &Collection{
		file: file, options: options, storeID: storeID, committer: committer, cache: cache,
		readFile: ownedRead, writeFile: ownedWrite,
		directRead: directRead, directWrite: directWrite,
		leases: leases, reclaimer: reclaimer,
		// A fold retires the whole superseded chain on top of the commit's own
		// retirements, so the scratch reserves both.
		retireScratch: make([]storeio.FreeExtent, 0, options.maxTransactionPages+32+
			storeio.FreeLogMaxChainPages+storeio.FreeLogMaxIndexPages+
			storeio.FreeLogMaxFoldSegments),
		reusable:      reusableArena[:0],
		reuseJournal:  make([]storeio.ReuseEdit, 0, options.maxTransactionPages),
		reusableBlock: reusableBlock,
		float64Masks:  make([]uint64, len(options.float64Columns)),
		float64Values: make([]float64, len(options.float64Columns)*64),
		pageValidator: pageValidator,

		freeSegments:    make([]storeio.FreeSegment, 0, storeio.FreeLogMaxIndexPages*indexPerPage),
		freeNewSegments: make([]storeio.FreeSegment, 0, storeio.FreeLogMaxIndexPages*indexPerPage),
		freeIndexPages:  make([]storeio.PageRef, 0, storeio.FreeLogMaxIndexPages),
		freeNewIndex:    make([]storeio.PageRef, 0, storeio.FreeLogMaxIndexPages),
		freeDeltaPages:  make([]storeio.PageRef, 0, storeio.FreeLogMaxChainPages),
		freeNewDelta:    make([]storeio.PageRef, 0, storeio.FreeLogMaxDeltaPages),
		freeDirty:       make([]bool, 0, storeio.FreeLogMaxIndexPages*indexPerPage),
		freeRetired:     make([]bool, 0, storeio.FreeLogMaxIndexPages*indexPerPage),
		freeFoldRanges:  make([][2]int, 0, storeio.FreeLogMaxFoldSegments),
		freeFoldOrder:   make([]freeFoldSlot, 0, storeio.FreeLogMaxIndexPages*indexPerPage),
		freeDirtyAll:    true,
		// Half the diff capacity belongs to changes made outside a transaction;
		// the rest is left for what the commit itself consumes. Overflowing the
		// half is not a failure, it schedules a fold.
		freePending: make([]storeio.FreeDelta, 0,
			storeio.FreeLogMaxDeltaPages*deltaPerPage/2),
		freeDeltas: make([]storeio.FreeDelta, 0,
			storeio.FreeLogMaxDeltaPages*deltaPerPage+options.maxTransactionPages),
		freeReclaimed: make([]storeio.FreeExtent, 0, freeReclaimBatch),
		// The fold image is the reusable set plus everything still fenced plus
		// what this commit just retired, so its scratch has to hold all three.
		freeFenced: make([]storeio.FreeExtent, 0,
			options.MaxRetiredExtents+options.maxTransactionPages+32+
				storeio.FreeLogMaxChainPages+storeio.FreeLogMaxIndexPages+
				storeio.FreeLogMaxFoldSegments),
		freeImageScratch: make([]storeio.FreeExtent, 0,
			freeSetLimit+options.MaxRetiredExtents+options.maxTransactionPages+32+
				storeio.FreeLogMaxChainPages+storeio.FreeLogMaxIndexPages+
				storeio.FreeLogMaxFoldSegments),
		freeAllocMark:    make([]uint32, freeSetLimit),
		freeSetLimit:     freeSetLimit,
		freeDeltaPerPage: deltaPerPage,
		freeImagePerPage: imagePerPage,
		freeIndexPerPage: indexPerPage,
	}, nil
}

func (c *Collection) createInitialState() error {
	tx, err := storeio.BeginWriteTransaction(c.committer, c.cache, 1, storeio.WriteTransactionOptions{
		StoreID: c.cacheStoreID(), Generation: 1, PageSize: uint32(c.options.PageSize),
		FileEnd: 2 * uint64(c.options.PageSize), NextLogicalID: 2,
	})
	if err != nil {
		return err
	}
	statePage, err := tx.Allocate(storeio.PageStateRoot, uint32(c.options.PageSize), storeio.StateRootLogicalID)
	if err != nil {
		_ = tx.Abort()
		return err
	}
	root := storeio.StateRoot{
		StoreID: c.cacheStoreID(), Generation: 1, PageSize: uint32(c.options.PageSize),
		NextLogicalID: tx.NextLogicalID(), ChunkDocuments: uint32(c.options.Collection.ChunkDocuments),
		IndexCount: uint32(len(c.options.indexes)), IndexCatalogHash: c.options.indexCatalogHash,
	}
	if len(c.options.float64Columns) != 0 {
		root.Options |= storeio.StateOptionFloat64Columns
	}
	if c.options.Collection.Schema != nil {
		root.Options |= storeio.StateOptionSchema
	}
	if _, err := storeio.EncodeStateRootPage(statePage.Bytes(), root, tx.FileEnd()); err != nil {
		_ = tx.Abort()
		return err
	}
	if err := statePage.Stage(); err != nil {
		_ = tx.Abort()
		return err
	}
	if err := tx.Publish(statePage.Ref(), storeio.PageChecksum(statePage.Bytes()), 0, 0, 0); err != nil {
		_ = tx.Abort()
		return err
	}
	if err := c.committer.Wait(1); err != nil {
		return err
	}
	c.cache.MarkDurable(1)
	super := storeio.Superblock{
		StoreID: root.StoreID, Generation: 1, StateOffset: statePage.Ref().Offset,
		StateLength: statePage.Ref().Length, StateChecksum: storeio.PageChecksum(statePage.Bytes()),
		FileEnd: tx.FileEnd(), PageSize: uint32(c.options.PageSize),
	}
	state := &fileStoreState{root: root, super: super, stateRef: statePage.Ref()}
	c.pageValidator.update(state)
	c.state.Store(state)
	c.freeLoaded = true
	return nil
}

func (c *Collection) cacheStoreID() [16]byte {
	return c.storeID
}

// Snapshot pins one immutable durable root generation. Close must be
// called; copy-out methods remain valid independently of page eviction.
//
// A snapshot is cheap to take and expensive to keep. Holding one open blocks
// reuse of every extent the writer retires after it was taken, so a snapshot
// held across a sustained write loop fills Options.MaxRetiredExtents — roughly
// twenty thousand replacements at the default bound and geometry — and writes
// then fail with ErrRetiredExtentCapacity until it is closed. Prefer one
// snapshot per query over one per request handler or per connection. See
// Options.MaxRetiredExtents for the arithmetic and the recovery behaviour.
type Snapshot struct {
	collection *Collection
	state      *fileStoreState
	lease      storeio.GenerationLease
	once       sync.Once
}

// IndexWorkspace retains the transient routing entries, their copied
// certificates, the ordered probe decisions, and the document bytes and tape
// used by one durable exact-index probe. Its zero value is ready to use. The
// certificate arena exists because a leaf representative borrows an evictable
// page frame: the traversal copies it out rather than letting a slice escape
// its lease. Reusing one workspace with AppendIndexMasksInto makes a warmed
// probe allocation-free when caller dst and the observed candidate and
// document high-water marks fit retained capacity.
//
// A workspace is single-consumer and must not be used concurrently. Release
// drops retained storage when a rare broad probe should not pin its high-water
// capacity.
type IndexWorkspace struct {
	probe             storeio.IndexTreeProbe
	postings          []fileIndexProbePosting
	document          []byte
	tape              []vibejson.IndexEntry
	groupArena        []byte
	groupState        []fileIndexScalarGroupState
	indexCoverage     []uint64
	certifiedCoverage []uint64
	lastProbe         IndexProbeStats
}

// IndexProbeStats reports the physical work of the most recent exact or
// candidate-only probe performed with an IndexWorkspace. CandidateRows is
// the number of stable-slot bits read from posting pages. CertificateRows were
// decided from a collision-free scalar or compound-tuple representative
// without opening the documents; DocumentRecheckRows required exact
// comparison against stored JSON. PostingPages counts the index-directory
// leaf pages the probe admitted, which is its physical read work now that the
// masks live in those leaves. MatchedRows is populated only by an exact
// probe.
type IndexProbeStats struct {
	CandidateRows       uint64
	CertificateRows     uint64
	DocumentRecheckRows uint64
	MatchedRows         uint64
	CandidateChunks     int
	PostingPages        int
}

// LastProbeStats returns value-only counters for the most recent probe.
func (w *IndexWorkspace) LastProbeStats() IndexProbeStats {
	if w == nil {
		return IndexProbeStats{}
	}
	return w.lastProbe
}

// Release drops all storage retained by the workspace.
func (w *IndexWorkspace) Release() {
	if w == nil {
		return
	}
	w.probe = storeio.IndexTreeProbe{}
	w.postings = nil
	w.document = nil
	w.tape = nil
	w.groupArena = nil
	w.groupState = nil
	w.indexCoverage = nil
	w.certifiedCoverage = nil
	w.lastProbe = IndexProbeStats{}
}

// Snapshot acquires an explicit generation lease.
func (c *Collection) Snapshot() (*Snapshot, error) {
	if c == nil {
		return nil, ErrClosed
	}
	c.snapshotGate.RLock()
	state := c.state.Load()
	if state == nil {
		c.snapshotGate.RUnlock()
		return nil, ErrClosed
	}
	lease, err := c.leases.Acquire(state.root.Generation)
	c.snapshotGate.RUnlock()
	if err != nil {
		return nil, err
	}
	return &Snapshot{collection: c, state: state, lease: lease}, nil
}

// Close releases the snapshot generation. It is idempotent.
func (s *Snapshot) Close() error {
	if s == nil {
		return nil
	}
	s.once.Do(func() {
		s.lease.Release()
		s.collection = nil
		s.state = nil
	})
	return nil
}

// Len returns the number of keys visible to the snapshot.
func (s *Snapshot) Len() uint64 {
	if s == nil || s.state == nil {
		return 0
	}
	return s.state.root.DocumentCount
}

// Generation returns the pinned durable publication generation.
func (s *Snapshot) Generation() uint64 {
	if s == nil || s.state == nil {
		return 0
	}
	return s.state.root.Generation
}

// AppendRaw appends key's exact JSON spelling into dst. It never returns a
// borrowed page slice.
func (s *Snapshot) AppendRaw(dst []byte, key string) ([]byte, bool, error) {
	if s == nil || s.collection == nil || s.state == nil {
		return dst, false, ErrClosed
	}
	state := s.state
	bounds := storeio.KeyTreeBounds{
		FileEnd: state.super.FileEnd, NextLogicalID: state.root.NextLogicalID,
		ChunkHighWater: state.root.ChunkHighWater,
		ChunkDocuments: uint8(state.root.ChunkDocuments),
	}
	location, ok, err := storeio.LookupKeyTree(s.collection.cache, state.keyRoot, []byte(key), bounds)
	if err != nil || !ok {
		return dst, false, err
	}
	documentRef, ok, err := storeio.LookupChunkTree(s.collection.cache, state.chunkRoot, location.Chunk, storeio.ChunkTreeBounds{
		FileEnd: state.super.FileEnd, NextLogicalID: state.root.NextLogicalID,
	})
	if err != nil || !ok {
		return dst, false, err
	}
	lease, err := s.collection.cache.Acquire(documentRef)
	if err != nil {
		return dst, false, err
	}
	view, err := admittedFileDocumentChunk(lease.Page(), documentRef, location.Chunk)
	if err != nil {
		lease.Release()
		return dst, false, err
	}
	value, ok := view.lookupString(location.Slot, key)
	if !ok {
		lease.Release()
		return dst, false, nil
	}
	if value.grouped || value.value.Overflow == (storeio.PageRef{}) {
		dst, ok = view.appendJSON(dst, value)
		lease.Release()
		if !ok {
			return dst, false, storeio.ErrDocumentGroupCorrupt
		}
		return dst, true, nil
	}
	lease.Release()
	dst, err = s.appendOverflow(dst, value.value, location)
	return dst, err == nil, err
}

// PrefetchKeys resolves keys through the pinned directories and submits their
// document extents to the bounded asynchronous read queue in physical order.
// It returns the number submitted; missing keys are ignored and queue pressure
// is visible through Stats.PrefetchDropped.
func (s *Snapshot) PrefetchKeys(keys []string) (int, error) {
	if s == nil || s.collection == nil || s.state == nil {
		return 0, ErrClosed
	}
	var refs [64]storeio.PageRef
	count := 0
	queued := 0
	flush := func() error {
		if count == 0 {
			return nil
		}
		batch := refs[:count]
		slices.SortFunc(batch, func(a, b storeio.PageRef) int {
			if a.Offset < b.Offset {
				return -1
			}
			if a.Offset > b.Offset {
				return 1
			}
			return 0
		})
		unique := batch[:0]
		for _, ref := range batch {
			if len(unique) == 0 || unique[len(unique)-1].Offset != ref.Offset {
				unique = append(unique, ref)
			}
		}
		n, err := s.collection.cache.Prefetch(unique)
		queued += n
		count = 0
		return err
	}
	state := s.state
	keyBounds := storeio.KeyTreeBounds{
		FileEnd: state.super.FileEnd, NextLogicalID: state.root.NextLogicalID,
		ChunkHighWater: state.root.ChunkHighWater, ChunkDocuments: uint8(state.root.ChunkDocuments),
	}
	chunkBounds := storeio.ChunkTreeBounds{FileEnd: state.super.FileEnd, NextLogicalID: state.root.NextLogicalID}
	for _, key := range keys {
		location, ok, err := storeio.LookupKeyTree(s.collection.cache, state.keyRoot, []byte(key), keyBounds)
		if err != nil {
			return queued, err
		}
		if !ok {
			continue
		}
		ref, ok, err := storeio.LookupChunkTree(s.collection.cache, state.chunkRoot, location.Chunk, chunkBounds)
		if err != nil {
			return queued, err
		}
		if !ok {
			return queued, storeio.ErrChunkDirectoryCorrupt
		}
		refs[count] = ref
		count++
		if count == len(refs) {
			if err := flush(); err != nil {
				return queued, err
			}
		}
	}
	return queued, flush()
}

func (s *Snapshot) appendOverflow(dst []byte, value storeio.DocumentValue, location storeio.KeyLocation) ([]byte, error) {
	ref := value.Overflow
	offset := uint64(0)
	for ref != (storeio.PageRef{}) {
		lease, err := s.collection.cache.Acquire(ref)
		if err != nil {
			return dst, err
		}
		view, err := storeio.OpenOverflowPage(
			lease.Page(), s.state.super.FileEnd, s.state.root.NextLogicalID,
			s.state.root.PageSize, s.state.root.ChunkHighWater, uint8(s.state.root.ChunkDocuments),
		)
		if err != nil {
			lease.Release()
			return dst, err
		}
		header := view.Header()
		if header.Chunk != location.Chunk || header.Slot != location.Slot ||
			header.Total != value.Length || header.Offset != offset {
			lease.Release()
			return dst, storeio.ErrOverflowPageCorrupt
		}
		dst = append(dst, view.Data()...)
		offset += uint64(len(view.Data()))
		next := header.Next
		lease.Release()
		if next != (storeio.PageRef{}) {
			_, _ = s.collection.cache.Prefetch([]storeio.PageRef{next})
		}
		ref = next
	}
	if offset != value.Length {
		return dst, storeio.ErrOverflowPageCorrupt
	}
	return dst, nil
}

// AppendRaw is the current-snapshot convenience form.
func (c *Collection) AppendRaw(dst []byte, key string) ([]byte, bool, error) {
	snapshot, err := c.Snapshot()
	if err != nil {
		return dst, false, err
	}
	defer snapshot.Close()
	return snapshot.AppendRaw(dst, key)
}

// PrefetchKeys submits current-snapshot document reads to the bounded
// asynchronous prefetch queue.
func (c *Collection) PrefetchKeys(keys []string) (int, error) {
	snapshot, err := c.Snapshot()
	if err != nil {
		return 0, err
	}
	defer snapshot.Close()
	return snapshot.PrefetchKeys(keys)
}

// Len returns the current durable-state key count.
func (c *Collection) Len() uint64 {
	if c == nil || c.state.Load() == nil {
		return 0
	}
	return c.state.Load().root.DocumentCount
}

// Generation returns the current reader-visible generation.
func (c *Collection) Generation() uint64 {
	if c == nil || c.state.Load() == nil {
		return 0
	}
	return c.state.Load().root.Generation
}

// DurableGeneration returns the newest crash-safe generation.
func (c *Collection) DurableGeneration() uint64 {
	if c == nil || c.committer == nil {
		return 0
	}
	return c.committer.DurableGeneration()
}

// Stats reports configured residency, page I/O, prefetch, durability queue,
// snapshot, and reclamation pressure without performing file I/O.
func (c *Collection) Stats() Stats {
	if c == nil || c.cache == nil || c.committer == nil {
		return Stats{}
	}
	c.writer.Lock()
	defer c.writer.Unlock()
	cache := c.cache.Stats()
	commit := c.committer.Stats()
	state := c.state.Load()
	current := uint64(0)
	if state != nil {
		current = state.root.Generation
	}
	leases := c.leases.Stats(current)
	retired := c.reclaimer.Stats()
	stats := Stats{
		CapacityBytes: cache.CapacityBytes, ResidentBytes: cache.ResidentBytes,
		CommitCapacityBytes: uint64(c.options.BufferCount) * uint64(c.options.MaxPageSize),
		PinnedPages:         cache.PinnedPages, DirtyBytes: cache.DirtyBytes,
		PageReads: cache.PageReads, ReadBytes: cache.ReadBytes, CacheHits: cache.CacheHits,
		CacheMisses: cache.Misses, CoalescedReads: cache.Coalesced, ReadErrors: cache.ReadErrors,
		PrefetchHits: cache.PrefetchHits, Evictions: cache.Evictions,
		PrefetchQueued: cache.PrefetchQueued, PrefetchDropped: cache.PrefetchDropped,
		PrefetchQueueDepth: cache.QueueDepth, ReadQueueDepth: cache.ReadQueueDepth,
		AsyncReadBatches: cache.AsyncReadBatches, LargestReadBatch: cache.LargestReadBatch,
		PublishedGeneration: commit.PublishedGeneration, DurableGeneration: commit.DurableGeneration,
		CommitQueueDepth: commit.QueuedGenerations, DeviceCommits: commit.DeviceCommits,
		CommittedBatches: commit.CommittedBatches, LargestCommitGroup: commit.LargestGroup,
		SuppressedRootWrites: commit.SuppressedRootWrites,
		SuppressedRootBytes:  commit.SuppressedRootBytes,
		DeviceBytes:          commit.DeviceBytes,
		Backend:              Backend(commit.Backend),
		ReadBackend:          Backend(cache.ReadBackend),
		DirectReads:          c.directRead,
		DirectWrites:         c.directWrite,
		SnapshotCapacity:     leases.Capacity, ActiveSnapshots: leases.Active,
		OldestSnapshotGeneration: leases.MinimumGeneration,
		RetiredExtentCapacity:    retired.Capacity, PendingRetiredExtents: retired.Pending,
		PendingRetiredBytes: retired.PendingBytes, ReusableExtents: uint64(len(c.reusable)),
		AbandonedExtents: c.abandonedExtents.Load(), AbandonedBytes: c.abandonedBytes.Load(),
		Float64ScratchBytes: uint64(len(c.float64Masks))*8 + uint64(len(c.float64Values))*8,
	}
	if c.reusableBlock != nil {
		stats.ReusableCapacityBytes = uint64(c.reusableBlock.Len())
		if c.reusableBlock.OutsideHeap() {
			stats.ReusableExternalBytes = stats.ReusableCapacityBytes
		}
	}
	for _, extent := range c.reusable {
		stats.ReusableBytes += extent.Length
	}
	if state != nil {
		stats.DocumentCount = state.root.DocumentCount
		stats.LiveChunks = state.root.LiveChunks
		stats.FileEnd = state.super.FileEnd
	}
	return stats
}

// Put validates and copies src, then atomically publishes a copy-on-write file
// generation. created reports whether key was absent. Async mode returns after
// the bounded committer accepts the generation; Synchronous waits for the
// double-root durability fence.
func (c *Collection) Put(key string, src []byte) (created bool, err error) {
	if c == nil {
		return false, ErrClosed
	}
	c.writer.Lock()
	var generation uint64
	defer func() {
		wait := generation != 0 && c.options.Synchronous
		if wait {
			c.durabilityWait.Add(1)
		}
		c.writer.Unlock()
		if wait {
			err = errors.Join(err, c.waitPublished(generation))
			c.durabilityWait.Done()
		}
	}()
	if c.closed {
		return false, ErrClosed
	}
	if len(key) > c.options.MaxKeyBytes {
		return false, ErrKeyTooLarge
	}
	if len(src) > c.options.MaxDocumentBytes {
		return false, ErrDocumentTooLarge
	}
	index, err := c.validateDocument(src)
	if err != nil {
		return false, err
	}
	state := c.state.Load()
	if state == nil {
		return false, ErrClosed
	}
	keyBytes := []byte(key)
	keyBounds := storeio.KeyTreeBounds{
		FileEnd: state.super.FileEnd, NextLogicalID: state.root.NextLogicalID,
		ChunkHighWater: state.root.ChunkHighWater,
		ChunkDocuments: uint8(state.root.ChunkDocuments),
	}
	var location storeio.KeyLocation
	found := false
	if state.keyRoot != (storeio.PageRef{}) {
		location, found, err = storeio.LookupKeyTree(c.cache, state.keyRoot, keyBytes, keyBounds)
		if err != nil {
			return false, err
		}
	}
	created = !found
	prospectiveHighWater := state.root.ChunkHighWater
	if !found {
		limit := fileStoreLiveMask(state.root.ChunkDocuments)
		if c.appendChunk < state.root.ChunkHighWater && c.appendLive != limit {
			location.Chunk = c.appendChunk
			location.Slot = uint8(bits.TrailingZeros64(^c.appendLive & limit))
		} else {
			if state.root.ChunkHighWater == ^uint32(0) {
				return false, store.ErrTooLarge
			}
			location = storeio.KeyLocation{Chunk: state.root.ChunkHighWater}
			prospectiveHighWater++
		}
	}
	if err := c.ensureDirtyCapacity(); err != nil {
		return false, err
	}
	created, err = c.putLocked(state, keyBytes, src, index, location, created, prospectiveHighWater)
	if err == nil {
		generation = state.root.Generation + 1
	}
	return created, err
}

func (c *Collection) putLocked(state *fileStoreState, key, src []byte, newIndex vibejson.Index, location storeio.KeyLocation, created bool, prospectiveHighWater uint32) (bool, error) {
	generation := state.root.Generation + 1
	if generation == 0 {
		return false, storeio.ErrGenerationOrder
	}
	if err := c.refreshReusable(state); err != nil {
		return false, err
	}
	tx, err := storeio.BeginWriteTransaction(c.committer, c.cache, c.options.maxTransactionPages, storeio.WriteTransactionOptions{
		StoreID: c.storeID, Generation: generation, PageSize: uint32(c.options.PageSize),
		FileEnd: state.super.FileEnd, NextLogicalID: state.root.NextLogicalID,
		Reusable: c.reusable, ReuseJournal: c.reuseJournal,
	})
	if err != nil {
		return false, err
	}
	abort := true
	retirementReserved := false
	defer func() {
		if abort {
			if retirementReserved {
				_ = c.reclaimer.CancelRetiredGeneration(state.root.Generation)
			}
			_ = tx.Abort()
		}
	}()
	c.retireScratch = c.retireScratch[:0]

	oldRef, oldView, oldLease, err := c.loadFileChunk(state, location.Chunk)
	if err != nil {
		return false, err
	}
	if oldLease != nil {
		defer oldLease.Release()
	}
	var oldIndex vibejson.Index
	hasOldIndex := false
	if created {
		if oldView != nil {
			if _, occupied := oldView.lookup(location.Slot); occupied {
				return false, storeio.ErrDocumentPageCorrupt
			}
		}
	} else {
		if oldView == nil {
			return false, storeio.ErrDocumentPageCorrupt
		}
		oldValue, ok := oldView.lookupKey(location.Slot, key)
		if !ok {
			return false, storeio.ErrDocumentPageCorrupt
		}
		if !oldValue.grouped {
			if err := c.appendOverflowRetirements(state, oldValue.value, location); err != nil {
				return false, err
			}
		}
		if len(c.options.indexes) != 0 ||
			len(c.options.float64Columns) != 0 {
			raw, valueErr := c.appendFileDocumentValue(
				c.indexValueScratch[:0], state, *oldView, oldValue, location,
			)
			if valueErr != nil {
				return false, valueErr
			}
			c.indexValueScratch = raw
			oldIndex, err = c.buildOldFileIndex(raw)
			if err != nil {
				return false, err
			}
			hasOldIndex = true
		}
	}
	newRecord, err := c.stageFileValue(tx, location, key, src)
	if err != nil {
		return false, err
	}
	rows, live, err := c.buildFileRows(state, oldView, []fileChunkEdit{{
		slot: location.Slot, record: newRecord, keep: true,
	}})
	if err != nil {
		return false, err
	}
	columns, err := c.buildFileFloat64Columns(state, oldView, location.Slot, &newIndex, true)
	if err != nil {
		return false, err
	}
	documentSize, err := c.fileDocumentPageSize(rows, columns)
	if err != nil {
		return false, err
	}
	documentLogicalID := uint64(0)
	if oldRef.Kind == storeio.PageDocument {
		documentLogicalID = oldRef.LogicalID
	}
	documentPage, err := tx.Allocate(storeio.PageDocument, documentSize, documentLogicalID)
	if err != nil {
		return false, err
	}
	if _, err := storeio.EncodeDocumentPageWithColumns(documentPage.Bytes(), storeio.DocumentPageHeader{
		StoreID: c.storeID, Generation: generation, LogicalID: documentPage.Ref().LogicalID,
		PageSize: documentPage.Ref().Length, ChunkID: location.Chunk, Live: live,
	}, rows, columns, tx.NextLogicalID(), tx.FileEnd(), uint32(c.options.PageSize)); err != nil {
		return false, err
	}
	if err := documentPage.Stage(); err != nil {
		return false, err
	}
	chunkMutation, err := storeio.UpsertChunkTree(c.cache, tx, state.chunkRoot, location.Chunk, documentPage.Ref(), storeio.ChunkTreeBounds{
		FileEnd: state.super.FileEnd, NextLogicalID: state.root.NextLogicalID,
	})
	if err != nil {
		return false, err
	}
	keyRoot := state.keyRoot
	var keyMutation storeio.KeyTreeMutation
	if created {
		keyMutation, err = storeio.UpsertKeyTree(c.cache, tx, state.keyRoot, key, location, storeio.KeyTreeBounds{
			FileEnd: state.super.FileEnd, NextLogicalID: state.root.NextLogicalID,
			ChunkHighWater: prospectiveHighWater, ChunkDocuments: uint8(state.root.ChunkDocuments),
		})
		if err != nil {
			return false, err
		}
		keyRoot = keyMutation.Root
	}
	var oldIndexPointer *vibejson.Index
	if hasOldIndex {
		oldIndexPointer = &oldIndex
	}
	indexRoot, err := c.updateFileIndexes(tx, state, location, oldIndexPointer, &newIndex)
	if err != nil {
		return false, err
	}
	documentCount := state.root.DocumentCount
	liveChunks := state.root.LiveChunks
	if created {
		documentCount++
		if oldRef == (storeio.PageRef{}) {
			liveChunks++
		}
	}
	indexGroupHead, retireIndexGroup, err := c.maintainFileIndexGroups(
		tx, state, location, oldIndexPointer, &newIndex,
		documentCount, prospectiveHighWater,
	)
	if err != nil {
		return false, err
	}
	float64ScanHead, retireFloat64Scan, err :=
		c.maintainFileFloat64Scan(
			tx, state, chunkMutation.Root, location,
			oldIndexPointer, &newIndex, created,
		)
	if err != nil {
		return false, err
	}
	statePage, err := c.reserveFileStatePage(tx)
	if err != nil {
		return false, err
	}
	if err := c.collectFileRetirements(
		state, oldRef, oldView, keyMutation, chunkMutation,
		retireFloat64Scan, retireIndexGroup,
	); err != nil {
		return false, err
	}
	freeLog, err := c.syncFreeLog(tx, state)
	if err != nil {
		return false, err
	}
	nextState, statePage, err := c.stageFileState(
		tx, statePage, state, generation, prospectiveHighWater, documentCount, state.root.TTLCount,
		liveChunks, chunkMutation.Root, keyRoot, indexRoot, state.ttlRoot,
		float64ScanHead, indexGroupHead, freeLog.head, freeLog.checksum,
	)
	if err != nil {
		return false, err
	}
	if err := c.reserveFileRetirements(); err != nil {
		return false, err
	}
	retirementReserved = true
	if err := tx.Publish(statePage.Ref(), storeio.PageChecksum(statePage.Bytes()), nextState.super.FreeOffset, nextState.super.FreeLength, nextState.super.FreeChecksum); err != nil {
		return false, err
	}
	abort = false
	c.finalizeReusable()
	c.commitFreeLog(freeLog)
	c.snapshotGate.Lock()
	c.pageValidator.update(nextState)
	c.state.Store(nextState)
	c.snapshotGate.Unlock()
	if location.Chunk >= state.root.ChunkHighWater || location.Chunk == c.appendChunk {
		c.appendChunk = location.Chunk
		c.appendLive = live
	}
	if live == fileStoreLiveMask(state.root.ChunkDocuments) {
		c.appendChunk = prospectiveHighWater
		c.appendLive = 0
	}
	return created, nil
}

// Delete removes key through the same failure-atomic page publication.
func (c *Collection) Delete(key string) (deleted bool, err error) {
	if c == nil {
		return false, ErrClosed
	}
	c.writer.Lock()
	var generation uint64
	defer func() {
		wait := generation != 0 && c.options.Synchronous
		if wait {
			c.durabilityWait.Add(1)
		}
		c.writer.Unlock()
		if wait {
			err = errors.Join(err, c.waitPublished(generation))
			c.durabilityWait.Done()
		}
	}()
	if c.closed {
		return false, ErrClosed
	}
	state := c.state.Load()
	if state == nil || state.keyRoot == (storeio.PageRef{}) {
		return false, nil
	}
	location, found, err := storeio.LookupKeyTree(c.cache, state.keyRoot, []byte(key), storeio.KeyTreeBounds{
		FileEnd: state.super.FileEnd, NextLogicalID: state.root.NextLogicalID,
		ChunkHighWater: state.root.ChunkHighWater, ChunkDocuments: uint8(state.root.ChunkDocuments),
	})
	if err != nil || !found {
		return false, err
	}
	if err := c.ensureDirtyCapacity(); err != nil {
		return false, err
	}
	deleted, err = c.deleteLocked(state, []byte(key), location)
	if err == nil && deleted {
		generation = state.root.Generation + 1
	}
	return deleted, err
}

// SetTTL assigns a deadline relative to the current clock. A non-positive TTL
// publishes an ordinary delete.
func (c *Collection) SetTTL(key string, ttl time.Duration) (bool, error) {
	if ttl <= 0 {
		return c.Delete(key)
	}
	return c.SetDeadline(key, time.Now().Add(ttl))
}

// SetDeadline durably assigns an absolute expiration. Ordinary reads never
// consult the clock; ExpireDue makes a due key invisible through a normal
// copy-on-write delete.
func (c *Collection) SetDeadline(key string, deadline time.Time) (updated bool, err error) {
	if !deadline.After(time.Now()) {
		return c.Delete(key)
	}
	nanos := deadline.UnixNano()
	if !time.Unix(0, nanos).Equal(deadline) || nanos == 0 {
		return false, ErrDeadlineRange
	}
	if c == nil {
		return false, ErrClosed
	}
	c.writer.Lock()
	var generation uint64
	defer func() {
		wait := generation != 0 && c.options.Synchronous
		if wait {
			c.durabilityWait.Add(1)
		}
		c.writer.Unlock()
		if wait {
			err = errors.Join(err, c.waitPublished(generation))
			c.durabilityWait.Done()
		}
	}()
	if c.closed {
		return false, ErrClosed
	}
	state := c.state.Load()
	if state == nil || state.keyRoot == (storeio.PageRef{}) {
		return false, nil
	}
	location, found, err := storeio.LookupKeyTree(c.cache, state.keyRoot, []byte(key), storeio.KeyTreeBounds{
		FileEnd: state.super.FileEnd, NextLogicalID: state.root.NextLogicalID,
		ChunkHighWater: state.root.ChunkHighWater, ChunkDocuments: uint8(state.root.ChunkDocuments),
	})
	if err != nil || !found {
		return false, err
	}
	if location.Deadline == nanos {
		return true, nil
	}
	if err := c.ensureDirtyCapacity(); err != nil {
		return false, err
	}
	updated, err = c.setDeadlineLocked(state, []byte(key), location, nanos)
	if err == nil && updated {
		generation = state.root.Generation + 1
	}
	return updated, err
}

// Persist removes key's expiration without changing the document.
func (c *Collection) Persist(key string) (updated bool, err error) {
	if c == nil {
		return false, ErrClosed
	}
	c.writer.Lock()
	var generation uint64
	defer func() {
		wait := generation != 0 && c.options.Synchronous
		if wait {
			c.durabilityWait.Add(1)
		}
		c.writer.Unlock()
		if wait {
			err = errors.Join(err, c.waitPublished(generation))
			c.durabilityWait.Done()
		}
	}()
	if c.closed {
		return false, ErrClosed
	}
	state := c.state.Load()
	if state == nil || state.keyRoot == (storeio.PageRef{}) {
		return false, nil
	}
	location, found, err := storeio.LookupKeyTree(c.cache, state.keyRoot, []byte(key), storeio.KeyTreeBounds{
		FileEnd: state.super.FileEnd, NextLogicalID: state.root.NextLogicalID,
		ChunkHighWater: state.root.ChunkHighWater, ChunkDocuments: uint8(state.root.ChunkDocuments),
	})
	if err != nil || !found || location.Deadline == 0 {
		return false, err
	}
	if err := c.ensureDirtyCapacity(); err != nil {
		return false, err
	}
	updated, err = c.setDeadlineLocked(state, []byte(key), location, 0)
	if err == nil && updated {
		generation = state.root.Generation + 1
	}
	return updated, err
}

func (c *Collection) setDeadlineLocked(state *fileStoreState, key []byte, location storeio.KeyLocation, deadline int64) (bool, error) {
	generation := state.root.Generation + 1
	if err := c.refreshReusable(state); err != nil {
		return false, err
	}
	tx, err := storeio.BeginWriteTransaction(c.committer, c.cache, c.options.maxTransactionPages, storeio.WriteTransactionOptions{
		StoreID: c.storeID, Generation: generation, PageSize: uint32(c.options.PageSize),
		FileEnd: state.super.FileEnd, NextLogicalID: state.root.NextLogicalID,
		Reusable: c.reusable, ReuseJournal: c.reuseJournal,
	})
	if err != nil {
		return false, err
	}
	abort := true
	retirementReserved := false
	defer func() {
		if abort {
			if retirementReserved {
				_ = c.reclaimer.CancelRetiredGeneration(state.root.Generation)
			}
			_ = tx.Abort()
		}
	}()
	c.retireScratch = c.retireScratch[:0]
	ttlRoot := state.ttlRoot
	ttlCount := state.root.TTLCount
	bounds := storeio.TTLTreeBounds{
		FileEnd: state.super.FileEnd, NextLogicalID: state.root.NextLogicalID,
		ChunkHighWater: state.root.ChunkHighWater, ChunkDocuments: uint8(state.root.ChunkDocuments),
	}
	if location.Deadline != 0 {
		mutation, deleteErr := storeio.DeleteTTLTree(c.cache, tx, ttlRoot, storeio.TTLKey{
			Deadline: location.Deadline, Chunk: location.Chunk, Slot: location.Slot,
		}, bounds)
		if deleteErr != nil {
			return false, deleteErr
		}
		if !mutation.Found {
			return false, storeio.ErrTTLDirectoryCorrupt
		}
		ttlRoot = mutation.Root
		ttlCount--
		if err := c.appendTTLRetirements(state, mutation); err != nil {
			return false, err
		}
	}
	if deadline != 0 {
		bounds.FileEnd, bounds.NextLogicalID = tx.FileEnd(), tx.NextLogicalID()
		mutation, insertErr := storeio.UpsertTTLTree(c.cache, tx, ttlRoot, storeio.TTLKey{
			Deadline: deadline, Chunk: location.Chunk, Slot: location.Slot,
		}, bounds)
		if insertErr != nil {
			return false, insertErr
		}
		ttlRoot = mutation.Root
		ttlCount++
		if err := c.appendTTLRetirements(state, mutation); err != nil {
			return false, err
		}
	}
	location.Deadline = deadline
	keyMutation, err := storeio.UpsertKeyTree(c.cache, tx, state.keyRoot, key, location, storeio.KeyTreeBounds{
		FileEnd: state.super.FileEnd, NextLogicalID: state.root.NextLogicalID,
		ChunkHighWater: state.root.ChunkHighWater, ChunkDocuments: uint8(state.root.ChunkDocuments),
	})
	if err != nil || !keyMutation.Found {
		return false, err
	}
	statePage, err := c.reserveFileStatePage(tx)
	if err != nil {
		return false, err
	}
	if err := c.collectFileRetirements(
		state, storeio.PageRef{}, nil, keyMutation, storeio.ChunkTreeMutation{},
		false, false,
	); err != nil {
		return false, err
	}
	freeLog, err := c.syncFreeLog(tx, state)
	if err != nil {
		return false, err
	}
	nextState, statePage, err := c.stageFileState(
		tx, statePage, state, generation, state.root.ChunkHighWater, state.root.DocumentCount, ttlCount,
		state.root.LiveChunks, state.chunkRoot, keyMutation.Root, state.indexRoot, ttlRoot,
		state.root.Float64ScanHead, state.root.IndexGroupHead, freeLog.head, freeLog.checksum,
	)
	if err != nil {
		return false, err
	}
	if err := c.reserveFileRetirements(); err != nil {
		return false, err
	}
	retirementReserved = true
	if err := tx.Publish(statePage.Ref(), storeio.PageChecksum(statePage.Bytes()), nextState.super.FreeOffset, nextState.super.FreeLength, nextState.super.FreeChecksum); err != nil {
		return false, err
	}
	abort = false
	c.finalizeReusable()
	c.commitFreeLog(freeLog)
	c.snapshotGate.Lock()
	c.pageValidator.update(nextState)
	c.state.Store(nextState)
	c.snapshotGate.Unlock()
	return true, nil
}

// Deadline returns the deadline encoded beside the key in this snapshot.
func (s *Snapshot) Deadline(key string) (time.Time, bool, error) {
	if s == nil || s.collection == nil || s.state == nil {
		return time.Time{}, false, ErrClosed
	}
	state := s.state
	location, found, err := storeio.LookupKeyTree(s.collection.cache, state.keyRoot, []byte(key), storeio.KeyTreeBounds{
		FileEnd: state.super.FileEnd, NextLogicalID: state.root.NextLogicalID,
		ChunkHighWater: state.root.ChunkHighWater, ChunkDocuments: uint8(state.root.ChunkDocuments),
	})
	if err != nil || !found || location.Deadline == 0 {
		return time.Time{}, false, err
	}
	return time.Unix(0, location.Deadline), true, nil
}

func (c *Collection) Deadline(key string) (time.Time, bool, error) {
	snapshot, err := c.Snapshot()
	if err != nil {
		return time.Time{}, false, err
	}
	defer snapshot.Close()
	return snapshot.Deadline(key)
}

func (c *Collection) TTLAt(key string, now time.Time) (time.Duration, bool, error) {
	deadline, ok, err := c.Deadline(key)
	if err != nil || !ok {
		return 0, false, err
	}
	return deadline.Sub(now), true, nil
}

// ExpireDue publishes up to limit normal deletes ordered by deadline. A
// non-positive limit drains every deadline due at now with bounded memory.
func (c *Collection) ExpireDue(now time.Time, limit int) (expired int, err error) {
	if c == nil {
		return 0, ErrClosed
	}
	c.writer.Lock()
	var generation uint64
	defer func() {
		wait := generation != 0 && c.options.Synchronous
		if wait {
			c.durabilityWait.Add(1)
		}
		c.writer.Unlock()
		if wait {
			err = errors.Join(err, c.waitPublished(generation))
			c.durabilityWait.Done()
		}
	}()
	if c.closed {
		return 0, ErrClosed
	}
	nowNanos := now.UnixNano()
	if !time.Unix(0, nowNanos).Equal(now) {
		return 0, ErrDeadlineRange
	}
	for limit <= 0 || expired < limit {
		state := c.state.Load()
		if state == nil || state.ttlRoot == (storeio.PageRef{}) {
			break
		}
		entry, ok, err := storeio.FirstTTLTree(c.cache, state.ttlRoot, storeio.TTLTreeBounds{
			FileEnd: state.super.FileEnd, NextLogicalID: state.root.NextLogicalID,
			ChunkHighWater: state.root.ChunkHighWater, ChunkDocuments: uint8(state.root.ChunkDocuments),
		})
		if err != nil {
			return expired, err
		}
		if !ok || entry.Deadline > nowNanos {
			break
		}
		_, view, lease, err := c.loadFileChunk(state, entry.Chunk)
		if err != nil || view == nil {
			return expired, err
		}
		record, found := view.lookup(entry.Slot)
		if !found {
			lease.Release()
			return expired, storeio.ErrTTLDirectoryCorrupt
		}
		location := storeio.KeyLocation{Chunk: entry.Chunk, Slot: entry.Slot, Deadline: entry.Deadline}
		_, err = c.deleteLocked(state, record.key, location)
		lease.Release()
		if err != nil {
			return expired, err
		}
		expired++
		generation = state.root.Generation + 1
	}
	return expired, nil
}

func (c *Collection) deleteLocked(state *fileStoreState, key []byte, location storeio.KeyLocation) (bool, error) {
	generation := state.root.Generation + 1
	if err := c.refreshReusable(state); err != nil {
		return false, err
	}
	tx, err := storeio.BeginWriteTransaction(c.committer, c.cache, c.options.maxTransactionPages, storeio.WriteTransactionOptions{
		StoreID: c.storeID, Generation: generation, PageSize: uint32(c.options.PageSize),
		FileEnd: state.super.FileEnd, NextLogicalID: state.root.NextLogicalID,
		Reusable: c.reusable, ReuseJournal: c.reuseJournal,
	})
	if err != nil {
		return false, err
	}
	abort := true
	retirementReserved := false
	defer func() {
		if abort {
			if retirementReserved {
				_ = c.reclaimer.CancelRetiredGeneration(state.root.Generation)
			}
			_ = tx.Abort()
		}
	}()
	c.retireScratch = c.retireScratch[:0]
	oldRef, oldView, oldLease, err := c.loadFileChunk(state, location.Chunk)
	if err != nil || oldView == nil {
		return false, err
	}
	defer oldLease.Release()
	oldValue, ok := oldView.lookupKey(location.Slot, key)
	if !ok {
		return false, storeio.ErrDocumentPageCorrupt
	}
	if !oldValue.grouped {
		if err := c.appendOverflowRetirements(state, oldValue.value, location); err != nil {
			return false, err
		}
	}
	var oldIndex vibejson.Index
	if len(c.options.indexes) != 0 ||
		len(c.options.float64Columns) != 0 {
		raw, valueErr := c.appendFileDocumentValue(
			c.indexValueScratch[:0], state, *oldView, oldValue, location,
		)
		if valueErr != nil {
			return false, valueErr
		}
		c.indexValueScratch = raw
		oldIndex, err = c.buildOldFileIndex(raw)
		if err != nil {
			return false, err
		}
	}
	rows, live, err := c.buildFileRows(state, oldView, []fileChunkEdit{{slot: location.Slot}})
	if err != nil {
		return false, err
	}
	var chunkMutation storeio.ChunkTreeMutation
	if live == 0 {
		chunkMutation, err = storeio.DeleteChunkTree(c.cache, tx, state.chunkRoot, location.Chunk, storeio.ChunkTreeBounds{
			FileEnd: state.super.FileEnd, NextLogicalID: state.root.NextLogicalID,
		})
	} else {
		columns, coverErr := c.buildFileFloat64Columns(state, oldView, location.Slot, nil, false)
		if coverErr != nil {
			return false, coverErr
		}
		documentSize, sizeErr := c.fileDocumentPageSize(rows, columns)
		if sizeErr != nil {
			return false, sizeErr
		}
		documentLogicalID := uint64(0)
		if oldRef.Kind == storeio.PageDocument {
			documentLogicalID = oldRef.LogicalID
		}
		documentPage, allocateErr := tx.Allocate(storeio.PageDocument, documentSize, documentLogicalID)
		if allocateErr != nil {
			return false, allocateErr
		}
		if _, encodeErr := storeio.EncodeDocumentPageWithColumns(documentPage.Bytes(), storeio.DocumentPageHeader{
			StoreID: c.storeID, Generation: generation, LogicalID: documentPage.Ref().LogicalID,
			PageSize: documentPage.Ref().Length, ChunkID: location.Chunk, Live: live,
		}, rows, columns, tx.NextLogicalID(), tx.FileEnd(), uint32(c.options.PageSize)); encodeErr != nil {
			return false, encodeErr
		}
		if stageErr := documentPage.Stage(); stageErr != nil {
			return false, stageErr
		}
		chunkMutation, err = storeio.UpsertChunkTree(c.cache, tx, state.chunkRoot, location.Chunk, documentPage.Ref(), storeio.ChunkTreeBounds{
			FileEnd: state.super.FileEnd, NextLogicalID: state.root.NextLogicalID,
		})
	}
	if err != nil {
		return false, err
	}
	chunkRoot := chunkMutation.Root
	keyMutation, err := storeio.DeleteKeyTree(c.cache, tx, state.keyRoot, key, storeio.KeyTreeBounds{
		FileEnd: state.super.FileEnd, NextLogicalID: state.root.NextLogicalID,
		ChunkHighWater: state.root.ChunkHighWater, ChunkDocuments: uint8(state.root.ChunkDocuments),
	})
	if err != nil || !keyMutation.Found {
		return false, err
	}
	indexRoot, err := c.updateFileIndexes(tx, state, location, &oldIndex, nil)
	if err != nil {
		return false, err
	}
	ttlRoot := state.ttlRoot
	ttlCount := state.root.TTLCount
	if location.Deadline != 0 {
		ttlMutation, ttlErr := storeio.DeleteTTLTree(c.cache, tx, ttlRoot, storeio.TTLKey{
			Deadline: location.Deadline, Chunk: location.Chunk, Slot: location.Slot,
		}, storeio.TTLTreeBounds{
			FileEnd: state.super.FileEnd, NextLogicalID: state.root.NextLogicalID,
			ChunkHighWater: state.root.ChunkHighWater, ChunkDocuments: uint8(state.root.ChunkDocuments),
		})
		if ttlErr != nil {
			return false, ttlErr
		}
		if !ttlMutation.Found {
			return false, storeio.ErrTTLDirectoryCorrupt
		}
		ttlRoot = ttlMutation.Root
		ttlCount--
		if err := c.appendTTLRetirements(state, ttlMutation); err != nil {
			return false, err
		}
	}
	liveChunks := state.root.LiveChunks
	if live == 0 {
		liveChunks--
	}
	float64ScanHead, retireFloat64Scan, err :=
		c.maintainFileFloat64Scan(
			tx, state, chunkRoot, location, &oldIndex, nil, false,
		)
	if err != nil {
		return false, err
	}
	indexGroupHead, retireIndexGroup, err := c.maintainFileIndexGroups(
		tx, state, location, &oldIndex, nil,
		state.root.DocumentCount-1, state.root.ChunkHighWater,
	)
	if err != nil {
		return false, err
	}
	statePage, err := c.reserveFileStatePage(tx)
	if err != nil {
		return false, err
	}
	if err := c.collectFileRetirements(
		state, oldRef, oldView, keyMutation, chunkMutation,
		retireFloat64Scan, retireIndexGroup,
	); err != nil {
		return false, err
	}
	freeLog, err := c.syncFreeLog(tx, state)
	if err != nil {
		return false, err
	}
	nextState, statePage, err := c.stageFileState(
		tx, statePage, state, generation, state.root.ChunkHighWater,
		state.root.DocumentCount-1, ttlCount, liveChunks,
		chunkRoot, keyMutation.Root, indexRoot, ttlRoot,
		float64ScanHead, indexGroupHead, freeLog.head, freeLog.checksum,
	)
	if err != nil {
		return false, err
	}
	if err := c.reserveFileRetirements(); err != nil {
		return false, err
	}
	retirementReserved = true
	if err := tx.Publish(statePage.Ref(), storeio.PageChecksum(statePage.Bytes()), nextState.super.FreeOffset, nextState.super.FreeLength, nextState.super.FreeChecksum); err != nil {
		return false, err
	}
	abort = false
	c.finalizeReusable()
	c.commitFreeLog(freeLog)
	c.snapshotGate.Lock()
	c.pageValidator.update(nextState)
	c.state.Store(nextState)
	c.snapshotGate.Unlock()
	if location.Chunk == c.appendChunk {
		c.appendLive = live
	}
	return true, nil
}

func (c *Collection) validateDocument(src []byte) (vibejson.Index, error) {
	estimate := len(src)/8 + 8
	if estimate < 8 {
		estimate = 8
	}
	if cap(c.parseScratch) < estimate {
		c.parseScratch = make([]vibejson.IndexEntry, estimate)
	}
	for {
		index, err := vibejson.BuildIndexOptions(src, c.parseScratch[:cap(c.parseScratch)], c.options.Collection.IndexOptions)
		if err != document.ErrIndexFull {
			if err != nil {
				return index, err
			}
			if schema := c.options.Collection.Schema; schema != nil {
				if schemaErr := schema.ValidateIndex(index); schemaErr != nil {
					return vibejson.Index{}, schemaErr
				}
			}
			return index, nil
		}
		if cap(c.parseScratch) > c.options.MaxDocumentBytes {
			return vibejson.Index{}, ErrDocumentTooLarge
		}
		c.parseScratch = make([]vibejson.IndexEntry, cap(c.parseScratch)*2)
	}
}

// ensureDirtyCapacity fences prior generations when the frame arena can no
// longer hold one more worst-case transaction. It asks the cache for the
// remaining budget directly instead of taking a full Stats snapshot: Stats
// walks every frame under its lock to build counters this check does not read,
// which made a bound that is O(1) by construction cost O(cache size) per Put.
func (c *Collection) ensureDirtyCapacity() error {
	required := c.options.maxTransactionBytes
	if c.cache.DirtyCapacityAvailable() >= required {
		return nil
	}
	if err := c.committer.Flush(); err != nil {
		return err
	}
	c.cache.MarkDurable(c.committer.DurableGeneration())
	return nil
}

// overflowPageSize returns the smallest legal extent holding one overflow
// piece: the page-size ladder starts at PageSize and doubles, the same search
// fileDocumentPageSize performs for a document extent. A full piece lands
// exactly on MaxPageSize, so the multi-page path is unchanged; only the final
// piece, which is the whole value for anything under one page, shrinks.
//
// A smaller extent stays within the reader's contract: validateOverflowPage
// requires a valid physical page size that is a multiple of the allocation
// quantum and holds the piece, and every page records its own size in its
// header, so pieces of one value need not agree.
func (c *Collection) overflowPageSize(piece int) uint32 {
	needed := storeio.PageHeaderSize + storeio.PageTrailerSize +
		storeio.OverflowPagePayloadHeaderSize + piece
	size := c.options.PageSize
	for size < needed && size < c.options.MaxPageSize {
		size <<= 1
	}
	return uint32(size)
}

func (c *Collection) stageFileValue(tx *storeio.WriteTransaction, location storeio.KeyLocation, key, src []byte) (storeio.DocumentRecord, error) {
	record := storeio.DocumentRecord{Key: key, Slot: location.Slot}
	if len(src) <= c.options.InlineValueBytes {
		record.JSON = src
		return record, nil
	}
	payloadBytes := c.options.MaxPageSize - storeio.PageHeaderSize - storeio.PageTrailerSize - storeio.OverflowPagePayloadHeaderSize
	pageCount := (len(src) + payloadBytes - 1) / payloadBytes
	pages := make([]storeio.TransactionPage, pageCount)
	for i := range pages {
		// Each piece gets the smallest extent that holds it rather than
		// MaxPageSize. Only a full piece needs MaxPageSize; the final piece of
		// any value — and the only piece of every value just past
		// InlineValueBytes — is usually far smaller. Allocating the maximum
		// unconditionally cost a 64 KiB extent for a 513-byte document under
		// the default options, a 128x amplification on exactly the document
		// sizes that overflow first.
		piece := min(payloadBytes, len(src)-i*payloadBytes)
		page, err := tx.Allocate(storeio.PageOverflow, c.overflowPageSize(piece), 0)
		if err != nil {
			return storeio.DocumentRecord{}, err
		}
		pages[i] = page
	}
	position := 0
	for i, page := range pages {
		end := min(position+payloadBytes, len(src))
		var next storeio.PageRef
		if i+1 < len(pages) {
			next = pages[i+1].Ref()
		}
		header := storeio.OverflowPageHeader{
			StoreID: c.storeID, Generation: tx.Generation(), LogicalID: page.Ref().LogicalID,
			PageSize: page.Ref().Length, Chunk: location.Chunk, Slot: location.Slot,
			Total: uint64(len(src)), Offset: uint64(position), Next: next,
		}
		if _, err := storeio.EncodeOverflowPage(
			page.Bytes(), header, src[position:end], tx.FileEnd(), tx.NextLogicalID(),
			uint32(c.options.PageSize), location.Chunk+1, uint8(c.options.Collection.ChunkDocuments),
		); err != nil {
			return storeio.DocumentRecord{}, err
		}
		if err := page.Stage(); err != nil {
			return storeio.DocumentRecord{}, err
		}
		position = end
	}
	record.Overflow = pages[0].Ref()
	record.JSONLength = uint64(len(src))
	return record, nil
}

type fileDocumentLeases struct {
	document storeio.PageLease
	columns  storeio.PageLease
	detached bool
}

func (l *fileDocumentLeases) Release() {
	if l == nil {
		return
	}
	if l.detached {
		l.columns.Release()
	}
	l.document.Release()
}

func (c *Collection) loadFileChunk(state *fileStoreState, chunkID uint32) (storeio.PageRef, *fileDocumentChunk, *fileDocumentLeases, error) {
	if chunkID >= state.root.ChunkHighWater || state.chunkRoot == (storeio.PageRef{}) {
		return storeio.PageRef{}, nil, nil, nil
	}
	ref, ok, err := storeio.LookupChunkTree(c.cache, state.chunkRoot, chunkID, storeio.ChunkTreeBounds{
		FileEnd: state.super.FileEnd, NextLogicalID: state.root.NextLogicalID,
	})
	if err != nil || !ok {
		return storeio.PageRef{}, nil, nil, err
	}
	lease, err := c.cache.Acquire(ref)
	if err != nil {
		return storeio.PageRef{}, nil, nil, err
	}
	view, err := admittedFileDocumentChunk(lease.Page(), ref, chunkID)
	if err != nil {
		lease.Release()
		return storeio.PageRef{}, nil, nil, err
	}
	leases := fileDocumentLeases{document: lease}
	columnsRef, detached, err := storeio.DocumentGroupFloat64Sidecar(
		ref, uint32(c.options.PageSize),
	)
	if err != nil {
		leases.Release()
		return storeio.PageRef{}, nil, nil, err
	}
	if detached {
		columns, acquireErr := c.cache.Acquire(columnsRef)
		if acquireErr != nil {
			leases.Release()
			return storeio.PageRef{}, nil, nil, acquireErr
		}
		leases.columns = columns
		leases.detached = true
		if attachErr := view.attachFloat64Group(columns.Page()); attachErr != nil {
			leases.Release()
			return storeio.PageRef{}, nil, nil, attachErr
		}
	}
	return ref, &view, &leases, nil
}

// fileChunkEdit is one slot of a chunk rebuild. A batch supplies several, in
// strictly ascending slot order; keep false removes the slot.
type fileChunkEdit struct {
	record storeio.DocumentRecord
	slot   uint8
	keep   bool
}

// buildFileRows materialises the replacement row set for one chunk page,
// applying every edit that lands in it in a single pass.
//
// Taking a slice of edits rather than one target slot is what lets a batched
// commit rebuild a 64-document page once instead of once per document. The
// per-slot work is unchanged; only the number of times the page is rebuilt is.
func (c *Collection) buildFileRows(state *fileStoreState, old *fileDocumentChunk, edits []fileChunkEdit) ([]storeio.DocumentRecord, uint64, error) {
	if cap(c.rowScratch) < store.MaxChunkDocuments {
		c.rowScratch = make([]storeio.DocumentRecord, store.MaxChunkDocuments)
	}
	storage := c.rowScratch[:store.MaxChunkDocuments]
	c.documentValueScratch = c.documentValueScratch[:0]
	position := 0
	var live uint64
	at := 0
	for slot := uint8(0); slot < uint8(c.options.Collection.ChunkDocuments); slot++ {
		if at < len(edits) && edits[at].slot == slot {
			edit := edits[at]
			at++
			if edit.keep {
				storage[position] = edit.record
				position++
				live |= uint64(1) << slot
				continue
			}
			if old != nil {
				if _, existed := old.lookup(slot); !existed {
					return nil, 0, storeio.ErrDocumentPageCorrupt
				}
			}
			continue
		}
		if old == nil {
			continue
		}
		record, ok := old.lookup(slot)
		if !ok {
			continue
		}
		json := record.value.value.Inline
		if record.value.grouped {
			var appendErr error
			start := len(c.documentValueScratch)
			c.documentValueScratch, appendErr = c.appendFileDocumentValue(
				c.documentValueScratch, state, *old, record.value,
				storeio.KeyLocation{Chunk: old.chunk, Slot: slot},
			)
			if appendErr != nil {
				return nil, 0, appendErr
			}
			json = c.documentValueScratch[start:]
		}
		stored := storeio.DocumentRecord{
			Key: record.key, JSON: json, Overflow: record.value.value.Overflow, Slot: slot,
		}
		if stored.Overflow != (storeio.PageRef{}) {
			stored.JSONLength = record.value.value.Length
		}
		storage[position] = stored
		position++
		live |= uint64(1) << slot
	}
	if at != len(edits) {
		return nil, 0, storeio.ErrDocumentPageCorrupt
	}
	return storage[:position], live, nil
}

// seedFileFloat64Columns loads a chunk's surviving typed covering values into
// the writer's fixed column scratch, dropping every slot the caller is about to
// rewrite. Callers then record each replacement with setFileFloat64Column.
//
// Seeding and recording are separate steps so that a batch never has to hold
// every replacement document's parsed index alive at once: each document's
// projection is extracted while its index is still on the stack and discarded
// immediately, which keeps a 64-document batch's peak memory the same as a
// single Put's.
func (c *Collection) seedFileFloat64Columns(state *fileStoreState, old *fileDocumentChunk, edited uint64) error {
	if state == nil || state.root.Options&storeio.StateOptionFloat64Columns == 0 {
		return nil
	}
	if len(c.float64Masks) != len(c.options.float64Columns) ||
		len(c.float64Values) != len(c.options.float64Columns)*64 {
		return storeio.ErrDocumentPageCorrupt
	}
	clear(c.float64Masks)
	if old == nil {
		return nil
	}
	if old.float64ColumnCount() != len(c.options.float64Columns) {
		return storeio.ErrDocumentPageCorrupt
	}
	for column := range c.options.float64Columns {
		view, ok := old.float64Column(column)
		if !ok {
			return storeio.ErrDocumentPageCorrupt
		}
		iterator := view.Iterator()
		for {
			slot, value, present := iterator.Next()
			if !present {
				break
			}
			if edited&(uint64(1)<<slot) != 0 {
				continue
			}
			c.float64Masks[column] |= uint64(1) << slot
			c.float64Values[column*64+int(slot)] = value
		}
	}
	return nil
}

// setFileFloat64Column records one replacement document's typed projection into
// the seeded column scratch.
func (c *Collection) setFileFloat64Column(state *fileStoreState, slot uint8, replacement *vibejson.Index) error {
	if state == nil || state.root.Options&storeio.StateOptionFloat64Columns == 0 {
		return nil
	}
	if replacement == nil {
		return storeio.ErrDocumentPageCorrupt
	}
	for column, definition := range c.options.float64Columns {
		node, ok, err := replacement.PointerCompiled(definition.pointer)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		value, ok := node.Raw().Float64()
		if !ok || math.IsNaN(value) || math.IsInf(value, 0) {
			continue
		}
		c.float64Masks[column] |= uint64(1) << slot
		c.float64Values[column*64+int(slot)] = value
	}
	return nil
}

// fileFloat64Columns exposes the seeded scratch as the encoder's column input.
func (c *Collection) fileFloat64Columns(state *fileStoreState) storeio.DocumentFloat64Columns {
	if state == nil || state.root.Options&storeio.StateOptionFloat64Columns == 0 {
		return storeio.DocumentFloat64Columns{}
	}
	return storeio.DocumentFloat64Columns{Masks: c.float64Masks, Values: c.float64Values}
}

func (c *Collection) buildFileFloat64Columns(state *fileStoreState, old *fileDocumentChunk, target uint8, replacement *vibejson.Index, keep bool) (storeio.DocumentFloat64Columns, error) {
	if err := c.seedFileFloat64Columns(state, old, uint64(1)<<target); err != nil {
		return storeio.DocumentFloat64Columns{}, err
	}
	if keep {
		if err := c.setFileFloat64Column(state, target, replacement); err != nil {
			return storeio.DocumentFloat64Columns{}, err
		}
	}
	return c.fileFloat64Columns(state), nil
}

func (c *Collection) fileDocumentPageSize(rows []storeio.DocumentRecord, columns storeio.DocumentFloat64Columns) (uint32, error) {
	needed := storeio.PageHeaderSize + storeio.PageTrailerSize + storeio.DocumentPagePayloadHeaderSize + len(rows)*storeio.DocumentPageRecordSize
	for _, row := range rows {
		needed += len(row.Key)
		if row.Overflow == (storeio.PageRef{}) {
			needed += len(row.JSON)
		} else {
			needed += storeio.DocumentOverflowDescriptorSize
		}
	}
	for _, mask := range columns.Masks {
		needed += 8 + bits.OnesCount64(mask)*8
	}
	size := c.options.PageSize
	for size < needed && size < c.options.MaxPageSize {
		size <<= 1
	}
	if size < needed || size > c.options.MaxPageSize {
		return 0, ErrDocumentTooLarge
	}
	return uint32(size), nil
}

// reserveFileStatePage claims the publication root's extent while the free log
// is still watching. Every other page a commit writes is allocated before
// syncFreeLog runs; the state root used to be allocated after it, so the extent
// it consumed was the one allocation no delta described, and the replayed free
// set would have handed that live page out again.
func (c *Collection) reserveFileStatePage(tx *storeio.WriteTransaction) (storeio.TransactionPage, error) {
	return tx.Allocate(storeio.PageStateRoot, uint32(c.options.PageSize), storeio.StateRootLogicalID)
}

// stageFileState encodes the publication root into a page the caller reserved
// with reserveFileStatePage. Reserving and encoding are separate steps because
// the state page is itself allocated out of the free set, and the free log can
// only record an allocation it has already seen: reserving first puts the state
// page inside this commit's diff, while encoding last still sees the final
// FileEnd and NextLogicalID the log's own pages moved.
func (c *Collection) stageFileState(
	tx *storeio.WriteTransaction,
	statePage storeio.TransactionPage,
	old *fileStoreState,
	generation uint64,
	chunkHighWater uint32,
	documentCount, ttlCount uint64,
	liveChunks uint32,
	chunkRoot, keyRoot, indexRoot, ttlRoot, float64ScanHead, indexGroupHead,
	freeHead storeio.PageRef,
	freeChecksum uint32,
) (*fileStoreState, storeio.TransactionPage, error) {
	root := storeio.StateRoot{
		StoreID: c.storeID, Generation: generation, PageSize: uint32(c.options.PageSize),
		Options:       old.root.Options,
		DocumentCount: documentCount, TTLCount: ttlCount, NextLogicalID: tx.NextLogicalID(),
		ChunkHighWater: chunkHighWater, LiveChunks: liveChunks,
		ChunkDocuments: uint32(c.options.Collection.ChunkDocuments),
		IndexCount:     uint32(len(c.options.indexes)), IndexCatalogHash: c.options.indexCatalogHash,
		ChunkDirectory: chunkRoot, KeyDirectory: keyRoot, IndexDirectory: indexRoot, TTLDirectory: ttlRoot,
		Float64ScanHead: float64ScanHead,
		IndexGroupHead:  indexGroupHead,
	}
	if _, err := storeio.EncodeStateRootPage(statePage.Bytes(), root, tx.FileEnd()); err != nil {
		return nil, storeio.TransactionPage{}, err
	}
	if err := statePage.Stage(); err != nil {
		return nil, storeio.TransactionPage{}, err
	}
	super := storeio.Superblock{
		StoreID: c.storeID, Generation: generation,
		StateOffset: statePage.Ref().Offset, StateLength: statePage.Ref().Length,
		StateChecksum: storeio.PageChecksum(statePage.Bytes()), FileEnd: tx.FileEnd(),
		PageSize: uint32(c.options.PageSize),
	}
	if freeHead != (storeio.PageRef{}) {
		super.FreeOffset = freeHead.Offset
		super.FreeLength = freeHead.Length
		super.FreeChecksum = freeChecksum
	}
	return &fileStoreState{
		root: root, super: super, stateRef: statePage.Ref(),
		keyRoot: keyRoot, chunkRoot: chunkRoot, indexRoot: indexRoot,
		ttlRoot: ttlRoot, freeHead: freeHead,
	}, statePage, nil
}

// collectFileRetirements lists the extents this commit makes unreachable. It
// runs before syncFreeLog rather than after, because the free log now writes
// those extents down in the same commit that retires them: a retirement that
// only reached memory was abandoned at the next Close, which cost a fixed three
// pages per open-write-close cycle and grew the file without bound across
// restarts. It reserves nothing — reserveFileRetirements does that after the
// free log has added its own superseded pages to the same list.
func (c *Collection) collectFileRetirements(
	old *fileStoreState,
	oldDocument storeio.PageRef,
	oldView *fileDocumentChunk,
	key storeio.KeyTreeMutation,
	chunk storeio.ChunkTreeMutation,
	retireFloat64Scan bool,
	retireIndexGroup bool,
) error {
	appendRef := func(ref storeio.PageRef) error {
		if ref == (storeio.PageRef{}) {
			return nil
		}
		if len(c.retireScratch) == cap(c.retireScratch) {
			return storeio.ErrRetiredExtentCapacity
		}
		c.retireScratch = append(c.retireScratch, storeio.FreeExtent{
			Offset: ref.Offset, Length: uint64(ref.Length), RetiredGeneration: old.root.Generation,
		})
		return nil
	}
	if err := appendRef(old.stateRef); err != nil {
		return err
	}
	if retireFloat64Scan && old.root.Float64ScanHead != (storeio.PageRef{}) {
		if err := c.appendFloat64ScanRetirements(old); err != nil {
			return err
		}
	}
	if retireIndexGroup {
		if err := c.appendIndexGroupRetirements(old); err != nil {
			return err
		}
	}
	if err := c.appendDocumentRetirement(old, oldDocument, oldView); err != nil {
		return err
	}
	for i := 0; i < int(key.RetiredCount); i++ {
		if err := appendRef(key.Retired[i]); err != nil {
			return err
		}
	}
	for i := 0; i < int(chunk.RetiredCount); i++ {
		if err := appendRef(chunk.Retired[i]); err != nil {
			return err
		}
	}
	return nil
}

// appendDocumentRetirement releases the extent a replaced chunk page occupied.
// A bulk-built PageDocumentGroup covers several chunks at once, so it may only
// be retired when no other chunk still names it; an ordinary PageDocument is
// owned by exactly one chunk and is released unconditionally.
func (c *Collection) appendDocumentRetirement(
	old *fileStoreState, oldDocument storeio.PageRef, oldView *fileDocumentChunk,
) error {
	appendRef := func(ref storeio.PageRef) error {
		if ref == (storeio.PageRef{}) {
			return nil
		}
		if len(c.retireScratch) == cap(c.retireScratch) {
			return storeio.ErrRetiredExtentCapacity
		}
		c.retireScratch = append(c.retireScratch, storeio.FreeExtent{
			Offset: ref.Offset, Length: uint64(ref.Length), RetiredGeneration: old.root.Generation,
		})
		return nil
	}
	if oldDocument.Kind == storeio.PageDocumentGroup {
		if oldView == nil {
			return storeio.ErrDocumentGroupCorrupt
		}
		header, ok := oldView.groupHeader()
		if !ok {
			return storeio.ErrDocumentGroupCorrupt
		}
		shared, err := storeio.ChunkTreeHasOtherReference(
			c.cache, old.chunkRoot, header.FirstChunk, header.ChunkCount,
			oldView.chunk, oldDocument, storeio.ChunkTreeBounds{
				FileEnd: old.super.FileEnd, NextLogicalID: old.root.NextLogicalID,
			},
		)
		if err != nil {
			return err
		}
		if !shared {
			if err := appendRef(oldDocument); err != nil {
				return err
			}
			columns, detached, deriveErr := storeio.DocumentGroupFloat64Sidecar(
				oldDocument, uint32(c.options.PageSize),
			)
			if deriveErr != nil {
				return deriveErr
			}
			if detached {
				columnsHeader, ok := oldView.detachedFloat64Header()
				if !ok || columnsHeader.LogicalID != columns.LogicalID {
					return storeio.ErrFloat64GroupCorrupt
				}
				sharedColumns, referenceErr := storeio.ChunkTreeHasOtherFloat64Sidecar(
					c.cache, old.chunkRoot, columnsHeader.FirstChunk, columnsHeader.ChunkCount,
					oldView.chunk, columns, uint32(c.options.PageSize), storeio.ChunkTreeBounds{
						FileEnd: old.super.FileEnd, NextLogicalID: old.root.NextLogicalID,
					},
				)
				if referenceErr != nil {
					return referenceErr
				}
				if !sharedColumns {
					if err := appendRef(columns); err != nil {
						return err
					}
				}
			}
		}
	} else if err := appendRef(oldDocument); err != nil {
		return err
	}
	return nil
}

// absorbRetirementPressure decides what a full retirement table means for the
// write that filled it. The answer differs entirely depending on whether a
// reader is responsible, and the old code gave the same bare sentinel to both.
//
// With a snapshot open, the extents genuinely might be dereferenced again, so
// the write fails. That is bounded, recoverable backpressure — closing the
// snapshot lets the next commit drain the set and the writer resumes with
// nothing lost — but the operator has to be told which snapshot to close, and
// "retired extent capacity exhausted" reads like corruption rather than like a
// reader holding a lease. The message now names the pinned generation.
//
// With no reader pinning anything, failing is simply wrong. The extents are
// already unreachable from the new root; the only thing missing is room to
// record that fact. Refusing the write there stalls a store nothing is reading,
// and the stall clears only by restarting the process — which discards the
// whole pending set anyway, reaching this same state at the cost of an outage.
// So reach it without the outage: forget the extents, count them, and keep
// writing. This package's stated asymmetry is that under-reporting free space
// leaks and is recoverable while over-reporting hands out live pages and is
// not, and a leak is the recoverable side of that line.
//
// The sentinel is wrapped, not replaced, so errors.Is keeps working.
func (c *Collection) absorbRetirementPressure(err error) error {
	if c == nil || !errors.Is(err, storeio.ErrRetiredExtentCapacity) {
		return err
	}
	retired := c.reclaimer.Stats()
	current := uint64(0)
	if state := c.state.Load(); state != nil {
		current = state.root.Generation
	}
	leases := c.leases.Stats(current)
	if leases.Active != 0 && leases.MinimumGeneration <= current {
		return fmt.Errorf(
			"%w: %d of %d retired extents (%d bytes) are pinned by %d open snapshot(s), "+
				"the oldest at generation %d against current generation %d; "+
				"close those snapshots or raise Options.MaxRetiredExtents",
			err, retired.Pending, retired.Capacity, retired.PendingBytes,
			leases.Active, leases.MinimumGeneration, current)
	}
	bytes := uint64(0)
	for _, extent := range c.retireScratch {
		bytes += extent.Length
	}
	c.abandonedBytes.Add(bytes)
	c.abandonedExtents.Add(uint64(len(c.retireScratch)))
	return nil
}

// reserveFileRetirements hands the complete list to the reclaimer. It runs after
// syncFreeLog so that the free log's own superseded pages — which a fold only
// knows once it has decided to fold — are reserved with everything else, and so
// that a failure here still precedes Publish and rolls the whole commit back.
//
// A full retirement table is routed through absorbRetirementPressure rather than
// returned bare: whether it is recoverable backpressure or a wedge nothing can
// clear depends on if a reader is actually responsible, and only that function
// can tell the two apart.
func (c *Collection) reserveFileRetirements() error {
	if err := c.reclaimer.RetireBatch(c.retireScratch); err != nil {
		return c.absorbRetirementPressure(err)
	}
	return nil
}

func (c *Collection) appendIndexGroupRetirements(
	old *fileStoreState,
) error {
	var previous storeio.PageRef
	for ref := old.root.IndexGroupHead; ref != (storeio.PageRef{}); {
		lease, err := c.cache.Acquire(ref)
		if err != nil {
			return err
		}
		catalog := storeio.AdmittedIndexGroupCatalog(lease.Page())
		next := catalog.Header().Next
		if previous != (storeio.PageRef{}) &&
			(ref.Offset <= previous.Offset ||
				ref.LogicalID <= previous.LogicalID) {
			lease.Release()
			return storeio.ErrIndexGroupCatalogCorrupt
		}
		lease.Release()
		length := uint64(ref.Length)
		if len(c.retireScratch) != 0 {
			last := &c.retireScratch[len(c.retireScratch)-1]
			if last.RetiredGeneration == old.root.Generation &&
				last.Offset <= ^uint64(0)-last.Length &&
				last.Offset+last.Length == ref.Offset &&
				last.Length <= ^uint64(0)-length {
				last.Length += length
			} else if err := c.appendIndexRetiredRef(old, ref); err != nil {
				return err
			}
		} else if err := c.appendIndexRetiredRef(old, ref); err != nil {
			return err
		}
		previous = ref
		ref = next
	}
	return nil
}

// appendFloat64ScanRetirements releases a complete aggregate-only projection
// after an out-of-range insert or incremental-rebuild fallback. Bulk stripes
// and ordered-directory levels are allocated as one physical run. Adjacent
// refs are folded into one retirement record so reclamation metadata remains
// O(1) for a large compact generation. TTL-only and projection-neutral
// publications retain the projection.
//
// Authoritative detached PageFloat64Group sidecars are not catalog entries
// and remain reachable from document refs.
func (c *Collection) appendFloat64ScanRetirements(old *fileStoreState) error {
	appendRef := func(ref storeio.PageRef) error {
		if ref == (storeio.PageRef{}) {
			return nil
		}
		length := uint64(ref.Length)
		if len(c.retireScratch) != 0 {
			last := &c.retireScratch[len(c.retireScratch)-1]
			if last.RetiredGeneration == old.root.Generation &&
				last.Offset <= ^uint64(0)-last.Length &&
				last.Offset+last.Length == ref.Offset &&
				last.Length <= ^uint64(0)-length {
				last.Length += length
				return nil
			}
		}
		if len(c.retireScratch) == cap(c.retireScratch) {
			return storeio.ErrRetiredExtentCapacity
		}
		c.retireScratch = append(c.retireScratch, storeio.FreeExtent{
			Offset: ref.Offset, Length: length, RetiredGeneration: old.root.Generation,
		})
		return nil
	}
	bounds := storeio.Float64DirectoryBounds{
		FileEnd:       old.super.FileEnd,
		NextLogicalID: old.root.NextLogicalID,
	}
	err := storeio.WalkFloat64DirectoryLeaves(
		c.cache, old.root.Float64ScanHead, bounds,
		uint32(c.options.PageSize),
		func(leaf storeio.Float64DirectoryView) error {
			for i := 0; i < leaf.Len(); i++ {
				entry, _ := leaf.EntryAt(i)
				if err := appendRef(entry.Ref); err != nil {
					return err
				}
			}
			return nil
		},
	)
	if err != nil {
		return err
	}
	return storeio.WalkFloat64DirectoryPages(
		c.cache, old.root.Float64ScanHead, bounds,
		uint32(c.options.PageSize), appendRef,
	)
}

func (c *Collection) appendTTLRetirements(old *fileStoreState, mutation storeio.TTLTreeMutation) error {
	for i := 0; i < int(mutation.RetiredCount); i++ {
		if len(c.retireScratch) == cap(c.retireScratch) {
			return storeio.ErrRetiredExtentCapacity
		}
		ref := mutation.Retired[i]
		c.retireScratch = append(c.retireScratch, storeio.FreeExtent{
			Offset: ref.Offset, Length: uint64(ref.Length), RetiredGeneration: old.root.Generation,
		})
	}
	return nil
}

func (c *Collection) appendOverflowRetirements(state *fileStoreState, value storeio.DocumentValue, location storeio.KeyLocation) error {
	ref := value.Overflow
	if ref == (storeio.PageRef{}) {
		return nil
	}
	offset := uint64(0)
	for ref != (storeio.PageRef{}) {
		if len(c.retireScratch) == cap(c.retireScratch) {
			return storeio.ErrRetiredExtentCapacity
		}
		lease, err := c.cache.Acquire(ref)
		if err != nil {
			return err
		}
		view, err := storeio.OpenOverflowPage(
			lease.Page(), state.super.FileEnd, state.root.NextLogicalID,
			state.root.PageSize, state.root.ChunkHighWater, uint8(state.root.ChunkDocuments),
		)
		if err != nil {
			lease.Release()
			return err
		}
		header := view.Header()
		if header.Chunk != location.Chunk || header.Slot != location.Slot ||
			header.Total != value.Length || header.Offset != offset {
			lease.Release()
			return storeio.ErrOverflowPageCorrupt
		}
		c.retireScratch = append(c.retireScratch, storeio.FreeExtent{
			Offset: ref.Offset, Length: uint64(ref.Length), RetiredGeneration: state.root.Generation,
		})
		offset += uint64(len(view.Data()))
		ref = header.Next
		lease.Release()
	}
	if offset != value.Length {
		return storeio.ErrOverflowPageCorrupt
	}
	return nil
}

func (c *Collection) appendFileValue(dst []byte, state *fileStoreState, value storeio.DocumentValue, location storeio.KeyLocation) ([]byte, error) {
	if value.Overflow == (storeio.PageRef{}) {
		return append(dst, value.Inline...), nil
	}
	ref := value.Overflow
	offset := uint64(0)
	for ref != (storeio.PageRef{}) {
		lease, err := c.cache.Acquire(ref)
		if err != nil {
			return dst, err
		}
		view, err := storeio.OpenOverflowPage(
			lease.Page(), state.super.FileEnd, state.root.NextLogicalID,
			state.root.PageSize, state.root.ChunkHighWater, uint8(state.root.ChunkDocuments),
		)
		if err != nil {
			lease.Release()
			return dst, err
		}
		header := view.Header()
		if header.Chunk != location.Chunk || header.Slot != location.Slot ||
			header.Total != value.Length || header.Offset != offset {
			lease.Release()
			return dst, storeio.ErrOverflowPageCorrupt
		}
		dst = append(dst, view.Data()...)
		offset += uint64(len(view.Data()))
		ref = header.Next
		lease.Release()
	}
	if offset != value.Length {
		return dst, storeio.ErrOverflowPageCorrupt
	}
	return dst, nil
}

func (c *Collection) buildOldFileIndex(src []byte) (vibejson.Index, error) {
	needed, err := vibejson.RequiredIndexEntries(src)
	if err != nil {
		return vibejson.Index{}, err
	}
	if cap(c.oldParseScratch) < needed {
		c.oldParseScratch = make([]vibejson.IndexEntry, needed)
	}
	return vibejson.BuildIndexOptions(src, c.oldParseScratch[:needed], c.options.Collection.IndexOptions)
}

func fileIndexTupleHash(exact *store.ExactIndex, index vibejson.Index) (uint64, bool, error) {
	hash := uint64(14695981039346656037)
	for i := 0; i < int(exact.N); i++ {
		node, ok, err := index.PointerCompiled(exact.Paths[i])
		if err != nil || !ok {
			return 0, false, err
		}
		hash, ok = fileIndexHashNode(hash, node)
		if !ok {
			return 0, false, nil
		}
	}
	return hash, true, nil
}

func fileIndexTuplesEqual(exact *store.ExactIndex, left, right vibejson.Index) (bool, error) {
	if exact == nil || exact.N == 0 {
		return false, nil
	}
	for column := range int(exact.N) {
		leftNode, leftOK, err := left.PointerCompiled(exact.Paths[column])
		if err != nil {
			return false, err
		}
		rightNode, rightOK, err := right.PointerCompiled(exact.Paths[column])
		if err != nil {
			return false, err
		}
		if !leftOK || !rightOK ||
			!fileIndexRawValuesEqual(leftNode.Raw(), rightNode.Raw()) {
			return false, nil
		}
	}
	return true, nil
}

func fileIndexNeedleHash(exact *store.ExactIndex, values []vibejson.Index) (uint64, error) {
	if len(values) != int(exact.N) {
		return 0, store.ErrIndexArity
	}
	hash := uint64(14695981039346656037)
	for _, value := range values {
		var ok bool
		hash, ok = fileIndexHashNode(hash, value.Root())
		if !ok {
			return 0, store.ErrIndexScalar
		}
	}
	return hash, nil
}

func fileIndexHashNode(hash uint64, node vibejson.Node) (uint64, bool) {
	raw := node.Raw()
	hash = fileIndexHashBytes(hash, []byte{byte(raw.Kind()), 0xff})
	switch raw.Kind() {
	case document.Null:
	case document.Bool:
		value, _ := raw.Bool()
		if value {
			hash = fileIndexHashBytes(hash, []byte{1})
		} else {
			hash = fileIndexHashBytes(hash, []byte{0})
		}
	case document.Number:
		if value, ok := raw.Float64(); ok {
			if value == 0 {
				value = 0
			}
			var encoded [8]byte
			binary.LittleEndian.PutUint64(encoded[:], math.Float64bits(value))
			hash = fileIndexHashBytes(hash, encoded[:])
		} else {
			hash = fileIndexHashBytes(hash, []byte{0x7f})
		}
	case document.String:
		if text, clean := raw.StringBytes(); clean {
			hash = fileIndexHashBytes(hash, text)
		} else {
			text, ok := node.AppendText(nil)
			if !ok {
				return 0, false
			}
			hash = fileIndexHashBytes(hash, text)
		}
	default:
		return 0, false
	}
	hash = fileIndexHashBytes(hash, []byte{0xfe})
	return hash, true
}

func (c *Collection) updateFileIndexes(tx *storeio.WriteTransaction, state *fileStoreState, location storeio.KeyLocation, oldIndex, newIndex *vibejson.Index) (storeio.PageRef, error) {
	root := state.indexRoot
	for indexID, exact := range c.options.indexes {
		var oldHash, newHash uint64
		var oldOK, newOK bool
		var err error
		if oldIndex != nil {
			oldHash, oldOK, err = fileIndexTupleHash(exact, *oldIndex)
			if err != nil {
				return storeio.PageRef{}, err
			}
		}
		if newIndex != nil {
			newHash, newOK, err = fileIndexTupleHash(exact, *newIndex)
			if err != nil {
				return storeio.PageRef{}, err
			}
		}
		if oldOK && newOK && oldHash == newHash {
			equal, equalErr := fileIndexTuplesEqual(exact, *oldIndex, *newIndex)
			if equalErr != nil {
				return storeio.PageRef{}, equalErr
			}
			if equal {
				continue
			}
			newCertificate, certificateErr := c.fileIndexCertificate(
				c.indexNewCertificate[:0], exact, *newIndex,
			)
			if certificateErr != nil {
				return storeio.PageRef{}, certificateErr
			}
			c.indexNewCertificate = newCertificate
			root, err = c.mutateFilePosting(
				tx, state, root, uint32(indexID), oldHash, location, true,
				newCertificate,
			)
			if err != nil {
				return storeio.PageRef{}, err
			}
			continue
		}
		if oldOK {
			root, err = c.mutateFilePosting(
				tx, state, root, uint32(indexID), oldHash, location, false, nil,
			)
			if err != nil {
				return storeio.PageRef{}, err
			}
		}
		if newOK {
			newCertificate, certificateErr := c.fileIndexCertificate(
				c.indexNewCertificate[:0], exact, *newIndex,
			)
			if certificateErr != nil {
				return storeio.PageRef{}, certificateErr
			}
			c.indexNewCertificate = newCertificate
			root, err = c.mutateFilePosting(
				tx, state, root, uint32(indexID), newHash, location, true,
				newCertificate,
			)
			if err != nil {
				return storeio.PageRef{}, err
			}
		}
	}
	return root, nil
}

func (c *Collection) fileIndexCertificate(dst []byte, exact *store.ExactIndex, index vibejson.Index) ([]byte, error) {
	if exact == nil || exact.N == 0 {
		return nil, nil
	}
	var values [store.MaxIndexColumns]vibejson.RawValue
	for column := range int(exact.N) {
		node, ok, err := index.PointerCompiled(exact.Paths[column])
		if err != nil || !ok {
			return nil, err
		}
		values[column] = node.Raw()
	}
	certificate, ok := appendFileIndexCertificate(
		dst, values[:exact.N],
		storeio.IndexDirectoryMaxCertificate(uint32(c.options.PageSize)),
	)
	if !ok {
		return nil, nil
	}
	return certificate, nil
}

func (c *Collection) mutateFilePosting(
	tx *storeio.WriteTransaction,
	state *fileStoreState,
	root storeio.PageRef,
	indexID uint32,
	tupleHash uint64,
	location storeio.KeyLocation,
	present bool,
	newCertificate []byte,
) (storeio.PageRef, error) {
	key := storeio.IndexDirectoryKey{IndexID: indexID, TupleHash: tupleHash, Chunk: location.Chunk}
	bounds := storeio.IndexTreeBounds{
		FileEnd: tx.FileEnd(), NextLogicalID: tx.NextLogicalID(), IndexHighWater: uint32(len(c.options.indexes)),
	}
	// LookupIndexTree copies the representative out before it drops the leaf
	// lease, so the scratch below never aliases an evictable page frame.
	entry, certificate, found, err := storeio.LookupIndexTree(
		c.cache, root, key, bounds, c.indexCertificateScratch[:0],
	)
	c.indexCertificateScratch = certificate
	if err != nil {
		return storeio.PageRef{}, err
	}
	mask := uint64(0)
	collision := false
	if found {
		if len(c.indexCertificateScratch) != 0 &&
			!fileIndexCertificateValid(
				c.indexCertificateScratch, int(c.options.indexes[indexID].N),
			) {
			return storeio.PageRef{}, storeio.ErrIndexDirectoryCorrupt
		}
		collision = entry.Flags&storeio.IndexEntryCollision != 0
		mask = entry.Bits
	}
	bit := uint64(1) << location.Slot
	if present {
		if len(newCertificate) == 0 {
			c.indexCertificateScratch = c.indexCertificateScratch[:0]
			collision = false
		} else if len(c.indexCertificateScratch) == 0 {
			if found {
				// An existing entry without a representative cannot prove that
				// its bits all belong to the new value, and a live entry always
				// carries a non-empty mask, so those bits really are at stake.
				collision = true
			}
			c.indexCertificateScratch = append(
				c.indexCertificateScratch, newCertificate...,
			)
		} else if !fileIndexCertificatesEqual(
			c.indexCertificateScratch, newCertificate,
			int(c.options.indexes[indexID].N),
		) {
			collision = true
		}
		mask |= bit
	} else {
		mask &^= bit
	}
	if mask == 0 {
		mutation, deleteErr := storeio.DeleteIndexTree(c.cache, tx, root, key, bounds)
		if deleteErr != nil {
			return storeio.PageRef{}, deleteErr
		}
		if !mutation.Found {
			return storeio.PageRef{}, storeio.ErrIndexDirectoryCorrupt
		}
		if err := c.appendIndexRetirements(state, mutation); err != nil {
			return storeio.PageRef{}, err
		}
		return mutation.Root, nil
	}
	// The mask now lives in the routing record itself, so one changed value
	// costs one rewritten leaf rather than a leaf plus a whole page holding a
	// single 64-bit word. There is nothing left to retire here either: the leaf
	// rewrite reports its own retirements below.
	flags := uint16(0)
	if collision {
		flags |= storeio.IndexEntryCollision
	}
	mutation, err := storeio.UpsertIndexTree(c.cache, tx, root, storeio.IndexDirectoryEntry{
		Key: key, Bits: mask, Flags: flags, Kind: storeio.IndexEntryInlineMask,
		Cert: storeio.CertSpan{Length: uint16(len(c.indexCertificateScratch))},
	}, c.indexCertificateScratch, bounds)
	if err != nil {
		return storeio.PageRef{}, err
	}
	if err := c.appendIndexRetirements(state, mutation); err != nil {
		return storeio.PageRef{}, err
	}
	return mutation.Root, nil
}

func (c *Collection) appendIndexRetiredRef(state *fileStoreState, ref storeio.PageRef) error {
	if len(c.retireScratch) == cap(c.retireScratch) {
		return storeio.ErrRetiredExtentCapacity
	}
	c.retireScratch = append(c.retireScratch, storeio.FreeExtent{
		Offset: ref.Offset, Length: uint64(ref.Length), RetiredGeneration: state.root.Generation,
	})
	return nil
}

func (c *Collection) appendIndexRetirements(state *fileStoreState, mutation storeio.IndexTreeMutation) error {
	for i := 0; i < int(mutation.RetiredCount); i++ {
		if err := c.appendIndexRetiredRef(state, mutation.Retired[i]); err != nil {
			return err
		}
	}
	return nil
}

func fileStoreLiveMask(chunkDocuments uint32) uint64 {
	if chunkDocuments >= 64 {
		return ^uint64(0)
	}
	return uint64(1)<<chunkDocuments - 1
}

func (c *Collection) restoreAppendChunk(state *fileStoreState) error {
	if state.root.ChunkHighWater == 0 || state.chunkRoot == (storeio.PageRef{}) {
		return nil
	}
	last := state.root.ChunkHighWater - 1
	ref, ok, err := storeio.LookupChunkTree(c.cache, state.chunkRoot, last, storeio.ChunkTreeBounds{
		FileEnd: state.super.FileEnd, NextLogicalID: state.root.NextLogicalID,
	})
	if err != nil || !ok {
		return err
	}
	lease, err := c.cache.Acquire(ref)
	if err != nil {
		return err
	}
	view, viewErr := admittedFileDocumentChunk(lease.Page(), ref, last)
	lease.Release()
	if viewErr != nil {
		return viewErr
	}
	limit := ^uint64(0)
	if state.root.ChunkDocuments < 64 {
		limit = uint64(1)<<state.root.ChunkDocuments - 1
	}
	if view.live() != limit {
		c.appendChunk = last
		c.appendLive = view.live()
	}
	return nil
}

func (c *Collection) waitPublished(generation uint64) error {
	if err := c.committer.Wait(generation); err != nil {
		return err
	}
	c.cache.MarkDurable(generation)
	return nil
}

// Flush waits until the current reader-visible generation is crash-safe.
func (c *Collection) Flush() error {
	if c == nil || c.committer == nil {
		return ErrClosed
	}
	generation := c.Generation()
	if err := c.committer.Wait(generation); err != nil {
		return err
	}
	c.cache.MarkDurable(generation)
	return nil
}

// Close fences every publication and releases bounded I/O resources. It does
// not close the caller-owned file. Active snapshots must be closed first.
func (c *Collection) Close() error {
	if c == nil {
		return nil
	}
	c.writer.Lock()
	if c.closeDone {
		c.writer.Unlock()
		return nil
	}
	c.closed = true
	c.writer.Unlock()
	// Synchronous publishers release the construction lock before their
	// durability wait so independent writers can share one device commit.
	// Closed prevents any new waiter from registering before this drain.
	c.durabilityWait.Wait()
	if err := c.leases.Close(); err != nil {
		return err
	}
	if err := c.closeResources(); err != nil {
		return err
	}
	c.writer.Lock()
	c.closeDone = true
	c.writer.Unlock()
	return nil
}

func (c *Collection) closeResources() error {
	var result error
	if c.committer != nil {
		if err := c.committer.Close(); err != nil {
			result = errors.Join(result, err)
		}
		c.cache.MarkDurable(c.committer.DurableGeneration())
	}
	if c.cache != nil {
		if err := c.cache.Close(); err != nil {
			result = errors.Join(result, err)
		}
	}
	if c.readFile != nil {
		readFile := c.readFile
		c.readFile = nil
		if err := readFile.Close(); err != nil {
			result = errors.Join(result, err)
		}
	}
	if c.writeFile != nil {
		writeFile := c.writeFile
		c.writeFile = nil
		if err := writeFile.Close(); err != nil {
			result = errors.Join(result, err)
		}
	}
	if c.reusableBlock != nil {
		if err := c.reusableBlock.Close(); err != nil {
			result = errors.Join(result, err)
		}
		c.reusableBlock = nil
		c.reusable = nil
	}
	if c.writerLocked {
		if err := storeio.UnlockWriter(c.file); err != nil {
			result = errors.Join(result, err)
		} else {
			c.writerLocked = false
		}
	}
	return result
}
