package social

// dbfail_test.go provides shared DB-fault-injection helpers for
// schedule_test.go and scheduler_test.go (and any other white-box test in
// this package that needs them): three small smeldr.DB decorators that let
// the Nth call to one specific method fail with a real SQLite driver error
// — routed to a fresh, tableless :memory: database rather than fabricated —
// while every other call, and every call to the other two methods, passes
// through to the real DB unchanged.
//
// For a function that makes exactly one relevant DB call, prefer the
// simpler "drop the table the statement targets" trick used throughout
// mastodon_test.go/linkedin_test.go/oauth_test.go instead of these types.
// Reserve these for functions that make several distinct kinds of DB calls
// per invocation (e.g. processSlotQueue's listActiveSchedules /
// updateScheduleLastTick / dequeueOldestQueued) where only one of them
// needs to fail while the others succeed.

import (
	"context"
	"database/sql"

	"smeldr.dev/core"

	_ "modernc.org/sqlite"
)

// openFailingConn opens a fresh, tableless :memory: SQLite database. A
// statement run against it always fails with a real driver error ("no such
// table"), which the nth*FailDB types below return in place of the real
// DB's own result on the call they are configured to fail.
func openFailingConn() *sql.DB {
	sdb, _ := sql.Open("sqlite", ":memory:")
	return sdb
}

// nthExecFailDB wraps a real smeldr.DB and fails the Nth ExecContext call.
// QueryContext and QueryRowContext always delegate to the real DB.
type nthExecFailDB struct {
	smeldr.DB
	n    int
	fail int // 1-indexed
}

func (d *nthExecFailDB) ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error) {
	d.n++
	if d.n == d.fail {
		return openFailingConn().ExecContext(ctx, "INSERT INTO no_table_nthfail_xyz(x) VALUES (1)")
	}
	return d.DB.ExecContext(ctx, q, args...)
}

// nthQueryFailDB wraps a real smeldr.DB and fails the Nth QueryContext call.
// ExecContext and QueryRowContext always delegate to the real DB.
type nthQueryFailDB struct {
	DB   smeldr.DB
	n    int
	fail int // 1-indexed
}

func (d *nthQueryFailDB) ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error) {
	return d.DB.ExecContext(ctx, q, args...)
}

func (d *nthQueryFailDB) QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	d.n++
	if d.n == d.fail {
		return openFailingConn().QueryContext(ctx, "SELECT 1 FROM no_table_nthfail_xyz")
	}
	return d.DB.QueryContext(ctx, q, args...)
}

func (d *nthQueryFailDB) QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row {
	return d.DB.QueryRowContext(ctx, q, args...)
}

// nthQueryRowFailDB wraps a real smeldr.DB and fails the Nth QueryRowContext
// call. ExecContext and QueryContext always delegate to the real DB.
type nthQueryRowFailDB struct {
	DB   smeldr.DB
	n    int
	fail int // 1-indexed
}

func (d *nthQueryRowFailDB) ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error) {
	return d.DB.ExecContext(ctx, q, args...)
}

func (d *nthQueryRowFailDB) QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	return d.DB.QueryContext(ctx, q, args...)
}

func (d *nthQueryRowFailDB) QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row {
	d.n++
	if d.n == d.fail {
		return openFailingConn().QueryRowContext(ctx, "SELECT 1 FROM no_table_nthfail_xyz")
	}
	return d.DB.QueryRowContext(ctx, q, args...)
}
