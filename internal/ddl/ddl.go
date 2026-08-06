// Package ddl turns schema edits made in the diagram into SQL, and checks them
// before anything runs.
//
// Seedora is a seeding tool, so the editing it supports is deliberately narrow:
// create a table, add or drop a column, drop a table. That covers sketching a
// schema to seed against, which is the job. It does not try to be a migration
// tool — changing a column's type, reordering, or rewriting constraints is where
// engines stop agreeing with each other and where getting it wrong costs data.
//
// Every change is rendered to SQL the user can read before it is applied, and
// applied inside the transaction the caller opens. Nothing here executes.
package ddl

import (
	"fmt"
	"sort"
	"strings"

	"github.com/bakhod1r/seedora/internal/model"
)

// Kind is the sort of change being made.
type Kind string

const (
	CreateTable Kind = "create_table"
	DropTable   Kind = "drop_table"
	AddColumn   Kind = "add_column"
	DropColumn  Kind = "drop_column"
	// AddForeignKey constrains a column that already exists to point at
	// another table's key. It is separate from AddColumn because the column is
	// already there and only the constraint is new — the case where a schema
	// grew a relationship somebody drew after the fact.
	AddForeignKey Kind = "add_foreign_key"
	// AddUnique constrains a column that already exists to hold no value twice.
	// It is what turns a one-to-many into a one-to-one, and it is a unique
	// index rather than a table constraint because that spelling is the one all
	// three engines accept on a table that already has rows.
	AddUnique Kind = "add_unique"
)

// Change is one edit to the schema.
type Change struct {
	Kind  Kind   `json:"kind"`
	Table string `json:"table"`

	// Columns is the full column list for CreateTable, or the single column for
	// AddColumn.
	Columns []Column `json:"columns,omitempty"`
	// Column names the target of DropColumn and of AddForeignKey.
	Column string `json:"column,omitempty"`
	// References is "table.column", the parent an AddForeignKey points at.
	References string `json:"references,omitempty"`
}

// Column is a column as the diagram describes it. It is a smaller thing than
// model.Column: that one reports what a database has, this one states what the
// user wants, and the difference is everything the engine fills in.
type Column struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Nullable bool   `json:"nullable"`
	PK       bool   `json:"pk"`
	Unique   bool   `json:"unique"`
	// Auto marks a key the database assigns itself. It is a flag rather than
	// part of the type because every engine spells it somewhere different —
	// in the type on Postgres (`bigserial`), in a keyword after it on MySQL
	// (`AUTO_INCREMENT`), and nowhere at all on SQLite, where `INTEGER PRIMARY
	// KEY` already means it.
	Auto bool `json:"auto,omitempty"`
	// Default is written verbatim into the DDL, so it can be an expression.
	Default string `json:"default,omitempty"`
	// References is "table.column" for a foreign key.
	References string `json:"references,omitempty"`
}

// Dialect is the SQL an engine accepts. The differences that matter here are
// small and few, which is why this is a string and a handful of switches rather
// than an interface per engine.
type Dialect string

const (
	Postgres Dialect = "postgres"
	SQLite   Dialect = "sqlite"
	MySQL    Dialect = "mysql"
)

