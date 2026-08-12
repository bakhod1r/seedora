// Package clickhouse implements the Seedora driver for ClickHouse.
//
// One thing about ClickHouse shapes this driver and has to be said before
// anything else: it has no transactions. There is no BEGIN outside an
// experimental per-partition mode nobody runs in production, so the promise the
// rest of Seedora makes — a run is one transaction, and a failure leaves the
// database exactly as it was — cannot be kept here. This driver does not fake
// it. Begin returns a transaction object because the interface asks for one,
// every write it accepts lands immediately and permanently, and Rollback
// returns an error saying exactly what is still in the database rather than nil
// as though it had undone something. That is the same concession the MySQL
// driver makes for DDL, made louder because here it covers the rows too.
//
// The bulk path is the native block protocol: PrepareBatch buffers rows
// column-wise on the client and ships them as blocks, which is the fastest
// thing the server can be given and the reason ClickHouse is worth a driver at
// all. It is not atomic — each block that reaches the server is visible as soon
// as it is written, so a failure halfway leaves the blocks already sent.
package clickhouse

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"github.com/ClickHouse/clickhouse-go/v2"
	chdriver "github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/bakhod1r/seedora/internal/db"
	"github.com/bakhod1r/seedora/internal/ddl"
	"github.com/bakhod1r/seedora/internal/model"
)

func init() {
	db.Register(open, "clickhouse", "ch")
}

// Driver is a connected ClickHouse database.
type Driver struct {
	conn chdriver.Conn
}

func open(ctx context.Context, dsn string) (db.Driver, error) {
	if rest, ok := strings.CutPrefix(dsn, "ch://"); ok {
		dsn = "clickhouse://" + rest
	}
	opts, err := clickhouse.ParseDSN(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse DSN: %w", err)
	}
	conn, err := clickhouse.Open(opts)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	// clickhouse.Open is lazy, so without this the first error a user sees for a
	// wrong host is a failed introspection rather than a failed connection.
	if err := conn.Ping(ctx); err != nil {
		conn.Close()
		return nil, fmt.Errorf("connect: %w", err)
	}
	return &Driver{conn: conn}, nil
}

// Name implements db.Driver.
func (d *Driver) Name() string { return "ClickHouse" }

// Dialect implements db.Driver.
func (d *Driver) Dialect() ddl.Dialect { return ddl.ClickHouse }

// Close implements db.Driver.
func (d *Driver) Close(context.Context) error { return d.conn.Close() }

// Begin implements db.Driver. There is nothing to begin: ClickHouse has no
// transactions, and the object returned writes straight through. See the
// package comment, and Rollback for what that costs.
func (d *Driver) Begin(context.Context) (db.Tx, error) {
	return &Tx{conn: d.conn}, nil
}

// Introspect reads system.tables and system.columns — the catalog proper.
// ClickHouse also serves an information_schema view over the same data, but it
// flattens the type back to an ANSI name and loses exactly what seeding needs:
// whether a column is Nullable, what an Enum's labels are, and whether the
// column is part of the sorting key.
func (d *Driver) Introspect(ctx context.Context) (*model.Schema, error) {
	s := &model.Schema{Enums: map[string]model.Values{}}
	byName, err := d.loadTables(ctx, s)
	if err != nil {
		return nil, err
	}
	if err := d.loadColumns(ctx, s, byName); err != nil {
		return nil, err
	}
	return s, nil
}

// loadTables reads the tables in the connected database. Views, materialised
// views and dictionaries are excluded: none of them is a thing you insert seed
// rows into directly. Only the connected database is read, which is what makes
// an unqualified table name in a statement resolve to the table introspected.
func (d *Driver) loadTables(ctx context.Context, s *model.Schema) (map[string]*model.Table, error) {
	const q = `
SELECT name, ifNull(total_rows, 0) AS rows
FROM system.tables
WHERE database = currentDatabase()
  AND engine NOT LIKE '%View'
  AND engine NOT IN ('Dictionary', 'MaterializedView')
  AND NOT is_temporary
ORDER BY name`

	rows, err := d.conn.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("read tables: %w", err)
	}
	defer rows.Close()

	byName := map[string]*model.Table{}
	for rows.Next() {
		var name string
		var n uint64
		if err := rows.Scan(&name, &n); err != nil {
			return nil, err
		}
		// total_rows is exact for the MergeTree family and null for engines that
		// do not track it — Log, Memory, and every table function. Zero there
		// means "unknown", not "empty", which is the one place this number is
		// weaker than the estimate Postgres gives.
		t := &model.Table{Name: name, ExistingRows: int64(n)}
		byName[name] = t
		s.Tables = append(s.Tables, t)
	}
	return byName, rows.Err()
}

