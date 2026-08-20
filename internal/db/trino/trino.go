// Package trino implements the Seedora driver for Trino.
//
// Trino is a query engine rather than a database: what a statement can do
// depends on the connector behind the catalog, and so does what it can undo.
// Iceberg and Delta Lake commit a write atomically and support DELETE; Hive
// with plain files does not, and a handful of connectors are read-only
// altogether. Seedora's promise that a run is one transaction and a failure
// leaves the database exactly as it was cannot be made here at all: the Go
// client refuses Begin outright (trino: operation not supported), so every
// statement this driver sends is its own committed unit no matter which
// connector receives it. Begin returns an object because the interface asks for
// one, and Rollback says what it failed to undo rather than returning nil.
//
// The bulk path is a multi-row INSERT … VALUES. There is no load protocol on
// the wire, and the values are rendered as literals rather than sent as
// parameters: the client serialises a parameter into a literal anyway, refuses
// float64 while doing it, and — with explicit prepare, which is the default —
// puts the whole statement in an HTTP header, which a wide VALUES list
// overflows. Rendering the statement here avoids all three.
package trino

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	_ "github.com/trinodb/trino-go-client/trino"

	"github.com/bakhod1r/seedora/internal/db"
	"github.com/bakhod1r/seedora/internal/ddl"
	"github.com/bakhod1r/seedora/internal/model"
)

func init() {
	db.Register(open, "trino", "presto")
}

// Driver is a connected Trino coordinator.
type Driver struct {
	db *sql.DB
	// schema is the session schema. Trino's information_schema is per catalog
	// and carries every schema in it, so the schema has to be named in the
	// query rather than assumed.
	schema string
}

func open(ctx context.Context, dsn string) (db.Driver, error) {
	native, err := nativeDSN(dsn)
	if err != nil {
		return nil, err
	}
	conn, err := sql.Open("trino", native)
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	// One connection: a seeding run is a sequence, and spreading it over a pool
	// only spreads the session properties with it.
	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(1)

	d := &Driver{db: conn}
	// Trino has no ping; the first query is the connection test.
	if err := conn.QueryRowContext(ctx, "SELECT current_schema").Scan(&d.schema); err != nil {
		conn.Close()
		return nil, fmt.Errorf("connect: %w", err)
	}
	if d.schema == "" {
		conn.Close()
		return nil, fmt.Errorf("the DSN names no schema — add one, as in " +
			"trino://user@localhost:8080?catalog=hive&schema=myapp_dev")
	}
	return d, nil
}

