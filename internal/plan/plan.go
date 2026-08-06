// Package plan holds the editable mapping between a schema and the generators
// that will fill it, plus the inference that proposes that mapping.
//
// A Plan is what the UI edits and what seedora.yaml persists. It is deliberately
// not the execution format: the seeder translates it into a Synth spec at run
// time, so a change to Synth's spec language does not change the file users
// committed.
package plan

import (
	"sort"

	"github.com/bakhod1r/seedora/internal/model"
)

// Plan is a full seeding intent: which tables, how many rows, which generator
// per column.
type Plan struct {
	Version int                   `json:"version" yaml:"version"`
	Seed    uint64                `json:"seed,omitempty" yaml:"seed,omitempty"`
	Locale  string                `json:"locale,omitempty" yaml:"locale,omitempty"`
	Tables  map[string]*TablePlan `json:"tables" yaml:"tables"`
}

// TablePlan is the intent for one table.
type TablePlan struct {
	Rows     int                    `json:"rows" yaml:"rows"`
	Truncate bool                   `json:"truncate,omitempty" yaml:"truncate,omitempty"`
	Skip     bool                   `json:"skip,omitempty" yaml:"skip,omitempty"`
	Columns  map[string]*ColumnPlan `json:"columns" yaml:"columns"`
	// order preserves catalog column order, which JSON and YAML maps lose. The
	// UI renders in this order and bulk writers emit in it.
	Order []string `json:"order,omitempty" yaml:"-"`
}

// ColumnPlan is the generator chosen for one column, plus its options.
type ColumnPlan struct {
	// Generator is a Synth kind name ("email", "firstname"), or one of the
	// Seedora-only pseudo-generators below.
	Generator string `json:"generator" yaml:"generator"`
	// Skip leaves the column out of the insert so the database default fills
	// it. This is the default for serial, identity, and generated columns.
	Skip bool `json:"skip,omitempty" yaml:"skip,omitempty"`
	// Confidence records how the generator was chosen, so the UI can surface
	// what needs a human. It is advisory and never affects generation.
	Confidence Confidence `json:"confidence,omitempty" yaml:"-"`
	// Why explains the inference in one phrase, for the UI.
	Why string `json:"why,omitempty" yaml:"-"`

	Unique bool     `json:"unique,omitempty" yaml:"unique,omitempty"`
	Locale string   `json:"locale,omitempty" yaml:"locale,omitempty"`
	Min    any      `json:"min,omitempty" yaml:"min,omitempty"`
	Max    any      `json:"max,omitempty" yaml:"max,omitempty"`
	Values []string `json:"values,omitempty" yaml:"values,omitempty"`
	// Weights parallels Values for a weighted pick.
	Weights []float64 `json:"weights,omitempty" yaml:"weights,omitempty"`
	// TrueWeight is the probability of true for a boolean column.
	TrueWeight *float64 `json:"true_weight,omitempty" yaml:"true_weight,omitempty"`
	// NullRate is the share of rows left NULL, only legal on a nullable column.
	NullRate float64 `json:"null_rate,omitempty" yaml:"null_rate,omitempty"`
	// References is "table.column" for a foreign key.
	References string `json:"references,omitempty" yaml:"references,omitempty"`
	// Pattern is a regex the generated value must match.
	Pattern string `json:"pattern,omitempty" yaml:"pattern,omitempty"`
	// Const is a fixed value written to every row.
	Const any `json:"const,omitempty" yaml:"const,omitempty"`
}

// Seedora-only pseudo-generators. Everything else is a Synth kind, passed
// through untouched.
const (
	// GenForeignKey draws from the parent keys written in the same run.
	GenForeignKey = "foreign_key"
	// GenSequence is a monotonic counter starting at 1.
	GenSequence = "sequence"
	// GenNull writes NULL to every row.
	GenNull = "null"
	// GenConst writes ColumnPlan.Const to every row.
	GenConst = "const"
	// GenDefault omits the column so the database default applies.
	GenDefault = "default"
)

// Confidence is why a generator was picked.
type Confidence string

const (
	// High: the column name matched a known semantic (email, first_name).
	High Confidence = "high"
	// Medium: only the type was decisive; the name said nothing.
	Medium Confidence = "medium"
	// Low: nothing matched and a placeholder was chosen. The UI flags these.
	Low Confidence = "low"
	// Manual: a human set it, so inference must never overwrite it.
	Manual Confidence = "manual"
)

