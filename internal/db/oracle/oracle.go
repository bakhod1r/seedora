// Package oracle implements the Seedora driver for Oracle Database.
//
// Two things about Oracle shape this driver.
//
// The first is that DDL commits. Every CREATE or ALTER ends the open
// transaction before it runs and starts a new one after, so a schema change the
// editor applies cannot be rolled back — and TRUNCATE, which is DDL here, would
// commit the seeding run halfway through. Truncation is therefore a DELETE, and
// Exec says what it cannot promise at the point it runs.
//
// The second is that there is no COPY. What Oracle has instead is array
// binding: one INSERT parsed once and executed with a column-wise array of
// values, which the server applies as a single call. That is what Insert uses,
// chunked so the arrays stay a sane size in memory.
//
// Everything is read from the USER_* catalog views, which are the current
// schema's own objects — the same scoping the MySQL driver gets from DATABASE().
package oracle

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	_ "github.com/sijms/go-ora/v2"

	"github.com/bakhod1r/seedora/internal/db"
	"github.com/bakhod1r/seedora/internal/ddl"
	"github.com/bakhod1r/seedora/internal/model"
)

func init() {
	db.Register(open, "oracle")
}

// Driver is a connected Oracle database.
type Driver struct {
	db *sql.DB
}

func open(ctx context.Context, dsn string) (db.Driver, error) {
	conn, err := sql.Open("oracle", dsn)
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	// One connection, because the whole run is a single transaction and the
	// session is where Oracle keeps it.
	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(1)
	if err := conn.PingContext(ctx); err != nil {
		conn.Close()
		return nil, fmt.Errorf("connect: %w", err)
	}
	return &Driver{db: conn}, nil
}

// Name implements db.Driver.
func (d *Driver) Name() string { return "Oracle" }

// Dialect implements db.Driver.
func (d *Driver) Dialect() ddl.Dialect { return ddl.Oracle }

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
	// Oracle has no enum type: the equivalent is a check constraint, which is a
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
	if err := d.loadCounts(ctx, s); err != nil {
		return nil, err
	}
	return s, nil
}

