// Package storetest gives tests a migrated store database without running the migrations again.
package storetest

import (
	"testing"

	"github.com/babs/grill-room/internal/sqlitetest"
	"github.com/babs/grill-room/internal/store"
)

// Path writes a migrated, empty store database to name under t.TempDir and returns its path.
func Path(t testing.TB, name string) string {
	t.Helper()
	return sqlitetest.Template(t, name, func(p string) error {
		st, err := store.Open(p)
		if err != nil {
			return err
		}
		return st.Close()
	})
}
