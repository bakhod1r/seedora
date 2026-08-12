// Package neo4j implements the Seedora driver for Neo4j.
//
// A graph has no tables, so the mapping has to be decided rather than read: a
// node label is a table, and its property keys are that table's columns. That
// works because seeding a graph is overwhelmingly a matter of creating nodes,
// and a node with a label and a set of properties is exactly a row.
//
// The property keys are not invented. `db.schema.nodeTypeProperties` walks what
// is actually stored and reports, per label, every property key seen and the
// types it was seen with — so this is inference, but the database does the
// inferring and keeps the answer up to date. It is not a declared schema: Neo4j
// only declares things when a constraint or a property-existence rule says so,
// and a label with no nodes reports no properties at all.
//
// Relationship types are introspected too, from `db.schema.relTypeProperties`,
// and are listed with "relationship" as their schema so the UI can tell the two
// apart. They cannot be seeded: a relationship needs two endpoint nodes, and
// nothing in a row of generated values says which. Insert says so rather than
// creating a heap of disconnected nothing.
//
// Transactions are real and are the one thing on the non-relational list that
// needs no apology: Begin opens an explicit transaction, and Rollback undoes
// every write in it.
package neo4j

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"

	"github.com/bakhod1r/seedora/internal/db"
	"github.com/bakhod1r/seedora/internal/ddl"
	"github.com/bakhod1r/seedora/internal/model"
)

func init() {
	db.Register(open, "neo4j", "bolt")
}

// relationshipSchema marks a table that describes a relationship type rather
// than a node label. It goes in Table.Schema because that is the only namespace
// the model has, and the distinction has to survive into the UI.
const relationshipSchema = "relationship"

// Driver is a connected Neo4j database.
type Driver struct {
	driver   neo4j.DriverWithContext
	database string
}

// open connects to the server named by the DSN, `neo4j://user:pass@host:7687/db`.
//
// The database in the path is optional: a server has a default (`neo4j`) and
// Community Edition only has that one, so leaving it out is the common case.
func open(ctx context.Context, dsn string) (db.Driver, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse DSN: %w", err)
	}
	database := strings.TrimPrefix(u.Path, "/")

	auth := neo4j.NoAuth()
	if u.User != nil {
		pass, _ := u.User.Password()
		auth = neo4j.BasicAuth(u.User.Username(), pass, "")
	}
	// The credentials are stripped from the URI handed to the driver: it takes
	// them as an argument, and a URI carrying userinfo is rejected.
	target := *u
	target.User = nil
	target.Path = ""
	target.RawQuery = ""

	drv, err := neo4j.NewDriverWithContext(target.String(), auth)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	// The driver is lazy, so without this a wrong host or a bad password would
	// only surface at the first query.
	if err := drv.VerifyConnectivity(ctx); err != nil {
		_ = drv.Close(ctx)
		return nil, fmt.Errorf("connect: %w", err)
	}
	return &Driver{driver: drv, database: database}, nil
}

// Name implements db.Driver.
func (d *Driver) Name() string { return "Neo4j" }

// Dialect implements db.Driver.
func (d *Driver) Dialect() ddl.Dialect { return ddl.Document }

// Close implements db.Driver.
func (d *Driver) Close(ctx context.Context) error { return d.driver.Close(ctx) }

// History implements db.Driver, and finds nothing. The migration-tool catalogue
// is a list of SQL tables; nothing writes one into a graph, and the server keeps
// no record of past constraint or index changes a query could read back.
func (d *Driver) History(context.Context) ([]model.Migration, error) { return nil, nil }

