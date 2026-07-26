package competitive

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/thesyncim/vibejson/query"
	"github.com/thesyncim/vibejson/store"
	"github.com/thesyncim/vibejson/store/durable"
)

// durableCollection is the subset of store/durable's collection type this
// harness uses. As with heapCollection, it is spelled as a local interface so
// the in-flight rename of the concrete type cannot break the benchmark.
type durableCollection interface {
	Put(key string, src []byte) (bool, error)
	Delete(key string) (bool, error)
	AppendRaw(dst []byte, key string) ([]byte, bool, error)
	Snapshot() (*durable.Snapshot, error)
	Len() uint64
	Flush() error
	Close() error
}

type vibeDurable struct {
	cfg     Config
	path    string
	file    *os.File
	coll    durableCollection
	snap    *durable.Snapshot
	exec    query.Exec
	scratch []byte
}

func newVibeDurable(cfg Config) (Engine, error) {
	if cfg.Compact && cfg.PutLoop {
		return nil, fmt.Errorf("vibejson-durable: compact format requires the bulk path")
	}
	return &vibeDurable{cfg: cfg}, nil
}

func (v *vibeDurable) Name() string { return "vibejson-durable" }

func (v *vibeDurable) Durability() string {
	if v.cfg.Sync {
		return "DurabilitySync (each generation fenced to stable storage before Put returns or becomes visible)"
	}
	return "DurabilityAsyncVisible (explicit opt-in; a background worker commits visible generations)"
}

func (v *vibeDurable) Tuning() string {
	if v.cfg.Untuned {
		return "defaults only, for comparison against the tuned row"
	}
	format := "DocumentFormatVerbatim"
	if v.cfg.Compact {
		format = "DocumentFormatCompact"
	}
	return format + "; ResidentBytes=64 MiB (the default, and the read-cache budget every other engine was matched to); " +
		"PageSize=4 KiB default; buffered read and write modes (O_DIRECT is Linux-only); " +
		"MaxBatchDocuments=1 because this engine adapter exposes only point Put; " +
		"BufferCount=1024, QueueSlots=1024, GroupLimit=64. The default BufferCount is sized for the collection's " +
		"worst-case transaction geometry; the explicit pool keeps this workload's staging capacity stable. " +
		"This tuning was originally justified by a " +
		"25-35x faster Put and that figure does not currently reproduce — BenchmarkPointWriteDurableDefaults measures " +
		"the pair and RESULTS.md reports what it is worth today, which is far less. " +
		"CommitCoalesce=0, i.e. no acknowledged-latency-for-throughput trade. " +
		"CreateFrom defaults to verbatim; compact is a separate explicit row because it materially trades read speed " +
		"for space. Put replay always emits verbatim pages. Never publish one representation as the engine's only footprint"
}

func (v *vibeDurable) options() durable.Options {
	opts := durable.Options{
		ResidentBytes: v.cfg.CacheBytes,
	}
	if !v.cfg.Sync {
		opts.Durability = durable.DurabilityAsyncVisible
	}
	if v.cfg.Compact {
		opts.DocumentFormat = durable.DocumentFormatCompact
	}
	if !v.cfg.Untuned {
		// This adapter cannot express Collection.Update, so reserving for the
		// collection default's multi-document transaction would spend staging
		// memory on an operation the competitive interface cannot issue.
		opts.MaxBatchDocuments = 1
		opts.BufferCount = 1024
		opts.QueueSlots = 1024
		opts.GroupLimit = 64
	}
	if v.cfg.Indexed {
		opts.Indexes = []store.IndexDefinition{{
			Name:  FilterField,
			Paths: []string{FilterPath},
		}}
	}
	return opts
}

