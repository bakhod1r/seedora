package plan

import (
	"regexp/syntax"
	"strings"

	"github.com/bakhod1r/synth/infer"
	sschema "github.com/bakhod1r/synth/schema"

	"github.com/bakhod1r/seedora/internal/model"
)

// DefaultRows is the row count proposed for a table nobody has set one for.
const DefaultRows = 1000

// Infer proposes a plan for a schema. It is conservative by design: a column
// whose name matches a known semantic gets that generator at High confidence, a
// column that only matched on type gets a plain value at Medium, and anything
// else is marked Low so the UI can ask.
//
// Inference never decides anything a human cannot override, and Merge makes
// sure it never overwrites a decision a human already made.
func Infer(s *model.Schema) *Plan {
	p := &Plan{Version: 1, Locale: "en_US", Tables: map[string]*TablePlan{}}
	for _, t := range s.Tables {
		tp := &TablePlan{Rows: DefaultRows, Columns: map[string]*ColumnPlan{}}
		for _, c := range t.Columns {
			tp.Order = append(tp.Order, c.Name)
			tp.Columns[c.Name] = InferColumn(s, t, c)
		}
		p.Tables[t.Name] = tp
	}
	return p
}

// InferColumn proposes the generator for a single column. The order of the
// checks is the priority order: a structural fact from the catalog (foreign key,
// generated column, enum type) always beats a guess from the column's name,
// because the catalog is authoritative and the name is a heuristic.
func InferColumn(s *model.Schema, t *model.Table, c *model.Column) *ColumnPlan {
	cp := inferColumn(s, t, c)
	applyChecks(t, c, cp)
	return cp
}

func inferColumn(s *model.Schema, t *model.Table, c *model.Column) *ColumnPlan {
	switch {
	case c.AlwaysNull:
		// Every partial index on the table is written `WHERE <col> IS NULL`, so
		// a row with a value here is a row no index covers. Filling it produces
		// a dataset that loads cleanly and measures nothing.
		return &ColumnPlan{Generator: GenNull, Confidence: High,
			Why: "every partial index requires this column to be NULL"}

	case c.Generated:
		return &ColumnPlan{Generator: GenDefault, Skip: true, Confidence: High,
			Why: "database computes this column"}

	case c.FK != nil:
		return &ColumnPlan{
			Generator:  GenForeignKey,
			References: c.FK.Table + "." + c.FK.Column,
			NullRate:   0,
			// A unique foreign key is a one-to-one: each child row takes a
			// different parent. Drawing at random would give one parent five
			// children and most parents none, and then fail on the duplicate.
			Unique:     c.Unique,
			Confidence: High,
			Why:        "foreign key to " + c.FK.Table + "." + c.FK.Column,
		}

	case c.EnumType != "":
		vals := append([]string(nil), s.Enums[c.EnumType]...)
		return &ColumnPlan{Generator: string(sschema.KindEnum), Values: vals,
			Confidence: High, Why: "enum type " + c.EnumType}

	case isAutoKey(t, c):
		// A serial or identity primary key fills itself, and letting the
		// database assign it is what keeps its sequence in step with the rows.
		return &ColumnPlan{Generator: GenDefault, Skip: true, Confidence: High,
			Why: "auto-assigned primary key"}
	}

	class := Classify(c.Type)

	// Name-based inference, delegated to Synth so Seedora and Synth agree on
	// what "email" or "first_name" means.
	if kind, ok := infer.Kind(c.Name, goTypeFor(class)); ok && !isFallback(kind, class) && fitsClass(kind, class) {
		conf, why := High, "column name matched "+string(kind)
		if ambiguous[normalizeName(c.Name)] {
			// The match is real but the name carries no domain: `name` on a
			// products table is a product, on a people table a person, and the
			// column alone does not say which. The table does, often enough to
			// be worth asking: a person's name on `categories` is wrong in a
			// way anyone reading the preview notices immediately.
			if alt, ok := disambiguate(t, kind); ok {
				kind = alt
				conf = Low
				why = "ambiguous name on " + t.Name + " — " + string(kind) + " is a guess"
			} else {
				// Applying the guess and flagging it is the README's contract —
				// a confident match is applied, an ambiguous one is raised for
				// a human to confirm.
				conf = Low
				why = "column name is ambiguous — " + string(kind) + " is a guess"
			}
		}
		cp := &ColumnPlan{Generator: string(kind), Confidence: conf, Why: why}
		applyBounds(cp, c, class)
		finish(cp, c)
		return cp
	}

	cp := typeOnly(class, c)
	applyBounds(cp, c, class)
	finish(cp, c)
	return cp
}

