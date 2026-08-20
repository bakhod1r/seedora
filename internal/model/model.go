// Package model is the engine-independent description of a database schema and
// of the seeding plan laid over it. Drivers produce a Schema; the planner turns
// it into a Plan; the UI edits the Plan; the seeder executes it.
//
// Nothing here knows about Postgres, SQLite, or Synth. That separation is what
// lets a mapping written against one engine transfer to another.
package model

import "time"

// Schema is a database as introspected: the tables worth seeding, in catalog
// order, plus the enum types their columns refer to.
type Schema struct {
	Tables []*Table          `json:"tables"`
	Enums  map[string]Values `json:"enums,omitempty"`
}

// Values are the labels of a database enum type.
type Values []string

// Table returns the table with the given name, or nil.
func (s *Schema) Table(name string) *Table {
	for _, t := range s.Tables {
		if t.Name == name {
			return t
		}
	}
	return nil
}

// Table is one relation and everything about it that changes how it is seeded.
type Table struct {
	// Schema is the namespace ("public"), empty on engines without one.
	Schema string `json:"schema,omitempty"`
	Name   string `json:"name"`
	// Columns are in ordinal position, which is the order a bulk load must use.
	Columns []*Column `json:"columns"`
	// PrimaryKey lists the columns forming the PK, empty if there is none.
	PrimaryKey []string `json:"primary_key,omitempty"`
	// Rows already present. Seeding an empty table is the normal case; a
	// non-zero count is what the truncate confirmation reports.
	ExistingRows int64 `json:"existing_rows"`
	// Checks are the table's CHECK constraints. A seeder that does not read
	// them generates data the database then refuses, and the refusal arrives
	// mid-load rather than at planning time.
	Checks []*Check `json:"checks,omitempty"`
}

// Check is one CHECK constraint, as the catalog spells it.
//
// Seedora enforces the shapes it can read — a single-column regex becomes a
// pattern generator — and reports the rest rather than pretending. A constraint
// spanning several columns is a statement about a combination, and no
// per-column generator can promise it.
type Check struct {
	Name string `json:"name"`
	// Expr is the constraint body, verbatim from the catalog.
	Expr string `json:"expr"`
	// Columns are the columns the expression mentions, in catalog order.
	Columns []string `json:"columns,omitempty"`
}

// Enforceable reports whether Seedora can satisfy the check by generation. Only
// a single-column constraint is a candidate; anything wider is a relationship
// between columns that per-column generation cannot express.
func (c *Check) Enforceable() bool { return len(c.Columns) == 1 }

// Checks returns the constraints mentioning a column.
func (t *Table) ChecksFor(col string) []*Check {
	var out []*Check
	for _, ck := range t.Checks {
		for _, c := range ck.Columns {
			if c == col {
				out = append(out, ck)
				break
			}
		}
	}
	return out
}

// Qualified is the table name as a statement must spell it.
func (t *Table) Qualified() string {
	if t.Schema == "" {
		return quoteIdent(t.Name)
	}
	return quoteIdent(t.Schema) + "." + quoteIdent(t.Name)
}

// Column returns the named column, or nil.
func (t *Table) Column(name string) *Column {
	for _, c := range t.Columns {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// Column is one column's catalog facts. These are inputs to inference, never
// decisions: what to generate lives in the Plan.
type Column struct {
	Name string `json:"name"`
	// Type is the engine's own type name, verbatim ("varchar", "timestamptz").
	Type string `json:"type"`
	// Native is the fully-spelled declaration ("character varying(255)"), kept
	// for display so the UI can show what the database actually says.
	Native   string `json:"native"`
	Nullable bool   `json:"nullable"`
	// HasDefault means the column fills itself when omitted. Serial and
	// identity columns are the common case, and are skipped by default.
	HasDefault bool `json:"has_default"`
	// Generated marks a column the database computes; it can never be written.
	Generated bool `json:"generated"`
	Unique    bool `json:"unique"`
	// UniqueFold means the uniqueness is over a case-folded value —
	// `CREATE UNIQUE INDEX ON t (lower(nickname))`. Distinct raw values are not
	// enough: "Bob" and "bob" collide.
	UniqueFold bool `json:"unique_fold,omitempty"`
	// AlwaysNull marks a column every partial index on the table requires to be
	// NULL — the soft-delete stamp. Filling it hides the row from every index
	// that exists, which makes a seeded dataset useless for measurement even
	// though the load itself succeeds.
	AlwaysNull bool `json:"always_null,omitempty"`
	// MaxLen is the declared character limit, 0 when unbounded.
	MaxLen int `json:"max_len,omitempty"`
	// Numeric precision and scale, 0 when not applicable.
	Precision int `json:"precision,omitempty"`
	Scale     int `json:"scale,omitempty"`
	// EnumType names the entry in Schema.Enums this column draws from.
	EnumType string `json:"enum_type,omitempty"`
	// FK points at the referenced column when this is a foreign key.
	FK *Ref `json:"fk,omitempty"`
}

// Ref is the target of a foreign key.
type Ref struct {
	Table  string `json:"table"`
	Column string `json:"column"`
}

// Migration is one entry from a schema's history.
//
// A database keeps no record of the ALTERs that were run against it — neither
// Postgres nor SQLite writes DDL anywhere a catalog query can reach. What does
// exist is whatever a migration tool left behind: nearly every one of them
// records what it applied in a table of its own. Reading those tables is the
// only honest way to answer "how did this schema get like this", and it is
// read-only, needs no privileges, and installs nothing.
type Migration struct {
	// Source names the tool the entry came from ("flyway", "goose"), or
	// "seedora" for a change this tool applied itself.
	Source string `json:"source"`
	// Version is the tool's own identifier: a timestamp, a sequence number, a
	// revision hash. Its format is the tool's business.
	Version string `json:"version"`
	// Name describes the change, when the tool recorded one.
	Name string `json:"name,omitempty"`
	// AppliedAt is when it ran, if the tool stored it. Several do not.
	AppliedAt *time.Time `json:"applied_at,omitempty"`
	// Applied is false for an entry the tool marked failed or rolled back, and
	// nil when the tool records no outcome at all — which is not the same as
	// success and must not be displayed as it.
	Applied *bool `json:"applied,omitempty"`
	// Statements are the SQL of a change Seedora applied. Empty for everything
	// read out of another tool's table.
	Statements []string `json:"statements,omitempty"`
}

// Class is the broad shape of a column's type, which is what inference and the
// bulk writers care about — the engine's spelling of it is not.
type Class string

const (
	ClassString  Class = "string"
	ClassInt     Class = "int"
	ClassFloat   Class = "float"
	ClassBool    Class = "bool"
	ClassTime    Class = "time"
	ClassUUID    Class = "uuid"
	ClassJSON    Class = "json"
	ClassBytes   Class = "bytes"
	ClassEnum    Class = "enum"
	ClassUnknown Class = "unknown"
)

// quoteIdent is the SQL-standard double-quote form, which every engine Seedora
// targets accepts. An embedded quote is doubled.
func quoteIdent(s string) string {
	out := make([]byte, 0, len(s)+2)
	out = append(out, '"')
	for i := 0; i < len(s); i++ {
		if s[i] == '"' {
			out = append(out, '"')
		}
		out = append(out, s[i])
	}
	return string(append(out, '"'))
}

// QuoteIdent exposes identifier quoting to drivers.
func QuoteIdent(s string) string { return quoteIdent(s) }
