// Package snowflake implements the Seedora driver for Snowflake.
//
// Two things about Snowflake shape this driver and are worth stating up front.
//
// The first is how rows get in. A row-at-a-time INSERT over the connection is
// the wrong shape here: every statement is compiled and dispatched by a virtual
// warehouse, so a hundred thousand single-row inserts is a hundred thousand
// compilations and a seeding run that never finishes. The path Snowflake is
// built for is staged: write the rows to a file, PUT the file onto a stage,
// then COPY INTO the table from the stage. That is what Insert does, and it is
// why the file-writing machinery below exists.
//
// The second is that Seedora's central promise — a run is one transaction, and
// a failure leaves the database exactly as it was — cannot be honoured on
// Snowflake, so this driver does not pretend to. TRUNCATE TABLE and every
// statement the schema editor renders are DDL, and DDL commits the open
// transaction implicitly; a run that dies after the first truncate has already
// destroyed those rows whatever happens next. Wrapping the COPYs in a BEGIN
// would make part of a run reversible and part of it not, which is a worse
// thing to tell a user than the truth. So no transaction is opened, every
// statement autocommits, and the driver instead records exactly what it has
// already made permanent — see Tx.Rollback, which reports that rather than
// returning nil as though it had undone anything.
package snowflake

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	sf "github.com/snowflakedb/gosnowflake"

	"github.com/bakhod1r/seedora/internal/db"
	"github.com/bakhod1r/seedora/internal/ddl"
	"github.com/bakhod1r/seedora/internal/model"
)

func init() {
	db.Register(open, "snowflake")
}

// Driver is a connected Snowflake database.
type Driver struct {
	db *sql.DB
	// database and schema are what the DSN selected. Snowflake's
	// INFORMATION_SCHEMA is per database, and every catalog query below is
	// filtered to one schema, so both are needed as values rather than left
	// to the session.
	database string
	schema   string
}

func open(ctx context.Context, dsn string) (db.Driver, error) {
	native, cfg, err := nativeDSN(dsn)
	if err != nil {
		return nil, err
	}
	conn, err := sql.Open("snowflake", native)
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	// One connection, because a stage a PUT wrote to and the COPY that reads it
	// must be the same session when the stage is a session-scoped one, and
	// because the run is a single logical unit however little Snowflake will
	// guarantee about it.
	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(1)
	if err := conn.PingContext(ctx); err != nil {
		conn.Close()
		return nil, fmt.Errorf("connect: %w", err)
	}

	d := &Driver{db: conn, database: cfg.Database, schema: cfg.Schema}
	if d.database == "" {
		conn.Close()
		return nil, fmt.Errorf("the DSN names no database — add one, as in " +
			"snowflake://user:pass@account/mydb/public?warehouse=compute_wh")
	}
	if d.schema == "" {
		// Snowflake's own default when a connection names only a database.
		d.schema = "PUBLIC"
	}
	// A run that lands on a suspended or missing warehouse fails at the first
	// COPY, several minutes of generation later. Asking now costs one round trip
	// and moves the failure to where it can be read.
	var warehouse sql.NullString
	if err := conn.QueryRowContext(ctx, "SELECT CURRENT_WAREHOUSE()").Scan(&warehouse); err == nil {
		if !warehouse.Valid || warehouse.String == "" {
			conn.Close()
			return nil, fmt.Errorf("no warehouse selected — add one to the DSN, " +
				"as in ?warehouse=compute_wh; loading rows needs compute")
		}
	}
	return d, nil
}

// nativeDSN converts a URL-shaped DSN into the form gosnowflake expects.
//
// Users type `snowflake://user:pass@account/db/schema?warehouse=wh`, because
// that is what Seedora takes for every other engine. gosnowflake wants the same
// string without the scheme, and misparses the account if the scheme is left
// on: it scans backwards for `@` and `/`, and `snowflake://` supplies both.
func nativeDSN(dsn string) (string, *sf.Config, error) {
	native := strings.TrimSpace(dsn)
	if i := strings.Index(native, "://"); i >= 0 {
		native = native[i+3:]
	}
	cfg, err := sf.ParseDSN(native)
	if err != nil {
		return "", nil, fmt.Errorf("parse DSN: %w", err)
	}
	return native, cfg, nil
}

// Name implements db.Driver.
func (d *Driver) Name() string { return "Snowflake" }

