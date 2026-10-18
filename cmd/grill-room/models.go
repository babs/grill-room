package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os/signal"
	"syscall"

	"go.uber.org/zap"

	"github.com/babs/grill-room/internal/devicestt"
)

// command runs a subcommand: `models fetch --dir <dir>` fills the device transcription model
// directory (010-AC6). With --best-effort a failed fetch logs and succeeds: device mode is
// optional, and the server's start check then leaves it off.
func command(args []string, m devicestt.Manifest, client *http.Client) error {
	if len(args) < 2 || args[0] != "models" || args[1] != "fetch" {
		return fmt.Errorf("unknown command %q: want no argument to serve, models fetch [--best-effort] --dir <dir>, or db copy|verify --from <file> [--to <dsn>]", args)
	}
	fs := flag.NewFlagSet("models fetch", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dir := fs.String("dir", "", "directory to fill with the pinned files")
	bestEffort := fs.Bool("best-effort", false, "log a failed fetch and exit 0")
	if err := fs.Parse(args[2:]); err != nil {
		return fmt.Errorf("models fetch: %w", err)
	}
	if *dir == "" || fs.NArg() > 0 {
		return errors.New("usage: grill-room models fetch [--best-effort] --dir <dir>")
	}
	logger, err := newLogger(Config{LogLevel: getEnv("LOG_LEVEL", "info"), LogFormat: getEnv("LOG_FORMAT", "auto")})
	if err != nil {
		return fmt.Errorf("init logger: %w", err)
	}
	defer logger.Sync() //nolint:errcheck // stdout sync is best-effort
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	err = m.Fetch(ctx, *dir, client, logger)
	if err != nil && *bestEffort {
		logger.Warn("models_fetch_failed", zap.Error(err))
		return nil
	}
	if err != nil {
		return fmt.Errorf("models fetch: %w", err)
	}
	return nil
}
