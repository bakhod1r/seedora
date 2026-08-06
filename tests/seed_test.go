// Package tests runs Seedora end to end against a real database: introspect a
// schema, infer a plan, seed it, and then read back what landed.
//
// The unit tests under internal/ prove each piece. These prove the promises
// made to the person running the tool — that foreign keys point at rows that
// exist, that a unique column is unique, that the same seed produces the same
// database, and that a dry run writes nothing.
package tests

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	_ "github.com/go-sql-driver/mysql"
	_ "modernc.org/sqlite"

	"github.com/bakhod1r/seedora/internal/db"
	mysqldrv "github.com/bakhod1r/seedora/internal/db/mysql"
	_ "github.com/bakhod1r/seedora/internal/db/postgres"
	_ "github.com/bakhod1r/seedora/internal/db/sqlite"
	"github.com/bakhod1r/seedora/internal/model"
	"github.com/bakhod1r/seedora/internal/plan"
	"github.com/bakhod1r/seedora/internal/seed"
)

// The schema is written twice because the engines do not spell types the same
// way, and a test that only ran against the spelling one engine accepts would
// prove nothing about the other.
const sqliteSchema = `
CREATE TABLE users (
  id         INTEGER PRIMARY KEY,
  email      VARCHAR(120) NOT NULL UNIQUE,
  first_name VARCHAR(50) NOT NULL,
  city       VARCHAR(60),
  is_active  BOOLEAN NOT NULL,
  created_at TIMESTAMP NOT NULL
);
CREATE TABLE orders (
  id        INTEGER PRIMARY KEY,
  user_id   INTEGER NOT NULL REFERENCES users(id),
  status    VARCHAR(20) NOT NULL,
  total     DECIMAL(10,2) NOT NULL
);
`

const postgresSchema = `
DROP TABLE IF EXISTS orders;
DROP TABLE IF EXISTS users;
CREATE TABLE users (
  id         bigserial PRIMARY KEY,
  email      varchar(120) NOT NULL UNIQUE,
  first_name varchar(50) NOT NULL,
  city       varchar(60),
  is_active  boolean NOT NULL,
  created_at timestamptz NOT NULL
);
CREATE TABLE orders (
  id      bigserial PRIMARY KEY,
  user_id bigint NOT NULL REFERENCES users(id),
  status  varchar(20) NOT NULL,
  total   numeric(10,2) NOT NULL
);
`

// MySQL spells the same schema a third way: no boolean, no timestamptz, and a
// key that fills itself is AUTO_INCREMENT rather than a serial type.
const mysqlSchema = `
DROP TABLE IF EXISTS invoices;
DROP TABLE IF EXISTS orders;
DROP TABLE IF EXISTS users;
CREATE TABLE users (
  id         BIGINT AUTO_INCREMENT PRIMARY KEY,
  email      VARCHAR(120) NOT NULL UNIQUE,
  first_name VARCHAR(50) NOT NULL,
  city       VARCHAR(60),
  is_active  BOOLEAN NOT NULL,
  created_at DATETIME NOT NULL
);
CREATE TABLE orders (
  id      BIGINT AUTO_INCREMENT PRIMARY KEY,
  user_id BIGINT NOT NULL,
  status  VARCHAR(20) NOT NULL,
  total   DECIMAL(10,2) NOT NULL,
  FOREIGN KEY (user_id) REFERENCES users(id)
);
`

// target is one engine, ready to seed, plus a raw handle for the assertions:
// what the tool believes it wrote is not evidence, and the database is.
type target struct {
	name   string
	driver db.Driver
	schema *model.Schema
	raw    *sql.DB
}

// targets returns every engine available on this machine. Postgres is included
// only when a DSN says which database it may destroy.
func targets(t *testing.T) []target {
	t.Helper()
	out := []target{openSQLite(t)}
	if dsn := os.Getenv("SEEDORA_TEST_POSTGRES"); dsn != "" {
		out = append(out, openPostgres(t, dsn))
	}
	if dsn := os.Getenv("SEEDORA_TEST_MYSQL"); dsn != "" {
		out = append(out, openMySQL(t, dsn))
	}
	return out
}

func openSQLite(t *testing.T) target {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(sqliteSchema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { raw.Close() })

	return connect(t, "sqlite", path, raw)
}