// Plan renders changes to SQL, in an order that works: tables are created before
// anything references them, and dropped in the reverse.
func Plan(d Dialect, s *model.Schema, changes []Change) ([]string, error) {
	if errs := Validate(d, s, changes); len(errs) > 0 {
		return nil, errs[0]
	}

	var creates, alters, dropCols, dropTables []string

	for _, c := range changes {
		switch c.Kind {
		case CreateTable:
			stmt, err := createTable(d, c)
			if err != nil {
				return nil, err
			}
			creates = append(creates, stmt)

		case AddColumn:
			for _, col := range c.Columns {
				stmts, err := addColumn(d, c.Table, col)
				if err != nil {
					return nil, err
				}
				alters = append(alters, stmts...)
			}

		case AddUnique:
			alters = append(alters, fmt.Sprintf(
				"CREATE UNIQUE INDEX %s ON %s (%s)",
				quoteAs(d, indexName(c.Table, c.Column)), quoteAs(d, c.Table), quoteAs(d, c.Column)))

		case AddForeignKey:
			t, rc, ok := splitRef(c.References)
			if !ok {
				return nil, fmt.Errorf("%s.%s: references must be table.column, got %q",
					c.Table, c.Column, c.References)
			}
			alters = append(alters, fmt.Sprintf(
				"ALTER TABLE %s ADD FOREIGN KEY (%s) REFERENCES %s (%s)",
				quoteAs(d, c.Table), quoteAs(d, c.Column), quoteAs(d, t), quoteAs(d, rc)))

		case DropColumn:
			dropCols = append(dropCols,
				fmt.Sprintf("ALTER TABLE %s DROP COLUMN %s",
					quoteAs(d, c.Table), quoteAs(d, c.Column)))

		case DropTable:
			// CASCADE on Postgres because a table with children cannot be
			// dropped without it; SQLite has no such clause and drops the table
			// with its own indexes, leaving foreign keys to fail loudly if they
			// are enforced.
			if d == Postgres {
				dropTables = append(dropTables,
					fmt.Sprintf("DROP TABLE IF EXISTS %s CASCADE", quoteAs(d, c.Table)))
			} else {
				dropTables = append(dropTables,
					fmt.Sprintf("DROP TABLE IF EXISTS %s", quoteAs(d, c.Table)))
			}

		default:
			return nil, fmt.Errorf("unknown change %q", c.Kind)
		}
	}

	// Creates first — a new child may reference a new parent. Drops last, so a
	// column being dropped is not referenced by something also being dropped.
	out := append([]string{}, sortCreates(creates, changes)...)
	out = append(out, alters...)
	out = append(out, dropCols...)
	out = append(out, dropTables...)
	return out, nil
}

// sortCreates orders CREATE TABLE statements so a table comes after everything
// it references. Two new tables referencing each other cannot be ordered, and
// the cycle is reported by Validate before this runs.
func sortCreates(stmts []string, changes []Change) []string {
	type item struct {
		stmt string
		name string
		refs map[string]bool
	}

	var items []item
	i := 0
	for _, c := range changes {
		if c.Kind != CreateTable {
			continue
		}
		refs := map[string]bool{}
		for _, col := range c.Columns {
			if col.References == "" {
				continue
			}
			if t, _, ok := splitRef(col.References); ok && t != c.Table {
				refs[t] = true
			}
		}
		items = append(items, item{stmt: stmts[i], name: c.Table, refs: refs})
		i++
	}

	// Only references to other tables in this same batch impose an order; a
	// reference to a table that already exists is satisfied already.
	inBatch := map[string]bool{}
	for _, it := range items {
		inBatch[it.name] = true
	}

	var out []string
	done := map[string]bool{}
	for len(out) < len(items) {
		progress := false
		// Alphabetical among the ready ones, so the same edits always produce
		// the same script.
		sort.Slice(items, func(a, b int) bool { return items[a].name < items[b].name })
		for _, it := range items {
			if done[it.name] {
				continue
			}
			ready := true
			for r := range it.refs {
				if inBatch[r] && !done[r] {
					ready = false
					break
				}
			}
			if !ready {
				continue
			}
			out = append(out, it.stmt)
			done[it.name] = true
			progress = true
		}
		if !progress {
			// A cycle. Validate rejects these, so reaching here means the batch
			// changed underneath us; emitting the rest unordered is better than
			// looping.
			for _, it := range items {
				if !done[it.name] {
					out = append(out, it.stmt)
				}
			}
			break
		}
	}
	return out
}

