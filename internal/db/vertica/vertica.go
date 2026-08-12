// Package vertica implements the Seedora driver for Vertica.
//
// The thing that shapes this driver is that Vertica's INSERT takes exactly one
// row. There is no multi-row VALUES to chunk on a placeholder limit the way the
// MySQL and SQLite drivers do, and a statement per row against a columnar store
// is not a seeding strategy — it is how you fill the write-optimised store with
// a million single-row containers. What Vertica has instead is COPY, and the
// driver exposes it with the input coming from a stream of our choosing, so
// Insert streams rows into a COPY the same way the Postgres driver does.
//
// DDL is transactional here, so a schema change the editor applies unwinds with
// the rest of the run. TRUNCATE is the exception and is documented where it is
// not used.
package vertica

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	vertigo "github.com/vertica/vertica-sql-go"

	"github.com/bakhod1r/seedora/internal/db"
	"github.com/bakhod1r/seedora/internal/ddl"
	"github.com/bakhod1r/seedora/internal/model"
)

func init() {
	db.Register(open, "vertica")
}

// Driver is a connected Vertica database.
type Driver struct {
	db *sql.DB
}

func open(ctx context.Context, dsn string) (db.Driver, error) {
	conn, err := sql.Open("vertica", dsn)
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	// One connection, because the whole run is a single transaction and a COPY
	// holds the session it started on until its stream ends.
	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(1)
	if err := conn.PingContext(ctx); err != nil {
		conn.Close()
		return nil, fmt.Errorf("connect: %w", err)
	}
	return &Driver{db: conn}, nil
}

// Name implements db.Driver.
func (d *Driver) Name() string { return "Vertica" }

// Dialect implements db.Driver.
func (d *Driver) Dialect() ddl.Dialect { return ddl.Vertica }

// Close implements db.Driver.
func (d *Driver) Close(context.Context) error { return d.db.Close() }

// Begin implements db.Driver.
func (d *Driver) Begin(ctx context.Context) (db.Tx, error) {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	return &Tx{tx: tx}, nil
}

// Introspect reads the catalog in three queries plus a count per table: on a
// schema with a few hundred tables the round trips dominate everything else.
func (d *Driver) Introspect(ctx context.Context) (*model.Schema, error) {
	// Vertica has no enum type: the equivalent is a check constraint, which is
	// a predicate rather than a list of labels, so the map stays empty rather
	// than being guessed at.
	s := &model.Schema{Enums: map[string]model.Values{}}

	byName, err := d.loadColumns(ctx, s)
	if err != nil {
		return nil, err
	}
	if err := d.loadConstraints(ctx, byName); err != nil {
		return nil, err
	}
	if err := d.loadCounts(ctx, s); err != nil {
		return nil, err
	}
	return s, nil
}

func (d *Driver) loadColumns(ctx context.Context, s *model.Schema) (map[string]*model.Table, error) {
	const q = `
SELECT c.table_schema, c.table_name, c.column_name, c.data_type,
       c.is_nullable,
       CASE WHEN c.column_default IS NULL THEN 0 ELSE 1 END,
       CASE WHEN c.is_identity THEN 1 ELSE 0 END,
       COALESCE(c.character_maximum_length, 0),
       COALESCE(c.numeric_precision, 0),
       COALESCE(c.numeric_scale, 0)
-- v_catalog.columns holds tables only; a view's columns live in
-- v_catalog.view_columns, so there is nothing here to filter out.
FROM v_catalog.columns c
WHERE c.table_schema = CURRENT_SCHEMA
ORDER BY c.table_schema, c.table_name, c.ordinal_position`

	rows, err := d.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("read columns: %w", err)
	}
	defer rows.Close()

	byName := map[string]*model.Table{}
	for rows.Next() {
		var (
			schema, table, name, dataType string
			nullable                      bool
			hasDefault, identity          int
			maxLen, precision, scale      int64
		)
		if err := rows.Scan(&schema, &table, &name, &dataType, &nullable,
			&hasDefault, &identity, &maxLen, &precision, &scale); err != nil {
			return nil, err
		}
		t := byName[table]
		if t == nil {
			t = &model.Table{Schema: schema, Name: table}
			byName[table] = t
			s.Tables = append(s.Tables, t)
		}
		// data_type is the fully spelled declaration here — "varchar(255)" —
		// which is exactly what Native wants and not what Type does.
		base := dataType
		if i := strings.IndexByte(base, '('); i > 0 {
			base = base[:i]
		}
		t.Columns = append(t.Columns, &model.Column{
			Name:     name,
			Type:     strings.ToLower(strings.TrimSpace(base)),
			Native:   dataType,
			Nullable: nullable,
			// An identity column fills itself, which is the same thing to the
			// planner as a column with a default.
			HasDefault: hasDefault == 1 || identity == 1,
			MaxLen:     int(maxLen),
			Precision:  int(precision),
			Scale:      int(scale),
		})
	}
	return byName, rows.Err()
}