func (d *Driver) loadColumns(ctx context.Context, s *model.Schema, byName map[string]*model.Table) error {
	const q = `
SELECT table, name, type, default_kind, is_in_primary_key
FROM system.columns
WHERE database = currentDatabase()
ORDER BY table, position`

	rows, err := d.conn.Query(ctx, q)
	if err != nil {
		return fmt.Errorf("read columns: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var table, name, typ, defaultKind string
		var inPK uint8
		if err := rows.Scan(&table, &name, &typ, &defaultKind, &inPK); err != nil {
			return err
		}
		t := byName[table]
		if t == nil {
			continue
		}
		base, nullable := unwrap(typ)
		c := &model.Column{
			Name:     name,
			Type:     normalize(base),
			Native:   typ,
			Nullable: nullable,
			// DEFAULT and the two computed kinds all mean the column fills
			// itself if the insert leaves it out.
			HasDefault: defaultKind != "",
			// MATERIALIZED and ALIAS columns are computed and refuse a written
			// value; DEFAULT is an ordinary column with a default.
			Generated: defaultKind == "MATERIALIZED" || defaultKind == "ALIAS",
		}
		c.MaxLen, c.Precision, c.Scale = decorations(base)
		if labels := enumLabels(base); labels != nil {
			// An enum is declared inline rather than as a named type, so the
			// type is synthesised from where it appears.
			typeName := table + "_" + name
			s.Enums[typeName] = labels
			c.EnumType = typeName
		}
		if inPK == 1 {
			// ClickHouse's "primary key" is the sorting key: it orders the parts
			// and does not constrain anything. It is still the best answer to
			// "which columns identify a row", which is what the planner uses it
			// for — but nothing is marked Unique off the back of it, because
			// ClickHouse enforces no uniqueness anywhere. Nor is there any
			// foreign key to read: the engine has none.
			t.PrimaryKey = append(t.PrimaryKey, name)
		}
		t.Columns = append(t.Columns, c)
	}
	return rows.Err()
}

// History reads whatever a migration tool left behind. ClickHouse records no
// DDL history of its own that a query can reach.
func (d *Driver) History(ctx context.Context) ([]model.Migration, error) {
	rows, err := d.conn.Query(ctx,
		"SELECT name FROM system.tables WHERE database = currentDatabase()")
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
			rows, err := d.conn.Query(ctx, query)
			if err != nil {
				return nil, err
			}
			defer rows.Close()
			return scanRows(rows)
		}), nil
}