func openPostgres(t *testing.T, dsn string) target {
	t.Helper()

	raw, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Skipf("postgres: %v", err)
	}
	if err := raw.Ping(); err != nil {
		t.Skipf("postgres is not reachable: %v", err)
	}
	if _, err := raw.Exec(postgresSchema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { raw.Close() })

	return connect(t, "postgres", dsn, raw)
}

func openMySQL(t *testing.T, dsn string) target {
	t.Helper()

	// The raw handle needs the driver's own DSN form, and the statements below
	// are several, which MySQL only accepts on a connection that asked for it.
	native, err := mysqldrv.NativeDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("mysql", native+"&multiStatements=true")
	if err != nil {
		t.Skipf("mysql: %v", err)
	}
	if err := raw.Ping(); err != nil {
		t.Skipf("mysql is not reachable: %v", err)
	}
	// The database named by SEEDORA_TEST_MYSQL is one the tests may destroy, and
	// emptying it first is what makes the suite immune to whatever a previous
	// run — or a previous experiment — left in it. A leftover child table makes
	// every DROP TABLE below fail on a foreign key.
	if err := dropAllMySQL(raw); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(mysqlSchema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { raw.Close() })

	return connect(t, "mysql", dsn, raw)
}

// dropAllMySQL empties the test database. Foreign key checks are off for the
// duration because the drop order of a schema nobody wrote down is not worth
// computing.
func dropAllMySQL(raw *sql.DB) error {
	rows, err := raw.Query(`
SELECT TABLE_NAME FROM information_schema.TABLES
WHERE TABLE_SCHEMA = DATABASE() AND TABLE_TYPE = 'BASE TABLE'`)
	if err != nil {
		return err
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return err
		}
		names = append(names, n)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(names) == 0 {
		return nil
	}

	if _, err := raw.Exec("SET FOREIGN_KEY_CHECKS = 0"); err != nil {
		return err
	}
	defer raw.Exec("SET FOREIGN_KEY_CHECKS = 1")
	for _, n := range names {
		if _, err := raw.Exec("DROP TABLE IF EXISTS `" + n + "`"); err != nil {
			return err
		}
	}
	return nil
}

func connect(t *testing.T, name, dsn string, raw *sql.DB) target {
	t.Helper()
	ctx := t.Context()

	d, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close(context.Background()) })

	s, err := d.Introspect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return target{name: name, driver: d, schema: s, raw: raw}
}

func planFor(tg target, users, orders int) *plan.Plan {
	p := plan.Infer(tg.schema)
	p.Tables["users"].Rows = users
	p.Tables["orders"].Rows = orders
	return p
}