// Introspect reads the two schema procedures.
//
// `db.schema.nodeTypeProperties` yields one row per (label, property) pair, with
// the types that property has been seen with and whether every node carrying the
// label carries it. `db.schema.relTypeProperties` does the same for relationship
// types. Both walk stored data rather than a catalog — Neo4j has no catalog of
// properties, because a node is free to carry any — but they are the database's
// own answer rather than this driver's guess at one.
func (d *Driver) Introspect(ctx context.Context) (*model.Schema, error) {
	sess := d.session(ctx, neo4j.AccessModeRead)
	defer sess.Close(ctx)

	s := &model.Schema{Enums: map[string]model.Values{}}
	nodes, err := d.typeProperties(ctx, sess, "CALL db.schema.nodeTypeProperties()", "nodeLabels", "")
	if err != nil {
		return nil, err
	}
	rels, err := d.typeProperties(ctx, sess, "CALL db.schema.relTypeProperties()", "relType", relationshipSchema)
	if err != nil {
		return nil, err
	}
	s.Tables = append(nodes, rels...)

	for _, t := range s.Tables {
		if t.Schema == relationshipSchema {
			continue
		}
		// A label count comes straight from the count store and does not touch
		// the nodes, so this is a constant-time read however large the graph.
		res, err := sess.Run(ctx, fmt.Sprintf("MATCH (n:%s) RETURN count(n) AS n", quote(t.Name)), nil)
		if err != nil {
			return nil, fmt.Errorf("count %s: %w", t.Name, err)
		}
		rec, err := res.Single(ctx)
		if err != nil {
			return nil, fmt.Errorf("count %s: %w", t.Name, err)
		}
		if n, ok := rec.Get("n"); ok {
			if v, ok := n.(int64); ok {
				t.ExistingRows = v
			}
		}
	}
	return s, nil
}

// typeProperties runs one of the two schema procedures and turns its rows into
// tables. The label column is a list for nodes and a string for relationships,
// which is the only difference between the two shapes.
func (d *Driver) typeProperties(
	ctx context.Context, sess neo4j.SessionWithContext, query, labelKey, schema string,
) ([]*model.Table, error) {
	res, err := sess.Run(ctx, query, nil)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", labelKey, err)
	}
	records, err := res.Collect(ctx)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", labelKey, err)
	}

	byName := map[string]*model.Table{}
	var order []string
	for _, rec := range records {
		name := label(rec, labelKey)
		if name == "" {
			continue
		}
		t, ok := byName[name]
		if !ok {
			t = &model.Table{Schema: schema, Name: name}
			byName[name] = t
			order = append(order, name)
		}
		prop, _ := rec.Get("propertyName")
		key, _ := prop.(string)
		if key == "" {
			// A label with no properties on any node still gets a row, with a
			// null property name. The label exists; it just has nothing in it.
			continue
		}
		typ := propertyType(rec)
		mandatory, _ := rec.Get("mandatory")
		required, _ := mandatory.(bool)
		t.Columns = append(t.Columns, &model.Column{
			Name:   key,
			Type:   typ,
			Native: typ,
			// `mandatory` means every node with this label carries the property
			// today — which the procedure computes from the data, not from a
			// constraint. It is the strongest statement available and still
			// weaker than NOT NULL.
			Nullable: !required,
		})
	}

	sort.Strings(order)
	out := make([]*model.Table, 0, len(order))
	for _, name := range order {
		t := byName[name]
		sort.Slice(t.Columns, func(i, j int) bool { return t.Columns[i].Name < t.Columns[j].Name })
		out = append(out, t)
	}
	return out, nil
}

// label pulls the label out of a procedure row, which spells it as a
// single-element list for nodes and as a `:`-wrapped string for relationships.
func label(rec *neo4j.Record, key string) string {
	v, ok := rec.Get(key)
	if !ok {
		return ""
	}
	switch x := v.(type) {
	case string:
		return strings.Trim(x, ":`")
	case []any:
		if len(x) == 0 {
			return ""
		}
		s, _ := x[0].(string)
		return s
	default:
		return ""
	}
}

