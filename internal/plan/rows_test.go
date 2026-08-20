package plan_test

import (
	"encoding/json"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/bakhod1r/seedora/internal/model"
	"github.com/bakhod1r/seedora/internal/plan"
)

func twoTableSchema() *model.Schema {
	return &model.Schema{Tables: []*model.Table{
		{Name: "users", Columns: []*model.Column{{Name: "id", Type: "bigint"}}},
		{Name: "audit_log", Columns: []*model.Column{{Name: "id", Type: "bigint"}}},
	}}
}

// `rows: 0` is how a committed config leaves a table out, and a re-scan used to
// overwrite it with the inferred default — silently, so the table filled up on
// the next run with nothing in the file to explain why.
func TestRowsZeroSurvivesAMerge(t *testing.T) {
	const cfg = `
version: 1
tables:
  users:
    rows: 100
    columns:
      id: { generator: int }
  audit_log:
    rows: 0
    columns:
      id: { generator: int }
`
	var p plan.Plan
	if err := yaml.Unmarshal([]byte(cfg), &p); err != nil {
		t.Fatal(err)
	}
	p.Merge(plan.Infer(twoTableSchema()))

	if got := p.Tables["audit_log"].Rows; got != 0 {
		t.Errorf("audit_log got %d rows after the merge, want 0 — the config said so", got)
	}
	if got := p.Tables["users"].Rows; got != 100 {
		t.Errorf("users got %d rows, want 100", got)
	}
}

// The other half: a table with no count at all is a question, and inference is
// what answers it. If this stops working, the fix above has turned every new
// table into an empty one.
func TestAnAbsentRowCountStillTakesTheProposal(t *testing.T) {
	const cfg = `
version: 1
tables:
  users:
    columns:
      id: { generator: int }
`
	var p plan.Plan
	if err := yaml.Unmarshal([]byte(cfg), &p); err != nil {
		t.Fatal(err)
	}
	p.Merge(plan.Infer(twoTableSchema()))

	if got := p.Tables["users"].Rows; got != plan.DefaultRows {
		t.Errorf("users got %d rows, want the inferred %d", got, plan.DefaultRows)
	}
}

// The UI sends the plan as JSON, and a table the user set to zero there has to
// mean the same thing it means in the file.
func TestRowsZeroSurvivesAMergeFromJSON(t *testing.T) {
	const body = `{"version":1,"tables":{"audit_log":{"rows":0,"columns":{}}}}`

	var p plan.Plan
	if err := json.Unmarshal([]byte(body), &p); err != nil {
		t.Fatal(err)
	}
	p.Merge(plan.Infer(twoTableSchema()))

	if got := p.Tables["audit_log"].Rows; got != 0 {
		t.Errorf("audit_log got %d rows after the merge, want 0", got)
	}
}
