// Package sqlite implements the Seedora driver for SQLite.
//
// SQLite has no bulk-load protocol, so the fast path is a prepared multi-row
// INSERT inside one transaction with the synchronous pragma relaxed. That is
// within a small factor of what the engine can do at all, and unlike COPY it
// costs nothing to set up.
//
// The driver is pure Go (modernc.org/sqlite), so a Seedora binary still needs
// no cgo and no toolchain on the machine that runs it.
package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	_ "modernc.org/sqlite"

	"github.com/bakhod1r/seedora/internal/db"
	"github.com/bakhod1r/seedora/internal/ddl"
	"github.com/bakhod1r/seedora/internal/model"
)

func init() {
	db.Register(open, "sqlite", "sqlite3", "file")
}

// Driver is a connected SQLite database.
type Driver struct {
	db *sql.DB
}

func open(ctx context.Context, dsn string) (db.Driver, error) {
	path := strings.TrimPrefix(dsn, "sqlite3://")
	path = strings.TrimPrefix(path, "sqlite://")
	// modernc's driver takes a filename, optionally with a file: URI; anything
	// else it treats as a path, which is what a bare ./dev.db already is.
	conn, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	// One connection, because the whole run is a single transaction and a pool
	// would hand the seeder a connection that cannot see its own uncommitted
	// parent rows.
	conn.SetMaxOpenConns(1)
	if err := conn.PingContext(ctx); err != nil {
		conn.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	for _, pragma := range pragmas {
		if _, err := conn.ExecContext(ctx, pragma); err != nil {
			conn.Close()
			return nil, fmt.Errorf("%s: %w", pragma, err)
		}
	}
	return &Driver{db: conn}, nil
}

// pragmas configure the connection for bulk writing.
//
// synchronous = OFF is the one that matters: it stops SQLite waiting for an
// fsync per commit, which is most of the wall clock on a seeding run and buys
// nothing here. Atomicity is untouched — the rollback journal still exists, so a
// failed run still unwinds — and what is given up is durability against an
// operating-system crash, on a development database that exists to be thrown
// away and regenerated in seconds.
var pragmas = []string{
	// Foreign keys are off by default in SQLite. Seeding wants them on: a plan
	// that produces a dangling key should fail loudly rather than write a
	// database that fails somewhere else later.
	"PRAGMA foreign_keys = ON",
	"PRAGMA synchronous = OFF",
	// A larger page cache keeps the b-tree pages a bulk insert keeps returning
	// to in memory. The value is negative, which SQLite reads as kibibytes
	// rather than pages, so it does not change meaning with the page size.
	"PRAGMA cache_size = -64000",
	"PRAGMA temp_store = MEMORY",
}

// Name implements db.Driver.
func (d *Driver) Name() string { return "SQLite" }

// Dialect implements db.Driver.
func (d *Driver) Dialect() ddl.Dialect { return ddl.SQLite }

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

// Introspect reads the catalog through the pragma functions, which are the only
// interface SQLite offers to its own schema.
func (d *Driver) Introspect(ctx context.Context) (*model.Schema, error) {
	s := &model.Schema{Enums: map[string]model.Values{}}

	names, err := d.tableNames(ctx)
	if err != nil {
		return nil, err
	}
	for _, name := range names {
		t := &model.Table{Name: name}
		if err := d.loadColumns(ctx, t); err != nil {
			return nil, err
		}
		if err := d.loadIndexes(ctx, t); err != nil {
			return nil, err
		}
		if err := d.loadForeignKeys(ctx, t); err != nil {
			return nil, err
		}
		if err := d.db.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM "+t.Qualified()).Scan(&t.ExistingRows); err != nil {
			return nil, fmt.Errorf("count %s: %w", name, err)
		}
		s.Tables = append(s.Tables, t)
	}
	return s, nil
}

