package ddl

import (
	"strings"
	"testing"

	"github.com/bakhod1r/seedora/internal/model"
)

// schema is the existing database every case validates against: one populated
// table with a key, and one empty one. The row counts are the point — the
// NOT NULL rule turns on them.
func schema() *model.Schema {
	return &model.Schema{
		Tables: []*model.Table{
			{
				Name:         "users",
				PrimaryKey:   []string{"id"},
				ExistingRows: 12,
				Columns: []*model.Column{
					{Name: "id", Type: "bigint"},
					{Name: "email", Type: "text"},
				},
			},
			{
				Name:       "empty",
				PrimaryKey: []string{"id"},
				Columns: []*model.Column{
					{Name: "id", Type: "bigint"},
				},
			},
		},
	}
}

func col(name, typ string) Column { return Column{Name: name, Type: typ} }

func pkCol(name, typ string) Column { return Column{Name: name, Type: typ, PK: true} }

// TestValidate covers each rule the package exists to enforce, on both
// dialects where the dialect changes the answer. A case asserts on a fragment
// of the message rather than the whole string, so rewording an error does not
// break the test that proves it fires.
func TestValidate(t *testing.T) {
	cases := []struct {
		name    string
		dialect Dialect
		changes []Change
		want    string // substring of the expected error, empty means no error
	}{
		{
			name:    "create table is accepted",
			dialect: Postgres,
			changes: []Change{{Kind: CreateTable, Table: "orders", Columns: []Column{
				pkCol("id", "bigserial"), col("total", "numeric(10,2)"),
			}}},
		},
		{
			name:    "table name must be a usable identifier",
			dialect: Postgres,
			changes: []Change{{Kind: CreateTable, Table: "drop table;", Columns: []Column{
				pkCol("id", "bigserial"),
			}}},
			want: "not a usable table name",
		},
		{
			name:    "column name must be a usable identifier",
			dialect: Postgres,
			changes: []Change{{Kind: CreateTable, Table: "orders", Columns: []Column{
				pkCol("id", "bigserial"), col("1st", "text"),
			}}},
			want: "not a usable column name",
		},
		{
			name:    "creating a table that already exists",
			dialect: Postgres,
			changes: []Change{{Kind: CreateTable, Table: "users", Columns: []Column{
				pkCol("id", "bigserial"),
			}}},
			want: "already exists",
		},
		{
			name:    "a table needs columns",
			dialect: Postgres,
			changes: []Change{{Kind: CreateTable, Table: "orders"}},
			want:    "has no columns",
		},
		{
			name:    "duplicate column",
			dialect: Postgres,
			changes: []Change{{Kind: CreateTable, Table: "orders", Columns: []Column{
				pkCol("id", "bigserial"), col("total", "numeric"), col("total", "numeric"),
			}}},
			want: "defined twice",
		},
		{
			name:    "missing type",
			dialect: Postgres,
			changes: []Change{{Kind: CreateTable, Table: "orders", Columns: []Column{
				pkCol("id", "bigserial"), col("total", "   "),
			}}},
			want: "has no type",
		},
		{
			name:    "a table with no primary key is reported",
			dialect: Postgres,
			changes: []Change{{Kind: CreateTable, Table: "orders", Columns: []Column{
				col("total", "numeric"),
			}}},
			want: "no primary key",
		},

		{
			name:    "dropping a table that does not exist",
			dialect: Postgres,
			changes: []Change{{Kind: DropTable, Table: "ghosts"}},
			want:    "does not exist",
		},
		{
			name:    "dropping an existing table",
			dialect: Postgres,
			changes: []Change{{Kind: DropTable, Table: "users"}},
		},

		{
			name:    "adding to a table that does not exist",
			dialect: Postgres,
			changes: []Change{{Kind: AddColumn, Table: "ghosts", Columns: []Column{col("x", "text")}}},
			want:    "does not exist",
		},
		{
			name:    "adding a column that is already there",
			dialect: Postgres,
			changes: []Change{{Kind: AddColumn, Table: "users", Columns: []Column{col("email", "text")}}},
			want:    "already has a column named email",
		},
		{
			name:    "NOT NULL with no default onto a populated table",
			dialect: Postgres,
			changes: []Change{{Kind: AddColumn, Table: "users", Columns: []Column{col("nick", "text")}}},
			want:    "already has rows",
		},
		{
			name:    "NOT NULL with a default is fine",
			dialect: Postgres,
			changes: []Change{{Kind: AddColumn, Table: "users", Columns: []Column{
				{Name: "nick", Type: "text", Default: "''"},
			}}},
		},
		{
			name:    "NOT NULL onto an empty table is fine",
			dialect: Postgres,
			changes: []Change{{Kind: AddColumn, Table: "empty", Columns: []Column{col("nick", "text")}}},
		},
		{
			name:    "nullable onto a populated table is fine",
			dialect: Postgres,
			changes: []Change{{Kind: AddColumn, Table: "users", Columns: []Column{
				{Name: "nick", Type: "text", Nullable: true},
			}}},
		},
		{
			name:    "SQLite refuses a UNIQUE column on ALTER",
			dialect: SQLite,
			changes: []Change{{Kind: AddColumn, Table: "empty", Columns: []Column{
				{Name: "nick", Type: "TEXT", Nullable: true, Unique: true},
			}}},
			want: "SQLite cannot add a UNIQUE column",
		},
		{
			name:    "Postgres allows a UNIQUE column on ALTER",
			dialect: Postgres,
			changes: []Change{{Kind: AddColumn, Table: "empty", Columns: []Column{
				{Name: "nick", Type: "text", Nullable: true, Unique: true},
			}}},
		},

		{
			name:    "dropping from a table that does not exist",
			dialect: Postgres,
			changes: []Change{{Kind: DropColumn, Table: "ghosts", Column: "x"}},
			want:    "does not exist",
		},
		{
			name:    "dropping a column that does not exist",
			dialect: Postgres,
			changes: []Change{{Kind: DropColumn, Table: "users", Column: "nope"}},
			want:    "has no column named nope",
		},
		{
			name:    "dropping a primary key column",
			dialect: Postgres,
			changes: []Change{{Kind: DropColumn, Table: "users", Column: "id"}},
			want:    "part of the primary key",
		},
		{
			name:    "dropping an ordinary column",
			dialect: Postgres,
			changes: []Change{{Kind: DropColumn, Table: "users", Column: "email"}},
		},

		{
			name:    "reference must be table.column",
			dialect: Postgres,
			changes: []Change{{Kind: CreateTable, Table: "orders", Columns: []Column{
				pkCol("id", "bigserial"),
				{Name: "user_id", Type: "bigint", References: "users"},
			}}},
			want: "must be table.column",
		},
		{
			name:    "reference to an unknown table",
			dialect: Postgres,
			changes: []Change{{Kind: CreateTable, Table: "orders", Columns: []Column{
				pkCol("id", "bigserial"),
				{Name: "user_id", Type: "bigint", References: "ghosts.id"},
			}}},
			want: "references unknown table",
		},
		{
			name:    "reference to an unknown column",
			dialect: Postgres,
			changes: []Change{{Kind: CreateTable, Table: "orders", Columns: []Column{
				pkCol("id", "bigserial"),
				{Name: "user_id", Type: "bigint", References: "users.nope"},
			}}},
			want: "references unknown column",
		},
		{
			name:    "reference to a table being dropped",
			dialect: Postgres,
			changes: []Change{
				{Kind: DropTable, Table: "users"},
				{Kind: CreateTable, Table: "orders", Columns: []Column{
					pkCol("id", "bigserial"),
					{Name: "user_id", Type: "bigint", References: "users.id"},
				}},
			},
			want: "which is being dropped",
		},
		{
			name:    "reference to a table created in the same batch",
			dialect: Postgres,
			changes: []Change{
				{Kind: CreateTable, Table: "carts", Columns: []Column{pkCol("id", "bigserial")}},
				{Kind: CreateTable, Table: "cart_items", Columns: []Column{
					pkCol("id", "bigserial"),
					{Name: "cart_id", Type: "bigint", References: "carts.id"},
				}},
			},
		},
		{
			name:    "reference to a column the new parent does not have",
			dialect: Postgres,
			changes: []Change{
				{Kind: CreateTable, Table: "carts", Columns: []Column{pkCol("id", "bigserial")}},
				{Kind: CreateTable, Table: "cart_items", Columns: []Column{
					pkCol("id", "bigserial"),
					{Name: "cart_id", Type: "bigint", References: "carts.uuid"},
				}},
			},
			want: "not one of that table's columns",
		},
		{
			name:    "a self reference is allowed",
			dialect: Postgres,
			changes: []Change{{Kind: CreateTable, Table: "nodes", Columns: []Column{
				pkCol("id", "bigserial"),
				{Name: "parent_id", Type: "bigint", Nullable: true, References: "nodes.id"},
			}}},
		},
		{
			name:    "two new tables referencing each other",
			dialect: Postgres,
			changes: []Change{
				{Kind: CreateTable, Table: "a", Columns: []Column{
					pkCol("id", "bigserial"),
					{Name: "b_id", Type: "bigint", References: "b.id"},
				}},
				{Kind: CreateTable, Table: "b", Columns: []Column{
					pkCol("id", "bigserial"),
					{Name: "a_id", Type: "bigint", References: "a.id"},
				}},
			},
			want: "reference each other",
		},
		{
			name:    "unknown change kind",
			dialect: Postgres,
			changes: []Change{{Kind: "rename_table", Table: "users"}},
			want:    "unknown change",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			errs := Validate(c.dialect, schema(), c.changes)
			if c.want == "" {
				if len(errs) > 0 {
					t.Fatalf("want no error, got %v", errs)
				}
				return
			}
			for _, err := range errs {
				if strings.Contains(err.Error(), c.want) {
					return
				}
			}
			t.Fatalf("want an error containing %q, got %v", c.want, errs)
		})
	}
}

