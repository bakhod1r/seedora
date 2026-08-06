package ddl

import (
	"fmt"
	"strings"
)

// Parse reads CREATE TABLE statements and returns them as changes, so a schema
// that exists as a .sql file can be created in the database the tool is pointed
// at and then seeded.
//
// It is not a SQL parser. It reads the subset this package can also write —
// columns, their types, NOT NULL, PRIMARY KEY, UNIQUE, DEFAULT, REFERENCES —
// and ignores everything else in the file rather than failing on it. A schema
// file usually carries indexes, extensions, and grants alongside its tables,
// and refusing the whole file over a line that does not describe a column would
// make the feature useless on every real file it would be pointed at.
func Parse(sql string) ([]Change, error) {
	var out []Change

	for _, stmt := range statements(stripComments(sql)) {
		body, name, ok := createTableParts(stmt)
		if !ok {
			continue
		}
		c, err := parseCreateTable(name, body)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no CREATE TABLE statements found")
	}
	return out, nil
}

func parseCreateTable(name, body string) (Change, error) {
	c := Change{Kind: CreateTable, Table: name}

	// Table-level constraints are collected first, because a composite primary
	// key and a table-level FOREIGN KEY clause both describe columns that were
	// already declared above them.
	var pk []string
	unique := map[string]bool{}
	refs := map[string]string{}

	var cols []Column
	for _, item := range splitTop(body, ',') {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}

		switch keyword(item) {
		case "primary":
			pk = append(pk, identsIn(item)...)
			continue
		case "unique":
			for _, u := range identsIn(item) {
				unique[u] = true
			}
			continue
		case "foreign":
			cols, target := foreignKeyClause(item)
			for _, col := range cols {
				if target != "" {
					refs[col] = target
				}
			}
			continue
		case "constraint", "check", "exclude":
			// Named and checked constraints carry no information this package
			// can act on, and a check expression can contain anything.
			continue
		}

		col, ok := parseColumn(item)
		if !ok {
			continue
		}
		cols = append(cols, col)
	}

	if len(cols) == 0 {
		return c, fmt.Errorf("table %s: no columns could be read", name)
	}

	inPK := map[string]bool{}
	for _, k := range pk {
		inPK[k] = true
	}
	for i := range cols {
		if inPK[cols[i].Name] {
			cols[i].PK = true
		}
		if unique[cols[i].Name] {
			cols[i].Unique = true
		}
		if r, ok := refs[cols[i].Name]; ok && cols[i].References == "" {
			cols[i].References = r
		}
		// A primary key is NOT NULL by definition, and carrying both would
		// render as a contradiction on the way back out.
		if cols[i].PK {
			cols[i].Nullable = false
			cols[i].Unique = false
		}
	}
	c.Columns = cols
	return c, nil
}

// parseColumn reads one column definition: a name, a type, and the modifiers
// this package understands.
func parseColumn(def string) (Column, bool) {
	fields := splitTop(def, ' ')
	var toks []string
	for _, f := range fields {
		if f = strings.TrimSpace(f); f != "" {
			toks = append(toks, f)
		}
	}
	if len(toks) < 2 {
		return Column{}, false
	}

	col := Column{Name: unquote(toks[0]), Nullable: true}

	// The type is every token up to the first modifier keyword. `double
	// precision`, `timestamp with time zone`, and `character varying(255)` are
	// all several tokens long.
	i := 1
	var typ []string
	for ; i < len(toks); i++ {
		if isModifier(toks[i]) {
			break
		}
		typ = append(typ, toks[i])
	}
	col.Type = strings.Join(typ, " ")
	if col.Type == "" {
		return Column{}, false
	}
	// Postgres carries the auto-assignment in the type, so a `serial` column is
	// an auto key even though no modifier says so.
	if hasAuto(col.Type) {
		col.Auto = true
	}

	rest := toks[i:]
	for j := 0; j < len(rest); j++ {
		switch strings.ToLower(rest[j]) {
		case "not":
			if j+1 < len(rest) && strings.EqualFold(rest[j+1], "null") {
				col.Nullable = false
				j++
			}
		case "null":
			col.Nullable = true
		case "primary":
			col.PK = true
			col.Nullable = false
			if j+1 < len(rest) && strings.EqualFold(rest[j+1], "key") {
				j++
			}
		case "unique":
			col.Unique = true
		case "default":
			if j+1 < len(rest) {
				col.Default = rest[j+1]
				j++
			}
		case "references":
			if j+1 < len(rest) {
				col.References = refTarget(strings.Join(rest[j+1:], " "))
				j++
			}
		case "generated", "autoincrement", "auto_increment", "identity":
			// An auto-assigned key. The clause itself is not copied — every
			// engine spells it differently, and copying it across dialects is
			// how a script stops being portable — but the fact is kept, so the
			// renderer can spell it for whichever engine it is writing for.
			col.Auto = true
		}
	}
	return col, true
}

// isModifier reports whether a token ends the type and begins the constraints.
func isModifier(tok string) bool {
	switch strings.ToLower(strings.TrimSuffix(tok, ",")) {
	case "not", "null", "primary", "unique", "default", "references",
		"check", "collate", "constraint", "generated", "autoincrement",
		"auto_increment", "identity":
		return true
	}
	return false
}