// loadColumns reads every ordinary table and its columns.
//
// The default is read as DEFAULT_LENGTH rather than DATA_DEFAULT: the value
// itself is a LONG, which cannot be joined, filtered, or fetched alongside
// ordinary columns without special handling, and all the planner needs to know
// is whether one exists.
func (d *Driver) loadColumns(ctx context.Context, s *model.Schema) (map[string]*model.Table, error) {
	const q = `
SELECT c.TABLE_NAME, c.COLUMN_NAME, c.DATA_TYPE, c.NULLABLE,
       CASE WHEN c.DEFAULT_LENGTH IS NULL THEN 0 ELSE 1 END,
       CASE WHEN c.VIRTUAL_COLUMN = 'YES' THEN 1 ELSE 0 END,
       NVL(c.CHAR_LENGTH, 0), NVL(c.DATA_PRECISION, 0), NVL(c.DATA_SCALE, 0)
FROM USER_TAB_COLS c
JOIN USER_TABLES t ON t.TABLE_NAME = c.TABLE_NAME
WHERE c.HIDDEN_COLUMN = 'NO'
ORDER BY c.TABLE_NAME, c.COLUMN_ID`

	rows, err := d.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("read columns: %w", err)
	}
	defer rows.Close()

	byName := map[string]*model.Table{}
	for rows.Next() {
		var (
			table, name, dataType, nullable string
			hasDefault, virtual             int
			charLen, precision, scale       int
		)
		if err := rows.Scan(&table, &name, &dataType, &nullable, &hasDefault,
			&virtual, &charLen, &precision, &scale); err != nil {
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
			Name:       name,
			Type:       typ,
			Native:     native(dataType, charLen, precision, scale),
			Nullable:   nullable == "Y",
			HasDefault: hasDefault == 1,
			Generated:  virtual == 1,
		}
		if strings.Contains(typ, "char") {
			c.MaxLen = charLen
		}
		if typ == "number" {
			c.Precision, c.Scale = precision, scale
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
// no default in USER_TAB_COLS — the sequence behind it is a separate object —
// so without this the planner would try to generate its primary keys.
func (d *Driver) loadIdentity(ctx context.Context, byName map[string]*model.Table) error {
	const q = `SELECT TABLE_NAME, COLUMN_NAME FROM USER_TAB_IDENTITY_COLS`
	rows, err := d.db.QueryContext(ctx, q)
	if err != nil {
		// The view arrived in 12c. On an older server there are no identity
		// columns to find, and failing the whole introspection over it would be
		// wrong.
		return nil
	}
	defer rows.Close()
	for rows.Next() {
		var table, column string
		if err := rows.Scan(&table, &column); err != nil {
			return err
		}
		if t := byName[table]; t != nil {
			if c := t.Column(column); c != nil {
				c.HasDefault = true
			}
		}
	}
	return rows.Err()
}

// native rebuilds the declaration as the table was written, which is what the
// UI shows. The catalog stores the parts rather than the text.
func native(dataType string, charLen, precision, scale int) string {
	switch {
	case strings.Contains(dataType, "CHAR") && charLen > 0:
		return fmt.Sprintf("%s(%d)", dataType, charLen)
	case dataType == "NUMBER" && precision > 0 && scale > 0:
		return fmt.Sprintf("NUMBER(%d,%d)", precision, scale)
	case dataType == "NUMBER" && precision > 0:
		return fmt.Sprintf("NUMBER(%d)", precision)
	}
	return dataType
}

// loadConstraints marks primary keys and single-column uniqueness.
func (d *Driver) loadConstraints(ctx context.Context, byName map[string]*model.Table) error {
	const q = `
SELECT c.TABLE_NAME, c.CONSTRAINT_NAME, c.CONSTRAINT_TYPE, cc.COLUMN_NAME
FROM USER_CONSTRAINTS c
JOIN USER_CONS_COLUMNS cc ON cc.CONSTRAINT_NAME = c.CONSTRAINT_NAME
WHERE c.CONSTRAINT_TYPE IN ('P', 'U') AND c.STATUS = 'ENABLED'
ORDER BY c.TABLE_NAME, c.CONSTRAINT_NAME, cc.POSITION`

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
		k := key{table, name}
		if _, seen := cols[k]; !seen {
			order = append(order, k)
		}
		cols[k] = append(cols[k], column)
		kind[k] = ctype
	}
	if err := rows.Err(); err != nil {
		return err
	}

	for _, k := range order {
		t := byName[k.table]
		if t == nil {
			continue
		}
		if kind[k] == "P" {
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

// loadForeignKeys resolves each referential constraint to the columns of the
// unique constraint it points at, which is how Oracle records the target: by
// constraint name rather than by table and column.
func (d *Driver) loadForeignKeys(ctx context.Context, byName map[string]*model.Table) error {
	const q = `
SELECT c.TABLE_NAME, c.CONSTRAINT_NAME, cc.COLUMN_NAME,
       rc.TABLE_NAME, rcc.COLUMN_NAME
FROM USER_CONSTRAINTS c
JOIN USER_CONS_COLUMNS cc  ON cc.CONSTRAINT_NAME = c.CONSTRAINT_NAME
JOIN USER_CONSTRAINTS rc   ON rc.CONSTRAINT_NAME = c.R_CONSTRAINT_NAME
JOIN USER_CONS_COLUMNS rcc ON rcc.CONSTRAINT_NAME = rc.CONSTRAINT_NAME
                          AND rcc.POSITION = cc.POSITION
WHERE c.CONSTRAINT_TYPE = 'R'
ORDER BY c.TABLE_NAME, c.CONSTRAINT_NAME, cc.POSITION`

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

// loadCounts uses the optimiser's row estimate rather than COUNT(*). The number
// exists to answer "is this table empty, and what would truncating destroy",
// and an estimate answers that instantly on a table where COUNT(*) would take
// minutes. A table that has never been analysed reports nothing, which is
// itself a hint that nobody has loaded it.
func (d *Driver) loadCounts(ctx context.Context, s *model.Schema) error {
	const q = `SELECT TABLE_NAME, NVL(NUM_ROWS, 0) FROM USER_TABLES`
	rows, err := d.db.QueryContext(ctx, q)
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

// History reads whatever a migration tool left behind. Oracle records no DDL
// history of its own.
//
// Names are upper-cased on the way in: an identifier written without quotes is
// folded to upper case and stored that way, which is how every migration tool
// creates its bookkeeping table. Looking for a lower-case `flyway_schema_history`
// would find nothing.
func (d *Driver) History(ctx context.Context) ([]model.Migration, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT TABLE_NAME FROM USER_TABLES`)
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
			rows, err := d.db.QueryContext(ctx, limitToFetch(query))
			if err != nil {
				return nil, err
			}
			defer rows.Close()
			return scanRows(rows)
		}), nil
}

// limitToFetch rewrites the trailing LIMIT the shared history reader appends
// into the row-limiting clause Oracle spells it with. Without this every
// history query is a syntax error, which the reader would quietly read as "no
// such table".
func limitToFetch(query string) string {
	i := strings.LastIndex(query, " LIMIT ")
	if i < 0 {
		return query
	}
	n := strings.TrimSpace(query[i+len(" LIMIT "):])
	return query[:i] + " FETCH FIRST " + n + " ROWS ONLY"
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
			// The history catalogue names its columns in lower case, and Oracle
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

// Tx is an Oracle seeding transaction.
type Tx struct {
	tx   *sql.Tx
	done bool
}

// Exec implements db.Tx.
//
// Oracle commits the open transaction before and after every DDL statement, so
// unlike Postgres or SQL Server a schema change applied here cannot be rolled
// back. The statements are validated and rendered before they run, and they run
// in dependency order, which is what is left to offer.
func (t *Tx) Exec(ctx context.Context, stmt string) error {
	if _, err := t.tx.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("exec: %w", err)
	}
	return nil
}

// Truncate implements db.Tx with DELETE rather than TRUNCATE TABLE.
//
// TRUNCATE is DDL on Oracle: it commits the transaction the seeder is relying
// on to unwind a failed run, and it is refused outright on a table an enabled
// foreign key points at. DELETE is slower and is the only version that keeps
// the promise that a failure leaves the database as it was.
func (t *Tx) Truncate(ctx context.Context, tb *model.Table) error {
	if _, err := t.tx.ExecContext(ctx, "DELETE FROM "+tb.Qualified()); err != nil {
		return fmt.Errorf("truncate %s: %w", tb.Name, err)
	}
	// The sequence behind an identity column is deliberately left alone:
	// restarting it is an ALTER, which would commit the transaction.
	return nil
}

// chunk is how many rows are bound per execution. Oracle's limit is on the
// number of bind variables — one per column here, however many rows the arrays
// hold — so the cap is memory rather than the statement, and this is the size
// where the round trip has already stopped mattering.
const chunk = 5000

// Insert implements db.Tx using array binding: one INSERT, parsed once, executed
// with a column-wise array of values.
//
// This is the closest thing Oracle has to COPY. The server applies the whole
// array in a single call rather than a round trip per row, and the statement is
// prepared once for the entire table. Rows are pulled from the source a chunk at
// a time, so a hundred thousand of them are never held at once.
func (t *Tx) Insert(ctx context.Context, tb *model.Table, cols []string, rows db.Source) (int64, error) {
	if len(cols) == 0 {
		return 0, nil
	}
	quoted := make([]string, len(cols))
	binds := make([]string, len(cols))
	for i, c := range cols {
		quoted[i] = model.QuoteIdent(c)
		// Oracle binds by name, and a positional marker is spelled :1, :2, …
		binds[i] = ":" + strconv.Itoa(i+1)
	}
	stmt, err := t.tx.PrepareContext(ctx, "INSERT INTO "+tb.Qualified()+
		" ("+strings.Join(quoted, ", ")+") VALUES ("+strings.Join(binds, ", ")+")")
	if err != nil {
		return 0, fmt.Errorf("prepare insert into %s: %w", tb.Name, err)
	}
	defer stmt.Close()

	// One array per column, filled row by row: the driver reads a slice per
	// bind variable and takes the shortest as the array size.
	arrays := make([][]any, len(cols))
	for i := range arrays {
		arrays[i] = make([]any, 0, chunk)
	}
	args := make([]any, len(cols))
	var written int64

	flush := func() error {
		if len(arrays[0]) == 0 {
			return nil
		}
		n := len(arrays[0])
		for i := range arrays {
			args[i] = arrays[i]
		}
		if _, err := stmt.ExecContext(ctx, args...); err != nil {
			return fmt.Errorf("insert into %s: %w", tb.Name, err)
		}
		written += int64(n)
		for i := range arrays {
			arrays[i] = arrays[i][:0]
		}
		return nil
	}

	var loopErr error
	for row := range rows.Rows() {
		for i, c := range cols {
			arrays[i] = append(arrays[i], value(row[c]))
		}
		if len(arrays[0]) == chunk {
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

// value adapts the few generated types the driver will not take as they are.
func value(v any) any {
	switch x := v.(type) {
	case nil:
		return nil
	case bool:
		// Oracle has no boolean column type before 23c; a flag is a NUMBER(1),
		// and that is what the seeded schema will have.
		if x {
			return int64(1)
		}
		return int64(0)
	case map[string]any, []any:
		// JSON is stored in a character column on every version that matters,
		// and the server wants the text either way.
		return jsonText(x)
	case uint64:
		return int64(x)
	default:
		return v
	}
}

// jsonText encodes a generated JSON value as the text a JSON column takes. A
// value that cannot be encoded is written as NULL rather than failing the run:
// the alternative is losing a hundred thousand rows to one unserialisable cell.
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
	// ROWNUM rather than FETCH FIRST, because this runs against whatever
	// version the DSN points at and the pseudo-column has been there forever.
	q := fmt.Sprintf("SELECT %s FROM %s WHERE %s IS NOT NULL AND ROWNUM <= %s",
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
	if err == sql.ErrTxDone {
		return nil
	}
	return err
}
