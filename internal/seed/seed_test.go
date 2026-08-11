package seed_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/bakhod1r/seedora/internal/db"
	_ "github.com/bakhod1r/seedora/internal/db/sqlite"
	"github.com/bakhod1r/seedora/internal/model"
	"github.com/bakhod1r/seedora/internal/plan"
	"github.com/bakhod1r/seedora/internal/seed"
)

// schemaSQL is deliberately awkward: a serial primary key, a unique column with
// a tight length limit, a nullable column, an enum-shaped text column, and a
// foreign key that forces an insertion order.
const schemaSQL = `
CREATE TABLE users (
  id         INTEGER PRIMARY KEY,
  email      VARCHAR(30) NOT NULL UNIQUE,
  first_name VARCHAR(50) NOT NULL,
  city       VARCHAR(60),
  is_active  BOOLEAN NOT NULL,
  created_at TIMESTAMP NOT NULL
);

CREATE TABLE orders (
  id      INTEGER PRIMARY KEY,
  user_id INTEGER NOT NULL REFERENCES users(id),
  status  VARCHAR(20) NOT NULL,
  total   DECIMAL(10,2) NOT NULL
);
`

// open builds a fresh database with the test schema and returns a driver, the
// introspected schema, and a raw handle the assertions query through — the
// driver interface deliberately has no general-purpose query, so checking what
// landed needs a second connection.
func open(t *testing.T) (db.Driver, *model.Schema, *sql.DB) {
	t.Helper()
	return openWith(t, schemaSQL)
}

// openWith is open against a schema of the test's choosing, for the shapes the
// standard two tables do not have.
func openWith(t *testing.T, ddl string) (db.Driver, *model.Schema, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(ddl); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { raw.Close() })

	ctx := t.Context()
	d, err := db.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close(context.Background()) })

	s, err := d.Introspect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return d, s, raw
}

func TestIntrospectFindsStructure(t *testing.T) {
	_, s, _ := open(t)

	users := s.Table("users")
	if users == nil {
		t.Fatal("users table not found")
	}
	if got := len(users.Columns); got != 6 {
		t.Errorf("users has %d columns, want 6", got)
	}
	if got := users.PrimaryKey; len(got) != 1 || got[0] != "id" {
		t.Errorf("users primary key = %v, want [id]", got)
	}
	if e := users.Column("email"); e == nil || !e.Unique {
		t.Error("users.email should be unique")
	}
	if c := users.Column("city"); c == nil || !c.Nullable {
		t.Error("users.city should be nullable")
	}
	if c := users.Column("email"); c == nil || c.MaxLen != 30 {
		t.Errorf("users.email max length = %d, want 30", c.MaxLen)
	}

	orders := s.Table("orders")
	fk := orders.Column("user_id")
	if fk == nil || fk.FK == nil {
		t.Fatal("orders.user_id should carry a foreign key")
	}
	if fk.FK.Table != "users" || fk.FK.Column != "id" {
		t.Errorf("orders.user_id references %v, want users.id", fk.FK)
	}
}

func TestInferProposesFromNames(t *testing.T) {
	_, s, _ := open(t)
	p := plan.Infer(s)

	users := p.Tables["users"]
	want := map[string]string{
		"email":      "email",
		"first_name": "firstname",
		"city":       "city",
		"user_id":    plan.GenForeignKey,
	}
	for col, gen := range want {
		tp := users
		if col == "user_id" {
			tp = p.Tables["orders"]
		}
		got := tp.Columns[col]
		if got == nil {
			t.Fatalf("no plan for %s", col)
		}
		if got.Generator != gen {
			t.Errorf("%s generator = %q, want %q", col, got.Generator, gen)
		}
	}

	// A serial primary key must be left to the database, or its sequence and
	// its rows fall out of step.
	if id := users.Columns["id"]; id == nil || !id.Skip {
		t.Error("users.id should be skipped so the database assigns it")
	}
	if e := users.Columns["email"]; e == nil || !e.Unique {
		t.Error("users.email should inherit the unique constraint")
	}
}

func TestOrderPutsParentsFirst(t *testing.T) {
	_, s, _ := open(t)
	p := plan.Infer(s)

	order, err := seed.Order(s, p)
	if err != nil {
		t.Fatal(err)
	}
	pos := map[string]int{}
	for i, tb := range order {
		pos[tb.Name] = i
	}
	if pos["users"] > pos["orders"] {
		t.Errorf("users must be seeded before orders, got %v", order)
	}
}

