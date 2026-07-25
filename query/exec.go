package query

import (
	"fmt"

	"github.com/thesyncim/vibejson/store"
	"github.com/thesyncim/vibejson/store/durable"
)

// A sourceKind discriminates the collection a Source names. The invalid state
// is deliberately the zero value, so a Source that was declared but never
// constructed is rejected by RunInto instead of silently executing over an
// empty Segment and returning a plausible, wrong, empty result.
type sourceKind uint8

const (
	sourceInvalid sourceKind = iota
	sourceSegment
	sourceHeapSnapshot
	sourceFileSnapshot
	sourceDatabase
)

// A Source is the collection a compiled query runs over. Construct one with
// exactly one of [FromSegment], [FromSnapshot], [FromFile], and
// [FromDatabase]; the zero Source names nothing and every execution rejects it.
//
// Source is a concrete discriminated struct rather than an interface for two
// reasons. The three backends genuinely do not share a method set:
// [store.IndexSource] already names everything the two snapshot backends have
// in common and deliberately excludes the Segment, whose []int candidate
// ordinals are a different shape in kind rather than a different way to reach
// the same rows. And a Source crosses [Query.RunInto], whose whole contract is
// that a warmed execution allocates nothing; a snapshot value carried in an
// interface is one added field away from ceasing to be pointer-shaped, at
// which point every call would pay a boxing allocation for a boundary that
// buys no polymorphism. Copying four words onto the callee's frame has neither
// problem.
type Source struct {
	kind    sourceKind
	docs    *store.Segment
	heap    store.Snapshot
	file    *durable.Snapshot
	catalog store.DatabaseSnapshot
	name    string
}

// FromSegment names an in-memory [store.Segment]. The Segment is not modified by
// execution, and projected result cells borrow its bytes.
func FromSegment(s *store.Segment) Source {
	return Source{kind: sourceSegment, docs: s}
}

// FromSnapshot names an immutable heap [store.Snapshot]. Declared exact
// indexes are bound from that snapshot's catalog at execution time, so a Query
// compiled before an online index creation can use the index as soon as it is
// Ready without being recompiled. Candidate predicates combine native
// stable-slot masks and only surviving rows are decoded, before exact
// predicate recheck and late projection, grouping, aggregation, ordering, and
// limiting.
func FromSnapshot(s store.Snapshot) Source {
	return Source{kind: sourceHeapSnapshot, heap: s}
}

// FromFile names a page-backed [durable.Snapshot]. The snapshot stays owned by
// the caller; execution reads it in bounded parallel batches under
// [Exec.Options] and copies every selected value into Result-owned storage, so
// result cells survive snapshot close and page eviction.
func FromFile(s *durable.Snapshot) Source {
	return Source{kind: sourceFileSnapshot, file: s}
}

// FromDatabase names collection as the driving side of a query executed
// against catalog, a [store.DatabaseSnapshot] capturing every collection at one
// instant. Execution over the driving collection is exactly [FromSnapshot]'s;
// what the catalog adds is the ability to resolve a join clause's inner
// collection at that same instant.
//
// This is the only Source a query with a join accepts, and it is the reason
// snapshot skew across a join is not expressible rather than merely
// discouraged. Two separately taken snapshots can disagree — one collection
// observed before a commit and another after it — and a join resolving
// references against a state its driving side never saw would return rows that
// were never simultaneously true. Taking the catalog and the driving name
// together makes the consistent cut the only thing there is to pass.
//
// A query with no join is unaffected and may use whichever Source is
// convenient.
func FromDatabase(catalog store.DatabaseSnapshot, collection string) Source {
	return Source{kind: sourceDatabase, catalog: catalog, name: collection}
}

// An Exec is the caller-owned storage one execution stream runs against: the
// destination Result, the transient Workspace, the Options only the durable
// backend reads, and the Stats only backends that measure physical work write.
// The zero Exec is ready to use.
//
// An Exec is single-consumer, exactly like the Result and Workspace it
// contains. It holds the high-water buffers that make a warmed [Query.RunInto]
// allocation-free, so two goroutines sharing one Exec would interleave writes
// into a single set of column, posting, group, and result buffers. A compiled
// Query stays safe for concurrent execution; give each goroutine its own Exec.
//
// Result cells produced from a Segment or a heap snapshot borrow that
// collection and this Exec's Workspace, and stay valid until the collection is
// modified or the next RunInto reuses this Exec. Cells produced from a durable
// snapshot are copied into Result-owned storage instead, because the page
// frames they were read from may be evicted as soon as the snapshot is closed.
type Exec struct {
	// Result is the execution destination. RunInto overwrites it while keeping
	// its column and cell capacity.
	Result Result
	// Workspace is the transient scan, predicate, grouping, and ordering
	// storage for every backend, including the durable backend's index
	// planning. Retaining it across calls is what makes the steady state
	// allocation-free.
	Workspace Workspace
	// Options tune bounded execution. Workers bounds the filter phase of every
	// backend, including the two in-memory ones, and the two join bounds apply
	// wherever a semi-join binds; the batch, memory, and spill fields are
	// durable-only, a heap collection having neither worker batches nor a spill
	// frontier to bound.
	Options ExecOptions
	// Stats reports the physical work the last execution performed. Only
	// backends that measure a field write it, and every execution resets the
	// whole struct first, so a previous durable execution's numbers can never be
	// read back as if they described a Segment scan — and the join fields a heap
	// execution does write describe that execution alone.
	Stats ExecStats

	// file is the durable backend's persistent-index planning storage. It is
	// unexported because it has no caller-serviceable parts: unlike Options it
	// is not configuration, and unlike Result it is not output. Folding it in
	// here is what lets the durable entry point drop the separate workspace
	// handle its options used to carry, and it shares the general Workspace for
	// candidate planning rather than nesting a second copy of one.
	file fileWorkspace
}