// refTarget turns the tail of a REFERENCES clause into "table.column".
func refTarget(s string) string {
	s = strings.TrimSpace(s)
	table := s
	if i := strings.IndexAny(s, "( "); i > 0 {
		table = s[:i]
	}
	table = unquote(strings.TrimSpace(table))

	col := "id"
	if open := strings.Index(s, "("); open >= 0 {
		if close := strings.Index(s[open:], ")"); close > 0 {
			inner := strings.TrimSpace(s[open+1 : open+close])
			if inner != "" {
				col = unquote(strings.TrimSpace(splitTop(inner, ',')[0]))
			}
		}
	}
	if table == "" {
		return ""
	}
	return table + "." + col
}

// foreignKeyClause reads a table-level FOREIGN KEY (a, b) REFERENCES t (x, y).
// Only the first column pair is used: a composite foreign key is outside what
// the rest of this package can express.
func foreignKeyClause(item string) ([]string, string) {
	open := strings.Index(item, "(")
	if open < 0 {
		return nil, ""
	}
	close := strings.Index(item[open:], ")")
	if close < 0 {
		return nil, ""
	}
	cols := identList(item[open+1 : open+close])

	rest := item[open+close:]
	at := indexFold(rest, "references")
	if at < 0 {
		return cols, ""
	}
	return cols, refTarget(rest[at+len("references"):])
}

// keyword is the first word of a clause, lowercased, which is what says whether
// it is a column or a table-level constraint.
func keyword(item string) string {
	for i := 0; i < len(item); i++ {
		if item[i] == ' ' || item[i] == '\t' || item[i] == '(' {
			return strings.ToLower(item[:i])
		}
	}
	return strings.ToLower(item)
}

// identsIn returns the identifiers inside the first parenthesised list.
func identsIn(item string) []string {
	open := strings.Index(item, "(")
	if open < 0 {
		return nil
	}
	close := strings.LastIndex(item, ")")
	if close <= open {
		return nil
	}
	return identList(item[open+1 : close])
}

func identList(s string) []string {
	var out []string
	for _, part := range splitTop(s, ',') {
		if p := unquote(strings.TrimSpace(part)); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// createTableParts pulls the table name and the column body out of a statement,
// and reports whether it was a CREATE TABLE at all.
func createTableParts(stmt string) (body, name string, ok bool) {
	trimmed := strings.TrimSpace(stmt)
	if indexFold(trimmed, "create") != 0 {
		return "", "", false
	}
	at := indexFold(trimmed, "table")
	if at < 0 {
		return "", "", false
	}
	rest := strings.TrimSpace(trimmed[at+len("table"):])
	for _, prefix := range []string{"if not exists"} {
		if indexFold(rest, prefix) == 0 {
			rest = strings.TrimSpace(rest[len(prefix):])
		}
	}

	open := strings.Index(rest, "(")
	if open < 0 {
		return "", "", false
	}
	close := strings.LastIndex(rest, ")")
	if close <= open {
		return "", "", false
	}

	name = unquote(strings.TrimSpace(rest[:open]))
	// A schema-qualified name is taken as its last part: the tool connects to
	// one database and introspects one search path, and carrying the qualifier
	// into a CREATE would put the table somewhere the diagram is not looking.
	if i := strings.LastIndex(name, "."); i >= 0 {
		name = name[i+1:]
	}
	if name == "" {
		return "", "", false
	}
	return rest[open+1 : close], name, true
}

// statements splits a script on the semicolons that are not inside quotes or
// parentheses.
func statements(sql string) []string {
	var out []string
	for _, s := range splitTop(sql, ';') {
		if strings.TrimSpace(s) != "" {
			out = append(out, s)
		}
	}
	return out
}

// splitTop splits on a separator that is at paren depth zero and outside any
// quoted string. Column definitions are full of commas inside `numeric(10,2)`,
// and a naive split gets every one of them wrong.
func splitTop(s string, sep byte) []string {
	var out []string
	var b strings.Builder

	depth := 0
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]

		if quote != 0 {
			b.WriteByte(c)
			if c == quote {
				// A doubled quote is an escaped one and does not close.
				if i+1 < len(s) && s[i+1] == quote {
					b.WriteByte(s[i+1])
					i++
					continue
				}
				quote = 0
			}
			continue
		}

		switch c {
		case '\'', '"', '`':
			quote = c
			b.WriteByte(c)
			continue
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		}

		if c == sep && depth == 0 {
			out = append(out, b.String())
			b.Reset()
			continue
		}
		b.WriteByte(c)
	}
	out = append(out, b.String())
	return out
}

// stripComments removes line and block comments, which otherwise land in the
// middle of a column definition and take the rest of the line with them.
func stripComments(s string) string {
	var b strings.Builder
	var quote byte

	for i := 0; i < len(s); i++ {
		c := s[i]

		if quote != 0 {
			b.WriteByte(c)
			if c == quote {
				quote = 0
			}
			continue
		}
		switch {
		case c == '\'' || c == '"' || c == '`':
			quote = c
			b.WriteByte(c)
		case c == '-' && i+1 < len(s) && s[i+1] == '-':
			for i < len(s) && s[i] != '\n' {
				i++
			}
			b.WriteByte('\n')
		case c == '/' && i+1 < len(s) && s[i+1] == '*':
			i += 2
			for i+1 < len(s) && !(s[i] == '*' && s[i+1] == '/') {
				i++
			}
			i++
			b.WriteByte(' ')
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// indexFold is strings.Index without case sensitivity, since SQL keywords come
// in whichever case the file was written in.
func indexFold(s, sub string) int {
	return strings.Index(strings.ToLower(s), sub)
}

// unquote strips the quoting a dialect puts around an identifier.
func unquote(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 {
		switch s[0] {
		case '"', '`', '[':
			return strings.TrimSpace(s[1 : len(s)-1])
		}
	}
	return s
}
