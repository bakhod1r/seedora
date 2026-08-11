package tests

import (
	"testing"

	"github.com/bakhod1r/seedora/internal/seed"
)

// --append against every engine on the machine.
//
// This is here rather than only in internal/seed because the thing that can
// break it is per-driver: the values a unique column is read back as. SQLite
// hands back int64, pgx hands back int32 for an integer column, and MySQL hands
// back yet another shape. If those are not normalised, the preloaded set does
// not recognise the value the generator is about to duplicate, and the run
// fails on a constraint the tool exists to respect.
func TestAppendIsUniqueAgainstWhatIsAlreadyThere(t *testing.T) {
	for _, tg := range targets(t) {
		t.Run(tg.name, func(t *testing.T) {
			// The first run starts from empty, whatever a previous test left.
			if _, err := seed.Run(t.Context(), tg.driver, tg.schema, planFor(tg, 400, 800),
				seed.Options{Truncate: true, Seed: 21}); err != nil {
				t.Fatal(err)
			}

			// The same seed, which is what somebody re-running a command
			// produces and the case that collides.
			res, err := seed.Run(t.Context(), tg.driver, tg.schema, planFor(tg, 400, 800),
				seed.Options{Append: true, Seed: 21})
			if err != nil {
				t.Fatal(err)
			}
			if res.Rows != 1200 {
				t.Errorf("appended %d rows, want 1200", res.Rows)
			}

			if got := count(t, tg.raw, "SELECT COUNT(*) FROM users"); got != 800 {
				t.Errorf("users holds %d rows, want 800", got)
			}
			if got := count(t, tg.raw, "SELECT COUNT(DISTINCT email) FROM users"); got != 800 {
				t.Errorf("%d distinct emails across 800 rows: the append collided "+
					"with the rows already there", got)
			}
			if got := count(t, tg.raw, "SELECT COUNT(*) FROM orders"); got != 1600 {
				t.Errorf("orders holds %d rows, want 1600", got)
			}

			// Children written by the second run may point at parents from
			// either, and must point at one that exists.
			orphans := count(t, tg.raw, `SELECT COUNT(*) FROM orders o
				LEFT JOIN users u ON u.id = o.user_id WHERE u.id IS NULL`)
			if orphans != 0 {
				t.Errorf("%d orders point at no user", orphans)
			}
		})
	}
}

// --append leaves the rows already present alone. Emptying a table somebody
// asked to add to is the one failure that loses data.
func TestAppendDestroysNothing(t *testing.T) {
	for _, tg := range targets(t) {
		t.Run(tg.name, func(t *testing.T) {
			if _, err := seed.Run(t.Context(), tg.driver, tg.schema, planFor(tg, 100, 0),
				seed.Options{Truncate: true, Seed: 4}); err != nil {
				t.Fatal(err)
			}
			before := count(t, tg.raw, "SELECT COUNT(*) FROM users")

			// Even with the plan asking for truncation, which describes a run
			// that starts from empty.
			p := planFor(tg, 100, 0)
			p.Tables["users"].Truncate = true
			if _, err := seed.Run(t.Context(), tg.driver, tg.schema, p,
				seed.Options{Append: true, Seed: 5}); err != nil {
				t.Fatal(err)
			}

			if got := count(t, tg.raw, "SELECT COUNT(*) FROM users"); got != before+100 {
				t.Errorf("users holds %d rows, want %d — the append truncated", got, before+100)
			}
		})
	}
}
