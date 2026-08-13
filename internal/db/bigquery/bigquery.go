// Package bigquery implements the Seedora driver for Google BigQuery.
//
// Three things about BigQuery shape this driver and are worth stating up front.
//
// The first is how rows get in. BigQuery charges and queues per statement, and
// a row-at-a-time INSERT is both the slowest and the most expensive way to put
// data in one: a hundred thousand INSERTs is a hundred thousand query jobs.
// What it is built for is a load — hand it a file of rows and it ingests them
// in one job — so Insert streams newline-delimited JSON straight into a load
// job rather than issuing statements. See Insert for why a load job rather than
// the storage write API.
//
// The second is quoting. BigQuery quotes identifiers with backticks and reads
// a double-quoted word as a string literal, so the double quotes Seedora uses
// everywhere else cannot appear in a statement this driver sends. Every
// identifier here goes through quoteIdent below, including the ones handed to
// the shared migration-history reader.
//
// The third is that Seedora's central promise — a run is one transaction, and a
// failure leaves the database exactly as it was — cannot be honoured here.
// BigQuery has no transaction spanning separate jobs: each load job and each
// DML statement commits on its own as it completes, and there is nothing to
// begin and nothing to undo. This driver does not pretend otherwise. It records
// what it has already made permanent, and Rollback reports that rather than
// returning nil as though it had reversed it.
package bigquery

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/bigquery"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"

	"github.com/bakhod1r/seedora/internal/db"
	"github.com/bakhod1r/seedora/internal/ddl"
	"github.com/bakhod1r/seedora/internal/model"
)

func init() {
	db.Register(open, "bigquery")
}

// Driver is a connected BigQuery dataset.
//
// A dataset rather than a project, because a dataset is what holds tables and
// so is what maps onto everything above the driver. Its name is carried
// separately rather than left on model.Table.Schema alone, since every
// statement needs the project on the front of it too.
type Driver struct {
	client  *bigquery.Client
	project string
	dataset string
}

// open connects to the dataset named by the DSN.
//
// The DSN is `bigquery://project/dataset`, with two optional parameters:
// `credentials` naming a service-account key file, and `location` for the
// region the dataset lives in. Without `credentials` the SDK finds Application
// Default Credentials the usual way, which is what a developer with gcloud
// already set up has.
func open(ctx context.Context, dsn string) (db.Driver, error) {
	project, dataset, opts, location, err := parseDSN(dsn)
	if err != nil {
		return nil, err
	}
	client, err := bigquery.NewClient(ctx, project, opts...)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	d := &Driver{client: client, project: project, dataset: dataset}

	// One call at open, for two reasons. It fails now rather than after a run's
	// worth of generation if the dataset is missing or the credentials cannot
	// see it, and it supplies the region: a job that does not name the same
	// region as the data it touches is rejected, and INFORMATION_SCHEMA is
	// exactly such a job.
	md, err := client.Dataset(dataset).Metadata(ctx)
	if err != nil {
		client.Close()
		return nil, fmt.Errorf("open dataset %s.%s: %w", project, dataset, err)
	}
	if location == "" {
		location = md.Location
	}
	client.Location = location
	return d, nil
}

func parseDSN(dsn string) (project, dataset string, opts []option.ClientOption, location string, err error) {
	u, err := url.Parse(strings.TrimSpace(dsn))
	if err != nil {
		return "", "", nil, "", fmt.Errorf("parse DSN: %w", err)
	}
	project = u.Host
	dataset = strings.Trim(u.Path, "/")
	// `bigquery://project.dataset` is the spelling BigQuery's own tools use for
	// a qualified name, and people paste it.
	if dataset == "" {
		if i := strings.IndexByte(project, '.'); i > 0 {
			project, dataset = project[:i], project[i+1:]
		}
	}
	if project == "" || dataset == "" {
		return "", "", nil, "", fmt.Errorf(
			"the DSN names no project and dataset — write it as bigquery://my-project/my_dataset")
	}
	if strings.ContainsRune(dataset, '/') {
		return "", "", nil, "", fmt.Errorf(
			"a BigQuery DSN names one dataset: bigquery://my-project/my_dataset, not %q", dataset)
	}

	q := u.Query()
	if f := q.Get("credentials"); f != "" {
		if err := checkServiceAccountKey(f); err != nil {
			return "", "", nil, "", err
		}
		opts = append(opts, option.WithAuthCredentialsFile(option.ServiceAccount, f))
	}
	return project, dataset, opts, q.Get("location"), nil
}

