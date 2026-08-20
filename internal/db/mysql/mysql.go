// Package mysql implements the Seedora driver for MySQL and MariaDB.
//
// Two things about MySQL shape this driver and are worth stating up front.
//
// The first is quoting. Seedora renders identifiers with double quotes, which
// MySQL reads as a string literal unless the session says otherwise, so the
// connection turns ANSI_QUOTES on before anything else runs. That keeps one
// spelling of an identifier across the whole codebase instead of threading a
// quoting function through the seeder. DDL rendered by internal/ddl uses
// backticks, which mean the same thing in either mode, so an exported
// schema.sql runs on a session Seedora did not configure.
//
// The second is that MySQL commits implicitly on DDL. A CREATE TABLE from the
// schema editor cannot be rolled back the way it can on Postgres or SQLite;
// the statements are still validated and rendered before they run, and they
// still run in order, but a failure halfway leaves the earlier ones applied.
// Seeding itself is unaffected: it is INSERTs only, and those are transactional
// on InnoDB.
//
// There is no bulk-load protocol on the wire (LOAD DATA LOCAL INFILE needs the
// server to allow it and the client to opt in, which is not a thing to turn on
// in somebody else's database), so the fast path is the same as SQLite's: one
// prepared multi-row INSERT per chunk, chunked on the placeholder limit.
package mysql

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/bakhod1r/seedora/internal/db"
	"github.com/bakhod1r/seedora/internal/ddl"
	"github.com/bakhod1r/seedora/internal/model"
)

func init() {
	db.Register(open, "mysql", "mariadb")
}

// Driver is a connected MySQL database.
type Driver struct {
	db     *sql.DB
	name   string
	schema string
}

func open(ctx context.Context, dsn string) (db.Driver, error) {
	native, err := NativeDSN(dsn)
	if err != nil {
		return nil, err
	}
	conn, err := sql.Open("mysql", native)
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	// One connection, because the whole run is a single transaction and the
	// session settings below are per connection.
	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(1)
	if err := conn.PingContext(ctx); err != nil {
		conn.Close()
		return nil, fmt.Errorf("connect: %w", err)
	}
	for _, stmt := range session {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			conn.Close()
			return nil, fmt.Errorf("%s: %w", stmt, err)
		}
	}

	d := &Driver{db: conn, name: "MySQL"}
	var version string
	if err := conn.QueryRowContext(ctx, "SELECT VERSION()").Scan(&version); err == nil {
		if strings.Contains(strings.ToLower(version), "mariadb") {
			d.name = "MariaDB"
		}
	}
	if err := conn.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&d.schema); err != nil || d.schema == "" {
		conn.Close()
		return nil, fmt.Errorf("the DSN names no database — add one, as in mysql://user:pass@localhost:3306/myapp_dev")
	}
	return d, nil
}

// session is what the connection is configured with before anything runs.
//
// ANSI_QUOTES is the one that matters: without it every identifier Seedora
// writes would be read as a string. STRICT_ALL_TABLES is deliberate too — MySQL
// will otherwise truncate an over-long value and carry on, and a seeder that
// silently writes something other than what it generated is worse than one that
// stops.
var session = []string{
	"SET SESSION sql_mode = 'ANSI_QUOTES,STRICT_ALL_TABLES'",
	// Unique-index probing during a run reads its own uncommitted writes, which
	// is the default isolation anyway; saying so keeps it true on a server whose
	// default was changed.
	"SET SESSION TRANSACTION ISOLATION LEVEL REPEATABLE READ",
}

// NativeDSN converts a URL-shaped DSN into the form go-sql-driver expects.
//
// Users type `mysql://user:pass@host:3306/db`, because that is what every other
// tool takes. The driver wants `user:pass@tcp(host:3306)/db`. A DSN already in
// the native form is passed through, so both work.
func NativeDSN(dsn string) (string, error) {
	dsn = strings.TrimSpace(dsn)
	i := strings.Index(dsn, "://")
	if i < 0 {
		// Already native. Only the parameters this driver depends on are added.
		cfg, err := mysql.ParseDSN(dsn)
		if err != nil {
			return "", fmt.Errorf("parse DSN: %w", err)
		}
		return withParams(cfg), nil
	}

	u, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("parse DSN: %w", err)
	}
	cfg := mysql.NewConfig()
	cfg.Net = "tcp"
	cfg.Addr = u.Host
	if u.Host == "" {
		cfg.Addr = "127.0.0.1:3306"
	} else if u.Port() == "" {
		cfg.Addr = u.Host + ":3306"
	}
	if u.User != nil {
		cfg.User = u.User.Username()
		cfg.Passwd, _ = u.User.Password()
	}
	cfg.DBName = strings.TrimPrefix(u.Path, "/")

	// A unix socket is spelled the way libmysqlclient spells it, as a parameter.
	q := u.Query()
	if sock := q.Get("socket"); sock != "" {
		cfg.Net = "unix"
		cfg.Addr = sock
		q.Del("socket")
	}
	cfg.Params = map[string]string{}
	for k := range q {
		cfg.Params[k] = q.Get(k)
	}
	return withParams(cfg), nil
}