func createTable(d Dialect, c Change) (string, error) {
	if len(c.Columns) == 0 {
		return "", fmt.Errorf("table %s has no columns", c.Table)
	}

	var lines []string
	var pk []string

	// A composite key is one clause at the end, so no column may also carry an
	// inline PRIMARY KEY — two of them in one CREATE TABLE is a syntax error on
	// both engines.
	keys := 0
	for _, col := range c.Columns {
		if col.PK {
			keys++
		}
	}
	composite := keys > 1

	for _, col := range c.Columns {
		if col.PK {
			pk = append(pk, quoteAs(d, col.Name))
			if composite {
				col.PK = false
			}
		}
		line, err := columnDef(d, col)
		if err != nil {
			return "", fmt.Errorf("%s.%s: %w", c.Table, col.Name, err)
		}
		lines = append(lines, "  "+line)
	}

	// A single-column key is written inline by columnDef; a composite one needs
	// its own clause. Doing it this way keeps `id INTEGER PRIMARY KEY` intact on
	// SQLite, where that exact spelling is what makes the column an alias for
	// rowid and therefore auto-assigning.
	if composite {
		lines = append(lines, "  PRIMARY KEY ("+strings.Join(pk, ", ")+")")
	}

	for _, col := range c.Columns {
		if col.References == "" {
			continue
		}
		t, rc, ok := splitRef(col.References)
		if !ok {
			return "", fmt.Errorf("%s.%s: references must be table.column, got %q",
				c.Table, col.Name, col.References)
		}
		lines = append(lines, fmt.Sprintf("  FOREIGN KEY (%s) REFERENCES %s (%s)",
			quoteAs(d, col.Name), quoteAs(d, t), quoteAs(d, rc)))
	}

	return fmt.Sprintf("CREATE TABLE %s (\n%s\n)", quoteAs(d, c.Table), strings.Join(lines, ",\n")), nil
}

func columnDef(d Dialect, col Column) (string, error) {
	if col.Name == "" {
		return "", fmt.Errorf("column has no name")
	}
	typ := strings.TrimSpace(col.Type)
	if typ == "" {
		return "", fmt.Errorf("column has no type")
	}

	var b strings.Builder
	b.WriteString(quoteAs(d, col.Name))
	b.WriteString(" ")
	b.WriteString(typ)

	// An auto-assigned key, spelled the way this engine spells it. SQLite needs
	// nothing: `INTEGER PRIMARY KEY` is already an alias for rowid, and adding
	// AUTOINCREMENT there only costs a table of used keys.
	if col.Auto && !hasAuto(typ) {
		switch d {
		case MySQL:
			b.WriteString(" AUTO_INCREMENT")
		case Postgres:
			b.WriteString(" GENERATED BY DEFAULT AS IDENTITY")
		}
	}

	if col.PK {
		// PRIMARY KEY implies NOT NULL everywhere, and saying both is noise.
		b.WriteString(" PRIMARY KEY")
	} else {
		if !col.Nullable {
			b.WriteString(" NOT NULL")
		}
		if col.Unique {
			b.WriteString(" UNIQUE")
		}
	}
	if col.Default != "" {
		b.WriteString(" DEFAULT ")
		b.WriteString(col.Default)
	}
	return b.String(), nil
}

// hasAuto reports whether the type already carries the auto-assignment, which
// is how Postgres spells it: `bigserial` is a bigint plus a sequence.
func hasAuto(typ string) bool {
	t := strings.ToLower(typ)
	return strings.Contains(t, "serial") || strings.Contains(t, "auto_increment") ||
		strings.Contains(t, "identity") || strings.Contains(t, "generated")
}