// checkServiceAccountKey refuses a credentials file that is anything other than
// a service account key.
//
// The check is here rather than left to the library because neither option does
// it. WithCredentialsFile is deprecated precisely for accepting any credential
// configuration, and WithAuthCredentialsFile — which names a type and looks like
// the fix — does not enforce the name: setting it switches the client onto the
// new auth library, whose detection path drops the type and reads the file
// without checking it.
//
// What that admits is an external_account configuration, which names the host
// it fetches a token from. A DSN is the kind of string that gets pasted out of
// a wiki or a ticket, so a credentials file it points at is exactly the input
// that should not be allowed to choose where a token comes from.
func checkServiceAccountKey(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read the credentials file named by the DSN: %w", err)
	}
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(b, &probe); err != nil {
		return fmt.Errorf("the credentials file named by the DSN is not JSON: %w", err)
	}
	if probe.Type != "service_account" {
		return fmt.Errorf(
			"the credentials file named by the DSN is of type %q; Seedora accepts only "+
				"a service account key here, because the other kinds can name the host "+
				"a token is fetched from. Use application default credentials instead "+
				"(unset the credentials parameter and run gcloud auth application-default login)",
			probe.Type)
	}
	return nil
}

// Name implements db.Driver.
func (d *Driver) Name() string { return "BigQuery" }

// Dialect implements db.Driver.
func (d *Driver) Dialect() ddl.Dialect { return ddl.BigQuery }

// Close implements db.Driver.
func (d *Driver) Close(context.Context) error { return d.client.Close() }

// Begin implements db.Driver.
//
// Nothing is begun. See the package comment: there is no transaction across
// jobs to open, and opening a per-statement one would describe a guarantee the
// run does not have. The returned Tx records what it makes permanent so
// Rollback can say what a failure left behind.
func (d *Driver) Begin(context.Context) (db.Tx, error) {
	return &Tx{d: d}, nil
}

// quoteIdent spells an identifier the way BigQuery wants it. Seedora's shared
// quoting is the SQL-standard double quote, which BigQuery reads as a string
// literal — so nothing in this package may use model.QuoteIdent.
func quoteIdent(s string) string {
	return "`" + strings.ReplaceAll(s, "`", "\\`") + "`"
}

// qualified spells a table as project.dataset.table inside one pair of
// backticks, which is the form BigQuery documents and the only one that works
// for a project id containing a hyphen.
func (d *Driver) qualified(name string) string {
	return quoteIdent(d.project + "." + d.dataset + "." + name)
}

// Introspect reads the catalog in three queries rather than one per table.
func (d *Driver) Introspect(ctx context.Context) (*model.Schema, error) {
	// Enums stays non-nil and empty: BigQuery has no enum type.
	s := &model.Schema{Enums: map[string]model.Values{}}

	byName, err := d.loadColumns(ctx, s)
	if err != nil {
		return nil, err
	}
	if err := d.loadKeys(ctx, byName); err != nil {
		return nil, err
	}
	if err := d.loadCounts(ctx, byName); err != nil {
		return nil, err
	}
	return s, nil
}

// query runs one statement and returns its rows as field→value maps. Every read
// in this package goes through it, so the dataset defaults and the region are
// set in one place.
func (d *Driver) query(ctx context.Context, sql string) ([]map[string]any, error) {
	q := d.client.Query(sql)
	q.DefaultProjectID = d.project
	q.DefaultDatasetID = d.dataset
	it, err := q.Read(ctx)
	if err != nil {
		return nil, err
	}
	var out []map[string]any
	for {
		row := map[string]bigquery.Value{}
		err := it.Next(&row)
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, err
		}
		m := make(map[string]any, len(row))
		for k, v := range row {
			m[k] = any(v)
		}
		out = append(out, m)
	}
	return out, nil
}

