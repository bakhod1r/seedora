package postgres

import (
	"reflect"
	"testing"
)

func TestIndexKeyColumn(t *testing.T) {
	cases := []struct {
		def  string
		col  string
		fold bool
		ok   bool
	}{
		{`CREATE UNIQUE INDEX uq_email ON public."user" USING btree (email_canonical)`,
			"email_canonical", false, true},
		{`CREATE UNIQUE INDEX uq_nick ON public."user" USING btree (lower(nickname)) WHERE (deleted_at IS NULL)`,
			"nickname", true, true},
		{`CREATE UNIQUE INDEX uq_q ON public.t USING btree ("weird name")`,
			"weird name", false, false},
		{`CREATE UNIQUE INDEX uq_d ON public.t USING btree (created_at DESC)`,
			"created_at", false, true},
		// An expression with no column to name: marking anything unique from
		// this would be a guess, and guessing wrong here means the run fails at
		// the insert.
		{`CREATE UNIQUE INDEX uq_c ON public.t USING btree (COALESCE(a, b))`,
			"", false, false},
	}
	for _, c := range cases {
		col, fold, ok := indexKeyColumn(c.def)
		if ok != c.ok || (ok && (col != c.col || fold != c.fold)) {
			t.Errorf("indexKeyColumn(%q) = %q, %v, %v; want %q, %v, %v",
				c.def, col, fold, ok, c.col, c.fold, c.ok)
		}
	}
}

func TestAlwaysNullColumns(t *testing.T) {
	// Every predicate on the table agrees deleted_at is NULL, so a row with a
	// value there falls out of every index the schema has.
	preds := []string{
		"(deleted_at IS NULL)",
		"((deleted_at IS NULL) AND (nickname IS NOT NULL))",
		"((deleted_at IS NULL) AND (email_canonical IS NOT NULL))",
	}
	if got := alwaysNullColumns(preds); !reflect.DeepEqual(got, []string{"deleted_at"}) {
		t.Errorf("alwaysNullColumns = %v, want [deleted_at]", got)
	}
}

func TestAlwaysNullNeedsEveryPredicateToAgree(t *testing.T) {
	// One index covers the deleted rows. deleted_at is a real two-sided column
	// here, not a stamp, and filling it is correct.
	preds := []string{
		"(deleted_at IS NULL)",
		"(deleted_at IS NOT NULL)",
	}
	if got := alwaysNullColumns(preds); len(got) != 0 {
		t.Errorf("alwaysNullColumns = %v, want none", got)
	}
}

func TestAlwaysNullIgnoresDisjunctions(t *testing.T) {
	// `a IS NULL OR b IS NULL` promises nothing about either column.
	preds := []string{"((a IS NULL) OR (b IS NULL))"}
	if got := alwaysNullColumns(preds); len(got) != 0 {
		t.Errorf("alwaysNullColumns = %v, want none", got)
	}
}
