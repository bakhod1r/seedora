// Package cassandra implements the Seedora driver for Apache Cassandra and
// ScyllaDB, which speak the same protocol and carry the same catalog.
//
// Of the non-relational engines this is the one that fits the existing model
// almost unchanged: `system_schema` is a genuine catalog with typed columns, a
// declared primary key, and a keyspace that behaves like a schema. Nothing here
// is inferred from stored data — every column below is something the server was
// told about at CREATE TABLE time.
//
// What does not fit is the transaction. CQL has none. A logged batch is not a
// transaction: it is a promise that the writes eventually all land, bought with
// a write to the batchlog on two other nodes, and it cannot be rolled back. So
// Begin returns a transaction object because the interface asks for one, the
// writes go out as they are made, and Rollback says plainly that it cannot take
// them back. Documenting that is worth more than a Rollback that returns nil.
package cassandra

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gocql/gocql"

	"github.com/bakhod1r/seedora/internal/db"
	"github.com/bakhod1r/seedora/internal/ddl"
	"github.com/bakhod1r/seedora/internal/model"
)

func init() {
	db.Register(open, "cassandra", "scylla")
}

// Driver is a connected Cassandra keyspace.
type Driver struct {
	session  *gocql.Session
	keyspace string
	name     string
}

// open connects to the cluster named by the DSN.
//
// The DSN is `cassandra://user:pass@host1,host2:9042/keyspace`. The keyspace is
// required: it is what a "schema" means here, and every catalog query below is
// scoped to it.
func open(ctx context.Context, dsn string) (db.Driver, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse DSN: %w", err)
	}
	keyspace := strings.TrimPrefix(u.Path, "/")
	if keyspace == "" {
		return nil, fmt.Errorf("the DSN names no keyspace — add one, as in cassandra://localhost:9042/myapp")
	}

	host, port := u.Hostname(), u.Port()
	if host == "" {
		host = "127.0.0.1"
	}
	hosts := strings.Split(host, ",")
	if port != "" {
		for i, h := range hosts {
			hosts[i] = h + ":" + port
		}
	}

	cluster := gocql.NewCluster(hosts...)
	cluster.Keyspace = keyspace
	cluster.Timeout = 15 * time.Second
	cluster.ConnectTimeout = 15 * time.Second
	// QUORUM rather than the default: a seed read back by ReadKeys has to see
	// the rows this same run just wrote, and ONE does not guarantee that on a
	// replicated keyspace.
	cluster.Consistency = gocql.Quorum
	if u.User != nil {
		pass, _ := u.User.Password()
		cluster.Authenticator = gocql.PasswordAuthenticator{
			Username: u.User.Username(),
			Password: pass,
		}
	}

	sess, err := cluster.CreateSession()
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	d := &Driver{session: sess, keyspace: keyspace, name: "Cassandra"}
	// Scylla answers the same catalog queries but reports itself in
	// system.local; the name only reaches the UI header.
	var product string
	if err := sess.Query("SELECT release_version FROM system.local").
		WithContext(ctx).Scan(&product); err == nil && strings.Contains(strings.ToLower(product), "scylla") {
		d.name = "ScyllaDB"
	}
	return d, nil
}

// Name implements db.Driver.
func (d *Driver) Name() string { return d.name }

// Dialect implements db.Driver.
func (d *Driver) Dialect() ddl.Dialect { return ddl.CQL }

// Close implements db.Driver.
func (d *Driver) Close(context.Context) error {
	d.session.Close()
	return nil
}

// History implements db.Driver, and finds nothing.
//
// The migration-tool catalogue in internal/db/history.go is built on queries CQL
// cannot run: it orders by a column that is not part of the primary key, which
// Cassandra refuses outright. No migration tool writes a bookkeeping table into
// a Cassandra keyspace in a shape that could be read that way either, so this
// returns nothing rather than issuing eight queries that are all going to fail.
func (d *Driver) History(context.Context) ([]model.Migration, error) { return nil, nil }

// Introspect reads system_schema, which is a real catalog: the tables, their
// columns, each column's declared CQL type, and which columns form the primary
// key. Two queries, both keyspace-scoped.
func (d *Driver) Introspect(ctx context.Context) (*model.Schema, error) {
	s := &model.Schema{Enums: map[string]model.Values{}}

	byName := map[string]*model.Table{}
	iter := d.session.Query(
		"SELECT table_name FROM system_schema.tables WHERE keyspace_name = ?", d.keyspace).
		WithContext(ctx).Iter()
	var name string
	for iter.Scan(&name) {
		t := &model.Table{Schema: d.keyspace, Name: name}
		byName[name] = t
		s.Tables = append(s.Tables, t)
	}
	if err := iter.Close(); err != nil {
		return nil, fmt.Errorf("read tables: %w", err)
	}

	if err := d.loadColumns(ctx, byName); err != nil {
		return nil, err
	}
	return s, nil
}

