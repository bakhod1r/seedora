// Package store remembers the databases a user has connected to, so the UI's
// connect screen is a list to click rather than a DSN to retype.
//
// It deliberately does not live in the project. seedora.yaml is committed and
// shared, and a connection string is neither — it is per-machine, per-developer,
// and usually carries a password. The store is a file under the user's config
// directory with 0600 permissions, and nothing in the repository ever points at
// it.
//
// Passwords are opt-in per connection. The default is to keep the DSN and drop
// the password, so a stolen laptop yields a hostname rather than a credential,
// and the UI asks for the password on reconnect.
package store

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/bakhod1r/seedora/internal/config"
)

// Connection is one remembered database.
type Connection struct {
	// Name is what the connect screen shows. It defaults to "database on host".
	Name string `yaml:"name" json:"name"`
	// DSN is the connection string. Its password is present only when the user
	// asked for it to be remembered.
	DSN string `yaml:"dsn" json:"dsn"`
	// HasPassword records that the original DSN carried a password which was
	// not stored, so the UI knows to ask for one rather than trying to connect
	// with a string it knows is incomplete.
	HasPassword bool `yaml:"needs_password,omitempty" json:"needs_password,omitempty"`
	// LastUsed orders the list, most recent first.
	LastUsed time.Time `yaml:"last_used" json:"last_used"`
}

// Redacted returns the DSN with any stored password masked, for display.
func (c Connection) Redacted() string { return maskDSN(c.DSN) }

// File is the on-disk shape.
type File struct {
	Version     int          `yaml:"version"`
	Connections []Connection `yaml:"connections"`
}

// version is the store format version.
const version = 1

// maxConnections bounds the list. A connect screen is a shortcut, not a history:
// past a dozen entries, finding the right one is slower than pasting the DSN.
const maxConnections = 12

// Path is where the store lives. It follows the platform's config convention,
// which on Linux means XDG_CONFIG_HOME and on macOS means the same — Seedora is
// a developer tool, and a dotfile under a predictable path is what a developer
// expects to find and to be able to delete.
func Path() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "connections.yaml"), nil
}

// Dir is the directory the per-machine files live in.
//
// SEEDORA_CONFIG_DIR overrides it. That exists for two reasons: a test must not
// write into the developer's own store, and someone running Seedora in a
// container has nowhere sensible for os.UserConfigDir to point.
func Dir() (string, error) {
	if override := os.Getenv("SEEDORA_CONFIG_DIR"); override != "" {
		return override, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate config directory: %w", err)
	}
	return filepath.Join(dir, "seedora"), nil
}

// Load reads the store. A missing file is not an error — it is the state every
// user starts in.
func Load() (*File, error) {
	path, err := Path()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &File{Version: version}, nil
		}
		return nil, err
	}
	var f File
	if err := yaml.Unmarshal(b, &f); err != nil {
		// A corrupt store is not worth failing a session over: the connect
		// screen simply starts empty and the next save rewrites it.
		return &File{Version: version}, nil
	}
	if f.Version > version {
		return nil, fmt.Errorf("connection store version %d is newer than this build understands", f.Version)
	}
	f.Version = version
	return &f, nil
}

// Save writes the store with owner-only permissions.
func (f *File) Save() error {
	path, err := Path()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f.Version = version
	b, err := yaml.Marshal(f)
	if err != nil {
		return err
	}
	// Written through a temporary file in the same directory, created 0600 from
	// the start — a file that is briefly world-readable while it holds a
	// password is a file that leaked one.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".connections-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Remember adds or updates a connection.
//
// keepPassword is the user's explicit choice. When it is false the password is
// stripped before anything touches the disk, which is the difference between a
// convenience and a credential store.
func (f *File) Remember(name, dsn string, keepPassword bool) error {
	if strings.TrimSpace(dsn) == "" {
		return errors.New("empty DSN")
	}
	stored, had := dsn, false
	if !keepPassword {
		stored, had = stripPassword(dsn)
	} else {
		_, had = passwordOf(dsn)
	}
	if name == "" {
		name = describe(dsn)
	}

	c := Connection{
		Name:        name,
		DSN:         stored,
		HasPassword: had && !keepPassword,
		LastUsed:    time.Now(),
	}

	// Identity is the DSN with the password removed, so reconnecting with a
	// corrected password updates the entry instead of adding a second one.
	id, _ := stripPassword(dsn)
	out := make([]Connection, 0, len(f.Connections)+1)
	out = append(out, c)
	for _, existing := range f.Connections {
		if eid, _ := stripPassword(existing.DSN); eid == id {
			continue
		}
		out = append(out, existing)
	}
	if len(out) > maxConnections {
		out = out[:maxConnections]
	}
	f.Connections = out
	return f.Save()
}

// Forget removes a connection by its DSN.
func (f *File) Forget(dsn string) error {
	id, _ := stripPassword(dsn)
	out := f.Connections[:0]
	for _, c := range f.Connections {
		if cid, _ := stripPassword(c.DSN); cid == id {
			continue
		}
		out = append(out, c)
	}
	f.Connections = out
	return f.Save()
}

// WithPassword returns the connection's DSN with a password substituted in, for
// an entry that was stored without one.
func WithPassword(dsn, password string) (string, error) {
	if password == "" {
		return dsn, nil
	}
	u, err := url.Parse(dsn)
	if err != nil || u.Scheme == "" {
		return "", fmt.Errorf("cannot add a password to this DSN — paste the full connection string instead")
	}
	user := ""
	if u.User != nil {
		user = u.User.Username()
	}
	u.User = url.UserPassword(user, password)
	return u.String(), nil
}

// stripPassword removes the password from a DSN, reporting whether there was
// one. A DSN that does not parse is returned unchanged: it is not Seedora's job
// to rewrite a string it does not understand, and the caller stores it as-is
// only when the user asked for that.
func stripPassword(dsn string) (string, bool) {
	u, err := url.Parse(dsn)
	if err != nil || u.User == nil {
		return dsn, false
	}
	if _, ok := u.User.Password(); !ok {
		return dsn, false
	}
	u.User = url.User(u.User.Username())
	return u.String(), true
}

func passwordOf(dsn string) (string, bool) {
	u, err := url.Parse(dsn)
	if err != nil || u.User == nil {
		return "", false
	}
	return u.User.Password()
}

// maskDSN replaces the password for display. It defers to the config package so
// there is one masking rule in the codebase: two would drift, and the one that
// drifted would be the one nobody was watching.
func maskDSN(dsn string) string { return config.Redacted(dsn) }

// describe names a connection the way a person would: the database, then where
// it lives.
func describe(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil || u.Scheme == "" {
		return filepath.Base(dsn)
	}
	name := strings.TrimPrefix(u.Path, "/")
	if name == "" {
		name = u.Opaque
	}
	host := u.Hostname()
	switch {
	case name != "" && host != "":
		return name + " on " + host
	case name != "":
		return name
	case host != "":
		return host
	}
	return dsn
}
