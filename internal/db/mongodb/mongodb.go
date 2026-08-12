// Package mongodb implements the Seedora driver for MongoDB.
//
// The thing to be clear about is where the schema comes from, because MongoDB
// has no catalog. `listCollections` names the collections and says nothing about
// their fields: there is no table of columns to read, because a collection never
// declared any. So Introspect below has two sources, and they are not equal:
//
//   - A `$jsonSchema` validator, when the collection has one. This is a real
//     declared schema — somebody wrote it down, the server enforces it, and the
//     required list is a genuine NOT NULL. It is used verbatim.
//   - Otherwise, a sample of the documents already stored. Everything that comes
//     out of that is inference from data: the fields are the fields those
//     documents happened to have, the types are the types those values happened
//     to be, and a field absent from every sampled document does not exist as far
//     as this driver can tell. It is a good guess and it is still a guess, which
//     is why the two paths are kept visibly separate rather than merged.
//
// Transactions are real here, but only on a replica set or a sharded cluster: a
// standalone mongod has no oplog to build them on and refuses to start one.
// Begin tries, and falls back to writing without one rather than failing the
// run — with Rollback saying which of the two happened.
package mongodb

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/bakhod1r/seedora/internal/db"
	"github.com/bakhod1r/seedora/internal/ddl"
	"github.com/bakhod1r/seedora/internal/model"
)

func init() {
	db.Register(open, "mongodb")
}

// Driver is a connected MongoDB database.
type Driver struct {
	client *mongo.Client
	db     *mongo.Database
}

func open(ctx context.Context, dsn string) (db.Driver, error) {
	client, err := mongo.Connect(options.Client().ApplyURI(dsn))
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	// The URI carries the database in its path, and there is no server-side
	// default to fall back on the way `SELECT DATABASE()` gives one on MySQL.
	name := databaseFromURI(dsn)
	if name == "" {
		_ = client.Disconnect(ctx)
		return nil, fmt.Errorf("the DSN names no database — add one, as in mongodb://localhost:27017/myapp_dev")
	}
	if err := client.Ping(ctx, nil); err != nil {
		_ = client.Disconnect(ctx)
		return nil, fmt.Errorf("connect: %w", err)
	}
	return &Driver{client: client, db: client.Database(name)}, nil
}

// databaseFromURI pulls the database out of a mongodb:// URI. url.Parse would
// do for the simple case, but a seed list with several hosts is not a valid URL
// host, so the path is taken by hand from after the last host.
func databaseFromURI(dsn string) string {
	rest := dsn
	if i := strings.Index(rest, "://"); i >= 0 {
		rest = rest[i+3:]
	}
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		rest = rest[i+1:]
	} else {
		return ""
	}
	if i := strings.IndexAny(rest, "?"); i >= 0 {
		rest = rest[:i]
	}
	return rest
}

// Name implements db.Driver.
func (d *Driver) Name() string { return "MongoDB" }

// Dialect implements db.Driver.
func (d *Driver) Dialect() ddl.Dialect { return ddl.Document }

// Close implements db.Driver.
func (d *Driver) Close(ctx context.Context) error { return d.client.Disconnect(ctx) }

// History implements db.Driver, and finds nothing. The migration-tool catalogue
// in internal/db/history.go is a list of SQL tables; no tool writes one into a
// MongoDB database, and the server keeps no record of past collection changes
// because there were never any changes to record.
func (d *Driver) History(context.Context) ([]model.Migration, error) { return nil, nil }

// sampleSize is how many documents are read to infer a collection's fields.
//
// $sample with a size below 5% of the collection uses a pseudo-random cursor and
// touches only that many documents, so this is cheap on a large collection and a
// full scan on a small one — which is the right shape either way. Two hundred is
// enough to see a field present in a few percent of documents, and small enough
// that the whole introspection stays under a second.
const sampleSize = 200

