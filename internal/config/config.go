// Package config resolves everything Seedora needs to start: where the database
// is, which port the UI binds, and how careful to be.
//
// Credentials are handled by oneenv rather than by hand, which buys three things
// the README promises: a .env cascade so a repo's own dev database is picked up
// with no flags, ",file" so a DSN can come from a Docker or Kubernetes secret
// mount instead of the environment, and Secret[string] so the DSN cannot be
// printed by accident — its String, GoString, and JSON forms are all masked.
package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/bakhod1r/oneenv"
)

// Config is the resolved runtime configuration.
type Config struct {
	// DSN is the connection string. It is a Secret so that logging the config,
	// or marshalling it into an API response, cannot leak the password.
	DSN oneenv.Secret[string] `env:"SEEDORA_DSN,secret" desc:"database connection string"`
	// DSNFile reads the DSN from a path instead, for /run/secrets mounts.
	DSNFile string `env:"SEEDORA_DSN_FILE" desc:"file to read the DSN from"`

	Port int    `env:"SEEDORA_PORT" default:"7777" desc:"UI port"`
	Host string `env:"SEEDORA_HOST" default:"127.0.0.1" desc:"UI bind address"`

	ConfigPath string `env:"SEEDORA_CONFIG" default:"seedora.yaml" desc:"mapping file"`
	// Migrations is the project's migration directory. Seedora reads it to
	// learn about tables the repository has and the database does not, which is
	// the state a fresh checkout or a new branch is in.
	Migrations string `env:"SEEDORA_MIGRATIONS" desc:"migration directory to read the schema from"`
	Locale     string `env:"SEEDORA_LOCALE" default:"en_US" desc:"generator locale"`

	Seed  uint64 `env:"SEEDORA_SEED" desc:"fixed RNG seed for reproducible output"`
	Rows  int    `env:"SEEDORA_ROWS" desc:"override the row count for every table"`
	Batch int    `env:"SEEDORA_BATCH" default:"5000" desc:"rows per bulk write"`

	Truncate bool `env:"SEEDORA_TRUNCATE" desc:"truncate target tables before seeding"`
	// Append adds to tables that already hold rows rather than assuming they
	// are empty. It is the opposite of Truncate and refuses to run with it.
	Append bool `env:"SEEDORA_APPEND" desc:"add rows to tables that already have some"`
	// AppendUniqueCap bounds what --append holds in memory per unique text
	// column. Zero takes the seeder's default.
	AppendUniqueCap int `env:"SEEDORA_APPEND_UNIQUE_CAP" desc:"existing values --append holds per unique text column"`
	// TxPerTable commits each table separately instead of running the whole
	// seed in one transaction. It trades all-or-nothing for a run that does not
	// hold one transaction open over a hundred million rows.
	TxPerTable bool `env:"SEEDORA_TX_PER_TABLE" desc:"commit after each table instead of once at the end"`
	DryRun     bool `env:"SEEDORA_DRY_RUN" desc:"generate and validate without writing"`

	// AllowProduction disables the production-target guard. It exists so the
	// guard can be overridden deliberately and never silently.
	AllowProduction bool `env:"SEEDORA_ALLOW_PRODUCTION" desc:"bypass the production-target guard"`
}

// Load resolves configuration from the process environment layered over the
// .env cascade (.env, .env.local, .env.<APP_ENV>, .env.<APP_ENV>.local).
// Missing files are not an error: running with only flags is the common case.
//
// Variable expansion is deliberately off. oneenv offers it, and for a path it
// would be a convenience, but the field that matters here is a connection string
// and a password is not shell syntax. Expansion reads `$` as the start of a
// variable, so a password of `$ecret123` expands to nothing at all and one of
// `pa$word` is truncated to `pa` — the connection then fails with an
// authentication error that points nowhere near the cause. A literal DSN is
// worth more than an expanded one.
func Load() (*Config, error) {
	cfg, err := oneenv.Parse[Config](oneenv.WithEnvFiles())
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	return cfg, nil
}

// Resolve finalises the DSN once flags have been applied. A flag beats the
// environment, which beats the secret file — the order a user expects, since the
// flag is the most explicit thing they typed.
func (c *Config) Resolve(flagDSN string) error {
	switch {
	case flagDSN != "":
		c.DSN = oneenv.NewSecret(flagDSN)
	case c.DSN.Value() != "":
		// Already resolved from SEEDORA_DSN.
	case c.DSNFile != "":
		v, err := readSecretFile(c.DSNFile)
		if err != nil {
			return err
		}
		c.DSN = oneenv.NewSecret(v)
	}
	if c.Batch <= 0 {
		c.Batch = 5000
	}
	return nil
}

// ErrNoDSN is returned when no connection string was supplied anywhere. The UI
// treats it as "show the connect screen" rather than as a fatal error.
var ErrNoDSN = errors.New("no DSN: pass --dsn, set SEEDORA_DSN, or enter one in the UI")

// Connection returns the DSN, or ErrNoDSN.
func (c *Config) Connection() (string, error) {
	if v := c.DSN.Value(); v != "" {
		return v, nil
	}
	return "", ErrNoDSN
}

// Addr is the UI listen address.
func (c *Config) Addr() string { return fmt.Sprintf("%s:%d", c.Host, c.Port) }

// Redacted renders the DSN with its password replaced, for logs and for the UI
// header.
//
// The work is done on the raw string rather than by parsing and re-printing.
// Round-tripping through net/url re-encodes as it goes — the mask itself comes
// back as %2A%2A%2A%2A — and it rewrites parts of the DSN the user typed, which
// makes the header stop matching what they entered. String surgery touches only
// the span that has to go.
func Redacted(dsn string) string {
	// No scheme means no credentials to hide: a SQLite path is the whole DSN
	// and hiding it would leave the header saying nothing.
	sep := strings.Index(dsn, "://")
	if sep < 0 {
		return dsn
	}
	start := sep + 3

	// The authority ends at the first delimiter after it. Everything past that
	// is the path and query, neither of which holds the userinfo.
	end := len(dsn)
	for i := start; i < len(dsn); i++ {
		if c := dsn[i]; c == '/' || c == '?' || c == '#' {
			end = i
			break
		}
	}
	authority := dsn[start:end]

	// The last '@' separates userinfo from host, because a password may
	// legitimately contain one.
	at := strings.LastIndexByte(authority, '@')

	// The query is dropped whole. It can carry a password of its own under any
	// number of parameter names, and enumerating them would mean being wrong
	// about the one engine that spells it differently.
	tail := dsn[end:]
	if q := strings.IndexAny(tail, "?#"); q >= 0 {
		tail = tail[:q]
	}

	if at < 0 {
		return dsn[:start] + authority + tail
	}
	userinfo := authority[:at]
	colon := strings.IndexByte(userinfo, ':')
	if colon < 0 {
		// A username with no password.
		return dsn[:start] + authority + tail
	}
	return dsn[:start] + userinfo[:colon] + ":****" + authority[at:] + tail
}

// Usage writes the environment variables Seedora reads, for `--help`.
func Usage(w io.Writer) error { return oneenv.Usage[Config](w) }

// readSecretFile reads a DSN from a mounted secret. The trailing newline a
// secret manager almost always appends is stripped, since a DSN with a newline
// in it fails to connect with an error that points nowhere near the cause.
func readSecretFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read DSN file %s: %w", path, err)
	}
	return strings.TrimSpace(string(b)), nil
}
