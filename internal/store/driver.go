package store

import (
	"net/url"
	"strings"

	_ "modernc.org/sqlite" // pure-Go SQLite driver (no CGO)
)

// Everything driver-specific lives in this file.

const driverName = "sqlite"

// dsn builds a modernc.org/sqlite DSN with sensible pragmas:
// WAL for durability/concurrency, and a busy timeout in case another process
// (e.g. a backup tool) touches the file.
func dsn(path string) string {
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(NORMAL)")

	// Characters that are special in a file: URI.
	escaped := strings.NewReplacer("%", "%25", "?", "%3f", "#", "%23").Replace(path)
	return "file:" + escaped + "?" + q.Encode()
}
