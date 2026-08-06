package ddl

import (
	"fmt"
	"sort"
	"strings"

	"github.com/bakhod1r/seedora/internal/model"
)

// Script renders a live schema back to the SQL that would recreate it.
//
// This is the schema as the catalog reports it, not the migration that produced
// it: no indexes beyond the keys, no triggers, no grants. It is enough to stand
// the same tables up somewhere else and seed them, which is the job — a tool
// that claimed to reproduce a database from an introspection would be lying
// about what introspection returns.
func Script(d Dialect, s *model.Schema) []string {
	if s == nil {
		return nil
	}

	changes := make([]Change, 0, len(s.Tables))
	for _, t := range s.Tables {
		c := Change{Kind: CreateTable, Table: t.Name}
		pk := map[string]bool{}
		for _, k := range t.PrimaryKey {
			pk[k] = true
		}
		for _, col := range t.Columns {
			out := Column{
				Name:     col.Name,
				Type:     nativeType(col),
				Auto:     isAutoKey(t, col),
				Nullable: col.Nullable,
				PK:       pk[col.Name],
				Unique:   col.Unique && !pk[col.Name],
			}
			if col.FK != nil {
				out.References = col.FK.Table + "." + col.FK.Column
			}
			c.Columns = append(c.Columns, out)
		}
		changes = append(changes, c)
	}

	// Rendered against an empty schema, so every table counts as new and the
	// ordering pass puts parents before children.
	stmts, err := Plan(d, &model.Schema{}, changes)
	if err != nil {
		// A schema that came out of a live database can still fail validation —
		// a table with no primary key is legal and is reported. The statements
		// are worth more than the complaint here, so they are rendered anyway.
		stmts = nil
		for _, c := range changes {
			if stmt, err := createTable(d, c); err == nil {
				stmts = append(stmts, stmt)
			}
		}
	}
	return stmts
}

// isAutoKey reports a single-column integer primary key that fills itself.
//
// The catalog does not always carry the auto-assignment in the type: MySQL
// reports `bigint` for an AUTO_INCREMENT column and records the rest elsewhere,
// and a script rendered from the type alone recreates the table with a key
// nobody assigns — which fails on the first insert. Postgres reports the
// sequence as a default, which is the same fact in a different place.
func isAutoKey(t *model.Table, col *model.Column) bool {
	return col.HasDefault && len(t.PrimaryKey) == 1 && t.PrimaryKey[0] == col.Name &&
		strings.Contains(strings.ToLower(col.Type), "int")
}

// nativeType prefers the fully-spelled declaration the catalog reports, since
// `character varying(255)` carries a limit that `varchar` alone does not.
func nativeType(c *model.Column) string {
	if strings.TrimSpace(c.Native) != "" {
		return c.Native
	}
	return c.Type
}

// Mermaid renders the schema as a Mermaid entity-relationship diagram.
//
// It is the diagram this tool draws, in a form that can be pasted into a
// README, a pull request, or a wiki — all of which render Mermaid, and none of
// which will run a Go binary to look at a schema.
func Mermaid(s *model.Schema) string {
	if s == nil {
		return "erDiagram\n"
	}

	var b strings.Builder
	b.WriteString("erDiagram\n")

	for _, t := range s.Tables {
		pk := map[string]bool{}
		for _, k := range t.PrimaryKey {
			pk[k] = true
		}

		fmt.Fprintf(&b, "  %s {\n", mermaidIdent(t.Name))
		for _, c := range t.Columns {
			var marks []string
			if pk[c.Name] {
				marks = append(marks, "PK")
			}
			if c.FK != nil {
				marks = append(marks, "FK")
			}
			if c.Unique && !pk[c.Name] {
				marks = append(marks, "UK")
			}
			line := fmt.Sprintf("    %s %s", mermaidType(c), mermaidIdent(c.Name))
			if len(marks) > 0 {
				line += " " + strings.Join(marks, ",")
			}
			b.WriteString(line + "\n")
		}
		b.WriteString("  }\n")
	}

	// Relationships last, so the entity blocks read as a list of tables and the
	// joins as a list of joins.
	for _, rel := range relationships(s) {
		fmt.Fprintf(&b, "  %s\n", rel)
	}
	return b.String()
}

func relationships(s *model.Schema) []string {
	var out []string
	for _, t := range s.Tables {
		pk := map[string]bool{}
		for _, k := range t.PrimaryKey {
			pk[k] = true
		}
		for _, c := range t.Columns {
			if c.FK == nil {
				continue
			}
			// The same rule the diagram uses: a unique key, or a key that is the
			// whole primary key, can only point at one parent row.
			one := c.Unique || (len(t.PrimaryKey) == 1 && pk[c.Name])
			// Optional on the child side when the column is nullable, which is
			// the difference between "has one" and "may have one".
			mark := "o{"
			if one {
				mark = "||"
			}
			left := "||"
			if c.Nullable {
				left = "|o"
			}
			out = append(out, fmt.Sprintf("  %s %s--%s %s : %s",
				mermaidIdent(c.FK.Table), left, mark, mermaidIdent(t.Name), mermaidIdent(c.Name)))
		}
	}
	sort.Strings(out)
	return out
}

// mermaidType flattens a column's declaration to the single token Mermaid
// accepts in an entity block: it has no syntax for `numeric(10,2)`.
func mermaidType(c *model.Column) string {
	typ := c.Type
	if typ == "" {
		typ = "unknown"
	}
	if i := strings.IndexAny(typ, "( "); i > 0 {
		typ = typ[:i]
	}
	return mermaidIdent(typ)
}

// mermaidIdent replaces the characters Mermaid reads as syntax. Names in a
// catalog are already tame; a schema-qualified one carries a dot, which is not.
func mermaidIdent(s string) string {
	if s == "" {
		return "unnamed"
	}
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			out = append(out, r)
		default:
			out = append(out, '_')
		}
	}
	return string(out)
}