func TestRunWritesRowsAndHonoursForeignKeys(t *testing.T) {
	d, s, raw := open(t)
	p := plan.Infer(s)
	p.Tables["users"].Rows = 500
	p.Tables["orders"].Rows = 2000

	ctx := t.Context()
	res, err := seed.Run(ctx, d, s, p, seed.Options{Seed: 42, Batch: 128})
	if err != nil {
		t.Fatal(err)
	}
	if res.Rows != 2500 {
		t.Errorf("wrote %d rows, want 2500", res.Rows)
	}

	// Every child must point at a parent that exists, and uniqueness must have
	// survived being generated across many batches on many goroutines.
	assertCount(t, raw, "SELECT COUNT(*) FROM users", 500)
	assertCount(t, raw, "SELECT COUNT(DISTINCT email) FROM users", 500)
	assertCount(t, raw, "SELECT COUNT(*) FROM orders", 2000)
	assertCount(t, raw,
		"SELECT COUNT(*) FROM orders o LEFT JOIN users u ON u.id = o.user_id WHERE u.id IS NULL", 0)
	// The unique column is 30 characters wide; a repaired collision must still fit.
	assertCount(t, raw, "SELECT COUNT(*) FROM users WHERE LENGTH(email) > 30", 0)
}

func TestSameSeedProducesSameRows(t *testing.T) {
	first := runOnce(t, 7)
	second := runOnce(t, 7)
	if first != second {
		t.Errorf("same seed produced different data:\n %q\n %q", first, second)
	}

	third := runOnce(t, 8)
	if third == first {
		t.Error("different seeds produced identical data")
	}
}

// runOnce seeds a fresh database and returns a fingerprint of what landed in it.
func runOnce(t *testing.T, seedVal uint64) string {
	t.Helper()
	d, s, raw := open(t)
	_ = raw
	p := plan.Infer(s)
	p.Tables["users"].Rows = 200
	p.Tables["orders"].Rows = 400

	if _, err := seed.Run(t.Context(), d, s, p, seed.Options{Seed: seedVal, Batch: 64}); err != nil {
		t.Fatal(err)
	}

	tx, err := d.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())

	keys, err := tx.ReadKeys(t.Context(), s.Table("users"), "email", 1000)
	if err != nil {
		t.Fatal(err)
	}
	out := ""
	for _, k := range keys {
		out += toString(k) + "\n"
	}
	return out
}

func TestDryRunWritesNothing(t *testing.T) {
	d, s, raw := open(t)
	p := plan.Infer(s)
	p.Tables["users"].Rows = 100
	p.Tables["orders"].Rows = 100

	res, err := seed.Run(t.Context(), d, s, p, seed.Options{Seed: 1, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Rows != 200 {
		t.Errorf("dry run generated %d rows, want 200", res.Rows)
	}
	assertCount(t, raw, "SELECT COUNT(*) FROM users", 0)
	assertCount(t, raw, "SELECT COUNT(*) FROM orders", 0)
}

func TestValidateCatchesDrift(t *testing.T) {
	_, s, _ := open(t)
	p := plan.Infer(s)

	p.Tables["ghosts"] = &plan.TablePlan{Rows: 1, Columns: map[string]*plan.ColumnPlan{}}
	p.Tables["users"].Columns["nonexistent"] = &plan.ColumnPlan{Generator: "email"}
	p.Tables["users"].Columns["first_name"].NullRate = 0.5

	problems := p.Validate(s)
	if len(problems) < 3 {
		t.Fatalf("expected at least 3 problems, got %d: %v", len(problems), problems)
	}
}

func TestPreviewTouchesNothing(t *testing.T) {
	d, s, raw := open(t)
	p := plan.Infer(s)

	rows, cols, err := seed.Preview(t.Context(), d, s, p, "users", 5, "en_US", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 5 {
		t.Fatalf("preview returned %d rows, want 5", len(rows))
	}
	for _, c := range cols {
		if c == "id" {
			t.Error("preview should not include the auto-assigned key")
		}
	}
	if _, ok := rows[0]["email"]; !ok {
		t.Error("preview rows should carry the email column")
	}
	assertCount(t, raw, "SELECT COUNT(*) FROM users", 0)
}

// A child previewed against an empty parent must still show keys: a column of
// NULLs says nothing about what a run will produce, and the parent is about to
// be seeded too.
func TestPreviewFillsForeignKeysFromAnEmptyParent(t *testing.T) {
	d, s, _ := open(t)
	p := plan.Infer(s)

	rows, _, err := seed.Preview(t.Context(), d, s, p, "orders", 5, "en_US", 0)
	if err != nil {
		t.Fatal(err)
	}
	for i, row := range rows {
		v, ok := row["user_id"]
		if !ok {
			t.Fatalf("row %d has no user_id", i)
		}
		if v == nil {
			t.Fatalf("row %d: user_id is NULL, so the preview shows no relationship", i)
		}
	}
}

func assertCount(t *testing.T, raw *sql.DB, query string, want int64) {
	t.Helper()
	var got int64
	if err := raw.QueryRow(query).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("%s = %d, want %d", query, got, want)
	}
}

func toString(v any) string {
	switch s := v.(type) {
	case string:
		return s
	case []byte:
		return string(s)
	}
	return ""
}