// ambiguous are column names that match a generator but say nothing about what
// the column holds. `name` maps to a person's name because that is the common
// case across schemas, not because this schema says so — on a products or
// categories table it is wrong, and the catalog offers no way to tell.
//
// These are not errors and not left blank: the guess is applied so the run
// works, and marked Low so the UI puts it in front of a human.
var ambiguous = map[string]bool{
	"name":        true,
	"title":       true,
	"label":       true,
	"type":        true,
	"kind":        true,
	"code":        true,
	"value":       true,
	"key":         true,
	"description": true,
	"comment":     true,
	"note":        true,
	"notes":       true,
	"data":        true,
	"content":     true,
	"text":        true,
	"reference":   true,
	"source":      true,
	"target":      true,
	"tag":         true,
	"tags":        true,
}

// peopleTables are the tables where an unqualified `name` really is a person's.
// The list is short on purpose: it covers the tables that hold people in most
// schemas, and everything outside it falls back to a plain word, which is wrong
// in a way that is obvious and easy to fix rather than plausible and easy to
// miss.
var peopleTables = map[string]bool{
	"user": true, "users": true,
	"person": true, "people": true, "persons": true,
	"customer": true, "customers": true,
	"client": true, "clients": true,
	"member": true, "members": true,
	"employee": true, "employees": true,
	"staff":  true,
	"author": true, "authors": true,
	"contact": true, "contacts": true,
	"account": true, "accounts": true,
	"profile": true, "profiles": true,
	"student": true, "students": true,
	"teacher": true, "teachers": true,
	"doctor": true, "doctors": true,
	"patient": true, "patients": true,
	"guest": true, "guests": true,
	"subscriber": true, "subscribers": true,
}

// disambiguate replaces a person-shaped guess on a table that does not hold
// people. It reports whether it changed anything, so the caller can say which
// of the two guesses it is explaining.
func disambiguate(t *model.Table, kind sschema.Kind) (sschema.Kind, bool) {
	switch kind {
	case sschema.KindName, sschema.KindFirstName, sschema.KindLastName:
	default:
		return kind, false
	}
	if peopleTables[normalizeName(t.Name)] {
		return kind, false
	}
	// A word, not a sentence: these columns are labels, and a length limit is
	// usually tight enough that a sentence would be clipped anyway.
	return sschema.KindWord, true
}

// normalizeName lowercases and strips the separators column names differ by, so
// `Name`, `name`, and `NAME` are one entry in the table above.
func normalizeName(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z':
			out = append(out, c+('a'-'A'))
		case c == '_' || c == '-' || c == ' ':
			// dropped
		default:
			out = append(out, c)
		}
	}
	return string(out)
}

// isFallback reports whether Synth's answer carries no more information than the
// column's type already did. Synth returns KindInt for any int field it cannot
// place, and treating that as a High-confidence name match would hide from the
// user that nothing actually matched.
func isFallback(k sschema.Kind, class Class) bool {
	switch k {
	case sschema.KindInt:
		return class == ClassInt
	case sschema.KindFloat:
		return class == ClassFloat
	case sschema.KindBool:
		return class == ClassBool
	case sschema.KindTime:
		return class == ClassTime
	case sschema.KindLorem, sschema.KindWord, sschema.KindSentence:
		return class == ClassString
	}
	return false
}