// Dialect implements db.Driver.
func (d *Driver) Dialect() ddl.Dialect { return ddl.Snowflake }

// Close implements db.Driver.
func (d *Driver) Close(context.Context) error { return d.db.Close() }

// Begin implements db.Driver.
//
// Nothing is begun. See the package comment: DDL commits implicitly here, so a
// transaction spanning the run cannot exist, and opening one would only make
// the failure modes harder to describe. The returned Tx records what it makes
// permanent so Rollback can say what a failure left behind.
func (d *Driver) Begin(context.Context) (db.Tx, error) {
	return &Tx{d: d}, nil
}

// Introspect reads the catalog in a handful of statements rather than one per
// table: on a schema with a few hundred tables the round trips dominate.
//
// Columns and row counts come from INFORMATION_SCHEMA. Keys do not: Snowflake's
// INFORMATION_SCHEMA has TABLE_CONSTRAINTS but no KEY_COLUMN_USAGE, so it can
// say a primary key exists and not which columns it covers. SHOW PRIMARY KEYS
// and friends are the only catalog surface that names the columns.
func (d *Driver) Introspect(ctx context.Context) (*model.Schema, error) {
	// Enums stays non-nil and empty: Snowflake has no enum type, and a column
	// whose values are constrained is constrained by a CHECK the catalog does
	// not decompose.
	s := &model.Schema{Enums: map[string]model.Values{}}

	byName, err := d.loadColumns(ctx, s)
	if err != nil {
		return nil, err
	}
	if err := d.loadKeys(ctx, byName); err != nil {
		return nil, err
	}
	if err := d.loadCounts(ctx, byName); err != nil {
		return nil, err
	}
	return s, nil
}

func (d *Driver) loadColumns(ctx context.Context, s *model.Schema) (map[string]*model.Table, error) {
	// The database is spelled into the statement rather than bound: an
	// INFORMATION_SCHEMA lives inside one database and has to be named as part
	// of the object, which is a position a bind variable cannot occupy. It comes
	// from the DSN, not from anything a seed touches, and is quoted regardless.
	q := fmt.Sprintf(`
SELECT c.TABLE_NAME, c.COLUMN_NAME, c.DATA_TYPE, c.IS_NULLABLE,
       c.COLUMN_DEFAULT, c.IS_IDENTITY,
       c.CHARACTER_MAXIMUM_LENGTH, c.NUMERIC_PRECISION, c.NUMERIC_SCALE
FROM %[1]s.INFORMATION_SCHEMA.COLUMNS c
JOIN %[1]s.INFORMATION_SCHEMA.TABLES t
  ON t.TABLE_SCHEMA = c.TABLE_SCHEMA AND t.TABLE_NAME = c.TABLE_NAME
WHERE c.TABLE_SCHEMA = ? AND t.TABLE_TYPE = 'BASE TABLE'
ORDER BY c.TABLE_NAME, c.ORDINAL_POSITION`, model.QuoteIdent(d.database))

	rows, err := d.db.QueryContext(ctx, q, d.schema)
	if err != nil {
		return nil, fmt.Errorf("read columns: %w", err)
	}
	defer rows.Close()

	byName := map[string]*model.Table{}
	for rows.Next() {
		var (
			table, name, dataType, nullable, identity string
			def                                       sql.NullString
			maxLen, precision, scale                  sql.NullInt64
		)
		if err := rows.Scan(&table, &name, &dataType, &nullable, &def, &identity,
			&maxLen, &precision, &scale); err != nil {
			return nil, err
		}
		t := byName[table]
		if t == nil {
			t = &model.Table{Schema: d.schema, Name: table}
			byName[table] = t
			s.Tables = append(s.Tables, t)
		}
		c := &model.Column{
			Name:     name,
			Type:     strings.ToLower(dataType),
			Nullable: strings.EqualFold(nullable, "YES"),
			// An identity column fills itself, which is what makes it skippable
			// by default the same way a Postgres serial is.
			HasDefault: def.Valid || strings.EqualFold(identity, "YES"),
			MaxLen:     int(maxLen.Int64),
			Precision:  int(precision.Int64),
			Scale:      int(scale.Int64),
		}
		// Snowflake reports a virtual column's expression nowhere in
		// INFORMATION_SCHEMA.COLUMNS, so Generated is left false: the worst case
		// is a write the server rejects with a message that names the column,
		// which is better than guessing a column is unwritable and silently
		// leaving it out of the plan.
		c.Native = nativeType(c.Type, maxLen, precision, scale)
		t.Columns = append(t.Columns, c)
	}
	return byName, rows.Err()
}

