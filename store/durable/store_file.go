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
	// ErrClosed reports use after Store.Close has started.
	ErrClosed = errors.New("vibejson: Store is closed")
	// ErrNotEmpty requires Create to receive an empty file.
	ErrNotEmpty = errors.New("vibejson: Store create requires an empty file")
	// ErrKeyTooLarge reports a key beyond the configured durable page
	// bound.
	ErrKeyTooLarge = errors.New("vibejson: Store key exceeds configured bound")
	// ErrDocumentTooLarge reports a JSON value beyond the configured
	// transaction bound.
	ErrDocumentTooLarge = errors.New("vibejson: Store document exceeds configured bound")
	// ErrDeadlineRange reports a deadline outside the durable signed
	// Unix-nanosecond representation.
	ErrDeadlineRange = errors.New("vibejson: Store deadline is outside Unix-nanosecond range")
	// ErrWriterLocked reports that another mutable Store owns the
	// page file. A durable file has exactly one generation publisher.
	ErrWriterLocked = storeio.ErrWriterLocked
	// ErrWriterLockUnsupported rejects mutable durable Stores on a
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

// Options fixes every Store-owned resident and in-flight memory
// bound. The zero value selects 4 KiB metadata pages, 64 KiB
// document/overflow extents, a 64 MiB read cache, and 4 MiB maximum documents.
type Options struct {
	Store store.Options
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
	BufferCount      int
	QueueSlots       int
	GroupLimit       int
	// CommitCoalesce bounds the background durability worker's group-commit
	// window. Async Put/Delete publication remains immediate. Synchronous
	// operations also wait through this window, so latency-sensitive durable
	// callers should leave it zero.
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
	MaxRetiredExtents int
}

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
	storeOptions, err := o.Store.Normalized()
	if err != nil {
		return normalizedFileStoreOptions{}, err
	}
	o.Store = storeOptions
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
		return normalizedFileStoreOptions{}, fmt.Errorf("%w: Store supports at most 64 indexes", store.ErrIndexDefinition)
	}
	if len(o.Float64Columns) > fileStoreMaxFloat64Columns {
		return normalizedFileStoreOptions{}, fmt.Errorf(
			"vibejson: Store supports at most %d float64 columns", fileStoreMaxFloat64Columns,
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
	if o.Store.Schema != nil {
		catalogHash = fileIndexHashBytes(
			catalogHash, []byte{0x53, 0x43, 0x48},
		)
		var identity [8]byte
		binary.LittleEndian.PutUint64(
			identity[:], o.Store.Schema.Hash,
		)
		catalogHash = fileIndexHashBytes(
			catalogHash, identity[:],
		)
	}
	if len(compiled) == 0 && len(columns) == 0 &&
		o.Store.Schema == nil {
		catalogHash = 0
	}
	maxRowBytes := o.MaxKeyBytes + max(o.InlineValueBytes, storeio.DocumentOverflowDescriptorSize)
	worstDocumentPage := storeio.PageHeaderSize + storeio.PageTrailerSize + storeio.DocumentPagePayloadHeaderSize +
		o.Store.ChunkDocuments*storeio.DocumentPageRecordSize + o.Store.ChunkDocuments*maxRowBytes +
		len(columns)*(8+o.Store.ChunkDocuments*8)
	if worstDocumentPage > o.MaxPageSize {
		return normalizedFileStoreOptions{}, fmt.Errorf("vibejson: Store MaxPageSize cannot hold configured chunk/key/inline bounds")
	}
	overflowPayload := o.MaxPageSize - storeio.PageHeaderSize - storeio.PageTrailerSize - storeio.OverflowPagePayloadHeaderSize
	if overflowPayload <= 0 {
		return normalizedFileStoreOptions{}, fmt.Errorf("vibejson: Store overflow page has no payload")
	}
	overflowPages := 1 + (o.MaxDocumentBytes-1)/overflowPayload
	metadataPageLimit := 48 + len(compiled)*24
	// Buffer indexes are uint16 today and the configured device ceiling is
	// 32,768. Reject the transaction geometry before int addition or byte
	// multiplication can wrap on adversarial maximum-document options.
	if overflowPages >= 32768-metadataPageLimit {
		return normalizedFileStoreOptions{}, fmt.Errorf("vibejson: Store maximum document requires too many transaction pages")
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
		return normalizedFileStoreOptions{}, fmt.Errorf("vibejson: Store MaxRetiredExtents must retain one worst-case transaction")
	}
	if o.BufferCount == 0 {
		o.BufferCount = 1
		for o.BufferCount <= maxTransactionPages {
			o.BufferCount <<= 1
		}
	}
	if o.BufferCount <= maxTransactionPages || o.BufferCount > 32768 {
		return normalizedFileStoreOptions{}, fmt.Errorf("vibejson: Store BufferCount must exceed worst-case %d-page transaction", maxTransactionPages)
	}
	if o.ResidentBytes < 0 || uint64(o.ResidentBytes) < maxTransactionBytes {
		return normalizedFileStoreOptions{}, fmt.Errorf("vibejson: Store ResidentBytes cannot retain one worst-case dirty transaction")
	}
	return normalizedFileStoreOptions{
		Options: o, maxTransactionPages: maxTransactionPages, maxTransactionBytes: maxTransactionBytes,
		indexes: compiled, float64Columns: columns, indexCatalogHash: catalogHash,
	}, nil
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

// Store is a bounded-residency, page-oriented JSON document store. It owns
// no caller file lifetime: file must remain open through Close. Mutations are
// copy-on-write and automatically persisted through a checksummed double root.
// Reads use explicit Snapshot leases and caller-owned copy-out buffers.
type Store struct {
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
	retireScratch           []storeio.FreeExtent
	reusable                []storeio.FreeExtent
	reuseJournal            []storeio.ReuseEdit
	reusableBlock           *storemem.Block
	float64Masks            []uint64
	float64Values           []float64
	float64StripeBytes      []byte
	float64StripeColumns    []storeio.Float64StripeColumn

	// Durable free-set bookkeeping. freeImagePages and freeDeltaPages are the
	// pages the published chain occupies, kept so a fold can retire exactly what
	// it supersedes. freePending holds free-set changes made outside a
	// transaction — reclamation, which is not rolled back by Abort — and so must
	// survive an aborted commit or those extents would never be written down.
	freeImagePages   []storeio.PageRef
	freeDeltaPages   []storeio.PageRef
	freeNewImage     []storeio.PageRef
	freeNewDelta     []storeio.PageRef
	freePending      []storeio.FreeDelta
	freeDeltas       []storeio.FreeDelta
	freeReclaimed    []storeio.FreeExtent
	freeImageScratch []storeio.FreeExtent
	freeAllocMark    []uint32
	freeAllocStamp   uint32
	freeSetLimit     int
	freeDeltaPerPage int
	freeImagePerPage int
	freeFoldRequired bool
	freeLoaded       bool

	appendChunk uint32
	appendLive  uint64
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
	ReusableExtents       uint64
	ReusableBytes         uint64
	DocumentCount         uint64
	LiveChunks            uint32
	FileEnd               uint64
}

// Create initializes an empty durable Store in file and fences its
// first root before returning.
func Create(file *os.File, options Options) (*Store, error) {
	if file == nil {
		return nil, fmt.Errorf("vibejson: nil Store file")
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
		return nil, fmt.Errorf("vibejson: create Store identity: %w", err)
	}
	store, err := newFileStoreResources(file, normalized, storeID)
	if err != nil {
		return nil, err
	}
	store.writerLocked = true
	locked = false
	if err := store.createInitialState(); err != nil {
		_ = store.closeResources()
		return nil, err
	}
	return store, nil
}

// Open performs bounded recovery: it reads the two superblocks, the
// selected state root, and its top-level directory pages, then starts with an
// empty read cache. It does not scan keys, documents, postings, or TTL leaves.
func Open(file *os.File, options Options) (*Store, error) {
	if file == nil {
		return nil, fmt.Errorf("vibejson: nil Store file")
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
	if root.ChunkDocuments != uint32(normalized.Store.ChunkDocuments) ||
		root.IndexCount != uint32(len(normalized.indexes)) ||
		root.IndexCatalogHash != normalized.indexCatalogHash ||
		rootHasSchema != (normalized.Store.Schema != nil) {
		return nil, fmt.Errorf("vibejson: Store options or unsupported durable catalog mismatch")
	}
	store, err := newFileStoreResources(file, normalized, root.StoreID)
	if err != nil {
		return nil, err
	}
	store.writerLocked = true
	locked = false
	if err := store.committer.InitializeGeneration(root.Generation); err != nil {
		_ = store.closeResources()
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
			_ = store.closeResources()
			if readErr != nil {
				return nil, readErr
			}
			return nil, storeio.ErrFreeLogCorrupt
		}
		view, openErr := storeio.OpenFreeDeltaPage(page, super.FileEnd, root.NextLogicalID)
		if openErr != nil {
			_ = store.closeResources()
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
	store.pageValidator.update(state)
	store.state.Store(state)
	store.appendChunk = root.ChunkHighWater
	if err := store.restoreAppendChunk(state); err != nil {
		_ = store.closeResources()
		return nil, err
	}
	return store, nil
}

func newFileStoreResources(file *os.File, options normalizedFileStoreOptions, storeID [16]byte) (*Store, error) {
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
		uint32(options.Store.ChunkDocuments),
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
	if options.MaxRetiredExtents > store.MaxInt()/extentSize {
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
	// The free set is capped at what one base image can hold, not at what the
	// arena can hold, so a fold always fits inside one ordinary transaction: a
	// fold that could not be written would strand the chain at its length bound
	// with nowhere to go.
	//
	// The cap lowers the fragmentation a store tolerates before reclamation
	// stalls and ExtentReclaimer fills, which fails writes with
	// ErrRetiredExtentCapacity. That cliff is not new — MaxRetiredExtents has
	// always bounded the same thing — but at the 4 KiB page size this ceiling is
	// roughly 2,600 extents rather than the 65,536 the default arena allows, so
	// it arrives sooner. Raising it is a matter of letting an image page use a
	// multiple of PageSize, which the encoding already permits, once the
	// transaction's dirty-byte reservation is widened to match.
	freeSetLimit := min(options.MaxRetiredExtents, storeio.FreeLogMaxImagePages*imagePerPage)
	return &Store{
		file: file, options: options, storeID: storeID, committer: committer, cache: cache,
		readFile: ownedRead, writeFile: ownedWrite,
		directRead: directRead, directWrite: directWrite,
		leases: leases, reclaimer: reclaimer,
		// A fold retires the whole superseded chain on top of the commit's own
		// retirements, so the scratch reserves both.
		retireScratch: make([]storeio.FreeExtent, 0, options.maxTransactionPages+32+
			storeio.FreeLogMaxChainPages+storeio.FreeLogMaxImagePages),
		reusable:      reusableArena[:0],
		reuseJournal:  make([]storeio.ReuseEdit, 0, options.maxTransactionPages),
		reusableBlock: reusableBlock,
		float64Masks:  make([]uint64, len(options.float64Columns)),
		float64Values: make([]float64, len(options.float64Columns)*64),
		pageValidator: pageValidator,

		freeImagePages: make([]storeio.PageRef, 0, storeio.FreeLogMaxImagePages),
		freeDeltaPages: make([]storeio.PageRef, 0, storeio.FreeLogMaxChainPages),
		freeNewImage:   make([]storeio.PageRef, 0, storeio.FreeLogMaxImagePages),
		freeNewDelta:   make([]storeio.PageRef, 0, storeio.FreeLogMaxDeltaPages),
		// Half the diff capacity belongs to changes made outside a transaction;
		// the rest is left for what the commit itself consumes. Overflowing the
		// half is not a failure, it schedules a fold.
		freePending: make([]storeio.FreeDelta, 0,
			storeio.FreeLogMaxDeltaPages*deltaPerPage/2),
		freeDeltas: make([]storeio.FreeDelta, 0,
			storeio.FreeLogMaxDeltaPages*deltaPerPage+options.maxTransactionPages),
		freeReclaimed:    make([]storeio.FreeExtent, 0, freeReclaimBatch),
		freeImageScratch: make([]storeio.FreeExtent, 0, freeSetLimit),
		freeAllocMark:    make([]uint32, freeSetLimit),
		freeSetLimit:     freeSetLimit,
		freeDeltaPerPage: deltaPerPage,
		freeImagePerPage: imagePerPage,
	}, nil
}

func (s *Store) createInitialState() error {
	tx, err := storeio.BeginWriteTransaction(s.committer, s.cache, 1, storeio.WriteTransactionOptions{
		StoreID: s.cacheStoreID(), Generation: 1, PageSize: uint32(s.options.PageSize),
		FileEnd: 2 * uint64(s.options.PageSize), NextLogicalID: 2,
	})
	if err != nil {
		return err
	}
	statePage, err := tx.Allocate(storeio.PageStateRoot, uint32(s.options.PageSize), storeio.StateRootLogicalID)
	if err != nil {
		_ = tx.Abort()
		return err
	}
	root := storeio.StateRoot{
		StoreID: s.cacheStoreID(), Generation: 1, PageSize: uint32(s.options.PageSize),
		NextLogicalID: tx.NextLogicalID(), ChunkDocuments: uint32(s.options.Store.ChunkDocuments),
		IndexCount: uint32(len(s.options.indexes)), IndexCatalogHash: s.options.indexCatalogHash,
	}
	if len(s.options.float64Columns) != 0 {
		root.Options |= storeio.StateOptionFloat64Columns
	}
	if s.options.Store.Schema != nil {
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
	if err := s.committer.Wait(1); err != nil {
		return err
	}
	s.cache.MarkDurable(1)
	super := storeio.Superblock{
		StoreID: root.StoreID, Generation: 1, StateOffset: statePage.Ref().Offset,
		StateLength: statePage.Ref().Length, StateChecksum: storeio.PageChecksum(statePage.Bytes()),
		FileEnd: tx.FileEnd(), PageSize: uint32(s.options.PageSize),
	}
	state := &fileStoreState{root: root, super: super, stateRef: statePage.Ref()}
	s.pageValidator.update(state)
	s.state.Store(state)
	s.freeLoaded = true
	return nil
}

func (s *Store) cacheStoreID() [16]byte {
	return s.storeID
}

// Snapshot pins one immutable durable root generation. Close must be
// called; copy-out methods remain valid independently of page eviction.
type Snapshot struct {
	store *Store
	state *fileStoreState
	lease storeio.GenerationLease
	once  sync.Once
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
func (s *Store) Snapshot() (*Snapshot, error) {
	if s == nil {
		return nil, ErrClosed
	}
	s.snapshotGate.RLock()
	state := s.state.Load()
	if state == nil {
		s.snapshotGate.RUnlock()
		return nil, ErrClosed
	}
	lease, err := s.leases.Acquire(state.root.Generation)
	s.snapshotGate.RUnlock()
	if err != nil {
		return nil, err
	}
	return &Snapshot{store: s, state: state, lease: lease}, nil
}

// Close releases the snapshot generation. It is idempotent.
func (s *Snapshot) Close() error {
	if s == nil {
		return nil
	}
	s.once.Do(func() {
		s.lease.Release()
		s.store = nil
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
	if s == nil || s.store == nil || s.state == nil {
		return dst, false, ErrClosed
	}
	state := s.state
	bounds := storeio.KeyTreeBounds{
		FileEnd: state.super.FileEnd, NextLogicalID: state.root.NextLogicalID,
		ChunkHighWater: state.root.ChunkHighWater,
		ChunkDocuments: uint8(state.root.ChunkDocuments),
	}
	location, ok, err := storeio.LookupKeyTree(s.store.cache, state.keyRoot, []byte(key), bounds)
	if err != nil || !ok {
		return dst, false, err
	}
	documentRef, ok, err := storeio.LookupChunkTree(s.store.cache, state.chunkRoot, location.Chunk, storeio.ChunkTreeBounds{
		FileEnd: state.super.FileEnd, NextLogicalID: state.root.NextLogicalID,
	})
	if err != nil || !ok {
		return dst, false, err
	}
	lease, err := s.store.cache.Acquire(documentRef)
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
	if s == nil || s.store == nil || s.state == nil {
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
		n, err := s.store.cache.Prefetch(unique)
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
		location, ok, err := storeio.LookupKeyTree(s.store.cache, state.keyRoot, []byte(key), keyBounds)
		if err != nil {
			return queued, err
		}
		if !ok {
			continue
		}
		ref, ok, err := storeio.LookupChunkTree(s.store.cache, state.chunkRoot, location.Chunk, chunkBounds)
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
		lease, err := s.store.cache.Acquire(ref)
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
			_, _ = s.store.cache.Prefetch([]storeio.PageRef{next})
		}
		ref = next
	}
	if offset != value.Length {
		return dst, storeio.ErrOverflowPageCorrupt
	}
	return dst, nil
}

// AppendRaw is the current-snapshot convenience form.
func (s *Store) AppendRaw(dst []byte, key string) ([]byte, bool, error) {
	snapshot, err := s.Snapshot()
	if err != nil {
		return dst, false, err
	}
	defer snapshot.Close()
	return snapshot.AppendRaw(dst, key)
}

// PrefetchKeys submits current-snapshot document reads to the bounded
// asynchronous prefetch queue.
func (s *Store) PrefetchKeys(keys []string) (int, error) {
	snapshot, err := s.Snapshot()
	if err != nil {
		return 0, err
	}
	defer snapshot.Close()
	return snapshot.PrefetchKeys(keys)
}

// Len returns the current durable-state key count.
func (s *Store) Len() uint64 {
	if s == nil || s.state.Load() == nil {
		return 0
	}
	return s.state.Load().root.DocumentCount
}

// Generation returns the current reader-visible generation.
func (s *Store) Generation() uint64 {
	if s == nil || s.state.Load() == nil {
		return 0
	}
	return s.state.Load().root.Generation
}

// DurableGeneration returns the newest crash-safe generation.
func (s *Store) DurableGeneration() uint64 {
	if s == nil || s.committer == nil {
		return 0
	}
	return s.committer.DurableGeneration()
}

// Stats reports configured residency, page I/O, prefetch, durability queue,
// snapshot, and reclamation pressure without performing file I/O.
func (s *Store) Stats() Stats {
	if s == nil || s.cache == nil || s.committer == nil {
		return Stats{}
	}
	s.writer.Lock()
	defer s.writer.Unlock()
	cache := s.cache.Stats()
	commit := s.committer.Stats()
	state := s.state.Load()
	current := uint64(0)
	if state != nil {
		current = state.root.Generation
	}
	leases := s.leases.Stats(current)
	retired := s.reclaimer.Stats()
	stats := Stats{
		CapacityBytes: cache.CapacityBytes, ResidentBytes: cache.ResidentBytes,
		CommitCapacityBytes: uint64(s.options.BufferCount) * uint64(s.options.MaxPageSize),
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
		Backend:              Backend(commit.Backend),
		ReadBackend:          Backend(cache.ReadBackend),
		DirectReads:          s.directRead,
		DirectWrites:         s.directWrite,
		SnapshotCapacity:     leases.Capacity, ActiveSnapshots: leases.Active,
		OldestSnapshotGeneration: leases.MinimumGeneration,
		RetiredExtentCapacity:    retired.Capacity, PendingRetiredExtents: retired.Pending,
		PendingRetiredBytes: retired.PendingBytes, ReusableExtents: uint64(len(s.reusable)),
		Float64ScratchBytes: uint64(len(s.float64Masks))*8 + uint64(len(s.float64Values))*8,
	}
	if s.reusableBlock != nil {
		stats.ReusableCapacityBytes = uint64(s.reusableBlock.Len())
		if s.reusableBlock.OutsideHeap() {
			stats.ReusableExternalBytes = stats.ReusableCapacityBytes
		}
	}
	for _, extent := range s.reusable {
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
func (s *Store) Put(key string, src []byte) (created bool, err error) {
	if s == nil {
		return false, ErrClosed
	}
	s.writer.Lock()
	var generation uint64
	defer func() {
		wait := generation != 0 && s.options.Synchronous
		if wait {
			s.durabilityWait.Add(1)
		}
		s.writer.Unlock()
		if wait {
			err = errors.Join(err, s.waitPublished(generation))
			s.durabilityWait.Done()
		}
	}()
	if s.closed {
		return false, ErrClosed
	}
	if len(key) > s.options.MaxKeyBytes {
		return false, ErrKeyTooLarge
	}
	if len(src) > s.options.MaxDocumentBytes {
		return false, ErrDocumentTooLarge
	}
	index, err := s.validateDocument(src)
	if err != nil {
		return false, err
	}
	state := s.state.Load()
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
		location, found, err = storeio.LookupKeyTree(s.cache, state.keyRoot, keyBytes, keyBounds)
		if err != nil {
			return false, err
		}
	}
	created = !found
	prospectiveHighWater := state.root.ChunkHighWater
	if !found {
		limit := fileStoreLiveMask(state.root.ChunkDocuments)
		if s.appendChunk < state.root.ChunkHighWater && s.appendLive != limit {
			location.Chunk = s.appendChunk
			location.Slot = uint8(bits.TrailingZeros64(^s.appendLive & limit))
		} else {
			if state.root.ChunkHighWater == ^uint32(0) {
				return false, store.ErrTooLarge
			}
			location = storeio.KeyLocation{Chunk: state.root.ChunkHighWater}
			prospectiveHighWater++
		}
	}
	if err := s.ensureDirtyCapacity(); err != nil {
		return false, err
	}
	created, err = s.putLocked(state, keyBytes, src, index, location, created, prospectiveHighWater)
	if err == nil {
		generation = state.root.Generation + 1
	}
	return created, err
}

func (s *Store) putLocked(state *fileStoreState, key, src []byte, newIndex vibejson.Index, location storeio.KeyLocation, created bool, prospectiveHighWater uint32) (bool, error) {
	generation := state.root.Generation + 1
	if generation == 0 {
		return false, storeio.ErrGenerationOrder
	}
	if err := s.refreshReusable(state); err != nil {
		return false, err
	}
	tx, err := storeio.BeginWriteTransaction(s.committer, s.cache, s.options.maxTransactionPages, storeio.WriteTransactionOptions{
		StoreID: s.storeID, Generation: generation, PageSize: uint32(s.options.PageSize),
		FileEnd: state.super.FileEnd, NextLogicalID: state.root.NextLogicalID,
		Reusable: s.reusable, ReuseJournal: s.reuseJournal,
	})
	if err != nil {
		return false, err
	}
	abort := true
	retirementReserved := false
	defer func() {
		if abort {
			if retirementReserved {
				_ = s.reclaimer.CancelRetiredGeneration(state.root.Generation)
			}
			_ = tx.Abort()
		}
	}()
	s.retireScratch = s.retireScratch[:0]

	oldRef, oldView, oldLease, err := s.loadFileChunk(state, location.Chunk)
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
			if err := s.appendOverflowRetirements(state, oldValue.value, location); err != nil {
				return false, err
			}
		}
		if len(s.options.indexes) != 0 ||
			len(s.options.float64Columns) != 0 {
			raw, valueErr := s.appendFileDocumentValue(
				s.indexValueScratch[:0], state, *oldView, oldValue, location,
			)
			if valueErr != nil {
				return false, valueErr
			}
			s.indexValueScratch = raw
			oldIndex, err = s.buildOldFileIndex(raw)
			if err != nil {
				return false, err
			}
			hasOldIndex = true
		}
	}
	newRecord, err := s.stageFileValue(tx, location, key, src)
	if err != nil {
		return false, err
	}
	rows, live, err := s.buildFileRows(state, oldView, location.Slot, newRecord, true)
	if err != nil {
		return false, err
	}
	columns, err := s.buildFileFloat64Columns(state, oldView, location.Slot, &newIndex, true)
	if err != nil {
		return false, err
	}
	documentSize, err := s.fileDocumentPageSize(rows, columns)
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
		StoreID: s.storeID, Generation: generation, LogicalID: documentPage.Ref().LogicalID,
		PageSize: documentPage.Ref().Length, ChunkID: location.Chunk, Live: live,
	}, rows, columns, tx.NextLogicalID(), tx.FileEnd(), uint32(s.options.PageSize)); err != nil {
		return false, err
	}
	if err := documentPage.Stage(); err != nil {
		return false, err
	}
	chunkMutation, err := storeio.UpsertChunkTree(s.cache, tx, state.chunkRoot, location.Chunk, documentPage.Ref(), storeio.ChunkTreeBounds{
		FileEnd: state.super.FileEnd, NextLogicalID: state.root.NextLogicalID,
	})
	if err != nil {
		return false, err
	}
	keyRoot := state.keyRoot
	var keyMutation storeio.KeyTreeMutation
	if created {
		keyMutation, err = storeio.UpsertKeyTree(s.cache, tx, state.keyRoot, key, location, storeio.KeyTreeBounds{
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
	indexRoot, err := s.updateFileIndexes(tx, state, location, oldIndexPointer, &newIndex)
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
	indexGroupHead, retireIndexGroup, err := s.maintainFileIndexGroups(
		tx, state, location, oldIndexPointer, &newIndex,
		documentCount, prospectiveHighWater,
	)
	if err != nil {
		return false, err
	}
	float64ScanHead, retireFloat64Scan, err :=
		s.maintainFileFloat64Scan(
			tx, state, chunkMutation.Root, location,
			oldIndexPointer, &newIndex, created,
		)
	if err != nil {
		return false, err
	}
	statePage, err := s.reserveFileStatePage(tx)
	if err != nil {
		return false, err
	}
	freeLog, err := s.syncFreeLog(tx, state)
	if err != nil {
		return false, err
	}
	nextState, statePage, err := s.stageFileState(
		tx, statePage, state, generation, prospectiveHighWater, documentCount, state.root.TTLCount,
		liveChunks, chunkMutation.Root, keyRoot, indexRoot, state.ttlRoot,
		float64ScanHead, indexGroupHead, freeLog.head, freeLog.checksum,
	)
	if err != nil {
		return false, err
	}
	if err := s.reserveFileRetirements(
		state, oldRef, oldView, keyMutation, chunkMutation,
		retireFloat64Scan, retireIndexGroup,
	); err != nil {
		return false, err
	}
	retirementReserved = true
	if err := tx.Publish(statePage.Ref(), storeio.PageChecksum(statePage.Bytes()), nextState.super.FreeOffset, nextState.super.FreeLength, nextState.super.FreeChecksum); err != nil {
		return false, err
	}
	abort = false
	s.finalizeReusable()
	s.commitFreeLog(freeLog)
	s.snapshotGate.Lock()
	s.pageValidator.update(nextState)
	s.state.Store(nextState)
	s.snapshotGate.Unlock()
	if location.Chunk >= state.root.ChunkHighWater || location.Chunk == s.appendChunk {
		s.appendChunk = location.Chunk
		s.appendLive = live
	}
	if live == fileStoreLiveMask(state.root.ChunkDocuments) {
		s.appendChunk = prospectiveHighWater
		s.appendLive = 0
	}
	return created, nil
}

// Delete removes key through the same failure-atomic page publication.
func (s *Store) Delete(key string) (deleted bool, err error) {
	if s == nil {
		return false, ErrClosed
	}
	s.writer.Lock()
	var generation uint64
	defer func() {
		wait := generation != 0 && s.options.Synchronous
		if wait {
			s.durabilityWait.Add(1)
		}
		s.writer.Unlock()
		if wait {
			err = errors.Join(err, s.waitPublished(generation))
			s.durabilityWait.Done()
		}
	}()
	if s.closed {
		return false, ErrClosed
	}
	state := s.state.Load()
	if state == nil || state.keyRoot == (storeio.PageRef{}) {
		return false, nil
	}
	location, found, err := storeio.LookupKeyTree(s.cache, state.keyRoot, []byte(key), storeio.KeyTreeBounds{
		FileEnd: state.super.FileEnd, NextLogicalID: state.root.NextLogicalID,
		ChunkHighWater: state.root.ChunkHighWater, ChunkDocuments: uint8(state.root.ChunkDocuments),
	})
	if err != nil || !found {
		return false, err
	}
	if err := s.ensureDirtyCapacity(); err != nil {
		return false, err
	}
	deleted, err = s.deleteLocked(state, []byte(key), location)
	if err == nil && deleted {
		generation = state.root.Generation + 1
	}
	return deleted, err
}

// SetTTL assigns a deadline relative to the current clock. A non-positive TTL
// publishes an ordinary delete.
func (s *Store) SetTTL(key string, ttl time.Duration) (bool, error) {
	if ttl <= 0 {
		return s.Delete(key)
	}
	return s.SetDeadline(key, time.Now().Add(ttl))
}

// SetDeadline durably assigns an absolute expiration. Ordinary reads never
// consult the clock; ExpireDue makes a due key invisible through a normal
// copy-on-write delete.
func (s *Store) SetDeadline(key string, deadline time.Time) (updated bool, err error) {
	if !deadline.After(time.Now()) {
		return s.Delete(key)
	}
	nanos := deadline.UnixNano()
	if !time.Unix(0, nanos).Equal(deadline) || nanos == 0 {
		return false, ErrDeadlineRange
	}
	if s == nil {
		return false, ErrClosed
	}
	s.writer.Lock()
	var generation uint64
	defer func() {
		wait := generation != 0 && s.options.Synchronous
		if wait {
			s.durabilityWait.Add(1)
		}
		s.writer.Unlock()
		if wait {
			err = errors.Join(err, s.waitPublished(generation))
			s.durabilityWait.Done()
		}
	}()
	if s.closed {
		return false, ErrClosed
	}
	state := s.state.Load()
	if state == nil || state.keyRoot == (storeio.PageRef{}) {
		return false, nil
	}
	location, found, err := storeio.LookupKeyTree(s.cache, state.keyRoot, []byte(key), storeio.KeyTreeBounds{
		FileEnd: state.super.FileEnd, NextLogicalID: state.root.NextLogicalID,
		ChunkHighWater: state.root.ChunkHighWater, ChunkDocuments: uint8(state.root.ChunkDocuments),
	})
	if err != nil || !found {
		return false, err
	}
	if location.Deadline == nanos {
		return true, nil
	}
	if err := s.ensureDirtyCapacity(); err != nil {
		return false, err
	}
	updated, err = s.setDeadlineLocked(state, []byte(key), location, nanos)
	if err == nil && updated {
		generation = state.root.Generation + 1
	}
	return updated, err
}

// Persist removes key's expiration without changing the document.
func (s *Store) Persist(key string) (updated bool, err error) {
	if s == nil {
		return false, ErrClosed
	}
	s.writer.Lock()
	var generation uint64
	defer func() {
		wait := generation != 0 && s.options.Synchronous
		if wait {
			s.durabilityWait.Add(1)
		}
		s.writer.Unlock()
		if wait {
			err = errors.Join(err, s.waitPublished(generation))
			s.durabilityWait.Done()
		}
	}()
	if s.closed {
		return false, ErrClosed
	}
	state := s.state.Load()
	if state == nil || state.keyRoot == (storeio.PageRef{}) {
		return false, nil
	}
	location, found, err := storeio.LookupKeyTree(s.cache, state.keyRoot, []byte(key), storeio.KeyTreeBounds{
		FileEnd: state.super.FileEnd, NextLogicalID: state.root.NextLogicalID,
		ChunkHighWater: state.root.ChunkHighWater, ChunkDocuments: uint8(state.root.ChunkDocuments),
	})
	if err != nil || !found || location.Deadline == 0 {
		return false, err
	}
	if err := s.ensureDirtyCapacity(); err != nil {
		return false, err
	}
	updated, err = s.setDeadlineLocked(state, []byte(key), location, 0)
	if err == nil && updated {
		generation = state.root.Generation + 1
	}
	return updated, err
}

func (s *Store) setDeadlineLocked(state *fileStoreState, key []byte, location storeio.KeyLocation, deadline int64) (bool, error) {
	generation := state.root.Generation + 1
	if err := s.refreshReusable(state); err != nil {
		return false, err
	}
	tx, err := storeio.BeginWriteTransaction(s.committer, s.cache, s.options.maxTransactionPages, storeio.WriteTransactionOptions{
		StoreID: s.storeID, Generation: generation, PageSize: uint32(s.options.PageSize),
		FileEnd: state.super.FileEnd, NextLogicalID: state.root.NextLogicalID,
		Reusable: s.reusable, ReuseJournal: s.reuseJournal,
	})
	if err != nil {
		return false, err
	}
	abort := true
	retirementReserved := false
	defer func() {
		if abort {
			if retirementReserved {
				_ = s.reclaimer.CancelRetiredGeneration(state.root.Generation)
			}
			_ = tx.Abort()
		}
	}()
	s.retireScratch = s.retireScratch[:0]
	ttlRoot := state.ttlRoot
	ttlCount := state.root.TTLCount
	bounds := storeio.TTLTreeBounds{
		FileEnd: state.super.FileEnd, NextLogicalID: state.root.NextLogicalID,
		ChunkHighWater: state.root.ChunkHighWater, ChunkDocuments: uint8(state.root.ChunkDocuments),
	}
	if location.Deadline != 0 {
		mutation, deleteErr := storeio.DeleteTTLTree(s.cache, tx, ttlRoot, storeio.TTLKey{
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
		if err := s.appendTTLRetirements(state, mutation); err != nil {
			return false, err
		}
	}
	if deadline != 0 {
		bounds.FileEnd, bounds.NextLogicalID = tx.FileEnd(), tx.NextLogicalID()
		mutation, insertErr := storeio.UpsertTTLTree(s.cache, tx, ttlRoot, storeio.TTLKey{
			Deadline: deadline, Chunk: location.Chunk, Slot: location.Slot,
		}, bounds)
		if insertErr != nil {
			return false, insertErr
		}
		ttlRoot = mutation.Root
		ttlCount++
		if err := s.appendTTLRetirements(state, mutation); err != nil {
			return false, err
		}
	}
	location.Deadline = deadline
	keyMutation, err := storeio.UpsertKeyTree(s.cache, tx, state.keyRoot, key, location, storeio.KeyTreeBounds{
		FileEnd: state.super.FileEnd, NextLogicalID: state.root.NextLogicalID,
		ChunkHighWater: state.root.ChunkHighWater, ChunkDocuments: uint8(state.root.ChunkDocuments),
	})
	if err != nil || !keyMutation.Found {
		return false, err
	}
	statePage, err := s.reserveFileStatePage(tx)
	if err != nil {
		return false, err
	}
	freeLog, err := s.syncFreeLog(tx, state)
	if err != nil {
		return false, err
	}
	nextState, statePage, err := s.stageFileState(
		tx, statePage, state, generation, state.root.ChunkHighWater, state.root.DocumentCount, ttlCount,
		state.root.LiveChunks, state.chunkRoot, keyMutation.Root, state.indexRoot, ttlRoot,
		state.root.Float64ScanHead, state.root.IndexGroupHead, freeLog.head, freeLog.checksum,
	)
	if err != nil {
		return false, err
	}
	if err := s.reserveFileRetirements(
		state, storeio.PageRef{}, nil, keyMutation, storeio.ChunkTreeMutation{},
		false, false,
	); err != nil {
		return false, err
	}
	retirementReserved = true
	if err := tx.Publish(statePage.Ref(), storeio.PageChecksum(statePage.Bytes()), nextState.super.FreeOffset, nextState.super.FreeLength, nextState.super.FreeChecksum); err != nil {
		return false, err
	}
	abort = false
	s.finalizeReusable()
	s.commitFreeLog(freeLog)
	s.snapshotGate.Lock()
	s.pageValidator.update(nextState)
	s.state.Store(nextState)
	s.snapshotGate.Unlock()
	return true, nil
}

// Deadline returns the deadline encoded beside the key in this snapshot.
func (s *Snapshot) Deadline(key string) (time.Time, bool, error) {
	if s == nil || s.store == nil || s.state == nil {
		return time.Time{}, false, ErrClosed
	}
	state := s.state
	location, found, err := storeio.LookupKeyTree(s.store.cache, state.keyRoot, []byte(key), storeio.KeyTreeBounds{
		FileEnd: state.super.FileEnd, NextLogicalID: state.root.NextLogicalID,
		ChunkHighWater: state.root.ChunkHighWater, ChunkDocuments: uint8(state.root.ChunkDocuments),
	})
	if err != nil || !found || location.Deadline == 0 {
		return time.Time{}, false, err
	}
	return time.Unix(0, location.Deadline), true, nil
}

func (s *Store) Deadline(key string) (time.Time, bool, error) {
	snapshot, err := s.Snapshot()
	if err != nil {
		return time.Time{}, false, err
	}
	defer snapshot.Close()
	return snapshot.Deadline(key)
}

func (s *Store) TTLAt(key string, now time.Time) (time.Duration, bool, error) {
	deadline, ok, err := s.Deadline(key)
	if err != nil || !ok {
		return 0, false, err
	}
	return deadline.Sub(now), true, nil
}

// ExpireDue publishes up to limit normal deletes ordered by deadline. A
// non-positive limit drains every deadline due at now with bounded memory.
func (s *Store) ExpireDue(now time.Time, limit int) (expired int, err error) {
	if s == nil {
		return 0, ErrClosed
	}
	s.writer.Lock()
	var generation uint64
	defer func() {
		wait := generation != 0 && s.options.Synchronous
		if wait {
			s.durabilityWait.Add(1)
		}
		s.writer.Unlock()
		if wait {
			err = errors.Join(err, s.waitPublished(generation))
			s.durabilityWait.Done()
		}
	}()
	if s.closed {
		return 0, ErrClosed
	}
	nowNanos := now.UnixNano()
	if !time.Unix(0, nowNanos).Equal(now) {
		return 0, ErrDeadlineRange
	}
	for limit <= 0 || expired < limit {
		state := s.state.Load()
		if state == nil || state.ttlRoot == (storeio.PageRef{}) {
			break
		}
		entry, ok, err := storeio.FirstTTLTree(s.cache, state.ttlRoot, storeio.TTLTreeBounds{
			FileEnd: state.super.FileEnd, NextLogicalID: state.root.NextLogicalID,
			ChunkHighWater: state.root.ChunkHighWater, ChunkDocuments: uint8(state.root.ChunkDocuments),
		})
		if err != nil {
			return expired, err
		}
		if !ok || entry.Deadline > nowNanos {
			break
		}
		_, view, lease, err := s.loadFileChunk(state, entry.Chunk)
		if err != nil || view == nil {
			return expired, err
		}
		record, found := view.lookup(entry.Slot)
		if !found {
			lease.Release()
			return expired, storeio.ErrTTLDirectoryCorrupt
		}
		location := storeio.KeyLocation{Chunk: entry.Chunk, Slot: entry.Slot, Deadline: entry.Deadline}
		_, err = s.deleteLocked(state, record.key, location)
		lease.Release()
		if err != nil {
			return expired, err
		}
		expired++
		generation = state.root.Generation + 1
	}
	return expired, nil
}

func (s *Store) deleteLocked(state *fileStoreState, key []byte, location storeio.KeyLocation) (bool, error) {
	generation := state.root.Generation + 1
	if err := s.refreshReusable(state); err != nil {
		return false, err
	}
	tx, err := storeio.BeginWriteTransaction(s.committer, s.cache, s.options.maxTransactionPages, storeio.WriteTransactionOptions{
		StoreID: s.storeID, Generation: generation, PageSize: uint32(s.options.PageSize),
		FileEnd: state.super.FileEnd, NextLogicalID: state.root.NextLogicalID,
		Reusable: s.reusable, ReuseJournal: s.reuseJournal,
	})
	if err != nil {
		return false, err
	}
	abort := true
	retirementReserved := false
	defer func() {
		if abort {
			if retirementReserved {
				_ = s.reclaimer.CancelRetiredGeneration(state.root.Generation)
			}
			_ = tx.Abort()
		}
	}()
	s.retireScratch = s.retireScratch[:0]
	oldRef, oldView, oldLease, err := s.loadFileChunk(state, location.Chunk)
	if err != nil || oldView == nil {
		return false, err
	}
	defer oldLease.Release()
	oldValue, ok := oldView.lookupKey(location.Slot, key)
	if !ok {
		return false, storeio.ErrDocumentPageCorrupt
	}
	if !oldValue.grouped {
		if err := s.appendOverflowRetirements(state, oldValue.value, location); err != nil {
			return false, err
		}
	}
	var oldIndex vibejson.Index
	if len(s.options.indexes) != 0 ||
		len(s.options.float64Columns) != 0 {
		raw, valueErr := s.appendFileDocumentValue(
			s.indexValueScratch[:0], state, *oldView, oldValue, location,
		)
		if valueErr != nil {
			return false, valueErr
		}
		s.indexValueScratch = raw
		oldIndex, err = s.buildOldFileIndex(raw)
		if err != nil {
			return false, err
		}
	}
	rows, live, err := s.buildFileRows(state, oldView, location.Slot, storeio.DocumentRecord{}, false)
	if err != nil {
		return false, err
	}
	var chunkMutation storeio.ChunkTreeMutation
	if live == 0 {
		chunkMutation, err = storeio.DeleteChunkTree(s.cache, tx, state.chunkRoot, location.Chunk, storeio.ChunkTreeBounds{
			FileEnd: state.super.FileEnd, NextLogicalID: state.root.NextLogicalID,
		})
	} else {
		columns, coverErr := s.buildFileFloat64Columns(state, oldView, location.Slot, nil, false)
		if coverErr != nil {
			return false, coverErr
		}
		documentSize, sizeErr := s.fileDocumentPageSize(rows, columns)
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
			StoreID: s.storeID, Generation: generation, LogicalID: documentPage.Ref().LogicalID,
			PageSize: documentPage.Ref().Length, ChunkID: location.Chunk, Live: live,
		}, rows, columns, tx.NextLogicalID(), tx.FileEnd(), uint32(s.options.PageSize)); encodeErr != nil {
			return false, encodeErr
		}
		if stageErr := documentPage.Stage(); stageErr != nil {
			return false, stageErr
		}
		chunkMutation, err = storeio.UpsertChunkTree(s.cache, tx, state.chunkRoot, location.Chunk, documentPage.Ref(), storeio.ChunkTreeBounds{
			FileEnd: state.super.FileEnd, NextLogicalID: state.root.NextLogicalID,
		})
	}
	if err != nil {
		return false, err
	}
	chunkRoot := chunkMutation.Root
	keyMutation, err := storeio.DeleteKeyTree(s.cache, tx, state.keyRoot, key, storeio.KeyTreeBounds{
		FileEnd: state.super.FileEnd, NextLogicalID: state.root.NextLogicalID,
		ChunkHighWater: state.root.ChunkHighWater, ChunkDocuments: uint8(state.root.ChunkDocuments),
	})
	if err != nil || !keyMutation.Found {
		return false, err
	}
	indexRoot, err := s.updateFileIndexes(tx, state, location, &oldIndex, nil)
	if err != nil {
		return false, err
	}
	ttlRoot := state.ttlRoot
	ttlCount := state.root.TTLCount
	if location.Deadline != 0 {
		ttlMutation, ttlErr := storeio.DeleteTTLTree(s.cache, tx, ttlRoot, storeio.TTLKey{
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
		if err := s.appendTTLRetirements(state, ttlMutation); err != nil {
			return false, err
		}
	}
	liveChunks := state.root.LiveChunks
	if live == 0 {
		liveChunks--
	}
	float64ScanHead, retireFloat64Scan, err :=
		s.maintainFileFloat64Scan(
			tx, state, chunkRoot, location, &oldIndex, nil, false,
		)
	if err != nil {
		return false, err
	}
	indexGroupHead, retireIndexGroup, err := s.maintainFileIndexGroups(
		tx, state, location, &oldIndex, nil,
		state.root.DocumentCount-1, state.root.ChunkHighWater,
	)
	if err != nil {
		return false, err
	}
	statePage, err := s.reserveFileStatePage(tx)
	if err != nil {
		return false, err
	}
	freeLog, err := s.syncFreeLog(tx, state)
	if err != nil {
		return false, err
	}
	nextState, statePage, err := s.stageFileState(
		tx, statePage, state, generation, state.root.ChunkHighWater,
		state.root.DocumentCount-1, ttlCount, liveChunks,
		chunkRoot, keyMutation.Root, indexRoot, ttlRoot,
		float64ScanHead, indexGroupHead, freeLog.head, freeLog.checksum,
	)
	if err != nil {
		return false, err
	}
	if err := s.reserveFileRetirements(
		state, oldRef, oldView, keyMutation, chunkMutation,
		retireFloat64Scan, retireIndexGroup,
	); err != nil {
		return false, err
	}
	retirementReserved = true
	if err := tx.Publish(statePage.Ref(), storeio.PageChecksum(statePage.Bytes()), nextState.super.FreeOffset, nextState.super.FreeLength, nextState.super.FreeChecksum); err != nil {
		return false, err
	}
	abort = false
	s.finalizeReusable()
	s.commitFreeLog(freeLog)
	s.snapshotGate.Lock()
	s.pageValidator.update(nextState)
	s.state.Store(nextState)
	s.snapshotGate.Unlock()
	if location.Chunk == s.appendChunk {
		s.appendLive = live
	}
	return true, nil
}

func (s *Store) validateDocument(src []byte) (vibejson.Index, error) {
	estimate := len(src)/8 + 8
	if estimate < 8 {
		estimate = 8
	}
	if cap(s.parseScratch) < estimate {
		s.parseScratch = make([]vibejson.IndexEntry, estimate)
	}
	for {
		index, err := vibejson.BuildIndexOptions(src, s.parseScratch[:cap(s.parseScratch)], s.options.Store.IndexOptions)
		if err != document.ErrIndexFull {
			if err != nil {
				return index, err
			}
			if schema := s.options.Store.Schema; schema != nil {
				if schemaErr := schema.ValidateIndex(index); schemaErr != nil {
					return vibejson.Index{}, schemaErr
				}
			}
			return index, nil
		}
		if cap(s.parseScratch) > s.options.MaxDocumentBytes {
			return vibejson.Index{}, ErrDocumentTooLarge
		}
		s.parseScratch = make([]vibejson.IndexEntry, cap(s.parseScratch)*2)
	}
}

func (s *Store) ensureDirtyCapacity() error {
	stats := s.cache.Stats()
	required := s.options.maxTransactionBytes
	if stats.CapacityBytes-stats.DirtyBytes >= required {
		return nil
	}
	if err := s.committer.Flush(); err != nil {
		return err
	}
	s.cache.MarkDurable(s.committer.DurableGeneration())
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
func (s *Store) overflowPageSize(piece int) uint32 {
	needed := storeio.PageHeaderSize + storeio.PageTrailerSize +
		storeio.OverflowPagePayloadHeaderSize + piece
	size := s.options.PageSize
	for size < needed && size < s.options.MaxPageSize {
		size <<= 1
	}
	return uint32(size)
}

func (s *Store) stageFileValue(tx *storeio.WriteTransaction, location storeio.KeyLocation, key, src []byte) (storeio.DocumentRecord, error) {
	record := storeio.DocumentRecord{Key: key, Slot: location.Slot}
	if len(src) <= s.options.InlineValueBytes {
		record.JSON = src
		return record, nil
	}
	payloadBytes := s.options.MaxPageSize - storeio.PageHeaderSize - storeio.PageTrailerSize - storeio.OverflowPagePayloadHeaderSize
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
		page, err := tx.Allocate(storeio.PageOverflow, s.overflowPageSize(piece), 0)
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
			StoreID: s.storeID, Generation: tx.Generation(), LogicalID: page.Ref().LogicalID,
			PageSize: page.Ref().Length, Chunk: location.Chunk, Slot: location.Slot,
			Total: uint64(len(src)), Offset: uint64(position), Next: next,
		}
		if _, err := storeio.EncodeOverflowPage(
			page.Bytes(), header, src[position:end], tx.FileEnd(), tx.NextLogicalID(),
			uint32(s.options.PageSize), location.Chunk+1, uint8(s.options.Store.ChunkDocuments),
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

func (s *Store) loadFileChunk(state *fileStoreState, chunkID uint32) (storeio.PageRef, *fileDocumentChunk, *fileDocumentLeases, error) {
	if chunkID >= state.root.ChunkHighWater || state.chunkRoot == (storeio.PageRef{}) {
		return storeio.PageRef{}, nil, nil, nil
	}
	ref, ok, err := storeio.LookupChunkTree(s.cache, state.chunkRoot, chunkID, storeio.ChunkTreeBounds{
		FileEnd: state.super.FileEnd, NextLogicalID: state.root.NextLogicalID,
	})
	if err != nil || !ok {
		return storeio.PageRef{}, nil, nil, err
	}
	lease, err := s.cache.Acquire(ref)
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
		ref, uint32(s.options.PageSize),
	)
	if err != nil {
		leases.Release()
		return storeio.PageRef{}, nil, nil, err
	}
	if detached {
		columns, acquireErr := s.cache.Acquire(columnsRef)
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

func (s *Store) buildFileRows(state *fileStoreState, old *fileDocumentChunk, target uint8, replacement storeio.DocumentRecord, keep bool) ([]storeio.DocumentRecord, uint64, error) {
	var storage [store.MaxChunkDocuments]storeio.DocumentRecord
	s.documentValueScratch = s.documentValueScratch[:0]
	position := 0
	var live uint64
	for slot := uint8(0); slot < uint8(s.options.Store.ChunkDocuments); slot++ {
		if slot == target {
			if keep {
				storage[position] = replacement
				position++
				live |= uint64(1) << slot
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
			start := len(s.documentValueScratch)
			s.documentValueScratch, appendErr = s.appendFileDocumentValue(
				s.documentValueScratch, state, *old, record.value,
				storeio.KeyLocation{Chunk: old.chunk, Slot: slot},
			)
			if appendErr != nil {
				return nil, 0, appendErr
			}
			json = s.documentValueScratch[start:]
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
	if old != nil {
		if _, existed := old.lookup(target); !keep && !existed {
			return nil, 0, storeio.ErrDocumentPageCorrupt
		}
	}
	return storage[:position], live, nil
}

func (s *Store) buildFileFloat64Columns(state *fileStoreState, old *fileDocumentChunk, target uint8, replacement *vibejson.Index, keep bool) (storeio.DocumentFloat64Columns, error) {
	if state == nil || state.root.Options&storeio.StateOptionFloat64Columns == 0 {
		return storeio.DocumentFloat64Columns{}, nil
	}
	if len(s.float64Masks) != len(s.options.float64Columns) ||
		len(s.float64Values) != len(s.options.float64Columns)*64 {
		return storeio.DocumentFloat64Columns{}, storeio.ErrDocumentPageCorrupt
	}
	clear(s.float64Masks)
	if old != nil {
		if old.float64ColumnCount() != len(s.options.float64Columns) {
			return storeio.DocumentFloat64Columns{}, storeio.ErrDocumentPageCorrupt
		}
		for column := range s.options.float64Columns {
			view, ok := old.float64Column(column)
			if !ok {
				return storeio.DocumentFloat64Columns{}, storeio.ErrDocumentPageCorrupt
			}
			iterator := view.Iterator()
			for {
				slot, value, present := iterator.Next()
				if !present {
					break
				}
				if slot == target {
					continue
				}
				s.float64Masks[column] |= uint64(1) << slot
				s.float64Values[column*64+int(slot)] = value
			}
		}
	}
	if keep {
		if replacement == nil {
			return storeio.DocumentFloat64Columns{}, storeio.ErrDocumentPageCorrupt
		}
		for column, definition := range s.options.float64Columns {
			node, ok, err := replacement.PointerCompiled(definition.pointer)
			if err != nil {
				return storeio.DocumentFloat64Columns{}, err
			}
			if !ok {
				continue
			}
			value, ok := node.Raw().Float64()
			if !ok || math.IsNaN(value) || math.IsInf(value, 0) {
				continue
			}
			s.float64Masks[column] |= uint64(1) << target
			s.float64Values[column*64+int(target)] = value
		}
	}
	return storeio.DocumentFloat64Columns{Masks: s.float64Masks, Values: s.float64Values}, nil
}

func (s *Store) fileDocumentPageSize(rows []storeio.DocumentRecord, columns storeio.DocumentFloat64Columns) (uint32, error) {
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
	size := s.options.PageSize
	for size < needed && size < s.options.MaxPageSize {
		size <<= 1
	}
	if size < needed || size > s.options.MaxPageSize {
		return 0, ErrDocumentTooLarge
	}
	return uint32(size), nil
}

// reserveFileStatePage claims the publication root's extent while the free log
// is still watching. Every other page a commit writes is allocated before
// syncFreeLog runs; the state root used to be allocated after it, so the extent
// it consumed was the one allocation no delta described, and the replayed free
// set would have handed that live page out again.
func (s *Store) reserveFileStatePage(tx *storeio.WriteTransaction) (storeio.TransactionPage, error) {
	return tx.Allocate(storeio.PageStateRoot, uint32(s.options.PageSize), storeio.StateRootLogicalID)
}

// stageFileState encodes the publication root into a page the caller reserved
// with reserveFileStatePage. Reserving and encoding are separate steps because
// the state page is itself allocated out of the free set, and the free log can
// only record an allocation it has already seen: reserving first puts the state
// page inside this commit's diff, while encoding last still sees the final
// FileEnd and NextLogicalID the log's own pages moved.
func (s *Store) stageFileState(
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
		StoreID: s.storeID, Generation: generation, PageSize: uint32(s.options.PageSize),
		Options:       old.root.Options,
		DocumentCount: documentCount, TTLCount: ttlCount, NextLogicalID: tx.NextLogicalID(),
		ChunkHighWater: chunkHighWater, LiveChunks: liveChunks,
		ChunkDocuments: uint32(s.options.Store.ChunkDocuments),
		IndexCount:     uint32(len(s.options.indexes)), IndexCatalogHash: s.options.indexCatalogHash,
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
		StoreID: s.storeID, Generation: generation,
		StateOffset: statePage.Ref().Offset, StateLength: statePage.Ref().Length,
		StateChecksum: storeio.PageChecksum(statePage.Bytes()), FileEnd: tx.FileEnd(),
		PageSize: uint32(s.options.PageSize),
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

func (s *Store) reserveFileRetirements(
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
		if len(s.retireScratch) == cap(s.retireScratch) {
			return storeio.ErrRetiredExtentCapacity
		}
		s.retireScratch = append(s.retireScratch, storeio.FreeExtent{
			Offset: ref.Offset, Length: uint64(ref.Length), RetiredGeneration: old.root.Generation,
		})
		return nil
	}
	if err := appendRef(old.stateRef); err != nil {
		return err
	}
	if retireFloat64Scan && old.root.Float64ScanHead != (storeio.PageRef{}) {
		if err := s.appendFloat64ScanRetirements(old); err != nil {
			return err
		}
	}
	if retireIndexGroup {
		if err := s.appendIndexGroupRetirements(old); err != nil {
			return err
		}
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
			s.cache, old.chunkRoot, header.FirstChunk, header.ChunkCount,
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
				oldDocument, uint32(s.options.PageSize),
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
					s.cache, old.chunkRoot, columnsHeader.FirstChunk, columnsHeader.ChunkCount,
					oldView.chunk, columns, uint32(s.options.PageSize), storeio.ChunkTreeBounds{
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
	return s.reclaimer.RetireBatch(s.retireScratch)
}

func (s *Store) appendIndexGroupRetirements(
	old *fileStoreState,
) error {
	var previous storeio.PageRef
	for ref := old.root.IndexGroupHead; ref != (storeio.PageRef{}); {
		lease, err := s.cache.Acquire(ref)
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
		if len(s.retireScratch) != 0 {
			last := &s.retireScratch[len(s.retireScratch)-1]
			if last.RetiredGeneration == old.root.Generation &&
				last.Offset <= ^uint64(0)-last.Length &&
				last.Offset+last.Length == ref.Offset &&
				last.Length <= ^uint64(0)-length {
				last.Length += length
			} else if err := s.appendIndexRetiredRef(old, ref); err != nil {
				return err
			}
		} else if err := s.appendIndexRetiredRef(old, ref); err != nil {
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
func (s *Store) appendFloat64ScanRetirements(old *fileStoreState) error {
	appendRef := func(ref storeio.PageRef) error {
		if ref == (storeio.PageRef{}) {
			return nil
		}
		length := uint64(ref.Length)
		if len(s.retireScratch) != 0 {
			last := &s.retireScratch[len(s.retireScratch)-1]
			if last.RetiredGeneration == old.root.Generation &&
				last.Offset <= ^uint64(0)-last.Length &&
				last.Offset+last.Length == ref.Offset &&
				last.Length <= ^uint64(0)-length {
				last.Length += length
				return nil
			}
		}
		if len(s.retireScratch) == cap(s.retireScratch) {
			return storeio.ErrRetiredExtentCapacity
		}
		s.retireScratch = append(s.retireScratch, storeio.FreeExtent{
			Offset: ref.Offset, Length: length, RetiredGeneration: old.root.Generation,
		})
		return nil
	}
	bounds := storeio.Float64DirectoryBounds{
		FileEnd:       old.super.FileEnd,
		NextLogicalID: old.root.NextLogicalID,
	}
	err := storeio.WalkFloat64DirectoryLeaves(
		s.cache, old.root.Float64ScanHead, bounds,
		uint32(s.options.PageSize),
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
		s.cache, old.root.Float64ScanHead, bounds,
		uint32(s.options.PageSize), appendRef,
	)
}

func (s *Store) appendTTLRetirements(old *fileStoreState, mutation storeio.TTLTreeMutation) error {
	for i := 0; i < int(mutation.RetiredCount); i++ {
		if len(s.retireScratch) == cap(s.retireScratch) {
			return storeio.ErrRetiredExtentCapacity
		}
		ref := mutation.Retired[i]
		s.retireScratch = append(s.retireScratch, storeio.FreeExtent{
			Offset: ref.Offset, Length: uint64(ref.Length), RetiredGeneration: old.root.Generation,
		})
	}
	return nil
}

func (s *Store) appendOverflowRetirements(state *fileStoreState, value storeio.DocumentValue, location storeio.KeyLocation) error {
	ref := value.Overflow
	if ref == (storeio.PageRef{}) {
		return nil
	}
	offset := uint64(0)
	for ref != (storeio.PageRef{}) {
		if len(s.retireScratch) == cap(s.retireScratch) {
			return storeio.ErrRetiredExtentCapacity
		}
		lease, err := s.cache.Acquire(ref)
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
		s.retireScratch = append(s.retireScratch, storeio.FreeExtent{
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

func (s *Store) appendFileValue(dst []byte, state *fileStoreState, value storeio.DocumentValue, location storeio.KeyLocation) ([]byte, error) {
	if value.Overflow == (storeio.PageRef{}) {
		return append(dst, value.Inline...), nil
	}
	ref := value.Overflow
	offset := uint64(0)
	for ref != (storeio.PageRef{}) {
		lease, err := s.cache.Acquire(ref)
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

func (s *Store) buildOldFileIndex(src []byte) (vibejson.Index, error) {
	needed, err := vibejson.RequiredIndexEntries(src)
	if err != nil {
		return vibejson.Index{}, err
	}
	if cap(s.oldParseScratch) < needed {
		s.oldParseScratch = make([]vibejson.IndexEntry, needed)
	}
	return vibejson.BuildIndexOptions(src, s.oldParseScratch[:needed], s.options.Store.IndexOptions)
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

func (s *Store) updateFileIndexes(tx *storeio.WriteTransaction, state *fileStoreState, location storeio.KeyLocation, oldIndex, newIndex *vibejson.Index) (storeio.PageRef, error) {
	root := state.indexRoot
	for indexID, exact := range s.options.indexes {
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
			newCertificate, certificateErr := s.fileIndexCertificate(
				s.indexNewCertificate[:0], exact, *newIndex,
			)
			if certificateErr != nil {
				return storeio.PageRef{}, certificateErr
			}
			s.indexNewCertificate = newCertificate
			root, err = s.mutateFilePosting(
				tx, state, root, uint32(indexID), oldHash, location, true,
				newCertificate,
			)
			if err != nil {
				return storeio.PageRef{}, err
			}
			continue
		}
		if oldOK {
			root, err = s.mutateFilePosting(
				tx, state, root, uint32(indexID), oldHash, location, false, nil,
			)
			if err != nil {
				return storeio.PageRef{}, err
			}
		}
		if newOK {
			newCertificate, certificateErr := s.fileIndexCertificate(
				s.indexNewCertificate[:0], exact, *newIndex,
			)
			if certificateErr != nil {
				return storeio.PageRef{}, certificateErr
			}
			s.indexNewCertificate = newCertificate
			root, err = s.mutateFilePosting(
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

func (s *Store) fileIndexCertificate(dst []byte, exact *store.ExactIndex, index vibejson.Index) ([]byte, error) {
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
		storeio.IndexDirectoryMaxCertificate(uint32(s.options.PageSize)),
	)
	if !ok {
		return nil, nil
	}
	return certificate, nil
}

func (s *Store) mutateFilePosting(
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
		FileEnd: tx.FileEnd(), NextLogicalID: tx.NextLogicalID(), IndexHighWater: uint32(len(s.options.indexes)),
	}
	// LookupIndexTree copies the representative out before it drops the leaf
	// lease, so the scratch below never aliases an evictable page frame.
	entry, certificate, found, err := storeio.LookupIndexTree(
		s.cache, root, key, bounds, s.indexCertificateScratch[:0],
	)
	s.indexCertificateScratch = certificate
	if err != nil {
		return storeio.PageRef{}, err
	}
	mask := uint64(0)
	collision := false
	if found {
		if len(s.indexCertificateScratch) != 0 &&
			!fileIndexCertificateValid(
				s.indexCertificateScratch, int(s.options.indexes[indexID].N),
			) {
			return storeio.PageRef{}, storeio.ErrIndexDirectoryCorrupt
		}
		collision = entry.Flags&storeio.IndexEntryCollision != 0
		mask = entry.Bits
	}
	bit := uint64(1) << location.Slot
	if present {
		if len(newCertificate) == 0 {
			s.indexCertificateScratch = s.indexCertificateScratch[:0]
			collision = false
		} else if len(s.indexCertificateScratch) == 0 {
			if found {
				// An existing entry without a representative cannot prove that
				// its bits all belong to the new value, and a live entry always
				// carries a non-empty mask, so those bits really are at stake.
				collision = true
			}
			s.indexCertificateScratch = append(
				s.indexCertificateScratch, newCertificate...,
			)
		} else if !fileIndexCertificatesEqual(
			s.indexCertificateScratch, newCertificate,
			int(s.options.indexes[indexID].N),
		) {
			collision = true
		}
		mask |= bit
	} else {
		mask &^= bit
	}
	if mask == 0 {
		mutation, deleteErr := storeio.DeleteIndexTree(s.cache, tx, root, key, bounds)
		if deleteErr != nil {
			return storeio.PageRef{}, deleteErr
		}
		if !mutation.Found {
			return storeio.PageRef{}, storeio.ErrIndexDirectoryCorrupt
		}
		if err := s.appendIndexRetirements(state, mutation); err != nil {
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
	mutation, err := storeio.UpsertIndexTree(s.cache, tx, root, storeio.IndexDirectoryEntry{
		Key: key, Bits: mask, Flags: flags, Kind: storeio.IndexEntryInlineMask,
		Cert: storeio.CertSpan{Length: uint16(len(s.indexCertificateScratch))},
	}, s.indexCertificateScratch, bounds)
	if err != nil {
		return storeio.PageRef{}, err
	}
	if err := s.appendIndexRetirements(state, mutation); err != nil {
		return storeio.PageRef{}, err
	}
	return mutation.Root, nil
}

func (s *Store) appendIndexRetiredRef(state *fileStoreState, ref storeio.PageRef) error {
	if len(s.retireScratch) == cap(s.retireScratch) {
		return storeio.ErrRetiredExtentCapacity
	}
	s.retireScratch = append(s.retireScratch, storeio.FreeExtent{
		Offset: ref.Offset, Length: uint64(ref.Length), RetiredGeneration: state.root.Generation,
	})
	return nil
}

func (s *Store) appendIndexRetirements(state *fileStoreState, mutation storeio.IndexTreeMutation) error {
	for i := 0; i < int(mutation.RetiredCount); i++ {
		if err := s.appendIndexRetiredRef(state, mutation.Retired[i]); err != nil {
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

func (s *Store) restoreAppendChunk(state *fileStoreState) error {
	if state.root.ChunkHighWater == 0 || state.chunkRoot == (storeio.PageRef{}) {
		return nil
	}
	last := state.root.ChunkHighWater - 1
	ref, ok, err := storeio.LookupChunkTree(s.cache, state.chunkRoot, last, storeio.ChunkTreeBounds{
		FileEnd: state.super.FileEnd, NextLogicalID: state.root.NextLogicalID,
	})
	if err != nil || !ok {
		return err
	}
	lease, err := s.cache.Acquire(ref)
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
		s.appendChunk = last
		s.appendLive = view.live()
	}
	return nil
}

func (s *Store) waitPublished(generation uint64) error {
	if err := s.committer.Wait(generation); err != nil {
		return err
	}
	s.cache.MarkDurable(generation)
	return nil
}

// Flush waits until the current reader-visible generation is crash-safe.
func (s *Store) Flush() error {
	if s == nil || s.committer == nil {
		return ErrClosed
	}
	generation := s.Generation()
	if err := s.committer.Wait(generation); err != nil {
		return err
	}
	s.cache.MarkDurable(generation)
	return nil
}

// Close fences every publication and releases bounded I/O resources. It does
// not close the caller-owned file. Active snapshots must be closed first.
func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	s.writer.Lock()
	if s.closeDone {
		s.writer.Unlock()
		return nil
	}
	s.closed = true
	s.writer.Unlock()
	// Synchronous publishers release the construction lock before their
	// durability wait so independent writers can share one device commit.
	// Closed prevents any new waiter from registering before this drain.
	s.durabilityWait.Wait()
	if err := s.leases.Close(); err != nil {
		return err
	}
	if err := s.closeResources(); err != nil {
		return err
	}
	s.writer.Lock()
	s.closeDone = true
	s.writer.Unlock()
	return nil
}

func (s *Store) closeResources() error {
	var result error
	if s.committer != nil {
		if err := s.committer.Close(); err != nil {
			result = errors.Join(result, err)
		}
		s.cache.MarkDurable(s.committer.DurableGeneration())
	}
	if s.cache != nil {
		if err := s.cache.Close(); err != nil {
			result = errors.Join(result, err)
		}
	}
	if s.readFile != nil {
		readFile := s.readFile
		s.readFile = nil
		if err := readFile.Close(); err != nil {
			result = errors.Join(result, err)
		}
	}
	if s.writeFile != nil {
		writeFile := s.writeFile
		s.writeFile = nil
		if err := writeFile.Close(); err != nil {
			result = errors.Join(result, err)
		}
	}
	if s.reusableBlock != nil {
		if err := s.reusableBlock.Close(); err != nil {
			result = errors.Join(result, err)
		}
		s.reusableBlock = nil
		s.reusable = nil
	}
	if s.writerLocked {
		if err := storeio.UnlockWriter(s.file); err != nil {
			result = errors.Join(result, err)
		} else {
			s.writerLocked = false
		}
	}
	return result
}