// loadColumns reads every column in the keyspace in one query and assembles the
// primary key from the `kind` and `position` the catalog carries.
//
// The key ordering matters and is not the column ordering: partition key columns
// come first in their own position order, then the clustering columns in theirs.
// That is the order the key is spelled in, and getting it wrong would put the
// wrong column in the partition.
func (d *Driver) loadColumns(ctx context.Context, byName map[string]*model.Table) error {
	partition := map[string][]keyPart{}
	clustering := map[string][]keyPart{}

	iter := d.session.Query(`
SELECT table_name, column_name, type, kind, position
FROM system_schema.columns
WHERE keyspace_name = ?`, d.keyspace).WithContext(ctx).Iter()

	var table, column, typ, kind string
	var position int
	for iter.Scan(&table, &column, &typ, &kind, &position) {
		t := byName[table]
		if t == nil {
			continue
		}
		c := &model.Column{
			Name:   column,
			Type:   baseType(typ),
			Native: typ,
			// Every column outside the primary key may be left unset, and an
			// unset column simply has no cell — which reads back as null.
			Nullable: kind != "partition_key" && kind != "clustering",
		}
		t.Columns = append(t.Columns, c)

		switch kind {
		case "partition_key":
			partition[table] = append(partition[table], keyPart{column, position})
		case "clustering":
			clustering[table] = append(clustering[table], keyPart{column, position})
		}
	}
	if err := iter.Close(); err != nil {
		return fmt.Errorf("read columns: %w", err)
	}

	for name, t := range byName {
		key := append(sortByPosition(partition[name]), sortByPosition(clustering[name])...)
		t.PrimaryKey = key
		// A single-column primary key is the one case where uniqueness of the
		// whole key is uniqueness of a column, which is what the generator can
		// actually act on. A compound key is left unmarked: the pair is unique,
		// neither half is.
		if len(key) == 1 {
			if c := t.Column(key[0]); c != nil {
				c.Unique = true
			}
		}
	}
	// ExistingRows is deliberately left at zero. There is no row estimate in
	// system_schema, and COUNT(*) is a full scan of every partition on every
	// replica — on a table large enough for the number to matter it is the most
	// expensive thing this tool could do, and it would time out before it
	// answered. The truncate confirmation says less here as a result.
	return nil
}

// keyPart is one column of a primary key and where it sits in it.
type keyPart struct {
	column   string
	position int
}

// sortByPosition puts a key's columns in the order the key is declared in.
func sortByPosition(parts []keyPart) []string {
	for i := 1; i < len(parts); i++ {
		for j := i; j > 0 && parts[j].position < parts[j-1].position; j-- {
			parts[j], parts[j-1] = parts[j-1], parts[j]
		}
	}
	out := make([]string, len(parts))
	for i, p := range parts {
		out[i] = p.column
	}
	return out
}

// baseType strips a collection or frozen wrapper down to the name inference
// cares about: `frozen<list<text>>` is a list however it is stored.
func baseType(t string) string {
	t = strings.TrimSpace(t)
	if i := strings.IndexByte(t, '<'); i > 0 {
		base := t[:i]
		if base == "frozen" {
			return baseType(t[i+1 : len(t)-1])
		}
		return base
	}
	return t
}

// Begin implements db.Driver. There is nothing to begin: CQL has no transaction,
// so this hands back a handle on the same session and the Tx below is honest
// about what that means.
func (d *Driver) Begin(context.Context) (db.Tx, error) {
	return &Tx{session: d.session, keyspace: d.keyspace}, nil
}

// Tx is a Cassandra seeding run. It is not a transaction and does not pretend to
// be one; it exists because db.Tx is how the seeder is shaped.
type Tx struct {
	session  *gocql.Session
	keyspace string
	done     bool
	// applied counts the statements that have already landed on the cluster,
	// which is what Rollback has to report it cannot take back.
	applied int64
}

// writers is the number of concurrent single-row writes in flight.
//
// This, not a batch, is how Cassandra is loaded fast. A logged batch is written
// to the batchlog on two other nodes before any of it is applied, so it costs
// more than the writes it carries; an unlogged batch skips that but is only
// cheap when every statement in it targets the same partition, which generated
// rows almost never do — a multi-partition batch makes the coordinator fan out
// and wait for all of them, turning independent writes into one slow one. Many
// small writes in parallel let every replica take work directly. The number is a
// concurrency limit rather than a batch size for the same reason: there is no
// statement-size ceiling to respect (batch_size_fail_threshold_in_kb, 50 KB by
// default) when nothing is batched.
const writers = 32