// addColumn returns one statement, or two: a foreign key on an added column is
// a separate ALTER on Postgres. Each element is executed on its own, so nothing
// downstream has to split a script on semicolons.
func addColumn(d Dialect, table string, col Column) ([]string, error) {
	def, err := columnDef(d, col)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", table, err)
	}
	stmt := fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s", quoteAs(d, table), def)

	// SQLite cannot add a foreign key after the fact — ALTER TABLE ADD COLUMN
	// there accepts a REFERENCES clause only in the column definition, which
	// columnDef does not emit.
	if col.References != "" {
		t, rc, ok := splitRef(col.References)
		if !ok {
			return nil, fmt.Errorf("%s.%s: references must be table.column", table, col.Name)
		}
		// MySQL parses an inline REFERENCES in a column definition and then
		// silently ignores it, so the constraint has to be its own statement
		// there too — a foreign key that quietly does not exist is worse than
		// one that fails loudly.
		if d == Postgres || d == MySQL {
			return []string{stmt, fmt.Sprintf(
				"ALTER TABLE %s ADD FOREIGN KEY (%s) REFERENCES %s (%s)",
				quoteAs(d, table), quoteAs(d, col.Name), quoteAs(d, t), quoteAs(d, rc))}, nil
		}
		return []string{stmt + fmt.Sprintf(" REFERENCES %s (%s)", quoteAs(d, t), quoteAs(d, rc))}, nil
	}
	return []string{stmt}, nil
}