// Release drops every buffer e retains, returning it to its zero state.
// Reusing a warm Exec normally gives better throughput, since the retained
// high-water capacity is exactly what makes a steady-state execution
// allocation-free; Release is for after one unusually large execution whose
// working set should not stay pinned for the rest of e's lifetime. It also
// drops the document bytes and decoded text the last Result borrowed, letting
// the collection those views point into be collected.
func (e *Exec) Release() {
	if e == nil {
		return
	}
	e.Result.Release()
	e.Workspace.Release()
	e.file.release()
	e.Stats = ExecStats{}
}

// Run executes q over src and returns the column-oriented result, compiling q
// on first use. It owns the transient storage for the call, which makes it the
// form to reach for when a query runs once; [Query.RunInto] is the form for a
// hot loop and the only one that reports [ExecStats]. Run does not modify src
// and may be called concurrently on one compiled Query, each call using its
// own execution state.
func (q *Query) Run(src Source) (Result, error) {
	var e Exec
	err := q.RunInto(&e, src)
	return e.Result, err
}

// RunInto executes q into e's caller-owned storage. After one warm-up, an
// execution over a Segment or a heap snapshot whose row count, posting
// frontier, decoded text, and group count fit e's retained high-water marks
// allocates no heap memory at all — stable ordering, grouping, containment
// indexing, typed aggregates, and escaped-string decoding included. Use
// [Cell.AppendJSON] with a retained destination to format computed aggregates
// without allocating either.
//
// RunInto overwrites e.Result and e.Stats and leaves e.Options alone. It
// rejects the zero Source, which names no collection at all: a query that
// forgets its source must fail rather than report an empty result.
func (q *Query) RunInto(e *Exec, src Source) error {
	if e == nil {
		return fmt.Errorf("query: RunInto requires a non-nil Exec")
	}
	p, err := q.compiled()
	if err != nil {
		return err
	}
	switch src.kind {
	case sourceSegment:
		if src.docs == nil {
			return fmt.Errorf("query: FromSegment was given a nil Segment")
		}
		if err := rejectJoins(p, "FromSegment"); err != nil {
			return err
		}
		e.Stats = ExecStats{}
		return p.runInto(&e.Result, src.docs, &e.Workspace, e.Options.Workers)
	case sourceHeapSnapshot:
		if err := rejectJoins(p, "FromSnapshot"); err != nil {
			return err
		}
		e.Stats = ExecStats{}
		return p.runSnapshotInto(e, src.heap, store.DatabaseSnapshot{})
	case sourceFileSnapshot:
		if err := rejectJoins(p, "FromFile"); err != nil {
			return err
		}
		return p.runFileInto(e, src.file)
	case sourceDatabase:
		driving, ok := src.catalog.Collection(src.name)
		if !ok {
			return fmt.Errorf(
				"query: FromDatabase names collection %q, which the database snapshot does not hold",
				src.name)
		}
		e.Stats = ExecStats{}
		return p.runSnapshotInto(e, driving, src.catalog)
	default:
		return fmt.Errorf(
			"query: the zero Source names no collection; " +
				"build one with FromSegment, FromSnapshot, FromFile, or FromDatabase",
		)
	}
}

// rejectJoins refuses a join plan on a Source that names one collection. There
// is no catalog behind such a Source to resolve the inner collection from, and
// resolving it from a second, independently taken snapshot is precisely the
// skew a join must not be able to express — so this is a hard rejection rather
// than a fallback that would quietly answer a different question.
func rejectJoins(p *plan, constructor string) error {
	if len(p.joins) == 0 {
		return nil
	}
	return fmt.Errorf(
		"query: this query joins collection %q, so both sides must come from one "+
			"database snapshot; build the Source with FromDatabase instead of %s",
		p.joins[0].collection, constructor)
}
