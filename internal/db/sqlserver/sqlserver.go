// Package sqlserver implements the Seedora driver for Microsoft SQL Server and
// Azure SQL.
//
// SQL Server is the one engine outside Postgres that keeps every promise the
// contract makes. DDL is transactional, so a schema change the editor applies
// unwinds with everything else; and the wire protocol carries a bulk load —
// TDS bulk copy, the same thing bcp uses — so a table goes in as one stream
// rather than as a series of INSERTs.
//
// The catalog is read from sys.* rather than information_schema, which hides
// exactly what seeding needs: which columns are identity, which are computed,
// and what the row count is without counting.
package sqlserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	mssql "github.com/microsoft/go-mssqldb"

	"github.com/bakhod1r/seedora/internal/db"
	"github.com/bakhod1r/seedora/internal/ddl"
	"github.com/bakhod1r/seedora/internal/model"
)

func init() {
	db.Register(open, "sqlserver", "mssql")
}

// Driver is a connected SQL Server database.
type Driver struct {
	db *sql.DB
}

func open(ctx context.Context, dsn string) (db.Driver, error) {
	// The driver registers both names but only parses the one URL form; mssql://
	// is what older tools print, and it means the same thing.
	if strings.HasPrefix(dsn, "mssql://") {
		dsn = "sqlserver://" + strings.TrimPrefix(dsn, "mssql://")
	}
	conn, err := sql.Open("sqlserver", dsn)
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	// One connection, because the whole run is a single transaction and bulk
	// copy holds the session it started on until the stream is closed.
	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(1)
	if err := conn.PingContext(ctx); err != nil {
		conn.Close()
		return nil, fmt.Errorf("connect: %w", err)
	}
	return &Driver{db: conn}, nil
}

// Name implements db.Driver.
func (d *Driver) Name() string { return "SQL Server" }

