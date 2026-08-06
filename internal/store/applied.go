package store

import (
	"errors"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/bakhod1r/seedora/internal/config"
)

// A migration tool's table says what that tool applied. It says nothing about a
// column added from this diagram, because Seedora is not a migration tool and
// does not write into anyone else's bookkeeping.
//
// So it keeps its own. Every batch the schema editor applies is recorded here,
// against the database it was applied to, which answers the one question the
// other tables cannot: what did I change from this window, and when.
//
// It lives beside the connection store — per-machine, outside the repository —
// because it is a record of what someone did at their desk, not a property of
// the schema. The committed history of a schema is its migration files.

// Applied is one batch of schema changes this tool ran.
type Applied struct {
	// Target is the redacted DSN, which is what identifies the database
	// without carrying a password onto the disk.
	Target string    `yaml:"target" json:"target"`
	At     time.Time `yaml:"at" json:"at"`
	// Statements is the SQL as it was executed.
	Statements []string `yaml:"statements" json:"statements"`
	// Summary is the short description shown in the list.
	Summary string `yaml:"summary" json:"summary"`
}

// AppliedFile is the on-disk shape.
type AppliedFile struct {
	Version int       `yaml:"version"`
	Entries []Applied `yaml:"entries"`
}

// maxApplied bounds the log. It is a record of recent work, not an audit trail;
// a database's real history is its migration files.
const maxApplied = 200

// AppliedPath is where the log lives.
func AppliedPath() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "applied.yaml"), nil
}

// LoadApplied reads the log. A missing or unreadable file is an empty log: this
// is a convenience, and failing a session over it would be absurd.
func LoadApplied() *AppliedFile {
	path, err := AppliedPath()
	if err != nil {
		return &AppliedFile{Version: version}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			// Unreadable for some other reason — still nothing worth stopping
			// for, and the next write replaces it.
			return &AppliedFile{Version: version}
		}
		return &AppliedFile{Version: version}
	}
	var f AppliedFile
	if err := yaml.Unmarshal(b, &f); err != nil {
		return &AppliedFile{Version: version}
	}
	f.Version = version
	return &f
}

// RecordApplied appends a batch and writes the log.
//
// The DSN is redacted before it is stored: this file identifies databases, and
// a file that identifies them by a string containing a password is a file that
// stores passwords.
func RecordApplied(dsn, summary string, statements []string) error {
	f := LoadApplied()
	f.Entries = append(f.Entries, Applied{
		Target:     config.Redacted(dsn),
		At:         time.Now().UTC(),
		Statements: statements,
		Summary:    summary,
	})
	if len(f.Entries) > maxApplied {
		f.Entries = f.Entries[len(f.Entries)-maxApplied:]
	}
	return f.save()
}

// AppliedFor returns the entries recorded against one database, oldest first.
func AppliedFor(dsn string) []Applied {
	target := config.Redacted(dsn)

	var out []Applied
	for _, e := range LoadApplied().Entries {
		if e.Target == target {
			out = append(out, e)
		}
	}
	return out
}

func (f *AppliedFile) save() error {
	path, err := AppliedPath()
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
	// Same care as the connection store: created 0600 from the start and moved
	// into place, so there is no window where it is readable by anyone else and
	// no half-written file if this is interrupted.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".applied-*")
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