// nativeType re-renders the declaration INFORMATION_SCHEMA took apart, so the
// UI can show what the database actually says rather than a bare type name.
func nativeType(typ string, maxLen, precision, scale sql.NullInt64) string {
	up := strings.ToUpper(typ)
	switch {
	case maxLen.Valid && (strings.Contains(up, "CHAR") || up == "TEXT" || up == "BINARY"):
		return fmt.Sprintf("%s(%d)", up, maxLen.Int64)
	case precision.Valid && (up == "NUMBER" || up == "DECIMAL" || up == "NUMERIC"):
		return fmt.Sprintf("%s(%d,%d)", up, precision.Int64, scale.Int64)
	}
	return up
}

// loadKeys marks primary keys, single-column uniqueness, and foreign keys.
//
// The three SHOW commands are read by column name rather than by position:
// their result sets have gained columns across Snowflake releases, and a
// positional scan would break on the next one. Each is tolerated failing —
// a role without the privilege to see constraints should still be able to
// introspect and seed, just without inferred references.
func (d *Driver) loadKeys(ctx context.Context, byName map[string]*model.Table) error {
	in := fmt.Sprintf("IN SCHEMA %s.%s", model.QuoteIdent(d.database), model.QuoteIdent(d.schema))

	if pk, err := d.showRows(ctx, "SHOW PRIMARY KEYS "+in); err == nil {
		for _, r := range pk {
			t := byName[str(r["table_name"])]
			if t == nil {
				continue
			}
			col := str(r["column_name"])
			t.PrimaryKey = append(t.PrimaryKey, col)
		}
		// A single-column primary key is also a uniqueness constraint on that
		// column; a composite one constrains the tuple and says nothing about
		// either half, so it must not make a generator produce unique values.
		for _, t := range byName {
			if len(t.PrimaryKey) == 1 {
				if c := t.Column(t.PrimaryKey[0]); c != nil {
					c.Unique = true
				}
			}
		}
	}

	if uk, err := d.showRows(ctx, "SHOW UNIQUE KEYS "+in); err == nil {
		// Grouped by constraint first, for the same reason: only a constraint
		// covering exactly one column makes that column unique.
		type key struct{ table, name string }
		cols := map[key][]string{}
		for _, r := range uk {
			k := key{str(r["table_name"]), str(r["constraint_name"])}
			cols[k] = append(cols[k], str(r["column_name"]))
		}
		for k, cs := range cols {
			if len(cs) != 1 {
				continue
			}
			if t := byName[k.table]; t != nil {
				if c := t.Column(cs[0]); c != nil {
					c.Unique = true
				}
			}
		}
	}

	if fk, err := d.showRows(ctx, "SHOW IMPORTED KEYS "+in); err == nil {
		type key struct{ table, name string }
		type part struct{ from, refTable, refColumn string }
		byKey := map[key][]part{}
		for _, r := range fk {
			k := key{str(r["fk_table_name"]), str(r["fk_name"])}
			byKey[k] = append(byKey[k], part{
				from:      str(r["fk_column_name"]),
				refTable:  str(r["pk_table_name"]),
				refColumn: str(r["pk_column_name"]),
			})
		}
		for k, parts := range byKey {
			// A composite key cannot be satisfied one column at a time, so it is
			// left unmarked and the user maps it explicitly.
			if len(parts) != 1 {
				continue
			}
			if t := byName[k.table]; t != nil {
				if c := t.Column(parts[0].from); c != nil {
					c.FK = &model.Ref{Table: parts[0].refTable, Column: parts[0].refColumn}
				}
			}
		}
	}
	return nil
}

