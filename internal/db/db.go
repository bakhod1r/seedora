// Package db is the contract every database engine implements: introspect,
// bulk-insert, truncate. A driver's whole job is to make its engine look like
// this interface, so the planner, the UI, and the seeder never branch on which
// database they are pointed at.
package db

import (
	"context"
	"fmt"
	"iter"
	"net/url"
	"sort"
	"strings"

	"github.com/bakhod1r/seedora/internal/ddl"
	"github.com/bakhod1r/seedora/internal/model"
)

// Driver is a connected database.
type Driver interface {
	// Name is the engine's display name ("PostgreSQL").
	Name() string
	// Dialect is the SQL the engine accepts, so the schema editor knows which
	// types to offer and which statements it can render.
	Dialect() ddl.Dialect
	// Introspect reads the live catalog.
	Introspect(ctx context.Context) (*model.Schema, error)
	// History reads whatever record of past schema changes the database
	// carries. That is never the engine's own — no catalog stores DDL history —
	// so it is whatever a migration tool wrote into a table of its own. A
	// database with no such table returns nothing, which is not an error.
	History(ctx context.Context) ([]model.Migration, error)
	// Begin opens the transaction the whole seeding run lives inside. A failure
	// halfway through must leave the database exactly as it was, which is why
	// this is on the driver rather than per table.
	Begin(ctx context.Context) (Tx, error)
	// Close releases the connection.
	Close(ctx context.Context) error
}

// Tx is a seeding transaction.
type Tx interface {
	// Truncate empties a table. Whether that cascades to children is the
	// engine's business; the seeder only asks for the table it was told to.
	Truncate(ctx context.Context, t *model.Table) error
	// Insert writes rows into the named columns. Rows are field→value maps, and
	// a column absent from the map takes the database default.
	//
	// Rows arrive as a sequence rather than a slice so a driver can stream them
	// straight onto the wire. On Postgres that means the whole table is one
	// COPY: the server sees a single statement no matter how many rows, and the
	// generator never blocks it. A slice would force one statement per batch
	// and put generation on the database's critical path.
	//
	// The source is consumed exactly once, and its Err must be checked after
	// the sequence ends: a generator that fails halfway simply stops yielding,
	// and without that check a short write would look like a successful one.
	Insert(ctx context.Context, t *model.Table, cols []string, rows Source) (int64, error)
	// ReadKeys returns the values of a column, used to give child tables real
	// parent keys to point at.
	ReadKeys(ctx context.Context, t *model.Table, col string, limit int) ([]any, error)
	// Exec runs one statement that returns no rows. It exists for the schema
	// editor, which renders its own SQL and needs it applied inside the same
	// transaction it can still roll back.
	Exec(ctx context.Context, sql string) error
	// Commit makes the run permanent.
	Commit(ctx context.Context) error
	// Rollback undoes it. Calling it after Commit must be a no-op, so the
	// seeder can defer it unconditionally.
	Rollback(ctx context.Context) error
}

// Source is a stream of rows to write, plus whatever went wrong producing them.
//
// Splitting the error out of the sequence is what lets generation run on its own
// goroutines while the driver holds an open COPY: the driver pulls rows as fast
// as the wire takes them, the generator fills ahead, and neither waits on the
// other. A failure on the generating side ends the sequence, and Err says why.
type Source interface {
	// Rows yields each row exactly once. A row is a field→value map; a key the
	// column list does not name is ignored, and a named column missing from the
	// map is written as NULL.
	Rows() iter.Seq[map[string]any]
	// Err reports why the sequence ended early, or nil if it ran to completion.
	// It is only meaningful after Rows has been fully consumed.
	Err() error
}

// SliceSource adapts a materialised batch to Source, for callers that already
// hold every row — tests, mostly.
type SliceSource []map[string]any

// Rows implements Source.
func (s SliceSource) Rows() iter.Seq[map[string]any] {
	return func(yield func(map[string]any) bool) {
		for _, r := range s {
			if !yield(r) {
				return
			}
		}
	}
}

// Err implements Source.
func (SliceSource) Err() error { return nil }

// Opener builds a driver from a DSN.
type Opener func(ctx context.Context, dsn string) (Driver, error)

var openers = map[string]Opener{}

// Register makes a driver available under one or more DSN schemes. Drivers call
// it from init, so linking a driver in is what enables it.
func Register(o Opener, schemes ...string) {
	for _, s := range schemes {
		openers[s] = o
	}
}

// Schemes lists every registered DSN scheme, for error messages.
func Schemes() []string {
	out := make([]string, 0, len(openers))
	for s := range openers {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// Open connects to the database named by the DSN.
func Open(ctx context.Context, dsn string) (Driver, error) {
	scheme, err := Scheme(dsn)
	if err != nil {
		return nil, err
	}
	o, ok := openers[scheme]
	if !ok {
		return nil, fmt.Errorf("no driver for %q (supported: %s)",
			scheme, strings.Join(Schemes(), ", "))
	}
	return o(ctx, dsn)
}

// Scheme extracts the DSN's scheme. A bare path with no scheme is read as
// SQLite, since that is the only engine whose connection string is a filename.
func Scheme(dsn string) (string, error) {
	dsn = strings.TrimSpace(dsn)
	if dsn == "" {
		return "", fmt.Errorf("empty DSN")
	}
	i := strings.Index(dsn, "://")
	if i < 0 {
		if strings.HasPrefix(dsn, "file:") {
			return "sqlite", nil
		}
		// MySQL's own client library spells a DSN `user:pass@tcp(host)/db`,
		// with no scheme at all, and people paste that in. The `@tcp(` is
		// unambiguous: nothing else uses it and no filename contains it.
		if strings.Contains(dsn, "@tcp(") || strings.Contains(dsn, "@unix(") {
			return "mysql", nil
		}
		if strings.ContainsAny(dsn, "/\\.") {
			return "sqlite", nil
		}
		return "", fmt.Errorf("DSN has no scheme: %s", dsn)
	}
	return strings.ToLower(dsn[:i]), nil
}

// Target describes what a DSN points at, for the safety guard and the UI header.
type Target struct {
	Engine   string
	Host     string
	Database string
}

// Describe parses a DSN into its target, ignoring credentials entirely.
func Describe(dsn string) Target {
	scheme, err := Scheme(dsn)
	if err != nil {
		return Target{}
	}
	t := Target{Engine: scheme}
	// A native MySQL DSN is not a URL; the guard still needs the host and the
	// database out of it, since that is what it decides on.
	if scheme == "mysql" && !strings.Contains(dsn, "://") {
		if open, close := strings.Index(dsn, "("), strings.Index(dsn, ")"); open >= 0 && close > open {
			t.Host = dsn[open+1 : close]
			if i := strings.LastIndex(t.Host, ":"); i > 0 {
				t.Host = t.Host[:i]
			}
			rest := dsn[close+1:]
			t.Database = strings.TrimPrefix(rest, "/")
			if i := strings.IndexByte(t.Database, '?'); i >= 0 {
				t.Database = t.Database[:i]
			}
		}
		return t
	}
	u, err := url.Parse(dsn)
	if err != nil {
		return t
	}
	t.Host = u.Hostname()
	t.Database = strings.TrimPrefix(u.Path, "/")
	if t.Database == "" && u.Opaque != "" {
		t.Database = u.Opaque
	}
	return t
}
