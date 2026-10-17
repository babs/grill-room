// Package sqlitetest makes SQLite cheap in tests. It turns fsync off for every database a test
// binary opens, as CI disks take milliseconds per fsync, and Template copies a database made once.
package sqlitetest

import (
	"context"
	"os"
	"path/filepath"
	"sync"
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

// template is a database made once and the copies made from it.
type template struct {
	once sync.Once
	data []byte
	err  error
}

var shared template

// Template writes a database to name under t.TempDir and returns its path: a copy of the one
// create made at a path of its own, once per test binary, so every call in a binary must create
// the same database. Under -race, running every store migration took over a second per test.
func Template(t testing.TB, name string, create func(path string) error) string {
	t.Helper()
	return shared.copy(t, name, create)
}

func (m *template) copy(t testing.TB, name string, create func(path string) error) string {
	t.Helper()
	m.once.Do(func() {
		dir, err := os.MkdirTemp("", "sqlitetest-")
		if err != nil {
			m.err = err
			return
		}
		defer func() { _ = os.RemoveAll(dir) }()
		p := filepath.Join(dir, "template.db")
		if m.err = create(p); m.err == nil {
			m.data, m.err = os.ReadFile(p) //nolint:gosec // p is the temp dir this function made
		}
	})
	if m.err != nil {
		t.Fatalf("sqlitetest template: %v", m.err)
	}
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, m.data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}
