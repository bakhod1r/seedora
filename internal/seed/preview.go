package seed

import (
	"context"
	"fmt"

	"github.com/bakhod1r/seedora/internal/db"
	"github.com/bakhod1r/seedora/internal/model"
	"github.com/bakhod1r/seedora/internal/plan"
)

// Preview generates a handful of rows for one table and writes nothing.
//
// This is the loop the UI exists for: change a generator, see what it actually
// produces, keep it or change it again. It costs the database one short key read
// per foreign-key column and nothing else — the rows are built in memory and
// thrown away, and the transaction is rolled back.
func Preview(
	ctx context.Context,
	d db.Driver,
	s *model.Schema,
	p *plan.Plan,
	table string,
	rows int,
	locale string,
	nonce uint64,
) ([]map[string]any, []string, error) {
	t := s.Table(table)
	if t == nil {
		return nil, nil, fmt.Errorf("unknown table %s", table)
	}
	tp := p.Tables[table]
	if tp == nil {
		return nil, nil, fmt.Errorf("no plan for table %s", table)
	}
	if rows <= 0 {
		rows = 5
	}

	cols, err := writableColumns(t, tp)
	if err != nil {
		return nil, nil, err
	}
	if len(cols) == 0 {
		return nil, cols, nil
	}

	if locale == "" {
		locale = p.Locale
	}
	// A fixed seed, so opening the same table twice shows the same rows and a
	// change in the output is a change the user made rather than noise. A
	// non-zero nonce is the caller asking for a different draw — that is what
	// Regenerate sends, so the button produces new rows instead of the same
	// ones again.
	seedVal := firstNonZero(p.Seed, 1)
	if nonce != 0 {
		seedVal ^= nonce * 0x9e3779b97f4a7c15
		if seedVal == 0 {
			seedVal = nonce
		}
	}

	gen, err := newGenerator(t, tp, locale, seedVal, rows)
	if err != nil {
		return nil, nil, err
	}
	gen.cols = cols

	tx, err := d.Begin(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// A preview only needs enough parent keys to show plausible values, not the
	// full pool a run would draw from.
	fks, err := previewFKs(ctx, tx, s, p, t, tp, locale, seedVal)
	if err != nil {
		return nil, nil, err
	}
	gen.fks = fks

	batch, err := gen.batch(0, 0, rows)
	if err != nil {
		return nil, nil, err
	}
	if u := newUniqueSet(t, tp, rows); u.active() {
		for i, row := range batch {
			if err := u.enforce(row, i); err != nil {
				return nil, nil, err
			}
		}
	}

	// Trim each row to the columns that will actually be written, so the
	// preview table matches the insert.
	out := make([]map[string]any, len(batch))
	for i, row := range batch {
		trimmed := make(map[string]any, len(cols))
		for _, c := range cols {
			trimmed[c] = row[c]
		}
		out[i] = trimmed
	}
	return out, cols, nil
}

// previewFKCap is how many parent keys a preview reads. Enough to show that the
// key resolves to something real, few enough to be free.
const previewFKCap = 100

func previewFKs(
	ctx context.Context,
	tx db.Tx,
	s *model.Schema,
	p *plan.Plan,
	t *model.Table,
	tp *plan.TablePlan,
	locale string,
	seedVal uint64,
) (map[string][]any, error) {
	out := map[string][]any{}
	for col, cp := range tp.Columns {
		if cp.Skip || cp.Generator != plan.GenForeignKey {
			continue
		}
		ref := cp.References
		if ref == "" {
			c := t.Column(col)
			if c == nil || c.FK == nil {
				continue
			}
			ref = c.FK.Table + "." + c.FK.Column
		}
		parentName, parentCol, ok := plan.SplitRef(ref)
		if !ok {
			continue
		}
		parent := s.Table(parentName)
		if parent == nil {
			continue
		}
		keys, err := tx.ReadKeys(ctx, parent, parentCol, previewFKCap)
		if err != nil {
			continue
		}
		// An empty parent is the normal case on a database nobody has seeded
		// yet, and a column of NULLs shows nothing about what a run produces.
		// The keys the parent is about to be given are generated instead, so a
		// preview of a child reads like a real row.
		if len(keys) == 0 {
			keys = proposedKeys(s, p, parentName, parentCol, locale, seedVal)
		}
		out[col] = keys
	}
	return out, nil
}

// proposedKeys is what the parent's key column will hold once it is seeded.
//
// A key the database assigns itself — a serial or identity column, which the
// plan skips — becomes 1..n, because that is exactly what the engine will hand
// out on an empty table. Anything the plan does generate is generated here, so
// a uuid or a code previews as a uuid or a code rather than as a number.
func proposedKeys(
	s *model.Schema,
	p *plan.Plan,
	table, col, locale string,
	seedVal uint64,
) []any {
	const n = 20

	parent := s.Table(table)
	tp := p.Tables[table]
	if parent == nil || tp == nil {
		return nil
	}
	cp := tp.Get(col)
	if cp == nil || cp.Skip || cp.Generator == plan.GenDefault {
		out := make([]any, n)
		for i := range out {
			out[i] = int64(i + 1)
		}
		return out
	}

	gen, err := newGenerator(parent, tp, locale, seedVal, n)
	if err != nil {
		return nil
	}
	// Only the key column, so a parent whose other columns need their own
	// foreign keys does not drag the whole graph into a preview.
	gen.cols = []string{col}
	rows, err := gen.batch(0, 0, n)
	if err != nil {
		return nil
	}
	out := make([]any, 0, len(rows))
	for _, row := range rows {
		if v := row[col]; v != nil {
			out = append(out, v)
		}
	}
	return out
}