// timeUTC is the location timestamps are read and written in, so a run is
// reproducible on a machine in another timezone.
var timeUTC = time.UTC

func withParams(cfg *mysql.Config) string {
	// Timestamps come back as time.Time rather than a byte string, which is what
	// the history reader and the preview both expect.
	cfg.ParseTime = true
	// Values are generated in Go and compared in Go; asking the server for UTC
	// keeps a run reproducible on a machine in another timezone.
	if cfg.Loc == nil || cfg.Loc.String() == "UTC" {
		cfg.Loc = timeUTC
	}
	if cfg.Params == nil {
		cfg.Params = map[string]string{}
	}
	// Round trips dominate a seeding run, and interpolating the arguments client
	// side turns each chunk from prepare-bind-execute into one packet.
	if _, ok := cfg.Params["interpolateParams"]; !ok {
		cfg.InterpolateParams = true
	}
	return cfg.FormatDSN()
}

// Name implements db.Driver.
func (d *Driver) Name() string { return d.name }

// Dialect implements db.Driver.
func (d *Driver) Dialect() ddl.Dialect { return ddl.MySQL }

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
	if err := d.loadCounts(ctx, s); err != nil {
		return nil, err
	}
	return s, nil
}

func (d *Driver) loadColumns(ctx context.Context, s *model.Schema) (map[string]*model.Table, error) {
	const q = `
SELECT c.TABLE_NAME, c.COLUMN_NAME, c.DATA_TYPE, c.COLUMN_TYPE, c.IS_NULLABLE,
       c.COLUMN_DEFAULT, c.EXTRA, c.CHARACTER_MAXIMUM_LENGTH,
       c.NUMERIC_PRECISION, c.NUMERIC_SCALE
FROM information_schema.COLUMNS c
JOIN information_schema.TABLES t
  ON t.TABLE_SCHEMA = c.TABLE_SCHEMA AND t.TABLE_NAME = c.TABLE_NAME
WHERE c.TABLE_SCHEMA = DATABASE() AND t.TABLE_TYPE = 'BASE TABLE'
ORDER BY c.TABLE_NAME, c.ORDINAL_POSITION`

	rows, err := d.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("read columns: %w", err)
	}
	defer rows.Close()

	byName := map[string]*model.Table{}
	for rows.Next() {
		var (
			table, name, dataType, colType, nullable, extra string
			def                                             sql.NullString
			maxLen, precision, scale                        sql.NullInt64
		)
		if err := rows.Scan(&table, &name, &dataType, &colType, &nullable, &def,
			&extra, &maxLen, &precision, &scale); err != nil {
			return nil, err
		}

		t := byName[table]
		if t == nil {
			t = &model.Table{Name: table}
			byName[table] = t
			s.Tables = append(s.Tables, t)
		}

		extra = strings.ToUpper(extra)
		c := &model.Column{
			Name:     name,
			Type:     strings.ToLower(dataType),
			Native:   colType,
			Nullable: strings.EqualFold(nullable, "YES"),
			// auto_increment fills itself; so does a column whose default is an
			// expression, which MySQL reports in EXTRA rather than as a default.
			HasDefault: def.Valid || strings.Contains(extra, "AUTO_INCREMENT") ||
				strings.Contains(extra, "DEFAULT_GENERATED"),
			// VIRTUAL GENERATED and STORED GENERATED are computed and cannot be
			// written. DEFAULT_GENERATED only means the default is an
			// expression, which is a different thing.
			Generated: strings.Contains(extra, "GENERATED") &&
				!strings.Contains(extra, "DEFAULT_GENERATED"),
			MaxLen:    int(maxLen.Int64),
			Precision: int(precision.Int64),
			Scale:     int(scale.Int64),
		}
		// MySQL has no boolean: BOOL is an alias for TINYINT(1), and that
		// spelling is the only signal that a 0/1 column means true/false.
		if strings.EqualFold(colType, "tinyint(1)") {
			c.Type = "boolean"
		}
		// A decimal's precision is meaningful; an integer's is not, and passing
		// it on makes inference read a bigint as a bounded number.
		if !strings.Contains(c.Type, "decimal") && !strings.Contains(c.Type, "numeric") &&
			!strings.Contains(c.Type, "float") && !strings.Contains(c.Type, "double") {
			c.Precision, c.Scale = 0, 0
		}
		// An enum is declared inline rather than as a named type, so the type is
		// synthesised from where it appears.
		if c.Type == "enum" || c.Type == "set" {
			if labels := enumLabels(colType); len(labels) > 0 {
				typeName := table + "_" + name
				s.Enums[typeName] = labels
				c.EnumType = typeName
			}
		}
		t.Columns = append(t.Columns, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return byName, nil
}

// enumLabels reads the labels out of `enum('draft','sent')`. MySQL escapes a
// quote inside a label by doubling it, which is the only escape to undo.
func enumLabels(colType string) model.Values {
	open := strings.IndexByte(colType, '(')
	if open < 0 || !strings.HasSuffix(colType, ")") {
		return nil
	}
	body := colType[open+1 : len(colType)-1]

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
		case body[i] == '\'' && i+1 < len(body) && body[i+1] == '\'':
			cur.WriteByte('\'')
			i++
		case body[i] == '\'':
			inside = false
			out = append(out, cur.String())
		default:
			cur.WriteByte(body[i])
		}
	}
	return out
}

