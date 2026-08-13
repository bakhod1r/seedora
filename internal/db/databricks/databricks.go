// Package databricks implements the Seedora driver for Databricks SQL
// warehouses and the Unity Catalog behind them.
//
// Two things shape this driver.
//
// The first is that there is no transaction. Delta Lake commits per statement:
// every INSERT is its own version of the table and is visible the moment it
// lands. The SQL warehouse protocol has no BEGIN, and the Go client has no
// driver.Tx to open one with. So Seedora's promise — a run is one transaction,
// and a failure leaves the database exactly as it was — cannot be kept here.
// This driver does not fake it: Begin hands back an object because the
// interface asks for one, and Rollback returns an error naming what is still in
// the table rather than nil as though it had undone something. That is the
// MySQL DDL concession again, widened to cover the rows.
//
// The second is quoting. Databricks spells identifiers with backticks; a
// double-quoted name is a string literal on a warehouse that has not had
// double-quoted identifiers turned on. Every statement this driver writes
// quotes with backticks itself rather than trusting a session setting, and the
// setting is only attempted so that DDL rendered by internal/ddl — which uses
// double quotes for this dialect — is accepted through Exec.
//
// The bulk path is a staged multi-row INSERT: rows are rendered into VALUES
// tuples and sent in statements sized to the warehouse's statement limit.
// COPY INTO is the faster load, but it reads from a cloud storage location the
// files would have to be written to first, and Seedora has no staging bucket to
// write them to — the rows exist only in this process.
package databricks

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	_ "github.com/databricks/databricks-sql-go"

	"github.com/bakhod1r/seedora/internal/db"
	"github.com/bakhod1r/seedora/internal/ddl"
	"github.com/bakhod1r/seedora/internal/model"
)

func init() {
	db.Register(open, "databricks")
}

// Driver is a connected Databricks SQL warehouse.
type Driver struct {
	db *sql.DB
	// catalog and schema are the session's, read once. Unity Catalog serves an
	// information_schema per catalog, so both have to be named in the query
	// rather than assumed.
	catalog string
	schema  string
}

func open(ctx context.Context, dsn string) (db.Driver, error) {
	// The client's own DSN has no scheme — `token:…@host:443/sql/1.0/…` — and
	// prepends https itself. The scheme is only how Seedora routes the DSN to
	// this driver.
	native := strings.TrimPrefix(strings.TrimSpace(dsn), "databricks://")
	conn, err := sql.Open("databricks", native)
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	// One connection: the session settings below are per connection, and a
	// seeding run is a sequence anyway.
	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(1)

	d := &Driver{db: conn}
	if err := conn.QueryRowContext(ctx,
		"SELECT current_catalog(), current_database()").Scan(&d.catalog, &d.schema); err != nil {
		conn.Close()
		return nil, fmt.Errorf("connect: %w", err)
	}
	if d.catalog == "" || d.schema == "" {
		conn.Close()
		return nil, fmt.Errorf("the DSN names no catalog and schema — add them, as in " +
			"databricks://token:***@host:443/sql/1.0/warehouses/abc?catalog=main&schema=myapp_dev")
	}
	// Best effort, and deliberately not fatal: it only decides whether the
	// schema editor's rendered DDL parses, and a warehouse too old to know the
	// setting still seeds correctly, since the seeding path quotes with
	// backticks of its own.
	_, _ = conn.ExecContext(ctx, "SET spark.sql.ansi.doubleQuotedIdentifiers = true")
	return d, nil
}

// Name implements db.Driver.
func (d *Driver) Name() string { return "Databricks" }

// Dialect implements db.Driver.
func (d *Driver) Dialect() ddl.Dialect { return ddl.Databricks }

// Close implements db.Driver.
func (d *Driver) Close(context.Context) error { return d.db.Close() }

// Begin implements db.Driver. Nothing is begun — see the package comment.
func (d *Driver) Begin(context.Context) (db.Tx, error) {
	return &Tx{db: d.db}, nil
}