// fitsClass rejects a name match that would produce a value the column cannot
// hold. The catalog outranks the name here for the same reason it does
// everywhere else, and the case that forced this is common: `id BIGINT PRIMARY
// KEY` with no auto-increment matches the name "id", which means a UUID nearly
// everywhere else, and a UUID does not go into a bigint. On a strict engine
// that is an error mid-run; on a lax one it is silently a zero.
func fitsClass(k sschema.Kind, class Class) bool {
	switch class {
	case ClassInt:
		return k == sschema.KindInt
	case ClassFloat:
		return k == sschema.KindFloat || k == sschema.KindInt
	case ClassBool:
		return k == sschema.KindBool
	case ClassTime:
		return k == sschema.KindTime
	}
	return true
}

// typeOnly is the proposal when the name said nothing. The type still narrows
// it enough to produce loadable data, which is the bar for Medium.
func typeOnly(class Class, c *model.Column) *ColumnPlan {
	switch class {
	case ClassUUID:
		return &ColumnPlan{Generator: string(sschema.KindUUID), Confidence: Medium,
			Why: "uuid column"}
	case ClassBool:
		w := 0.5
		return &ColumnPlan{Generator: string(sschema.KindBool), TrueWeight: &w,
			Confidence: Medium, Why: "boolean column"}
	case ClassTime:
		return &ColumnPlan{Generator: string(sschema.KindTime), Confidence: Medium,
			Why: "timestamp column"}
	case ClassInt:
		return &ColumnPlan{Generator: string(sschema.KindInt), Confidence: Medium,
			Why: "integer column"}
	case ClassFloat:
		return &ColumnPlan{Generator: string(sschema.KindFloat), Confidence: Medium,
			Why: "numeric column"}
	case ClassJSON:
		return &ColumnPlan{Generator: GenConst, Const: "{}", Confidence: Low,
			Why: "json column — set a template or a value list"}
	case ClassBytes:
		return &ColumnPlan{Generator: GenDefault, Skip: c.Nullable || c.HasDefault,
			Confidence: Low, Why: "binary column — no generator inferred"}
	case ClassString:
		// A short string column is almost never free text; a long one almost
		// always is. The split at 40 is where names, codes, and slugs stop and
		// descriptions start.
		if c.MaxLen > 0 && c.MaxLen <= 40 {
			return &ColumnPlan{Generator: string(sschema.KindWord), Confidence: Low,
				Why: "short text column — no name match"}
		}
		return &ColumnPlan{Generator: string(sschema.KindSentence), Confidence: Low,
			Why: "text column — no name match"}
	}
	return &ColumnPlan{Generator: GenDefault, Skip: true, Confidence: Low,
		Why: "unrecognised type " + c.Type}
}

// applyBounds pushes the catalog's own limits into the generator, so a
// varchar(20) never produces 30 characters and a numeric(10,2) never overflows
// its precision. Doing this here means every driver gets it for free.
func applyBounds(cp *ColumnPlan, c *model.Column, class Class) {
	switch class {
	case ClassString:
		if c.MaxLen > 0 && cp.Max == nil {
			cp.Max = c.MaxLen
		}
	case ClassFloat:
		if c.Precision > 0 && cp.Max == nil {
			// numeric(p,s) holds at most p-s integer digits.
			whole := c.Precision - c.Scale
			if whole > 0 && whole <= 15 {
				cp.Max = pow10(whole) - 1
				cp.Min = 0
			}
		}
	}
}

