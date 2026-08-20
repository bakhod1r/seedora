// Package dynamodb implements the Seedora driver for Amazon DynamoDB and the
// local emulator that answers the same API.
//
// DynamoDB declares exactly one thing about a table's shape: its key schema. The
// partition key, the optional sort key, and the type of each — S, N, or B — are
// real, enforced, and readable from DescribeTable. Every other attribute is
// undeclared. An item may carry any attributes at all, and nothing anywhere says
// what they should be.
//
// So Introspect has two halves and they are not equally trustworthy. The key
// columns come from the key schema and are as solid as a relational primary key.
// Everything else comes from scanning a page of stored items and reporting what
// they happened to contain — inference from data, not a schema. A table with no
// items yields its key attributes and nothing more, which is correct: there is
// no other source that could say otherwise.
//
// There is no transaction covering a run. TransactWriteItems exists and is
// atomic, but it caps at 100 items and 4 MB, which is not a seeding run — so the
// bulk path is BatchWriteItem, which is not atomic, and Rollback says so.
package dynamodb

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/bakhod1r/seedora/internal/db"
	"github.com/bakhod1r/seedora/internal/ddl"
	"github.com/bakhod1r/seedora/internal/model"
)

func init() {
	db.Register(open, "dynamodb")
}

// Driver is a connected DynamoDB endpoint.
type Driver struct {
	api *dynamodb.Client
}

// open builds a client from the DSN.
//
// There is no connection to make — the API is HTTPS — so this only resolves
// where to send requests and who to send them as. `dynamodb://` on its own uses
// the ambient AWS configuration, which is what a real account wants;
// `dynamodb://localhost:8000/?region=us-east-1` points at the local emulator,
// which needs an endpoint and accepts any credentials at all.
func open(ctx context.Context, dsn string) (db.Driver, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse DSN: %w", err)
	}
	q := u.Query()
	var loadOpts []func(*config.LoadOptions) error
	if region := q.Get("region"); region != "" {
		loadOpts = append(loadOpts, config.WithRegion(region))
	}
	if u.User != nil {
		secret, _ := u.User.Password()
		// Keys in a DSN are what the emulator takes and what a scratch account
		// sometimes gets pasted in as. A DSN without them falls through to the
		// environment, the profile, and the instance role, in that order.
		loadOpts = append(loadOpts, config.WithCredentialsProvider(
			aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
				return aws.Credentials{
					AccessKeyID: u.User.Username(), SecretAccessKey: secret,
					Source: "seedora DSN",
				}, nil
			})))
	}
	cfg, err := config.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("load AWS configuration: %w", err)
	}

	var clientOpts []func(*dynamodb.Options)
	if u.Host != "" {
		scheme := "http"
		if q.Get("tls") == "true" {
			scheme = "https"
		}
		endpoint := scheme + "://" + u.Host
		clientOpts = append(clientOpts, func(o *dynamodb.Options) { o.BaseEndpoint = aws.String(endpoint) })
		// The emulator does not check the signature but the signer still needs a
		// region and a key to build one from.
		if cfg.Region == "" {
			cfg.Region = "us-east-1"
		}
		if u.User == nil {
			clientOpts = append(clientOpts, func(o *dynamodb.Options) {
				o.Credentials = aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
					return aws.Credentials{AccessKeyID: "local", SecretAccessKey: "local", Source: "seedora local"}, nil
				})
			})
		}
	}
	if cfg.Region == "" {
		return nil, fmt.Errorf("no AWS region — set AWS_REGION or add ?region=us-east-1 to the DSN")
	}
	return &Driver{api: dynamodb.NewFromConfig(cfg, clientOpts...)}, nil
}

// Name implements db.Driver.
func (d *Driver) Name() string { return "DynamoDB" }

// Dialect implements db.Driver.
func (d *Driver) Dialect() ddl.Dialect { return ddl.Document }

// Close implements db.Driver. An HTTP client has nothing to close.
func (d *Driver) Close(context.Context) error { return nil }

// History implements db.Driver, and finds nothing: no migration tool writes its
// bookkeeping into a DynamoDB table, and the service keeps no record of past
// table changes that a caller could read.
func (d *Driver) History(context.Context) ([]model.Migration, error) { return nil, nil }

// sampleItems is how many stored items are read to infer the non-key attributes.
// One Scan page is capped at 1 MB whatever the limit says, so this is an upper
// bound rather than a promise, and it is deliberately one request per table.
const sampleItems = 100

