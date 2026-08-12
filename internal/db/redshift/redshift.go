// Package redshift implements the Seedora driver for Amazon Redshift.
//
// Redshift speaks the Postgres wire protocol, so this uses pgx exactly as
// internal/db/postgres does, and the one thing that matters most is inherited
// whole: Redshift has real transactions. BEGIN, COMMIT and ROLLBACK do what
// they say, so unlike ClickHouse, Trino and Databricks this driver keeps
// Seedora's promise that a run is one transaction and a failure leaves the
// database exactly as it was.
//
// What it cannot inherit is the postgres driver's code, and it is worth saying
// exactly why rather than leaving the duplication looking accidental. Three
// things diverge, and each is on the hot path:
//
//   - No COPY from a client connection. Redshift's COPY loads from S3, EMR or
//     DynamoDB and has no FROM STDIN form at all, so the protocol-level bulk
//     path the postgres driver is built around does not exist here. The fast
//     path is instead a multi-row INSERT, which is what Amazon's own guidance
//     says to use when the data is not already in S3.
//   - A thinner catalog. The postgres driver reads pg_catalog with
//     `unnest(...) WITH ORDINALITY` and `cardinality`, and filters on
//     `pg_class.relispartition`. Redshift's leader node has neither function
//     and no such column, so those queries do not merely return less — they
//     error. information_schema is what Redshift actually serves, so that is
//     what is read.
//   - No RESTART IDENTITY. An IDENTITY column's counter can only be reset by
//     recreating the table, and TRUNCATE here commits the transaction on its
//     own, which is the promise above thrown away. Truncate is a DELETE for
//     that reason, the same concession the MySQL driver makes and for the same
//     reason.
//
// Everything genuinely shared — the migration-table catalogue, identifier
// quoting, the Source contract — is used from internal/db and internal/model
// rather than restated.
package redshift

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/bakhod1r/seedora/internal/db"
	"github.com/bakhod1r/seedora/internal/ddl"
	"github.com/bakhod1r/seedora/internal/model"
)

func init() {
	db.Register(open, "redshift")
}

// Driver is a connected Redshift cluster or serverless workgroup.
type Driver struct {
	conn *pgx.Conn
}

func open(ctx context.Context, dsn string) (db.Driver, error) {
	// The scheme is only how Seedora routes the DSN here; pgx takes the two it
	// knows and the rest of the URL is a Postgres URL already.
	if rest, ok := strings.CutPrefix(strings.TrimSpace(dsn), "redshift://"); ok {
		dsn = "postgres://" + rest
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse DSN: %w", err)
	}
	// Redshift's implementation of the extended protocol is partial: it does
	// not answer Describe for every statement pgx would prepare, and pgx's
	// statement cache trips over that on the first non-trivial query. The
	// simple protocol interpolates arguments client side instead, which also
	// lifts the 65535-parameter ceiling the multi-row INSERT below would
	// otherwise hit long before the statement got large enough to be worth
	// sending.
	cfg.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	return &Driver{conn: conn}, nil
}

// Name implements db.Driver.
func (d *Driver) Name() string { return "Redshift" }

// Dialect implements db.Driver. Redshift's SQL is Postgres 8.0's, which is the
// dialect the schema editor already renders.
func (d *Driver) Dialect() ddl.Dialect { return ddl.Postgres }

// Close implements db.Driver.
func (d *Driver) Close(ctx context.Context) error { return d.conn.Close(ctx) }

// Begin implements db.Driver. This is a real transaction: everything the run
// does is inside it, and Rollback undoes all of it.
func (d *Driver) Begin(ctx context.Context) (db.Tx, error) {
	tx, err := d.conn.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	return &Tx{tx: tx}, nil
}