// applyChecks folds the column's CHECK constraints into its plan.
//
// Two outcomes, and the split is the honest one. A single-column regex is a
// complete specification of the value, so it replaces whatever the name-based
// guess was: no generator named `sentence` will ever produce an argon2id hash,
// and the constraint says exactly what one looks like. Everything else — a
// constraint over two columns, an OR of several shapes, a comparison against
// another column — is a statement about a combination that per-column
// generation cannot promise, so it is recorded on the plan and reported instead
// of being silently ignored until the database rejects the batch.
func applyChecks(t *model.Table, c *model.Column, cp *ColumnPlan) {
	if cp.Generator == GenDefault || cp.Generator == GenNull {
		// Nothing is written, so nothing can violate a check.
		return
	}
	for _, ck := range t.ChecksFor(c.Name) {
		if ck.Enforceable() {
			if pat, ok := checkPattern(ck.Expr, c.Name); ok {
				cp.Generator = GenPattern
				cp.Pattern = pat
				cp.Confidence = High
				cp.Why = "CHECK " + ck.Name + " constrains the value to " + pat
				// The regex is now the whole definition; bounds inferred from
				// the type would only fight it.
				cp.Min, cp.Max, cp.Values, cp.Weights = nil, nil, nil, nil
				continue
			}
		}
		cp.Unenforced = append(cp.Unenforced, ck.Name+": "+ck.Expr)
		if cp.Confidence == High || cp.Confidence == Medium {
			cp.Confidence = Low
		}
		if cp.Why != "" {
			cp.Why += " — "
		}
		cp.Why += "CHECK " + ck.Name + " is not enforced by generation"
	}
}

// normalizeOperand reduces the left side of a rendered comparison to the column
// name. Postgres prints `(nickname)::text`, so the parentheses and the cast come
// off before the name can be recognised.
func normalizeOperand(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndex(s, "::"); i > 0 {
		s = strings.TrimSpace(s[:i])
	}
	for len(s) >= 2 && s[0] == '(' && s[len(s)-1] == ')' {
		s = strings.TrimSpace(s[1 : len(s)-1])
	}
	return strings.Trim(s, `"`)
}

// checkPattern reads a single-column regex check and returns the pattern.
//
// Postgres renders the constraint back as `(nickname ~ '^[a-z]{3,30}$'::text)`,
// which is the only shape accepted here: the column, a match operator, and a
// literal. `~*` is case-insensitive and is not translated, because the generated
// value has one case and matching it is what matters.
func checkPattern(expr, col string) (string, bool) {
	s := strings.TrimSpace(expr)
	for strings.HasPrefix(s, "(") && strings.HasSuffix(s, ")") {
		s = strings.TrimSpace(s[1 : len(s)-1])
	}
	for _, op := range []string{" ~ ", " ~* "} {
		lhs, rhs, found := strings.Cut(s, op)
		if !found {
			continue
		}
		if normalizeOperand(lhs) != col {
			continue
		}
		rhs = strings.TrimSpace(rhs)
		if i := strings.LastIndex(rhs, "::"); i > 0 {
			rhs = strings.TrimSpace(rhs[:i])
		}
		if len(rhs) < 2 || rhs[0] != '\'' || rhs[len(rhs)-1] != '\'' {
			continue
		}
		pat := strings.ReplaceAll(rhs[1:len(rhs)-1], "''", "'")
		if pat == "" {
			continue
		}
		if _, err := syntax.Parse(pat, syntax.Perl); err != nil {
			// A POSIX class or an engine extension Go's parser rejects. Better
			// reported as unenforced than generated from wrongly.
			return "", false
		}
		return pat, true
	}
	return "", false
}

// finish applies the facts that hold whatever the generator is.
func finish(cp *ColumnPlan, c *model.Column) {
	if c.Unique {
		cp.Unique = true
	}
	// A column with nothing to fill it and nothing behind it to fill itself is
	// written as NULL, and on a NOT NULL column that is a failed insert. This
	// catches the case where a low-confidence guess reached for the database
	// default that does not exist.
	if cp.Skip && !c.Nullable && !c.HasDefault && !c.Generated {
		cp.Skip = false
		cp.Confidence = Low
		cp.Why = "NOT NULL with no database default — a value must be generated"
		if cp.Generator == GenDefault {
			if alt := typeOnly(Classify(c.Type), c); alt.Generator != GenDefault {
				cp.Generator, cp.Const = alt.Generator, alt.Const
			} else {
				cp.Generator = string(sschema.KindWord)
			}
		}
	}
	// A nullable column that carries no meaning gets a light sprinkle of NULLs,
	// because a dataset where no nullable column is ever null does not exercise
	// the null paths the schema explicitly allows.
	if c.Nullable && cp.NullRate == 0 && cp.Generator != GenDefault {
		cp.NullRate = 0.05
	}
}