// Introspect reads Unity Catalog's information_schema, which unlike most
// engines' is the real catalog here rather than a view over one: it is where
// Unity Catalog keeps tables, columns, and the informational key constraints.
func (d *Driver) Introspect(ctx context.Context) (*model.Schema, error) {
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

// info qualifies an information_schema table with the session's catalog. Unity
// Catalog has one per catalog and an unqualified name resolves against the
// session schema, which is not where it lives.
func (d *Driver) info(table string) string {
	return qi(d.catalog) + ".information_schema." + table
}

func (d *Driver) loadColumns(ctx context.Context, s *model.Schema) (map[string]*model.Table, error) {
	q := fmt.Sprintf(`
SELECT c.table_name, c.column_name, c.data_type, c.full_data_type, c.is_nullable,
       c.column_default, c.is_identity, c.character_maximum_length,
       c.numeric_precision, c.numeric_scale
FROM %s c
JOIN %s t ON t.table_schema = c.table_schema AND t.table_name = c.table_name
WHERE c.table_schema = %s AND t.table_type != 'VIEW'
ORDER BY c.table_name, c.ordinal_position`,
		d.info("columns"), d.info("tables"), lit(d.schema))

	rows, err := d.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("read columns: %w", err)
	}
	defer rows.Close()

	byName := map[string]*model.Table{}
	for rows.Next() {
		var (
			table, name, dataType, fullType, nullable string
			def, identity                             sql.NullString
			maxLen, precision, scale                  sql.NullInt64
		)
		if err := rows.Scan(&table, &name, &dataType, &fullType, &nullable, &def,
			&identity, &maxLen, &precision, &scale); err != nil {
			return nil, err
		}
		t := byName[table]
		if t == nil {
			t = &model.Table{Name: table}
			byName[table] = t
			s.Tables = append(s.Tables, t)
		}
		c := &model.Column{
			Name:     name,
			Type:     normalize(dataType),
			Native:   fullType,
			Nullable: strings.EqualFold(nullable, "YES"),
			// A GENERATED … AS IDENTITY column fills itself, which is the same
			// thing a default is as far as the planner is concerned.
			HasDefault: def.Valid || strings.EqualFold(identity.String, "YES"),
			MaxLen:     int(maxLen.Int64),
			Precision:  int(precision.Int64),
			Scale:      int(scale.Int64),
		}
		// Precision on an integer is a width, not a scale anything here should
		// generate against.
		if c.Type != "decimal" {
			c.Precision, c.Scale = 0, 0
		}
		t.Columns = append(t.Columns, c)
	}
	return byName, rows.Err()
}

