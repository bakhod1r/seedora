// Package elasticsearch implements the Seedora driver for Elasticsearch and the
// engines that answer its REST API.
//
// An index is a table and the mapping is its catalog. That mapping is a real
// declared schema — a name and a type per field, written down before any
// document arrives — which puts this engine, with Cassandra, in the half of the
// non-relational list that needs no inference at all. What it lacks is
// everything relational above the field: no primary key beyond the generated
// `_id`, no foreign keys, no uniqueness. Those are left unset rather than
// guessed at.
//
// There is no transaction, and no equivalent of one. `_bulk` is a batch, not an
// atomic unit: the response reports per-document success and the successful ones
// stay. Rollback below says that instead of returning nil.
package elasticsearch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strings"

	"github.com/elastic/go-elasticsearch/v8"
	"github.com/elastic/go-elasticsearch/v8/esapi"

	"github.com/bakhod1r/seedora/internal/db"
	"github.com/bakhod1r/seedora/internal/ddl"
	"github.com/bakhod1r/seedora/internal/model"
)

func init() {
	db.Register(open, "elasticsearch", "es")
}

// Driver is a connected Elasticsearch cluster.
type Driver struct {
	es *elasticsearch.Client
}

// open connects to the cluster named by the DSN, which is spelled
// `elasticsearch://user:pass@host:9200`. The scheme is rewritten to http or
// https before it reaches the client: `elasticsearch://` is Seedora's way of
// naming the engine, not a protocol the transport knows.
func open(ctx context.Context, dsn string) (db.Driver, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse DSN: %w", err)
	}
	cfg := elasticsearch.Config{}
	// A DSN carrying credentials is almost always talking to a cluster with TLS
	// on, and one without them is almost always a local container without it.
	// `?tls=true` and `?tls=false` override the guess.
	secure := u.User != nil
	q := u.Query()
	if v := q.Get("tls"); v != "" {
		secure = v == "true" || v == "1"
	}
	scheme := "http"
	if secure {
		scheme = "https"
	}
	host := u.Host
	if host == "" {
		host = "127.0.0.1:9200"
	} else if u.Port() == "" {
		host += ":9200"
	}
	cfg.Addresses = []string{scheme + "://" + host}
	if u.User != nil {
		cfg.Username = u.User.Username()
		cfg.Password, _ = u.User.Password()
	}
	if key := q.Get("api_key"); key != "" {
		cfg.APIKey = key
	}

	es, err := elasticsearch.NewClient(cfg)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	d := &Driver{es: es}
	// The client is lazy, so without a round trip here a wrong host would only
	// surface at the first introspection query.
	res, err := es.Info(es.Info.WithContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	if err := closeResponse(res); err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	return d, nil
}

// Name implements db.Driver.
func (d *Driver) Name() string { return "Elasticsearch" }

// Dialect implements db.Driver.
func (d *Driver) Dialect() ddl.Dialect { return ddl.Document }

// Close implements db.Driver. The client holds an HTTP transport with pooled
// connections and nothing that needs unwinding.
func (d *Driver) Close(context.Context) error { return nil }

// History implements db.Driver, and finds nothing. Migration tools write their
// bookkeeping into a SQL table; none of them targets an Elasticsearch index, and
// there is no cluster-side record of past mapping changes to read instead.
func (d *Driver) History(context.Context) ([]model.Migration, error) { return nil, nil }