// scanRows turns a result set into field→value maps without knowing what the
// columns are, which is what reading another tool's table requires.
func scanRows(rows chdriver.Rows) ([]map[string]any, error) {
	cols := rows.Columns()
	types := rows.ColumnTypes()
	var out []map[string]any
	for rows.Next() {
		// ClickHouse is strictly typed on the way out too: a destination has to
		// match the column, so the scan targets are built from the column types
		// rather than from `any`.
		into := make([]any, len(cols))
		for i := range into {
			into[i] = reflectNew(types[i])
		}
		if err := rows.Scan(into...); err != nil {
			return nil, err
		}
		row := make(map[string]any, len(cols))
		for i, name := range cols {
			row[name] = deref(into[i])
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// reflectNew makes a scan destination for a column of the given type. The
// driver refuses to scan into `any`, so the destination has to be built from
// what the column says it is.
func reflectNew(ct chdriver.ColumnType) any {
	return reflect.New(ct.ScanType()).Interface()
}

// deref unwraps a scan destination back into the value it holds, and turns the
// types no consumer of this expects to see into ones they do.
func deref(p any) any {
	v := reflect.ValueOf(p).Elem()
	// A Nullable column scans into a pointer, and a nil one is a NULL.
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return nil
		}
		v = v.Elem()
	}
	return v.Interface()
}

// Tx is a ClickHouse seeding "transaction" — a bookkeeper for writes that have
// already happened, since the engine offers nothing to hold them open in.
type Tx struct {
	conn chdriver.Conn
	done bool
	// truncated and written record what has already landed, so Rollback can say
	// what it is failing to undo rather than just that it failed.
	truncated []string
	written   int64
}

// Truncate implements db.Tx. This is the point where the missing transaction
// first bites: TRUNCATE TABLE on ClickHouse drops the parts immediately, and
// nothing that happens afterwards puts them back. A run that dies after this
// and before its inserts leaves the table empty.
func (t *Tx) Truncate(ctx context.Context, tb *model.Table) error {
	if err := t.conn.Exec(ctx, "TRUNCATE TABLE "+tb.Qualified()); err != nil {
		return fmt.Errorf("truncate %s: %w", tb.Name, err)
	}
	t.truncated = append(t.truncated, tb.Name)
	return nil
}

// Exec implements db.Tx. Unlike Postgres and SQLite, the statement is applied
// for good the moment it runs: the schema editor's changes cannot be taken back
// here, the same way MySQL's DDL cannot.
func (t *Tx) Exec(ctx context.Context, stmt string) error {
	if err := t.conn.Exec(ctx, stmt); err != nil {
		return fmt.Errorf("exec: %w", err)
	}
	return nil
}

// batchRows is how many rows are buffered client-side before a block goes to
// the server. ClickHouse wants large blocks — the cost is per block, not per
// row — and this is a size that keeps memory bounded on a wide table while
// still being one the server treats as a bulk write rather than a trickle.
const batchRows = 100_000

// Insert implements db.Tx using the native batch protocol: rows are buffered
// column-wise on the client and flushed as blocks, which is the cheapest thing
// ClickHouse can be asked to do.
//
// Each flushed block is visible to every reader the instant the server accepts
// it. If the run fails after the third block, three blocks of rows are in the
// table and stay there — there is no statement to roll back and no way to
// identify the rows afterwards except by having generated them. Abort stops the
// insert; it does not retract what has already been sent.
func (t *Tx) Insert(ctx context.Context, tb *model.Table, cols []string, rows db.Source) (int64, error) {
	if len(cols) == 0 {
		return 0, nil
	}
	quoted := make([]string, len(cols))
	for i, c := range cols {
		quoted[i] = model.QuoteIdent(c)
	}
	stmt := "INSERT INTO " + tb.Qualified() + " (" + strings.Join(quoted, ", ") + ")"

	batch, err := t.conn.PrepareBatch(ctx, stmt)
	if err != nil {
		return 0, fmt.Errorf("prepare batch for %s: %w", tb.Name, err)
	}
	defer batch.Close()

	buf := make([]any, len(cols))
	var written, buffered int64
	var loopErr error
	for row := range rows.Rows() {
		for i, c := range cols {
			// A column the plan skips is absent from the map. On Postgres that
			// reaches COPY as NULL and the column default fills in; there is no
			// such thing in a native block, and a non-Nullable column takes the
			// zero value for its type instead. A column with a DEFAULT should be
			// left out of the column list rather than sent empty.
			buf[i] = value(row[c])
		}
		if err := batch.Append(buf...); err != nil {
			loopErr = fmt.Errorf("insert into %s: %w", tb.Name, err)
			break
		}
		buffered++
		if buffered == batchRows {
			if err := batch.Flush(); err != nil {
				loopErr = fmt.Errorf("insert into %s: %w", tb.Name, err)
				break
			}
			written += buffered
			t.written += buffered
			buffered = 0
		}
	}
	if loopErr != nil {
		_ = batch.Abort()
		return written, loopErr
	}
	// The generator stops yielding when it fails, so a short stream is
	// indistinguishable from a complete one without this.
	if err := rows.Err(); err != nil {
		_ = batch.Abort()
		return written, fmt.Errorf("generate rows for %s: %w", tb.Name, err)
	}
	if err := batch.Send(); err != nil {
		return written, fmt.Errorf("insert into %s: %w", tb.Name, err)
	}
	written += buffered
	t.written += buffered
	return written, nil
}

// ReadKeys implements db.Tx. Every row this reads has already been committed by
// definition, so unlike the transactional drivers there is no question of
// whether the connection can see its own writes.
func (t *Tx) ReadKeys(ctx context.Context, tb *model.Table, col string, limit int) ([]any, error) {
	name := model.QuoteIdent(col)
	q := fmt.Sprintf("SELECT %s FROM %s WHERE %s IS NOT NULL LIMIT %d",
		name, tb.Qualified(), name, limit)
	rows, err := t.conn.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("read keys from %s.%s: %w", tb.Name, col, err)
	}
	defer rows.Close()

	types := rows.ColumnTypes()
	var out []any
	for rows.Next() {
		into := reflectNew(types[0])
		if err := rows.Scan(into); err != nil {
			return nil, err
		}
		out = append(out, deref(into))
	}
	return out, rows.Err()
}

// Commit implements db.Tx. Everything it would commit is already in the
// database; this only closes the bookkeeping so a deferred Rollback stays
// silent.
func (t *Tx) Commit(context.Context) error {
	t.done = true
	return nil
}

