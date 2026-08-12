package clickhouse

import (
	"context"
	"errors"
	"iter"
	"os"
	"testing"
	"time"

	"github.com/bakhod1r/seedora/internal/db"
)

// TestLive exercises the driver against a real ClickHouse. It needs a server,
// so it runs only when a DSN names one — the same bargain the engine tests
// under tests/ strike. Skip when unset, but Fatal rather than Skip once a DSN
// is given: somebody who sets it wants this run, and skipping on a bad
// connection is how a suite stays green without ever having executed.
func TestLive(t *testing.T) {
	dsn := os.Getenv("SEEDORA_TEST_CLICKHOUSE")
	if dsn == "" {
		t.Skip("set SEEDORA_TEST_CLICKHOUSE to a ClickHouse this test may destroy")
	}
	ctx := context.Background()
	d, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close(ctx)

	tx0, _ := d.Begin(ctx)
	for _, stmt := range []string{
		`DROP TABLE IF EXISTS users`,
		`CREATE TABLE users (
			id UInt64,
			email String,
			score Nullable(Float64),
			status Enum8('draft' = 1, 'sent' = 2),
			created DateTime64(3),
			tag LowCardinality(String),
			amount Decimal(10,2),
			note FixedString(8) DEFAULT 'x',
			doubled UInt64 MATERIALIZED id * 2
		) ENGINE = MergeTree ORDER BY id`,
	} {
		if err := tx0.Exec(ctx, stmt); err != nil {
			t.Fatal(stmt, err)
		}
	}

	s, err := d.Introspect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, tb := range s.Tables {
		t.Logf("table %s rows=%d pk=%v", tb.Name, tb.ExistingRows, tb.PrimaryKey)
		for _, c := range tb.Columns {
			t.Logf("  %-8s type=%-9s native=%-22s null=%v def=%v gen=%v enum=%q len=%d p=%d s=%d",
				c.Name, c.Type, c.Native, c.Nullable, c.HasDefault, c.Generated, c.EnumType, c.MaxLen, c.Precision, c.Scale)
		}
	}
	t.Logf("enums: %v", s.Enums)

	tb := s.Table("users")
	if tb == nil {
		t.Fatal("no users table")
	}

	tx, err := d.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Truncate(ctx, tb); err != nil {
		t.Fatal(err)
	}
	rows := db.SliceSource{}
	for i := 1; i <= 2500; i++ {
		rows = append(rows, map[string]any{
			"id": int64(i), "email": "a@b.c", "score": 1.5, "status": "draft",
			"created": time.Now().UTC(), "tag": "t", "amount": 1.25,
		})
	}
	n, err := tx.Insert(ctx, tb, []string{"id", "email", "score", "status", "created", "tag", "amount"}, rows)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("inserted %d", n)

	keys, err := tx.ReadKeys(ctx, tb, "id", 3)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("keys %v", keys)

	if err := tx.Rollback(ctx); err == nil {
		t.Fatal("Rollback returned nil after writes")
	} else {
		t.Logf("Rollback said: %v", err)
	}

	// A transaction that wrote nothing rolls back cleanly.
	tx2, _ := d.Begin(ctx)
	if err := tx2.Rollback(ctx); err != nil {
		t.Fatal("empty rollback:", err)
	}
	// Rollback after Commit is a no-op.
	tx3, _ := d.Begin(ctx)
	if err := tx3.Truncate(ctx, tb); err != nil {
		t.Fatal(err)
	}
	if err := tx3.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := tx3.Rollback(ctx); err != nil {
		t.Fatal("rollback after commit:", err)
	}

	// Source.Err must surface.
	tx4, _ := d.Begin(ctx)
	if _, err := tx4.Insert(ctx, tb, []string{"id"}, badSource{}); err == nil {
		t.Fatal("expected generator error")
	} else {
		t.Logf("generator error: %v", err)
	}

	h, err := d.History(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("history: %v", h)
}

type badSource struct{}

func (badSource) Rows() iter.Seq[map[string]any] {
	return func(yield func(map[string]any) bool) {
		yield(map[string]any{"id": int64(1)})
	}
}
func (badSource) Err() error { return errors.New("generator blew up") }