// Introspect reads the mapping API — the declared schema, not the documents.
//
// Fields are flattened with dotted paths, because that is how a document is
// addressed everywhere else in Elasticsearch and it is the name a bulk write
// would use. An `object` field contributes its leaves and not itself; a field
// with no type at all is an object in disguise and is treated the same way.
func (d *Driver) Introspect(ctx context.Context) (*model.Schema, error) {
	res, err := d.es.Indices.GetMapping(
		d.es.Indices.GetMapping.WithContext(ctx),
		// System indices start with a dot and are the cluster's own business.
		d.es.Indices.GetMapping.WithIndex("*"),
		d.es.Indices.GetMapping.WithExpandWildcards("open"),
	)
	if err != nil {
		return nil, fmt.Errorf("read mappings: %w", err)
	}
	var mappings map[string]struct {
		Mappings struct {
			Properties map[string]json.RawMessage `json:"properties"`
		} `json:"mappings"`
	}
	if err := decode(res, &mappings); err != nil {
		return nil, fmt.Errorf("read mappings: %w", err)
	}

	s := &model.Schema{Enums: map[string]model.Values{}}
	for index, m := range mappings {
		if strings.HasPrefix(index, ".") {
			continue
		}
		t := &model.Table{Name: index}
		t.Columns = fields("", m.Mappings.Properties)
		if err := d.count(ctx, t); err != nil {
			return nil, err
		}
		s.Tables = append(s.Tables, t)
	}
	// The mapping API returns a JSON object, whose order is not the cluster's;
	// sorting keeps two runs against the same cluster showing the same list.
	sort.Slice(s.Tables, func(i, j int) bool { return s.Tables[i].Name < s.Tables[j].Name })
	return s, nil
}

