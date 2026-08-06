package spec

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bakhod1r/seedora/internal/model"
	"github.com/bakhod1r/seedora/internal/plan"
)

func schema() *model.Schema {
	return &model.Schema{
		Tables: []*model.Table{{
			Name:       "users",
			PrimaryKey: []string{"id"},
			Columns: []*model.Column{
				{Name: "id", Type: "integer", HasDefault: true},
				{Name: "email", Type: "varchar", MaxLen: 255, Unique: true},
				{Name: "is_active", Type: "boolean"},
			},
		}},
	}
}

func TestRoundTripPreservesChoices(t *testing.T) {
	w := 0.85
	p := &plan.Plan{
		Version: 1,
		Locale:  "uz_UZ",
		Tables: map[string]*plan.TablePlan{
			"users": {
				Rows: 10000,
				Columns: map[string]*plan.ColumnPlan{
					"id":        {Generator: plan.GenDefault, Skip: true},
					"email":     {Generator: "email", Unique: true},
					"is_active": {Generator: "bool", TrueWeight: &w},
				},
			},
		},
	}

	path := filepath.Join(t.TempDir(), "seedora.yaml")
	if err := Save(path, p, schema()); err != nil {
		t.Fatal(err)
	}

	back, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	users := back.Tables["users"]
	if users == nil || users.Rows != 10000 {
		t.Fatalf("rows did not survive the round trip: %+v", users)
	}
	if e := users.Columns["email"]; e == nil || e.Generator != "email" || !e.Unique {
		t.Errorf("email column did not survive: %+v", e)
	}
	if a := users.Columns["is_active"]; a == nil || a.TrueWeight == nil || *a.TrueWeight != 0.85 {
		t.Errorf("true_weight did not survive: %+v", a)
	}
	if back.Locale != "uz_UZ" {
		t.Errorf("locale = %q, want uz_UZ", back.Locale)
	}
}

// The README promises credentials never reach the file. This is the test that
// makes that a property rather than a habit.
func TestSavedFileHoldsNoCredentials(t *testing.T) {
	p := plan.Infer(schema())
	path := filepath.Join(t.TempDir(), "seedora.yaml")
	if err := Save(path, p, schema()); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"dsn", "password", "postgres://", "user:"} {
		if strings.Contains(strings.ToLower(string(b)), forbidden) {
			t.Errorf("saved config contains %q:\n%s", forbidden, b)
		}
	}
}

func TestMergeKeepsHumanChoicesAndAddsNewColumns(t *testing.T) {
	committed := &plan.Plan{
		Version: 1,
		Tables: map[string]*plan.TablePlan{
			"users": {
				Rows: 50,
				Columns: map[string]*plan.ColumnPlan{
					// A deliberate override: the schema says varchar, the human
					// says company name. Re-scanning must not undo this.
					"email": {Generator: "company", Unique: true},
				},
			},
		},
	}

	committed.Merge(plan.Infer(schema()))

	users := committed.Tables["users"]
	if got := users.Columns["email"].Generator; got != "company" {
		t.Errorf("re-scan overwrote a human choice: email = %q", got)
	}
	if users.Rows != 50 {
		t.Errorf("re-scan overwrote the row count: %d", users.Rows)
	}
	if _, ok := users.Columns["is_active"]; !ok {
		t.Error("re-scan did not add the column the config had never seen")
	}
}

func TestMergeDropsColumns(t *testing.T) {
	p := &plan.Plan{
		Version: 1,
		Tables: map[string]*plan.TablePlan{
			"users": {Rows: 10, Columns: map[string]*plan.ColumnPlan{
				"email":   {Generator: "email"},
				"dropped": {Generator: "email"},
			}},
		},
	}
	p.Merge(plan.Infer(schema()))
	if _, ok := p.Tables["users"].Columns["dropped"]; ok {
		t.Error("a column the schema no longer has should be dropped from the plan")
	}
}

func TestUnknownKeyIsAnError(t *testing.T) {
	_, err := Parse([]byte("version: 1\ntabels:\n  users:\n    rows: 1\n"))
	if err == nil {
		t.Fatal("a misspelled key should be rejected, not ignored")
	}
}