// propertyType takes the first of the types a property has been seen with. A
// property whose type varies across nodes is legal in Neo4j and there is no
// single honest answer for it; the first keeps the result stable in the common
// case where they agree.
func propertyType(rec *neo4j.Record) string {
	v, ok := rec.Get("propertyTypes")
	if !ok {
		return "Any"
	}
	list, ok := v.([]any)
	if !ok || len(list) == 0 {
		return "Any"
	}
	s, _ := list[0].(string)
	if s == "" {
		return "Any"
	}
	return s
}

func (d *Driver) session(ctx context.Context, mode neo4j.AccessMode) neo4j.SessionWithContext {
	return d.driver.NewSession(ctx, neo4j.SessionConfig{
		DatabaseName: d.database,
		AccessMode:   mode,
	})
}

// Begin implements db.Driver with a real explicit transaction. Neo4j is one of
// the two engines in this group where that phrase means what it means on
// Postgres: the writes are invisible until commit and disappear on rollback.
func (d *Driver) Begin(ctx context.Context) (db.Tx, error) {
	sess := d.session(ctx, neo4j.AccessModeWrite)
	tx, err := sess.BeginTransaction(ctx)
	if err != nil {
		_ = sess.Close(ctx)
		return nil, fmt.Errorf("begin: %w", err)
	}
	return &Tx{sess: sess, tx: tx}, nil
}

// Tx is a Neo4j seeding transaction, and is one.
type Tx struct {
	sess neo4j.SessionWithContext
	tx   neo4j.ExplicitTransaction
	done bool
}

// createBatch is how many nodes one UNWIND creates.
//
// The whole point of UNWIND is that a batch is one query rather than one query
// per node: the planner compiles the CREATE once and runs it over the list. The
// batch is bounded because the transaction holds every uncommitted change in
// heap until commit, and a single unbounded list would be materialised there in
// its entirety on top of that.
const createBatch = 1000

// Insert implements db.Tx with UNWIND over a batch of property maps.
func (t *Tx) Insert(ctx context.Context, tb *model.Table, cols []string, rows db.Source) (int64, error) {
	if tb.Schema == relationshipSchema {
		return 0, fmt.Errorf("cannot seed %s: it is a relationship type, and a relationship needs "+
			"two endpoint nodes that a row of generated values does not name — seed the node labels "+
			"and connect them with your own Cypher", tb.Name)
	}
	if len(cols) == 0 {
		return 0, nil
	}
	// `SET n = row` writes exactly the keys the map carries, so a column the
	// plan skipped is simply not a property on the created node — which is what
	// absence means in a graph.
	query := fmt.Sprintf("UNWIND $rows AS row CREATE (n:%s) SET n = row", quote(tb.Name))

	batch := make([]any, 0, createBatch)
	var written int64
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		res, err := t.tx.Run(ctx, query, map[string]any{"rows": batch})
		if err != nil {
			return fmt.Errorf("insert into %s: %w", tb.Name, err)
		}
		// The result has to be consumed before the next query on the same
		// transaction: until it is, the records are still streaming and the
		// write it describes has not necessarily been applied.
		if _, err := res.Consume(ctx); err != nil {
			return fmt.Errorf("insert into %s: %w", tb.Name, err)
		}
		written += int64(len(batch))
		batch = batch[:0]
		return nil
	}

	var loopErr error
	for row := range rows.Rows() {
		props := make(map[string]any, len(cols))
		for _, c := range cols {
			if v, ok := row[c]; ok && v != nil {
				props[c] = value(v)
			}
		}
		batch = append(batch, props)
		if len(batch) == createBatch {
			if err := flush(); err != nil {
				loopErr = err
				break
			}
		}
	}
	if loopErr != nil {
		return written, loopErr
	}
	// A generator that failed halfway just stops yielding; without this check a
	// short write would look like a complete one.
	if err := rows.Err(); err != nil {
		return written, fmt.Errorf("generate rows for %s: %w", tb.Name, err)
	}
	if err := flush(); err != nil {
		return written, err
	}
	return written, nil
}

