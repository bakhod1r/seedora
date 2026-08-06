// Package postgres implements the Seedora driver for PostgreSQL and the engines
// that speak its wire protocol — CockroachDB, YugabyteDB, Aurora Postgres.
//
// Everything is read from the catalog rather than from information_schema where
// the two differ, because information_schema hides exactly the things seeding
// needs: which columns are identity, which types are enums, and what an enum's
// labels are.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/bakhod1r/seedora/internal/db"
	"github.com/bakhod1r/seedora/internal/ddl"
	"github.com/bakhod1r/seedora/internal/model"
)

func init() {
	db.Register(open, "postgres", "postgresql", "pgx", "cockroach", "cockroachdb", "yugabyte")
}

// Driver is a connected Postgres database.
type Driver struct {
	conn *pgx.Conn
}

func open(ctx context.Context, dsn string) (db.Driver, error) {
	// Non-standard schemes are aliases for the same protocol; pgx only accepts
	// the two it knows.
	dsn = normalizeScheme(dsn)
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	return &Driver{conn: conn}, nil
}

func normalizeScheme(dsn string) string {
	for _, alias := range []string{"cockroachdb://", "cockroach://", "yugabyte://", "pgx://"} {
		if strings.HasPrefix(dsn, alias) {
			return "postgres://" + strings.TrimPrefix(dsn, alias)
		}
	}
	return dsn
}

// Name implements db.Driver.
func (d *Driver) Name() string { return "PostgreSQL" }

// History reads whatever a migration tool left behind. Postgres records no DDL
// history of its own: the catalog describes the schema as it is now, and past
// ALTERs are recoverable only from a tool's own bookkeeping table.
func (d *Driver) History(ctx context.Context) ([]model.Migration, error) {
	const q = `
SELECT table_name
FROM information_schema.tables
WHERE table_schema = ANY (current_schemas(false))`

	rows, err := d.conn.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list tables: %w", err)
	}
	present := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return nil, err
		}
		present[name] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return db.ReadHistory(ctx, model.QuoteIdent,
		func(table string) bool { return present[table] },
		func(ctx context.Context, query string) ([]map[string]any, error) {
			rows, err := d.conn.Query(ctx, query)
			if err != nil {
				return nil, err
			}
			defer rows.Close()

			var out []map[string]any
			for rows.Next() {
				values, err := rows.Values()
				if err != nil {
					return nil, err
				}
				row := make(map[string]any, len(values))
				for i, fd := range rows.FieldDescriptions() {
					row[string(fd.Name)] = values[i]
				}
				out = append(out, row)
			}
			return out, rows.Err()
		}), nil
}

// Dialect implements db.Driver.
func (d *Driver) Dialect() ddl.Dialect { return ddl.Postgres }

// Close implements db.Driver.
func (d *Driver) Close(ctx context.Context) error { return d.conn.Close(ctx) }

// Begin implements db.Driver.
func (d *Driver) Begin(ctx context.Context) (db.Tx, error) {
	tx, err := d.conn.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	return &Tx{tx: tx}, nil
}