// loadIndexes fills in primary keys and single-column uniqueness. MySQL records
// both as indexes, so one query answers both.
func (d *Driver) loadIndexes(ctx context.Context, byName map[string]*model.Table) error {
	const q = `
SELECT TABLE_NAME, INDEX_NAME, COLUMN_NAME, NON_UNIQUE, SEQ_IN_INDEX
FROM information_schema.STATISTICS
WHERE TABLE_SCHEMA = DATABASE()
ORDER BY TABLE_NAME, INDEX_NAME, SEQ_IN_INDEX`

	rows, err := d.db.QueryContext(ctx, q)
	if err != nil {
		return fmt.Errorf("read indexes: %w", err)
	}
	defer rows.Close()

	type key struct{ table, index string }
	cols := map[key][]string{}
	uniq := map[key]bool{}
	var order []key
	for rows.Next() {
		var table, index string
		var column sql.NullString
		var nonUnique, seq int64
		if err := rows.Scan(&table, &index, &column, &nonUnique, &seq); err != nil {
			return err
		}
		// A functional index reports no column, and there is nothing to mark.
		if !column.Valid {
			continue
		}
		k := key{table, index}
		if _, seen := cols[k]; !seen {
			order = append(order, k)
		}
		cols[k] = append(cols[k], column.String)
		uniq[k] = nonUnique == 0
	}
	if err := rows.Err(); err != nil {
		return err
	}

	for _, k := range order {
		t := byName[k.table]
		if t == nil {
			continue
		}
		if k.index == "PRIMARY" {
			t.PrimaryKey = append(t.PrimaryKey, cols[k]...)
			continue
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
SELECT TABLE_NAME, CONSTRAINT_NAME, COLUMN_NAME,
       REFERENCED_TABLE_NAME, REFERENCED_COLUMN_NAME
FROM information_schema.KEY_COLUMN_USAGE
WHERE TABLE_SCHEMA = DATABASE()
  AND REFERENCED_TABLE_NAME IS NOT NULL
  AND REFERENCED_TABLE_SCHEMA = DATABASE()
ORDER BY TABLE_NAME, CONSTRAINT_NAME, ORDINAL_POSITION`

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

// loadCounts reads the exact row count per table. information_schema.TABLES
// carries an estimate, and it is wrong often enough on InnoDB that a truncate
// confirmation built on it would be a lie.
func (d *Driver) loadCounts(ctx context.Context, s *model.Schema) error {
	for _, t := range s.Tables {
		if err := d.db.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM "+t.Qualified()).Scan(&t.ExistingRows); err != nil {
			return fmt.Errorf("count %s: %w", t.Name, err)
		}
	}
	return nil
}

// History reads whatever a migration tool left behind. MySQL records no DDL
// history of its own.
func (d *Driver) History(ctx context.Context) ([]model.Migration, error) {
	rows, err := d.db.QueryContext(ctx, `
SELECT TABLE_NAME FROM information_schema.TABLES
WHERE TABLE_SCHEMA = DATABASE() AND TABLE_TYPE = 'BASE TABLE'`)
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

// scanRows turns a result set into field→value maps without knowing what the
// columns are, which is what reading another tool's table requires. MySQL hands
// text back as []byte, which no consumer of this expects to see.
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

// Tx is a MySQL seeding transaction.
type Tx struct {
	tx   *sql.Tx
	done bool
}

// Exec implements db.Tx.
//
// MySQL commits the open transaction implicitly before any DDL statement, so
// unlike the other engines a schema change applied here cannot be rolled back.
// The statements are validated and rendered before they run, and they run in
// dependency order, which is what is left to offer.
func (t *Tx) Exec(ctx context.Context, stmt string) error {
	if _, err := t.tx.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("exec: %w", err)
	}
	return nil
}

// Truncate implements db.Tx with DELETE rather than TRUNCATE TABLE.
//
// TRUNCATE is DDL on MySQL: it commits the transaction the seeder is relying on
// to unwind a failed run, and it is refused outright on a table a foreign key
// points at. DELETE is slower and is the only version that keeps the promise
// that a failure leaves the database as it was.
func (t *Tx) Truncate(ctx context.Context, tb *model.Table) error {
	if _, err := t.tx.ExecContext(ctx, "DELETE FROM "+tb.Qualified()); err != nil {
		// 1451 is a child row still pointing at a parent being emptied. Postgres
		// hides this case behind TRUNCATE … CASCADE, which deletes rows from
		// tables the user never named; saying what is in the way is worth more
		// than matching that, since the fix is one line of the plan.
		var me *mysql.MySQLError
		if errors.As(err, &me) && me.Number == 1451 {
			return fmt.Errorf("truncate %s: rows in another table still point at it — "+
				"include that table in this run, or truncate it first (%s)", tb.Name, me.Message)
		}
		return fmt.Errorf("truncate %s: %w", tb.Name, err)
	}
	// The auto-increment counter is deliberately left alone: resetting it is an
	// ALTER, which would commit the transaction.
	return nil
}

// maxParams is the placeholder limit in one prepared statement. Splitting on it
// is what keeps a wide table from failing at a batch size a narrow one handles,
// since the limit is on values, not rows.
const maxParams = 60000

// Insert implements db.Tx with multi-row INSERT statements, chunked so no
// statement exceeds the placeholder limit.
func (t *Tx) Insert(ctx context.Context, tb *model.Table, cols []string, rows db.Source) (int64, error) {
	if len(cols) == 0 {
		return 0, nil
	}
	perRow := len(cols)
	chunk := maxParams / perRow
	if chunk < 1 {
		return 0, fmt.Errorf("insert into %s: %d columns exceeds the statement limit", tb.Name, perRow)
	}

	quoted := make([]string, len(cols))
	for i, c := range cols {
		quoted[i] = model.QuoteIdent(c)
	}
	head := "INSERT INTO " + tb.Qualified() + " (" + strings.Join(quoted, ", ") + ") VALUES "

	full, err := t.tx.PrepareContext(ctx, head+placeholders(chunk, perRow))
	if err != nil {
		return 0, fmt.Errorf("prepare insert into %s: %w", tb.Name, err)
	}
	defer full.Close()

	var written int64
	args := make([]any, 0, chunk*perRow)

	flush := func() error {
		if len(args) == 0 {
			return nil
		}
		n := len(args) / perRow
		var err error
		if n == chunk {
			_, err = full.ExecContext(ctx, args...)
		} else {
			_, err = t.tx.ExecContext(ctx, head+placeholders(n, perRow), args...)
		}
		if err != nil {
			return fmt.Errorf("insert into %s: %w", tb.Name, err)
		}
		written += int64(n)
		args = args[:0]
		return nil
	}

	var loopErr error
	for row := range rows.Rows() {
		for _, c := range cols {
			args = append(args, value(row[c]))
		}
		if len(args) == chunk*perRow {
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
	case map[string]any, []any:
		// A JSON column is generated as a Go value; the driver has no encoder
		// for one, and the server wants the text anyway.
		return jsonText(x)
	case uint64:
		// The protocol has no unsigned parameter, and anything a generator
		// produces is far inside the signed range.
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

// ReadKeys implements db.Tx.
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

// MaxValue implements db.Tx.
func (t *Tx) MaxValue(ctx context.Context, tb *model.Table, col string) (int64, bool, error) {
	q := fmt.Sprintf("SELECT max(%s) FROM %s", model.QuoteIdent(col), tb.Qualified())
	var v *int64
	if err := t.tx.QueryRowContext(ctx, q).Scan(&v); err != nil {
		return 0, false, fmt.Errorf("read max of %s.%s: %w", tb.Name, col, err)
	}
	if v == nil {
		return 0, false, nil
	}
	return *v, true, nil
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

// placeholders builds "(?,?),(?,?),…" for n rows of width w.
func placeholders(n, w int) string {
	var sb strings.Builder
	sb.Grow(n * (w*2 + 2))
	for i := range n {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteByte('(')
		for j := range w {
			if j > 0 {
				sb.WriteByte(',')
			}
			sb.WriteByte('?')
		}
		sb.WriteByte(')')
	}
	return sb.String()
}