// value adapts the few generated types Bolt will not carry. A property can only
// be a primitive or a list of primitives — there are no nested documents in a
// graph, which is what having relationships is for.
func value(v any) any {
	switch x := v.(type) {
	case uint64:
		return int64(x)
	case map[string]any, []any:
		// A generated JSON value has nowhere to go: storing it as a property is
		// not allowed, so it goes in as its text form rather than failing the
		// batch.
		return fmt.Sprint(x)
	default:
		return v
	}
}

// Truncate implements db.Tx with DETACH DELETE, which removes the relationships
// attached to each node as well as the node itself. Without DETACH the delete is
// refused for any node still connected to something, which for a graph is most
// of them.
func (t *Tx) Truncate(ctx context.Context, tb *model.Table) error {
	if tb.Schema == relationshipSchema {
		res, err := t.tx.Run(ctx, fmt.Sprintf("MATCH ()-[r:%s]->() DELETE r", quote(tb.Name)), nil)
		if err == nil {
			_, err = res.Consume(ctx)
		}
		if err != nil {
			return fmt.Errorf("truncate %s: %w", tb.Name, err)
		}
		return nil
	}
	res, err := t.tx.Run(ctx, fmt.Sprintf("MATCH (n:%s) DETACH DELETE n", quote(tb.Name)), nil)
	if err == nil {
		_, err = res.Consume(ctx)
	}
	if err != nil {
		return fmt.Errorf("truncate %s: %w", tb.Name, err)
	}
	return nil
}

// ReadKeys implements db.Tx. It reads inside the transaction on purpose: the
// nodes it returns were created by this same uncommitted run, and no other
// session can see them yet.
func (t *Tx) ReadKeys(ctx context.Context, tb *model.Table, col string, limit int) ([]any, error) {
	query := fmt.Sprintf("MATCH (n:%s) WHERE n.%s IS NOT NULL RETURN n.%s AS v LIMIT $limit",
		quote(tb.Name), quote(col), quote(col))
	res, err := t.tx.Run(ctx, query, map[string]any{"limit": limit})
	if err != nil {
		return nil, fmt.Errorf("read keys from %s.%s: %w", tb.Name, col, err)
	}
	records, err := res.Collect(ctx)
	if err != nil {
		return nil, fmt.Errorf("read keys from %s.%s: %w", tb.Name, col, err)
	}
	var out []any
	for _, rec := range records {
		if v, ok := rec.Get("v"); ok && v != nil {
			out = append(out, v)
		}
	}
	return out, nil
}

// Exec implements db.Tx by running the statement inside the transaction.
//
// The schema editor renders nothing for the Document dialect, so what arrives
// here is Cypher somebody wrote. It is worth passing through — a data fix-up is
// a real thing to want — but Neo4j's own schema commands (CREATE CONSTRAINT,
// CREATE INDEX) cannot run inside an explicit transaction and the server will
// refuse them here rather than this driver doing it.
func (t *Tx) Exec(ctx context.Context, stmt string) error {
	res, err := t.tx.Run(ctx, stmt, nil)
	if err == nil {
		_, err = res.Consume(ctx)
	}
	if err != nil {
		return fmt.Errorf("exec: %w", err)
	}
	return nil
}

// Commit implements db.Tx.
func (t *Tx) Commit(ctx context.Context) error {
	if t.done {
		return nil
	}
	t.done = true
	err := t.tx.Commit(ctx)
	_ = t.sess.Close(ctx)
	return err
}

// Rollback implements db.Tx, and genuinely rolls back: every node created and
// every node deleted in this transaction is undone by the server. It is safe
// after Commit, so the seeder can defer it unconditionally.
func (t *Tx) Rollback(ctx context.Context) error {
	if t.done {
		return nil
	}
	t.done = true
	err := t.tx.Rollback(ctx)
	_ = t.sess.Close(ctx)
	return err
}

// quote is Cypher's identifier quoting: a backtick-wrapped name, with an
// embedded backtick doubled.
func quote(s string) string {
	return "`" + strings.ReplaceAll(s, "`", "``") + "`"
}
