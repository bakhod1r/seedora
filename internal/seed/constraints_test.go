package seed_test

import (
	"regexp"
	"testing"

	"github.com/bakhod1r/seedora/internal/plan"
	"github.com/bakhod1r/seedora/internal/seed"
)

// A CHECK constraint is often the only definition of what a column holds, and
// the generator that satisfies it has to come from the constraint itself: no
// name-based guess produces a value matching a regex it has never seen.
func TestPatternGeneratorSatisfiesARegexCheck(t *testing.T) {
	const ddl = `
CREATE TABLE accounts (
  id       INTEGER PRIMARY KEY,
  nickname VARCHAR(30) NOT NULL
);`
	d, s, raw := openWith(t, ddl)

	p := plan.Infer(s)
	p.Tables["accounts"].Rows = 500
	p.Tables["accounts"].Columns["nickname"] = &plan.ColumnPlan{
		Generator: plan.GenPattern,
		Pattern:   `^[a-zA-Z0-9_]{3,30}$`,
		Unique:    true,
	}

	if _, err := seed.Run(t.Context(), d, s, p, seed.Options{Seed: 4, Batch: 64}); err != nil {
		t.Fatal(err)
	}

	rows, err := raw.Query(`SELECT nickname FROM accounts`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	re := regexp.MustCompile(`^[a-zA-Z0-9_]{3,30}$`)
	n := 0
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		if !re.MatchString(v) {
			t.Fatalf("generated %q, which the CHECK would reject", v)
		}
		n++
	}
	if n != 500 {
		t.Errorf("wrote %d rows, want 500", n)
	}
}

// The shape the argon2id check has: literal text, escaped dollars, and bounded
// digit runs. It is here because it is the constraint that made the pattern
// generator necessary, and a generator that handles `[a-z]{3}` but not this one
// would not have helped.
func TestPatternHandlesALiteralHeavyExpression(t *testing.T) {
	const expr = `^\$argon2id\$v=\d+\$m=\d+,t=\d+,p=\d+\$`
	const ddl = `CREATE TABLE identity (id INTEGER PRIMARY KEY, secret TEXT NOT NULL);`

	d, s, raw := openWith(t, ddl)
	p := plan.Infer(s)
	p.Tables["identity"].Rows = 200
	p.Tables["identity"].Columns["secret"] = &plan.ColumnPlan{
		Generator: plan.GenPattern, Pattern: expr,
	}
	if _, err := seed.Run(t.Context(), d, s, p, seed.Options{Seed: 11, Batch: 32}); err != nil {
		t.Fatal(err)
	}

	re := regexp.MustCompile(expr)
	rows, err := raw.Query(`SELECT secret FROM identity`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		if !re.MatchString(v) {
			t.Fatalf("generated %q, which does not match %s", v, expr)
		}
	}
}

// A sequence that cannot start anywhere but 1 cannot seed a table whose ids
// live in an external space — a Telegram id, a shard's range.
func TestSequenceStartsWhereItIsTold(t *testing.T) {
	const ddl = `CREATE TABLE ids (n BIGINT NOT NULL);`
	d, s, raw := openWith(t, ddl)

	start := int64(800000001000)
	p := plan.Infer(s)
	p.Tables["ids"].Rows = 10
	p.Tables["ids"].Columns["n"] = &plan.ColumnPlan{Generator: plan.GenSequence, Start: &start}

	if _, err := seed.Run(t.Context(), d, s, p, seed.Options{Seed: 1, Batch: 4}); err != nil {
		t.Fatal(err)
	}

	var lo, hi int64
	if err := raw.QueryRow(`SELECT min(n), max(n) FROM ids`).Scan(&lo, &hi); err != nil {
		t.Fatal(err)
	}
	if lo != start || hi != start+9 {
		t.Errorf("sequence ran %d..%d, want %d..%d", lo, hi, start, start+9)
	}
}

// A unique foreign key is a one-to-one. Drawing at random gives one parent
// several children and most parents none, and then fails on the duplicate; the
// useful dataset is the one where every child has its own parent.
func TestUniqueForeignKeyIsOneToOne(t *testing.T) {
	const ddl = `
CREATE TABLE users (id INTEGER PRIMARY KEY, email VARCHAR(40) NOT NULL UNIQUE);
CREATE TABLE profiles (
  id      INTEGER PRIMARY KEY,
  user_id INTEGER NOT NULL UNIQUE REFERENCES users(id)
);`
	d, s, raw := openWith(t, ddl)

	p := plan.Infer(s)
	p.Tables["users"].Rows = 400
	p.Tables["profiles"].Rows = 400

	if _, err := seed.Run(t.Context(), d, s, p, seed.Options{Seed: 5, Batch: 64}); err != nil {
		t.Fatal(err)
	}

	var rows, distinct int
	if err := raw.QueryRow(`SELECT count(*), count(DISTINCT user_id) FROM profiles`).
		Scan(&rows, &distinct); err != nil {
		t.Fatal(err)
	}
	if rows != 400 || distinct != 400 {
		t.Errorf("%d profiles over %d distinct users, want 400 over 400", rows, distinct)
	}
}

// More children than parents cannot be a one-to-one, and saying so before the
// run is the difference between a sentence and a duplicate-key error several
// hundred thousand rows into a load.
func TestUniqueForeignKeyRefusesMoreChildrenThanParents(t *testing.T) {
	const ddl = `
CREATE TABLE users (id INTEGER PRIMARY KEY, email VARCHAR(40) NOT NULL UNIQUE);
CREATE TABLE profiles (
  id      INTEGER PRIMARY KEY,
  user_id INTEGER NOT NULL UNIQUE REFERENCES users(id)
);`
	d, s, _ := openWith(t, ddl)

	p := plan.Infer(s)
	p.Tables["users"].Rows = 10
	p.Tables["profiles"].Rows = 50

	_, err := seed.Run(t.Context(), d, s, p, seed.Options{Seed: 5, Batch: 8})
	if err == nil {
		t.Fatal("seeded 50 one-to-one children onto 10 parents without complaint")
	}
}

// Committing per table is what makes a very large run survivable, and the rows
// still have to land — all of them, with the children still pointing at parents
// that are now committed rather than pending.
func TestTxPerTableWritesEverything(t *testing.T) {
	d, s, raw := open(t)

	p := plan.Infer(s)
	p.Tables["users"].Rows = 200
	p.Tables["orders"].Rows = 500

	if _, err := seed.Run(t.Context(), d, s, p, seed.Options{
		Seed: 9, Batch: 64, TxPerTable: true,
	}); err != nil {
		t.Fatal(err)
	}

	var users, orders, orphans int
	if err := raw.QueryRow(`SELECT count(*) FROM users`).Scan(&users); err != nil {
		t.Fatal(err)
	}
	if err := raw.QueryRow(`SELECT count(*) FROM orders`).Scan(&orders); err != nil {
		t.Fatal(err)
	}
	if err := raw.QueryRow(
		`SELECT count(*) FROM orders o LEFT JOIN users u ON u.id = o.user_id WHERE u.id IS NULL`,
	).Scan(&orphans); err != nil {
		t.Fatal(err)
	}
	if users != 200 || orders != 500 {
		t.Errorf("wrote %d users and %d orders, want 200 and 500", users, orders)
	}
	if orphans != 0 {
		t.Errorf("%d orders point at users that do not exist", orphans)
	}
}
