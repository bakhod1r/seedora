package postgres

import (
	"context"
	"net/url"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
)

// Two schemas holding a table of the same name must not merge into one table
// with both tables' columns and constraints. The table kept is the one an
// unqualified name resolves to.
func TestSameTableNameInTwoSchemas(t *testing.T) {
	dsn := os.Getenv("SEEDORA_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("SEEDORA_TEST_POSTGRES not set")
	}
	ctx := context.Background()
	// A database of its own: the end-to-end tests seed every table in the
	// shared one and run in parallel with this package.
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	const db = "seedora_schemas_test"
	_, _ = admin.Exec(ctx, "DROP DATABASE IF EXISTS "+db)
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+db); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(ctx, "DROP DATABASE IF EXISTS "+db+" WITH (FORCE)")
		_ = admin.Close(ctx)
	})
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Database = db
	if u, err := url.Parse(dsn); err == nil {
		u.Path = "/" + db
		dsn = u.String()
	}
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	setup := []string{
		`CREATE SCHEMA seedora_audit`,
		`CREATE TABLE public.sx_users (id int PRIMARY KEY, email text UNIQUE)`,
		`CREATE TABLE seedora_audit.sx_users (event_id bigint PRIMARY KEY, happened_at timestamptz, actor int)`,
	}
	for _, q := range setup {
		if _, err := conn.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	_ = conn.Close(ctx)

	d, err := open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close(ctx)
	s, err := d.Introspect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, tb := range s.Tables {
		if tb.Name != "sx_users" {
			continue
		}
		n++
		if tb.Schema != "public" {
			t.Errorf("kept schema %q, want public (the one on the search_path)", tb.Schema)
		}
		if len(tb.Columns) != 2 || tb.Column("event_id") != nil {
			t.Errorf("columns merged across schemas: %d columns", len(tb.Columns))
		}
		if len(tb.PrimaryKey) != 1 || tb.PrimaryKey[0] != "id" {
			t.Errorf("primary key = %v, want [id]", tb.PrimaryKey)
		}
	}
	if n != 1 {
		t.Fatalf("sx_users appears %d times", n)
	}
}
