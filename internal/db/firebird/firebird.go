// Package firebird implements the Seedora driver for Firebird.
//
// Firebird keeps the transactional promise the contract makes — DDL included,
// which puts it in the small group with Postgres, SQLite, and SQL Server: a
// failed run leaves the database exactly as it was.
//
// What it has no answer for is bulk loading. There is no COPY, no bulk protocol
// on the wire, and INSERT takes a single row of VALUES, so the multi-row
// statement the MySQL and SQLite drivers chunk on cannot be written at all.
// Insert therefore prepares one statement and executes it per row; see the
// comment there for what that costs and why the alternatives are worse.
//
// The catalog is the RDB$ system tables, which store what other engines expose
// as views: names padded to their column width, types as numeric codes, and
// nothing at all about row counts.
package firebird

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	_ "github.com/nakagami/firebirdsql"

	"github.com/bakhod1r/seedora/internal/db"
	"github.com/bakhod1r/seedora/internal/ddl"
	"github.com/bakhod1r/seedora/internal/model"
)

func init() {
	db.Register(open, "firebird", "fdb")
}

// Driver is a connected Firebird database.
type Driver struct {
	db *sql.DB
}

func open(ctx context.Context, dsn string) (db.Driver, error) {
	// The driver reads one spelling of the URL; fdb:// is what some tools
	// print, and it means the same thing.
	if strings.HasPrefix(dsn, "fdb://") {
		dsn = "firebird://" + strings.TrimPrefix(dsn, "fdb://")
	}
	conn, err := sql.Open("firebirdsql", dsn)
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	// One connection, because the whole run is a single transaction and
	// Firebird ties one to the connection that started it.
	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(1)
	if err := conn.PingContext(ctx); err != nil {
		conn.Close()
		return nil, fmt.Errorf("connect: %w", err)
	}
	return &Driver{db: conn}, nil
}

// Name implements db.Driver.
func (d *Driver) Name() string { return "Firebird" }

// Dialect implements db.Driver.
func (d *Driver) Dialect() ddl.Dialect { return ddl.Firebird }

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
	// Firebird has no enum type: the equivalent is a check constraint or a
	// domain with one, which is a predicate rather than a list of labels, so
	// the map stays empty rather than being guessed at.
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
	if err := d.loadCounts(ctx, s); err != nil {
		return nil, err
	}
	return s, nil
}