// Introspect reads the key schema, then samples items for everything else. See
// the package comment for why those two are kept apart.
func (d *Driver) Introspect(ctx context.Context) (*model.Schema, error) {
	var names []string
	var start *string
	for {
		out, err := d.api.ListTables(ctx, &dynamodb.ListTablesInput{ExclusiveStartTableName: start})
		if err != nil {
			return nil, fmt.Errorf("list tables: %w", err)
		}
		names = append(names, out.TableNames...)
		if out.LastEvaluatedTableName == nil {
			break
		}
		start = out.LastEvaluatedTableName
	}
	sort.Strings(names)

	s := &model.Schema{Enums: map[string]model.Values{}}
	for _, name := range names {
		t, err := d.describe(ctx, name)
		if err != nil {
			return nil, err
		}
		if err := d.sample(ctx, t); err != nil {
			return nil, err
		}
		s.Tables = append(s.Tables, t)
	}
	return s, nil
}

// describe reads the declared half: the key schema and the types of the key
// attributes. AttributeDefinitions only ever lists attributes that are part of a
// key or an index, which is precisely the set DynamoDB has an opinion about.
func (d *Driver) describe(ctx context.Context, name string) (*model.Table, error) {
	out, err := d.api.DescribeTable(ctx, &dynamodb.DescribeTableInput{TableName: aws.String(name)})
	if err != nil {
		return nil, fmt.Errorf("describe %s: %w", name, err)
	}
	desc := out.Table

	declared := map[string]string{}
	for _, a := range desc.AttributeDefinitions {
		declared[aws.ToString(a.AttributeName)] = attributeType(a.AttributeType)
	}

	t := &model.Table{Name: name}
	// HASH comes before RANGE in the key however the API happens to list them:
	// that is the order the key is spelled in, and the partition key is the half
	// that decides where an item lives.
	var hash, sortKey string
	for _, k := range desc.KeySchema {
		if k.KeyType == types.KeyTypeHash {
			hash = aws.ToString(k.AttributeName)
		} else {
			sortKey = aws.ToString(k.AttributeName)
		}
	}
	for _, name := range []string{hash, sortKey} {
		if name == "" {
			continue
		}
		typ := declared[name]
		t.PrimaryKey = append(t.PrimaryKey, name)
		t.Columns = append(t.Columns, &model.Column{
			Name: name, Type: typ, Native: typ,
			// A key attribute is required on every item, which is the one NOT
			// NULL DynamoDB actually enforces.
			Nullable: false,
			// A partition key on its own identifies an item; a partition key
			// paired with a sort key does not, and neither half of the pair is
			// unique on its own.
			Unique: sortKey == "",
		})
	}
	// ItemCount is maintained by the service and refreshed about every six
	// hours, so it is an estimate that can be badly stale on a table written to
	// recently. A Scan would be exact and would read the whole table to get
	// there, which is not worth it for a truncate confirmation.
	t.ExistingRows = aws.ToInt64(desc.ItemCount)
	return t, nil
}

// sample fills in the undeclared attributes by reading a page of stored items.
//
// Everything added here is inference from data. An attribute appears because
// some item carried it; its type is the type that item's value had; and
// Nullable is true for all of them, because DynamoDB requires nothing beyond the
// key and an item omitting an attribute is perfectly valid. A table with no
// items adds nothing, which is the honest result rather than an empty guess.
func (d *Driver) sample(ctx context.Context, t *model.Table) error {
	out, err := d.api.Scan(ctx, &dynamodb.ScanInput{
		TableName: aws.String(t.Name),
		Limit:     aws.Int32(sampleItems),
	})
	if err != nil {
		return fmt.Errorf("sample %s: %w", t.Name, err)
	}
	kinds := map[string]string{}
	var order []string
	for _, item := range out.Items {
		for name, v := range item {
			if t.Column(name) != nil {
				continue
			}
			if _, ok := kinds[name]; ok {
				continue
			}
			// The first value seen decides the type. Items in one table may
			// disagree — that is legal here — and there is no single right
			// answer when they do.
			kinds[name] = valueType(v)
			order = append(order, name)
		}
	}
	sort.Strings(order)
	for _, name := range order {
		t.Columns = append(t.Columns, &model.Column{
			Name: name, Type: kinds[name], Native: kinds[name], Nullable: true,
		})
	}
	return nil
}