// loadColumns reads every base table and its columns.
//
// INFORMATION_SCHEMA.COLUMNS is queried rather than the per-table metadata API:
// the API needs one round trip per table, and a dataset with a few hundred of
// them would spend the whole introspection in HTTP. The one thing the view
// cannot express is a nested RECORD, whose leaves it reports as one column of
// type STRUCT<…>; seeding into a repeated or nested field is not something the
// plan can describe anyway, so it is left as the type it is and the user sees
// what the database says.
func (d *Driver) loadColumns(ctx context.Context, s *model.Schema) (map[string]*model.Table, error) {
	sql := fmt.Sprintf(`
SELECT c.table_name, c.column_name, c.data_type, c.is_nullable,
       c.is_generated, c.is_partitioning_column, c.column_default
FROM %s.INFORMATION_SCHEMA.COLUMNS c
JOIN %s.INFORMATION_SCHEMA.TABLES t USING (table_catalog, table_schema, table_name)
WHERE t.table_type = 'BASE TABLE' AND c.is_hidden = 'NO'
ORDER BY c.table_name, c.ordinal_position`,
		d.datasetRef(), d.datasetRef())

	rows, err := d.query(ctx, sql)
	if err != nil {
		return nil, fmt.Errorf("read columns: %w", err)
	}

	byName := map[string]*model.Table{}
	for _, r := range rows {
		table := str(r["table_name"])
		t := byName[table]
		if t == nil {
			t = &model.Table{Schema: d.dataset, Name: table}
			byName[table] = t
			s.Tables = append(s.Tables, t)
		}
		native := str(r["data_type"])
		c := &model.Column{
			Name:     str(r["column_name"]),
			Type:     strings.ToLower(baseType(native)),
			Native:   native,
			Nullable: strings.EqualFold(str(r["is_nullable"]), "YES"),
			// A default here is a real DEFAULT expression; BigQuery has no
			// identity or serial column, so this is the whole of it.
			HasDefault: str(r["column_default"]) != "" && !strings.EqualFold(str(r["column_default"]), "NULL"),
			// A partitioning pseudo-column (_PARTITIONTIME) is not writable
			// either, and reads the same way to everything above here as a
			// generated one does.
			Generated: strings.EqualFold(str(r["is_generated"]), "YES") ||
				strings.EqualFold(str(r["is_partitioning_column"]), "YES"),
		}
		c.MaxLen, c.Precision, c.Scale = decorations(native)
		t.Columns = append(t.Columns, c)
	}
	return byName, nil
}

// datasetRef is the dataset as an INFORMATION_SCHEMA query must name it: the
// view lives inside the dataset, so the prefix is the dataset, not the table.
func (d *Driver) datasetRef() string {
	return quoteIdent(d.project + "." + d.dataset)
}

