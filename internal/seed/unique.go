package seed

import (
	"fmt"
	"strconv"
	"strings"
	"time"

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
}

func newUniqueSet(t *model.Table, tp *plan.TablePlan, rows int) *uniqueSet {
	u := &uniqueSet{
		cols:   map[string]map[any]bool{},
		kind:   map[string]model.Class{},
		maxLen: map[string]int{},
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
		return idx, nil
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
func key(v any) any {
	switch t := v.(type) {
	case []byte:
		return string(t)
	case time.Time:
		return t.UnixNano()
	}
	return v
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
