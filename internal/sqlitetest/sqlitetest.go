// Package sqlitetest turns fsync off for every SQLite database a test binary opens: CI disks take
// milliseconds per fsync, and each store Open commits once per migration. Tests import it for its
// side effect.
package sqlitetest

import (
	"context"
	"testing"

	"modernc.org/sqlite"
)

func init() {
	// A program that is not a test keeps its durability.
	if !testing.Testing() {
		panic("sqlitetest imported outside a test binary")
	}
	sqlite.RegisterConnectionHook(func(c sqlite.ExecQuerierContext, _ string) error {
		_, err := c.ExecContext(context.Background(), "PRAGMA synchronous = OFF", nil)
		return err
	})
}