// Introspect lists the collections and works out what a document in each looks
// like. See the package comment: a $jsonSchema validator is a declared schema
// and is used as one; everything else is inferred from stored documents.
func (d *Driver) Introspect(ctx context.Context) (*model.Schema, error) {
	specs, err := d.db.ListCollectionSpecifications(ctx, bson.D{})
	if err != nil {
		return nil, fmt.Errorf("list collections: %w", err)
	}

	s := &model.Schema{Enums: map[string]model.Values{}}
	for _, spec := range specs {
		// A view is a stored aggregation over another collection and cannot be
		// written to, so it is not something to seed.
		if spec.Type == "view" || strings.HasPrefix(spec.Name, "system.") {
			continue
		}
		t := &model.Table{Name: spec.Name}

		if cols := declared(spec.Options); cols != nil {
			t.Columns = cols
		} else {
			cols, err := d.sample(ctx, spec.Name)
			if err != nil {
				return nil, err
			}
			t.Columns = cols
		}

		n, err := d.db.Collection(spec.Name).EstimatedDocumentCount(ctx)
		if err != nil {
			return nil, fmt.Errorf("count %s: %w", spec.Name, err)
		}
		t.ExistingRows = n
		s.Tables = append(s.Tables, t)
	}
	sort.Slice(s.Tables, func(i, j int) bool { return s.Tables[i].Name < s.Tables[j].Name })
	return s, nil
}