// loadCounts reads the row count INFORMATION_SCHEMA already keeps per table.
// Snowflake maintains it as part of a table's metadata, so it is exact and
// costs no warehouse time — unlike COUNT(*), which would scan.
func (d *Driver) loadCounts(ctx context.Context, byName map[string]*model.Table) error {
	q := fmt.Sprintf(`
SELECT TABLE_NAME, ROW_COUNT
FROM %s.INFORMATION_SCHEMA.TABLES
WHERE TABLE_SCHEMA = ? AND TABLE_TYPE = 'BASE TABLE'`, model.QuoteIdent(d.database))

	rows, err := d.db.QueryContext(ctx, q, d.schema)
	if err != nil {
		return fmt.Errorf("read row counts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var n sql.NullInt64
		if err := rows.Scan(&name, &n); err != nil {
			return err
		}
		if t := byName[name]; t != nil {
			t.ExistingRows = n.Int64
		}
	}
	return rows.Err()
}

// History reads whatever a migration tool left behind. Snowflake records no DDL
// history of its own that a catalog query can reach.
func (d *Driver) History(ctx context.Context) ([]model.Migration, error) {
	rows, err := d.db.QueryContext(ctx, fmt.Sprintf(`
SELECT TABLE_NAME FROM %s.INFORMATION_SCHEMA.TABLES
WHERE TABLE_SCHEMA = ? AND TABLE_TYPE = 'BASE TABLE'`, model.QuoteIdent(d.database)), d.schema)
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
		func(table string) bool {
			// Snowflake folds an unquoted identifier to upper case, so a table
			// created as `schema_migrations` is stored as SCHEMA_MIGRATIONS —
			// but a tool that quoted its own DDL kept the lower case. Both
			// spellings are real, and the catalogue only knows the lower one.
			return present[table] || present[strings.ToUpper(table)]
		},
		func(ctx context.Context, query string) ([]map[string]any, error) {
			rows, err := d.db.QueryContext(ctx, query)
			if err != nil {
				return nil, err
			}
			defer rows.Close()
			return scanRows(rows)
		}), nil
}

// showRows runs a SHOW command and returns its rows as field→value maps.
func (d *Driver) showRows(ctx context.Context, stmt string) ([]map[string]any, error) {
	rows, err := d.db.QueryContext(ctx, stmt)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanRows(rows)
}

// scanRows turns a result set into field→value maps without knowing what the
// columns are, which is what reading a SHOW command or another tool's table
// requires.
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
			v := cells[i]
			if b, ok := v.([]byte); ok {
				v = string(b)
			}
			// SHOW output is addressed by lower-case name below; the header
			// casing has changed between releases and this is one place it
			// costs nothing to be insensitive to it.
			row[strings.ToLower(name)] = v
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func str(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case []byte:
		return string(t)
	default:
		return fmt.Sprint(t)
	}
}

// Tx is a Snowflake seeding run. It is not a transaction — see the package
// comment — and it exists to hold the staging machinery and the record of what
// has already been committed.
type Tx struct {
	d    *Driver
	done bool
	// applied is what this run has already made permanent, in order. It is the
	// whole of what Rollback has to offer, and the reason it is kept.
	applied []string
	// run distinguishes this run's staged files from any left behind by an
	// earlier one that died before its COPY purged them.
	run string
}

func (t *Tx) runID() string {
	if t.run == "" {
		t.run = strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return t.run
}

// Exec implements db.Tx.
//
// Every statement the schema editor renders is DDL, and Snowflake commits the
// open transaction before running DDL. So a CREATE or ALTER applied here is
// permanent the moment it succeeds: a later failure in the run leaves it in
// place, and Rollback will say so rather than remove it. The statements are
// still validated and rendered before they run, and they still run in
// dependency order, which is what is left to offer.
func (t *Tx) Exec(ctx context.Context, stmt string) error {
	if _, err := t.d.db.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("exec: %w", err)
	}
	t.applied = append(t.applied, "applied a schema change")
	return nil
}

// Truncate implements db.Tx.
//
// TRUNCATE is DDL on Snowflake, which means two things. It commits whatever
// came before it, and it cannot itself be undone: once this returns, those rows
// are gone whether or not the rest of the run succeeds. Time Travel can still
// recover them with an AT clause for as long as the table's retention period
// lasts, but that is a thing a person does deliberately, not something a driver
// can do on the way out of a failure.
func (t *Tx) Truncate(ctx context.Context, tb *model.Table) error {
	if _, err := t.d.db.ExecContext(ctx, "TRUNCATE TABLE IF EXISTS "+t.qualified(tb)); err != nil {
		return fmt.Errorf("truncate %s: %w", tb.Name, err)
	}
	t.applied = append(t.applied, "emptied "+tb.Name)
	return nil
}

