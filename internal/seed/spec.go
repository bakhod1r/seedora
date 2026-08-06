package seed

import (
	"fmt"
	"strings"

	"github.com/bakhod1r/seedora/internal/plan"
	"gopkg.in/yaml.v3"
)

// renderSpec turns one table's plan into a Synth YAML spec.
//
// Seedora keeps its own config format and translates here rather than storing
// Synth's format directly. That costs one function and buys two things: the file
// users commit is stable across Synth releases, and the columns Synth has no
// concept of — foreign keys drawn from a live parent, database defaults — can be
// left out of the spec entirely and handled by the seeder.
func renderSpec(name string, tp *plan.TablePlan, locale string, seed uint64) ([]byte, []string, error) {
	fields := yaml.Node{Kind: yaml.MappingNode}
	var synthCols []string

	for _, col := range columnOrder(tp) {
		cp := tp.Columns[col]
		if cp == nil || cp.Skip || !synthGenerated(cp.Generator) {
			continue
		}
		fd, err := fieldNode(col, cp)
		if err != nil {
			return nil, nil, err
		}
		fields.Content = append(fields.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: col}, fd)
		synthCols = append(synthCols, col)
	}

	if len(synthCols) == 0 {
		return nil, nil, nil
	}

	doc := yaml.Node{Kind: yaml.MappingNode}
	put := func(k string, v *yaml.Node) {
		doc.Content = append(doc.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: k}, v)
	}
	put("name", scalar(name))
	put("locale", scalar(localeOr(locale)))
	put("seed", scalar(fmt.Sprint(seed)))
	put("fields", &fields)

	out, err := yaml.Marshal(&doc)
	if err != nil {
		return nil, nil, fmt.Errorf("render spec for %s: %w", name, err)
	}
	return out, synthCols, nil
}

// fieldNode renders one column as a Synth field definition.
func fieldNode(col string, cp *plan.ColumnPlan) (*yaml.Node, error) {
	n := &yaml.Node{Kind: yaml.MappingNode}
	set := func(k string, v *yaml.Node) {
		n.Content = append(n.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: k}, v)
	}
	set("kind", scalar(cp.Generator))

	if cp.Min != nil {
		set("min", scalar(fmt.Sprint(cp.Min)))
	}
	if cp.Max != nil {
		set("max", scalar(fmt.Sprint(cp.Max)))
	}
	if len(cp.Values) > 0 {
		seq := &yaml.Node{Kind: yaml.SequenceNode}
		for _, v := range cp.Values {
			seq.Content = append(seq.Content, scalar(v))
		}
		set("choices", seq)
	}
	if len(cp.Weights) > 0 {
		if len(cp.Weights) != len(cp.Values) {
			return nil, fmt.Errorf("column %s: %d weights for %d values",
				col, len(cp.Weights), len(cp.Values))
		}
		seq := &yaml.Node{Kind: yaml.SequenceNode}
		for _, w := range cp.Weights {
			seq.Content = append(seq.Content, scalar(fmt.Sprint(w)))
		}
		set("weights", seq)
	}
	if cp.Unique {
		set("unique", scalar("true"))
	}
	return n, nil
}

// synthGenerated reports whether a generator is a Synth kind. The rest are
// Seedora's own and never reach the spec.
func synthGenerated(g string) bool {
	switch g {
	case plan.GenForeignKey, plan.GenSequence, plan.GenNull,
		plan.GenConst, plan.GenDefault, "":
		return false
	}
	return true
}

// columnOrder returns the plan's columns in catalog order, falling back to
// whatever order the map yields when a hand-written config has no Order — the
// output is still correct, only the spec's field order differs.
func columnOrder(tp *plan.TablePlan) []string {
	if len(tp.Order) > 0 {
		seen := make(map[string]bool, len(tp.Order))
		out := make([]string, 0, len(tp.Columns))
		for _, c := range tp.Order {
			if _, ok := tp.Columns[c]; ok && !seen[c] {
				out = append(out, c)
				seen[c] = true
			}
		}
		for c := range tp.Columns {
			if !seen[c] {
				out = append(out, c)
			}
		}
		return out
	}
	out := make([]string, 0, len(tp.Columns))
	for c := range tp.Columns {
		out = append(out, c)
	}
	return out
}

func scalar(v string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Value: v}
}

func localeOr(l string) string {
	if strings.TrimSpace(l) == "" {
		return "en_US"
	}
	return l
}
