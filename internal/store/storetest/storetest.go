// Package storetest gives tests a store database without building the schema again.
package storetest

import (
	"testing"

	"github.com/babs/grill-room/internal/pgtest"
	"github.com/babs/grill-room/internal/store"
)

// DSN returns the DSN of an empty store database of the test's own, dropped when t ends. It skips
// t without a test server. The package's TestMain must call pgtest.Main.
func DSN(t testing.TB) string {
	t.Helper()
	return pgtest.Template(t, func(dsn string) error {
		st, err := store.Open(dsn)
		if err != nil {
			return err
		}
		return st.Close()
	})
}
