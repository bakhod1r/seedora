// Package export writes generated rows to files instead of into a database.
//
// It is the same run: the same plan, the same insertion order, the same foreign
// keys and uniqueness. Only the destination differs. That is deliberate and it
// is why this is a db.Driver and a db.Tx rather than a second code path — a
// separate writer would drift from the seeder, and the value of a fixture is
// that it is the data the seeder would have written.
//
// The schema still comes from a real database, because the schema is what
// Seedora infers everything from. Export answers "give me this data as files",
// not "seed without a database".
package export

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/bakhod1r/seedora/internal/db"
	"github.com/bakhod1r/seedora/internal/ddl"
	"github.com/bakhod1r/seedora/internal/model"
	"github.com/bakhod1r/seedora/internal/plan"
)

// Format is a file format to write.
type Format string

const (
	// CSV is one file per table with a header row, which is what a database's
	// own bulk loader and every spreadsheet will read.
	CSV Format = "csv"
	// JSON is one file per table holding an array of objects, for a fixture
	// loaded by application code rather than by the database.
	JSON Format = "json"
	// SQL is one file per table of INSERT statements, for a fixture replayed
	// with psql or mysql and no other tooling.
	SQL Format = "sql"
)

// Formats are the formats Export accepts, for a flag's error message.
var Formats = []Format{CSV, JSON, SQL}

// ParseFormat reads a format name.
func ParseFormat(s string) (Format, error) {
	for _, f := range Formats {
		if strings.EqualFold(s, string(f)) {
			return f, nil
		}
	}
	names := make([]string, len(Formats))
	for i, f := range Formats {
		names[i] = string(f)
	}
	return "", fmt.Errorf("unknown format %q — use %s", s, strings.Join(names, ", "))
}

// Driver wraps a connected database so a run writes files. Introspection, the
// dialect, and history come from the real engine; only Begin is replaced.
type Driver struct {
	db.Driver

	dir     string
	format  Format
	dialect ddl.Dialect

	// written is every file the last transaction produced, in the order the
	// tables were seeded, which is the order they have to be loaded back in.
	written []string
}

// New wraps a driver. The directory is created if it does not exist.
func New(d db.Driver, dir string, format Format) (*Driver, error) {
	if d == nil {
		return nil, errors.New("export needs a database to read the schema from")
	}
	if dir == "" {
		return nil, errors.New("export needs a directory to write to")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create %s: %w", dir, err)
	}
	return &Driver{Driver: d, dir: dir, format: format, dialect: d.Dialect()}, nil
}

// Files are the paths the last run wrote, in load order.
func (d *Driver) Files() []string { return d.written }

// Begin starts a writing transaction. Nothing reaches the disk until Commit,
// for the same reason a seeding run is one database transaction: a failure
// halfway through should leave nothing behind to clean up or mistake for a
// complete fixture.
func (d *Driver) Begin(ctx context.Context) (db.Tx, error) {
	// The real transaction is still opened. Foreign keys fall back to the keys
	// already in the database when a parent is not part of this run, and a
	// fixture that references rows the database has is a valid fixture.
	inner, err := d.Driver.Begin(ctx)
	if err != nil {
		return nil, err
	}
	d.written = nil
	return &Tx{owner: d, inner: inner, keys: map[string][]any{}}, nil
}

// Tx collects a run's rows and writes them out on Commit.
type Tx struct {
	owner *Driver
	inner db.Tx

	tables []*table
	// keys records what each table's columns were given, so a child's foreign
	// keys point at rows in the fixture rather than at rows in a database the
	// fixture will never be loaded into.
	keys map[string][]any
}

type table struct {
	table *model.Table
	cols  []string
	rows  []map[string]any
}

// Truncate is not a file operation. A run that asks for it is asking for the
// output to be only what this run produced, and starting from an empty buffer
// is exactly that.
func (t *Tx) Truncate(context.Context, *model.Table) error { return nil }

// Exec refuses. Schema changes belong to a database; a directory of CSV files
// has no schema to change, and silently ignoring the statement would produce
// a fixture missing the column it was about to add.
func (t *Tx) Exec(context.Context, string) error {
	return errors.New("export cannot run schema changes: point Seedora at the database " +
		"for those, then export the result")
}