// loadKeys marks primary and foreign keys.
//
// BigQuery's keys are unenforced — it records the constraint and never checks
// it — which makes them useless to the database and exactly as useful to
// Seedora as an enforced one: they say which column points at which, and that
// is the whole input to generating a child row that references a real parent.
//
// The query is allowed to fail without failing introspection. These views are
// newer than the rest of INFORMATION_SCHEMA and a role can be granted table
// metadata without them; a dataset that declares no keys is the common case
// anyway, and the user maps the references by hand.
func (d *Driver) loadKeys(ctx context.Context, byName map[string]*model.Table) error {
	sql := fmt.Sprintf(`
SELECT tc.constraint_type, kcu.table_name, kcu.column_name,
       kcu.ordinal_position,
       IFNULL(ccu.table_name, '')  AS ref_table,
       IFNULL(ccu.column_name, '') AS ref_column
FROM %[1]s.INFORMATION_SCHEMA.TABLE_CONSTRAINTS tc
JOIN %[1]s.INFORMATION_SCHEMA.KEY_COLUMN_USAGE kcu
  ON kcu.constraint_name = tc.constraint_name
LEFT JOIN %[1]s.INFORMATION_SCHEMA.CONSTRAINT_COLUMN_USAGE ccu
  ON ccu.constraint_name = tc.constraint_name
 AND ccu.ordinal_position = kcu.ordinal_position
WHERE tc.constraint_type IN ('PRIMARY KEY', 'FOREIGN KEY')
ORDER BY kcu.table_name, kcu.constraint_name, kcu.ordinal_position`, d.datasetRef())

	rows, err := d.query(ctx, sql)
	if err != nil {
		return nil
	}

	// Grouped by constraint, because width is what decides whether a constraint
	// says anything about a single column: a composite key constrains the tuple
	// and neither half of it.
	type key struct{ table, kind string }
	type part struct{ column, refTable, refColumn string }
	byKey := map[key][]part{}
	var order []key
	for _, r := range rows {
		k := key{str(r["table_name"]), str(r["constraint_type"])}
		if _, seen := byKey[k]; !seen {
			order = append(order, k)
		}
		byKey[k] = append(byKey[k], part{
			column:    str(r["column_name"]),
			refTable:  str(r["ref_table"]),
			refColumn: str(r["ref_column"]),
		})
	}

	for _, k := range order {
		t := byName[k.table]
		if t == nil {
			continue
		}
		parts := byKey[k]
		if k.kind == "PRIMARY KEY" {
			for _, p := range parts {
				t.PrimaryKey = append(t.PrimaryKey, p.column)
			}
			if len(parts) == 1 {
				if c := t.Column(parts[0].column); c != nil {
					c.Unique = true
				}
			}
			continue
		}
		if len(parts) == 1 && parts[0].refTable != "" {
			if c := t.Column(parts[0].column); c != nil {
				c.FK = &model.Ref{Table: parts[0].refTable, Column: parts[0].refColumn}
			}
		}
	}
	return nil
}

// loadCounts reads the row count each table already carries in its metadata.
// COUNT(*) would be a scan across every table in the dataset, billed, to answer
// a question the metadata already holds exactly.
//
// __TABLES__ answers it for the whole dataset in one statement. It is the older
// spelling and a dataset can be configured so it is unavailable, so a failure
// falls back to the table metadata API — one round trip per table, which is
// slower but always there.
func (d *Driver) loadCounts(ctx context.Context, byName map[string]*model.Table) error {
	rows, err := d.query(ctx, fmt.Sprintf("SELECT table_id, row_count FROM %s",
		quoteIdent(d.project+"."+d.dataset+".__TABLES__")))
	if err == nil {
		for _, r := range rows {
			if t := byName[str(r["table_id"])]; t != nil {
				t.ExistingRows = asInt64(r["row_count"])
			}
		}
		return nil
	}

	for name, t := range byName {
		md, err := d.client.Dataset(d.dataset).Table(name).Metadata(ctx)
		if err != nil {
			return fmt.Errorf("read row count for %s: %w", name, err)
		}
		t.ExistingRows = int64(md.NumRows)
	}
	return nil
}

// History reads whatever a migration tool left behind. BigQuery records no DDL
// history of its own that a query can reach.
func (d *Driver) History(ctx context.Context) ([]model.Migration, error) {
	rows, err := d.query(ctx, fmt.Sprintf(
		"SELECT table_name FROM %s.INFORMATION_SCHEMA.TABLES WHERE table_type = 'BASE TABLE'",
		d.datasetRef()))
	if err != nil {
		return nil, fmt.Errorf("list tables: %w", err)
	}
	present := map[string]bool{}
	for _, r := range rows {
		present[str(r["table_name"])] = true
	}

	// The quoting handed to the shared reader is this package's own: it builds
	// the whole statement, and a double-quoted table name would reach BigQuery
	// as a string. The table it names is left unqualified, which resolves
	// because query sets the default dataset.
	return db.ReadHistory(ctx, quoteIdent,
		func(table string) bool { return present[table] },
		func(ctx context.Context, query string) ([]map[string]any, error) {
			return d.query(ctx, query)
		}), nil
}