func attributeType(t types.ScalarAttributeType) string {
	switch t {
	case types.ScalarAttributeTypeS:
		return "string"
	case types.ScalarAttributeTypeN:
		return "number"
	case types.ScalarAttributeTypeB:
		return "binary"
	default:
		return string(t)
	}
}

// valueType names the shape of a stored attribute in the same vocabulary the key
// types use, so a sampled column and a declared one read alike.
func valueType(v types.AttributeValue) string {
	switch v.(type) {
	case *types.AttributeValueMemberS:
		return "string"
	case *types.AttributeValueMemberN:
		return "number"
	case *types.AttributeValueMemberB:
		return "binary"
	case *types.AttributeValueMemberBOOL:
		return "bool"
	case *types.AttributeValueMemberM:
		return "map"
	case *types.AttributeValueMemberL:
		return "list"
	case *types.AttributeValueMemberSS, *types.AttributeValueMemberNS, *types.AttributeValueMemberBS:
		return "set"
	case *types.AttributeValueMemberNULL:
		return "null"
	default:
		return "unknown"
	}
}

// Begin implements db.Driver. There is no transaction to open; see Rollback.
func (d *Driver) Begin(context.Context) (db.Tx, error) {
	return &Tx{api: d.api}, nil
}

// Tx is a DynamoDB seeding run, which is not a transaction.
type Tx struct {
	api  *dynamodb.Client
	done bool
	// written counts the items already put, which is what Rollback reports it
	// cannot remove.
	written int64
	// keys caches each table's key attributes, which a delete needs and a Scan
	// would otherwise be asked for twice.
	keys map[string][]string
}

// batchItems is the service's own limit on BatchWriteItem: 25 requests, and a
// request with 26 is rejected outright rather than truncated.
const batchItems = 25

