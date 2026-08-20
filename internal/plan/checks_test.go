package plan_test

import (
	"strings"
	"testing"

	"github.com/bakhod1r/seedora/internal/model"
	"github.com/bakhod1r/seedora/internal/plan"
)

func schemaWithChecks() *model.Schema {
	return &model.Schema{Tables: []*model.Table{{
		Name: "user",
		Columns: []*model.Column{
			{Name: "id", Type: "uuid", HasDefault: true},
			{Name: "nickname", Type: "varchar", Native: "character varying(30)",
				MaxLen: 30, Nullable: true, Unique: true, UniqueFold: true},
			{Name: "phone", Type: "varchar", MaxLen: 20, Nullable: true},
			{Name: "phone_country_code", Type: "varchar", MaxLen: 4, Nullable: true},
			{Name: "deleted_at", Type: "bigint", Nullable: true, AlwaysNull: true},
		},
		Checks: []*model.Check{
			{Name: "nickname_format", Expr: `((nickname)::text ~ '^[a-zA-Z0-9_]{3,30}$'::text)`,
				Columns: []string{"nickname"}},
			{Name: "phone_pair", Expr: `((phone IS NULL) = (phone_country_code IS NULL))`,
				Columns: []string{"phone", "phone_country_code"}},
		},
	}}}
}

// A single-column regex check is the column's definition, and inference has to
// take it: no name-based generator produces a value matching it by accident.
func TestInferTurnsARegexCheckIntoAPattern(t *testing.T) {
	p := plan.Infer(schemaWithChecks())
	cp := p.Tables["user"].Columns["nickname"]

	if cp.Generator != plan.GenPattern {
		t.Fatalf("nickname generator is %q, want %q", cp.Generator, plan.GenPattern)
	}
	if cp.Pattern != `^[a-zA-Z0-9_]{3,30}$` {
		t.Errorf("pattern is %q", cp.Pattern)
	}
}

// A two-column check cannot be satisfied one column at a time, so the plan says
// so instead of generating into it and finding out at the insert.
func TestInferReportsAMultiColumnCheckAsUnenforced(t *testing.T) {
	s := schemaWithChecks()
	p := plan.Infer(s)

	for _, col := range []string{"phone", "phone_country_code"} {
		cp := p.Tables["user"].Columns[col]
		if len(cp.Unenforced) == 0 {
			t.Errorf("%s: nothing recorded as unenforced", col)
		}
		if cp.Confidence != plan.Low {
			t.Errorf("%s: confidence is %q, want low", col, cp.Confidence)
		}
	}

	unenforced := p.UnenforcedChecks(s)
	if len(unenforced) != 1 || !strings.Contains(unenforced[0], "phone_pair") {
		t.Errorf("UnenforcedChecks = %v, want the phone pair", unenforced)
	}
}

// The soft-delete stamp. Filling it drops the row out of every partial index on
// the table, which loads cleanly and measures nothing.
func TestInferLeavesASoftDeleteColumnNull(t *testing.T) {
	p := plan.Infer(schemaWithChecks())
	cp := p.Tables["user"].Columns["deleted_at"]

	if cp.Generator != plan.GenNull {
		t.Errorf("deleted_at generator is %q, want %q", cp.Generator, plan.GenNull)
	}
	if cp.NullRate != 0 {
		t.Errorf("deleted_at null_rate is %v; the generator already writes NULL", cp.NullRate)
	}
}

// A column the plan writes from its own check is enforced; the same column left
// to a name-based guess is not.
func TestUnenforcedChecksIgnoresAPatternThatCameFromTheCheck(t *testing.T) {
	s := schemaWithChecks()
	p := plan.Infer(s)
	for _, c := range p.UnenforcedChecks(s) {
		if strings.Contains(c, "nickname_format") {
			t.Errorf("nickname is generated from its own check but reported as %q", c)
		}
	}
}