func count(t *testing.T, raw *sql.DB, query string) int64 {
	t.Helper()
	var n int64
	if err := raw.QueryRow(query).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A run writes what it was asked for, and the rows it writes satisfy the
// constraints the schema declares. This is the whole promise in one test.
func TestSeedFillsATableAndKeepsItsConstraints(t *testing.T) {
	for _, tg := range targets(t) {
		t.Run(tg.name, func(t *testing.T) {
			res, err := seed.Run(t.Context(), tg.driver, tg.schema, planFor(tg, 500, 2000),
				seed.Options{Truncate: true, Seed: 7})
			if err != nil {
				t.Fatal(err)
			}
			if res.Rows != 2500 {
				t.Errorf("reported %d rows, want 2500", res.Rows)
			}

			if got := count(t, tg.raw, "SELECT COUNT(*) FROM users"); got != 500 {
				t.Errorf("users: %d rows, want 500", got)
			}
			if got := count(t, tg.raw, "SELECT COUNT(*) FROM orders"); got != 2000 {
				t.Errorf("orders: %d rows, want 2000", got)
			}

			// A unique column that is not unique is a constraint violation the
			// database happened not to catch, which is worse than one it did.
			if got := count(t, tg.raw, "SELECT COUNT(DISTINCT email) FROM users"); got != 500 {
				t.Errorf("emails: %d distinct, want 500", got)
			}

			// Every foreign key points at a row that exists. The engine enforces
			// this, so a failure here means it was not enforcing it.
			orphans := count(t, tg.raw,
				"SELECT COUNT(*) FROM orders o LEFT JOIN users u ON u.id = o.user_id WHERE u.id IS NULL")
			if orphans != 0 {
				t.Errorf("%d orders point at a user that does not exist", orphans)
			}

			// The database assigns its own keys. A seeder that filled them would
			// leave the sequence behind the rows it wrote.
			if got := count(t, tg.raw, "SELECT COUNT(DISTINCT id) FROM users"); got != 500 {
				t.Errorf("ids: %d distinct, want 500", got)
			}
		})
	}
}

// The same seed produces the same database. This is what makes a committed
// mapping worth committing: a test that asserts on row 7 keeps passing for the
// right reason.
func TestSameSeedProducesTheSameRows(t *testing.T) {
	for _, tg := range targets(t) {
		t.Run(tg.name, func(t *testing.T) {
			read := func() []string {
				t.Helper()
				rows, err := tg.raw.Query("SELECT email, first_name FROM users ORDER BY id")
				if err != nil {
					t.Fatal(err)
				}
				defer rows.Close()

				var out []string
				for rows.Next() {
					var email, name string
					if err := rows.Scan(&email, &name); err != nil {
						t.Fatal(err)
					}
					out = append(out, email+"|"+name)
				}
				if err := rows.Err(); err != nil {
					t.Fatal(err)
				}
				return out
			}

			run := func(seedVal uint64) []string {
				t.Helper()
				if _, err := seed.Run(t.Context(), tg.driver, tg.schema, planFor(tg, 200, 0),
					seed.Options{Truncate: true, Seed: seedVal}); err != nil {
					t.Fatal(err)
				}
				return read()
			}

			first := run(42)
			again := run(42)
			if len(first) != len(again) {
				t.Fatalf("row counts differ: %d and %d", len(first), len(again))
			}
			for i := range first {
				if first[i] != again[i] {
					t.Fatalf("row %d differs:\n%s\n%s", i, first[i], again[i])
				}
			}

			// And a different seed produces different rows, or the first half of
			// this test would pass on a generator that returns a constant.
			other := run(43)
			same := 0
			for i := range other {
				if i < len(first) && other[i] == first[i] {
					same++
				}
			}
			if same == len(first) {
				t.Fatal("a different seed produced an identical database")
			}
		})
	}
}

// A dry run generates everything and writes nothing. It is the answer to "will
// this work against my schema" for someone who is not ready to find out the
// other way.
func TestDryRunWritesNothing(t *testing.T) {
	for _, tg := range targets(t) {
		t.Run(tg.name, func(t *testing.T) {
			res, err := seed.Run(t.Context(), tg.driver, tg.schema, planFor(tg, 300, 900),
				seed.Options{DryRun: true, Seed: 7})
			if err != nil {
				t.Fatal(err)
			}
			if !res.DryRun || res.Rows != 1200 {
				t.Errorf("a dry run should still report its work: %+v", res)
			}
			for _, table := range []string{"users", "orders"} {
				if got := count(t, tg.raw, "SELECT COUNT(*) FROM "+table); got != 0 {
					t.Errorf("%s: a dry run wrote %d rows", table, got)
				}
			}
		})
	}
}

// Asking for more distinct values than a generator can produce is a failure
// before anything is written, not a constraint violation halfway through.
func TestImpossibleUniqueCountFailsBeforeWriting(t *testing.T) {
	for _, tg := range targets(t) {
		t.Run(tg.name, func(t *testing.T) {
			p := planFor(tg, 50, 0)
			// A boolean has two values, so fifty unique ones do not exist.
			p.Tables["users"].Columns["is_active"].Unique = true

			if _, err := seed.Run(t.Context(), tg.driver, tg.schema, p,
				seed.Options{Truncate: true, Seed: 7}); err == nil {
				t.Fatal("want an error for an impossible unique column")
			}
			if got := count(t, tg.raw, "SELECT COUNT(*) FROM users"); got != 0 {
				t.Errorf("a failed run left %d rows behind", got)
			}
		})
	}
}

// Seeding a child before its parent produces rows that point at nothing, so the
// order is derived from the foreign keys rather than from the config's order.
func TestParentsAreSeededFirst(t *testing.T) {
	for _, tg := range targets(t) {
		t.Run(tg.name, func(t *testing.T) {
			order, err := seed.Order(tg.schema, planFor(tg, 10, 10))
			if err != nil {
				t.Fatal(err)
			}
			at := map[string]int{}
			for i, tbl := range order {
				at[tbl.Name] = i
			}
			if at["users"] > at["orders"] {
				t.Fatalf("users must be seeded before orders: %v", order)
			}
		})
	}
}

func TestMain(m *testing.M) {
	code := m.Run()
	if code != 0 {
		fmt.Fprintln(os.Stderr,
			"note: Postgres tests need SEEDORA_TEST_POSTGRES to be set to a database they may destroy")
	}
	os.Exit(code)
}
