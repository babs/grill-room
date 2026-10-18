package main

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/babs/grill-room/internal/dbcopy"
)

const dbUsage = `usage: grill-room db copy|verify --from <sqlite file> [--to <postgres dsn>]

  copy    copies the SQLite database of the last SQLite server into an empty PostgreSQL
          database in one transaction, and commits only when every table compares equal
  verify  compares the two databases table by table, writing nothing

  --from  the SQLite file: a consistent backup taken with the server stopped, at schema 46
  --to    the PostgreSQL DSN; default GRILL_ROOM_DATABASE_URL

Both print each table's row count and sha256 on both sides. Exit status: 0 equal, 1 a
refusal, an error or a difference, 2 a usage error.
`

// dbCommand runs `db copy` and `db verify` (017-AC7, 017-AC8) and returns the exit status.
func dbCommand(ctx context.Context, args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	if len(args) == 0 || (args[0] != "copy" && args[0] != "verify") {
		_, _ = fmt.Fprint(stderr, dbUsage)
		return 2
	}
	fs := flag.NewFlagSet("db "+args[0], flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { _, _ = fmt.Fprint(stderr, dbUsage) }
	from := fs.String("from", "", "")
	to := fs.String("to", "", "")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *from == "" || fs.NArg() > 0 {
		_, _ = fmt.Fprint(stderr, dbUsage)
		return 2
	}
	dsn := cmp.Or(*to, getenv("GRILL_ROOM_DATABASE_URL"))
	if dsn == "" {
		_, _ = fmt.Fprintln(stderr, "grill-room: db "+args[0]+": no target: pass --to or set GRILL_ROOM_DATABASE_URL")
		return 2
	}
	run := dbcopy.Copy
	if args[0] == "verify" {
		run = dbcopy.Verify
	}
	if err := run(ctx, *from, dsn, stdout); err != nil {
		_, _ = fmt.Fprintf(stderr, "grill-room: db %s: %v\n", args[0], err)
		return 1
	}
	return 0
}