// loadConstraints marks primary keys, single-column uniqueness, and foreign
// keys. Vertica records all three in one view, so one query answers all three.
//
// Vertica does not enforce any of them — constraints are declarations the
// optimiser trusts and the loader ignores — which makes reading them more
// important rather than less: nothing will fail at insert time to tell the user
// their seed put duplicates in a unique column.
func (d *Driver) loadConstraints(ctx context.Context, byName map[string]*model.Table) error {
	const q = `
SELECT table_name, constraint_name, constraint_type, column_name,
       COALESCE(reference_table_name, ''), COALESCE(reference_column_name, '')
FROM v_catalog.constraint_columns
WHERE table_schema = CURRENT_SCHEMA AND constraint_type IN ('p', 'u', 'f')
ORDER BY table_name, constraint_name, ordinal_position`

	rows, err := d.db.QueryContext(ctx, q)
	if err != nil {
		return fmt.Errorf("read constraints: %w", err)
	}
	defer rows.Close()

	type key struct{ table, name string }
	type part struct{ column, refTable, refColumn string }
	byKey := map[key][]part{}
	kind := map[key]string{}
	var order []key
	for rows.Next() {
		var table, name, ctype, column, refTable, refColumn string
		if err := rows.Scan(&table, &name, &ctype, &column, &refTable, &refColumn); err != nil {
			return err
		}
		k := key{table, name}
		if _, seen := byKey[k]; !seen {
			order = append(order, k)
		}
		byKey[k] = append(byKey[k], part{column, refTable, refColumn})
		kind[k] = strings.ToLower(ctype)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	for _, k := range order {
		t := byName[k.table]
		if t == nil {
			continue
		}
		parts := byKey[k]
		switch kind[k] {
		case "p":
			for _, p := range parts {
				t.PrimaryKey = append(t.PrimaryKey, p.column)
			}
			if len(parts) == 1 {
				if c := t.Column(parts[0].column); c != nil {
					c.Unique = true
				}
			}
		case "u":
			// Only a single-column constraint constrains one generator; a
			// composite one holds across columns, which the seeder cannot
			// honour by making any single value unique.
			if len(parts) == 1 {
				if c := t.Column(parts[0].column); c != nil {
					c.Unique = true
				}
			}
		case "f":
			// A composite key cannot be satisfied one column at a time, so it
			// is left alone rather than half-applied.
			if len(parts) == 1 && parts[0].refTable != "" {
				if c := t.Column(parts[0].column); c != nil {
					c.FK = &model.Ref{Table: parts[0].refTable, Column: parts[0].refColumn}
				}
			}
		}
	}
	return nil
}

// loadCounts counts each table rather than reading a stored estimate. Vertica
// keeps its row counts per projection, and a table has as many projections as
// its segmentation demands — summing them counts most rows several times, and a
// truncate confirmation built on that number would be a lie.
func (d *Driver) loadCounts(ctx context.Context, s *model.Schema) error {
	for _, t := range s.Tables {
		if err := d.db.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM "+t.Qualified()).Scan(&t.ExistingRows); err != nil {
			return fmt.Errorf("count %s: %w", t.Name, err)
		}
	}
	return nil
}