// baseType strips a parameterised type back to its name, so NUMERIC(10, 2) and
// STRING(64) classify as what they are.
func baseType(native string) string {
	if i := strings.IndexAny(native, "(<"); i > 0 {
		return strings.TrimSpace(native[:i])
	}
	return native
}

// decorations pulls the length, precision, and scale out of a rendered type
// such as STRING(64) or NUMERIC(10, 2).
func decorations(native string) (maxLen, precision, scale int) {
	open := strings.IndexByte(native, '(')
	if open < 0 || !strings.HasSuffix(native, ")") {
		return 0, 0, 0
	}
	parts := strings.Split(native[open+1:len(native)-1], ",")
	first, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return 0, 0, 0
	}
	switch strings.ToUpper(strings.TrimSpace(native[:open])) {
	case "STRING", "BYTES":
		return first, 0, 0
	case "NUMERIC", "BIGNUMERIC", "DECIMAL", "BIGDECIMAL":
		if len(parts) > 1 {
			s, _ := strconv.Atoi(strings.TrimSpace(parts[1]))
			return 0, first, s
		}
		return 0, first, 0
	}
	return 0, 0, 0
}

func str(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case []byte:
		return string(t)
	default:
		return fmt.Sprint(t)
	}
}

func asInt64(v any) int64 {
	switch t := v.(type) {
	case int64:
		return t
	case int:
		return int64(t)
	case float64:
		return int64(t)
	case string:
		n, _ := strconv.ParseInt(t, 10, 64)
		return n
	}
	return 0
}

// Tx is a BigQuery seeding run. It is not a transaction — see the package
// comment — and it exists to hold the load machinery and the record of what has
// already been committed.
type Tx struct {
	d    *Driver
	done bool
	// applied is what this run has already made permanent, in order. It is the
	// whole of what Rollback has to offer, and the reason it is kept.
	applied []string
}

// Exec implements db.Tx. The schema editor renders its own SQL and it is run
// here as a query job — which commits when it completes, like everything else
// this driver issues. A CREATE or ALTER applied here survives a later failure
// in the run.
func (t *Tx) Exec(ctx context.Context, sql string) error {
	if err := t.d.run(ctx, sql); err != nil {
		return fmt.Errorf("exec: %w", err)
	}
	t.applied = append(t.applied, "applied a schema change")
	return nil
}

// run executes one statement that returns no rows, waiting for the job.
func (d *Driver) run(ctx context.Context, sql string) error {
	q := d.client.Query(sql)
	q.DefaultProjectID = d.project
	q.DefaultDatasetID = d.dataset
	job, err := q.Run(ctx)
	if err != nil {
		return err
	}
	status, err := job.Wait(ctx)
	if err != nil {
		return err
	}
	return status.Err()
}

// Truncate implements db.Tx.
//
// TRUNCATE TABLE is a DML statement here and it is its own transaction: once
// this returns, the rows are gone whether or not the rest of the run succeeds.
// BigQuery's time travel can still read the table as it was up to seven days
// back, but recovering it is a statement a person runs deliberately, not
// something a driver can do on the way out of a failure.
func (t *Tx) Truncate(ctx context.Context, tb *model.Table) error {
	if err := t.d.run(ctx, "TRUNCATE TABLE "+t.d.qualified(tb.Name)); err != nil {
		return fmt.Errorf("truncate %s: %w", tb.Name, err)
	}
	t.applied = append(t.applied, "emptied "+tb.Name)
	return nil
}

// Rows per load job. A load is billed as one operation however big it is, so
// fewer jobs is cheaper as well as faster — but a job that fails takes its
// whole batch with it, and a batch is also the granularity a mid-run failure
// leaves behind. A hundred thousand rows is small enough to retry and large
// enough that the per-job overhead disappears against it.
const loadRows = 100_000

