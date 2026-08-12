// Package redis implements the Seedora driver for Redis.
//
// This is the engine where the product question the rest of this list raises
// cannot be dodged. Redis has no schema of any kind and no way to derive one.
// There is no catalog, no mapping, no key schema, and — unlike MongoDB or
// DynamoDB — no way to sample a collection either, because there are no
// collections: there is one flat keyspace of strings, and what a key means is
// entirely a convention held in the application that wrote it. `SCAN` returns
// key names; `TYPE` says whether one holds a string, a hash, a list, a set, or a
// stream. Neither says what a record is or what fields it should have.
//
// So Introspect returns an empty schema and an error saying that. It could
// instead SCAN a thousand keys, split them on ':', and present the prefixes as
// tables — which would be a fabrication: `user:1:sessions` and `user:1` share a
// prefix and are not the same thing, and a keyspace with no colons at all would
// yield one table containing everything. Presenting a guess as a catalog is
// worse than presenting nothing, because the user cannot tell which they got.
//
// The workable source is a key template the user writes — `user:{id}` with a
// field list — which is a feature above the driver layer and not something this
// package can invent. Until that exists, the rest of the driver is here and
// works: given a table (from a template, or from a plan written by hand) it
// seeds it, truncates it, and reads keys back.
//
// Redis has no transaction. MULTI/EXEC queues commands and runs them without
// interruption, which is isolation, not atomicity: there is no undo, a failing
// command does not abort the ones around it, and DISCARD only works before EXEC.
// Rollback below says so.
package redis

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/redis/go-redis/v9"

	"github.com/bakhod1r/seedora/internal/db"
	"github.com/bakhod1r/seedora/internal/ddl"
	"github.com/bakhod1r/seedora/internal/model"
)

func init() {
	db.Register(open, "redis")
}

// Driver is a connected Redis server.
type Driver struct {
	client *redis.Client
}

func open(ctx context.Context, dsn string) (db.Driver, error) {
	opts, err := redis.ParseURL(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse DSN: %w", err)
	}
	client := redis.NewClient(opts)
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("connect: %w", err)
	}
	return &Driver{client: client}, nil
}

// Name implements db.Driver.
func (d *Driver) Name() string { return "Redis" }

// Dialect implements db.Driver.
func (d *Driver) Dialect() ddl.Dialect { return ddl.Document }

// Close implements db.Driver.
func (d *Driver) Close(context.Context) error { return d.client.Close() }

// History implements db.Driver, and finds nothing. There is nowhere for a
// migration tool to have written anything, and nothing that could have been
// migrated.
func (d *Driver) History(context.Context) ([]model.Migration, error) { return nil, nil }

// Introspect implements db.Driver by returning an empty schema and saying why.
//
// See the package comment. Every other driver in this tree answers this by
// reading something the database knows; Redis knows nothing about the shape of
// what it stores, so the honest answer is the empty one plus an explanation the
// user can act on. The schema is returned rather than left nil so a caller that
// shows the error and carries on has something valid to hold.
func (d *Driver) Introspect(context.Context) (*model.Schema, error) {
	return &model.Schema{Enums: map[string]model.Values{}},
		fmt.Errorf("redis has no schema to introspect: the keyspace is flat and untyped, " +
			"and what a key means is a convention held in your application rather than " +
			"anything the server records — seeding it needs a key template " +
			"(`user:{id}` and the fields a user has), which Seedora cannot derive from the database")
}

// Begin implements db.Driver. There is no transaction to open; see Rollback.
func (d *Driver) Begin(context.Context) (db.Tx, error) {
	return &Tx{client: d.client}, nil
}

// Tx is a Redis seeding run, which is not a transaction.
type Tx struct {
	client *redis.Client
	done   bool
	// written counts commands already applied, which is what Rollback reports it
	// cannot take back.
	written int64
	// counters give each table's rows a distinct key across the whole run, for
	// tables whose plan names no primary key column.
	counters map[string]int64
}

// pipelineCommands is how many commands are buffered before a round trip.
//
// A pipeline is Redis's bulk path: the commands go out in one write and the
// replies come back in one read, so the cost per command drops to its parsing.
// The bound is on memory rather than protocol — there is no server-side limit —
// and a thousand is enough to amortise the round trip completely.
const pipelineCommands = 1000