// loadConstraints marks primary keys, single-column unique constraints, and
// foreign keys.
//
// Unity Catalog's key constraints are informational: it records them and does
// not enforce them, so nothing here will fail an insert the way Postgres would.
// They are still worth reading, and are in fact the only statement of intent
// available — they are what tells the planner which column is a key and which
// table a column points at, so the generated data has the shape the schema
// claims. A table declared without them looks unrelated to everything else.
func (d *Driver) loadConstraints(ctx context.Context, byName map[string]*model.Table) error {
	q := fmt.Sprintf(`
SELECT tc.constraint_type, kcu.table_name, kcu.column_name, kcu.ordinal_position,
       COALESCE(ccu.table_name, ''), COALESCE(ccu.column_name, ''),
       count(*) OVER (PARTITION BY tc.constraint_name) AS key_width
FROM %s tc
JOIN %s kcu
  ON kcu.constraint_name = tc.constraint_name
 AND kcu.constraint_schema = tc.constraint_schema
LEFT JOIN %s rc
  ON rc.constraint_name = tc.constraint_name
 AND rc.constraint_schema = tc.constraint_schema
LEFT JOIN %s ccu
  ON ccu.constraint_name = rc.unique_constraint_name
 AND ccu.constraint_schema = rc.unique_constraint_schema
 AND ccu.ordinal_position = kcu.ordinal_position
WHERE tc.table_schema = %s
ORDER BY kcu.table_name, tc.constraint_name, kcu.ordinal_position`,
		d.info("table_constraints"), d.info("key_column_usage"),
		d.info("referential_constraints"), d.info("constraint_column_usage"),
		lit(d.schema))

	rows, err := d.db.QueryContext(ctx, q)
	if err != nil {
		// A workspace on the legacy Hive metastore has no constraint tables at
		// all. That is a schema with no declared keys, not a broken database,
		// and it should not stop introspection.
		return nil
	}
	defer rows.Close()

	for rows.Next() {
		var ctype, table, column, refTable, refCol string
		var ord, width int64
		if err := rows.Scan(&ctype, &table, &column, &ord, &refTable, &refCol, &width); err != nil {
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

// countChunk is how many tables are counted in one statement. A warehouse
// charges for the time a statement is running and the queue in front of it, so
// folding the counts into a UNION ALL turns a catalog of two hundred tables
// from two hundred round trips into two.
const countChunk = 100

// loadCounts counts the rows in every table. Delta keeps the number in its own
// transaction log and answers COUNT(*) from the file statistics rather than by
// scanning, which is what makes an exact count affordable here — and it is what
// the truncate confirmation is built on, so an estimate would not do.
func (d *Driver) loadCounts(ctx context.Context, s *model.Schema) error {
	counts := map[string]int64{}
	for start := 0; start < len(s.Tables); start += countChunk {
		end := min(start+countChunk, len(s.Tables))
		parts := make([]string, 0, end-start)
		for _, t := range s.Tables[start:end] {
			parts = append(parts, fmt.Sprintf("SELECT %s AS t, count(*) AS n FROM %s",
				lit(t.Name), d.qualify(t)))
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

// qualify names a table the way a statement must spell it here: backticks, and
// resolved against the session catalog and schema rather than model.Table's own
// double-quoted spelling.
func (d *Driver) qualify(t *model.Table) string {
	return qi(d.catalog) + "." + qi(d.schema) + "." + qi(t.Name)
}

// History reads whatever a migration tool left behind. Databricks records no
// DDL history a query can reach — DESCRIBE HISTORY is per table and is the
// Delta commit log, not a record of the schema's origins.
func (d *Driver) History(ctx context.Context) ([]model.Migration, error) {
	rows, err := d.db.QueryContext(ctx, fmt.Sprintf(
		"SELECT table_name FROM %s WHERE table_schema = %s", d.info("tables"), lit(d.schema)))
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

	return db.ReadHistory(ctx, qi,
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

// Tx is a Databricks seeding "transaction" — a record of writes that have
// already been committed, since the warehouse offers nothing to hold them open
// in.
type Tx struct {
	db        *sql.DB
	done      bool
	truncated []string
	written   int64
}

// Truncate implements db.Tx. TRUNCATE TABLE on Delta is one commit that removes
// every file from the table, and it is not DDL the way MySQL's is — but it is
// still permanent the instant it succeeds. A run that dies between this and its
// inserts leaves the table empty, and only time travel (`VERSION AS OF`) can
// show what was there.
func (t *Tx) Truncate(ctx context.Context, tb *model.Table) error {
	if _, err := t.db.ExecContext(ctx, "TRUNCATE TABLE "+qualifyName(tb)); err != nil {
		return fmt.Errorf("truncate %s: %w", tb.Name, err)
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

// insertRows and insertBytes bound one INSERT. The warehouse rejects a
// statement over its length limit, so the byte budget is the one that decides;
// the row count keeps a narrow table from building a tuple list so long that
// parsing it costs more than writing it.
const (
	insertRows  = 5000
	insertBytes = 500_000
)

// Insert implements db.Tx with staged multi-row INSERT … VALUES statements.
//
// Values are rendered into the statement rather than bound as parameters: the
// warehouse caps how many parameter markers one statement may carry, well below
// the tuple counts that make a bulk insert worth doing, and a rendered
// statement has no such limit.
//
// Each statement is a Delta commit of its own. A failure on the eleventh leaves
// the first ten committed and visible, as ten separate versions of the table;
// nothing here can take them back.
func (t *Tx) Insert(ctx context.Context, tb *model.Table, cols []string, rows db.Source) (int64, error) {
	if len(cols) == 0 {
		return 0, nil
	}
	quoted := make([]string, len(cols))
	targets := make([]*model.Column, len(cols))
	for i, c := range cols {
		quoted[i] = qi(c)
		targets[i] = tb.Column(c)
	}
	head := "INSERT INTO " + qualifyName(tb) + " (" + strings.Join(quoted, ", ") + ") VALUES "

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
			// A column absent from the row map is NULL: a VALUES list here has
			// no DEFAULT keyword, so a column meant to fill itself belongs out
			// of the column list rather than in it as NULL.
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
	name := qi(col)
	q := fmt.Sprintf("SELECT %s FROM %s WHERE %s IS NOT NULL LIMIT %s",
		name, qualifyName(tb), name, strconv.Itoa(limit))
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

// Rollback implements db.Tx, and cannot do what its name says. Returning nil
// would report a clean unwind of a table that still holds every row written and
// is still missing every row truncated, so it names what is left instead — and
// what the user's own recourse is, which on Delta is the one thing this engine
// has that the others do not. A run that changed nothing reports success
// honestly, which is also what keeps a deferred Rollback quiet after a Commit.
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
	return fmt.Errorf("this run cannot be rolled back: Databricks commits as it writes. "+
		"Already permanent: %s. Restore the table from the Delta version before the run "+
		"(RESTORE TABLE … TO VERSION AS OF) if you need it back", strings.Join(what, " and "))
}

// qualifyName is the table as a statement must spell it, in backticks. Unlike
// the driver's own qualify it uses whatever schema the table carries, which for
// a table this driver introspected is none — leaving it to resolve against the
// session, which is the schema it was read from.
func qualifyName(t *model.Table) string {
	if t.Schema == "" {
		return qi(t.Name)
	}
	return qi(t.Schema) + "." + qi(t.Name)
}

// qi quotes an identifier the way Databricks spells one. A double-quoted name
// is a string literal on a warehouse whose double-quoted-identifier setting is
// off, which is the default.
func qi(s string) string {
	return "`" + strings.ReplaceAll(s, "`", "``") + "`"
}

// lit renders a string as a SQL literal. Databricks reads a backslash inside a
// literal as an escape, so both it and the quote have to be escaped — one more
// than the SQL standard needs, and the reason this is not model's quoting.
func lit(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `'`, `\'`)
	return "'" + r.Replace(s) + "'"
}

// literal renders one generated value as Databricks SQL. The column is passed
// in because the same Go value spells differently depending on where it lands,
// and Spark applies no implicit conversion from a string to a date.
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
			return "true"
		}
		return "false"
	case int:
		return strconv.Itoa(x)
	case int8, int16, int32, int64:
		return fmt.Sprintf("%d", x)
	case uint, uint8, uint16, uint32, uint64:
		return fmt.Sprintf("%d", x)
	case float32:
		return "CAST(" + lit(strconv.FormatFloat(float64(x), 'g', -1, 32)) + " AS FLOAT)"
	case float64:
		// A bare 1.5 is DECIMAL in Spark, and a DECIMAL wider than the column
		// fails the insert rather than rounding. The cast says which type it is.
		return "CAST(" + lit(strconv.FormatFloat(x, 'g', -1, 64)) + " AS DOUBLE)"
	case []byte:
		return "X'" + hex(x) + "'"
	case time.Time:
		if strings.HasPrefix(typ, "date") {
			return "DATE " + lit(x.UTC().Format("2006-01-02"))
		}
		return "TIMESTAMP " + lit(x.UTC().Format("2006-01-02 15:04:05.000"))
	case map[string]any, []any:
		// Databricks has no JSON type: a document lands in a string column and
		// is read back with from_json or the : operator.
		b, err := json.Marshal(x)
		if err != nil {
			return "NULL"
		}
		return lit(string(b))
	case string:
		return lit(x)
	default:
		return lit(fmt.Sprint(x))
	}
}

const hexDigits = "0123456789abcdef"

func hex(b []byte) string {
	out := make([]byte, 0, len(b)*2)
	for _, c := range b {
		out = append(out, hexDigits[c>>4], hexDigits[c&0x0f])
	}
	return string(out)
}

// normalize maps a Databricks type name onto the conventional SQL one.
// Inference classifies on the type name and knows the ANSI spellings; STRING
// and BINARY are the two that would otherwise come out unrecognised, which
// costs the user a manual mapping on every text column in the schema. The
// verbatim type is kept in Column.Native.
func normalize(dataType string) string {
	switch strings.ToLower(strings.TrimSpace(dataType)) {
	case "string":
		return "text"
	case "binary":
		return "bytea"
	case "long":
		return "bigint"
	case "short":
		return "smallint"
	case "byte":
		return "tinyint"
	case "interval":
		return "interval"
	}
	// BIGINT, INT, DOUBLE, FLOAT, BOOLEAN, DATE, TIMESTAMP, TIMESTAMP_NTZ and
	// DECIMAL already say what they are. ARRAY, MAP, STRUCT and VARIANT have no
	// single-value equivalent to claim, and leaving them verbatim is what makes
	// them show up unrecognised rather than wrongly recognised.
	return strings.ToLower(strings.TrimSpace(dataType))
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
