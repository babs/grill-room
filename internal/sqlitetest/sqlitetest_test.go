package sqlitetest

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
)

func TestSynchronousOff(t *testing.T) {
	for _, dsn := range []string{"%s", "file:%s?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"} {
		db, err := sql.Open("sqlite", fmt.Sprintf(dsn, filepath.Join(t.TempDir(), "t.db")))
		if err != nil {
			t.Fatal(err)
		}
		var mode int
		err = db.QueryRowContext(t.Context(), "PRAGMA synchronous").Scan(&mode)
		_ = db.Close()
		if err != nil || mode != 0 {
			t.Fatalf("%s: synchronous %d, %v; want 0 (off)", dsn, mode, err)
		}
	}
}
