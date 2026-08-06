package seed

import (
	"fmt"
	"strings"
	"testing"

	"github.com/bakhod1r/seedora/internal/model"
	"github.com/bakhod1r/seedora/internal/plan"
)

// joinTable is `user_tags(user_id, tag_id)`: the shape the diagram produces for
// a many-to-many, and the one a per-column uniqueness pass cannot help with.
func joinTable() (*model.Table, *plan.TablePlan) {
	t := &model.Table{
		Name:       "user_tags",
		PrimaryKey: []string{"user_id", "tag_id"},
		Columns: []*model.Column{
			{Name: "user_id", Type: "bigint", FK: &model.Ref{Table: "users", Column: "id"}},
			{Name: "tag_id", Type: "bigint", FK: &model.Ref{Table: "tags", Column: "id"}},
		},
	}
	tp := &plan.TablePlan{Columns: map[string]*plan.ColumnPlan{
		"user_id": {Generator: plan.GenForeignKey, References: "users.id"},
		"tag_id":  {Generator: plan.GenForeignKey, References: "tags.id"},
	}}
	return t, tp
}

func pool(n int) []any {
	out := make([]any, n)
	for i := range out {
		out[i] = int64(i + 1)
	}
	return out
}

// Every row gets a pair no other row has. Drawing both columns at random
// collides almost immediately at these sizes, which is the reason this exists.
func TestCompositeAssignsDistinctPairs(t *testing.T) {
	tb, tp := joinTable()
	fks := map[string][]any{"user_id": pool(20), "tag_id": pool(10)}

	c, err := newComposite(tb, tp, fks, 200, 99)
	if err != nil {
		t.Fatal(err)
	}
	if c == nil {
		t.Fatal("no assigner for a join table")
	}

	seen := map[string]bool{}
	for i := range 200 {
		row := map[string]any{}
		c.assign(row, i)
		k := fmt.Sprint(row["user_id"], "-", row["tag_id"])
		if seen[k] {
			t.Fatalf("row %d repeats the pair %s", i, k)
		}
		seen[k] = true
	}
	if len(seen) != 200 {
		t.Errorf("got %d distinct pairs, want 200", len(seen))
	}
}

// The pairs are spread over the parents rather than filling the first one,
// which is what the coprime stride buys.
func TestCompositeSpreadsOverParents(t *testing.T) {
	tb, tp := joinTable()
	c, err := newComposite(tb, tp, map[string][]any{
		"user_id": pool(50), "tag_id": pool(50),
	}, 100, 7)
	if err != nil {
		t.Fatal(err)
	}

	users := map[any]bool{}
	for i := range 100 {
		row := map[string]any{}
		c.assign(row, i)
		users[row["user_id"]] = true
	}
	// A naive walk would put all hundred rows on the first two users.
	if len(users) < 10 {
		t.Errorf("100 rows landed on %d users — they are clustered", len(users))
	}
}

// The same seed produces the same pairs, and a different one does not. A join
// table has to reproduce like every other table.
func TestCompositeIsReproducible(t *testing.T) {
	tb, tp := joinTable()
	fks := map[string][]any{"user_id": pool(30), "tag_id": pool(30)}

	run := func(seed uint64) []string {
		c, err := newComposite(tb, tp, fks, 50, seed)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for i := range 50 {
			row := map[string]any{}
			c.assign(row, i)
			out = append(out, fmt.Sprint(row["user_id"], "-", row["tag_id"]))
		}
		return out
	}

	a, b, c := run(11), run(11), run(12)
	if strings.Join(a, ",") != strings.Join(b, ",") {
		t.Error("the same seed produced different pairs")
	}
	if strings.Join(a, ",") == strings.Join(c, ",") {
		t.Error("a different seed produced the same pairs")
	}
}

// More rows than pairs cannot be done, and the run has to say so before it
// writes anything rather than failing on a duplicate key part way in.
func TestCompositeRefusesMoreRowsThanPairs(t *testing.T) {
	tb, tp := joinTable()
	_, err := newComposite(tb, tp, map[string][]any{
		"user_id": pool(10), "tag_id": pool(10),
	}, 500, 1)
	if err == nil {
		t.Fatal("500 rows over 100 possible pairs was accepted")
	}
	for _, want := range []string{"500 rows", "100 distinct combinations", "user_id: 10"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}
}

// Anything that is not a composite key of foreign keys is left alone: there is
// no pool to enumerate, and guessing would be a wrong answer rather than a
// missing one.
func TestCompositeIgnoresEverythingElse(t *testing.T) {
	tb, tp := joinTable()
	fks := map[string][]any{"user_id": pool(10), "tag_id": pool(10)}

	single := &model.Table{Name: "users", PrimaryKey: []string{"id"}}
	if c, err := newComposite(single, tp, fks, 10, 1); c != nil || err != nil {
		t.Error("a single-column key got an assigner")
	}

	// A composite key with a generated column in it.
	tp.Columns["tag_id"] = &plan.ColumnPlan{Generator: "int"}
	if c, err := newComposite(tb, tp, fks, 10, 1); c != nil || err != nil {
		t.Error("a composite key that is not all foreign keys got an assigner")
	}
}

// A one-to-one is a single foreign key that may use each parent once. Repairing
// a collision the ordinary way would invent a key that points at nothing, so
// this shape is assigned too.
func TestCompositeAssignsAUniqueForeignKey(t *testing.T) {
	tb := &model.Table{
		Name:       "profiles",
		PrimaryKey: []string{"id"},
		Columns: []*model.Column{
			{Name: "id", Type: "bigint"},
			{Name: "user_id", Type: "bigint", Unique: true,
				FK: &model.Ref{Table: "users", Column: "id"}},
		},
	}
	tp := &plan.TablePlan{Columns: map[string]*plan.ColumnPlan{
		"id":      {Generator: plan.GenDefault, Skip: true},
		"user_id": {Generator: plan.GenForeignKey, References: "users.id", Unique: true},
	}}
	parents := pool(40)

	c, err := newComposite(tb, tp, map[string][]any{"user_id": parents}, 40, 5)
	if err != nil {
		t.Fatal(err)
	}
	if c == nil {
		t.Fatal("no assigner for a unique foreign key")
	}

	seen := map[any]bool{}
	for i := range 40 {
		row := map[string]any{}
		c.assign(row, i)
		v := row["user_id"]
		if seen[v] {
			t.Fatalf("row %d reuses parent %v", i, v)
		}
		// Every value has to be a key that exists; the ordinary repair path
		// would have produced the row index instead.
		if !contains(parents, v) {
			t.Fatalf("row %d points at %v, which is not a parent key", i, v)
		}
		seen[v] = true
	}

	// One more row than there are parents cannot be one-to-one.
	if _, err := newComposite(tb, tp, map[string][]any{"user_id": parents}, 41, 5); err == nil {
		t.Error("41 rows over 40 parents was accepted")
	}
}

func contains(pool []any, v any) bool {
	for _, p := range pool {
		if p == v {
			return true
		}
	}
	return false
}
