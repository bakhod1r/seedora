// Package spec reads and writes seedora.yaml — the mapping a team commits
// alongside its code.
//
// The file never contains credentials. That is not a convention here but a
// property of the format: there is nowhere in it to put one.
package spec

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"github.com/bakhod1r/seedora/internal/model"
	"github.com/bakhod1r/seedora/internal/plan"
)

// Version is the format version written by this build.
const Version = 1

// Load reads a mapping file.
func Load(path string) (*plan.Plan, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(b)
}

// Parse decodes a mapping from YAML.
func Parse(b []byte) (*plan.Plan, error) {
	var p plan.Plan
	dec := yaml.NewDecoder(bytes.NewReader(b))
	// A key the format does not define is far more likely to be a typo than a
	// forward-compatible extension, and silently ignoring it produces a run
	// that does not do what the file says.
	dec.KnownFields(true)
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if p.Version == 0 {
		p.Version = Version
	}
	if p.Version > Version {
		return nil, fmt.Errorf("config version %d is newer than this build understands (%d)",
			p.Version, Version)
	}
	if p.Tables == nil {
		p.Tables = map[string]*plan.TablePlan{}
	}
	for _, tp := range p.Tables {
		if tp.Columns == nil {
			tp.Columns = map[string]*plan.ColumnPlan{}
		}
	}
	return &p, nil
}

// Save writes a mapping, creating parent directories as needed. The write goes
// to a temporary file first and is renamed into place, so an interrupted save
// cannot leave a half-written config where a working one used to be.
func Save(path string, p *plan.Plan, s *model.Schema) error {
	p.Version = Version
	b, err := Render(p, s)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".seedora-*.yaml")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Render encodes a mapping. Passing the schema is what lets tables and columns
// be written in catalog order rather than in map order, so a re-scan produces a
// diff a human can read instead of a reshuffle.
func Render(p *plan.Plan, s *model.Schema) ([]byte, error) {
	doc := &yaml.Node{Kind: yaml.MappingNode}
	put := func(n *yaml.Node, k string, v *yaml.Node) {
		n.Content = append(n.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: k}, v)
	}

	put(doc, "version", scalar(fmt.Sprint(p.Version)))
	if p.Seed != 0 {
		put(doc, "seed", scalar(fmt.Sprint(p.Seed)))
	}
	if p.Locale != "" {
		put(doc, "locale", scalar(p.Locale))
	}

	tables := &yaml.Node{Kind: yaml.MappingNode}
	for _, name := range tableOrder(p, s) {
		tp := p.Tables[name]
		if tp == nil {
			continue
		}
		tn := &yaml.Node{Kind: yaml.MappingNode}
		put(tn, "rows", scalar(fmt.Sprint(tp.Rows)))
		if tp.Truncate {
			put(tn, "truncate", scalar("true"))
		}
		if tp.Skip {
			put(tn, "skip", scalar("true"))
		}

		cols := &yaml.Node{Kind: yaml.MappingNode}
		for _, col := range columnOrder(name, tp, s) {
			cp := tp.Columns[col]
			if cp == nil {
				continue
			}
			cn, err := encodeColumn(cp)
			if err != nil {
				return nil, fmt.Errorf("%s.%s: %w", name, col, err)
			}
			put(cols, col, cn)
		}
		put(tn, "columns", cols)
		put(tables, name, tn)
	}
	put(doc, "tables", tables)

	buf := &bytes.Buffer{}
	enc := yaml.NewEncoder(buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// encodeColumn writes a column plan as a flow mapping, which is what keeps the
// file readable: one line per column, as in the README.
func encodeColumn(cp *plan.ColumnPlan) (*yaml.Node, error) {
	n := &yaml.Node{Kind: yaml.MappingNode, Style: yaml.FlowStyle}
	var buf []byte
	b, err := yaml.Marshal(cp)
	if err != nil {
		return nil, err
	}
	buf = b
	var tmp yaml.Node
	if err := yaml.Unmarshal(buf, &tmp); err != nil {
		return nil, err
	}
	if len(tmp.Content) == 0 {
		return n, nil
	}
	src := tmp.Content[0]
	n.Content = src.Content
	// A nested sequence inside a flow mapping must itself be flow, or the
	// encoder produces YAML it cannot read back.
	for _, c := range n.Content {
		if c.Kind == yaml.SequenceNode || c.Kind == yaml.MappingNode {
			c.Style = yaml.FlowStyle
		}
	}
	return n, nil
}

func tableOrder(p *plan.Plan, s *model.Schema) []string {
	if s == nil {
		return p.TableNames()
	}
	seen := map[string]bool{}
	var out []string
	for _, t := range s.Tables {
		if _, ok := p.Tables[t.Name]; ok {
			out = append(out, t.Name)
			seen[t.Name] = true
		}
	}
	for _, n := range p.TableNames() {
		if !seen[n] {
			out = append(out, n)
		}
	}
	return out
}

// columnOrder is the order columns are written to the file. The plan's own
// order wins when it has one: it is what the user arranged in the diagram, and
// a file that reorders itself on every save is a file that produces a diff
// nobody asked for. The catalog is the fallback for a plan that never carried
// an order.
func columnOrder(table string, tp *plan.TablePlan, s *model.Schema) []string {
	catalog := tp.Order
	if len(catalog) == 0 && s != nil {
		if t := s.Table(table); t != nil {
			for _, c := range t.Columns {
				catalog = append(catalog, c.Name)
			}
		}
	}
	seen := map[string]bool{}
	var out []string
	for _, c := range catalog {
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

func scalar(v string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Value: v}
}

// ErrNotFound is returned when no mapping file exists.
var ErrNotFound = errors.New("config not found")

// LoadOrInfer reads a mapping if one exists and folds a fresh inference into it,
// or infers one from scratch. This is the behaviour the README describes for
// re-scanning: overrides survive, new columns appear.
func LoadOrInfer(path string, s *model.Schema) (*plan.Plan, bool, error) {
	fresh := plan.Infer(s)
	existing, err := Load(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fresh, false, nil
		}
		return nil, false, err
	}
	existing.Merge(fresh)
	if existing.Locale == "" {
		existing.Locale = fresh.Locale
	}
	return existing, true, nil
}