// TableNames returns the plan's tables in a stable order.
func (p *Plan) TableNames() []string {
	names := make([]string, 0, len(p.Tables))
	for n := range p.Tables {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Get returns the column plan, or nil.
func (t *TablePlan) Get(col string) *ColumnPlan {
	if t.Columns == nil {
		return nil
	}
	return t.Columns[col]
}

// Merge folds a freshly inferred plan into an existing one: human decisions win,
// and columns the old plan never saw are added from the new one. This is what
// makes re-scanning a database with a committed seedora.yaml non-destructive.
func (p *Plan) Merge(fresh *Plan) {
	if p.Tables == nil {
		p.Tables = map[string]*TablePlan{}
	}
	for name, ft := range fresh.Tables {
		ot, ok := p.Tables[name]
		if !ok {
			p.Tables[name] = ft
			continue
		}
		// The order is the user's arrangement, not a fact about the schema, so
		// a merge keeps it and only fills it in when there is none. Callers
		// reconcile it against the live column list afterwards.
		if len(ot.Order) == 0 {
			ot.Order = ft.Order
		}
		if ot.Rows == 0 {
			ot.Rows = ft.Rows
		}
		if ot.Columns == nil {
			ot.Columns = map[string]*ColumnPlan{}
		}
		for col, fc := range ft.Columns {
			oc, ok := ot.Columns[col]
			if !ok {
				// New column since the config was written. Take the proposal.
				ot.Columns[col] = fc
				continue
			}
			// The column existed, so the committed choice stands. Only the
			// advisory fields refresh, since they describe the current schema.
			oc.Confidence = Manual
			oc.Why = fc.Why
		}
		// Columns dropped from the schema are dropped from the plan; keeping
		// them would fail validation against the live database anyway.
		for col := range ot.Columns {
			if _, ok := ft.Columns[col]; !ok {
				delete(ot.Columns, col)
			}
		}
	}
	for name := range p.Tables {
		if _, ok := fresh.Tables[name]; !ok {
			delete(p.Tables, name)
		}
	}
}

// Validate checks a plan against a live schema and returns every problem found,
// rather than the first — a config that drifted usually drifted in several
// places at once.
func (p *Plan) Validate(s *model.Schema) []error {
	var errs []error
	for name, tp := range p.Tables {
		t := s.Table(name)
		if t == nil {
			errs = append(errs, &ValidationError{Table: name, Msg: "table not found in database"})
			continue
		}
		if tp.Rows < 0 {
			errs = append(errs, &ValidationError{Table: name, Msg: "row count is negative"})
		}
		for col, cp := range tp.Columns {
			c := t.Column(col)
			if c == nil {
				errs = append(errs, &ValidationError{Table: name, Column: col, Msg: "column not found in database"})
				continue
			}
			errs = append(errs, validateColumn(s, name, c, cp)...)
		}
		// A non-nullable column with no default and no plan entry cannot be
		// inserted at all, which is a failure worth catching before the run.
		for _, c := range t.Columns {
			if c.Nullable || c.HasDefault || c.Generated {
				continue
			}
			if cp := tp.Get(c.Name); cp == nil || cp.Skip {
				errs = append(errs, &ValidationError{
					Table: name, Column: c.Name,
					Msg: "column is NOT NULL with no default but has no generator",
				})
			}
		}
	}
	return errs
}

func validateColumn(s *model.Schema, table string, c *model.Column, cp *ColumnPlan) []error {
	var errs []error
	fail := func(msg string) {
		errs = append(errs, &ValidationError{Table: table, Column: c.Name, Msg: msg})
	}
	if cp.NullRate > 0 && !c.Nullable {
		fail("null_rate set on a NOT NULL column")
	}
	if cp.NullRate < 0 || cp.NullRate > 1 {
		fail("null_rate must be between 0 and 1")
	}
	if cp.TrueWeight != nil && (*cp.TrueWeight < 0 || *cp.TrueWeight > 1) {
		fail("true_weight must be between 0 and 1")
	}
	if len(cp.Weights) > 0 && len(cp.Weights) != len(cp.Values) {
		fail("weights and values have different lengths")
	}
	if cp.Unique && !c.Unique && !isPK(s, table, c.Name) {
		// Not an error: generating unique values into a non-unique column is
		// legal, just slower. Left unreported on purpose.
		_ = cp
	}
	switch cp.Generator {
	case GenForeignKey:
		if cp.References == "" && c.FK == nil {
			fail("foreign_key generator with no reference and no FK constraint")
		}
		ref := cp.References
		if ref == "" {
			ref = c.FK.Table + "." + c.FK.Column
		}
		rt, rc, ok := splitRef(ref)
		if !ok {
			fail("references must be table.column, got " + ref)
			break
		}
		parent := s.Table(rt)
		if parent == nil {
			fail("references unknown table " + rt)
		} else if parent.Column(rc) == nil {
			fail("references unknown column " + ref)
		}
	case GenConst:
		if cp.Const == nil && !c.Nullable {
			fail("const generator with no value on a NOT NULL column")
		}
	case GenNull:
		if !c.Nullable {
			fail("null generator on a NOT NULL column")
		}
	case "":
		fail("no generator set")
	}
	return errs
}

func isPK(s *model.Schema, table, col string) bool {
	t := s.Table(table)
	if t == nil {
		return false
	}
	for _, k := range t.PrimaryKey {
		if k == col {
			return true
		}
	}
	return false
}

// splitRef parses "table.column". A schema-qualified "public.users.id" keeps
// everything before the last dot as the table.
func splitRef(ref string) (table, column string, ok bool) {
	for i := len(ref) - 1; i >= 0; i-- {
		if ref[i] == '.' {
			if i == 0 || i == len(ref)-1 {
				return "", "", false
			}
			return ref[:i], ref[i+1:], true
		}
	}
	return "", "", false
}

// SplitRef exposes reference parsing to the seeder.
func SplitRef(ref string) (table, column string, ok bool) { return splitRef(ref) }

// ValidationError is one problem found by Validate.
type ValidationError struct {
	Table  string
	Column string
	Msg    string
}

func (e *ValidationError) Error() string {
	loc := e.Table
	if e.Column != "" {
		loc += "." + e.Column
	}
	return loc + ": " + e.Msg
}