// Insert implements db.Tx with BatchWriteItem, which is the bulk path: 25 items
// per HTTP round trip instead of one.
//
// Unprocessed items are the part that cannot be skipped. A batch is throttled
// per item rather than as a whole, so a partially-applied batch comes back as a
// 200 with the leftovers in UnprocessedItems, and a driver that ignored them
// would silently drop rows. They are retried with a growing delay, which is what
// the service asks callers to do.
func (t *Tx) Insert(ctx context.Context, tb *model.Table, cols []string, rows db.Source) (int64, error) {
	if len(cols) == 0 {
		return 0, nil
	}
	var written int64
	batch := make([]types.WriteRequest, 0, batchItems)

	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		n, err := t.write(ctx, tb.Name, batch)
		written += n
		t.written += n
		batch = batch[:0]
		if err != nil {
			return fmt.Errorf("insert into %s: %w", tb.Name, err)
		}
		return nil
	}

	var loopErr error
	for row := range rows.Rows() {
		item := make(map[string]types.AttributeValue, len(cols))
		for _, c := range cols {
			v, ok := row[c]
			if !ok || v == nil {
				// An absent attribute is how "no value" is spelled here. Writing
				// an explicit NULL would store an attribute whose value is the
				// null type, which is a different thing and one nobody wants.
				continue
			}
			av, err := attributeValue(v)
			if err != nil {
				loopErr = fmt.Errorf("encode %s.%s: %w", tb.Name, c, err)
				break
			}
			item[c] = av
		}
		if loopErr != nil {
			break
		}
		batch = append(batch, types.WriteRequest{PutRequest: &types.PutRequest{Item: item}})
		if len(batch) == batchItems {
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

// write sends one batch and retries whatever the service did not process.
func (t *Tx) write(ctx context.Context, table string, batch []types.WriteRequest) (int64, error) {
	pending := batch
	var done int64
	for attempt := 0; len(pending) > 0; attempt++ {
		out, err := t.api.BatchWriteItem(ctx, &dynamodb.BatchWriteItemInput{
			RequestItems: map[string][]types.WriteRequest{table: pending},
		})
		if err != nil {
			return done, err
		}
		left := out.UnprocessedItems[table]
		done += int64(len(pending) - len(left))
		if len(left) == 0 {
			return done, nil
		}
		if attempt >= 6 {
			return done, fmt.Errorf("%d items still unprocessed after %d attempts — the table is throttling",
				len(left), attempt+1)
		}
		pending = left
		// Backing off is the documented response to unprocessed items: retrying
		// immediately is what turns a throttled table into a failed run.
		select {
		case <-ctx.Done():
			return done, ctx.Err()
		case <-time.After(time.Duration(1<<attempt) * 50 * time.Millisecond):
		}
	}
	return done, nil
}

// attributeValue converts a generated value to the wire form. This is written
// out by hand rather than reached for from the attributevalue helper package,
// which is not a dependency of this project.
func attributeValue(v any) (types.AttributeValue, error) {
	switch x := v.(type) {
	case string:
		if x == "" {
			// An empty string is legal in a non-key attribute and rejected in a
			// key one; storing it as a null keeps a generated blank from failing
			// the whole batch.
			return &types.AttributeValueMemberNULL{Value: true}, nil
		}
		return &types.AttributeValueMemberS{Value: x}, nil
	case bool:
		return &types.AttributeValueMemberBOOL{Value: x}, nil
	case []byte:
		return &types.AttributeValueMemberB{Value: x}, nil
	case int:
		return number(strconv.FormatInt(int64(x), 10)), nil
	case int32:
		return number(strconv.FormatInt(int64(x), 10)), nil
	case int64:
		return number(strconv.FormatInt(x, 10)), nil
	case uint64:
		return number(strconv.FormatUint(x, 10)), nil
	case float32:
		return number(strconv.FormatFloat(float64(x), 'f', -1, 32)), nil
	case float64:
		return number(strconv.FormatFloat(x, 'f', -1, 64)), nil
	case time.Time:
		// There is no date type: the convention is an ISO-8601 string, which
		// sorts correctly as a sort key and is what every SDK example uses.
		return &types.AttributeValueMemberS{Value: x.Format(time.RFC3339Nano)}, nil
	case map[string]any:
		m := make(map[string]types.AttributeValue, len(x))
		for k, val := range x {
			av, err := attributeValue(val)
			if err != nil {
				return nil, err
			}
			m[k] = av
		}
		return &types.AttributeValueMemberM{Value: m}, nil
	case []any:
		l := make([]types.AttributeValue, 0, len(x))
		for _, val := range x {
			av, err := attributeValue(val)
			if err != nil {
				return nil, err
			}
			l = append(l, av)
		}
		return &types.AttributeValueMemberL{Value: l}, nil
	case nil:
		return &types.AttributeValueMemberNULL{Value: true}, nil
	default:
		return &types.AttributeValueMemberS{Value: fmt.Sprint(x)}, nil
	}
}

func number(s string) types.AttributeValue { return &types.AttributeValueMemberN{Value: s} }

// Truncate implements db.Tx by scanning the keys and deleting them in batches.
//
// There is no TRUNCATE and no DELETE ... WHERE: the only ways to empty a table
// are to delete every item by key or to drop the table and recreate it. Dropping
// it is faster and loses the throughput settings, the indexes, and the stream
// configuration, and takes long enough to come back that the run would have to
// poll for it. Deleting by key is slower and leaves the table exactly as it was.
func (t *Tx) Truncate(ctx context.Context, tb *model.Table) error {
	keys, err := t.keyAttributes(ctx, tb)
	if err != nil {
		return err
	}
	projection := make([]string, len(keys))
	names := map[string]string{}
	for i, k := range keys {
		// Key attributes are projected through placeholders because an attribute
		// named like a reserved word — `name`, `status`, `timestamp` — is
		// rejected in a bare projection expression.
		alias := "#k" + strconv.Itoa(i)
		projection[i] = alias
		names[alias] = k
	}

	var start map[string]types.AttributeValue
	for {
		out, err := t.api.Scan(ctx, &dynamodb.ScanInput{
			TableName:                aws.String(tb.Name),
			ProjectionExpression:     aws.String(strings.Join(projection, ", ")),
			ExpressionAttributeNames: names,
			ExclusiveStartKey:        start,
		})
		if err != nil {
			return fmt.Errorf("truncate %s: %w", tb.Name, err)
		}
		batch := make([]types.WriteRequest, 0, batchItems)
		for _, item := range out.Items {
			batch = append(batch, types.WriteRequest{DeleteRequest: &types.DeleteRequest{Key: item}})
			if len(batch) == batchItems {
				n, err := t.write(ctx, tb.Name, batch)
				t.written += n
				if err != nil {
					return fmt.Errorf("truncate %s: %w", tb.Name, err)
				}
				batch = batch[:0]
			}
		}
		if len(batch) > 0 {
			n, err := t.write(ctx, tb.Name, batch)
			t.written += n
			if err != nil {
				return fmt.Errorf("truncate %s: %w", tb.Name, err)
			}
		}
		if len(out.LastEvaluatedKey) == 0 {
			return nil
		}
		start = out.LastEvaluatedKey
	}
}

// keyAttributes returns a table's key attribute names, from the introspected
// table if it carries them and from DescribeTable if it does not.
func (t *Tx) keyAttributes(ctx context.Context, tb *model.Table) ([]string, error) {
	if len(tb.PrimaryKey) > 0 {
		return tb.PrimaryKey, nil
	}
	if keys, ok := t.keys[tb.Name]; ok {
		return keys, nil
	}
	out, err := t.api.DescribeTable(ctx, &dynamodb.DescribeTableInput{TableName: aws.String(tb.Name)})
	if err != nil {
		return nil, fmt.Errorf("describe %s: %w", tb.Name, err)
	}
	var keys []string
	for _, k := range out.Table.KeySchema {
		keys = append(keys, aws.ToString(k.AttributeName))
	}
	if t.keys == nil {
		t.keys = map[string][]string{}
	}
	t.keys[tb.Name] = keys
	return keys, nil
}

// ReadKeys implements db.Tx with a Scan, which is the only read that does not
// already know the key it wants. It is a full-table read by definition, so it
// stops at the first page that satisfies the limit.
func (t *Tx) ReadKeys(ctx context.Context, tb *model.Table, col string, limit int) ([]any, error) {
	out, err := t.api.Scan(ctx, &dynamodb.ScanInput{
		TableName:                aws.String(tb.Name),
		ProjectionExpression:     aws.String("#c"),
		ExpressionAttributeNames: map[string]string{"#c": col},
		Limit:                    aws.Int32(int32(limit)),
	})
	if err != nil {
		return nil, fmt.Errorf("read keys from %s.%s: %w", tb.Name, col, err)
	}
	var keys []any
	for _, item := range out.Items {
		if v, ok := item[col]; ok {
			if value := goValue(v); value != nil {
				keys = append(keys, value)
			}
		}
	}
	return keys, nil
}

// goValue unwraps the scalar forms a key can take. A composite value cannot be a
// key attribute, so anything else is not a key worth handing to a child row.
func goValue(v types.AttributeValue) any {
	switch x := v.(type) {
	case *types.AttributeValueMemberS:
		return x.Value
	case *types.AttributeValueMemberN:
		if n, err := strconv.ParseInt(x.Value, 10, 64); err == nil {
			return n
		}
		f, err := strconv.ParseFloat(x.Value, 64)
		if err != nil {
			return nil
		}
		return f
	case *types.AttributeValueMemberB:
		return x.Value
	case *types.AttributeValueMemberBOOL:
		return x.Value
	default:
		return nil
	}
}

// Exec implements db.Tx by refusing. Tables are created with CreateTable, which
// is an API call with a JSON body and not a statement; the Document dialect
// renders nothing for that reason, so anything arriving here is a mistake worth
// naming.
func (t *Tx) Exec(context.Context, string) error {
	return fmt.Errorf("dynamodb takes no DDL: a table is created through CreateTable, " +
		"not through a statement Seedora can render")
}

// Commit implements db.Tx. Everything was written as it was made.
func (t *Tx) Commit(context.Context) error {
	t.done = true
	return nil
}

// Atomic implements db.Atomic: this engine commits as it writes, so a rollback
// undoes nothing and --dry-run must not write.
func (t *Tx) Atomic() bool { return false }

// Rollback implements db.Tx by reporting that it cannot undo the writes.
//
// BatchWriteItem is not atomic even within one batch — each of the 25 requests
// succeeds or fails on its own — and there is no operation that puts a table
// back as it was. TransactWriteItems is atomic, but it takes at most 100 items
// in 4 MB, which is a constraint on the shape of one write rather than something
// a seeding run of any size could be built on. Returning nil here would tell the
// seeder the database was restored when the items are still there.
func (t *Tx) Rollback(context.Context) error {
	if t.done || t.written == 0 {
		t.done = true
		return nil
	}
	t.done = true
	return fmt.Errorf("dynamodb cannot roll back: %d writes have already been applied and are "+
		"permanent — BatchWriteItem is not a transaction, so undoing them means deleting the "+
		"items yourself", t.written)
}