// TestValidateReportsEveryProblem is the reason Validate returns a slice: a
// sketched schema has several gaps at once, and reporting them one per round
// trip is the slowest way to find that out.
func TestValidateReportsEveryProblem(t *testing.T) {
	errs := Validate(Postgres, schema(), []Change{{
		Kind: CreateTable, Table: "orders", Columns: []Column{
			{Name: "1st", Type: ""},
			{Name: "user_id", Type: "bigint", References: "ghosts.id"},
		},
	}})
	if len(errs) < 4 {
		t.Fatalf("want the name, type, missing key and reference problems, got %d: %v",
			len(errs), errs)
	}
}

func TestPlanRendersPostgres(t *testing.T) {
	stmts, err := Plan(Postgres, schema(), []Change{
		{Kind: CreateTable, Table: "orders", Columns: []Column{
			pkCol("id", "bigserial"),
			{Name: "user_id", Type: "bigint", References: "users.id"},
			{Name: "note", Type: "text", Nullable: true},
			{Name: "code", Type: "text", Unique: true},
			{Name: "created_at", Type: "timestamptz", Default: "now()"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(stmts) != 1 {
		t.Fatalf("want one statement, got %d: %v", len(stmts), stmts)
	}
	sql := stmts[0]
	for _, want := range []string{
		`CREATE TABLE "orders"`,
		`"id" bigserial PRIMARY KEY`,
		`"note" text`,
		`"code" text NOT NULL UNIQUE`,
		`"created_at" timestamptz NOT NULL DEFAULT now()`,
		`FOREIGN KEY ("user_id") REFERENCES "users" ("id")`,
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("want %q in:\n%s", want, sql)
		}
	}
	// A primary key is NOT NULL by definition, and saying both is noise.
	if strings.Contains(sql, `"id" bigserial PRIMARY KEY NOT NULL`) {
		t.Errorf("primary key should not also be spelled NOT NULL:\n%s", sql)
	}
	// Nullable columns carry no constraint at all.
	if strings.Contains(sql, `"note" text NOT NULL`) {
		t.Errorf("nullable column should not be NOT NULL:\n%s", sql)
	}
}

// A single-column key stays inline, because `id INTEGER PRIMARY KEY` is the
// exact spelling that makes the column an alias for rowid on SQLite and
// therefore auto-assigning. A composite key needs its own clause.
func TestPlanCompositePrimaryKey(t *testing.T) {
	stmts, err := Plan(Postgres, schema(), []Change{
		{Kind: CreateTable, Table: "memberships", Columns: []Column{
			pkCol("user_id", "bigint"),
			pkCol("group_id", "bigint"),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	sql := stmts[0]
	if !strings.Contains(sql, `PRIMARY KEY ("user_id", "group_id")`) {
		t.Errorf("want a composite key clause in:\n%s", sql)
	}
	if strings.Contains(sql, `"user_id" bigint PRIMARY KEY`) {
		t.Errorf("composite key should not also be inline:\n%s", sql)
	}
}

func TestPlanSingleKeyStaysInline(t *testing.T) {
	stmts, err := Plan(SQLite, schema(), []Change{
		{Kind: CreateTable, Table: "notes", Columns: []Column{pkCol("id", "INTEGER")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stmts[0], `"id" INTEGER PRIMARY KEY`) {
		t.Errorf("want the rowid-alias spelling in:\n%s", stmts[0])
	}
	if strings.Contains(stmts[0], "PRIMARY KEY (") {
		t.Errorf("a single key needs no separate clause:\n%s", stmts[0])
	}
}

// DROP TABLE needs CASCADE on Postgres or a table with children cannot be
// dropped; SQLite has no such clause.
func TestPlanDropTablePerDialect(t *testing.T) {
	for _, c := range []struct {
		dialect Dialect
		want    string
	}{
		{Postgres, `DROP TABLE IF EXISTS "users" CASCADE`},
		{SQLite, `DROP TABLE IF EXISTS "users"`},
	} {
		stmts, err := Plan(c.dialect, schema(), []Change{{Kind: DropTable, Table: "users"}})
		if err != nil {
			t.Fatal(err)
		}
		if stmts[0] != c.want {
			t.Errorf("%s: want %q, got %q", c.dialect, c.want, stmts[0])
		}
		if c.dialect == SQLite && strings.Contains(stmts[0], "CASCADE") {
			t.Errorf("SQLite has no CASCADE: %q", stmts[0])
		}
	}
}

// A foreign key on an added column is a second statement on Postgres, and an
// inline clause on SQLite, which accepts REFERENCES only in the definition.
func TestPlanAddColumnForeignKeyPerDialect(t *testing.T) {
	changes := []Change{{Kind: AddColumn, Table: "empty", Columns: []Column{
		{Name: "user_id", Type: "bigint", Nullable: true, References: "users.id"},
	}}}

	// Two statements on Postgres, each executable on its own — a caller should
	// never have to split a script on semicolons to apply it.
	pg, err := Plan(Postgres, schema(), changes)
	if err != nil {
		t.Fatal(err)
	}
	if len(pg) != 2 {
		t.Fatalf("want two statements, got %d: %v", len(pg), pg)
	}
	if !strings.Contains(pg[1], `ALTER TABLE "empty" ADD FOREIGN KEY ("user_id")`) {
		t.Errorf("want a separate ADD FOREIGN KEY on Postgres:\n%s", pg[1])
	}
	for _, s := range pg {
		if strings.Contains(s, ";") {
			t.Errorf("a statement should not carry a semicolon: %q", s)
		}
	}

	lite, err := Plan(SQLite, schema(), changes)
	if err != nil {
		t.Fatal(err)
	}
	if len(lite) != 1 {
		t.Fatalf("want one statement on SQLite, got %d: %v", len(lite), lite)
	}
	if !strings.Contains(lite[0], `ADD COLUMN "user_id" bigint REFERENCES "users" ("id")`) {
		t.Errorf("want an inline REFERENCES on SQLite:\n%s", lite[0])
	}
	if strings.Contains(lite[0], "ADD FOREIGN KEY") {
		t.Errorf("SQLite cannot add a foreign key after the fact:\n%s", lite[0])
	}
}

// Creates come before the alters and drops that may depend on them, and a new
// child comes after the new parent it points at whatever order it was sent in.
func TestPlanOrdersStatements(t *testing.T) {
	stmts, err := Plan(Postgres, schema(), []Change{
		{Kind: DropTable, Table: "empty"},
		{Kind: DropColumn, Table: "users", Column: "email"},
		{Kind: CreateTable, Table: "cart_items", Columns: []Column{
			pkCol("id", "bigserial"),
			{Name: "cart_id", Type: "bigint", References: "carts.id"},
		}},
		{Kind: CreateTable, Table: "carts", Columns: []Column{pkCol("id", "bigserial")}},
		{Kind: AddColumn, Table: "users", Columns: []Column{
			{Name: "nick", Type: "text", Nullable: true},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	at := func(want string) int {
		for i, s := range stmts {
			if strings.Contains(s, want) {
				return i
			}
		}
		t.Fatalf("no statement contains %q in %v", want, stmts)
		return -1
	}

	carts := at(`CREATE TABLE "carts"`)
	items := at(`CREATE TABLE "cart_items"`)
	alter := at(`ADD COLUMN "nick"`)
	dropCol := at(`DROP COLUMN "email"`)
	dropTab := at(`DROP TABLE IF EXISTS "empty"`)

	if carts > items {
		t.Errorf("parent must be created before the child: %v", stmts)
	}
	if !(items < alter && alter < dropCol && dropCol < dropTab) {
		t.Errorf("want creates, alters, dropped columns, dropped tables in that order: %v", stmts)
	}
}

// The same edits must produce the same script, or a preview cannot be trusted
// to match what apply runs.
func TestPlanIsDeterministic(t *testing.T) {
	changes := []Change{
		{Kind: CreateTable, Table: "zeta", Columns: []Column{pkCol("id", "bigserial")}},
		{Kind: CreateTable, Table: "alpha", Columns: []Column{pkCol("id", "bigserial")}},
		{Kind: CreateTable, Table: "mid", Columns: []Column{
			pkCol("id", "bigserial"),
			{Name: "alpha_id", Type: "bigint", References: "alpha.id"},
		}},
	}
	first, err := Plan(Postgres, schema(), changes)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		again, err := Plan(Postgres, schema(), changes)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Join(again, ";") != strings.Join(first, ";") {
			t.Fatalf("run %d differs:\n%v\n%v", i, first, again)
		}
	}
}

// Plan refuses an invalid batch rather than rendering SQL for it, so nothing
// downstream has to re-check.
func TestPlanRefusesInvalidChanges(t *testing.T) {
	if _, err := Plan(Postgres, schema(), []Change{
		{Kind: DropColumn, Table: "users", Column: "id"},
	}); err == nil {
		t.Fatal("want an error for dropping a primary key column")
	}
}

func TestValidIdent(t *testing.T) {
	for _, s := range []string{"users", "user_id", "_x", "T2", "a1_b2"} {
		if !validIdent(s) {
			t.Errorf("%q should be usable", s)
		}
	}
	for _, s := range []string{
		"", "1st", "user id", "user-id", `us"ers`, "users;", "users.id",
		strings.Repeat("a", 64),
	} {
		if validIdent(s) {
			t.Errorf("%q should be rejected", s)
		}
	}
	if !validIdent(strings.Repeat("a", 63)) {
		t.Error("63 characters is the limit, not one under it")
	}
}

func TestSplitRef(t *testing.T) {
	cases := []struct {
		ref           string
		table, column string
		ok            bool
	}{
		{"users.id", "users", "id", true},
		{"public.users.id", "public.users", "id", true},
		{"users", "", "", false},
		{".id", "", "", false},
		{"users.", "", "", false},
		{"", "", "", false},
	}
	for _, c := range cases {
		table, column, ok := splitRef(c.ref)
		if ok != c.ok || table != c.table || column != c.column {
			t.Errorf("splitRef(%q) = %q, %q, %v; want %q, %q, %v",
				c.ref, table, column, ok, c.table, c.column, c.ok)
		}
	}
}

func TestDialectForAndTypes(t *testing.T) {
	if got := DialectFor("SQLite"); got != SQLite {
		t.Errorf("want sqlite, got %q", got)
	}
	if got := DialectFor("sqlite"); got != SQLite {
		t.Errorf("the engine name is matched case-insensitively, got %q", got)
	}
	if got := DialectFor("PostgreSQL"); got != Postgres {
		t.Errorf("want postgres, got %q", got)
	}
	// An unknown engine falls back to Postgres rather than to an empty
	// dialect that renders nothing.
	if got := DialectFor("cockroach"); got != Postgres {
		t.Errorf("want postgres for an unknown engine, got %q", got)
	}

	// Every offered type must survive being put in a column definition.
	for _, d := range []Dialect{Postgres, SQLite} {
		types := Types(d)
		if len(types) == 0 {
			t.Fatalf("%s offers no types", d)
		}
		for _, typ := range types {
			def, err := columnDef(d, Column{Name: "c", Type: typ})
			if err != nil {
				t.Errorf("%s %q: %v", d, typ, err)
			}
			if !strings.Contains(def, typ) {
				t.Errorf("%s: %q lost its type: %q", d, typ, def)
			}
		}
	}
}
