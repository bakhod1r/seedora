package postgres

import (
	"sort"
	"strings"
)

// This file reads the two catalog answers Postgres only hands back as rendered
// SQL text: an index definition and a partial index's predicate. Both come out
// of the server already normalised — `pg_get_indexdef` and `pg_get_expr` print
// the parse tree, not the DDL anyone typed — so the shapes below are the
// server's spelling, not a user's, and the parsers reject anything else rather
// than guess.

// indexKeyColumn extracts the single key column from an index definition and
// reports whether the key is case-folded.
//
// The definition reads `CREATE UNIQUE INDEX x ON t USING btree (lower(nickname))
// WHERE ...`, so the key list is the parenthesised group after ` USING <method>`,
// and it holds exactly one entry because only single-key indexes reach here.
func indexKeyColumn(def string) (col string, fold, ok bool) {
	i := strings.Index(def, " USING ")
	if i < 0 {
		return "", false, false
	}
	open := strings.IndexByte(def[i:], '(')
	if open < 0 {
		return "", false, false
	}
	open += i

	depth, end := 0, -1
	for j := open; j < len(def) && end < 0; j++ {
		switch def[j] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				end = j
			}
		}
	}
	if end < 0 {
		return "", false, false
	}
	key := strings.TrimSpace(def[open+1 : end])

	// Sort options trail the key and are not part of it.
	for _, suffix := range []string{" NULLS FIRST", " NULLS LAST", " DESC", " ASC"} {
		key = strings.TrimSpace(strings.TrimSuffix(key, suffix))
	}
	if strings.HasPrefix(strings.ToLower(key), "lower(") && strings.HasSuffix(key, ")") {
		key = strings.TrimSpace(key[len("lower(") : len(key)-1])
		fold = true
	}
	key = unquoteIdent(key)
	if !isPlainIdent(key) {
		return "", false, false
	}
	return key, fold, true
}

// alwaysNullColumns reads partial-index predicates and returns the columns every
// one of them requires to be NULL.
//
// This is the soft-delete stamp, and mistaking it for an ordinary nullable
// column fails in the quietest way available. A sprinkle of values into
// `deleted_at` drops every row that receives one out of every index whose
// predicate is `WHERE deleted_at IS NULL`: the rows load, the constraints hold,
// and the dataset stops measuring the thing it was built to measure.
func alwaysNullColumns(preds []string) []string {
	required := map[string]int{}
	rejected := map[string]bool{}
	for _, p := range preds {
		for _, conj := range splitConjuncts(p) {
			col, wantNull, ok := nullTest(conj)
			if !ok {
				continue
			}
			if wantNull {
				required[col]++
			} else {
				rejected[col] = true
			}
		}
	}
	var out []string
	for col, n := range required {
		// A column one predicate wants NULL and another wants NOT NULL is a
		// genuinely two-sided column, not a soft-delete stamp.
		if n == len(preds) && !rejected[col] {
			out = append(out, col)
		}
	}
	sort.Strings(out)
	return out
}

// splitConjuncts breaks a predicate into its top-level AND terms. Anything
// joined by OR stays whole, fails nullTest, and is ignored — which is the
// correct reading: `a IS NULL OR b IS NULL` promises nothing about either.
func splitConjuncts(pred string) []string {
	const and = " AND "
	pred = stripOuterParens(strings.TrimSpace(pred))
	var out []string
	depth, start := 0, 0
	for i := 0; i < len(pred); i++ {
		switch pred[i] {
		case '(':
			depth++
		case ')':
			depth--
		}
		if depth == 0 && i+len(and) <= len(pred) && strings.EqualFold(pred[i:i+len(and)], and) {
			out = append(out, pred[start:i])
			start = i + len(and)
			i += len(and) - 1
		}
	}
	return append(out, pred[start:])
}

// stripOuterParens removes the parentheses wrapping a whole expression, which
// Postgres adds when it renders one. `(a) AND (b)` keeps both pairs: only a pair
// that spans the entire string is redundant.
func stripOuterParens(s string) string {
	for len(s) >= 2 && s[0] == '(' && s[len(s)-1] == ')' {
		depth := 0
		for i := 0; i < len(s); i++ {
			switch s[i] {
			case '(':
				depth++
			case ')':
				depth--
			}
			// The opening paren closed before the end, so it does not wrap the
			// expression and neither does anything outside it.
			if depth == 0 && i < len(s)-1 {
				return s
			}
		}
		s = strings.TrimSpace(s[1 : len(s)-1])
	}
	return s
}

// nullTest reads `col IS NULL` and `col IS NOT NULL`, and nothing else.
func nullTest(s string) (col string, wantNull, ok bool) {
	s = stripOuterParens(strings.TrimSpace(s))
	upper := strings.ToUpper(s)
	switch {
	case strings.HasSuffix(upper, " IS NOT NULL"):
		col, wantNull = s[:len(s)-len(" IS NOT NULL")], false
	case strings.HasSuffix(upper, " IS NULL"):
		col, wantNull = s[:len(s)-len(" IS NULL")], true
	default:
		return "", false, false
	}
	col = unquoteIdent(strings.TrimSpace(col))
	if !isPlainIdent(col) {
		return "", false, false
	}
	return col, wantNull, true
}

func unquoteIdent(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return strings.ReplaceAll(s[1:len(s)-1], `""`, `"`)
	}
	return s
}

// isPlainIdent guards every parse above: text pulled out of a rendered
// expression is only treated as a column name if it could be one.
func isPlainIdent(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '_':
		case c >= '0' && c <= '9' && i > 0:
		case c == '$' && i > 0:
		default:
			return false
		}
	}
	return true
}
