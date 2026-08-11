package seed

import (
	"fmt"

	"github.com/bakhod1r/seedora/internal/model"
	"github.com/bakhod1r/seedora/internal/plan"
)

// A join table is a composite primary key made of foreign keys, and it is the
// one shape the per-column uniqueness pass cannot help with.
//
// `user_tags(user_id, tag_id)` constrains the *pair*: either column may repeat,
// and only the combination has to be new. Drawing both columns at random gives
// a birthday collision almost immediately — two hundred rows over two hundred
// parents each is a duplicate key with near certainty — and repairing one side
// of a pair means picking a different parent, which the uniqueness pass has no
// pool to pick from.
//
// So the pairs are assigned rather than drawn. Row i takes a distinct
// combination of the parent pools, walked as a mixed-radix number, which cannot
// collide by construction and needs no retries. Stepping through that space by
// a stride coprime with its size keeps the run reproducible and stops the first
// thousand children all landing on the first parent.
type composite struct {
	cols  []string
	pools [][]any
	// space is the number of distinct combinations, and stride walks them.
	space  int
	stride int
	offset int
}

// newComposite returns the assigner for a table whose key is drawn from the
// tables it points at, or nil when the table is anything else.
//
// Two shapes qualify, and they are the same problem. A composite primary key of
// foreign keys is a join table, where the *pair* must be new. A single foreign
// key marked unique is a one-to-one, where each parent may be used once — the
// per-column uniqueness pass cannot repair that either, because repairing a key
// means choosing a different parent, and it would otherwise invent a value that
// points at nothing.
//
// The narrowness is deliberate. A key with a generated column in it cannot be
// assigned this way — there is no pool to enumerate — and pretending otherwise
// would be a wrong answer rather than a missing one.
func newComposite(t *model.Table, tp *plan.TablePlan, fks map[string][]any, rows int, seed uint64) (*composite, error) {
	cols := keyColumns(t, tp)
	if len(cols) == 0 {
		return nil, nil
	}

	c := &composite{space: 1}
	for _, name := range cols {
		cp := tp.Get(name)
		if cp == nil || cp.Skip || cp.Generator != plan.GenForeignKey {
			return nil, nil
		}
		pool := fks[name]
		if len(pool) == 0 {
			return nil, nil
		}
		c.cols = append(c.cols, name)
		c.pools = append(c.pools, pool)
		c.space *= len(pool)
	}

	// More rows than combinations is not a plan that can be executed, and saying
	// so before anything is written beats a duplicate-key error from the engine
	// after a hundred thousand rows have gone in.
	if rows > c.space {
		sizes := make([]string, len(c.pools))
		for i, p := range c.pools {
			sizes[i] = fmt.Sprintf("%s: %d", c.cols[i], len(p))
		}
		what := "distinct combinations its key allows"
		if len(c.cols) == 1 {
			what = "parent rows it can point at, one each"
		}
		// The table's name is not repeated here: the caller already prefixes it,
		// and "profiles: profiles: 1000 rows…" reads like a bug.
		return nil, fmt.Errorf(
			"%d rows is more than the %d %s (%s) — "+
				"seed fewer rows here, or more rows in the tables it points at",
			rows, c.space, what, join(sizes, ", "))
	}

	// A stride coprime with the space visits every combination exactly once
	// before repeating, so the pairs are spread over the parents instead of
	// filling the first one. It comes from the seed, so a run reproduces.
	c.stride = coprimeStride(c.space, seed)
	c.offset = int(seed % uint64(c.space))
	return c, nil
}

// keyColumns returns the columns whose values must not repeat as a set: the
// composite primary key of a join table, or a single unique foreign key.
func keyColumns(t *model.Table, tp *plan.TablePlan) []string {
	if len(t.PrimaryKey) > 1 {
		return t.PrimaryKey
	}
	var out []string
	for _, c := range t.Columns {
		cp := tp.Get(c.Name)
		if cp == nil || cp.Skip || cp.Generator != plan.GenForeignKey {
			continue
		}
		// Unique in the plan or in the database: a one-to-one is either, and
		// the database's answer is the one that will be enforced.
		//
		// Being the primary key counts, and has to be asked separately.
		// Column.Unique is read from the unique indexes, and a single-column
		// primary key does not always have one to read — on SQLite an INTEGER
		// PRIMARY KEY is the rowid, so pragma_index_list reports nothing at
		// all. The column is still unique, and a foreign key that is also the
		// primary key is the most ordinary one-to-one there is.
		if cp.Unique || c.Unique || soleKey(t, c.Name) {
			out = append(out, c.Name)
		}
	}
	// One at a time. Two unique foreign keys on one table are two independent
	// constraints, not one over the pair, and each gets its own assigner on a
	// later pass — which is not built, so the first is handled and the second
	// is left to the uniqueness pass as before.
	if len(out) > 1 {
		return out[:1]
	}
	return out
}

// soleKey reports whether a column is the table's entire primary key, and so
// unique whether or not an index says so.
func soleKey(t *model.Table, col string) bool {
	return len(t.PrimaryKey) == 1 && t.PrimaryKey[0] == col
}

// assign writes row idx's combination over whatever the generator produced for
// the key columns.
func (c *composite) assign(row map[string]any, idx int) {
	n := (c.offset + idx*c.stride) % c.space
	for i := len(c.pools) - 1; i >= 0; i-- {
		pool := c.pools[i]
		row[c.cols[i]] = pool[n%len(pool)]
		n /= len(pool)
	}
}

// coprimeStride picks a step that shares no factor with the space, which is
// what makes stepping through it a permutation rather than a short cycle.
func coprimeStride(space int, seed uint64) int {
	if space <= 2 {
		return 1
	}
	start := int(seed%uint64(space-1)) + 1
	for i := 0; i < space; i++ {
		s := start + i
		if s >= space {
			s = s%(space-1) + 1
		}
		if gcd(s, space) == 1 {
			return s
		}
	}
	return 1
}

func gcd(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

func join(parts []string, sep string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += sep
		}
		out += p
	}
	return out
}