// History reads whatever a migration tool left behind. Vertica records no DDL
// history of its own.
func (d *Driver) History(ctx context.Context) ([]model.Migration, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT table_name FROM v_catalog.tables WHERE table_schema = CURRENT_SCHEMA`)
	if err != nil {
		return nil, fmt.Errorf("list tables: %w", err)
	}
	present := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return nil, err
		}
		present[n] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return db.ReadHistory(ctx, model.QuoteIdent,
		func(table string) bool { return present[table] },
		func(ctx context.Context, query string) ([]map[string]any, error) {
			rows, err := d.db.QueryContext(ctx, query)
			if err != nil {
				return nil, err
			}
			defer rows.Close()
			return scanRows(rows)
		}), nil
}

// scanRows turns a result set into field→value maps without knowing what the
// columns are, which is what reading another tool's table requires.
func scanRows(rows *sql.Rows) ([]map[string]any, error) {
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out []map[string]any
	for rows.Next() {
		cells := make([]any, len(cols))
		into := make([]any, len(cols))
		for i := range cells {
			into[i] = &cells[i]
		}
		if err := rows.Scan(into...); err != nil {
			return nil, err
		}
		row := make(map[string]any, len(cols))
		for i, name := range cols {
			if b, ok := cells[i].([]byte); ok {
				row[name] = string(b)
				continue
			}
			row[name] = cells[i]
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// Tx is a Vertica seeding transaction.
type Tx struct {
	tx   *sql.Tx
	done bool
}

// Exec implements db.Tx. Vertica's DDL is transactional, so a schema change
// applied here really does unwind with the rest of the run.
func (t *Tx) Exec(ctx context.Context, stmt string) error {
	if _, err := t.tx.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("exec: %w", err)
	}
	return nil
}

// Truncate implements db.Tx with DELETE rather than TRUNCATE TABLE, because
// TRUNCATE commits the open transaction on Vertica — the one the seeder is
// relying on to unwind a failed run. DELETE is slower, and on a columnar store
// it marks rather than removes until the next purge, but it is the only version
// that keeps the promise that a failure leaves the database as it was.
func (t *Tx) Truncate(ctx context.Context, tb *model.Table) error {
	if _, err := t.tx.ExecContext(ctx, "DELETE FROM "+tb.Qualified()); err != nil {
		return fmt.Errorf("truncate %s: %w", tb.Name, err)
	}
	// The sequence behind an identity column is deliberately left alone:
	// restarting it is an ALTER, and its values are not reproducible across
	// runs on a cluster in any case — each node hands out its own range.
	return nil
}

// The three bytes that separate the COPY stream. They are control characters
// rather than a comma and a newline because generated text contains commas and
// newlines constantly, and the escaping below only has to cover what a value
// realistically holds. Vertica's default escape character is a backslash, so a
// value containing one of these — or a backslash — is escaped with one.
const (
	fieldSep  = "\x01"
	recordSep = "\x02"
	nullMark  = "\x03"
)

// Insert implements db.Tx using COPY FROM STDIN, with the rows encoded onto a
// pipe as they are generated.
//
// This is the only bulk path Vertica has: its INSERT accepts a single row of
// VALUES, so a multi-row statement — the trick the MySQL and SQLite drivers
// chunk on — cannot be written at all, and one statement per row would fill the
// write-optimised store with a container apiece. COPY is one statement for the
// whole table, and the encoder below runs on its own goroutine so the generator
// and the server never wait on each other.
func (t *Tx) Insert(ctx context.Context, tb *model.Table, cols []string, rows db.Source) (int64, error) {
	if len(cols) == 0 {
		return 0, nil
	}
	quoted := make([]string, len(cols))
	for i, c := range cols {
		quoted[i] = model.QuoteIdent(c)
	}

	pr, pw := io.Pipe()
	written := make(chan int64, 1)
	go func() {
		var n int64
		var buf strings.Builder
		for row := range rows.Rows() {
			buf.Reset()
			for i, c := range cols {
				if i > 0 {
					buf.WriteString(fieldSep)
				}
				buf.WriteString(field(row[c]))
			}
			buf.WriteString(recordSep)
			if _, err := io.WriteString(pw, buf.String()); err != nil {
				// The reader is gone, which means the COPY already failed; its
				// error is the one worth reporting, so this side just stops.
				break
			}
			n++
		}
		written <- n
		// Closing the write end is what ends the COPY; an error here would be
		// reported by the statement rather than by the writer.
		pw.Close()
	}()

	vctx := vertigo.NewVerticaContext(ctx)
	if err := vctx.SetCopyInputStream(pr); err != nil {
		pr.CloseWithError(err)
		<-written
		return 0, fmt.Errorf("copy into %s: %w", tb.Name, err)
	}

	stmt := fmt.Sprintf(
		"COPY %s (%s) FROM STDIN DELIMITER E'\\001' RECORD TERMINATOR E'\\002' "+
			"NULL AS E'\\003' ENCLOSED BY '' ABORT ON ERROR",
		tb.Qualified(), strings.Join(quoted, ", "))

	_, err := t.tx.ExecContext(vctx, stmt)
	// The encoder is drained either way: it holds the source, and leaving it
	// blocked on a write would leak the goroutine and the generator behind it.
	pr.CloseWithError(err)
	n := <-written

	if err != nil {
		// A generator failure surfaces here as a short stream, so its own error
		// is the more useful one to report.
		if gerr := rows.Err(); gerr != nil {
			return n, fmt.Errorf("generate rows for %s: %w", tb.Name, gerr)
		}
		return n, fmt.Errorf("copy into %s: %w", tb.Name, err)
	}
	if gerr := rows.Err(); gerr != nil {
		return n, fmt.Errorf("generate rows for %s: %w", tb.Name, gerr)
	}
	return n, nil
}

// field renders one generated value as the text COPY reads.
func field(v any) string {
	switch x := v.(type) {
	case nil:
		return nullMark
	case string:
		return escape(x)
	case []byte:
		return escape(string(x))
	case bool:
		if x {
			return "true"
		}
		return "false"
	case time.Time:
		// A timestamp is written with its offset so a run is read back the same
		// way on a server in another timezone.
		return x.Format("2006-01-02 15:04:05.999999999-07:00")
	case map[string]any, []any:
		b, err := json.Marshal(x)
		if err != nil {
			// A value that cannot be encoded is written as NULL rather than
			// failing the run: the alternative is losing a hundred thousand
			// rows to one unserialisable cell.
			return nullMark
		}
		return escape(string(b))
	default:
		return escape(fmt.Sprint(x))
	}
}

// escape protects the three separators and the escape character itself. Nothing
// else needs it: the separators are control characters, so ordinary text passes
// through untouched.
func escape(s string) string {
	if !strings.ContainsAny(s, "\x01\x02\x03\\") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 8)
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\x01', '\x02', '\x03', '\\':
			b.WriteByte('\\')
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// ReadKeys implements db.Tx. It reads inside the transaction on purpose: the
// parent rows it returns were written by this same uncommitted run, and any
// other connection would not see them.
func (t *Tx) ReadKeys(ctx context.Context, tb *model.Table, col string, limit int) ([]any, error) {
	name := model.QuoteIdent(col)
	q := fmt.Sprintf("SELECT %s FROM %s WHERE %s IS NOT NULL LIMIT %s",
		name, tb.Qualified(), name, strconv.Itoa(limit))
	rows, err := t.tx.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("read keys from %s.%s: %w", tb.Name, col, err)
	}
	defer rows.Close()

	var out []any
	for rows.Next() {
		var v any
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		// A key read back as bytes would be written to the child as a byte
		// string, which compares equal to nothing.
		if b, ok := v.([]byte); ok {
			v = string(b)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// Commit implements db.Tx.
func (t *Tx) Commit(context.Context) error {
	if t.done {
		return nil
	}
	t.done = true
	return t.tx.Commit()
}

// Rollback implements db.Tx.
func (t *Tx) Rollback(context.Context) error {
	if t.done {
		return nil
	}
	t.done = true
	err := t.tx.Rollback()
	if errors.Is(err, sql.ErrTxDone) {
		return nil
	}
	return err
}