// fields turns a mapping's properties into columns, recursing into objects.
func fields(prefix string, props map[string]json.RawMessage) []*model.Column {
	var out []*model.Column
	names := make([]string, 0, len(props))
	for name := range props {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		var f struct {
			Type       string                     `json:"type"`
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if err := json.Unmarshal(props[name], &f); err != nil {
			continue
		}
		path := name
		if prefix != "" {
			path = prefix + "." + name
		}
		if len(f.Properties) > 0 {
			out = append(out, fields(path, f.Properties)...)
			continue
		}
		if f.Type == "" {
			continue
		}
		out = append(out, &model.Column{
			Name:   path,
			Type:   f.Type,
			Native: f.Type,
			// Every field in Elasticsearch is optional: a document missing one
			// is indexed happily, and the field simply has no value. There is
			// no NOT NULL to read and none to report.
			Nullable: true,
		})
	}
	return out
}

// count fills in the row count, which the _count endpoint answers exactly and
// cheaply — unlike a relational engine, this is a maintained figure rather than
// a scan.
func (d *Driver) count(ctx context.Context, t *model.Table) error {
	res, err := d.es.Count(
		d.es.Count.WithContext(ctx),
		d.es.Count.WithIndex(t.Name),
	)
	if err != nil {
		return fmt.Errorf("count %s: %w", t.Name, err)
	}
	var body struct {
		Count int64 `json:"count"`
	}
	if err := decode(res, &body); err != nil {
		return fmt.Errorf("count %s: %w", t.Name, err)
	}
	t.ExistingRows = body.Count
	return nil
}

// Begin implements db.Driver. Elasticsearch has no transaction to open, so this
// returns a handle that writes as it goes; see Rollback.
func (d *Driver) Begin(context.Context) (db.Tx, error) {
	return &Tx{es: d.es}, nil
}

// Tx is an Elasticsearch seeding run, which is not a transaction.
type Tx struct {
	es   *elasticsearch.Client
	done bool
	// written counts the documents already indexed, which is what Rollback has
	// to report it cannot remove.
	written int64
}

// bulkDocs is how many documents go in one _bulk request.
//
// The endpoint takes as many as the request body allows, and the useful ceiling
// is bytes rather than documents: the default http.max_content_length is 100 MB
// and the guidance for a well-sized bulk request is a few megabytes. A thousand
// generated documents sit comfortably inside that for any realistic row, and
// bounding on count keeps the buffer predictable.
const bulkDocs = 1000

// Insert implements db.Tx using _bulk, which is the only bulk path the engine
// has and the only one worth using: one request indexes the whole batch, and the
// alternative is an HTTP round trip per document.
//
// Documents are written without an `_id`, so the cluster generates one. Seeding
// wants new documents, and supplying an id would silently overwrite whatever
// already held it.
func (t *Tx) Insert(ctx context.Context, tb *model.Table, cols []string, rows db.Source) (int64, error) {
	if len(cols) == 0 {
		return 0, nil
	}
	var (
		buf     bytes.Buffer
		pending int
		written int64
	)
	action := []byte(`{"index":{}}` + "\n")

	flush := func() error {
		if pending == 0 {
			return nil
		}
		res, err := t.es.Bulk(bytes.NewReader(buf.Bytes()),
			t.es.Bulk.WithContext(ctx),
			t.es.Bulk.WithIndex(tb.Name),
		)
		if err != nil {
			return fmt.Errorf("bulk index into %s: %w", tb.Name, err)
		}
		var body struct {
			Errors bool `json:"errors"`
			Items  []map[string]struct {
				Status int             `json:"status"`
				Error  json.RawMessage `json:"error"`
			} `json:"items"`
		}
		if err := decode(res, &body); err != nil {
			return fmt.Errorf("bulk index into %s: %w", tb.Name, err)
		}
		// A 200 from _bulk does not mean the documents were indexed: failures
		// are reported per item, and the ones that succeeded are already
		// searchable. Reporting the first failure is what makes a mapping
		// conflict visible instead of turning into a silently short load.
		ok := int64(0)
		for _, item := range body.Items {
			for _, r := range item {
				if r.Error != nil {
					if body.Errors {
						written += ok
						t.written += ok
						return fmt.Errorf("bulk index into %s: %s", tb.Name, r.Error)
					}
					continue
				}
				ok++
			}
		}
		written += ok
		t.written += ok
		buf.Reset()
		pending = 0
		return nil
	}

	var loopErr error
	for row := range rows.Rows() {
		doc := make(map[string]any, len(cols))
		for _, c := range cols {
			// A column the plan skipped is left out of the document entirely
			// rather than written as null, which is what "absent" means here.
			if v, ok := row[c]; ok && v != nil {
				doc[c] = v
			}
		}
		encoded, err := json.Marshal(doc)
		if err != nil {
			loopErr = fmt.Errorf("encode row for %s: %w", tb.Name, err)
			break
		}
		buf.Write(action)
		buf.Write(encoded)
		buf.WriteByte('\n')
		pending++
		if pending == bulkDocs {
			if err := flush(); err != nil {
				loopErr = err
				break
			}
		}
	}
	if loopErr != nil {
		return written, loopErr
	}
	if err := rows.Err(); err != nil {
		return written, fmt.Errorf("generate rows for %s: %w", tb.Name, err)
	}
	if err := flush(); err != nil {
		return written, err
	}
	return written, nil
}

// Truncate implements db.Tx with delete_by_query, because an index cannot be
// emptied any other way without dropping it — and dropping it would take the
// mapping with it, which is the only schema this engine has.
//
// The refresh is not optional: without it the deletes are not visible to the
// search a subsequent read would run, and the seeder would see rows it had just
// removed.
func (t *Tx) Truncate(ctx context.Context, tb *model.Table) error {
	res, err := t.es.DeleteByQuery([]string{tb.Name},
		strings.NewReader(`{"query":{"match_all":{}}}`),
		t.es.DeleteByQuery.WithContext(ctx),
		t.es.DeleteByQuery.WithRefresh(true),
		t.es.DeleteByQuery.WithConflicts("proceed"),
	)
	if err != nil {
		return fmt.Errorf("truncate %s: %w", tb.Name, err)
	}
	if err := closeResponse(res); err != nil {
		return fmt.Errorf("truncate %s: %w", tb.Name, err)
	}
	t.written++
	return nil
}

// ReadKeys implements db.Tx. It refreshes the index first: documents written by
// _bulk are not searchable until the next refresh, which by default is a second
// away, and a child table needing parent keys cannot wait for it.
func (t *Tx) ReadKeys(ctx context.Context, tb *model.Table, col string, limit int) ([]any, error) {
	res, err := t.es.Indices.Refresh(
		t.es.Indices.Refresh.WithContext(ctx),
		t.es.Indices.Refresh.WithIndex(tb.Name),
	)
	if err != nil {
		return nil, fmt.Errorf("refresh %s: %w", tb.Name, err)
	}
	if err := closeResponse(res); err != nil {
		return nil, fmt.Errorf("refresh %s: %w", tb.Name, err)
	}

	query := fmt.Sprintf(`{"size":%d,"_source":[%q],"query":{"exists":{"field":%q}}}`, limit, col, col)
	res, err = t.es.Search(
		t.es.Search.WithContext(ctx),
		t.es.Search.WithIndex(tb.Name),
		t.es.Search.WithBody(strings.NewReader(query)),
	)
	if err != nil {
		return nil, fmt.Errorf("read keys from %s.%s: %w", tb.Name, col, err)
	}
	var body struct {
		Hits struct {
			Hits []struct {
				Source map[string]any `json:"_source"`
			} `json:"hits"`
		} `json:"hits"`
	}
	if err := decode(res, &body); err != nil {
		return nil, fmt.Errorf("read keys from %s.%s: %w", tb.Name, col, err)
	}
	var out []any
	for _, h := range body.Hits.Hits {
		if v := lookup(h.Source, col); v != nil {
			out = append(out, v)
		}
	}
	return out, nil
}

// lookup resolves a dotted field path against a returned _source document, since
// the source comes back nested however the field is addressed.
func lookup(doc map[string]any, path string) any {
	if v, ok := doc[path]; ok {
		return v
	}
	cur := any(doc)
	for _, part := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur, ok = m[part]
		if !ok {
			return nil
		}
	}
	return cur
}

