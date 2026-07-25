package competitive

import (
	"database/sql"
	"fmt"
	"path/filepath"

	_ "modernc.org/sqlite"
)

type sqliteEngine struct {
	cfg Config
	db  *sql.DB

	getStmt     *sql.Stmt
	putStmt     *sql.Stmt
	scanStmt    *sql.Stmt
	filterStmt  *sql.Stmt
	indexedStmt *sql.Stmt
}

func newSQLite(cfg Config) (Engine, error) {
	path := filepath.Join(cfg.Dir, "sqlite.db")
	// _pragma parameters are applied to every pooled connection, which
	// matters because database/sql may open more than one.
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)" +
		"&_pragma=page_size(4096)" +
		"&_pragma=cache_size(-65536)" + // 64 MiB, in KiB, negative means bytes
		"&_pragma=temp_store(MEMORY)" +
		"&_pragma=foreign_keys(0)"
	if cfg.Sync {
		// synchronous=FULL alone is NOT durability-matched with the Go engines
		// on darwin, and quoting it as if it were would invalidate the whole
		// write comparison.
		//
		// Go's os.File.Sync issues F_FULLFSYNC on darwin, which forces the
		// drive to flush its write cache. modernc.org/sqlite is a translation
		// of SQLite's C source, so its VFS calls plain fsync(), which on macOS
		// returns once the data reaches the drive cache and not the platter.
		// SQLite's own escape hatch is PRAGMA fullfsync, and its default is 0.
		//
		// Measured on this machine, identical setup, 200 autocommit UPDATEs:
		// fullfsync=0 costs 545 us each, fullfsync=1 costs 6.92 ms — the same
		// order as bbolt (8.5 ms) and store/durable (5.2 ms). Without this
		// pragma SQLite appears to win the durable-write row by three orders
		// of magnitude while simply not making the data durable.
		dsn += "&_pragma=synchronous(FULL)&_pragma=fullfsync(1)"
	} else {
		dsn += "&_pragma=synchronous(OFF)"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// One connection: SQLite is single-writer, and a pool would make the
	// per-connection pragmas and the prepared-statement cache nondeterministic.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	// WITHOUT ROWID makes the primary key the clustered B-tree, which is the
	// same physical shape bbolt and vibejson use, rather than a rowid heap
	// plus a separate unique index over the key.
	//
	// The generated column is always declared so the table shape is identical
	// in both configurations; only the index over it is conditional. A VIRTUAL
	// generated column costs no storage — SQLite evaluates json_extract on
	// read — so the unindexed configuration is not paying for it at rest.
	schema := `CREATE TABLE docs (
		k TEXT PRIMARY KEY,
		doc TEXT NOT NULL,
		country TEXT GENERATED ALWAYS AS (json_extract(doc, '$.country')) VIRTUAL
	) WITHOUT ROWID;`
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	if cfg.Indexed {
		if _, err := db.Exec(`CREATE INDEX idx_country ON docs(country)`); err != nil {
			db.Close()
			return nil, err
		}
	}

	e := &sqliteEngine{cfg: cfg, db: db}
	if err := e.prepare(); err != nil {
		db.Close()
		return nil, err
	}
	return e, nil
}

func (s *sqliteEngine) prepare() error {
	var err error
	if s.getStmt, err = s.db.Prepare(`SELECT doc FROM docs WHERE k = ?`); err != nil {
		return err
	}
	if s.putStmt, err = s.db.Prepare(`UPDATE docs SET doc = ? WHERE k = ?`); err != nil {
		return err
	}
	if s.scanStmt, err = s.db.Prepare(`SELECT doc FROM docs`); err != nil {
		return err
	}
	// The unindexed filter deliberately spells the predicate as a JSON1
	// expression over the stored text, so SQLite cannot reach the generated
	// column's index even when one exists.
	if s.filterStmt, err = s.db.Prepare(
		`SELECT count(*) FROM docs WHERE json_extract(doc, '$.country') = ?`); err != nil {
		return err
	}
	if s.cfg.Indexed {
		if s.indexedStmt, err = s.db.Prepare(
			`SELECT count(*) FROM docs WHERE country = ?`); err != nil {
			return err
		}
	}
	return nil
}

func (s *sqliteEngine) Name() string { return "sqlite" }

func (s *sqliteEngine) Durability() string {
	if s.cfg.Sync {
		return "WAL + synchronous=FULL + fullfsync=1 (F_FULLFSYNC per commit on darwin, matching Go's os.File.Sync)"
	}
	return "WAL + synchronous=OFF (matched to vibejson durable Synchronous=false)"
}