// History reads whatever a migration tool left behind. SQLite records no DDL
// history of its own — sqlite_schema holds the current shape and nothing about
// how it got there.
func (d *Driver) History(ctx context.Context) ([]model.Migration, error) {
	names, err := d.tableNames(ctx)
	if err != nil {
		return nil, err
	}
	present := map[string]bool{}
	for _, n := range names {
		present[n] = true
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
			row[name] = cells[i]
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func (d *Driver) tableNames(ctx context.Context) ([]string, error) {
	const q = `
SELECT name FROM sqlite_schema
WHERE type = 'table'
  AND name NOT LIKE 'sqlite_%'
ORDER BY name`
	rows, err := d.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list tables: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func (d *Driver) loadColumns(ctx context.Context, t *model.Table) error {
	rows, err := d.db.QueryContext(ctx,
		"SELECT name, type, \"notnull\", dflt_value, pk, hidden FROM pragma_table_xinfo(?)", t.Name)
	if err != nil {
		return fmt.Errorf("columns of %s: %w", t.Name, err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			name, typ string
			notNull   int
			dflt      sql.NullString
			pk        int
			hidden    int
		)
		if err := rows.Scan(&name, &typ, &notNull, &dflt, &pk, &hidden); err != nil {
			return err
		}
		// hidden 2 and 3 are generated columns; 1 is a hidden virtual-table
		// column, which cannot be written either.
		c := &model.Column{
			Name:       name,
			Type:       baseType(typ),
			Native:     typ,
			Nullable:   notNull == 0 && pk == 0,
			HasDefault: dflt.Valid,
			Generated:  hidden != 0,
		}
		c.MaxLen, c.Precision, c.Scale = decorations(typ)
		if pk > 0 {
			t.PrimaryKey = append(t.PrimaryKey, name)
			// A single-column INTEGER PRIMARY KEY is an alias for rowid and
			// fills itself, which makes it a default even though the catalog
			// reports none.
			if strings.EqualFold(baseType(typ), "integer") {
				c.HasDefault = true
			}
		}
		t.Columns = append(t.Columns, c)
	}
	return rows.Err()
}

// loadIndexes marks columns backed by a single-column unique index. SQLite
// records UNIQUE constraints as indexes, so this is the only place uniqueness
// is visible.
func (d *Driver) loadIndexes(ctx context.Context, t *model.Table) error {
	rows, err := d.db.QueryContext(ctx,
		"SELECT name, \"unique\" FROM pragma_index_list(?)", t.Name)
	if err != nil {
		return fmt.Errorf("indexes of %s: %w", t.Name, err)
	}
	type idx struct{ name string }
	var uniques []idx
	for rows.Next() {
		var name string
		var uniq int
		if err := rows.Scan(&name, &uniq); err != nil {
			rows.Close()
			return err
		}
		if uniq == 1 {
			uniques = append(uniques, idx{name})
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}

	for _, u := range uniques {
		cols, err := d.indexColumns(ctx, u.name)
		if err != nil {
			return err
		}
		if len(cols) != 1 {
			continue
		}
		if c := t.Column(cols[0]); c != nil {
			c.Unique = true
		}
	}
	return nil
}

func (d *Driver) indexColumns(ctx context.Context, index string) ([]string, error) {
	rows, err := d.db.QueryContext(ctx,
		"SELECT name FROM pragma_index_info(?) ORDER BY seqno", index)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n sql.NullString
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		// An index over an expression reports a NULL column name, and there is
		// no column to mark unique.
		if n.Valid {
			out = append(out, n.String)
		}
	}
	return out, rows.Err()
}

func (d *Driver) loadForeignKeys(ctx context.Context, t *model.Table) error {
	rows, err := d.db.QueryContext(ctx,
		`SELECT id, "table", "from", "to" FROM pragma_foreign_key_list(?)`, t.Name)
	if err != nil {
		return fmt.Errorf("foreign keys of %s: %w", t.Name, err)
	}
	defer rows.Close()
	// A composite key spans several rows sharing an id. Counting first is what
	// lets composite keys be skipped rather than half-applied.
	type fk struct{ from, table, to string }
	byID := map[int][]fk{}
	var order []int
	for rows.Next() {
		var id int
		var refTable, from string
		var to sql.NullString
		if err := rows.Scan(&id, &refTable, &from, &to); err != nil {
			return err
		}
		if _, seen := byID[id]; !seen {
			order = append(order, id)
		}
		byID[id] = append(byID[id], fk{from: from, table: refTable, to: to.String})
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range order {
		parts := byID[id]
		if len(parts) != 1 {
			continue
		}
		p := parts[0]
		c := t.Column(p.from)
		if c == nil {
			continue
		}
		to := p.to
		if to == "" {
			// An omitted target column means the parent's primary key.
			to = "rowid"
		}
		c.FK = &model.Ref{Table: p.table, Column: to}
	}
	return nil
}

// baseType strips the length decoration so "VARCHAR(50)" classifies as varchar.
func baseType(t string) string {
	if i := strings.IndexByte(t, '('); i >= 0 {
		return strings.TrimSpace(t[:i])
	}
	return strings.TrimSpace(t)
}

func decorations(native string) (maxLen, precision, scale int) {
	open := strings.IndexByte(native, '(')
	if open < 0 || !strings.HasSuffix(native, ")") {
		return 0, 0, 0
	}
	parts := strings.Split(native[open+1:len(native)-1], ",")
	first := atoi(strings.TrimSpace(parts[0]))
	base := strings.ToLower(strings.TrimSpace(native[:open]))
	switch {
	case strings.Contains(base, "char"), strings.Contains(base, "text"):
		return first, 0, 0
	case strings.Contains(base, "decimal"), strings.Contains(base, "numeric"):
		if len(parts) > 1 {
			return 0, first, atoi(strings.TrimSpace(parts[1]))
		}
		return 0, first, 0
	}
	return 0, 0, 0
}

func atoi(s string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0
		}
		n = n*10 + int(s[i]-'0')
	}
	return n
}

// Tx is a SQLite seeding transaction.
type Tx struct {
	tx   *sql.Tx
	done bool
}

// Exec implements db.Tx. The schema editor renders its own SQL and applies it
// here, inside the transaction it can still roll back.
func (t *Tx) Exec(ctx context.Context, stmt string) error {
	if _, err := t.tx.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("exec: %w", err)
	}
	return nil
}

// Truncate implements db.Tx. SQLite has no TRUNCATE; a bare DELETE is the
// documented equivalent and the engine optimises it into a truncate when there
// is no WHERE clause and no trigger.
func (t *Tx) Truncate(ctx context.Context, tb *model.Table) error {
	if _, err := t.tx.ExecContext(ctx, "DELETE FROM "+tb.Qualified()); err != nil {
		return fmt.Errorf("truncate %s: %w", tb.Name, err)
	}
	// Reset the autoincrement counter if the table has one, so ids restart at 1
	// exactly as they do on Postgres with RESTART IDENTITY.
	_, _ = t.tx.ExecContext(ctx, "DELETE FROM sqlite_sequence WHERE name = ?", tb.Name)
	return nil
}

// maxParams is SQLite's default variable limit per statement. Splitting on it is
// what keeps a wide table from failing at a batch size that a narrow one
// handles, since the limit is on values, not rows.
const maxParams = 32000

// Insert implements db.Tx with multi-row INSERT statements, chunked so no
// statement exceeds the variable limit.
//
// SQLite has no bulk protocol, so unlike Postgres this cannot be one statement.
// It is still one statement per chunk rather than per row, and the statement for
// a full chunk is built once and reused, so the engine parses it once and the
// rest is bind-and-step. Rows are pulled from the source as they are needed, so
// a hundred thousand of them are never held at once.
func (t *Tx) Insert(ctx context.Context, tb *model.Table, cols []string, rows db.Source) (int64, error) {
	if len(cols) == 0 {
		return 0, nil
	}
	perRow := len(cols)
	chunk := maxParams / perRow
	if chunk < 1 {
		return 0, fmt.Errorf("insert into %s: %d columns exceeds the statement limit", tb.Name, perRow)
	}

	quoted := make([]string, len(cols))
	for i, c := range cols {
		quoted[i] = model.QuoteIdent(c)
	}
	head := "INSERT INTO " + tb.Qualified() + " (" + strings.Join(quoted, ", ") + ") VALUES "

	// The full-chunk statement is prepared once and reused for every chunk but
	// the last, which is where nearly all the rows go.
	full, err := t.tx.PrepareContext(ctx, head+placeholders(chunk, perRow))
	if err != nil {
		return 0, fmt.Errorf("prepare insert into %s: %w", tb.Name, err)
	}
	defer full.Close()

	var written int64
	args := make([]any, 0, chunk*perRow)

	flush := func() error {
		if len(args) == 0 {
			return nil
		}
		n := len(args) / perRow
		var err error
		if n == chunk {
			_, err = full.ExecContext(ctx, args...)
		} else {
			_, err = t.tx.ExecContext(ctx, head+placeholders(n, perRow), args...)
		}
		if err != nil {
			return fmt.Errorf("insert into %s: %w", tb.Name, err)
		}
		written += int64(n)
		args = args[:0]
		return nil
	}

	var loopErr error
	for row := range rows.Rows() {
		for _, c := range cols {
			args = append(args, row[c])
		}
		if len(args) == chunk*perRow {
			if err := flush(); err != nil {
				loopErr = err
				break
			}
		}
	}
	if loopErr != nil {
		return written, loopErr
	}
	if err := rows.Err(); err != nil {
		return written, fmt.Errorf("generate rows for %s: %w", tb.Name, err)
	}
	if err := flush(); err != nil {
		return written, err
	}
	return written, nil
}

// placeholders builds "(?,?),(?,?),…" for n rows of width w.
func placeholders(n, w int) string {
	var sb strings.Builder
	sb.Grow(n * (w*2 + 2))
	for i := range n {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteByte('(')
		for j := range w {
			if j > 0 {
				sb.WriteByte(',')
			}
			sb.WriteByte('?')
		}
		sb.WriteByte(')')
	}
	return sb.String()
}

// ReadKeys implements db.Tx.
func (t *Tx) ReadKeys(ctx context.Context, tb *model.Table, col string, limit int) ([]any, error) {
	// rowid is the implicit key a foreign key with no explicit target points at.
	name := model.QuoteIdent(col)
	if col == "rowid" {
		name = "rowid"
	}
	q := fmt.Sprintf("SELECT %s FROM %s WHERE %s IS NOT NULL LIMIT %d",
		name, tb.Qualified(), name, limit)
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
	if err == sql.ErrTxDone {
		return nil
	}
	return err
}