// qualified spells a table the way a statement must, filling in the database
// the DSN selected. Introspect leaves Schema set, so the two-part name a
// model.Table renders is already right; the database qualifies it for a session
// whose current database was changed by a statement the schema editor ran.
func (t *Tx) qualified(tb *model.Table) string {
	if tb.Schema == "" {
		return model.QuoteIdent(t.d.database) + "." + model.QuoteIdent(t.d.schema) +
			"." + model.QuoteIdent(tb.Name)
	}
	return model.QuoteIdent(t.d.database) + "." + tb.Qualified()
}

// Staging is chunked rather than written as one file per table. Snowflake wants
// files in the tens to hundreds of megabytes — one enormous file loads on a
// single thread and wastes the warehouse — and a chunk boundary is also what
// keeps the driver's memory flat, since a stream PUT holds the compressed chunk
// in memory while it uploads.
const (
	chunkRows  = 100_000
	chunkBytes = 64 << 20
)

// Insert implements db.Tx by staging the rows as gzipped CSV and loading them
// with COPY INTO.
//
// This is the shape Snowflake is built for. A row-at-a-time INSERT costs a
// statement compilation and a warehouse dispatch per row; a COPY reads every
// staged file in parallel across the warehouse's threads and is the only path
// that finishes a real seed in a sensible time. The rows are streamed into the
// PUT rather than written to a temp file first, so nothing of the run touches
// the local disk.
//
// The table's own stage (@%table) is used rather than a named one: it exists
// already, needs no CREATE STAGE privilege, and is scoped to exactly the table
// being loaded, so two concurrent runs against different tables cannot see each
// other's files.
//
// A failure partway leaves whatever earlier COPY statements already committed.
// Each COPY here is one statement over one chunk, so the visible result of a
// mid-run failure is a whole number of chunks loaded and the rest missing.
func (t *Tx) Insert(ctx context.Context, tb *model.Table, cols []string, rows db.Source) (int64, error) {
	if len(cols) == 0 {
		return 0, nil
	}
	target := t.qualified(tb)
	stage := "@%" + model.QuoteIdent(tb.Name)

	var (
		written int64
		chunk   int
		w       = newChunkWriter()
		loopErr error
	)

	// flush stages the buffered rows as one file and loads them. Staging and
	// loading per chunk rather than staging everything and loading once is
	// deliberate: the row count Insert returns has to mean rows the database
	// actually holds, and a COPY that has not run yet has loaded nothing.
	flush := func() error {
		if w.rows == 0 {
			return nil
		}
		body, n, err := w.finish()
		if err != nil {
			return fmt.Errorf("stage rows for %s: %w", tb.Name, err)
		}
		name := fmt.Sprintf("seedora_%s_%d.csv.gz", t.runID(), chunk)
		chunk++
		if err := t.put(ctx, stage, name, body); err != nil {
			return fmt.Errorf("stage %s onto %s: %w", name, tb.Name, err)
		}
		if err := t.copyInto(ctx, target, cols, stage, []string{name}); err != nil {
			return fmt.Errorf("load %s into %s: %w", name, tb.Name, err)
		}
		written += n
		t.applied = append(t.applied, fmt.Sprintf("loaded %d rows into %s", n, tb.Name))
		w.reset()
		return nil
	}

	for row := range rows.Rows() {
		if err := w.write(cols, row); err != nil {
			loopErr = fmt.Errorf("encode rows for %s: %w", tb.Name, err)
			break
		}
		if w.rows >= chunkRows || w.raw >= chunkBytes {
			if err := flush(); err != nil {
				loopErr = err
				break
			}
		}
	}
	if loopErr != nil {
		return written, loopErr
	}
	// A generator that fails halfway simply stops yielding, so without this a
	// short write would look like a successful one.
	if err := rows.Err(); err != nil {
		return written, fmt.Errorf("generate rows for %s: %w", tb.Name, err)
	}
	if err := flush(); err != nil {
		return written, err
	}
	// Every file was loaded with PURGE, so a run that got here left the stage
	// as it found it.
	return written, nil
}