func isAutoKey(t *model.Table, c *model.Column) bool {
	if !c.HasDefault {
		return false
	}
	for _, k := range t.PrimaryKey {
		if k == c.Name {
			return Classify(c.Type) == ClassInt
		}
	}
	return false
}

// goTypeFor maps a class to the Go type name Synth's inference expects, since
// Synth infers over structs and Seedora infers over catalogs.
func goTypeFor(c Class) string {
	switch c {
	case ClassInt:
		return "int64"
	case ClassFloat:
		return "float64"
	case ClassBool:
		return "bool"
	case ClassTime:
		return "time.Time"
	default:
		return "string"
	}
}

func pow10(n int) int {
	v := 1
	for i := 0; i < n; i++ {
		v *= 10
	}
	return v
}

// Classify reduces an engine's type name to a Class. It matches on the bare type
// with any length, precision, array, or unsigned decoration stripped, which is
// enough to cover Postgres, MySQL, SQLite, and SQL Server spellings with one
// table rather than one per engine.
func Classify(sqlType string) Class {
	t := strings.ToLower(strings.TrimSpace(sqlType))
	if i := strings.IndexByte(t, '('); i >= 0 {
		t = t[:i]
	}
	t = strings.TrimSuffix(t, "[]")
	t = strings.TrimSpace(t)
	for _, suffix := range []string{" unsigned", " zerofill", " without time zone", " with time zone", " precision", " varying"} {
		t = strings.TrimSuffix(t, suffix)
	}
	t = strings.TrimSpace(t)

	switch t {
	case "uuid", "uniqueidentifier":
		return ClassUUID
	case "bool", "boolean", "bit":
		return ClassBool
	case "smallint", "integer", "int", "int2", "int4", "int8", "bigint",
		"tinyint", "mediumint", "serial", "bigserial", "smallserial",
		"serial2", "serial4", "serial8", "year":
		return ClassInt
	case "real", "double", "float", "float4", "float8", "numeric", "decimal",
		"money", "smallmoney", "dec", "fixed":
		return ClassFloat
	case "date", "time", "timetz", "timestamp", "timestamptz", "datetime",
		"datetime2", "smalldatetime", "datetimeoffset":
		return ClassTime
	case "json", "jsonb":
		return ClassJSON
	case "bytea", "blob", "binary", "varbinary", "image", "longblob",
		"mediumblob", "tinyblob":
		return ClassBytes
	case "char", "varchar", "character", "text", "citext", "nchar",
		"nvarchar", "ntext", "longtext", "mediumtext", "tinytext", "name",
		"bpchar", "clob", "xml":
		return ClassString
	}

	// Engines spell interval, network, and geometry types in many ways; they
	// are all written as text and none has a dedicated generator.
	switch {
	case strings.Contains(t, "char"), strings.Contains(t, "text"):
		return ClassString
	case strings.Contains(t, "int"):
		return ClassInt
	case strings.Contains(t, "time"), strings.Contains(t, "date"):
		return ClassTime
	}
	return ClassUnknown
}

// Class is re-exported so callers do not need the model package for it.
type Class = model.Class

const (
	ClassString  = model.ClassString
	ClassInt     = model.ClassInt
	ClassFloat   = model.ClassFloat
	ClassBool    = model.ClassBool
	ClassTime    = model.ClassTime
	ClassUUID    = model.ClassUUID
	ClassJSON    = model.ClassJSON
	ClassBytes   = model.ClassBytes
	ClassEnum    = model.ClassEnum
	ClassUnknown = model.ClassUnknown
)
