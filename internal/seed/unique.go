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
	// counterOnly marks the columns whose existing values were never read,
	// because a number past the maximum is enough. Every row takes the counter,
	// since a generated value cannot be checked against a set that is not held.
	counterOnly map[string]bool
	// fold marks the columns whose uniqueness is over the case-folded value,
	// from a `lower(col)` unique index. Two values that differ only in case are
	// one key to the database and have to be one key here.
	fold map[string]bool
}

func newUniqueSet(t *model.Table, tp *plan.TablePlan, rows int) *uniqueSet {
	u := &uniqueSet{
		cols:   map[string]map[any]bool{},
		kind:   map[string]model.Class{},
		maxLen: map[string]int{},
		next:   map[string]int64{},

		counterOnly: map[string]bool{},
		fold:        map[string]bool{},
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
		u.fold[c.Name] = c.UniqueFold
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
// Not every column has to be read, and the ones that do not are the ones that
// made this refuse to run at all. An integer column needs a single number: every
// value above the existing maximum is free, so the counter starts there and no
// set is held. A UUID is not read either — a repaired value is a fresh v4, and
// the chance of one colliding with an existing row is smaller than the chance of
// the hardware getting it wrong. What is left is text, where the existing values
// genuinely are the constraint, and only that pays the cap.
//
// A text column with more existing values than the cap is refused rather than
// half-read. A partial set would silently pass a duplicate through to the
// database, and the error it raised there would name the constraint rather than
// the reason. The cap is a limit on memory, not a judgement, so it is settable.
func (u *uniqueSet) preload(ctx context.Context, tx db.Tx, t *model.Table, cap int) error {
	u.appending = true
	if cap <= 0 {
		cap = defaultAppendUniqueCap
	}
	for col, seen := range u.cols {
		switch u.kind[col] {
		case model.ClassInt, model.ClassFloat:
			mv, capable := tx.(db.MaxValuer)
			if !capable {
				// The driver cannot answer without reading, so read — the old
				// behaviour, cap and all.
				if err := u.preloadValues(ctx, tx, t, col, seen, cap); err != nil {
					return err
				}
				continue
			}
			maximum, ok, err := mv.MaxValue(ctx, t, col)
			if err != nil {
				return err
			}
			if !ok {
				maximum = 0
			}
			u.next[col] = maximum + 1
			// Nothing is read into the set, so a generated value could still
			// land on an existing one. It is corrected on the way past: while
			// appending, an integer column takes the counter rather than the
			// generated number, which is past everything already there.
			u.counterOnly[col] = true

		case model.ClassUUID:
			u.next[col] = 1

		default:
			if err := u.preloadValues(ctx, tx, t, col, seen, cap); err != nil {
				return err
			}
		}
	}
	return nil
}

// preloadValues reads a column's existing values into the set.
func (u *uniqueSet) preloadValues(
	ctx context.Context, tx db.Tx, t *model.Table,
	col string, seen map[any]bool, cap int,
) error {
	existing, err := tx.ReadKeys(ctx, t, col, cap)
	if err != nil {
		return err
	}
	if len(existing) >= cap {
		return fmt.Errorf("column %s already holds at least %d values: --append has to "+
			"hold every existing value to generate around it, and refuses rather than "+
			"read a partial set and risk a constraint violation partway through. Raise "+
			"the limit if the memory is available", col, cap)
	}
	var maximum int64
	for _, v := range existing {
		seen[u.key(col, v)] = true
		if n, ok := toInt64(v); ok && n > maximum {
			maximum = n
		}
	}
	u.next[col] = maximum + 1
	return nil
}

// defaultAppendUniqueCap bounds what preload will hold in memory for a text
// column. Ten million values is already several gigabytes of Go map.
const defaultAppendUniqueCap = 10_000_000

// enforce makes every unique column in the row unique, repairing collisions.
func (u *uniqueSet) enforce(row map[string]any, idx int) error {
	for col, seen := range u.cols {
		v, ok := row[col]
		if !ok || v == nil {
			// NULL does not participate in uniqueness in any engine Seedora
			// targets, so several are fine.
			continue
		}
		if u.counterOnly[col] {
			// The existing values were never read, so the generated one cannot
			// be cleared against them. The counter can: it starts past the
			// column's maximum and only goes up.
			n := u.next[col]
			u.next[col]++
			row[col] = n
			continue
		}
		k := u.key(col, v)
		if !seen[k] {
			seen[k] = true
			continue
		}
		repaired, err := u.repair(col, v, idx)
		if err != nil {
			return err
		}
		rk := u.key(col, repaired)
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

// key is the column's own key: the value as the database will compare it. On a
// column whose unique index is over `lower(col)`, that is the folded value —
// storing the raw one would let "Bob" and "bob" both through as distinct and
// leave the database to reject the second.
func (u *uniqueSet) key(col string, v any) any {
	k := key(v)
	if !u.fold[col] {
		return k
	}
	if s, ok := k.(string); ok {
		return strings.ToLower(s)
	}
	return k
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