// Insert buffers a table's rows.
//
// A column the database fills itself — an autoincrement primary key — is not in
// cols, because a seeding run lets the engine assign it and reads it back. A
// file has no engine to do either, so the key is assigned here instead. Without
// it the fixture has no ids, and a child exported alongside its parent has
// nothing to point at.
func (t *Tx) Insert(ctx context.Context, tb *model.Table, cols []string, rows db.Source) (int64, error) {
	surrogate := t.surrogateKey(tb, cols)
	if surrogate != "" {
		cols = append([]string{surrogate}, cols...)
	}

	buffered := &table{table: tb, cols: cols}
	for row := range rows.Rows() {
		// Copied: the generator reuses its maps between batches.
		kept := make(map[string]any, len(cols))
		for _, c := range cols {
			if v, ok := row[c]; ok {
				kept[c] = v
			}
		}
		if surrogate != "" {
			// Counting from one, the way every engine's sequence does, so a
			// fixture replayed into a database with a sequence on the column
			// does not leave a hole at zero.
			kept[surrogate] = int64(len(buffered.rows) + 1)
		}
		buffered.rows = append(buffered.rows, kept)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	for _, c := range cols {
		values := make([]any, 0, len(buffered.rows))
		for _, r := range buffered.rows {
			if v, ok := r[c]; ok && v != nil {
				values = append(values, v)
			}
		}
		if len(values) > 0 {
			t.keys[tb.Name+"."+c] = values
		}
	}

	t.tables = append(t.tables, buffered)
	return int64(len(buffered.rows)), nil
}

// surrogateKey names the single-column integer primary key this table has and
// the run is not writing, or "" when there is none to assign.
//
// Deliberately narrow. A composite key, a non-integer key, or a key the plan is
// already generating is left alone: those either have values or are not
// something a counter can invent. Only the autoincrement id — the one case
// where the database was going to supply the value and now nothing will — is
// filled in here.
func (t *Tx) surrogateKey(tb *model.Table, cols []string) string {
	if len(tb.PrimaryKey) != 1 {
		return ""
	}
	key := tb.PrimaryKey[0]
	for _, c := range cols {
		if c == key {
			return ""
		}
	}
	col := tb.Column(key)
	if col == nil || col.Generated {
		return ""
	}
	if plan.Classify(col.Type) != model.ClassInt {
		return ""
	}
	return key
}

// ReadKeys answers from this run first. A child exported alongside its parent
// must point at the parent rows in the fixture; falling through to the database
// would write keys that mean nothing wherever the fixture is loaded.
func (t *Tx) ReadKeys(ctx context.Context, tb *model.Table, col string, limit int) ([]any, error) {
	if k, ok := t.keys[tb.Name+"."+col]; ok {
		if limit > 0 && len(k) > limit {
			return k[:limit], nil
		}
		return k, nil
	}
	// Not part of this run: the parent already exists and is not being
	// exported, so its real keys are the right answer.
	return t.inner.ReadKeys(ctx, tb, col, limit)
}

// Commit writes every buffered table to disk.
func (t *Tx) Commit(ctx context.Context) error {
	// The database was only ever read from, and rolling it back is what makes
	// that true regardless of what the drivers did to get there.
	defer func() { _ = t.inner.Rollback(ctx) }()

	for _, buffered := range t.tables {
		path := filepath.Join(t.owner.dir, buffered.table.Name+"."+string(t.owner.format))
		if err := t.write(path, buffered); err != nil {
			return err
		}
		t.owner.written = append(t.owner.written, path)
	}
	return nil
}

func (t *Tx) write(path string, buffered *table) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	defer f.Close()

	switch t.owner.format {
	case CSV:
		err = writeCSV(f, buffered.table, buffered.cols, buffered.rows)
	case JSON:
		err = writeJSON(f, buffered.table, buffered.cols, buffered.rows)
	case SQL:
		err = writeSQL(f, buffered.table, buffered.cols, buffered.rows, t.owner.dialect)
	default:
		err = fmt.Errorf("unknown format %q", t.owner.format)
	}
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return f.Sync()
}

// Rollback discards everything. Nothing has been written, so there is nothing
// to undo — which is the reason writing is deferred to Commit.
func (t *Tx) Rollback(ctx context.Context) error {
	t.tables = nil
	return t.inner.Rollback(ctx)
}