// nativeDSN converts a `trino://` DSN into the http(s) URL the client expects,
// which is the only form it takes. Explicit prepare is turned off along the
// way: it sends the statement in a request header, and the INSERTs this driver
// writes are far larger than a header may be.
func nativeDSN(dsn string) (string, error) {
	dsn = strings.TrimSpace(dsn)
	for _, alias := range []string{"trino://", "presto://"} {
		if rest, ok := strings.CutPrefix(dsn, alias); ok {
			dsn = "http://" + rest
		}
	}
	u, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("parse DSN: %w", err)
	}
	q := u.Query()
	// A coordinator on 443 is behind TLS in every deployment that puts it
	// there, and `trino://…:443` is what people paste.
	if u.Scheme == "http" && (u.Port() == "443" || q.Get("SSL") == "true") {
		u.Scheme = "https"
	}
	q.Del("SSL")
	if q.Get("explicitPrepare") == "" {
		q.Set("explicitPrepare", "false")
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// Name implements db.Driver.
func (d *Driver) Name() string { return "Trino" }

// Dialect implements db.Driver.
func (d *Driver) Dialect() ddl.Dialect { return ddl.Trino }

// Close implements db.Driver.
func (d *Driver) Close(context.Context) error { return d.db.Close() }

// Begin implements db.Driver. Nothing is begun — see the package comment.
func (d *Driver) Begin(context.Context) (db.Tx, error) {
	return &Tx{db: d.db}, nil
}

// Introspect reads information_schema, which is all Trino exposes: there is no
// catalog underneath it to go to instead, because the catalog belongs to
// whatever system the connector is pointed at.
func (d *Driver) Introspect(ctx context.Context) (*model.Schema, error) {
	s := &model.Schema{Enums: map[string]model.Values{}}
	if err := d.loadColumns(ctx, s); err != nil {
		return nil, err
	}
	if err := d.loadCounts(ctx, s); err != nil {
		return nil, err
	}
	return s, nil
}

// loadColumns reads every table in the session schema and its columns.
//
// Nothing else is available to read. Trino's information_schema has no
// table_constraints, no key_column_usage, and no index catalog: primary keys,
// unique constraints and foreign keys simply do not exist at this layer, even
// when the underlying system has them. Every relationship in a seeded schema
// therefore has to be mapped by hand here, which is worth knowing before
// pointing Seedora at a Trino catalog rather than at the database behind it.
func (d *Driver) loadColumns(ctx context.Context, s *model.Schema) error {
	q := fmt.Sprintf(`
SELECT c.table_name, c.column_name, c.data_type, c.is_nullable, c.column_default
FROM information_schema.columns c
JOIN information_schema.tables t
  ON t.table_schema = c.table_schema AND t.table_name = c.table_name
WHERE c.table_schema = %s AND t.table_type != 'VIEW'
ORDER BY c.table_name, c.ordinal_position`, quoteLiteral(d.schema))

	rows, err := d.db.QueryContext(ctx, q)
	if err != nil {
		return fmt.Errorf("read columns: %w", err)
	}
	defer rows.Close()

	byName := map[string]*model.Table{}
	for rows.Next() {
		var table, name, dataType, nullable string
		var def sql.NullString
		if err := rows.Scan(&table, &name, &dataType, &nullable, &def); err != nil {
			return err
		}
		t := byName[table]
		if t == nil {
			t = &model.Table{Name: table}
			byName[table] = t
			s.Tables = append(s.Tables, t)
		}
		c := &model.Column{
			Name:       name,
			Type:       base(dataType),
			Native:     dataType,
			Nullable:   strings.EqualFold(nullable, "YES"),
			HasDefault: def.Valid,
		}
		c.MaxLen, c.Precision, c.Scale = decorations(dataType)
		t.Columns = append(t.Columns, c)
	}
	return rows.Err()
}

// countChunk is how many tables are counted in one statement. Trino charges per
// query, not per row scanned, so folding the counts into a UNION ALL turns a
// catalog of two hundred tables from two hundred queries into two.
const countChunk = 100

// loadCounts counts the rows in every table.
//
// This is COUNT(*) and it is exact, because there is nothing cheaper to ask.
// Postgres has a planner estimate and ClickHouse tracks the number itself;
// Trino has neither, and SHOW STATS answers only for connectors that collect
// statistics and answers NULL for the rest. The number is what the truncate
// confirmation is built on, so an unreliable one is worse than a slow one.
func (d *Driver) loadCounts(ctx context.Context, s *model.Schema) error {
	counts := map[string]int64{}
	for start := 0; start < len(s.Tables); start += countChunk {
		end := min(start+countChunk, len(s.Tables))
		parts := make([]string, 0, end-start)
		for _, t := range s.Tables[start:end] {
			parts = append(parts, fmt.Sprintf("SELECT %s AS t, count(*) AS n FROM %s",
				quoteLiteral(t.Name), t.Qualified()))
		}
		rows, err := d.db.QueryContext(ctx, strings.Join(parts, " UNION ALL "))
		if err != nil {
			return fmt.Errorf("count rows: %w", err)
		}
		for rows.Next() {
			var name string
			var n int64
			if err := rows.Scan(&name, &n); err != nil {
				rows.Close()
				return err
			}
			counts[name] = n
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
	}
	for _, t := range s.Tables {
		t.ExistingRows = counts[t.Name]
	}
	return nil
}

// History reads whatever a migration tool left behind. A migration tool would
// not normally be pointed at Trino at all, but a catalog over a real database
// shows that database's bookkeeping table, and reading it costs one query.
func (d *Driver) History(ctx context.Context) ([]model.Migration, error) {
	rows, err := d.db.QueryContext(ctx, fmt.Sprintf(
		"SELECT table_name FROM information_schema.tables WHERE table_schema = %s",
		quoteLiteral(d.schema)))
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

// Tx is a Trino seeding "transaction" — a record of writes that have already
// happened, since the client cannot open a real one.
type Tx struct {
	db        *sql.DB
	done      bool
	truncated []string
	written   int64
}

// Truncate implements db.Tx with DELETE rather than TRUNCATE TABLE.
//
// TRUNCATE is not SQL Trino accepts at all; DELETE without a predicate is, on
// the connectors that support writes — Iceberg and Delta Lake do it as a
// metadata-only operation, Hive can only drop whole partitions and rejects it
// otherwise, and a read-only connector rejects it with everything else. It
// commits on its own the moment it succeeds, so a run that dies between this
// and its inserts leaves the table empty.
func (t *Tx) Truncate(ctx context.Context, tb *model.Table) error {
	if _, err := t.db.ExecContext(ctx, "DELETE FROM "+tb.Qualified()); err != nil {
		return fmt.Errorf("truncate %s: %w — the connector behind this catalog may not "+
			"support deletes", tb.Name, err)
	}
	t.truncated = append(t.truncated, tb.Name)
	return nil
}

// Exec implements db.Tx. The statement is applied for good the moment it runs.
func (t *Tx) Exec(ctx context.Context, stmt string) error {
	if _, err := t.db.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("exec: %w", err)
	}
	return nil
}

// insertRows and insertBytes bound one INSERT. Trino refuses a statement over
// query.max-length — a million characters by default — so the byte budget is
// the one that actually decides, and the row count is there to keep a narrow
// table from building a statement with a hundred thousand tuples in it.
const (
	insertRows  = 1000
	insertBytes = 400_000
)

// Insert implements db.Tx with multi-row INSERT … VALUES statements.
//
// Every statement that succeeds is committed by itself. A failure on the
// eleventh leaves the first ten in the table, and there is nothing to roll
// back: on Iceberg or Delta each is a snapshot of its own, and on Hive the
// files are already written.
func (t *Tx) Insert(ctx context.Context, tb *model.Table, cols []string, rows db.Source) (int64, error) {
	if len(cols) == 0 {
		return 0, nil
	}
	quoted := make([]string, len(cols))
	targets := make([]*model.Column, len(cols))
	for i, c := range cols {
		quoted[i] = model.QuoteIdent(c)
		targets[i] = tb.Column(c)
	}
	head := "INSERT INTO " + tb.Qualified() + " (" + strings.Join(quoted, ", ") + ") VALUES "

	var (
		sb      strings.Builder
		tuples  int
		written int64
	)
	flush := func() error {
		if tuples == 0 {
			return nil
		}
		if _, err := t.db.ExecContext(ctx, head+sb.String()); err != nil {
			return fmt.Errorf("insert into %s: %w", tb.Name, err)
		}
		written += int64(tuples)
		t.written += int64(tuples)
		sb.Reset()
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
			// the plan skips reaches Trino at all: there is no DEFAULT keyword
			// in a VALUES list here that every connector honours.
			sb.WriteString(literal(targets[i], row[c]))
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

// ReadKeys implements db.Tx.
func (t *Tx) ReadKeys(ctx context.Context, tb *model.Table, col string, limit int) ([]any, error) {
	name := model.QuoteIdent(col)
	q := fmt.Sprintf("SELECT %s FROM %s WHERE %s IS NOT NULL LIMIT %s",
		name, tb.Qualified(), name, strconv.Itoa(limit))
	rows, err := t.db.QueryContext(ctx, q)
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
		if b, ok := v.([]byte); ok {
			v = string(b)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// Commit implements db.Tx. Every statement committed itself as it ran.
func (t *Tx) Commit(context.Context) error {
	t.done = true
	return nil
}

// Atomic implements db.Atomic: this engine commits as it writes, so a rollback
// undoes nothing and --dry-run must not write.
func (t *Tx) Atomic() bool { return false }

// Rollback implements db.Tx, and cannot do what its name says. Returning nil
// would report a clean unwind of a database that still holds every row written
// and is still missing every row deleted, so it names what is left instead. A
// run that changed nothing reports success honestly, which is also what keeps
// the seeder's deferred Rollback quiet after a Commit.
func (t *Tx) Rollback(context.Context) error {
	if t.done {
		return nil
	}
	t.done = true
	if t.written == 0 && len(t.truncated) == 0 {
		return nil
	}
	var what []string
	if t.written > 0 {
		what = append(what, fmt.Sprintf("%d rows are inserted", t.written))
	}
	if len(t.truncated) > 0 {
		what = append(what, fmt.Sprintf("%s was emptied", strings.Join(t.truncated, ", ")))
	}
	return fmt.Errorf("this run cannot be rolled back: Trino commits as it writes. "+
		"Already permanent: %s. The catalog is not in the state it was before the run",
		strings.Join(what, " and "))
}

// literal renders one generated value as Trino SQL.
//
// The column is passed in because the same Go value spells differently
// depending on where it lands: a time is DATE or TIMESTAMP, a string is UUID or
// JSON or VARCHAR, and Trino applies no implicit conversion between them.
func literal(c *model.Column, v any) string {
	typ := ""
	if c != nil {
		typ = c.Type
	}
	switch x := v.(type) {
	case nil:
		return "NULL"
	case bool:
		if x {
			return "TRUE"
		}
		return "FALSE"
	case int:
		return strconv.Itoa(x)
	case int8, int16, int32, int64:
		return fmt.Sprintf("%d", x)
	case uint, uint8, uint16, uint32, uint64:
		return fmt.Sprintf("%d", x)
	case float32:
		return "REAL " + quoteLiteral(strconv.FormatFloat(float64(x), 'g', -1, 32))
	case float64:
		// A bare 1.5 is DECIMAL in Trino, not DOUBLE, and the two do not
		// silently interchange in an INSERT. The typed literal says which.
		return "DOUBLE " + quoteLiteral(strconv.FormatFloat(x, 'g', -1, 64))
	case []byte:
		return "X'" + hex(x) + "'"
	case time.Time:
		switch {
		case strings.HasPrefix(typ, "date"):
			return "DATE " + quoteLiteral(x.UTC().Format("2006-01-02"))
		case strings.HasPrefix(typ, "time") && !strings.HasPrefix(typ, "timestamp"):
			return "TIME " + quoteLiteral(x.UTC().Format("15:04:05.000"))
		default:
			return "TIMESTAMP " + quoteLiteral(x.UTC().Format("2006-01-02 15:04:05.000"))
		}
	case map[string]any, []any:
		b, err := json.Marshal(x)
		if err != nil {
			return "NULL"
		}
		if typ == "json" {
			return "JSON " + quoteLiteral(string(b))
		}
		return quoteLiteral(string(b))
	case string:
		switch typ {
		case "uuid":
			return "UUID " + quoteLiteral(x)
		case "json":
			return "JSON " + quoteLiteral(x)
		}
		return quoteLiteral(x)
	default:
		return quoteLiteral(fmt.Sprint(x))
	}
}

// quoteLiteral renders a string as a SQL literal. A single quote is doubled,
// which is the only escape a Trino string literal has.
func quoteLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

const hexDigits = "0123456789abcdef"

func hex(b []byte) string {
	out := make([]byte, 0, len(b)*2)
	for _, c := range b {
		out = append(out, hexDigits[c>>4], hexDigits[c&0x0f])
	}
	return string(out)
}

// base strips the parameters off a rendered type, so "varchar(255)" classifies
// as the varchar it is. The full type is kept in Column.Native.
func base(dataType string) string {
	if i := strings.IndexByte(dataType, '('); i >= 0 {
		return strings.TrimSpace(dataType[:i])
	}
	return dataType
}

// decorations pulls the length, precision, and scale out of a rendered type
// such as "varchar(255)" or "decimal(10,2)".
func decorations(dataType string) (maxLen, precision, scale int) {
	open := strings.IndexByte(dataType, '(')
	if open < 0 || !strings.HasSuffix(dataType, ")") {
		return 0, 0, 0
	}
	head := strings.ToLower(strings.TrimSpace(dataType[:open]))
	parts := strings.Split(dataType[open+1:len(dataType)-1], ",")
	first := atoi(strings.TrimSpace(parts[0]))
	switch {
	case strings.Contains(head, "char"):
		return first, 0, 0
	case strings.Contains(head, "decimal"):
		if len(parts) > 1 {
			return 0, first, atoi(strings.TrimSpace(parts[1]))
		}
		return 0, first, 0
	}
	// timestamp(3) and time(6) carry a precision that means digits of a second,
	// which is not the precision anything here is asking about.
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