// Insert implements db.Tx by writing each row as a hash under a key derived from
// the table name and the row's primary key.
//
// A hash is the only Redis type a row maps onto without losing anything: field
// names and values, addressable individually. The key is `table:<pk>`, which is
// the convention nearly every application already uses, and a table whose plan
// names no primary key falls back to a per-run counter so the rows at least do
// not overwrite each other.
func (t *Tx) Insert(ctx context.Context, tb *model.Table, cols []string, rows db.Source) (int64, error) {
	if len(cols) == 0 {
		return 0, nil
	}
	keyCol := ""
	if len(tb.PrimaryKey) == 1 {
		keyCol = tb.PrimaryKey[0]
	}

	pipe := t.client.Pipeline()
	var written, pending int64
	flush := func() error {
		if pending == 0 {
			return nil
		}
		if _, err := pipe.Exec(ctx); err != nil {
			return fmt.Errorf("insert into %s: %w", tb.Name, err)
		}
		written += pending
		t.written += pending
		pending = 0
		return nil
	}

	var loopErr error
	for row := range rows.Rows() {
		fields := make([]any, 0, len(cols)*2)
		for _, c := range cols {
			if v, ok := row[c]; ok && v != nil {
				// Redis stores bytes: everything is rendered to its text form on
				// the way in, and there is no type on the way out to render it
				// back with. That is a real loss and it is the engine's, not
				// this driver's.
				fields = append(fields, c, fmt.Sprint(v))
			}
		}
		if len(fields) == 0 {
			continue
		}
		pipe.HSet(ctx, t.key(tb, row, keyCol), fields...)
		pending++
		if pending == pipelineCommands {
			if err := flush(); err != nil {
				loopErr = err
				break
			}
		}
	}
	if loopErr != nil {
		return written, loopErr
	}
	// A generator that stopped early just stops yielding; without this check the
	// short write would look like a complete one.
	if err := rows.Err(); err != nil {
		return written, fmt.Errorf("generate rows for %s: %w", tb.Name, err)
	}
	if err := flush(); err != nil {
		return written, err
	}
	return written, nil
}

// key builds the key one row is stored under.
func (t *Tx) key(tb *model.Table, row map[string]any, keyCol string) string {
	if keyCol != "" {
		if v, ok := row[keyCol]; ok && v != nil {
			return tb.Name + ":" + fmt.Sprint(v)
		}
	}
	if t.counters == nil {
		t.counters = map[string]int64{}
	}
	t.counters[tb.Name]++
	return tb.Name + ":" + strconv.FormatInt(t.counters[tb.Name], 10)
}

// Truncate implements db.Tx by removing every key under the table's prefix.
//
// SCAN rather than KEYS: KEYS walks the whole keyspace in one blocking pass and
// is the single most reliable way to stall a production Redis. UNLINK rather
// than DEL: the memory is reclaimed on a background thread, so emptying a large
// prefix does not block the server for the duration.
//
// This is also the clearest illustration of what having no schema costs. A
// prefix is not a table, and there is no way to be sure `user:*` does not also
// match something the application put there for its own reasons.
func (t *Tx) Truncate(ctx context.Context, tb *model.Table) error {
	pattern := tb.Name + ":*"
	var cursor uint64
	for {
		keys, next, err := t.client.Scan(ctx, cursor, pattern, 500).Result()
		if err != nil {
			return fmt.Errorf("truncate %s: %w", tb.Name, err)
		}
		if len(keys) > 0 {
			if err := t.client.Unlink(ctx, keys...).Err(); err != nil {
				return fmt.Errorf("truncate %s: %w", tb.Name, err)
			}
			t.written += int64(len(keys))
		}
		if next == 0 {
			return nil
		}
		cursor = next
	}
}

// ReadKeys implements db.Tx by scanning the table's prefix and reading one field
// out of each hash. There is no index to consult: this is a walk of the
// keyspace, bounded by the limit the caller asked for.
func (t *Tx) ReadKeys(ctx context.Context, tb *model.Table, col string, limit int) ([]any, error) {
	pattern := tb.Name + ":*"
	var cursor uint64
	var out []any
	for len(out) < limit {
		keys, next, err := t.client.Scan(ctx, cursor, pattern, int64(limit)).Result()
		if err != nil {
			return nil, fmt.Errorf("read keys from %s.%s: %w", tb.Name, col, err)
		}
		for _, k := range keys {
			v, err := t.client.HGet(ctx, k, col).Result()
			if err == redis.Nil {
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("read keys from %s.%s: %w", tb.Name, col, err)
			}
			out = append(out, v)
			if len(out) == limit {
				return out, nil
			}
		}
		if next == 0 {
			return out, nil
		}
		cursor = next
	}
	return out, nil
}

// Exec implements db.Tx by refusing. There is no DDL and nothing that resembles
// one: a key comes into existence when it is written and has no declared shape
// for a statement to change.
func (t *Tx) Exec(_ context.Context, stmt string) error {
	return fmt.Errorf("redis takes no DDL: there is nothing to declare and no statement to "+
		"apply (%s)", strings.TrimSpace(stmt))
}

// Commit implements db.Tx. Every command was applied as it was pipelined.
func (t *Tx) Commit(context.Context) error {
	t.done = true
	return nil
}

// Rollback implements db.Tx by reporting that it cannot undo the writes.
//
// MULTI/EXEC is not a transaction in the sense db.Tx means: it guarantees the
// queued commands run without another client interleaving, and nothing more.
// There is no undo after EXEC, a command that fails at runtime does not abort
// the others, and DISCARD only helps before EXEC has been sent. Returning nil
// would tell the seeder the keyspace was restored when the keys are still there.
func (t *Tx) Rollback(context.Context) error {
	if t.done || t.written == 0 {
		t.done = true
		return nil
	}
	t.done = true
	return fmt.Errorf("redis cannot roll back: %d commands have already been applied and are "+
		"permanent — MULTI/EXEC provides isolation, not atomicity, so undoing them means "+
		"deleting the keys yourself", t.written)
}
