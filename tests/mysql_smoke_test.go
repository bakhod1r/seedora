package tests

import (
	"context"
	"os"
	"testing"

	"github.com/bakhod1r/seedora/internal/db"
	"github.com/bakhod1r/seedora/internal/ddl"
	"github.com/bakhod1r/seedora/internal/plan"
	"github.com/bakhod1r/seedora/internal/seed"
)

func TestMySQLSmoke(t *testing.T) {
	dsn := os.Getenv("SEEDORA_TEST_MYSQL")
	if dsn == "" {
		t.Skip("no mysql")
	}
	ctx := context.Background()
	d, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close(ctx)
	t.Log("engine", d.Name(), "dialect", d.Dialect())

	if tx, err := d.Begin(ctx); err == nil {
		_ = tx.Exec(ctx, "DROP TABLE IF EXISTS `invoices`")
		_ = tx.Commit(ctx)
	}
	s, err := d.Introspect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// schema editor path: create a table with an enum-ish + json + fk column
	changes := []ddl.Change{{Kind: ddl.CreateTable, Table: "invoices", Columns: []ddl.Column{
		{Name: "id", Type: "BIGINT AUTO_INCREMENT", PK: true},
		{Name: "user_id", Type: "BIGINT", References: "users.id"},
		{Name: "status", Type: "ENUM('draft','sent','paid')"},
		{Name: "meta", Type: "JSON", Nullable: true},
		{Name: "code", Type: "VARCHAR(20)", Unique: true},
		{Name: "amount", Type: "DECIMAL(10,2)"},
		{Name: "created_at", Type: "DATETIME"},
	}}}
	stmts, err := ddl.Plan(d.Dialect(), s, changes)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := d.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range stmts {
		t.Log(st)
		if err := tx.Exec(ctx, st); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	s2, err := d.Introspect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tb := s2.Table("invoices")
	if tb == nil {
		t.Fatal("invoices not introspected")
	}
	for _, c := range tb.Columns {
		t.Logf("%s type=%s native=%s null=%v def=%v gen=%v uniq=%v enum=%s fk=%v",
			c.Name, c.Type, c.Native, c.Nullable, c.HasDefault, c.Generated, c.Unique, c.EnumType, c.FK)
	}
	t.Log("enums:", s2.Enums)
	t.Log("pk:", tb.PrimaryKey)

	p := plan.Infer(s2)
	p.Tables["users"].Rows = 100
	p.Tables["orders"].Rows = 50
	p.Tables["invoices"].Rows = 250
	res, err := seed.Run(ctx, d, s2, p, seed.Options{Truncate: true, Seed: 3})
	if err != nil {
		t.Fatal(err)
	}
	t.Log("seeded", res.Rows)

	rows, cols, err := seed.Preview(ctx, d, s2, p, "invoices", 3, "en_US", 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Log(cols)
	for _, r := range rows {
		t.Log(r)
	}
	h, err := d.History(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Log("history entries:", len(h))
	script := ddl.Script(d.Dialect(), s2)
	for _, st := range script {
		t.Log(st)
	}
}
