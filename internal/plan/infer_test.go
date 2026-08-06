package plan

import (
	"testing"

	"github.com/bakhod1r/seedora/internal/model"
)

func col(name, typ string, maxLen int) *model.Column {
	return &model.Column{Name: name, Type: typ, MaxLen: maxLen}
}

// A bare `name` means a person on a table of people and a label everywhere
// else. Getting this wrong is not subtle: a category called "Jacob Young" is
// the first thing anyone notices in a preview.
func TestAmbiguousNameFollowsTheTable(t *testing.T) {
	cases := []struct {
		table string
		want  string
	}{
		{"users", "name"},
		{"customers", "name"},
		{"employees", "name"},
		{"categories", "word"},
		{"products", "word"},
		{"tags", "word"},
		{"warehouses", "word"},
	}

	for _, c := range cases {
		tbl := &model.Table{Name: c.table, Columns: []*model.Column{col("name", "varchar", 80)}}
		cp := InferColumn(&model.Schema{Tables: []*model.Table{tbl}}, tbl, tbl.Columns[0])
		if cp.Generator != c.want {
			t.Errorf("%s.name: got %q, want %q (%s)", c.table, cp.Generator, c.want, cp.Why)
		}
		// Either way it is a guess, and the UI has to say so.
		if cp.Confidence != Low {
			t.Errorf("%s.name: want Low confidence, got %q", c.table, cp.Confidence)
		}
	}
}

// A qualified name is not ambiguous, and must not be rewritten on its way past.
func TestQualifiedNamesAreLeftAlone(t *testing.T) {
	tbl := &model.Table{Name: "categories", Columns: []*model.Column{
		col("first_name", "varchar", 50),
		col("email", "varchar", 120),
	}}
	s := &model.Schema{Tables: []*model.Table{tbl}}

	for _, c := range tbl.Columns {
		cp := InferColumn(s, tbl, c)
		if cp.Confidence != High {
			t.Errorf("%s: want High confidence, got %q (%s)", c.Name, cp.Confidence, cp.Why)
		}
	}
}