// put uploads one staged file from memory. gosnowflake reads the file content
// from the context when the statement is a PUT, which is what lets the rows go
// straight from the generator to the stage without a temp file. The path in the
// command is only where the basename comes from; no such file is opened.
func (t *Tx) put(ctx context.Context, stage, name string, body []byte) error {
	ctx = sf.WithFileStream(ctx, bytes.NewReader(body))
	// AUTO_COMPRESS is off because the payload is gzipped here already, and
	// SOURCE_COMPRESSION says so, so the client neither re-compresses nor
	// mistakes the bytes for text. OVERWRITE is on so a retry of the same chunk
	// replaces its file instead of adding a second one the COPY would load too.
	stmt := fmt.Sprintf(
		"PUT 'file:///tmp/%s' %s AUTO_COMPRESS=FALSE SOURCE_COMPRESSION=GZIP OVERWRITE=TRUE",
		name, stage)
	if _, err := t.d.db.ExecContext(ctx, stmt); err != nil {
		return err
	}
	return nil
}

// copyInto loads exactly the named files. FILES is given rather than letting
// COPY read the whole stage, because a previous run that died between its PUT
// and its COPY leaves a file behind, and a stage-wide COPY would load it as
// well — silently doubling rows that were never asked for. PURGE removes each
// file once its rows are in, so a successful run leaves the stage as it found
// it.
//
// The NULL encoding is the pair that makes an empty string and a NULL
// distinguishable in CSV: every non-NULL value is written quoted, NULL is
// written as an empty unquoted field, and EMPTY_FIELD_AS_NULL applies only to
// unquoted fields. Without that, every empty string in the seed would arrive as
// NULL and violate the first NOT NULL column it met.
func (t *Tx) copyInto(ctx context.Context, target string, cols []string, stage string, files []string) error {
	quoted := make([]string, len(cols))
	for i, c := range cols {
		quoted[i] = model.QuoteIdent(c)
	}
	names := make([]string, len(files))
	for i, f := range files {
		names[i] = "'" + f + "'"
	}
	stmt := fmt.Sprintf(`COPY INTO %s (%s) FROM %s FILES = (%s)
FILE_FORMAT = (TYPE = CSV COMPRESSION = GZIP FIELD_DELIMITER = ','
  FIELD_OPTIONALLY_ENCLOSED_BY = '"' EMPTY_FIELD_AS_NULL = TRUE
  ESCAPE_UNENCLOSED_FIELD = NONE TRIM_SPACE = FALSE)
ON_ERROR = ABORT_STATEMENT PURGE = TRUE FORCE = TRUE`,
		target, strings.Join(quoted, ", "), stage, strings.Join(names, ", "))
	_, err := t.d.db.ExecContext(ctx, stmt)
	return err
}