// loadColumns reads every ordinary table and its columns. A view has BLR for
// its body, which is what separates it from a table here, and the system tables
// carry a flag of their own.
func (d *Driver) loadColumns(ctx context.Context, s *model.Schema) (map[string]*model.Table, error) {
	// RDB$DEFAULT_SOURCE and RDB$COMPUTED_SOURCE are blobs, and only their
	// presence matters, so they are tested rather than read: fetching a blob
	// per column would be a round trip per column.
	const q = `
SELECT rf.RDB$RELATION_NAME, rf.RDB$FIELD_NAME,
       f.RDB$FIELD_TYPE, COALESCE(f.RDB$FIELD_SUB_TYPE, 0),
       COALESCE(rf.RDB$NULL_FLAG, 0),
       CASE WHEN rf.RDB$DEFAULT_SOURCE IS NULL AND f.RDB$DEFAULT_SOURCE IS NULL
            THEN 0 ELSE 1 END,
       CASE WHEN f.RDB$COMPUTED_SOURCE IS NULL THEN 0 ELSE 1 END,
       COALESCE(f.RDB$CHARACTER_LENGTH, 0),
       COALESCE(f.RDB$FIELD_PRECISION, 0), COALESCE(f.RDB$FIELD_SCALE, 0)
FROM RDB$RELATION_FIELDS rf
JOIN RDB$FIELDS f ON f.RDB$FIELD_NAME = rf.RDB$FIELD_SOURCE
JOIN RDB$RELATIONS r ON r.RDB$RELATION_NAME = rf.RDB$RELATION_NAME
WHERE r.RDB$VIEW_BLR IS NULL
  AND COALESCE(r.RDB$SYSTEM_FLAG, 0) = 0
ORDER BY rf.RDB$RELATION_NAME, rf.RDB$FIELD_POSITION`

	rows, err := d.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("read columns: %w", err)
	}
	defer rows.Close()

	byName := map[string]*model.Table{}
	for rows.Next() {
		var (
			table, name                 string
			fieldType, subType          int
			notNull, hasDefault, comput int
			charLen, precision, scale   int
		)
		if err := rows.Scan(&table, &name, &fieldType, &subType, &notNull,
			&hasDefault, &comput, &charLen, &precision, &scale); err != nil {
			return nil, err
		}
		table, name = trim(table), trim(name)

		t := byName[table]
		if t == nil {
			t = &model.Table{Name: table}
			byName[table] = t
			s.Tables = append(s.Tables, t)
		}
		typ := typeName(fieldType, subType, precision, scale)
		c := &model.Column{
			Name:       name,
			Type:       typ,
			Native:     native(typ, charLen, precision, scale),
			Nullable:   notNull == 0,
			HasDefault: hasDefault == 1,
			Generated:  comput == 1,
		}
		if strings.Contains(typ, "char") {
			c.MaxLen = charLen
		}
		if typ == "decimal" {
			// A scale is stored as the negative power of ten it multiplies by.
			c.Precision, c.Scale = precision, -scale
		}
		t.Columns = append(t.Columns, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := d.loadIdentity(ctx, byName); err != nil {
		return nil, err
	}
	return byName, nil
}

// loadIdentity marks the columns that fill themselves. An identity column has
// no default of its own — the generator behind it is a separate object — so
// without this the planner would try to generate its primary keys.
func (d *Driver) loadIdentity(ctx context.Context, byName map[string]*model.Table) error {
	const q = `
SELECT RDB$RELATION_NAME, RDB$FIELD_NAME
FROM RDB$RELATION_FIELDS
WHERE RDB$IDENTITY_TYPE IS NOT NULL`
	rows, err := d.db.QueryContext(ctx, q)
	if err != nil {
		// The column arrived with identity support in Firebird 3. On an older
		// server there is nothing to find, and failing the whole introspection
		// over it would be wrong.
		return nil
	}
	defer rows.Close()
	for rows.Next() {
		var table, column string
		if err := rows.Scan(&table, &column); err != nil {
			return err
		}
		if t := byName[trim(table)]; t != nil {
			if c := t.Column(trim(column)); c != nil {
				c.HasDefault = true
			}
		}
	}
	return rows.Err()
}

// typeName maps RDB$FIELD_TYPE onto the name the type is declared with. The
// catalog stores a code, and the two cases where the code is not enough — the
// sub-type of a blob, and a scaled integer, which is how a DECIMAL is stored —
// are what the other arguments settle.
func typeName(fieldType, subType, precision, scale int) string {
	switch fieldType {
	case 7, 8, 16, 26:
		if scale < 0 || precision > 0 {
			return "decimal"
		}
		switch fieldType {
		case 7:
			return "smallint"
		case 8:
			return "integer"
		case 26:
			return "int128"
		}
		return "bigint"
	case 10:
		return "float"
	case 27:
		return "double precision"
	case 12:
		return "date"
	case 13, 28:
		return "time"
	case 35, 29:
		return "timestamp"
	case 14:
		return "char"
	case 37:
		return "varchar"
	case 23:
		return "boolean"
	case 261:
		// Sub-type 1 is text; everything else is binary, and neither is
		// something the seeder writes without being told to.
		if subType == 1 {
			return "blob sub_type text"
		}
		return "blob"
	}
	return "unknown"
}

// native rebuilds the declaration as the table was written, which is what the
// UI shows. The catalog stores the parts rather than the text.
func native(typ string, charLen, precision, scale int) string {
	switch {
	case strings.Contains(typ, "char") && charLen > 0:
		return fmt.Sprintf("%s(%d)", typ, charLen)
	case typ == "decimal" && precision > 0:
		return fmt.Sprintf("decimal(%d,%d)", precision, -scale)
	}
	return typ
}

// loadConstraints marks primary keys and single-column uniqueness. Firebird
// records both against the index that enforces them, so one query answers both.
func (d *Driver) loadConstraints(ctx context.Context, byName map[string]*model.Table) error {
	const q = `
SELECT rc.RDB$RELATION_NAME, rc.RDB$CONSTRAINT_NAME, rc.RDB$CONSTRAINT_TYPE,
       s.RDB$FIELD_NAME
FROM RDB$RELATION_CONSTRAINTS rc
JOIN RDB$INDEX_SEGMENTS s ON s.RDB$INDEX_NAME = rc.RDB$INDEX_NAME
WHERE rc.RDB$CONSTRAINT_TYPE IN ('PRIMARY KEY', 'UNIQUE')
ORDER BY rc.RDB$RELATION_NAME, rc.RDB$CONSTRAINT_NAME, s.RDB$FIELD_POSITION`

	rows, err := d.db.QueryContext(ctx, q)
	if err != nil {
		return fmt.Errorf("read constraints: %w", err)
	}
	defer rows.Close()

	type key struct{ table, name string }
	cols := map[key][]string{}
	kind := map[key]string{}
	var order []key
	for rows.Next() {
		var table, name, ctype, column string
		if err := rows.Scan(&table, &name, &ctype, &column); err != nil {
			return err
		}
		k := key{trim(table), trim(name)}
		if _, seen := cols[k]; !seen {
			order = append(order, k)
		}
		cols[k] = append(cols[k], trim(column))
		kind[k] = trim(ctype)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	for _, k := range order {
		t := byName[k.table]
		if t == nil {
			continue
		}
		if kind[k] == "PRIMARY KEY" {
			t.PrimaryKey = append(t.PrimaryKey, cols[k]...)
		}
		// Only a single-column constraint constrains one generator; a composite
		// one holds across columns, which the seeder cannot honour by making
		// any single value unique.
		if len(cols[k]) == 1 {
			if c := t.Column(cols[k][0]); c != nil {
				c.Unique = true
			}
		}
	}
	return nil
}

// loadForeignKeys resolves each referential constraint to the index it points
// at, which is how Firebird records the target: by the name of the unique index
// on the other table rather than by table and column.
func (d *Driver) loadForeignKeys(ctx context.Context, byName map[string]*model.Table) error {
	const q = `
SELECT rc.RDB$RELATION_NAME, rc.RDB$CONSTRAINT_NAME, s.RDB$FIELD_NAME,
       rc2.RDB$RELATION_NAME, s2.RDB$FIELD_NAME
FROM RDB$RELATION_CONSTRAINTS rc
JOIN RDB$REF_CONSTRAINTS ref ON ref.RDB$CONSTRAINT_NAME = rc.RDB$CONSTRAINT_NAME
JOIN RDB$RELATION_CONSTRAINTS rc2 ON rc2.RDB$CONSTRAINT_NAME = ref.RDB$CONST_NAME_UQ
JOIN RDB$INDEX_SEGMENTS s ON s.RDB$INDEX_NAME = rc.RDB$INDEX_NAME
JOIN RDB$INDEX_SEGMENTS s2 ON s2.RDB$INDEX_NAME = rc2.RDB$INDEX_NAME
                          AND s2.RDB$FIELD_POSITION = s.RDB$FIELD_POSITION
WHERE rc.RDB$CONSTRAINT_TYPE = 'FOREIGN KEY'
ORDER BY rc.RDB$RELATION_NAME, rc.RDB$CONSTRAINT_NAME, s.RDB$FIELD_POSITION`

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
		k := key{trim(table), trim(name)}
		if _, seen := byKey[k]; !seen {
			order = append(order, k)
		}
		byKey[k] = append(byKey[k], part{trim(column), trim(refTable), trim(refColumn)})
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

// loadCounts counts each table. Firebird keeps no row count anywhere a query
// can reach — the closest thing is a page estimate the optimiser derives at
// prepare time — and a truncate confirmation built on a guess would be a lie.
func (d *Driver) loadCounts(ctx context.Context, s *model.Schema) error {
	for _, t := range s.Tables {
		if err := d.db.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM "+t.Qualified()).Scan(&t.ExistingRows); err != nil {
			return fmt.Errorf("count %s: %w", t.Name, err)
		}
	}
	return nil
}

// History reads whatever a migration tool left behind. Firebird records no DDL
// history of its own.
//
// Names are upper-cased on the way in: an identifier written without quotes is
// folded to upper case and stored that way, which is how every migration tool
// creates its bookkeeping table.
func (d *Driver) History(ctx context.Context) ([]model.Migration, error) {
	rows, err := d.db.QueryContext(ctx, `
SELECT RDB$RELATION_NAME FROM RDB$RELATIONS
WHERE RDB$VIEW_BLR IS NULL AND COALESCE(RDB$SYSTEM_FLAG, 0) = 0`)
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
		present[trim(n)] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	quote := func(s string) string { return model.QuoteIdent(strings.ToUpper(s)) }
	return db.ReadHistory(ctx, quote,
		func(table string) bool { return present[strings.ToUpper(table)] },
		func(ctx context.Context, query string) ([]map[string]any, error) {
			rows, err := d.db.QueryContext(ctx, limitToFirst(query))
			if err != nil {
				return nil, err
			}
			defer rows.Close()
			return scanRows(rows)
		}), nil
}

// limitToFirst rewrites the trailing LIMIT the shared history reader appends
// into the FIRST that Firebird spells it with. Without this every history query
// is a syntax error, which the reader would quietly read as "no such table".
func limitToFirst(query string) string {
	i := strings.LastIndex(query, " LIMIT ")
	if i < 0 || !strings.HasPrefix(query, "SELECT ") {
		return query
	}
	n := strings.TrimSpace(query[i+len(" LIMIT "):])
	return "SELECT FIRST " + n + " " + query[len("SELECT "):i]
}

// trim removes the padding the system tables carry: every name is stored in a
// fixed-width CHAR column and comes back with spaces to the end of it.
func trim(s string) string { return strings.TrimRight(s, " ") }

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
			// The history catalogue names its columns in lower case, and
			// Firebird hands them back folded to upper.
			key := strings.ToLower(trim(name))
			switch v := cells[i].(type) {
			case []byte:
				row[key] = string(v)
			case string:
				row[key] = trim(v)
			default:
				row[key] = v
			}
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// Tx is a Firebird seeding transaction.
type Tx struct {
	tx   *sql.Tx
	done bool
}

// Exec implements db.Tx. Firebird's DDL is transactional, so a schema change
// applied here really does unwind with the rest of the run.
func (t *Tx) Exec(ctx context.Context, stmt string) error {
	if _, err := t.tx.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("exec: %w", err)
	}
	return nil
}

// Truncate implements db.Tx. Firebird has no TRUNCATE of any kind, so DELETE is
// not a concession here — it is the only statement there is.
func (t *Tx) Truncate(ctx context.Context, tb *model.Table) error {
	if _, err := t.tx.ExecContext(ctx, "DELETE FROM "+tb.Qualified()); err != nil {
		return fmt.Errorf("truncate %s: %w", tb.Name, err)
	}
	// The generator behind an identity column is deliberately left alone: it is
	// a separate object, and resetting it is an ALTER SEQUENCE whose effect
	// does not roll back with the rest of the run.
	return nil
}

// Insert implements db.Tx with one prepared INSERT executed per row.
//
// This is the slowest write path of any driver here, and it is what Firebird
// offers. There is no bulk protocol on the wire, and INSERT takes exactly one
// row of VALUES — the multi-row statement the MySQL and SQLite drivers chunk on
// is a syntax error. The two ways around that are both worse: EXECUTE BLOCK
// needs a declared type per placeholder, which the catalog's type codes cannot
// always spell, and a SELECT … UNION ALL feeding the insert leaves every
// parameter untyped, which the engine refuses to prepare.
//
// What is left is a statement prepared once and executed per row, so the engine
// parses once and the rest is bind-and-execute, inside the single transaction
// the whole run lives in. Rows are pulled from the source as they are needed,
// so a hundred thousand of them are never held at once.
func (t *Tx) Insert(ctx context.Context, tb *model.Table, cols []string, rows db.Source) (int64, error) {
	if len(cols) == 0 {
		return 0, nil
	}
	// Firebird's own cap is 65535 parameters per statement, which one row of a
	// table this wide would already exceed; nothing narrower can reach it.
	if len(cols) > 65535 {
		return 0, fmt.Errorf("insert into %s: %d columns exceeds the statement limit",
			tb.Name, len(cols))
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

	var written int64
	args := make([]any, len(cols))

	var loopErr error
	for row := range rows.Rows() {
		for i, c := range cols {
			args[i] = value(row[c])
		}
		if _, err := stmt.ExecContext(ctx, args...); err != nil {
			loopErr = fmt.Errorf("insert into %s: %w", tb.Name, err)
			break
		}
		written++
	}
	if loopErr != nil {
		return written, loopErr
	}
	if err := rows.Err(); err != nil {
		return written, fmt.Errorf("generate rows for %s: %w", tb.Name, err)
	}
	return written, nil
}

// value adapts the few generated types the driver will not take as they are.
func value(v any) any {
	switch x := v.(type) {
	case nil:
		return nil
	case map[string]any, []any:
		// Firebird has no JSON type; the column is a text blob or a varchar,
		// and the server wants the text either way.
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
	// FIRST rather than FETCH FIRST, because it works on every version this
	// driver can connect to.
	q := fmt.Sprintf("SELECT FIRST %s %s FROM %s WHERE %s IS NOT NULL",
		strconv.Itoa(limit), name, tb.Qualified(), name)
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
		switch x := v.(type) {
		case []byte:
			// A key read back as bytes would be written to the child as a byte
			// string, which compares equal to nothing.
			v = string(x)
		case string:
			// A CHAR key comes back padded to its declared width, and the
			// padding is not part of the value the child should point at.
			v = trim(x)
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