func (v *vibeDurable) Load(docs []Doc) error {
	v.path = filepath.Join(v.cfg.Dir, "vibejson.db")
	f, err := os.OpenFile(v.path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	v.file = f
	if v.cfg.PutLoop {
		return v.loadByPut(f, docs)
	}
	return v.loadBulk(f, docs)
}

// loadBulk is store/durable's bulk path: build the collection in memory, then
// write it as one durable generation. It is the fair counterpart of bbolt's
// single write transaction, Badger's WriteBatch, Pebble's batch, and SQLite's
// single transaction — none of those replay individual mutations either. The
// in-memory build is included in the measurement, because a caller who only
// wants the file still has to pay it.
func (v *vibeDurable) loadBulk(f *os.File, docs []Doc) error {
	opts := v.options()
	// The heap options are left at their zero value, matching the durable
	// Options' own embedded collection defaults, so the intermediate
	// collection is shaped exactly like the one durable would have built.
	builder, err := store.NewBuilder(store.Options{})
	if err != nil {
		return err
	}
	for i := range docs {
		if err := builder.Append(docs[i].Key, docs[i].JSON); err != nil {
			return err
		}
	}
	built, err := builder.Build()
	if err != nil {
		return err
	}
	if _, err := durable.CreateFrom(built, f, opts); err != nil {
		return err
	}
	db, err := durable.Open(f, opts)
	if err != nil {
		return err
	}
	v.coll = db
	return nil
}

// loadByPut is the mutation-replay path, measured separately because the gap
// between it and loadBulk is one of the more useful numbers in the report.
func (v *vibeDurable) loadByPut(f *os.File, docs []Doc) error {
	db, err := durable.Create(f, v.options())
	if err != nil {
		return err
	}
	v.coll = db
	for i := range docs {
		if _, err := db.Put(docs[i].Key, docs[i].JSON); err != nil {
			return err
		}
	}
	return db.Flush()
}

// snapshot lazily opens, and then caches, the read snapshot the scan and
// filter workloads run against.
//
// It is lazy, and Put drops it, for a reason worth recording: an open durable
// snapshot holds a lease that pins retired extents, and a store written to
// while a snapshot stays open exhausts Options.MaxRetiredExtents and fails
// with "retired extent capacity exhausted". Holding one open across a
// long-running write loop — which an earlier version of this harness did —
// takes roughly thirteen thousand replacements to hit at the default bound.
// That is a genuine operational hazard for a reader that keeps a snapshot for
// the lifetime of a request handler, but it is not what the point-write
// benchmark is supposed to be measuring, so the write path holds no snapshot.
func (v *vibeDurable) snapshot() (*durable.Snapshot, error) {
	if v.snap != nil {
		return v.snap, nil
	}
	snap, err := v.coll.Snapshot()
	if err != nil {
		return nil, err
	}
	v.snap = snap
	return snap, nil
}

func (v *vibeDurable) releaseSnapshot() {
	if v.snap != nil {
		_ = v.snap.Close()
		v.snap = nil
	}
}

func (v *vibeDurable) Get(dst []byte, key string) ([]byte, error) {
	out, ok, err := v.coll.AppendRaw(dst, key)
	if err != nil {
		return dst, err
	}
	if !ok {
		return dst, fmt.Errorf("missing key %q", key)
	}
	return out, nil
}

func (v *vibeDurable) Put(key string, doc []byte) error {
	// See snapshot: a write path must not hold a snapshot lease open.
	v.releaseSnapshot()
	_, err := v.coll.Put(key, doc)
	return err
}

func (v *vibeDurable) Upsert(key string, doc []byte) error { return v.Put(key, doc) }

func (v *vibeDurable) Delete(key string) error {
	v.releaseSnapshot()
	deleted, err := v.coll.Delete(key)
	if err == nil && !deleted {
		return fmt.Errorf("missing key %q", key)
	}
	return err
}

func (v *vibeDurable) Scan() (int, error) {
	snap, err := v.snapshot()
	if err != nil {
		return 0, err
	}
	n := 0
	var sink byte
	scratch, err := snap.RangeRawBuffer(v.scratch, func(key, value []byte) error {
		if len(value) > 0 {
			sink ^= value[0]
		}
		n++
		return nil
	})
	v.scratch = scratch
	scanSink ^= sink
	return n, err
}

func (v *vibeDurable) ScanAllBytes() (int, error) {
	snap, err := v.snapshot()
	if err != nil {
		return 0, err
	}
	n := 0
	var sink byte
	scratch, err := snap.RangeRawBuffer(v.scratch, func(key, value []byte) error {
		sink ^= touchAll(value)
		n++
		return nil
	})
	v.scratch = scratch
	scanSink ^= sink
	return n, err
}

func (v *vibeDurable) Visit(fn func(key string, value []byte) error) error {
	snap, err := v.snapshot()
	if err != nil {
		return err
	}
	scratch, err := snap.RangeRawBuffer(v.scratch, func(key, value []byte) error {
		return fn(string(key), value)
	})
	v.scratch = scratch
	return err
}

func (v *vibeDurable) FilterCount(value string) (int, error) {
	if v.cfg.Indexed {
		return 0, fmt.Errorf("FilterCount must run against an unindexed instance")
	}
	return v.runFilter(value)
}

func (v *vibeDurable) IndexedCount(value string) (int, error) {
	if !v.cfg.Indexed {
		return 0, ErrNoIndex
	}
	return v.runFilter(value)
}

func (v *vibeDurable) runFilter(value string) (int, error) {
	snap, err := v.snapshot()
	if err != nil {
		return 0, err
	}
	q := query.Select(query.Count()).Where(query.Cmp(FilterField, query.Eq, value))
	if err := q.RunInto(&v.exec, query.FromFile(snap)); err != nil {
		return 0, err
	}
	col, ok := v.exec.Result.Column("count(*)")
	if !ok || len(col.Cells) == 0 {
		return 0, fmt.Errorf("no count column in result")
	}
	n, ok := col.Cells[0].Int64()
	if !ok {
		return 0, fmt.Errorf("count cell is not an integer")
	}
	return int(n), nil
}

func (v *vibeDurable) DiskBytes() (int64, error) {
	if v.coll != nil {
		if err := v.coll.Flush(); err != nil {
			return 0, err
		}
	}
	return dirBytes(v.cfg.Dir)
}

func (v *vibeDurable) Close() error {
	v.exec.Release()
	v.releaseSnapshot()
	if v.coll != nil {
		if err := v.coll.Close(); err != nil {
			return err
		}
		v.coll = nil
	}
	if v.file != nil {
		_ = v.file.Close()
		v.file = nil
	}
	return nil
}