// Insert implements db.Tx with concurrent prepared INSERTs. gocql prepares the
// statement once and caches it per connection, so the parse cost is paid once
// however many rows go through.
func (t *Tx) Insert(ctx context.Context, tb *model.Table, cols []string, rows db.Source) (int64, error) {
	if len(cols) == 0 {
		return 0, nil
	}
	names := make([]string, len(cols))
	marks := make([]string, len(cols))
	for i, c := range cols {
		names[i] = quote(c)
		marks[i] = "?"
	}
	stmt := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)",
		t.qualified(tb), strings.Join(names, ", "), strings.Join(marks, ", "))

	work := make(chan []any, writers*2)
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		written int64
		failed  error
	)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for args := range work {
				if err := t.session.Query(stmt, args...).WithContext(ctx).Exec(); err != nil {
					mu.Lock()
					if failed == nil {
						failed = fmt.Errorf("insert into %s: %w", tb.Name, err)
						// The rows already sent are staying where they are;
						// stopping early only limits how many more join them.
						cancel()
					}
					mu.Unlock()
					continue
				}
				mu.Lock()
				written++
				mu.Unlock()
			}
		}()
	}

	for row := range rows.Rows() {
		args := make([]any, len(cols))
		for i, c := range cols {
			args[i] = value(row[c])
		}
		select {
		case work <- args:
		case <-ctx.Done():
		}
		mu.Lock()
		stop := failed != nil
		mu.Unlock()
		if stop {
			break
		}
	}
	close(work)
	wg.Wait()

	t.applied += written
	if failed != nil {
		return written, failed
	}
	// A generator that stopped early just stops yielding, so without this the
	// short write would look like a complete one.
	if err := rows.Err(); err != nil {
		return written, fmt.Errorf("generate rows for %s: %w", tb.Name, err)
	}
	return written, nil
}

// value adapts the few generated types the protocol will not take as they are.
func value(v any) any {
	switch x := v.(type) {
	case uint64:
		// CQL bigint is signed, and nothing a generator produces is outside it.
		return int64(x)
	case uint32:
		return int64(x)
	default:
		return x
	}
}

// Truncate implements db.Tx with TRUNCATE, which on Cassandra takes a snapshot
// on every node and then drops every SSTable for the table. It is immediate,
// cluster-wide, and outside anything this run could undo — Rollback below says
// so. It also requires every replica to be up, and fails rather than partially
// emptying if one is not, which is the safer of the two behaviours.
func (t *Tx) Truncate(ctx context.Context, tb *model.Table) error {
	if err := t.session.Query("TRUNCATE " + t.qualified(tb)).WithContext(ctx).Exec(); err != nil {
		return fmt.Errorf("truncate %s: %w", tb.Name, err)
	}
	t.applied++
	return nil
}

// ReadKeys implements db.Tx. There is no isolation to worry about: the rows this
// run wrote are already visible to everyone, which is the flip side of having no
// transaction.
func (t *Tx) ReadKeys(ctx context.Context, tb *model.Table, col string, limit int) ([]any, error) {
	q := fmt.Sprintf("SELECT %s FROM %s LIMIT %s", quote(col), t.qualified(tb), strconv.Itoa(limit))
	iter := t.session.Query(q).WithContext(ctx).Iter()
	var out []any
	for {
		row := map[string]any{}
		if !iter.MapScan(row) {
			break
		}
		if v, ok := row[col]; ok && v != nil {
			out = append(out, v)
		}
	}
	if err := iter.Close(); err != nil {
		return nil, fmt.Errorf("read keys from %s.%s: %w", tb.Name, col, err)
	}
	return out, nil
}

// Exec implements db.Tx. CQL DDL is applied immediately and cluster-wide; there
// is no transaction to hold it, so unlike Postgres a schema change made here
// cannot be undone by failing the run. Statements are still validated and
// rendered before they run, and they still run in order.
func (t *Tx) Exec(ctx context.Context, stmt string) error {
	if err := t.session.Query(stmt).WithContext(ctx).Exec(); err != nil {
		return fmt.Errorf("exec: %w", err)
	}
	t.applied++
	return nil
}

// Commit implements db.Tx. Every write already landed as it was made, so there
// is nothing to commit — this only marks the run finished so a deferred Rollback
// stays quiet.
func (t *Tx) Commit(context.Context) error {
	t.done = true
	return nil
}

// Rollback implements db.Tx by reporting that it cannot do what the name says.
//
// Returning nil here would be a lie the caller acts on: it would read as "the
// database is back as it was" when the rows are on disk on three nodes. A no-op
// after Commit is still a no-op, and a run that wrote nothing has nothing to
// report, so the error only appears when there is something the user genuinely
// needs to clean up by hand.
func (t *Tx) Rollback(context.Context) error {
	if t.done || t.applied == 0 {
		t.done = true
		return nil
	}
	t.done = true
	return fmt.Errorf("cassandra cannot roll back: %d statements have already been applied "+
		"and are permanent — CQL has no transaction, so undoing them means deleting the rows yourself", t.applied)
}

func (t *Tx) qualified(tb *model.Table) string {
	ks := tb.Schema
	if ks == "" {
		ks = t.keyspace
	}
	return quote(ks) + "." + quote(tb.Name)
}

// quote is CQL's identifier quoting, which is the SQL-standard double quote.
func quote(s string) string { return ddl.QuoteIdent(ddl.CQL, s) }
