package ddl

import (
	"strings"
	"testing"

	"github.com/bakhod1r/seedora/internal/model"
)

const script = `
-- A schema as it comes out of a repository: comments, an index, and a
-- statement this package has no opinion about.
CREATE TABLE IF NOT EXISTS "users" (
  id          bigserial PRIMARY KEY,
  email       varchar(120) NOT NULL UNIQUE,
  nickname    text,
  balance     numeric(10,2) NOT NULL DEFAULT 0,
  created_at  timestamp with time zone NOT NULL DEFAULT now()
);

CREATE INDEX users_email_idx ON users (email);

CREATE TABLE orders (
  id       bigserial,
  user_id  bigint NOT NULL REFERENCES users (id),
  note     text /* inline block comment */,
  total    double precision NOT NULL,
  PRIMARY KEY (id)
);

CREATE TABLE memberships (
  user_id  bigint NOT NULL,
  group_id bigint NOT NULL,
  PRIMARY KEY (user_id, group_id),
  FOREIGN KEY (user_id) REFERENCES users (id)
);
`

func parsed(t *testing.T) map[string]Change {
	t.Helper()
	changes, err := Parse(script)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]Change{}
	for _, c := range changes {
		if c.Kind != CreateTable {
			t.Fatalf("%s: want a create, got %q", c.Table, c.Kind)
		}
		out[c.Table] = c
	}
	return out
}

func column(t *testing.T, c Change, name string) Column {
	t.Helper()
	for _, col := range c.Columns {
		if col.Name == name {
			return col
		}
	}
	t.Fatalf("%s has no column %s", c.Table, name)
	return Column{}
}

func TestParseReadsTablesAndSkipsTheRest(t *testing.T) {
	tables := parsed(t)
	for _, want := range []string{"users", "orders", "memberships"} {
		if _, ok := tables[want]; !ok {
			t.Errorf("missing table %s", want)
		}
	}
	if len(tables) != 3 {
		t.Errorf("the CREATE INDEX should be skipped, got %d tables", len(tables))
	}
}

// The type is every token up to the first constraint keyword, or
// `double precision` and `timestamp with time zone` lose most of themselves.
func TestParseReadsMultiWordAndParameterisedTypes(t *testing.T) {
	tables := parsed(t)
	for _, c := range []struct{ table, column, want string }{
		{"users", "balance", "numeric(10,2)"},
		{"users", "email", "varchar(120)"},
		{"users", "created_at", "timestamp with time zone"},
		{"orders", "total", "double precision"},
	} {
		if got := column(t, tables[c.table], c.column).Type; got != c.want {
			t.Errorf("%s.%s: got %q, want %q", c.table, c.column, got, c.want)
		}
	}
}

func TestParseReadsColumnConstraints(t *testing.T) {
	users := parsed(t)["users"]

	id := column(t, users, "id")
	if !id.PK || id.Nullable {
		t.Errorf("id should be a non-null primary key: %+v", id)
	}
	email := column(t, users, "email")
	if email.Nullable || !email.Unique {
		t.Errorf("email should be NOT NULL UNIQUE: %+v", email)
	}
	if nick := column(t, users, "nickname"); !nick.Nullable {
		t.Errorf("a column with no NOT NULL is nullable: %+v", nick)
	}
	if bal := column(t, users, "balance"); bal.Default != "0" {
		t.Errorf("balance default: got %q, want 0", bal.Default)
	}
}

func TestParseReadsBothFormsOfForeignKey(t *testing.T) {
	tables := parsed(t)

	if got := column(t, tables["orders"], "user_id").References; got != "users.id" {
		t.Errorf("inline REFERENCES: got %q", got)
	}
	if got := column(t, tables["memberships"], "user_id").References; got != "users.id" {
		t.Errorf("table-level FOREIGN KEY: got %q", got)
	}
}

// A key declared at the table level belongs to the columns it names, whether
// there is one of them or two.
func TestParseReadsTableLevelPrimaryKeys(t *testing.T) {
	tables := parsed(t)

	if !column(t, tables["orders"], "id").PK {
		t.Error("orders.id is the table's primary key")
	}
	m := tables["memberships"]
	if !column(t, m, "user_id").PK || !column(t, m, "group_id").PK {
		t.Error("both halves of a composite key are primary")
	}
}

// A comment can sit anywhere, including inside a column list, and taking the
// rest of the line with it would silently drop a column.
func TestParseIgnoresComments(t *testing.T) {
	orders := parsed(t)["orders"]
	if column(t, orders, "note").Type != "text" {
		t.Errorf("a block comment should not join the type: %+v", column(t, orders, "note"))
	}
}

func TestParseRejectsAFileWithNoTables(t *testing.T) {
	if _, err := Parse("SELECT 1; CREATE INDEX x ON y (z);"); err == nil {
		t.Fatal("want an error when there is nothing to create")
	}
}

// What Parse reads, Plan must be able to render, or importing a file would
// produce changes that cannot be applied.
func TestParsedChangesRender(t *testing.T) {
	changes, err := Parse(script)
	if err != nil {
		t.Fatal(err)
	}
	stmts, err := Plan(Postgres, &model.Schema{}, changes)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(stmts, "\n")
	for _, want := range []string{
		`CREATE TABLE "users"`,
		`"balance" numeric(10,2) NOT NULL DEFAULT 0`,
		`FOREIGN KEY ("user_id") REFERENCES "users" ("id")`,
		`PRIMARY KEY ("user_id", "group_id")`,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("want %q in:\n%s", want, joined)
		}
	}
	// users has to come before orders, which points at it.
	if strings.Index(joined, `CREATE TABLE "users"`) > strings.Index(joined, `CREATE TABLE "orders"`) {
		t.Error("the parent must be created first")
	}
}
