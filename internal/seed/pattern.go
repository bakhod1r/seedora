package seed

import (
	"fmt"
	"math/rand/v2"
	"regexp/syntax"
	"strings"
	"unicode"
)

// patternGen produces strings that match a regular expression.
//
// It exists because a schema's real constraints are often written as regexes and
// nowhere else: `CHECK (nickname ~ '^[a-zA-Z0-9_]{3,30}$')` is the whole
// definition of what a nickname is, and no name-based generator can be talked
// into satisfying it. Generating from the expression itself is the only approach
// that holds for a constraint the tool has never seen before, which is every
// constraint in someone else's schema.
//
// The expression is parsed once into Go's own regex syntax tree and then walked
// per row, so the cost per value is the walk and nothing else.
type patternGen struct {
	re *syntax.Regexp
	// maxRepeat bounds an unbounded quantifier. `a+` matches one `a` or a
	// million, and a column that must hold the result has a limit; a small
	// number keeps values realistic and keeps a `.*` from eating the run.
	maxRepeat int
}

func newPatternGen(expr string) (*patternGen, error) {
	if strings.TrimSpace(expr) == "" {
		return nil, fmt.Errorf("empty pattern")
	}
	re, err := syntax.Parse(expr, syntax.Perl)
	if err != nil {
		return nil, fmt.Errorf("pattern %q: %w", expr, err)
	}
	return &patternGen{re: re.Simplify(), maxRepeat: 8}, nil
}

// generate returns one string matching the expression.
func (p *patternGen) generate(r *rand.Rand) string {
	var b strings.Builder
	p.write(&b, p.re, r, 0)
	return b.String()
}

// write appends one match of re. depth guards a pathological expression — a
// nested star over an alternation can otherwise recurse as long as the RNG
// keeps saying yes.
func (p *patternGen) write(b *strings.Builder, re *syntax.Regexp, r *rand.Rand, depth int) {
	if depth > 20 {
		return
	}
	switch re.Op {
	case syntax.OpLiteral:
		for _, c := range re.Rune {
			b.WriteRune(c)
		}

	case syntax.OpCharClass:
		b.WriteRune(pickFromClass(re.Rune, r))

	case syntax.OpAnyChar, syntax.OpAnyCharNotNL:
		// A printable ASCII letter, not any rune in Unicode: `.` in a schema's
		// check almost always means "some character", and a random astral
		// codepoint would satisfy the regex while failing the column's encoding
		// or looking like corruption in the data.
		b.WriteRune(rune('a' + r.IntN(26)))

	case syntax.OpCapture, syntax.OpPlus, syntax.OpStar, syntax.OpQuest, syntax.OpRepeat:
		p.repeat(b, re, r, depth)

	case syntax.OpConcat:
		for _, sub := range re.Sub {
			p.write(b, sub, r, depth+1)
		}

	case syntax.OpAlternate:
		if len(re.Sub) > 0 {
			p.write(b, re.Sub[r.IntN(len(re.Sub))], r, depth+1)
		}

	// Anchors and boundaries match a position, not a character. Emitting
	// nothing for them is exactly right: `^` and `$` are what make the pattern
	// describe the whole value, which is already how it is being used.
	case syntax.OpBeginLine, syntax.OpEndLine, syntax.OpBeginText, syntax.OpEndText,
		syntax.OpWordBoundary, syntax.OpNoWordBoundary, syntax.OpEmptyMatch:

	case syntax.OpNoMatch:
	}
}

// repeat handles the quantifiers, all of which are "write the subexpression n
// times" over a different n.
func (p *patternGen) repeat(b *strings.Builder, re *syntax.Regexp, r *rand.Rand, depth int) {
	if len(re.Sub) == 0 {
		return
	}
	sub := re.Sub[0]

	min, max := 1, 1
	switch re.Op {
	case syntax.OpCapture:
		// Not a quantifier: a group matches its contents once.
	case syntax.OpStar:
		min, max = 0, p.maxRepeat
	case syntax.OpPlus:
		min, max = 1, p.maxRepeat
	case syntax.OpQuest:
		min, max = 0, 1
	case syntax.OpRepeat:
		min, max = re.Min, re.Max
		if max < 0 || max > min+p.maxRepeat {
			// `{3,}` and `{3,4096}` alike: honour the floor, which is the part
			// the constraint cares about, and stay near it.
			max = min + p.maxRepeat
		}
	}
	n := min
	if max > min {
		n = min + r.IntN(max-min+1)
	}
	for i := 0; i < n; i++ {
		p.write(b, sub, r, depth+1)
	}
}

// pickFromClass draws a rune from a character class, which the parser hands over
// as pairs of inclusive range bounds.
//
// The draw is uniform over the class's runes rather than over its ranges, so
// `[a-z0-9]` is not three-quarters digits. Ranges are clipped to printable
// ASCII where they overlap it: a negated class like `[^x]` spans nearly the
// whole of Unicode, and drawing uniformly from that produces unreadable values
// that technically match.
func pickFromClass(ranges []rune, r *rand.Rand) rune {
	type span struct{ lo, hi rune }
	var (
		spans []span
		total int
	)
	add := func(lo, hi rune) {
		if hi < lo {
			return
		}
		spans = append(spans, span{lo, hi})
		total += int(hi-lo) + 1
	}
	for i := 0; i+1 < len(ranges); i += 2 {
		lo, hi := ranges[i], ranges[i+1]
		if lo <= unicode.MaxASCII && hi > unicode.MaxASCII {
			hi = unicode.MaxASCII
		}
		if lo > unicode.MaxASCII {
			// Wholly non-ASCII: keep it, but only if nothing else is on offer.
			continue
		}
		if lo < ' ' {
			lo = ' '
		}
		add(lo, hi)
	}
	if total == 0 {
		for i := 0; i+1 < len(ranges); i += 2 {
			add(ranges[i], ranges[i+1])
		}
	}
	if total == 0 {
		return 'a'
	}
	n := r.IntN(total)
	for _, s := range spans {
		size := int(s.hi-s.lo) + 1
		if n < size {
			return s.lo + rune(n)
		}
		n -= size
	}
	return 'a'
}