// Insert implements db.Tx by streaming the rows into a load job as
// newline-delimited JSON.
//
// A load job is the staged path BigQuery documents for getting bulk data in,
// and it is the right one here for a reason worth writing down: the storage
// write API is faster still, but it speaks protocol buffers, and using it means
// building a descriptor for every table at runtime from the catalog and
// encoding each row against it. That is a large amount of machinery whose only
// payoff is throughput on a path that is already not the bottleneck — Seedora
// generates the rows it loads — and it cannot be tested against anything but a
// real billed project. NDJSON into a load job needs no schema of its own: the
// destination table already has one, and BigQuery matches the fields by name.
//
// The rows are pushed through a pipe rather than buffered, so a table of any
// size loads in constant memory; the upload and the generator run at whatever
// speed the slower of them allows.
//
// A failure partway leaves every load job that already finished. Each job is
// one batch of loadRows, so what a mid-run failure leaves in the table is a
// whole number of batches and nothing of the one that was in flight — a load
// job is atomic in itself, which is the one guarantee available here.
func (t *Tx) Insert(ctx context.Context, tb *model.Table, cols []string, rows db.Source) (int64, error) {
	if len(cols) == 0 {
		return 0, nil
	}
	table := t.d.client.Dataset(t.d.dataset).Table(tb.Name)

	next, stop := iter.Pull(rows.Rows())
	defer stop()

	var written int64
	for {
		n, more, err := t.loadBatch(ctx, table, cols, next)
		written += n
		if err != nil {
			return written, err
		}
		if n > 0 {
			t.applied = append(t.applied, fmt.Sprintf("loaded %d rows into %s", n, tb.Name))
		}
		if !more {
			break
		}
	}
	// A generator that fails halfway simply stops yielding, so without this a
	// short write would look like a successful one.
	if err := rows.Err(); err != nil {
		return written, fmt.Errorf("generate rows for %s: %w", tb.Name, err)
	}
	return written, nil
}

// loadBatch runs one load job over at most loadRows rows, and reports whether
// the source has more.
func (t *Tx) loadBatch(
	ctx context.Context,
	table *bigquery.Table,
	cols []string,
	next func() (map[string]any, bool),
) (int64, bool, error) {
	// The first row is pulled before anything is started: a batch boundary that
	// lands exactly on the end of the source would otherwise run a load job over
	// no rows at all, which costs a round trip to say nothing.
	first, ok := next()
	if !ok {
		return 0, false, nil
	}

	pr, pw := io.Pipe()

	type result struct {
		rows int64
		more bool
		err  error
	}
	done := make(chan result, 1)
	go func() {
		var (
			n   int64
			enc = json.NewEncoder(pw)
			buf = make(map[string]any, len(cols))
		)
		var err error
		row, ok := first, true
		for ok && n < loadRows {
			clear(buf)
			for _, c := range cols {
				// A key the plan did not name is absent, and absent stays
				// absent: BigQuery then applies the column's own default, which
				// is what an omitted column is supposed to mean.
				if v, found := row[c]; found {
					buf[c] = value(v)
				}
			}
			if err = enc.Encode(buf); err != nil {
				break
			}
			n++
			if n < loadRows {
				row, ok = next()
			}
		}
		// Stopping on the row limit means there may be more; the next batch
		// finds out by pulling, and returns without running a job if there is
		// not. Nothing pulled here is ever discarded, which is why the pull is
		// guarded rather than run unconditionally at the end of the loop.
		more := err == nil && n == loadRows
		pw.CloseWithError(err)
		done <- result{rows: n, more: more, err: err}
	}()

	src := bigquery.NewReaderSource(pr)
	src.SourceFormat = bigquery.JSON
	// A value the destination has no column for is a bug in the plan, not
	// something to drop quietly, and the same goes for a row that will not
	// parse: one bad row fails the job rather than vanishing from the table.
	src.IgnoreUnknownValues = false
	src.MaxBadRecords = 0

	loader := table.LoaderFrom(src)
	loader.WriteDisposition = bigquery.WriteAppend
	// The table must already exist. Creating one from an inferred schema would
	// silently produce a table shaped like the seed instead of failing on a
	// name the plan got wrong.
	loader.CreateDisposition = bigquery.CreateNever

	job, err := loader.Run(ctx)
	if err != nil {
		// The encoder may still be blocked writing into the pipe nobody is
		// reading any more.
		pr.CloseWithError(err)
		<-done
		return 0, false, fmt.Errorf("load into %s: %w", table.TableID, err)
	}
	res := <-done
	if res.err != nil {
		pr.CloseWithError(res.err)
		return 0, false, fmt.Errorf("encode rows for %s: %w", table.TableID, res.err)
	}
	status, err := job.Wait(ctx)
	if err != nil {
		return 0, false, fmt.Errorf("load into %s: %w", table.TableID, err)
	}
	if err := status.Err(); err != nil {
		return 0, false, fmt.Errorf("load into %s: %w", table.TableID, err)
	}
	return res.rows, res.more, nil
}