// Dialect implements db.Driver.
func (d *Driver) Dialect() ddl.Dialect { return ddl.SQLServer }

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
	// SQL Server has no enum type of any kind — the equivalent is a check
	// constraint or a lookup table, neither of which is a list of labels — so
	// the map stays empty rather than being guessed at.
	s := &model.Schema{Enums: map[string]model.Values{}}

	byName, err := d.loadColumns(ctx, s)
	if err != nil {
		return nil, err
	}
	if err := d.loadIndexes(ctx, byName); err != nil {
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
SELECT sch.name, t.name, c.name, ty.name,
       c.is_nullable, c.is_identity, c.is_computed,
       CASE WHEN c.default_object_id <> 0 THEN 1 ELSE 0 END,
       c.max_length, c.precision, c.scale
FROM sys.columns c
JOIN sys.tables t    ON t.object_id = c.object_id
JOIN sys.schemas sch ON sch.schema_id = t.schema_id
JOIN sys.types ty    ON ty.user_type_id = c.user_type_id
WHERE t.is_ms_shipped = 0
ORDER BY sch.name, t.name, c.column_id`

	rows, err := d.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("read columns: %w", err)
	}
	defer rows.Close()

	byName := map[string]*model.Table{}
	for rows.Next() {
		var (
			schema, table, name, typ           string
			nullable, identity, computed, hasD bool
			maxLen, precision, scale           int
		)
		if err := rows.Scan(&schema, &table, &name, &typ, &nullable, &identity,
			&computed, &hasD, &maxLen, &precision, &scale); err != nil {
			return nil, err
		}
		t := byName[schema+"."+table]
		if t == nil {
			t = &model.Table{Schema: schema, Name: table}
			byName[schema+"."+table] = t
			s.Tables = append(s.Tables, t)
		}
		c := &model.Column{
			Name:     name,
			Type:     strings.ToLower(typ),
			Nullable: nullable,
			// An identity column fills itself, which is the same thing to the
			// planner as a column with a default.
			HasDefault: hasD || identity,
			Generated:  computed,
		}
		c.MaxLen = characters(typ, maxLen)
		if isNumeric(c.Type) {
			c.Precision, c.Scale = precision, scale
		}
		c.Native = native(c.Type, maxLen, precision, scale)
		t.Columns = append(t.Columns, c)
	}
	return byName, rows.Err()
}

// characters converts sys.columns.max_length, which is bytes, into the declared
// character limit. The wide types store two bytes per character, and -1 is the
// MAX spelling, which has no limit worth reporting.
func characters(typ string, maxLen int) int {
	switch strings.ToLower(typ) {
	case "nchar", "nvarchar":
		if maxLen < 0 {
			return 0
		}
		return maxLen / 2
	case "char", "varchar", "binary", "varbinary":
		if maxLen < 0 {
			return 0
		}
		return maxLen
	}
	return 0
}

func isNumeric(typ string) bool {
	return typ == "decimal" || typ == "numeric" || typ == "money" || typ == "smallmoney"
}

// native rebuilds the declaration the way the table was written, which is what
// the UI shows. sys.* stores the parts rather than the text.
func native(typ string, maxLen, precision, scale int) string {
	if isNumeric(typ) {
		return fmt.Sprintf("%s(%d,%d)", typ, precision, scale)
	}
	switch typ {
	case "nchar", "nvarchar", "char", "varchar", "binary", "varbinary":
		if maxLen < 0 {
			return typ + "(max)"
		}
		return fmt.Sprintf("%s(%d)", typ, characters(typ, maxLen))
	}
	return typ
}

// loadIndexes fills in primary keys and single-column uniqueness. SQL Server
// records a unique constraint as a unique index, so one query answers both.
func (d *Driver) loadIndexes(ctx context.Context, byName map[string]*model.Table) error {
	const q = `
SELECT sch.name, t.name, i.index_id, c.name, i.is_primary_key, i.is_unique
FROM sys.indexes i
JOIN sys.tables t          ON t.object_id = i.object_id
JOIN sys.schemas sch       ON sch.schema_id = t.schema_id
JOIN sys.index_columns ic  ON ic.object_id = i.object_id AND ic.index_id = i.index_id
JOIN sys.columns c         ON c.object_id = ic.object_id AND c.column_id = ic.column_id
WHERE t.is_ms_shipped = 0
  AND ic.is_included_column = 0
  -- A filtered index constrains only the rows matching its predicate, so it is
  -- not a promise the seeder can generate against.
  AND i.has_filter = 0
ORDER BY sch.name, t.name, i.index_id, ic.key_ordinal`

	rows, err := d.db.QueryContext(ctx, q)
	if err != nil {
		return fmt.Errorf("read indexes: %w", err)
	}
	defer rows.Close()

	type key struct {
		table string
		index int64
	}
	cols := map[key][]string{}
	primary := map[key]bool{}
	uniq := map[key]bool{}
	var order []key
	for rows.Next() {
		var schema, table, column string
		var indexID int64
		var isPK, isUnique bool
		if err := rows.Scan(&schema, &table, &indexID, &column, &isPK, &isUnique); err != nil {
			return err
		}
		k := key{schema + "." + table, indexID}
		if _, seen := cols[k]; !seen {
			order = append(order, k)
		}
		cols[k] = append(cols[k], column)
		primary[k], uniq[k] = isPK, isUnique
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
		// Only a single-column unique index constrains one generator; a
		// composite one is a constraint across columns the seeder cannot honour
		// by making any single value unique.
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
SELECT sch.name, t.name, fk.name, pc.name, rt.name, rc.name
FROM sys.foreign_keys fk
JOIN sys.foreign_key_columns fkc ON fkc.constraint_object_id = fk.object_id
JOIN sys.tables t    ON t.object_id = fk.parent_object_id
JOIN sys.schemas sch ON sch.schema_id = t.schema_id
JOIN sys.columns pc  ON pc.object_id = fkc.parent_object_id AND pc.column_id = fkc.parent_column_id
JOIN sys.tables rt   ON rt.object_id = fk.referenced_object_id
JOIN sys.columns rc  ON rc.object_id = fkc.referenced_object_id AND rc.column_id = fkc.referenced_column_id
ORDER BY sch.name, t.name, fk.name, fkc.constraint_column_id`

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
		var schema, table, name, column, refTable, refColumn string
		if err := rows.Scan(&schema, &table, &name, &column, &refTable, &refColumn); err != nil {
			return err
		}
		k := key{schema + "." + table, name}
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

// loadCounts reads the row count the storage engine already keeps per
// partition. It answers "is this table empty, and what would truncating
// destroy" instantly on a table where COUNT(*) would take minutes.
func (d *Driver) loadCounts(ctx context.Context, byName map[string]*model.Table) error {
	const q = `
SELECT sch.name, t.name, SUM(p.rows)
FROM sys.tables t
JOIN sys.schemas sch ON sch.schema_id = t.schema_id
JOIN sys.partitions p ON p.object_id = t.object_id AND p.index_id IN (0, 1)
WHERE t.is_ms_shipped = 0
GROUP BY sch.name, t.name`

	rows, err := d.db.QueryContext(ctx, q)
	if err != nil {
		return fmt.Errorf("read row counts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var schema, table string
		var n int64
		if err := rows.Scan(&schema, &table, &n); err != nil {
			return err
		}
		if t := byName[schema+"."+table]; t != nil {
			t.ExistingRows = n
		}
	}
	return rows.Err()
}

// History reads whatever a migration tool left behind. SQL Server records no
// DDL history of its own.
func (d *Driver) History(ctx context.Context) ([]model.Migration, error) {
	rows, err := d.db.QueryContext(ctx, `
SELECT t.name FROM sys.tables t WHERE t.is_ms_shipped = 0`)
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
		// Identifiers are compared under the database's collation, which is
		// case-insensitive by default; lowering both sides keeps the lookup
		// agreeing with the server on a table named Flyway_Schema_History.
		present[strings.ToLower(n)] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return db.ReadHistory(ctx, model.QuoteIdent,
		func(table string) bool { return present[strings.ToLower(table)] },
		func(ctx context.Context, query string) ([]map[string]any, error) {
			rows, err := d.db.QueryContext(ctx, limitToTop(query))
			if err != nil {
				return nil, err
			}
			defer rows.Close()
			return scanRows(rows)
		}), nil
}

// limitToTop rewrites the trailing LIMIT the shared history reader appends into
// the TOP that SQL Server spells it with. Without this every history query is
// a syntax error, which the reader would quietly read as "no such table".
func limitToTop(query string) string {
	i := strings.LastIndex(query, " LIMIT ")
	if i < 0 || !strings.HasPrefix(query, "SELECT ") {
		return query
	}
	n := strings.TrimSpace(query[i+len(" LIMIT "):])
	return "SELECT TOP (" + n + ") " + query[len("SELECT "):i]
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

// Tx is a SQL Server seeding transaction.
type Tx struct {
	tx   *sql.Tx
	done bool
}

// Exec implements db.Tx. SQL Server is one of the few engines where DDL is
// transactional, so a schema change applied here really does unwind with the
// rest of the run.
func (t *Tx) Exec(ctx context.Context, stmt string) error {
	if _, err := t.tx.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("exec: %w", err)
	}
	return nil
}

// Truncate implements db.Tx with DELETE rather than TRUNCATE TABLE.
//
// TRUNCATE is transactional here, unlike on MySQL, but SQL Server refuses it
// outright on any table a foreign key points at — even when the referencing
// table is empty, and even when the key is disabled. DELETE is slower and is
// the only version that works on the tables a seeded schema actually has.
func (t *Tx) Truncate(ctx context.Context, tb *model.Table) error {
	if _, err := t.tx.ExecContext(ctx, "DELETE FROM "+tb.Qualified()); err != nil {
		return fmt.Errorf("truncate %s: %w", tb.Name, err)
	}
	// Reseed the identity column so a re-seed produces the same ids as the
	// first run, which is what makes --seed reproducible across
	// truncate-and-reseed cycles. A table without one makes this an error, and
	// there is nothing to report: it simply has no counter.
	_, _ = t.tx.ExecContext(ctx, "DBCC CHECKIDENT ("+literal(tb)+", RESEED, 0)")
	return nil
}

// literal is the table name as DBCC takes it: a string, not an identifier, and
// not a parameter either — DBCC is not a statement the server will prepare.
func literal(tb *model.Table) string {
	name := tb.Name
	if tb.Schema != "" {
		name = tb.Schema + "." + tb.Name
	}
	return "'" + strings.ReplaceAll(name, "'", "''") + "'"
}

// Insert implements db.Tx using TDS bulk copy — the protocol bcp speaks, and
// the only path into SQL Server that is not a statement per batch.
//
// The rows stream: each one is written to the wire as it arrives, the server
// never waits on the generator, and the final Exec with no arguments closes the
// stream and reports what landed. It runs inside the run's transaction, so a
// failure anywhere still unwinds the whole load.
func (t *Tx) Insert(ctx context.Context, tb *model.Table, cols []string, rows db.Source) (int64, error) {
	if len(cols) == 0 {
		return 0, nil
	}
	// KEEP_NULLS is what makes an absent field arrive as NULL rather than as the
	// column's default, which is the contract Insert states and what every other
	// driver here does.
	stmt, err := t.tx.PrepareContext(ctx,
		mssql.CopyIn(tb.Qualified(), mssql.BulkOptions{KeepNulls: true}, cols...))
	if err != nil {
		return 0, fmt.Errorf("prepare bulk copy into %s: %w", tb.Name, err)
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
			loopErr = fmt.Errorf("bulk copy into %s: %w", tb.Name, err)
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
	// An Exec with no arguments is how the driver spells "end of stream"; until
	// it runs, nothing is guaranteed to have reached the table.
	if _, err := stmt.ExecContext(ctx); err != nil {
		return written, fmt.Errorf("finish bulk copy into %s: %w", tb.Name, err)
	}
	return written, nil
}

// value adapts the few generated types the driver will not take as they are.
func value(v any) any {
	switch x := v.(type) {
	case nil:
		return nil
	case map[string]any, []any:
		// There is no JSON type before SQL Server 2016 and the column is
		// nvarchar in every version that has one, so the text is what goes.
		return jsonText(x)
	case uint64:
		// Bulk copy has no unsigned integer, and anything a generator produces
		// is far inside the signed range.
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
	q := fmt.Sprintf("SELECT TOP (%s) %s FROM %s WHERE %s IS NOT NULL",
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
