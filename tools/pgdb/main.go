// Command pgdb makes the throwaway databases the e2e servers run on.
//
//	pgdb fresh <server-url> <dbname>  drop dbname if it exists, create it with C collation, print its DSN
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/babs/grill-room/internal/store"
)

// DDL takes no placeholders and the name goes in as is: plain lowercase identifiers only. The e2e_
// prefix keeps the drop away from any database but the e2e ones.
var nameRE = regexp.MustCompile(`^e2e_[a-z0-9_]{1,59}$`)

const usage = "usage: pgdb fresh <server-url> <dbname>"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) != 3 || args[0] != "fresh" {
		_, _ = fmt.Fprintln(stderr, usage)
		return 2
	}
	server, name := args[1], args[2]
	if !nameRE.MatchString(name) {
		_, _ = fmt.Fprintf(stderr, "pgdb: database name %q must match %s\n", name, nameRE)
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	out, err := fresh(ctx, server, name)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "pgdb fresh %s: %v\n", name, err)
		return 1
	}
	_, _ = fmt.Fprintln(stdout, out)
	return 0
}

// fresh makes database name anew and returns its DSN. Its errors never hold the server URL: pgx
// redacts the password in its own.
func fresh(ctx context.Context, server, name string) (string, error) {
	cfg, err := pgx.ParseConfig(server)
	if err != nil {
		return "", fmt.Errorf("server url: %w", err)
	}
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return "", fmt.Errorf("connect: %w", err)
	}
	defer conn.Close(context.Background()) //nolint:errcheck // nothing left to flush
	if _, err := conn.Exec(ctx, `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`); err != nil {
		return "", fmt.Errorf("drop: %w", err)
	}
	if _, err := conn.Exec(ctx, `CREATE DATABASE `+name+` `+store.CreateDatabaseOptions); err != nil {
		return "", fmt.Errorf("create: %w", err)
	}
	return dsn(server, name)
}

// dsn is server pointed at database name, in the form server is written in.
func dsn(server, name string) (string, error) {
	if !strings.HasPrefix(server, "postgres://") && !strings.HasPrefix(server, "postgresql://") {
		return server + " dbname=" + name, nil
	}
	u, err := url.Parse(server)
	if err != nil {
		// url's error quotes the whole URL, password included.
		return "", errors.New("server url is not a valid URL")
	}
	u.Path = "/" + name
	return u.String(), nil
}
