package db

import (
	"fmt"
	"strings"
)

// productionTokens are the words that, in a database or host name, mean somebody
// is one flag away from filling a real system with fake customers. The list is
// deliberately short: every entry here is a word nobody uses for a scratch
// database, so a match is worth stopping for and a false positive is rare.
var productionTokens = []string{
	"prod", "production", "live", "master", "primary",
}

// hostTokens additionally catch managed-database hostnames, where the database
// name is often something innocuous like "app" and the host is the only clue.
var hostTokens = []string{
	".rds.amazonaws.com", ".rds.", "prod", "production",
	".azure.com", ".neon.tech", ".supabase.co", ".planetscale",
	".cloud.timescale.com", ".cockroachlabs.cloud",
}

// GuardError is a refusal to connect to something that looks like production.
type GuardError struct {
	Target Target
	Reason string
}

func (e *GuardError) Error() string {
	return fmt.Sprintf("refusing to seed %s on %s: %s\n"+
		"Seedora is a development tool. If this really is a target you meant, "+
		"re-run with --i-know-what-im-doing.",
		orUnknown(e.Target.Database), orUnknown(e.Target.Host), e.Reason)
}

// Guard reports whether a DSN looks like production. It is a heuristic and says
// so: it can only ever be a speed bump, and the flag that bypasses it is spelled
// out in full so nobody types it without reading it.
func Guard(dsn string, allow bool) error {
	if allow {
		return nil
	}
	t := Describe(dsn)
	// A local target is never guarded. This is the overwhelmingly common case
	// and stopping on it would train people to pass the bypass flag by reflex,
	// which would defeat the guard everywhere it actually matters.
	if isLocal(t.Host) {
		return nil
	}
	db := strings.ToLower(t.Database)
	for _, tok := range productionTokens {
		if wordMatch(db, tok) {
			return &GuardError{Target: t, Reason: "database name contains " + tok}
		}
	}
	host := strings.ToLower(t.Host)
	for _, tok := range hostTokens {
		if strings.Contains(host, tok) {
			return &GuardError{Target: t, Reason: "host looks like a managed or production instance"}
		}
	}
	if host != "" {
		return &GuardError{Target: t, Reason: "host is not local"}
	}
	return nil
}

func isLocal(host string) bool {
	switch strings.ToLower(host) {
	case "", "localhost", "127.0.0.1", "::1", "0.0.0.0",
		"host.docker.internal", "db", "postgres", "mysql", "database":
		return true
	}
	return strings.HasSuffix(host, ".local") ||
		strings.HasSuffix(host, ".localhost") ||
		strings.HasPrefix(host, "192.168.") ||
		strings.HasPrefix(host, "10.") ||
		strings.HasPrefix(host, "172.17.")
}

// wordMatch looks for a token at a word boundary, so "myapp_prod" and "prod-db"
// match but "product_catalog" and "reproduction" do not. Matching a bare
// substring here would fire on ordinary names often enough to be ignored.
func wordMatch(s, token string) bool {
	for i := 0; i+len(token) <= len(s); i++ {
		if s[i:i+len(token)] != token {
			continue
		}
		if i > 0 && isWordByte(s[i-1]) {
			continue
		}
		if j := i + len(token); j < len(s) && isWordByte(s[j]) {
			continue
		}
		return true
	}
	return false
}

func isWordByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9'
}

func orUnknown(s string) string {
	if s == "" {
		return "(unknown)"
	}
	return s
}
