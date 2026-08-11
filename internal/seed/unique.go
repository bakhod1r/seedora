package seed

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/bakhod1r/seedora/internal/db"
	"github.com/bakhod1r/seedora/internal/model"
	"github.com/bakhod1r/seedora/internal/plan"
)

// uniqueSet enforces uniqueness across an entire table.
//
// Synth guarantees uniqueness within one generation call, but a table is
// generated in many calls on many goroutines, and that guarantee does not span
// them. Retrying at random would work at first and then degrade badly as the
// column fills — exactly the failure the README calls out — because every retry
// is a fresh draw from a space that is mostly taken.
//
// Instead a collision is repaired from the row's index, which no other row
// shares. That is one map lookup per value in the common case, no retries in the
// worst case, and the cost does not change as the column fills.
type uniqueSet struct {
	cols   map[string]map[any]bool
	kind   map[string]model.Class
	maxLen map[string]int

	// appending records that the table already holds rows this run must not
	// collide with. It changes how an integer collision is repaired: the row
	// index is unique within the run and says nothing about what is already
	// in the column, so a counter past the existing maximum is used instead.
	appending bool
	next      map[string]int64
}

func newUniqueSet(t *model.Table, tp *plan.TablePlan, rows int) *uniqueSet {
	u := &uniqueSet{
		cols:   map[string]map[any]bool{},
		kind:   map[string]model.Class{},
		maxLen: map[string]int{},
		next:   map[string]int64{},
	}
	for _, c := range t.Columns {
		cp := tp.Get(c.Name)
		if cp == nil || cp.Skip || !cp.Unique {
			continue
		}
		if cp.Generator == plan.GenDefault || cp.Generator == plan.GenNull {
			continue
		}
		// Sized up front: the map is the run's largest allocation on a wide
		// unique column, and growing it row by row would rehash all of it
		// repeatedly at exactly the moment throughput matters.
		u.cols[c.Name] = make(map[any]bool, rows)
		u.kind[c.Name] = plan.Classify(c.Type)
		u.maxLen[c.Name] = c.MaxLen
	}
	return u
}

// active reports whether there is anything to enforce, so the caller can skip
// the wrapper entirely on the common table with no unique generated column.
func (u *uniqueSet) active() bool { return len(u.cols) > 0 }

// preload reads the values a unique column already holds, so an appending run
// is unique against the table rather than only against itself.
//
// This is the whole cost of --append and it is paid honestly: without it, the
// first generated email that matches one already in the table is a constraint
// violation partway through a run, which is worse than a slow start.
//
// A column with more existing values than the cap is refused rather than
// half-read. A partial set would silently pass a duplicate through to the
// database, and the error it raised there would name the constraint rather than
// the reason.
func (u *uniqueSet) preload(ctx context.Context, tx db.Tx, t *model.Table) error {
	u.appending = true
	for col, seen := range u.cols {
		existing, err := tx.ReadKeys(ctx, t, col, appendUniqueCap)
		if err != nil {
			return err
		}
		if len(existing) >= appendUniqueCap {
			return fmt.Errorf("column %s already holds at least %d values: --append "+
				"cannot guarantee uniqueness against a column that large, so it "+
				"refuses rather than risk a constraint violation partway through",
				col, appendUniqueCap)
		}
		var maximum int64
		for _, v := range existing {
			seen[key(v)] = true
			if n, ok := toInt64(v); ok && n > maximum {
				maximum = n
			}
		}
		u.next[col] = maximum + 1
	}
	return nil
}

// appendUniqueCap bounds what preload will hold in memory. Ten million values
// is a table nobody seeds into interactively, and reading it is already slower
// than generating the rows.
const appendUniqueCap = 10_000_000

// enforce makes every unique column in the row unique, repairing collisions.
func (u *uniqueSet) enforce(row map[string]any, idx int) error {
	for col, seen := range u.cols {
		v, ok := row[col]
		if !ok || v == nil {
			// NULL does not participate in uniqueness in any engine Seedora
			// targets, so several are fine.
			continue
		}
		k := key(v)
		if !seen[k] {
			seen[k] = true
			continue
		}
		repaired, err := u.repair(col, v, idx)
		if err != nil {
			return err
		}
		rk := key(repaired)
		if seen[rk] {
			return fmt.Errorf("column %s: cannot make value unique at row %d", col, idx)
		}
		seen[rk] = true
		row[col] = repaired
	}
	return nil
}

// repair makes a colliding value unique using the row's index, which is unique
// by construction.
func (u *uniqueSet) repair(col string, v any, idx int) (any, error) {
	switch u.kind[col] {
	case model.ClassInt:
		// An integer column cannot carry a suffix, so the index becomes the
		// value. It is unique across the table by definition.
		if !u.appending {
			return idx, nil
		}
		// Except when the table already has rows: the index says nothing about
		// what those rows hold, and row 3 of an append is very likely to be an
		// id that exists. Counting on from the largest existing value is, and
		// the loop covers the case where a generated row already took one.
		seen := u.cols[col]
		for {
			n := u.next[col]
			u.next[col]++
			if !seen[key(n)] {
				return n, nil
			}
		}
	case model.ClassFloat:
		f, ok := toFloat(v)
		if !ok {
			return nil, fmt.Errorf("column %s: cannot repair a %T", col, v)
		}
		return f + float64(idx)*1e-6, nil
	case model.ClassString, model.ClassUUID:
		s, ok := v.(string)
		if !ok {
			s = fmt.Sprint(v)
		}
		return suffix(s, idx, u.maxLen[col]), nil
	}
	return nil, fmt.Errorf("column %s: duplicate value and no way to repair a %s column",
		col, u.kind[col])
}

// suffix appends a discriminator, inserting it before the domain when the value
// looks like an email so the result is still a valid address.
func suffix(s string, idx, maxLen int) string {
	tag := strconv.Itoa(idx)
	if at := strings.LastIndexByte(s, '@'); at > 0 {
		local, domain := s[:at], s[at:]
		out := local + "+" + tag + domain
		if maxLen > 0 && len(out) > maxLen {
			// Trim the local part, never the domain: a truncated domain is not
			// an address at all.
			keep := maxLen - len(domain) - len(tag) - 1
			if keep < 1 {
				return trimTo(tag+domain, maxLen)
			}
			return local[:min(keep, len(local))] + "+" + tag + domain
		}
		return out
	}
	out := s + "-" + tag
	if maxLen > 0 && len(out) > maxLen {
		keep := maxLen - len(tag) - 1
		if keep < 1 {
			return trimTo(tag, maxLen)
		}
		return s[:min(keep, len(s))] + "-" + tag
	}
	return out
}

func trimTo(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	return s[:n]
}

// key normalises a value into something a map can hold, since Synth may hand
// back a slice or a time and neither is comparable as-is.
//
// Integers are widened to int64 because the same number arrives in two shapes:
// the generator produces int, and a driver reading the column back produces
// int64. As map keys those are different values, so without this an appending
// run would not recognise the id it is about to duplicate.
func key(v any) any {
	switch t := v.(type) {
	case []byte:
		return string(t)
	case time.Time:
		return t.UnixNano()
	case int:
		return int64(t)
	case int32:
		return int64(t)
	case int16:
		return int64(t)
	case int8:
		return int64(t)
	case uint:
		return int64(t)
	case uint32:
		return int64(t)
	case uint64:
		return int64(t)
	}
	return v
}

// toInt64 reports the value as an integer, for finding the largest id a column
// already holds.
func toInt64(v any) (int64, bool) {
	n, ok := key(v).(int64)
	return n, ok
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case int32:
		return float64(n), true
	}
	return 0, false
}
