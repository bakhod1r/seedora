// Package benchmarks measures the parts of Seedora whose speed is a
// requirement rather than a nicety.
//
// Generation is the one that matters. A bulk load is one statement and the
// database's problem; generation runs on Seedora's goroutines and has to stay
// ahead of the wire, or a COPY that could have been saturated ends up waiting
// on Go.
package benchmarks

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/bakhod1r/seedora/internal/db"
	_ "github.com/bakhod1r/seedora/internal/db/sqlite"
	"github.com/bakhod1r/seedora/internal/plan"
	"github.com/bakhod1r/seedora/internal/seed"
)

const schemaSQL = `
CREATE TABLE users (
  id         INTEGER PRIMARY KEY,
  email      VARCHAR(120) NOT NULL UNIQUE,
  first_name VARCHAR(50) NOT NULL,
  last_name  VARCHAR(50) NOT NULL,
  city       VARCHAR(60),
  phone      VARCHAR(30),
  company    VARCHAR(80),
  balance    DECIMAL(10,2) NOT NULL,
  is_active  BOOLEAN NOT NULL,
  created_at TIMESTAMP NOT NULL
);

CREATE TABLE orders (
  id        INTEGER PRIMARY KEY,
  user_id   INTEGER NOT NULL REFERENCES users(id),
  status    VARCHAR(20) NOT NULL,
  total     DECIMAL(10,2) NOT NULL,
  placed_at TIMESTAMP NOT NULL
);
`

func open(tb testing.TB) (db.Driver, *plan.Plan) {
	tb.Helper()

	path := filepath.Join(tb.TempDir(), "bench.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		tb.Fatal(err)
	}
	if _, err := raw.Exec(schemaSQL); err != nil {
		tb.Fatal(err)
	}
	raw.Close()

	ctx := context.Background()
	d, err := db.Open(ctx, path)
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = d.Close(context.Background()) })

	s, err := d.Introspect(ctx)
	if err != nil {
		tb.Fatal(err)
	}
	return d, plan.Infer(s)
}

// BenchmarkGenerate measures generation with the database taken out of the
// picture: a dry run builds and validates every row and writes none.
func BenchmarkGenerate(b *testing.B) {
	for _, rows := range []int{1_000, 10_000, 100_000} {
		b.Run(fmt.Sprintf("rows=%d", rows), func(b *testing.B) {
			d, p := open(b)
			s, err := d.Introspect(context.Background())
			if err != nil {
				b.Fatal(err)
			}
			p.Tables["users"].Rows = rows
			p.Tables["orders"].Rows = 0

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				res, err := seed.Run(context.Background(), d, s, p, seed.Options{
					DryRun: true,
					Seed:   42,
				})
				if err != nil {
					b.Fatal(err)
				}
				if res.Rows == 0 {
					b.Fatal("nothing was generated")
				}
			}
			b.SetBytes(int64(rows))
			b.ReportMetric(float64(rows*b.N)/b.Elapsed().Seconds(), "rows/s")
		})
	}
}

// BenchmarkInsertSQLite measures the write path. SQLite has no bulk protocol,
// so this is the floor: a prepared multi-row INSERT inside one transaction.
func BenchmarkInsertSQLite(b *testing.B) {
	d, p := open(b)
	s, err := d.Introspect(context.Background())
	if err != nil {
		b.Fatal(err)
	}
	const rows = 20_000
	p.Tables["users"].Rows = rows
	p.Tables["orders"].Rows = 0

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := seed.Run(context.Background(), d, s, p, seed.Options{
			Truncate: true,
			Seed:     42,
		}); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(rows*b.N)/b.Elapsed().Seconds(), "rows/s")
}