// Introspect reads information_schema and svv_table_info. See the package
// comment for why the postgres driver's pg_catalog queries are not reused: they
// error on Redshift rather than returning less.
func (d *Driver) Introspect(ctx context.Context) (*model.Schema, error) {
	// Redshift has no user-defined enum types — CREATE TYPE is not supported —
	// so the map stays empty and every enum-shaped column is a varchar the user
	// maps to a value list by hand.
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

// systemSchemas are the ones with nothing seedable in them. pg_internal is
// Redshift's own and is not in the Postgres list.
const systemSchemas = `('pg_catalog', 'information_schema', 'pg_internal', 'pg_automv')`

// loadColumns reads every ordinary table and its columns. External tables —
// Spectrum's, over S3 — are excluded by information_schema itself, which lists
// only local ones; they are read-only from Redshift's side anyway, so there is
// nothing to seed into them.
func (d *Driver) loadColumns(ctx context.Context, s *model.Schema) (map[string]*model.Table, error) {
	const q = `
SELECT c.table_schema, c.table_name, c.column_name, c.data_type,
       c.is_nullable, c.column_default,
       COALESCE(c.character_maximum_length, 0),
       COALESCE(c.numeric_precision, 0),
       COALESCE(c.numeric_scale, 0)
FROM information_schema.columns c
JOIN information_schema.tables t
  ON t.table_schema = c.table_schema AND t.table_name = c.table_name
WHERE t.table_type = 'BASE TABLE'
  AND c.table_schema NOT IN ` + systemSchemas + `
ORDER BY c.table_schema, c.table_name, c.ordinal_position`

	rows, err := d.conn.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("read columns: %w", err)
	}
	defer rows.Close()

	byName := map[string]*model.Table{}
	for rows.Next() {
		var (
			ns, table, name, dataType, nullable string
			def                                 *string
			maxLen, precision, scale            int
		)
		if err := rows.Scan(&ns, &table, &name, &dataType, &nullable, &def,
			&maxLen, &precision, &scale); err != nil {
			return nil, err
		}
		t := byName[table]
		if t == nil {
			t = &model.Table{Schema: ns, Name: table}
			byName[table] = t
			s.Tables = append(s.Tables, t)
		}
		c := &model.Column{
			Name:     name,
			Type:     dataType,
			Native:   native(dataType, maxLen, precision, scale),
			Nullable: strings.EqualFold(nullable, "YES"),
			// An IDENTITY column reports its default as `"identity"(…)`, which
			// is a default like any other as far as the planner cares: leave the
			// column out of the insert and Redshift fills it.
			HasDefault: def != nil,
			MaxLen:     maxLen,
			Precision:  precision,
			Scale:      scale,
		}
		// A decimal's precision is meaningful; an integer's is a width, and
		// passing it on makes inference read a bigint as a bounded number.
		if !strings.Contains(dataType, "numeric") && !strings.Contains(dataType, "decimal") &&
			!strings.Contains(dataType, "double") && !strings.Contains(dataType, "real") {
			c.Precision, c.Scale = 0, 0
		}
		// Redshift has no generated columns, so nothing is ever unwritable.
		t.Columns = append(t.Columns, c)
	}
	return byName, rows.Err()
}

// native rebuilds the decorated type — "character varying(255)", "numeric(10,2)"
// — which information_schema splits across three columns and which is what the
// UI shows.
func native(dataType string, maxLen, precision, scale int) string {
	switch {
	case maxLen > 0:
		return dataType + "(" + strconv.Itoa(maxLen) + ")"
	case (strings.Contains(dataType, "numeric") || strings.Contains(dataType, "decimal")) && precision > 0:
		return dataType + "(" + strconv.Itoa(precision) + "," + strconv.Itoa(scale) + ")"
	}
	return dataType
}