// value adapts the generated types whose default JSON encoding is not what
// BigQuery reads back as the same value.
func value(v any) any {
	switch x := v.(type) {
	case time.Time:
		// RFC 3339 in UTC, which TIMESTAMP, DATETIME, DATE, and TIME all parse
		// from a string. The zone is normalised so a run is reproducible on a
		// machine in another timezone.
		return x.UTC().Format(time.RFC3339Nano)
	case []byte:
		// encoding/json already base64s a []byte, which is exactly how BYTES is
		// spelled in a JSON load. Named here so it is clear that is deliberate.
		return x
	default:
		return v
	}
}

// ReadKeys implements db.Tx.
//
// On the other engines this reads the run's own uncommitted writes from inside
// the transaction. Here the rows it returns are already committed, which
// changes nothing about the answer: the parent rows this run loaded are visible
// because the load job that wrote them has finished.
func (t *Tx) ReadKeys(ctx context.Context, tb *model.Table, col string, limit int) ([]any, error) {
	name := quoteIdent(col)
	sql := fmt.Sprintf("SELECT %s AS k FROM %s WHERE %s IS NOT NULL LIMIT %d",
		name, t.d.qualified(tb.Name), name, limit)
	rows, err := t.d.query(ctx, sql)
	if err != nil {
		return nil, fmt.Errorf("read keys from %s.%s: %w", tb.Name, col, err)
	}
	out := make([]any, 0, len(rows))
	for _, r := range rows {
		out = append(out, r["k"])
	}
	return out, nil
}

// Commit implements db.Tx. There is nothing to commit: every job this run ran
// committed itself as it completed. It is still the method that ends the run,
// so it is what stops Rollback from reporting a failure that did not happen.
func (t *Tx) Commit(context.Context) error {
	t.done = true
	return nil
}

// Rollback implements db.Tx, and is the one method here that cannot do what the
// interface asks.
//
// BigQuery commits as it loads. By the time a run fails, the truncates have
// emptied their tables and the load jobs that already finished have made their
// rows permanent; there is no undo, and returning nil would tell the seeder —
// and through it the user — that the database was restored when it was not. So
// this returns an error naming exactly what was left behind, which is the only
// useful thing left to say.
//
// After Commit it is a no-op, so the seeder can still defer it unconditionally.
func (t *Tx) Rollback(context.Context) error {
	if t.done {
		return nil
	}
	t.done = true
	if len(t.applied) == 0 {
		return nil
	}
	return fmt.Errorf("this run cannot be rolled back: in BigQuery each load and each "+
		"statement commits on its own. Already permanent: %s. Re-run the seed "+
		"to replace it, or read the table as it was with FOR SYSTEM_TIME AS OF while the "+
		"time travel window lasts", strings.Join(t.applied, "; "))
}
