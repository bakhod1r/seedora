package ddl

import (
	"strings"
	"testing"

	"github.com/bakhod1r/seedora/internal/model"
)

func liveSchema() *model.Schema {
	return &model.Schema{
		Tables: []*model.Table{
			{
				Name:       "orders",
				PrimaryKey: []string{"id"},
				Columns: []*model.Column{
					{Name: "id", Type: "bigint", Native: "bigint", HasDefault: true},
					{Name: "user_id", Type: "bigint", Native: "bigint",
						FK: &model.Ref{Table: "users", Column: "id"}},
					{Name: "note", Type: "text", Native: "text", Nullable: true},
				},
			},
			{
				Name:       "users",
				PrimaryKey: []string{"id"},
				Columns: []*model.Column{
					{Name: "id", Type: "bigint", Native: "bigint", HasDefault: true},
					{Name: "email", Type: "varchar", Native: "character varying(120)", Unique: true},
				},
			},
		},
	}
}

func TestScriptRendersTheLiveSchema(t *testing.T) {
	sql := strings.Join(Script(Postgres, liveSchema()), "\n")

	for _, want := range []string{
		`CREATE TABLE "users"`,
		`CREATE TABLE "orders"`,
		// The fully-spelled declaration, because `varchar` alone loses the limit.
		`"email" character varying(120) NOT NULL UNIQUE`,
		`FOREIGN KEY ("user_id") REFERENCES "users" ("id")`,
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("want %q in:\n%s", want, sql)
		}
	}
	// A child cannot be created before the table it points at.
	if strings.Index(sql, `CREATE TABLE "users"`) > strings.Index(sql, `CREATE TABLE "orders"`) {
		t.Errorf("parents come first:\n%s", sql)
	}
}

// What Script writes, Parse must read: the two are the same schema in two
// directions, and a round trip that loses a column is worse than no export.
func TestScriptRoundTripsThroughParse(t *testing.T) {
	sql := strings.Join(Script(Postgres, liveSchema()), ";\n") + ";"

	changes, err := Parse(sql)
	if err != nil {
		t.Fatal(err)
	}
	byTable := map[string]Change{}
	for _, c := range changes {
		byTable[c.Table] = c
	}
	if len(byTable) != 2 {
		t.Fatalf("want both tables back, got %d", len(byTable))
	}

	orders := byTable["orders"]
	var userID Column
	for _, c := range orders.Columns {
		if c.Name == "user_id" {
			userID = c
		}
	}
	if userID.References != "users.id" {
		t.Errorf("the foreign key did not survive: %+v", userID)
	}
}

func TestMermaidDescribesEntitiesAndRelationships(t *testing.T) {
	out := Mermaid(liveSchema())

	if !strings.HasPrefix(out, "erDiagram") {
		t.Fatalf("want an erDiagram header:\n%s", out)
	}
	for _, want := range []string{
		"users {",
		"orders {",
		"bigint id PK",
		"varchar email UK",
		"bigint user_id FK",
		// One user, many orders, and the join is labelled with the column that
		// makes it.
		"users ||--o{ orders : user_id",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("want %q in:\n%s", want, out)
		}
	}
	// Mermaid has no syntax for a parameterised type inside an entity block.
	if strings.Contains(out, "(120)") {
		t.Errorf("types must be flattened:\n%s", out)
	}
}

// A unique foreign key points at exactly one parent row, and the diagram has to
// say so or every relationship looks the same.
func TestMermaidMarksOneToOne(t *testing.T) {
	s := liveSchema()
	s.Table("orders").Column("user_id").Unique = true

	if out := Mermaid(s); !strings.Contains(out, "users ||--|| orders : user_id") {
		t.Errorf("want a one-to-one relationship in:\n%s", out)
	}
}

func TestMermaidHandlesAnEmptySchema(t *testing.T) {
	if got := Mermaid(nil); got != "erDiagram\n" {
		t.Errorf("want a bare diagram, got %q", got)
	}
}
