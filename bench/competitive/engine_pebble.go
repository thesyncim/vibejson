package competitive

import (
	"github.com/cockroachdb/pebble"
)

type pebbleEngine struct {
	cfg   Config
	db    *pebble.DB
	cache *pebble.Cache
	wopts *pebble.WriteOptions
}

func newPebble(cfg Config) (Engine, error) {
	// Pebble's default block cache is 8 MiB. Every other engine here was given
	// 64 MiB, so leaving Pebble on the default would make it lose a cache
	// fight it was never entered into.
	cache := pebble.NewCache(cfg.CacheBytes)
	opts := &pebble.Options{
		Cache: cache,
		// Snappy by default. Turned off so bytes-on-disk is comparable with
		// the uncompressed engines and Pebble is not charged compression CPU
		// nobody else pays.
		Levels: []pebble.LevelOptions{{Compression: pebble.NoCompression}},
		// 64 MiB memtables let the whole ~25 MiB corpus land in one or two
		// flushes rather than a dozen, which is what a bulk load would be
		// configured for in practice.
		MemTableSize:                64 << 20,
		MemTableStopWritesThreshold: 4,
		L0CompactionThreshold:       4,
		MaxConcurrentCompactions:    func() int { return 2 },
	}
	opts.EnsureDefaults()
	db, err := pebble.Open(cfg.Dir, opts)
	if err != nil {
		cache.Unref()
		return nil, err
	}
	wopts := pebble.NoSync
	if cfg.Sync {
		wopts = pebble.Sync
	}
	return &pebbleEngine{cfg: cfg, db: db, cache: cache, wopts: wopts}, nil
}

func (p *pebbleEngine) Name() string { return "pebble" }

func (p *pebbleEngine) Durability() string {
	if p.cfg.Sync {
		return "pebble.Sync (WAL fsynced before the write returns)"
	}
	return "pebble.NoSync (WAL written, not fsynced; matched to vibejson durable Synchronous=false)"
}

func (p *pebbleEngine) Tuning() string {
	return "Cache=64 MiB instead of the 8 MiB default, matching every other engine's read-cache budget; " +
		"Compression=None on all levels so bytes-on-disk is comparable; " +
		"MemTableSize=64 MiB so the bulk load is not chopped into a dozen flushes; " +
		"MaxConcurrentCompactions=2. " +
		"CHECKED AND REJECTED: a bloom filter on every level (FilterPolicy=bloom.FilterPolicy(10)) measured 1441 ns " +
		"against 1284 ns without one, i.e. it is a small loss, because every key this harness probes exists — a bloom " +
		"filter can only save the read of a table that does not contain the key, and there are none. Do not re-add it " +
		"as an obvious win. " +
		"NOT tuned away, and it is a stated asymmetry against Pebble: about half of Pebble's disk figure is a live WAL " +
		"holding a second copy of records that DiskBytes' Flush has already written into SSTs, and Pebble offers no " +
		"call to retire it. Those bytes are fully allocated, not sparse — the allocated column charges every one of " +
		"them. SQLite, by contrast, is given PRAGMA wal_checkpoint(TRUNCATE) and its log leaves its figure entirely. " +
		"Run cmd/footprint -engine=pebble -files to see the split before comparing Pebble's total with anyone else's"
}

func (p *pebbleEngine) Load(docs []Doc) error {
	batch := p.db.NewBatch()
	defer batch.Close()
	for i := range docs {
		if err := batch.Set([]byte(docs[i].Key), docs[i].JSON, nil); err != nil {
			return err
		}
	}
	return batch.Commit(p.wopts)
}

func (p *pebbleEngine) Get(dst []byte, key string) ([]byte, error) {
	v, closer, err := p.db.Get([]byte(key))
	if err != nil {
		return dst, err
	}
	dst = append(dst, v...)
	return dst, closer.Close()
}

func (p *pebbleEngine) Put(key string, doc []byte) error {
	return p.db.Set([]byte(key), doc, p.wopts)
}

func (p *pebbleEngine) Scan() (int, error) {
	it, err := p.db.NewIter(nil)
	if err != nil {
		return 0, err
	}
	defer it.Close()
	n := 0
	var sink byte
	for it.First(); it.Valid(); it.Next() {
		v, err := it.ValueAndErr()
		if err != nil {
			return n, err
		}
		if len(v) > 0 {
			sink ^= v[0]
		}
		n++
	}
	scanSink ^= sink
	return n, it.Error()
}

func (p *pebbleEngine) ScanAllBytes() (int, error) {
	it, err := p.db.NewIter(nil)
	if err != nil {
		return 0, err
	}
	defer it.Close()
	n := 0
	var sink byte
	for it.First(); it.Valid(); it.Next() {
		v, err := it.ValueAndErr()
		if err != nil {
			return n, err
		}
		sink ^= touchAll(v)
		n++
	}
	scanSink ^= sink
	return n, it.Error()
}

func (p *pebbleEngine) Visit(fn func(key string, value []byte) error) error {
	it, err := p.db.NewIter(nil)
	if err != nil {
		return err
	}
	defer it.Close()
	for it.First(); it.Valid(); it.Next() {
		v, err := it.ValueAndErr()
		if err != nil {
			return err
		}
		if err := fn(string(it.Key()), v); err != nil {
			return err
		}
	}
	return it.Error()
}

func (p *pebbleEngine) FilterCount(value string) (int, error) {
	needle := jsonScalarNeedle(value)
	it, err := p.db.NewIter(nil)
	if err != nil {
		return 0, err
	}
	defer it.Close()
	n := 0
	for it.First(); it.Valid(); it.Next() {
		v, err := it.ValueAndErr()
		if err != nil {
			return n, err
		}
		ok, err := matchesCountry(v, needle)
		if err != nil {
			return n, err
		}
		if ok {
			n++
		}
	}
	return n, it.Error()
}

func (p *pebbleEngine) IndexedCount(string) (int, error) { return 0, ErrNoIndex }

func (p *pebbleEngine) DiskBytes() (int64, error) {
	if err := p.db.Flush(); err != nil {
		return 0, err
	}
	return dirBytes(p.cfg.Dir)
}

func (p *pebbleEngine) Close() error {
	err := p.db.Close()
	p.cache.Unref()
	return err
}
