package query

import (
	"fmt"

	"github.com/thesyncim/simdjson"
)

// A Plan is the immutable, typed execution form shared by every query front
// end. Builder paths, SQL tokens, predicates, and literals are resolved once;
// execution addresses compact path, predicate, aggregate, and group slots by
// ordinal. The SQL source and builder tree are not retained.
//
// A Plan is cheap to copy and safe for concurrent execution. Each concurrent
// RunInto or RunSnapshotInto still needs an independent Result and Workspace.
// Its zero value is invalid.
type Plan struct {
	compiled *plan
}

// Prepare compiles q and returns its canonical execution plan. Repeated calls
// return lightweight handles to the same immutable plan.
func (q *Query) Prepare() (Plan, error) {
	if q == nil {
		return Plan{}, fmt.Errorf("query: cannot prepare a nil Query")
	}
	p, err := q.compiled()
	if err != nil {
		return Plan{}, err
	}
	return Plan{compiled: p}, nil
}

// PrepareSQL parses the supported SQL subset and returns the same Plan that
// the equivalent programmatic builder produces. SQL is therefore an optional
// compile-time adapter, not the executor's representation.
func PrepareSQL(sql string) (Plan, error) {
	q, err := Compile(sql)
	if err != nil {
		return Plan{}, err
	}
	return q.Prepare()
}

// A Reduction identifies the typed reduction performed by an output column.
// ReductionNone denotes a projected JSON value.
type Reduction uint8

const (
	ReductionNone Reduction = iota
	ReductionCount
	ReductionSum
	ReductionAvg
	ReductionMin
	ReductionMax
)

// OutputColumn is cold result-schema metadata. Ordinal is the stable column ID
// used by the typed result batch; Header is a compatibility/display spelling,
// not an execution key.
type OutputColumn struct {
	Header    string
	Ordinal   uint32
	Reduction Reduction
	// Type is TypeAny for a schemaless projection and TypeNumber for the
	// current aggregate family. Future aggregate or schema-aware output types
	// extend ValueType without changing column ordinals or instruction opcodes.
	Type ValueType
	// Flags control column framing. Unknown required types fail preparation;
	// an explicitly optional length-delimited column may be skipped by a
	// negotiated older reader.
	Flags OutputFlags
}

// OutputFlags describe schema-level compatibility behavior independently from
// a value type's physical properties.
type OutputFlags uint16

const (
	// OutputOptional permits a reader that does not recognize Type to skip the
	// complete length-delimited column.
	OutputOptional OutputFlags = 1 << iota
)

// AppendSchema appends p's output schema to dst without allocating when dst
// has enough capacity. Headers borrow immutable plan storage.
func (p Plan) AppendSchema(dst []OutputColumn) []OutputColumn {
	if p.compiled == nil {
		return dst
	}
	for i, col := range p.compiled.columns {
		valueType := TypeAny
		if col.agg != aggNone {
			valueType = TypeNumber
		}
		dst = append(dst, OutputColumn{
			Header:    p.compiled.headers[i],
			Ordinal:   uint32(i),
			Reduction: Reduction(col.agg),
			Type:      valueType,
		})
	}
	return dst
}

// Run executes p over s and returns a column-oriented typed result.
func (p Plan) Run(s *simdjson.DocSet) (Result, error) {
	var result Result
	var workspace Workspace
	err := p.RunInto(&result, s, &workspace)
	return result, err
}

// RunInto is the caller-owned, zero-steady-allocation form of [Plan.Run].
func (p Plan) RunInto(dst *Result, s *simdjson.DocSet, w *Workspace) error {
	if p.compiled == nil {
		return fmt.Errorf("query: cannot execute a zero Plan")
	}
	if dst == nil || s == nil || w == nil {
		return fmt.Errorf("query: Plan.RunInto requires non-nil result, DocSet, and Workspace")
	}
	return p.compiled.runInto(dst, s, w)
}

// RunSnapshot executes p over an immutable Store snapshot.
func (p Plan) RunSnapshot(s simdjson.Snapshot) (Result, error) {
	var result Result
	var workspace Workspace
	err := p.RunSnapshotInto(&result, s, &workspace)
	return result, err
}

// RunSnapshotInto is the caller-owned, zero-steady-allocation form of
// [Plan.RunSnapshot]. Index binding remains late so a prepared Plan can use an
// index published after the plan was prepared.
func (p Plan) RunSnapshotInto(dst *Result, s simdjson.Snapshot, w *Workspace) error {
	if p.compiled == nil {
		return fmt.Errorf("query: cannot execute a zero Plan")
	}
	if dst == nil || w == nil {
		return fmt.Errorf("query: Plan.RunSnapshotInto requires non-nil result and Workspace")
	}
	return p.compiled.runSnapshotInto(dst, s, w)
}