// Rollback implements db.Tx, and cannot do what its name says.
//
// Returning nil here would be a lie the caller has no way to detect: it would
// report a clean unwind of a database that still holds every row written and is
// still missing every row truncated. So it returns an error naming what is
// left. A run that wrote nothing has nothing to undo and reports success
// honestly, which is also what makes the seeder's unconditional deferred
// Rollback quiet on the path where Commit already ran.
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
	return fmt.Errorf("ClickHouse cannot roll back: %s, and this failure does not undo it — "+
		"the database is not in the state it was before the run", strings.Join(what, " and "))
}

// value adapts the few generated types the batch protocol will not take as they
// are.
func value(v any) any {
	switch x := v.(type) {
	case map[string]any, []any:
		// A JSON or String column is generated as a Go value; the column has no
		// encoder for one and wants the text.
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

// unwrap strips the type wrappers that say something about storage rather than
// about the value: Nullable is the one that changes what may be written, and
// LowCardinality is a dictionary encoding that changes nothing at all.
func unwrap(typ string) (base string, nullable bool) {
	base = typ
	for {
		switch {
		case strings.HasPrefix(base, "Nullable(") && strings.HasSuffix(base, ")"):
			base, nullable = base[len("Nullable("):len(base)-1], true
		case strings.HasPrefix(base, "LowCardinality(") && strings.HasSuffix(base, ")"):
			base = base[len("LowCardinality(") : len(base)-1]
		default:
			return base, nullable
		}
	}
}

// normalize maps a ClickHouse type onto the conventional SQL name for the same
// thing. Inference classifies on the type name and knows the ANSI spellings;
// `String` and `Float64` would otherwise both come out unclassified, which
// costs the user a manual mapping on every text and float column in the schema.
// The verbatim type is kept in Column.Native, so nothing is lost.
func normalize(base string) string {
	head := base
	if i := strings.IndexByte(head, '('); i >= 0 {
		head = head[:i]
	}
	switch head {
	case "String", "FixedString", "IPv4", "IPv6":
		return "text"
	case "UUID":
		return "uuid"
	case "Bool":
		return "boolean"
	case "Date", "Date32":
		return "date"
	case "DateTime", "DateTime64":
		return "timestamp"
	case "Decimal", "Decimal32", "Decimal64", "Decimal128", "Decimal256":
		return "decimal"
	case "Float32", "Float64", "BFloat16":
		return "double"
	case "Enum8", "Enum16":
		return "enum"
	case "JSON", "Object":
		return "json"
	case "Int8", "Int16", "Int32", "UInt8", "UInt16", "UInt32":
		return "integer"
	case "Int64", "Int128", "Int256", "UInt64", "UInt128", "UInt256":
		return "bigint"
	}
	// Array, Map, Tuple, Nested, and the geo types have no single-value
	// equivalent to claim; leaving them verbatim is what makes them show up
	// unrecognised rather than wrongly recognised.
	return base
}

// decorations pulls the length, precision, and scale out of a decorated type
// such as "FixedString(36)" or "Decimal(10,2)".
func decorations(base string) (maxLen, precision, scale int) {
	open := strings.IndexByte(base, '(')
	if open < 0 || !strings.HasSuffix(base, ")") {
		return 0, 0, 0
	}
	head := base[:open]
	parts := strings.Split(base[open+1:len(base)-1], ",")
	first := atoi(strings.TrimSpace(parts[0]))
	switch {
	case head == "FixedString":
		return first, 0, 0
	case strings.HasPrefix(head, "Decimal"):
		if len(parts) > 1 {
			return 0, first, atoi(strings.TrimSpace(parts[1]))
		}
		// Decimal32(S) and friends carry the scale alone; the precision is in
		// the type name and is not a number this cares about.
		return 0, 0, first
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

// enumLabels reads the labels out of `Enum8('draft' = 1, 'sent' = 2)`.
// ClickHouse escapes a quote inside a label with a backslash.
func enumLabels(base string) model.Values {
	if !strings.HasPrefix(base, "Enum8(") && !strings.HasPrefix(base, "Enum16(") {
		return nil
	}
	open := strings.IndexByte(base, '(')
	if !strings.HasSuffix(base, ")") {
		return nil
	}
	body := base[open+1 : len(base)-1]

	var out model.Values
	var cur strings.Builder
	inside := false
	for i := 0; i < len(body); i++ {
		switch {
		case !inside:
			if body[i] == '\'' {
				inside = true
				cur.Reset()
			}
		case body[i] == '\\' && i+1 < len(body):
			i++
			cur.WriteByte(body[i])
		case body[i] == '\'':
			inside = false
			out = append(out, cur.String())
		default:
			cur.WriteByte(body[i])
		}
	}
	return out
}
