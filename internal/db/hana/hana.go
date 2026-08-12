// Package hana implements the Seedora driver for SAP HANA.
//
// HANA's driver carries a real bulk path: a prepared INSERT can be executed
// with a function that the driver calls once per row until it says stop, and
// the rows go out in packets rather than one statement per batch. Insert uses
// it, which keeps generation off the database's critical path the way COPY does
// on Postgres.
//
// What HANA does not have is transactional DDL. Every CREATE or ALTER commits
// the open transaction first, so a schema change the editor applies cannot be
// rolled back, and TRUNCATE — which is DDL here — would commit the seeding run
// halfway through. Truncation is therefore a DELETE.
//
// The catalog is read from SYS.*, scoped to CURRENT_SCHEMA, which is the same
// scoping the MySQL driver gets from DATABASE().
package hana

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"iter"

	hdb "github.com/SAP/go-hdb/driver"

	"github.com/bakhod1r/seedora/internal/db"
	"github.com/bakhod1r/seedora/internal/ddl"
	"github.com/bakhod1r/seedora/internal/model"
)

func init() {
	db.Register(open, "hana", "hdb")
}

// Driver is a connected HANA database.
type Driver struct {
	db *sql.DB
}

func open(ctx context.Context, dsn string) (db.Driver, error) {
	native, err := NativeDSN(dsn)
	if err != nil {
		return nil, err
	}
	conn, err := sql.Open(hdb.DriverName, native)
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	// One connection, because the whole run is a single transaction and the
	// current schema is a property of the session.
	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(1)
	if err := conn.PingContext(ctx); err != nil {
		conn.Close()
		return nil, fmt.Errorf("connect: %w", err)
	}
	return &Driver{db: conn}, nil
}

// NativeDSN converts the DSN into the form go-hdb expects.
//
// People type `hana://user:pass@host:39015/MYSCHEMA`, because that is the shape
// every other engine's DSN has. go-hdb wants the scheme it registered and takes
// the schema as a parameter rather than as a path, so the path is moved.
func NativeDSN(dsn string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(dsn))
	if err != nil {
		return "", fmt.Errorf("parse DSN: %w", err)
	}
	u.Scheme = hdb.DriverName
	if schema := strings.TrimPrefix(u.Path, "/"); schema != "" {
		q := u.Query()
		if q.Get(hdb.DSNDefaultSchema) == "" {
			q.Set(hdb.DSNDefaultSchema, schema)
		}
		u.RawQuery = q.Encode()
		u.Path = ""
	}
	return u.String(), nil
}

// Name implements db.Driver.
func (d *Driver) Name() string { return "SAP HANA" }

// Dialect implements db.Driver.
func (d *Driver) Dialect() ddl.Dialect { return ddl.HANA }

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

// Introspect reads the catalog in four queries rather than one per table: on a
// schema with a few hundred tables the round trips dominate everything else.
func (d *Driver) Introspect(ctx context.Context) (*model.Schema, error) {
	// HANA has no enum type: the equivalent is a check constraint, which is a
	// predicate rather than a list of labels, so the map stays empty rather
	// than being guessed at.
	s := &model.Schema{Enums: map[string]model.Values{}}

	byName, err := d.loadColumns(ctx, s)
	if err != nil {
		return nil, err
	}
	if err := d.loadConstraints(ctx, byName); err != nil {
		return nil, err
	}
	if err := d.loadForeignKeys(ctx, byName); err != nil {
		return nil, err
	}
	if err := d.loadCounts(ctx, byName); err != nil {
		return nil, err
	}
	return s, nil
}