func (s *sqliteEngine) Tuning() string {
	return "modernc.org/sqlite, the pure-Go translation (no cgo). journal_mode=WAL; page_size=4096 to match vibejson durable's default page; " +
		"cache_size=-65536 i.e. a 64 MiB page cache, matching every other engine; temp_store=MEMORY; " +
		"table is WITHOUT ROWID so the primary key is the clustered B-tree, the same physical shape as bbolt and vibejson; " +
		"bulk load is one transaction over one prepared INSERT; the index configuration adds an index over a VIRTUAL " +
		"json_extract generated column; scans read through sql.RawBytes rather than []byte, which avoids a " +
		"database/sql copy per row that no other engine here was charged, reverted by Config.Untuned and measured " +
		"by BenchmarkTuning/sqlite. " +
		"CHECKED AND REJECTED: mmap_size. PRAGMA mmap_size(268435456) measured 3949 ns against 3795 ns without it on " +
		"the unindexed filter, i.e. no help, because modernc.org/sqlite's VFS does not implement the memory-mapped " +
		"read path the pragma exists to enable. Do not re-add it as an obvious win. " +
		"CHECKED: EXPLAIN QUERY PLAN confirms SEARCH docs USING INDEX idx_country for the indexed query and " +
		"SCAN docs for the json_extract one, so neither row is accidentally measuring the wrong plan"
}

func (s *sqliteEngine) Load(docs []Doc) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`INSERT INTO docs (k, doc) VALUES (?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for i := range docs {
		if _, err := stmt.Exec(docs[i].Key, string(docs[i].JSON)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *sqliteEngine) Get(dst []byte, key string) ([]byte, error) {
	var doc []byte
	if err := s.getStmt.QueryRow(key).Scan(&doc); err != nil {
		return dst, err
	}
	return append(dst, doc...), nil
}

func (s *sqliteEngine) Put(key string, doc []byte) error {
	res, err := s.putStmt.Exec(string(doc), key)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("update affected %d rows", n)
	}
	return nil
}

// Scan uses sql.RawBytes rather than []byte. Scanning into a []byte makes
// database/sql copy every row's value into a fresh allocation before the loop
// body ever sees it; sql.RawBytes borrows the driver's buffer until the next
// Next. It is the spelling the database/sql documentation points at for exactly
// this case, and scanning into []byte charged SQLite a copy no other engine in
// this harness was charged for. Config.Untuned restores the []byte spelling and
// BenchmarkTuning/sqlite measures the pair.
func (s *sqliteEngine) Scan() (int, error) {
	rows, err := s.scanStmt.Query()
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	n := 0
	var sink byte
	if s.cfg.Untuned {
		var doc []byte
		for rows.Next() {
			if err := rows.Scan(&doc); err != nil {
				return n, err
			}
			if len(doc) > 0 {
				sink ^= doc[0]
			}
			n++
		}
		scanSink ^= sink
		return n, rows.Err()
	}
	var doc sql.RawBytes
	for rows.Next() {
		if err := rows.Scan(&doc); err != nil {
			return n, err
		}
		if len(doc) > 0 {
			sink ^= doc[0]
		}
		n++
	}
	scanSink ^= sink
	return n, rows.Err()
}

func (s *sqliteEngine) ScanAllBytes() (int, error) {
	rows, err := s.scanStmt.Query()
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	n := 0
	var sink byte
	var doc sql.RawBytes
	for rows.Next() {
		if err := rows.Scan(&doc); err != nil {
			return n, err
		}
		sink ^= touchAll(doc)
		n++
	}
	scanSink ^= sink
	return n, rows.Err()
}

func (s *sqliteEngine) Visit(fn func(key string, value []byte) error) error {
	rows, err := s.db.Query(`SELECT k, doc FROM docs`)
	if err != nil {
		return err
	}
	defer rows.Close()
	var key string
	var doc sql.RawBytes
	for rows.Next() {
		if err := rows.Scan(&key, &doc); err != nil {
			return err
		}
		if err := fn(key, doc); err != nil {
			return err
		}
	}
	return rows.Err()
}

func (s *sqliteEngine) FilterCount(value string) (int, error) {
	var n int
	err := s.filterStmt.QueryRow(value).Scan(&n)
	return n, err
}

func (s *sqliteEngine) IndexedCount(value string) (int, error) {
	if s.indexedStmt == nil {
		return 0, ErrNoIndex
	}
	var n int
	err := s.indexedStmt.QueryRow(value).Scan(&n)
	return n, err
}

func (s *sqliteEngine) DiskBytes() (int64, error) {
	// Checkpoint so the WAL's contents are counted where they will end up
	// rather than twice.
	if _, err := s.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return 0, err
	}
	return dirBytes(s.cfg.Dir)
}

func (s *sqliteEngine) Close() error {
	for _, st := range []*sql.Stmt{s.getStmt, s.putStmt, s.scanStmt, s.filterStmt, s.indexedStmt} {
		if st != nil {
			_ = st.Close()
		}
	}
	return s.db.Close()
}
