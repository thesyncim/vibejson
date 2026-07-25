package vibesql

import (
	"context"
	"database/sql/driver"
	"errors"

	"github.com/thesyncim/vibejson/query"
)

// A stmt is one prepared statement bound to one connection.
//
// The [query.Statement] it holds owns its own compiler, which is what makes
// re-binding a placeholder free of allocation after the first execution and
// what keeps two prepared statements on one connection from invalidating each
// other's plan. A statement with no placeholder never re-lowers at all: the
// plan compiled at Prepare is handed to the same executor on every execution,
// so its steady state is byte for byte the native builder's.
type stmt struct {
	conn      *conn
	statement *query.Statement
	// adhoc marks a statement QueryContext created for one execution. Its rows
	// own it, because nothing else will ever close it.
	adhoc  bool
	closed bool
}

var (
	_ driver.Stmt             = (*stmt)(nil)
	_ driver.StmtQueryContext = (*stmt)(nil)
	_ driver.StmtExecContext  = (*stmt)(nil)
)

// NumInput reports the placeholder count, which database/sql checks the
// argument count against before it calls Query. Reporting it accurately is what
// turns a wrong argument count into an error naming both numbers instead of a
// failure inside the bind.
func (s *stmt) NumInput() int { return s.statement.NumParams() }

// Close releases the statement's plan and compiler.
func (s *stmt) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	s.statement.Release()
	return nil
}

// Query executes the statement with positional arguments.
func (s *stmt) Query(args []driver.Value) (driver.Rows, error) {
	named := make([]driver.NamedValue, len(args))
	for i, v := range args {
		named[i] = driver.NamedValue{Ordinal: i + 1, Value: v}
	}
	return s.QueryContext(context.Background(), named)
}

// QueryContext executes the statement and returns its rows.
//
// The rows read the connection's [query.Exec] directly, without copying, which
// is what makes the per-row cost the executor's rather than the driver's.
// database/sql holds the connection until the rows are closed, so nothing else
// can execute into that Exec while they are being read — the invariant that
// makes borrowing safe rather than merely fast.
func (s *stmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	c := s.conn
	if s.closed {
		return nil, errors.New("vibesql: the statement is closed")
	}
	if err := c.usable(ctx); err != nil {
		return nil, err
	}
	if c.open {
		return nil, errors.New(
			"vibesql: this connection already has open rows; close them before executing again")
	}
	values, err := c.values(args)
	if err != nil {
		return nil, err
	}
	handle, err := c.src.resolve(s.statement.Collection(), s.statement.NumJoins() != 0)
	if err != nil {
		return nil, err
	}
	cursor, err := s.statement.RunInto(&c.exec, handle.src, values)
	if err != nil {
		handle.close()
		return nil, err
	}
	c.open = true
	return &rows{stmt: s, cursor: cursor, handle: handle}, nil
}

// ExecContext refuses a non-query statement. The parser accepts only SELECT, so
// reaching here means the caller used Exec for a query; saying so is more
// useful than reporting zero rows affected.
func (s *stmt) ExecContext(context.Context, []driver.NamedValue) (driver.Result, error) {
	return nil, errRead
}

// Exec refuses a non-query statement.
func (s *stmt) Exec([]driver.Value) (driver.Result, error) { return nil, errRead }

var errRead = errors.New(
	"vibesql: this driver is read-only; the dialect parses SELECT and nothing else, " +
		"so use Query rather than Exec")