// Introspect reads tables, columns, keys, and enum types in four queries rather
// than one per table: on a schema with a few hundred tables the round trips
// dominate everything else.
func (d *Driver) Introspect(ctx context.Context) (*model.Schema, error) {
	s := &model.Schema{Enums: map[string]model.Values{}}

	if err := d.loadEnums(ctx, s); err != nil {
		return nil, err
	}
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

func (d *Driver) loadEnums(ctx context.Context, s *model.Schema) error {
	const q = `
SELECT t.typname, e.enumlabel
FROM pg_type t
JOIN pg_enum e ON e.enumtypid = t.oid
JOIN pg_namespace n ON n.oid = t.typnamespace
WHERE n.nspname NOT IN ('pg_catalog', 'information_schema')
ORDER BY t.typname, e.enumsortorder`
	rows, err := d.conn.Query(ctx, q)
	if err != nil {
		return fmt.Errorf("read enum types: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var typ, label string
		if err := rows.Scan(&typ, &label); err != nil {
			return err
		}
		s.Enums[typ] = append(s.Enums[typ], label)
	}
	return rows.Err()
}

// loadColumns reads every ordinary table and its columns. Views, materialised
// views, foreign tables, and partitions of a partitioned table are excluded:
// none of them is a thing you insert seed rows into directly.
func (d *Driver) loadColumns(ctx context.Context, s *model.Schema) (map[string]*model.Table, error) {
	const q = `
SELECT n.nspname,
       c.relname,
       a.attname,
       t.typname,
       format_type(a.atttypid, a.atttypmod),
       NOT a.attnotnull                          AS nullable,
       (a.atthasdef OR a.attidentity <> '')      AS has_default,
       a.attgenerated <> ''                      AS generated,
       t.typtype = 'e'                           AS is_enum,
       a.attnum
FROM pg_attribute a
JOIN pg_class c      ON c.oid = a.attrelid
JOIN pg_namespace n  ON n.oid = c.relnamespace
JOIN pg_type t       ON t.oid = a.atttypid
WHERE c.relkind IN ('r', 'p')
  AND NOT c.relispartition
  AND a.attnum > 0
  AND NOT a.attisdropped
  AND n.nspname NOT IN ('pg_catalog', 'information_schema', 'pg_toast')
ORDER BY n.nspname, c.relname, a.attnum`

	rows, err := d.conn.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("read columns: %w", err)
	}
	defer rows.Close()

	byName := map[string]*model.Table{}
	for rows.Next() {
		var (
			ns, table, col, typ, native             string
			nullable, hasDefault, generated, isEnum bool
			attnum                                  int16
		)
		if err := rows.Scan(&ns, &table, &col, &typ, &native,
			&nullable, &hasDefault, &generated, &isEnum, &attnum); err != nil {
			return nil, err
		}
		t, ok := byName[table]
		if !ok {
			t = &model.Table{Schema: ns, Name: table}
			byName[table] = t
			s.Tables = append(s.Tables, t)
		}
		c := &model.Column{
			Name:       col,
			Type:       typ,
			Native:     native,
			Nullable:   nullable,
			HasDefault: hasDefault,
			Generated:  generated,
		}
		if isEnum {
			c.EnumType = typ
		}
		c.MaxLen, c.Precision, c.Scale = decorations(native)
		t.Columns = append(t.Columns, c)
	}
	return byName, rows.Err()
}

// loadConstraints marks primary keys, single-column unique constraints, and
// foreign keys. Multi-column unique constraints are read but not marked on the
// individual columns: uniqueness of a pair is not uniqueness of either half, and
// generating unique values per column would be both wrong and slower.
func (d *Driver) loadConstraints(ctx context.Context, byName map[string]*model.Table) error {
	const q = `
SELECT con.contype,
       c.relname                                        AS table_name,
       a.attname                                        AS column_name,
       COALESCE(fc.relname, '')                         AS ref_table,
       COALESCE(fa.attname, '')                         AS ref_column,
       cardinality(con.conkey)                          AS key_width
FROM pg_constraint con
JOIN pg_class c        ON c.oid = con.conrelid
JOIN pg_namespace n    ON n.oid = c.relnamespace
JOIN unnest(con.conkey) WITH ORDINALITY AS k(attnum, ord) ON TRUE
JOIN pg_attribute a    ON a.attrelid = con.conrelid AND a.attnum = k.attnum
LEFT JOIN pg_class fc  ON fc.oid = con.confrelid
LEFT JOIN unnest(con.confkey) WITH ORDINALITY AS fk(attnum, ord)
       ON fk.ord = k.ord
LEFT JOIN pg_attribute fa
       ON fa.attrelid = con.confrelid AND fa.attnum = fk.attnum
WHERE con.contype IN ('p', 'u', 'f')
  AND n.nspname NOT IN ('pg_catalog', 'information_schema')
ORDER BY c.relname, con.conname, k.ord`

	rows, err := d.conn.Query(ctx, q)
	if err != nil {
		return fmt.Errorf("read constraints: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			ctype                           string
			table, column, refTable, refCol string
			width                           int32
		)
		if err := rows.Scan(&ctype, &table, &column, &refTable, &refCol, &width); err != nil {
			return err
		}
		t, ok := byName[table]
		if !ok {
			continue
		}
		c := t.Column(column)
		if c == nil {
			continue
		}
		switch ctype {
		case "p":
			t.PrimaryKey = append(t.PrimaryKey, column)
			if width == 1 {
				c.Unique = true
			}
		case "u":
			if width == 1 {
				c.Unique = true
			}
		case "f":
			// A composite foreign key cannot be satisfied column by column, so
			// only single-column keys become a Ref. Composite keys stay
			// unmarked and the user maps them explicitly.
			if width == 1 && refTable != "" {
				c.FK = &model.Ref{Table: refTable, Column: refCol}
			}
		}
	}
	return rows.Err()
}

// loadCounts uses the planner's row estimate rather than COUNT(*). The number
// exists to answer "is this table empty, and what would truncating destroy",
// and an estimate answers that instantly on a table where COUNT(*) would take
// minutes. An estimate of -1 means the table has never been analysed, which is
// itself a strong hint that it is empty.
func (d *Driver) loadCounts(ctx context.Context, s *model.Schema) error {
	const q = `
SELECT c.relname, GREATEST(c.reltuples, 0)::bigint
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ('r', 'p')
  AND n.nspname NOT IN ('pg_catalog', 'information_schema')`
	rows, err := d.conn.Query(ctx, q)
	if err != nil {
		return fmt.Errorf("read row estimates: %w", err)
	}
	defer rows.Close()
	counts := map[string]int64{}
	for rows.Next() {
		var name string
		var n int64
		if err := rows.Scan(&name, &n); err != nil {
			return err
		}
		counts[name] = n
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, t := range s.Tables {
		t.ExistingRows = counts[t.Name]
	}
	return nil
}

// decorations pulls the length, precision, and scale out of a rendered type such
// as "character varying(255)" or "numeric(10,2)".
func decorations(native string) (maxLen, precision, scale int) {
	open := strings.IndexByte(native, '(')
	if open < 0 || !strings.HasSuffix(native, ")") {
		return 0, 0, 0
	}
	body := native[open+1 : len(native)-1]
	parts := strings.Split(body, ",")
	first := atoi(strings.TrimSpace(parts[0]))
	base := strings.ToLower(strings.TrimSpace(native[:open]))
	switch {
	case strings.Contains(base, "char"):
		return first, 0, 0
	case strings.Contains(base, "numeric"), strings.Contains(base, "decimal"):
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

// Tx is a Postgres seeding transaction.
type Tx struct {
	tx   pgx.Tx
	done bool
}

// Truncate implements db.Tx. RESTART IDENTITY resets the sequences behind serial
// columns, so a re-seed produces the same ids as the first run — which is what
// makes --seed reproducible across truncate-and-reseed cycles. CASCADE is
// required because a table with children cannot be truncated without it.
func (t *Tx) Truncate(ctx context.Context, tb *model.Table) error {
	_, err := t.tx.Exec(ctx, "TRUNCATE TABLE "+tb.Qualified()+" RESTART IDENTITY CASCADE")
	if err != nil {
		return fmt.Errorf("truncate %s: %w", tb.Name, err)
	}
	return nil
}

// Exec implements db.Tx. The schema editor renders its own SQL and applies it
// here, inside the transaction it can still roll back.
func (t *Tx) Exec(ctx context.Context, sql string) error {
	if _, err := t.tx.Exec(ctx, sql); err != nil {
		return fmt.Errorf("exec: %w", err)
	}
	return nil
}

// Insert implements db.Tx using COPY — one COPY for the whole table, however
// many rows it holds.
//
// This is the cheapest thing the server can be asked to do: a single statement,
// a single parse, one pass through the wire protocol, and no per-row round trip.
// pgx pulls rows through CopyFromSource as fast as the socket drains, so the
// database is never waiting on the generator and the generator is never waiting
// on the database. An INSERT per batch would cost a statement per batch and put
// the two in lockstep.
func (t *Tx) Insert(ctx context.Context, tb *model.Table, cols []string, rows db.Source) (int64, error) {
	if len(cols) == 0 {
		return 0, nil
	}
	ident := pgx.Identifier{tb.Name}
	if tb.Schema != "" {
		ident = pgx.Identifier{tb.Schema, tb.Name}
	}
	src := newPullSource(cols, rows)
	defer src.stop()

	n, err := t.tx.CopyFrom(ctx, ident, cols, src)
	if err != nil {
		// A generator failure surfaces here as a short stream, so its own error
		// is the more useful one to report.
		if gerr := rows.Err(); gerr != nil {
			return n, fmt.Errorf("generate rows for %s: %w", tb.Name, gerr)
		}
		return n, fmt.Errorf("copy into %s: %w", tb.Name, decorate(err))
	}
	if gerr := rows.Err(); gerr != nil {
		return n, fmt.Errorf("generate rows for %s: %w", tb.Name, gerr)
	}
	return n, nil
}

// ReadKeys implements db.Tx. It reads inside the transaction on purpose: the
// parent rows it returns were written by this same uncommitted run, and any
// other connection would not see them.
func (t *Tx) ReadKeys(ctx context.Context, tb *model.Table, col string, limit int) ([]any, error) {
	q := fmt.Sprintf("SELECT %s FROM %s WHERE %s IS NOT NULL LIMIT %d",
		model.QuoteIdent(col), tb.Qualified(), model.QuoteIdent(col), limit)
	rows, err := t.tx.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("read keys from %s.%s: %w", tb.Name, col, err)
	}
	defer rows.Close()
	var out []any
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			return nil, err
		}
		if len(vals) > 0 {
			out = append(out, vals[0])
		}
	}
	return out, rows.Err()
}

// Commit implements db.Tx.
func (t *Tx) Commit(ctx context.Context) error {
	if t.done {
		return nil
	}
	t.done = true
	return t.tx.Commit(ctx)
}

// Rollback implements db.Tx. It is safe to call after Commit so the seeder can
// defer it without tracking which path it took.
func (t *Tx) Rollback(ctx context.Context) error {
	if t.done {
		return nil
	}
	t.done = true
	err := t.tx.Rollback(ctx)
	if err == pgx.ErrTxClosed {
		return nil
	}
	return err
}

// pullSource adapts a push-style row sequence to pgx's pull-style
// CopyFromSource. iter.Pull does the inversion with a coroutine rather than a
// goroutine and a channel, so there is no per-row synchronisation on the hot
// path — which matters, because this path runs once per row of the entire seed.
type pullSource struct {
	cols []string
	next func() (map[string]any, bool)
	stop func()
	row  map[string]any
	buf  []any
}

func newPullSource(cols []string, src db.Source) *pullSource {
	next, stop := iter.Pull(src.Rows())
	return &pullSource{
		cols: cols,
		next: next,
		stop: stop,
		buf:  make([]any, len(cols)),
	}
}

func (s *pullSource) Next() bool {
	row, ok := s.next()
	s.row = row
	return ok
}

func (s *pullSource) Values() ([]any, error) {
	for j, c := range s.cols {
		// A missing key is a NULL, which is how a column the plan skips reaches
		// the database as its own default rather than as a zero value.
		s.buf[j] = s.row[c]
	}
	return s.buf, nil
}

// Err is pgx's error hook. Generation errors are reported by Insert from the
// Source itself, so there is nothing to add here.
func (s *pullSource) Err() error { return nil }

// decorate turns a bare Postgres error into one that names the constraint or
// column at fault. COPY reports a violation without saying which row, and the
// detail field is usually the only thing that identifies it.
func decorate(err error) error {
	var pg *pgconn.PgError
	if !errors.As(err, &pg) {
		return err
	}
	parts := []string{pg.Message}
	if pg.Detail != "" {
		parts = append(parts, pg.Detail)
	}
	if pg.ColumnName != "" {
		parts = append(parts, "column "+pg.ColumnName)
	}
	if pg.ConstraintName != "" {
		parts = append(parts, "constraint "+pg.ConstraintName)
	}
	return fmt.Errorf("%s", strings.Join(parts, "; "))
}