// Validate reports every problem with a batch of changes, rather than the first.
// A schema sketched in a diagram usually has several gaps at once, and fixing
// them one round trip at a time is the slowest way to find that out.
func Validate(d Dialect, s *model.Schema, changes []Change) []error {
	var errs []error
	fail := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}

	// The schema as it will be once these changes land, so a new table can be
	// referenced by another new table.
	created := map[string]map[string]bool{}
	dropped := map[string]bool{}

	for _, c := range changes {
		switch c.Kind {
		case CreateTable:
			if !validIdent(c.Table) {
				fail("%q is not a usable table name", c.Table)
			}
			if s.Table(c.Table) != nil {
				fail("table %s already exists", c.Table)
			}
			if len(c.Columns) == 0 {
				fail("table %s has no columns", c.Table)
			}
			cols := map[string]bool{}
			pkCount := 0
			for _, col := range c.Columns {
				if !validIdent(col.Name) {
					fail("%s: %q is not a usable column name", c.Table, col.Name)
				}
				if cols[col.Name] {
					fail("%s: column %s is defined twice", c.Table, col.Name)
				}
				cols[col.Name] = true
				if strings.TrimSpace(col.Type) == "" {
					fail("%s.%s has no type", c.Table, col.Name)
				}
				if col.PK {
					pkCount++
				}
			}
			if pkCount == 0 {
				// Not fatal — a table without a key is legal — but it means
				// nothing can reference it, which is usually a mistake in a
				// schema someone is drawing.
				fail("%s has no primary key, so nothing can reference it", c.Table)
			}
			created[c.Table] = cols

		case DropTable:
			if s.Table(c.Table) == nil {
				fail("table %s does not exist", c.Table)
			}
			dropped[c.Table] = true

		case AddColumn:
			t := s.Table(c.Table)
			if t == nil {
				fail("table %s does not exist", c.Table)
				continue
			}
			for _, col := range c.Columns {
				if !validIdent(col.Name) {
					fail("%s: %q is not a usable column name", c.Table, col.Name)
				}
				if t.Column(col.Name) != nil {
					fail("%s already has a column named %s", c.Table, col.Name)
				}
				if strings.TrimSpace(col.Type) == "" {
					fail("%s.%s has no type", c.Table, col.Name)
				}
				// The rule every engine shares: an existing row needs a value
				// for a new NOT NULL column, and only a default can supply one.
				if !col.Nullable && col.Default == "" && t.ExistingRows > 0 {
					fail("%s.%s is NOT NULL with no default, and %s already has rows",
						c.Table, col.Name, c.Table)
				}
				if col.Unique && d == SQLite {
					fail("SQLite cannot add a UNIQUE column to an existing table — "+
						"create it unique, or add the column and a unique index separately (%s.%s)",
						c.Table, col.Name)
				}
			}

		case AddUnique:
			t := s.Table(c.Table)
			if t == nil {
				fail("table %s does not exist", c.Table)
				continue
			}
			col := t.Column(c.Column)
			switch {
			case col == nil:
				fail("%s has no column named %s", c.Table, c.Column)
			case col.Unique:
				fail("%s.%s is already unique", c.Table, c.Column)
			case t.ExistingRows > 0:
				// The engine will refuse it if the rows already repeat, and it
				// will do so after the transaction has done other work. This is
				// a warning in the same place as everything else, not a refusal
				// the user cannot override — so it is only raised when there is
				// something to break.
				fail("%s already has %d rows — the index will fail if any two of them "+
					"share a %s. Truncate first, or check the data.",
					c.Table, t.ExistingRows, c.Column)
			}

		case AddForeignKey:
			// SQLite has no ALTER TABLE ADD CONSTRAINT at all: a foreign key
			// there can only be declared when the column is, and adding one
			// afterwards means rebuilding the table and copying every row.
			// Refusing is the honest answer; the mapping-level relationship
			// still works and needs no DDL.
			if d == SQLite {
				fail("SQLite cannot add a foreign key to a column that already exists — " +
					"the relationship still works as a mapping, or recreate the table with it")
				continue
			}
			t := s.Table(c.Table)
			if t == nil {
				fail("table %s does not exist", c.Table)
				continue
			}
			child := t.Column(c.Column)
			if child == nil {
				fail("%s has no column named %s", c.Table, c.Column)
				continue
			}
			rt, rc, ok := splitRef(c.References)
			if !ok {
				fail("%s.%s: references must be table.column, got %q",
					c.Table, c.Column, c.References)
				continue
			}
			parent := s.Table(rt)
			switch {
			case parent == nil:
				fail("%s.%s references unknown table %s", c.Table, c.Column, rt)
			case parent.Column(rc) == nil:
				fail("%s.%s references unknown column %s.%s", c.Table, c.Column, rt, rc)
			default:
				// The engine will refuse a constraint that existing rows break,
				// and it will do it after the transaction has done other work.
				// Saying it here costs one check and reports it with the rest.
				p := parent.Column(rc)
				if !p.Unique && !isPrimaryKey(parent, rc) {
					fail("%s.%s is neither a primary key nor unique, so it cannot be referenced", rt, rc)
				}
			}

		case DropColumn:
			t := s.Table(c.Table)
			if t == nil {
				fail("table %s does not exist", c.Table)
				continue
			}
			if t.Column(c.Column) == nil {
				fail("%s has no column named %s", c.Table, c.Column)
				continue
			}
			for _, k := range t.PrimaryKey {
				if k == c.Column {
					fail("%s.%s is part of the primary key and cannot be dropped",
						c.Table, c.Column)
				}
			}

		default:
			fail("unknown change %q", c.Kind)
		}
	}

	// References are checked last, against the schema as it will be.
	for _, c := range changes {
		if c.Kind != CreateTable && c.Kind != AddColumn {
			continue
		}
		for _, col := range c.Columns {
			if col.References == "" {
				continue
			}
			rt, rc, ok := splitRef(col.References)
			if !ok {
				fail("%s.%s: references must be table.column, got %q",
					c.Table, col.Name, col.References)
				continue
			}
			if dropped[rt] {
				fail("%s.%s references %s, which is being dropped", c.Table, col.Name, rt)
				continue
			}
			if cols, isNew := created[rt]; isNew {
				if !cols[rc] {
					fail("%s.%s references %s.%s, which is not one of that table's columns",
						c.Table, col.Name, rt, rc)
				}
				continue
			}
			parent := s.Table(rt)
			if parent == nil {
				fail("%s.%s references unknown table %s", c.Table, col.Name, rt)
			} else if parent.Column(rc) == nil {
				fail("%s.%s references unknown column %s.%s", c.Table, col.Name, rt, rc)
			}
		}
	}

	if cyc := cycle(changes); cyc != "" {
		fail("new tables %s reference each other, so neither can be created first — "+
			"make one of the keys nullable and add it afterwards", cyc)
	}
	return errs
}

// indexName is the conventional name for a unique index on one column, the
// same one Postgres generates for a UNIQUE constraint.
func indexName(table, column string) string {
	return table + "_" + column + "_key"
}

