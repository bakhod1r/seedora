package seed_test

import (
	"testing"

	"github.com/bakhod1r/seedora/internal/plan"
	"github.com/bakhod1r/seedora/internal/seed"
)

// The point of --append: a second run adds to what the first one wrote instead
// of assuming an empty table, and the unique column survives it. Without the
// preload the second run's emails are drawn from the same seeded space as the
// first and the UNIQUE index rejects the insert.
func TestAppendAddsToAPopulatedTable(t *testing.T) {
	d, s, raw := open(t)

	p := plan.Infer(s)
	p.Tables["users"].Rows = 300
	p.Tables["orders"].Rows = 0

	ctx := t.Context()
	if _, err := seed.Run(ctx, d, s, p, seed.Options{Seed: 7, Batch: 64}); err != nil {
		t.Fatal(err)
	}

	// The same seed, deliberately: it is the case that collides, and the case
	// somebody re-running a command actually produces.
	res, err := seed.Run(ctx, d, s, p, seed.Options{Seed: 7, Batch: 64, Append: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Rows != 300 {
		t.Errorf("appended %d rows, want 300", res.Rows)
	}

	var total, distinctEmails, distinctIDs int
	if err := raw.QueryRow(`SELECT count(*), count(DISTINCT email), count(DISTINCT id) FROM users`).
		Scan(&total, &distinctEmails, &distinctIDs); err != nil {
		t.Fatal(err)
	}
	if total != 600 {
		t.Errorf("users holds %d rows, want 600", total)
	}
	if distinctEmails != 600 {
		t.Errorf("%d distinct emails across %d rows: the append collided with the "+
			"rows already there", distinctEmails, total)
	}
	if distinctIDs != 600 {
		t.Errorf("%d distinct ids across %d rows", distinctIDs, total)
	}
}

// The other half of the same fact, and the reason the flag exists: without it
// a second run into a populated table is a constraint violation, because
// uniqueness is enforced within the run and nothing tells it what is already
// there. If this ever stops failing, the test above has stopped proving
// anything.
func TestASecondRunWithoutAppendCollides(t *testing.T) {
	d, s, _ := open(t)

	p := plan.Infer(s)
	p.Tables["users"].Rows = 300
	p.Tables["orders"].Rows = 0

	ctx := t.Context()
	if _, err := seed.Run(ctx, d, s, p, seed.Options{Seed: 7, Batch: 64}); err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Run(ctx, d, s, p, seed.Options{Seed: 7, Batch: 64}); err == nil {
		t.Fatal("expected the unique constraint to reject the second run")
	}
}

// An integer unique column is the case the row index cannot repair, because
// the index restarts at zero on every run and those values are taken.
func TestAppendRepairsIntegerCollisionsPastTheExistingMaximum(t *testing.T) {
	d, _, raw := open(t)

	if _, err := raw.Exec(`CREATE TABLE tickets (
		id INTEGER PRIMARY KEY,
		code INTEGER NOT NULL UNIQUE
	)`); err != nil {
		t.Fatal(err)
	}
	// Values a fresh run's row indices would land on.
	for i := range 50 {
		if _, err := raw.Exec(`INSERT INTO tickets (code) VALUES (?)`, i); err != nil {
			t.Fatal(err)
		}
	}

	ctx := t.Context()
	s2, err := d.Introspect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	p := plan.Infer(s2)
	for name, tp := range p.Tables {
		tp.Rows = 0
		if name == "tickets" {
			tp.Rows = 50
		}
	}

	if _, err := seed.Run(ctx, d, s2, p, seed.Options{Seed: 3, Batch: 16, Append: true}); err != nil {
		t.Fatal(err)
	}

	var total, distinct int
	if err := raw.QueryRow(`SELECT count(*), count(DISTINCT code) FROM tickets`).
		Scan(&total, &distinct); err != nil {
		t.Fatal(err)
	}
	if total != 100 || distinct != 100 {
		t.Errorf("tickets: %d rows, %d distinct codes, want 100 and 100", total, distinct)
	}
}

// --append must not empty anything, including a table the plan says to
// truncate. The plan's flag describes a run that starts from empty.
func TestAppendIgnoresTheTruncateInThePlan(t *testing.T) {
	d, s, raw := open(t)

	p := plan.Infer(s)
	p.Tables["users"].Rows = 100
	p.Tables["orders"].Rows = 0

	ctx := t.Context()
	if _, err := seed.Run(ctx, d, s, p, seed.Options{Seed: 11, Batch: 32}); err != nil {
		t.Fatal(err)
	}

	p.Tables["users"].Truncate = true
	if _, err := seed.Run(ctx, d, s, p, seed.Options{Seed: 12, Batch: 32, Append: true}); err != nil {
		t.Fatal(err)
	}

	var total int
	if err := raw.QueryRow(`SELECT count(*) FROM users`).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != 200 {
		t.Errorf("users holds %d rows, want 200 — the append truncated", total)
	}
}

// The two flags describe opposite intentions about the rows already present,
// so the run is refused rather than resolved by precedence.
func TestAppendAndTruncateAreRefusedTogether(t *testing.T) {
	d, s, _ := open(t)
	p := plan.Infer(s)

	_, err := seed.Run(t.Context(), d, s, p, seed.Options{Append: true, Truncate: true})
	if err == nil {
		t.Fatal("expected a refusal")
	}
}

// A child appended to must point at parents that exist, including the ones an
// earlier run wrote — which is already how foreign keys resolve, and this holds
// it to it.
func TestAppendedChildrenPointAtExistingParents(t *testing.T) {
	d, s, raw := open(t)

	p := plan.Infer(s)
	p.Tables["users"].Rows = 50
	p.Tables["orders"].Rows = 100

	ctx := t.Context()
	if _, err := seed.Run(ctx, d, s, p, seed.Options{Seed: 5, Batch: 32}); err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Run(ctx, d, s, p, seed.Options{Seed: 6, Batch: 32, Append: true}); err != nil {
		t.Fatal(err)
	}

	var orphans int
	if err := raw.QueryRow(`SELECT count(*) FROM orders o
		LEFT JOIN users u ON u.id = o.user_id WHERE u.id IS NULL`).Scan(&orphans); err != nil {
		t.Fatal(err)
	}
	if orphans != 0 {
		t.Errorf("%d orders point at no user", orphans)
	}
}