// ReadKeys implements db.Tx.
//
// On the other engines this reads the run's own uncommitted writes from inside
// the transaction. Here there is no transaction and the rows it returns are
// already committed, which changes nothing about the answer: the parent rows
// this run loaded are visible because the COPY that wrote them has landed.
func (t *Tx) ReadKeys(ctx context.Context, tb *model.Table, col string, limit int) ([]any, error) {
	name := model.QuoteIdent(col)
	q := fmt.Sprintf("SELECT %s FROM %s WHERE %s IS NOT NULL LIMIT %d",
		name, t.qualified(tb), name, limit)
	rows, err := t.d.db.QueryContext(ctx, q)
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

// Commit implements db.Tx. There is nothing to commit: every statement this run
// issued committed itself as it ran. It is still the method that ends the run,
// so it is what stops Rollback from reporting a failure that did not happen.
func (t *Tx) Commit(context.Context) error {
	t.done = true
	return nil
}

// Rollback implements db.Tx, and is the one method here that cannot do what the
// interface asks.
//
// Snowflake commits as it loads. By the time a run fails, the truncates have
// emptied their tables and the COPY statements that already ran have made their
// rows permanent; there is no undo, and returning nil would tell the seeder —
// and through it the user — that the database was restored when it was not. So
// this returns an error naming exactly what was left behind, which is the only
// useful thing left to say.
//
// After Commit it is a no-op, so the seeder can still defer it unconditionally.
func (t *Tx) Rollback(context.Context) error {
	if t.done {
		return nil
	}
	t.done = true
	if len(t.applied) == 0 {
		return nil
	}
	return fmt.Errorf("Snowflake cannot roll this run back — it commits as it loads, "+
		"and the following is already permanent: %s. Re-run the seed to replace it, "+
		"or recover the previous contents with Time Travel (SELECT … AT(OFFSET => …)) "+
		"while the retention period lasts", strings.Join(t.applied, "; "))
}

// chunkWriter builds one staged file: CSV rows, gzipped as they are written, so
// the whole chunk is never held twice.
type chunkWriter struct {
	buf  bytes.Buffer
	gz   *gzip.Writer
	line []byte
	rows int64
	// raw is the uncompressed size, which is what the chunk limit is about:
	// it bounds the work one COPY does, and the compressed size does not.
	raw int
}

func newChunkWriter() *chunkWriter {
	w := &chunkWriter{}
	w.gz = gzip.NewWriter(&w.buf)
	return w
}

func (w *chunkWriter) write(cols []string, row map[string]any) error {
	w.line = w.line[:0]
	for i, c := range cols {
		if i > 0 {
			w.line = append(w.line, ',')
		}
		// A key the plan did not name is absent from the map, and absent means
		// NULL — which is how a column the plan skips takes its own default.
		v, ok := row[c]
		if !ok || v == nil {
			continue // empty unquoted field: NULL under EMPTY_FIELD_AS_NULL
		}
		w.line = appendField(w.line, v)
	}
	w.line = append(w.line, '\n')
	w.raw += len(w.line)
	w.rows++
	_, err := w.gz.Write(w.line)
	return err
}

// finish closes the gzip stream and hands back the bytes to PUT.
func (w *chunkWriter) finish() ([]byte, int64, error) {
	if err := w.gz.Close(); err != nil {
		return nil, 0, err
	}
	return w.buf.Bytes(), w.rows, nil
}

func (w *chunkWriter) reset() {
	w.buf.Reset()
	w.gz.Reset(&w.buf)
	w.rows = 0
	w.raw = 0
}

// appendField writes one value as a quoted CSV field. Everything non-NULL is
// quoted, including numbers: quoting costs two bytes and is what makes the
// empty string distinguishable from NULL, which is the whole basis of the file
// format the COPY is told to expect.
func appendField(dst []byte, v any) []byte {
	dst = append(dst, '"')
	switch x := v.(type) {
	case string:
		dst = appendQuoted(dst, x)
	case []byte:
		// A BINARY column is loaded from its hex text, which is what Snowflake
		// reads a string into a BINARY as by default.
		dst = append(dst, hex.EncodeToString(x)...)
	case time.Time:
		// UTC and RFC 3339, which every Snowflake date, time, and timestamp
		// type parses from a string without a format having to be declared.
		dst = x.UTC().AppendFormat(dst, time.RFC3339Nano)
	case bool:
		dst = strconv.AppendBool(dst, x)
	case int:
		dst = strconv.AppendInt(dst, int64(x), 10)
	case int8:
		dst = strconv.AppendInt(dst, int64(x), 10)
	case int16:
		dst = strconv.AppendInt(dst, int64(x), 10)
	case int32:
		dst = strconv.AppendInt(dst, int64(x), 10)
	case int64:
		dst = strconv.AppendInt(dst, x, 10)
	case uint:
		dst = strconv.AppendUint(dst, uint64(x), 10)
	case uint8:
		dst = strconv.AppendUint(dst, uint64(x), 10)
	case uint16:
		dst = strconv.AppendUint(dst, uint64(x), 10)
	case uint32:
		dst = strconv.AppendUint(dst, uint64(x), 10)
	case uint64:
		dst = strconv.AppendUint(dst, x, 10)
	case float32:
		dst = strconv.AppendFloat(dst, float64(x), 'g', -1, 32)
	case float64:
		// NaN and the infinities have no CSV spelling a numeric column reads
		// back as themselves. Writing the word is what makes the COPY reject
		// the row and name the column, which beats a silent zero.
		if math.IsNaN(x) || math.IsInf(x, 0) {
			dst = append(dst, "NaN"...)
			break
		}
		dst = strconv.AppendFloat(dst, x, 'g', -1, 64)
	case map[string]any, []any:
		// A VARIANT, OBJECT, or ARRAY column is generated as a Go value and
		// loaded from its JSON text.
		b, err := json.Marshal(x)
		if err == nil {
			dst = appendQuoted(dst, string(b))
		}
	default:
		dst = appendQuoted(dst, fmt.Sprint(x))
	}
	return append(dst, '"')
}

// appendQuoted writes the body of a quoted CSV field, doubling any quote in it.
func appendQuoted(dst []byte, s string) []byte {
	for i := 0; i < len(s); i++ {
		if s[i] == '"' {
			dst = append(dst, '"')
		}
		dst = append(dst, s[i])
	}
	return dst
}