func (d *Driver) loadColumns(ctx context.Context, s *model.Schema) (map[string]*model.Table, error) {
	const q = `
SELECT c.TABLE_NAME, c.COLUMN_NAME, c.DATA_TYPE_NAME, c.IS_NULLABLE,
       CASE WHEN c.DEFAULT_VALUE IS NULL THEN 0 ELSE 1 END,
       CASE WHEN c.GENERATION_TYPE IS NULL THEN 0 ELSE 1 END,
       IFNULL(c.LENGTH, 0), IFNULL(c.SCALE, 0)
FROM SYS.TABLE_COLUMNS c
JOIN SYS.TABLES t ON t.SCHEMA_NAME = c.SCHEMA_NAME AND t.TABLE_NAME = c.TABLE_NAME
WHERE c.SCHEMA_NAME = CURRENT_SCHEMA
  AND t.IS_USER_DEFINED_TYPE = 'FALSE'
ORDER BY c.TABLE_NAME, c.POSITION`

	rows, err := d.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("read columns: %w", err)
	}
	defer rows.Close()

	byName := map[string]*model.Table{}
	for rows.Next() {
		var (
			table, name, dataType, nullable string
			hasDefault, generated           int
			length, scale                   int
		)
		if err := rows.Scan(&table, &name, &dataType, &nullable,
			&hasDefault, &generated, &length, &scale); err != nil {
			return nil, err
		}
		t := byName[table]
		if t == nil {
			t = &model.Table{Name: table}
			byName[table] = t
			s.Tables = append(s.Tables, t)
		}
		typ := strings.ToLower(dataType)
		c := &model.Column{
			Name:     name,
			Type:     typ,
			Native:   native(dataType, length, scale),
			Nullable: nullable == "TRUE",
			// An identity column carries a GENERATION_TYPE and fills itself,
			// which is the same thing to the planner as having a default. A
			// generated column is the ALWAYS AS form, which cannot be written
			// at all.
			HasDefault: hasDefault == 1 || generated == 1,
		}
		if strings.Contains(typ, "char") {
			c.MaxLen = length
		}
		if typ == "decimal" || typ == "smalldecimal" {
			c.Precision, c.Scale = length, scale
		}
		t.Columns = append(t.Columns, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := d.loadGenerated(ctx, byName); err != nil {
		return nil, err
	}
	return byName, nil
}

// loadGenerated marks the columns HANA computes rather than stores. They cannot
// be written, and TABLE_COLUMNS distinguishes them from an identity column only
// by the generation type, which is a separate lookup on older servers.
func (d *Driver) loadGenerated(ctx context.Context, byName map[string]*model.Table) error {
	const q = `
SELECT TABLE_NAME, COLUMN_NAME
FROM SYS.TABLE_COLUMNS
WHERE SCHEMA_NAME = CURRENT_SCHEMA AND GENERATION_TYPE = 'ALWAYS AS'`
	rows, err := d.db.QueryContext(ctx, q)
	if err != nil {
		return fmt.Errorf("read generated columns: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var table, column string
		if err := rows.Scan(&table, &column); err != nil {
			return err
		}
		if t := byName[table]; t != nil {
			if c := t.Column(column); c != nil {
				c.Generated = true
			}
		}
	}
	return rows.Err()
}

// native rebuilds the declaration as the table was written, which is what the
// UI shows. The catalog stores the parts rather than the text.
func native(dataType string, length, scale int) string {
	switch {
	case strings.Contains(dataType, "DECIMAL") && length > 0:
		return fmt.Sprintf("%s(%d,%d)", dataType, length, scale)
	case strings.Contains(dataType, "CHAR") && length > 0:
		return fmt.Sprintf("%s(%d)", dataType, length)
	}
	return dataType
}

// loadConstraints marks primary keys and single-column uniqueness. HANA records
// both in one view, so one query answers both.
func (d *Driver) loadConstraints(ctx context.Context, byName map[string]*model.Table) error {
	const q = `
SELECT TABLE_NAME, CONSTRAINT_NAME, COLUMN_NAME, IS_PRIMARY_KEY, IS_UNIQUE_KEY
FROM SYS.CONSTRAINTS
WHERE SCHEMA_NAME = CURRENT_SCHEMA
ORDER BY TABLE_NAME, CONSTRAINT_NAME, POSITION`

	rows, err := d.db.QueryContext(ctx, q)
	if err != nil {
		return fmt.Errorf("read constraints: %w", err)
	}
	defer rows.Close()

	type key struct{ table, name string }
	cols := map[key][]string{}
	primary := map[key]bool{}
	uniq := map[key]bool{}
	var order []key
	for rows.Next() {
		var table, name, column, isPK, isUnique string
		if err := rows.Scan(&table, &name, &column, &isPK, &isUnique); err != nil {
			return err
		}
		k := key{table, name}
		if _, seen := cols[k]; !seen {
			order = append(order, k)
		}
		cols[k] = append(cols[k], column)
		primary[k] = isPK == "TRUE"
		uniq[k] = isUnique == "TRUE" || isPK == "TRUE"
	}
	if err := rows.Err(); err != nil {
		return err
	}

	for _, k := range order {
		t := byName[k.table]
		if t == nil {
			continue
		}
		if primary[k] {
			t.PrimaryKey = append(t.PrimaryKey, cols[k]...)
		}
		// Only a single-column constraint constrains one generator; a composite
		// one holds across columns, which the seeder cannot honour by making
		// any single value unique.
		if uniq[k] && len(cols[k]) == 1 {
			if c := t.Column(cols[k][0]); c != nil {
				c.Unique = true
			}
		}
	}
	return nil
}

func (d *Driver) loadForeignKeys(ctx context.Context, byName map[string]*model.Table) error {
	const q = `
SELECT TABLE_NAME, CONSTRAINT_NAME, COLUMN_NAME,
       REFERENCED_TABLE_NAME, REFERENCED_COLUMN_NAME
FROM SYS.REFERENTIAL_CONSTRAINTS
WHERE SCHEMA_NAME = CURRENT_SCHEMA
ORDER BY TABLE_NAME, CONSTRAINT_NAME, POSITION`

	rows, err := d.db.QueryContext(ctx, q)
	if err != nil {
		return fmt.Errorf("read foreign keys: %w", err)
	}
	defer rows.Close()

	type key struct{ table, name string }
	type part struct{ from, refTable, refColumn string }
	byKey := map[key][]part{}
	var order []key
	for rows.Next() {
		var table, name, column, refTable, refColumn string
		if err := rows.Scan(&table, &name, &column, &refTable, &refColumn); err != nil {
			return err
		}
		k := key{table, name}
		if _, seen := byKey[k]; !seen {
			order = append(order, k)
		}
		byKey[k] = append(byKey[k], part{column, refTable, refColumn})
	}
	if err := rows.Err(); err != nil {
		return err
	}

	for _, k := range order {
		parts := byKey[k]
		// A composite key cannot be satisfied one column at a time, so it is
		// left alone rather than half-applied.
		if len(parts) != 1 {
			continue
		}
		t := byName[k.table]
		if t == nil {
			continue
		}
		if c := t.Column(parts[0].from); c != nil {
			c.FK = &model.Ref{Table: parts[0].refTable, Column: parts[0].refColumn}
		}
	}
	return nil
}

// loadCounts reads the record count the engine already keeps per table, which
// answers "is this table empty, and what would truncating destroy" without
// scanning anything.
func (d *Driver) loadCounts(ctx context.Context, byName map[string]*model.Table) error {
	const q = `
SELECT TABLE_NAME, RECORD_COUNT FROM SYS.M_TABLES WHERE SCHEMA_NAME = CURRENT_SCHEMA`
	rows, err := d.db.QueryContext(ctx, q)
	if err != nil {
		return fmt.Errorf("read row counts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var n int64
		if err := rows.Scan(&name, &n); err != nil {
			return err
		}
		if t := byName[name]; t != nil {
			t.ExistingRows = n
		}
	}
	return rows.Err()
}

// History reads whatever a migration tool left behind. HANA records no DDL
// history of its own.
//
// Names are upper-cased on the way in: an identifier written without quotes is
// folded to upper case and stored that way, which is how every migration tool
// creates its bookkeeping table.
func (d *Driver) History(ctx context.Context) ([]model.Migration, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT TABLE_NAME FROM SYS.TABLES WHERE SCHEMA_NAME = CURRENT_SCHEMA`)
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

	quote := func(s string) string { return model.QuoteIdent(strings.ToUpper(s)) }
	return db.ReadHistory(ctx, quote,
		func(table string) bool { return present[strings.ToUpper(table)] },
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
			// The history catalogue names its columns in lower case, and HANA
			// hands them back folded to upper.
			key := strings.ToLower(name)
			if b, ok := cells[i].([]byte); ok {
				row[key] = string(b)
				continue
			}
			row[key] = cells[i]
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// Tx is a HANA seeding transaction.
type Tx struct {
	tx   *sql.Tx
	done bool
}

// Exec implements db.Tx.
//
// HANA commits the open transaction before every DDL statement, so unlike
// Postgres or SQL Server a schema change applied here cannot be rolled back.
// The statements are validated and rendered before they run, and they run in
// dependency order, which is what is left to offer.
func (t *Tx) Exec(ctx context.Context, stmt string) error {
	if _, err := t.tx.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("exec: %w", err)
	}
	return nil
}

// Truncate implements db.Tx with DELETE rather than TRUNCATE TABLE, because
// TRUNCATE is DDL on HANA: it commits the transaction the seeder is relying on
// to unwind a failed run. DELETE is slower and is the only version that keeps
// the promise that a failure leaves the database as it was.
func (t *Tx) Truncate(ctx context.Context, tb *model.Table) error {
	if _, err := t.tx.ExecContext(ctx, "DELETE FROM "+tb.Qualified()); err != nil {
		return fmt.Errorf("truncate %s: %w", tb.Name, err)
	}
	// The sequence behind an identity column is deliberately left alone:
	// restarting it is an ALTER, which would commit the transaction.
	return nil
}

// Insert implements db.Tx using go-hdb's function-based bulk exec: the driver
// calls back for each row and sends them in packets, rather than taking one
// statement per batch.
//
// The rows stream — a hundred thousand of them are never held at once — and the
// statement is prepared once for the whole table. The driver documents that a
// bulk exec is not atomic across its internal packets, which would matter if
// this ran on its own; it runs inside the run's transaction, so a failure
// partway still unwinds everything already written.
func (t *Tx) Insert(ctx context.Context, tb *model.Table, cols []string, rows db.Source) (int64, error) {
	if len(cols) == 0 {
		return 0, nil
	}
	quoted := make([]string, len(cols))
	marks := make([]string, len(cols))
	for i, c := range cols {
		quoted[i] = model.QuoteIdent(c)
		marks[i] = "?"
	}
	stmt, err := t.tx.PrepareContext(ctx, "INSERT INTO "+tb.Qualified()+
		" ("+strings.Join(quoted, ", ")+") VALUES ("+strings.Join(marks, ", ")+")")
	if err != nil {
		return 0, fmt.Errorf("prepare insert into %s: %w", tb.Name, err)
	}
	defer stmt.Close()

	next, stop := pull(rows)
	defer stop()

	var written int64
	fill := func(args []any) error {
		row, ok := next()
		if !ok {
			return hdb.ErrEndOfRows
		}
		for i, c := range cols {
			args[i] = value(row[c])
		}
		written++
		return nil
	}

	if _, err := stmt.ExecContext(ctx, fill); err != nil {
		// A generator failure surfaces here as a short stream, so its own error
		// is the more useful one to report.
		if gerr := rows.Err(); gerr != nil {
			return written, fmt.Errorf("generate rows for %s: %w", tb.Name, gerr)
		}
		return written, fmt.Errorf("insert into %s: %w", tb.Name, err)
	}
	if err := rows.Err(); err != nil {
		return written, fmt.Errorf("generate rows for %s: %w", tb.Name, err)
	}
	return written, nil
}

// pull inverts the source's push-style sequence into the next-row call the
// driver's fill function has to make. iter.Pull does it with a coroutine rather
// than a goroutine and a channel, so there is no per-row synchronisation on a
// path that runs once for every row of the entire seed.
func pull(src db.Source) (func() (map[string]any, bool), func()) {
	return iter.Pull(src.Rows())
}

// value adapts the few generated types the driver will not take as they are.
func value(v any) any {
	switch x := v.(type) {
	case nil:
		return nil
	case map[string]any, []any:
		// HANA stores JSON in an NCLOB, and the server wants the text.
		return jsonText(x)
	case uint64:
		return int64(x)
	default:
		return v
	}
}

// jsonText encodes a generated JSON value as the text a character column takes.
// A value that cannot be encoded is written as NULL rather than failing the
// run: the alternative is losing a hundred thousand rows to one unserialisable
// cell.
func jsonText(v any) any {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return string(b)
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