func isPrimaryKey(t *model.Table, col string) bool {
	for _, k := range t.PrimaryKey {
		if k == col {
			return true
		}
	}
	return false
}

// cycle reports a reference cycle among newly created tables, which cannot be
// ordered into a script.
func cycle(changes []Change) string {
	deps := map[string]map[string]bool{}
	for _, c := range changes {
		if c.Kind != CreateTable {
			continue
		}
		deps[c.Table] = map[string]bool{}
	}
	for _, c := range changes {
		if c.Kind != CreateTable {
			continue
		}
		for _, col := range c.Columns {
			if col.References == "" {
				continue
			}
			if t, _, ok := splitRef(col.References); ok && t != c.Table {
				if _, isNew := deps[t]; isNew {
					deps[c.Table][t] = true
				}
			}
		}
	}

	for len(deps) > 0 {
		var ready []string
		for name, d := range deps {
			if len(d) == 0 {
				ready = append(ready, name)
			}
		}
		if len(ready) == 0 {
			var stuck []string
			for name := range deps {
				stuck = append(stuck, name)
			}
			sort.Strings(stuck)
			return strings.Join(stuck, " and ")
		}
		for _, name := range ready {
			delete(deps, name)
		}
		for _, d := range deps {
			for _, name := range ready {
				delete(d, name)
			}
		}
	}
	return ""
}

// validIdent accepts the names a schema actually uses. It is strict on purpose:
// these strings end up in generated SQL, and a name needing exotic quoting is
// far more likely to be a typo than a deliberate choice.
func validIdent(s string) bool {
	if s == "" || len(s) > 63 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_' ||
			(i > 0 && c >= '0' && c <= '9')
		if !ok {
			return false
		}
	}
	return true
}

func splitRef(ref string) (table, column string, ok bool) {
	i := strings.LastIndexByte(ref, '.')
	if i <= 0 || i == len(ref)-1 {
		return "", "", false
	}
	return ref[:i], ref[i+1:], true
}

// quoteAs quotes an identifier the way the target engine spells it. Backticks
// on MySQL rather than double quotes, because a double-quoted identifier there
// is a string literal unless the session sets ANSI_QUOTES — which the driver
// does, but a script exported from here is meant to run anywhere.
func quoteAs(d Dialect, s string) string {
	if d == MySQL {
		return "`" + strings.ReplaceAll(s, "`", "``") + "`"
	}
	return model.QuoteIdent(s)
}

// Types lists the column types offered in the diagram, per dialect. A short
// list of the types a seeded schema actually uses beats every type the engine
// has: the rest are a search problem, not a choice.
func Types(d Dialect) []string {
	switch d {
	case SQLite:
		return []string{
			"INTEGER", "TEXT", "VARCHAR(255)", "VARCHAR(50)", "REAL",
			"DECIMAL(10,2)", "BOOLEAN", "TIMESTAMP", "DATE", "BLOB",
		}
	case MySQL:
		// AUTO_INCREMENT is offered as part of the type because that is where
		// MySQL wants it, and a key column that does not fill itself is the
		// single most common thing to get wrong when sketching a table here.
		return []string{
			"BIGINT AUTO_INCREMENT", "BIGINT", "INT", "VARCHAR(255)", "VARCHAR(50)",
			"TEXT", "BOOLEAN", "DATETIME", "DATE", "DECIMAL(10,2)", "DOUBLE",
			"CHAR(36)", "JSON", "BLOB",
		}
	default:
		return []string{
			"bigserial", "bigint", "integer", "text", "varchar(255)", "varchar(50)",
			"boolean", "timestamptz", "date", "numeric(10,2)", "double precision",
			"uuid", "jsonb", "bytea",
		}
	}
}

// DialectFor maps a driver's name to its dialect.
func DialectFor(engine string) Dialect {
	switch {
	case strings.EqualFold(engine, "SQLite"):
		return SQLite
	case strings.EqualFold(engine, "MySQL"), strings.EqualFold(engine, "MariaDB"):
		return MySQL
	}
	return Postgres
}