// declared reads a collection's $jsonSchema validator, and returns nil when
// there is none.
//
// This is the one place in this driver where the schema is something the
// database was actually told, rather than something worked out from the data:
// `required` is a real NOT NULL, and `bsonType` is a real declared type. Only
// top-level properties are read — a nested object's fields are addressed with a
// dotted path that InsertMany would treat as a literal key, so promoting them to
// columns would produce documents nobody wants.
func declared(opts bson.Raw) []*model.Column {
	if len(opts) == 0 {
		return nil
	}
	validator, err := opts.LookupErr("validator")
	if err != nil {
		return nil
	}
	raw, ok := validator.DocumentOK()
	if !ok {
		return nil
	}
	schema, err := raw.LookupErr("$jsonSchema")
	if err != nil {
		return nil
	}
	doc, ok := schema.DocumentOK()
	if !ok {
		return nil
	}

	required := map[string]bool{}
	if v, err := doc.LookupErr("required"); err == nil {
		if arr, ok := v.ArrayOK(); ok {
			values, _ := arr.Values()
			for _, name := range values {
				if s, ok := name.StringValueOK(); ok {
					required[s] = true
				}
			}
		}
	}
	props, err := doc.LookupErr("properties")
	if err != nil {
		return nil
	}
	propsDoc, ok := props.DocumentOK()
	if !ok {
		return nil
	}
	elements, err := propsDoc.Elements()
	if err != nil {
		return nil
	}

	var out []*model.Column
	for _, e := range elements {
		name := e.Key()
		if name == "_id" {
			continue
		}
		typ := "any"
		if field, ok := e.Value().DocumentOK(); ok {
			if v, err := field.LookupErr("bsonType"); err == nil {
				if s, ok := v.StringValueOK(); ok {
					typ = s
				} else if arr, ok := v.ArrayOK(); ok {
					// A union type is spelled as an array; the first entry is
					// the one to generate for, and a "null" member means the
					// field is nullable however `required` reads.
					if values, err := arr.Values(); err == nil && len(values) > 0 {
						typ, _ = values[0].StringValueOK()
					}
				}
			}
		}
		out = append(out, &model.Column{
			Name:     name,
			Type:     typ,
			Native:   typ,
			Nullable: !required[name],
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// sample infers a collection's fields from the documents already in it.
//
// Everything this returns is inference, not schema. A field is reported because
// some sampled document had it; its type is whatever BSON type that value had;
// and Nullable is set from how many of the sampled documents carried the field
// at all, which is the closest thing to optionality the data can say. A field
// that only exists in documents the sample missed is invisible here, and an
// empty collection produces no columns at all — correctly, since there is
// nothing anywhere that says what one of its documents should contain.
func (d *Driver) sample(ctx context.Context, name string) ([]*model.Column, error) {
	cursor, err := d.db.Collection(name).Aggregate(ctx, []bson.D{
		{{Key: "$sample", Value: bson.D{{Key: "size", Value: sampleSize}}}},
	})
	if err != nil {
		return nil, fmt.Errorf("sample %s: %w", name, err)
	}
	defer cursor.Close(ctx)

	types := map[string]string{}
	seen := map[string]int{}
	docs := 0
	for cursor.Next(ctx) {
		docs++
		elements, err := cursor.Current.Elements()
		if err != nil {
			continue
		}
		for _, e := range elements {
			key := e.Key()
			if key == "_id" {
				continue
			}
			seen[key]++
			v := e.Value()
			if v.Type == bson.TypeNull {
				continue
			}
			// The first non-null value decides the type. A field whose values
			// disagree across documents is a real thing in MongoDB and there is
			// no honest single answer for it; taking the first seen at least
			// makes the result stable for the common case where they agree.
			if _, ok := types[key]; !ok {
				types[key] = bsonTypeName(v.Type)
			}
		}
	}
	if err := cursor.Err(); err != nil {
		return nil, fmt.Errorf("sample %s: %w", name, err)
	}

	out := make([]*model.Column, 0, len(seen))
	for key, count := range seen {
		typ := types[key]
		if typ == "" {
			typ = "null"
		}
		out = append(out, &model.Column{
			Name:   key,
			Type:   typ,
			Native: typ,
			// Present in every document sampled is the strongest statement the
			// data can make, and it is still weaker than a validator's
			// `required`. Anything less is reported nullable.
			Nullable: count < docs,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// bsonTypeName is the type name as a $jsonSchema would spell it, so a sampled
// collection and a validated one describe their fields in the same vocabulary.
func bsonTypeName(t bson.Type) string {
	switch t {
	case bson.TypeString:
		return "string"
	case bson.TypeInt32:
		return "int"
	case bson.TypeInt64:
		return "long"
	case bson.TypeDouble:
		return "double"
	case bson.TypeDecimal128:
		return "decimal"
	case bson.TypeBoolean:
		return "bool"
	case bson.TypeDateTime:
		return "date"
	case bson.TypeTimestamp:
		return "timestamp"
	case bson.TypeObjectID:
		return "objectId"
	case bson.TypeEmbeddedDocument:
		return "object"
	case bson.TypeArray:
		return "array"
	case bson.TypeBinary:
		return "binData"
	case bson.TypeNull:
		return "null"
	default:
		return t.String()
	}
}

// Begin implements db.Driver by starting a real transaction where the deployment
// supports one.
//
// MongoDB has multi-document transactions, but only on a replica set or a
// sharded cluster: they are built on the oplog, and a standalone mongod has no
// oplog to write to. Refusing to seed a standalone would be the wrong trade —
// most local development runs one — so the fallback is to write without a
// transaction, and Rollback reports which of the two this run got.
func (d *Driver) Begin(ctx context.Context) (db.Tx, error) {
	sess, err := d.client.StartSession()
	if err != nil {
		return &Tx{db: d.db}, nil
	}
	if err := sess.StartTransaction(); err != nil {
		sess.EndSession(ctx)
		return &Tx{db: d.db}, nil
	}
	return &Tx{db: d.db, sess: sess}, nil
}

// Tx is a MongoDB seeding transaction, real when sess is set.
type Tx struct {
	db   *mongo.Database
	sess *mongo.Session
	done bool
	// written counts documents already inserted outside a transaction, which is
	// what Rollback reports it cannot remove.
	written int64
}

// ctx binds an operation to the session, which is what puts it inside the
// transaction. Without this the write goes out on its own session and commits
// immediately, whatever the transaction does afterwards.
func (t *Tx) ctx(ctx context.Context) context.Context {
	if t.sess == nil {
		return ctx
	}
	return mongo.NewSessionContext(ctx, t.sess)
}

// insertBatch is how many documents go in one InsertMany.
//
// The wire protocol caps a single command at 48 MB and 100,000 documents, and
// the driver splits a larger InsertMany itself. A thousand is well inside both
// and keeps the buffer of pending documents small enough not to matter next to
// the rows themselves.
const insertBatch = 1000

// Insert implements db.Tx with InsertMany, batched. That is the bulk path: one
// command carries the whole batch, and the alternative is a round trip per
// document.
func (t *Tx) Insert(ctx context.Context, tb *model.Table, cols []string, rows db.Source) (int64, error) {
	if len(cols) == 0 {
		return 0, nil
	}
	coll := t.db.Collection(tb.Name)
	sctx := t.ctx(ctx)

	batch := make([]any, 0, insertBatch)
	var written int64
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		res, err := coll.InsertMany(sctx, batch)
		if res != nil {
			written += int64(len(res.InsertedIDs))
			if t.sess == nil {
				t.written += int64(len(res.InsertedIDs))
			}
		}
		if err != nil {
			return fmt.Errorf("insert into %s: %w", tb.Name, err)
		}
		batch = batch[:0]
		return nil
	}

	var loopErr error
	for row := range rows.Rows() {
		doc := make(bson.D, 0, len(cols))
		for _, c := range cols {
			// A column the plan skipped is left out of the document rather than
			// written as null: an absent field is what "no value" means here,
			// and a null one is a value.
			if v, ok := row[c]; ok && v != nil {
				doc = append(doc, bson.E{Key: c, Value: v})
			}
		}
		batch = append(batch, doc)
		if len(batch) == insertBatch {
			if err := flush(); err != nil {
				loopErr = err
				break
			}
		}
	}
	if loopErr != nil {
		return written, loopErr
	}
	// A generator that failed halfway simply stops yielding, so without this a
	// short write would look like a complete one.
	if err := rows.Err(); err != nil {
		return written, fmt.Errorf("generate rows for %s: %w", tb.Name, err)
	}
	if err := flush(); err != nil {
		return written, err
	}
	return written, nil
}

// Truncate implements db.Tx with DeleteMany rather than Drop.
//
// Drop is faster and wrong twice over: it cannot run inside a transaction, and
// it takes the collection's indexes and its $jsonSchema validator with it — the
// only declared schema this engine has. DeleteMany keeps both.
func (t *Tx) Truncate(ctx context.Context, tb *model.Table) error {
	res, err := t.db.Collection(tb.Name).DeleteMany(t.ctx(ctx), bson.D{})
	if err != nil {
		return fmt.Errorf("truncate %s: %w", tb.Name, err)
	}
	if t.sess == nil && res != nil && res.DeletedCount > 0 {
		t.written += res.DeletedCount
	}
	return nil
}

// ReadKeys implements db.Tx. It reads through the session so it sees the
// documents this same uncommitted run wrote — outside the transaction they
// would not be visible yet.
func (t *Tx) ReadKeys(ctx context.Context, tb *model.Table, col string, limit int) ([]any, error) {
	cursor, err := t.db.Collection(tb.Name).Find(t.ctx(ctx),
		bson.D{{Key: col, Value: bson.D{{Key: "$ne", Value: nil}}}},
		options.Find().SetLimit(int64(limit)).SetProjection(bson.D{{Key: col, Value: 1}}),
	)
	if err != nil {
		return nil, fmt.Errorf("read keys from %s.%s: %w", tb.Name, col, err)
	}
	defer cursor.Close(ctx)

	var out []any
	for cursor.Next(ctx) {
		v, err := cursor.Current.LookupErr(col)
		if err != nil {
			continue
		}
		if value := goValue(v); value != nil {
			out = append(out, value)
		}
	}
	if err := cursor.Err(); err != nil {
		return nil, fmt.Errorf("read keys from %s.%s: %w", tb.Name, col, err)
	}
	return out, nil
}

// goValue unwraps a BSON value into something a child row can carry. A key read
// back as a raw BSON value would be written to the child as that wrapper, which
// matches nothing.
func goValue(v bson.RawValue) any {
	var out any
	if err := v.Unmarshal(&out); err != nil {
		return nil
	}
	return out
}

// Exec implements db.Tx by refusing. There is no DDL: a collection comes into
// existence when something is written to it, and everything the schema editor
// could want to change is a command with a JSON body rather than a statement.
// The Document dialect renders nothing for exactly this reason, so anything
// arriving here is a mistake worth reporting rather than swallowing.
func (t *Tx) Exec(context.Context, string) error {
	return fmt.Errorf("mongodb takes no DDL: a collection is created by writing to it, " +
		"and its validator is set with a command rather than a statement Seedora can render")
}

// Commit implements db.Tx.
func (t *Tx) Commit(ctx context.Context) error {
	if t.done {
		return nil
	}
	t.done = true
	if t.sess == nil {
		return nil
	}
	err := t.sess.CommitTransaction(ctx)
	t.sess.EndSession(ctx)
	return err
}

// Rollback implements db.Tx, and does undo the run when there was a transaction
// to undo it with — which makes MongoDB one of the two engines in this group
// that can keep the promise db.Tx makes.
//
// Without one (a standalone mongod), the documents are already on disk and there
// is nothing to abort. Saying so is the point: returning nil would tell the
// seeder the database was restored when it was not.
func (t *Tx) Rollback(ctx context.Context) error {
	if t.done {
		return nil
	}
	t.done = true
	if t.sess != nil {
		err := t.sess.AbortTransaction(ctx)
		t.sess.EndSession(ctx)
		return err
	}
	if t.written == 0 {
		return nil
	}
	return fmt.Errorf("mongodb cannot roll back: this deployment is a standalone mongod, "+
		"which has no transactions, so the %d writes already made are permanent — "+
		"run against a replica set for a run that can be undone", t.written)
}