// loadConstraints marks primary keys, single-column unique constraints, and
// foreign keys.
//
// Redshift does not enforce any of them: they are declared, recorded, and
// trusted by the query planner, and an insert that violates one succeeds. They
// are still what tells the planner here which column is a key and which table a
// column points at, so a schema declared without them looks like a set of
// unrelated tables — and a duplicate value in a "unique" column will not fail
// the run the way it would on Postgres, which is worth knowing when a seeded
// dataset later turns out to have them.
func (d *Driver) loadConstraints(ctx context.Context, byName map[string]*model.Table) error {
	// Postgres reads this from pg_constraint with unnest(conkey) WITH
	// ORDINALITY. Redshift's leader node has neither unnest nor WITH
	// ORDINALITY, so the constraint's width comes from a window function over
	// information_schema instead.
	const q = `
SELECT tc.constraint_type,
       kcu.table_name,
       kcu.column_name,
       COALESCE(ccu.table_name, ''),
       COALESCE(ccu.column_name, ''),
       COUNT(*) OVER (PARTITION BY tc.constraint_schema, tc.constraint_name) AS key_width
FROM information_schema.table_constraints tc
JOIN information_schema.key_column_usage kcu
  ON kcu.constraint_schema = tc.constraint_schema
 AND kcu.constraint_name = tc.constraint_name
LEFT JOIN information_schema.referential_constraints rc
  ON rc.constraint_schema = tc.constraint_schema
 AND rc.constraint_name = tc.constraint_name
LEFT JOIN information_schema.constraint_column_usage ccu
  ON ccu.constraint_schema = rc.unique_constraint_schema
 AND ccu.constraint_name = rc.unique_constraint_name
WHERE tc.constraint_type IN ('PRIMARY KEY', 'UNIQUE', 'FOREIGN KEY')
  AND tc.table_schema NOT IN ` + systemSchemas + `
ORDER BY kcu.table_name, tc.constraint_name, kcu.ordinal_position`

	rows, err := d.conn.Query(ctx, q)
	if err != nil {
		return fmt.Errorf("read constraints: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var ctype, table, column, refTable, refCol string
		var width int64
		if err := rows.Scan(&ctype, &table, &column, &refTable, &refCol, &width); err != nil {
			return err
		}
		t := byName[table]
		if t == nil {
			continue
		}
		c := t.Column(column)
		if c == nil {
			continue
		}
		switch strings.ToUpper(ctype) {
		case "PRIMARY KEY":
			t.PrimaryKey = append(t.PrimaryKey, column)
			if width == 1 {
				c.Unique = true
			}
		case "UNIQUE":
			if width == 1 {
				c.Unique = true
			}
		case "FOREIGN KEY":
			// A composite foreign key cannot be satisfied column by column, so
			// only single-column keys become a Ref.
			if width == 1 && refTable != "" {
				c.FK = &model.Ref{Table: refTable, Column: refCol}
			}
		}
	}
	return rows.Err()
}

// loadCounts reads the row count per table from svv_table_info.
//
// Postgres uses pg_class.reltuples; Redshift keeps that column but never
// updates it outside ANALYZE, so it is stale far more often than not.
// svv_table_info is maintained by the system and is what the console shows. A
// table missing from it has never had a block written to it, which is exactly
// the empty table the truncate confirmation wants to report as empty.
func (d *Driver) loadCounts(ctx context.Context, s *model.Schema) error {
	const q = `SELECT "schema", "table", tbl_rows FROM svv_table_info`
	rows, err := d.conn.Query(ctx, q)
	if err != nil {
		return fmt.Errorf("read row counts: %w", err)
	}
	defer rows.Close()

	type key struct{ schema, table string }
	counts := map[key]int64{}
	for rows.Next() {
		var schema, table string
		var n float64
		if err := rows.Scan(&schema, &table, &n); err != nil {
			return err
		}
		counts[key{schema, table}] = int64(n)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, t := range s.Tables {
		t.ExistingRows = counts[key{t.Schema, t.Name}]
	}
	return nil
}

// History reads whatever a migration tool left behind. Redshift records no DDL
// history of its own: STL_DDLTEXT holds the last few days of statement text and
// is a log, not a schema history.
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

// Tx is a Redshift seeding transaction — a real one.
type Tx struct {
	tx   pgx.Tx
	done bool
}

// Truncate implements db.Tx with DELETE rather than TRUNCATE TABLE.
//
// TRUNCATE on Redshift commits the transaction it runs in, which is the whole
// unwind the seeder depends on, thrown away before the first row is written.
// DELETE is slower and is the only version that keeps the promise that a
// failure leaves the database as it was. This is the same trade the MySQL
// driver makes, for the same reason.
//
// There is no RESTART IDENTITY to go with it either: an IDENTITY column's
// counter cannot be reset by any statement — recreating the table is the only
// way — so unlike Postgres a re-seed does not reproduce the same ids. The rows
// are reproducible; the keys the database assigns are not.
func (t *Tx) Truncate(ctx context.Context, tb *model.Table) error {
	if _, err := t.tx.Exec(ctx, "DELETE FROM "+tb.Qualified()); err != nil {
		return fmt.Errorf("truncate %s: %w", tb.Name, err)
	}
	return nil
}

// Exec implements db.Tx. Redshift's DDL is transactional, so unlike MySQL a
// schema change applied here is still inside the transaction that can undo it.
func (t *Tx) Exec(ctx context.Context, sql string) error {
	if _, err := t.tx.Exec(ctx, sql); err != nil {
		return fmt.Errorf("exec: %w", err)
	}
	return nil
}

// insertRows and insertBytes bound one INSERT. Redshift refuses a statement
// over 16 MB, and a single-row INSERT is the documented worst way to load it —
// each one costs a commit-queue round trip regardless of how little it writes,
// which is why these are as large as they are.
const (
	insertRows  = 5000
	insertBytes = 8 << 20
)

// Insert implements db.Tx with multi-row INSERT statements.
//
// There is no COPY here. Redshift's COPY reads from S3, EMR, DynamoDB or a
// remote host over SSH; there is no FROM STDIN, so the protocol-level bulk load
// the postgres driver uses cannot be reached from a client connection at all.
// Loading through S3 would mean holding the user's bucket credentials, writing
// their generated data to their cloud storage, and leaving it there — which is
// a different tool. A multi-row INSERT is what Amazon recommends for everything
// not already in S3, and it is transactional, which the COPY route would still
// have been.
func (t *Tx) Insert(ctx context.Context, tb *model.Table, cols []string, rows db.Source) (int64, error) {
	if len(cols) == 0 {
		return 0, nil
	}
	quoted := make([]string, len(cols))
	for i, c := range cols {
		quoted[i] = model.QuoteIdent(c)
	}
	head := "INSERT INTO " + tb.Qualified() + " (" + strings.Join(quoted, ", ") + ") VALUES "

	var (
		sb      strings.Builder
		args    []any
		tuples  int
		written int64
	)
	flush := func() error {
		if tuples == 0 {
			return nil
		}
		if _, err := t.tx.Exec(ctx, head+sb.String(), args...); err != nil {
			return fmt.Errorf("insert into %s: %w", tb.Name, err)
		}
		written += int64(tuples)
		sb.Reset()
		args = args[:0]
		tuples = 0
		return nil
	}

	var loopErr error
	for row := range rows.Rows() {
		if tuples > 0 {
			sb.WriteByte(',')
		}
		sb.WriteByte('(')
		for i, c := range cols {
			if i > 0 {
				sb.WriteByte(',')
			}
			// A column absent from the row map is NULL, which is how a column
			// the plan skips reaches the database — as NULL rather than as a
			// zero value. A column meant to take its default belongs out of the
			// column list.
			args = append(args, value(row[c]))
			sb.WriteByte('$')
			sb.WriteString(strconv.Itoa(len(args)))
		}
		sb.WriteByte(')')
		tuples++
		if tuples >= insertRows || sb.Len() >= insertBytes {
			if err := flush(); err != nil {
				loopErr = err
				break
			}
		}
	}
	if loopErr != nil {
		return written, loopErr
	}
	// A generator that fails simply stops yielding, so without this a short
	// write would look like a successful one.
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
	case map[string]any, []any:
		// Redshift's SUPER column takes JSON text; there is no encoder for a Go
		// map, and the server wants the text anyway.
		b, err := json.Marshal(x)
		if err != nil {
			// Losing a hundred thousand rows to one unserialisable cell is the
			// worse trade.
			return nil
		}
		return string(b)
	default:
		return v
	}
}

// ReadKeys implements db.Tx. It reads inside the transaction on purpose: the
// parent rows it returns were written by this same uncommitted run, and any
// other connection would not see them.
func (t *Tx) ReadKeys(ctx context.Context, tb *model.Table, col string, limit int) ([]any, error) {
	name := model.QuoteIdent(col)
	q := fmt.Sprintf("SELECT %s FROM %s WHERE %s IS NOT NULL LIMIT %d",
		name, tb.Qualified(), name, limit)
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

// Rollback implements db.Tx, and here it genuinely rolls back: every insert,
// every delete, and every schema change the run made goes away together. It is
// safe to call after Commit so the seeder can defer it without tracking which
// path it took.
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