// Exec implements db.Tx by refusing. The schema editor renders nothing for the
// Document dialect precisely because there is no statement Elasticsearch would
// take; an index is created by a JSON mapping request, not by DDL, and quietly
// swallowing the call would hide that from whoever asked for it.
func (t *Tx) Exec(context.Context, string) error {
	return fmt.Errorf("elasticsearch takes no DDL: an index and its mapping are created " +
		"through the mapping API, not through a statement Seedora can render")
}

// Commit implements db.Tx. Every document was indexed as it was written; this
// only marks the run finished.
func (t *Tx) Commit(context.Context) error {
	t.done = true
	return nil
}

// Rollback implements db.Tx by reporting that it cannot undo anything.
//
// _bulk is a batch and not an atomic unit: each document is indexed on its own,
// the successful ones are already on disk and searchable, and there is no
// cluster-side operation that puts an index back as it was. Returning nil would
// tell the seeder the database was restored when nothing of the sort happened.
func (t *Tx) Rollback(context.Context) error {
	if t.done || t.written == 0 {
		t.done = true
		return nil
	}
	t.done = true
	return fmt.Errorf("elasticsearch cannot roll back: %d writes have already been applied "+
		"and are permanent — _bulk is a batch, not a transaction, so undoing them means "+
		"deleting the documents yourself", t.written)
}

// decode reads a JSON response body and reports an HTTP-level failure as an
// error, which the client does not do on its own: it returns a *Response whose
// IsError says so and whose body carries the reason.
func decode(res *esapi.Response, into any) error {
	defer res.Body.Close()
	if res.IsError() {
		body, _ := io.ReadAll(res.Body)
		return fmt.Errorf("%s: %s", res.Status(), strings.TrimSpace(string(body)))
	}
	return json.NewDecoder(res.Body).Decode(into)
}

// closeResponse drains and closes a response whose body is not needed, so the
// connection returns to the pool rather than being abandoned mid-body.
func closeResponse(res *esapi.Response) error {
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.IsError() {
		return fmt.Errorf("%s: %s", res.Status(), strings.TrimSpace(string(body)))
	}
	return nil
}
