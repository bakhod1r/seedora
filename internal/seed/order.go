package seed

import (
	"fmt"
	"sort"

	"github.com/bakhod1r/seedora/internal/model"
	"github.com/bakhod1r/seedora/internal/plan"
)

// Order returns the tables to seed, parents before children.
//
// The edges come from the plan rather than from the schema, because a foreign
// key column the user chose to skip or to fill with a constant imposes no
// ordering — and a plan that references a table the catalog does not link to
// imposes one the catalog does not know about.
func Order(s *model.Schema, p *plan.Plan) ([]*model.Table, error) {
	// deps[child] is the set of tables that must be written first.
	deps := map[string]map[string]bool{}
	included := map[string]bool{}

	for _, t := range s.Tables {
		tp := p.Tables[t.Name]
		if tp == nil || tp.Skip || tp.Rows <= 0 {
			continue
		}
		included[t.Name] = true
		deps[t.Name] = map[string]bool{}
	}

	for name := range included {
		tp := p.Tables[name]
		t := s.Table(name)
		for col, cp := range tp.Columns {
			if cp.Skip || cp.Generator != plan.GenForeignKey {
				continue
			}
			ref := cp.References
			if ref == "" {
				if c := t.Column(col); c != nil && c.FK != nil {
					ref = c.FK.Table + "." + c.FK.Column
				}
			}
			parent, _, ok := plan.SplitRef(ref)
			if !ok || parent == name {
				// A self-reference cannot be ordered around and is satisfied
				// from rows written earlier in the same table.
				continue
			}
			if included[parent] {
				deps[name][parent] = true
			}
		}
	}

	order, cyc := kahn(deps)
	if cyc != nil {
		return nil, &CycleError{Tables: cyc}
	}

	out := make([]*model.Table, 0, len(order))
	for _, name := range order {
		out = append(out, s.Table(name))
	}
	return out, nil
}

// kahn topologically sorts, breaking ties alphabetically so the same schema
// always seeds in the same order — which is what makes --seed reproducible
// across runs and machines.
func kahn(deps map[string]map[string]bool) (order []string, cycle []string) {
	remaining := make(map[string]map[string]bool, len(deps))
	for n, d := range deps {
		remaining[n] = make(map[string]bool, len(d))
		for p := range d {
			remaining[n][p] = true
		}
	}

	for len(remaining) > 0 {
		var ready []string
		for n, d := range remaining {
			if len(d) == 0 {
				ready = append(ready, n)
			}
		}
		if len(ready) == 0 {
			// Everything left is part of, or downstream of, a cycle.
			for n := range remaining {
				cycle = append(cycle, n)
			}
			sort.Strings(cycle)
			return nil, cycle
		}
		sort.Strings(ready)
		for _, n := range ready {
			order = append(order, n)
			delete(remaining, n)
		}
		for _, d := range remaining {
			for _, n := range ready {
				delete(d, n)
			}
		}
	}
	return order, nil
}

// CycleError reports a foreign-key cycle, which cannot be seeded in one pass.
type CycleError struct{ Tables []string }

func (e *CycleError) Error() string {
	return fmt.Sprintf("foreign-key cycle between %v: seed one side with a nullable "+
		"key set to the null generator, or skip one of the tables", e.Tables)
}
