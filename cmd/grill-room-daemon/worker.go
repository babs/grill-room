package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"path/filepath"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/babs/grill-room/internal/creds"
	"github.com/babs/grill-room/internal/daemon"
	"github.com/babs/grill-room/internal/sandbox"
)

// runWorker serves one instance's rooms for the supervisor that started it, over stdin and
// stdout (docs/daemon-protocol.md). Its logs travel on stdout too; stderr is for crashes.
func runWorker(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("grill-room-daemon worker", flag.ContinueOnError)
	fs.SetOutput(stderr)
	instance := fs.String("instance", "", "the instance id this worker serves")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *instance == "" {
		return errors.New("worker: --instance is required; the supervisor starts workers, run grill-room-daemon without arguments")
	}
	conn := daemon.NewConn(stdin, stdout)
	cfg, err := daemon.WorkerHandshake(conn, *instance, Version)
	if err != nil {
		return err
	}
	lvl, err := zapcore.ParseLevel(cfg.LogLevel)
	if err != nil {
		lvl = zapcore.InfoLevel
	}
	enc := zap.NewProductionEncoderConfig()
	enc.EncodeTime = zapcore.ISO8601TimeEncoder
	logger := zap.New(zapcore.NewCore(zapcore.NewJSONEncoder(enc), daemon.LogWriter{Conn: conn}, lvl)).With(zap.String("instance", cfg.Instance))
	defer logger.Sync() //nolint:errcheck // each entry is sent as it is written

	settingsPath := filepath.Join(cfg.ConfigDir, "settings.json")
	settings, err := daemon.LoadSettings(settingsPath, cfg.Settings)
	if err != nil {
		return fmt.Errorf("settings: %w", err)
	}
	allowlistPath := filepath.Join(cfg.ConfigDir, "allowlist.json")
	allowlist, err := daemon.LoadAllowlist(allowlistPath)
	if err != nil {
		return fmt.Errorf("allowlist: %w", err)
	}
	exe, err := executable()
	if err != nil {
		logger.Warn("self_update_disabled", zap.Error(err))
	}
	src := creds.NewSource(creds.New())
	d := daemon.New(daemon.Options{
		Settings: settings, SettingsPath: settingsPath, Allowlist: allowlist, AllowlistPath: allowlistPath,
		Name: cfg.Host, Version: Version, Tokens: daemon.NewTokens(cfg.StateDir), Docker: sandbox.Docker, Creds: src,
		// No client timeout: it would cut the long-lived room stream; each request has its own deadline.
		HTTP: &http.Client{}, Log: logger, Model: cfg.Model,
		Image: cfg.Image, Stream: sandbox.DockerStream, Executable: exe,
		CacheDir: filepath.Join(cfg.CacheDir, "workspace-cache"), CacheMax: cfg.CacheMB << 20,
		OnTurn: func(e daemon.TurnEvent) { _ = conn.Send(daemon.Msg{Type: "turn", Turn: &e}) },
		NewAgent: func(r daemon.Room, image string) daemon.Agent {
			return sandbox.New(sandbox.Config{Image: image, Name: daemon.SandboxName(cfg.SandboxRoot, r), Host: cfg.Host, Upstream: anthropic, Limits: cfg.Limits},
				sandbox.Docker, src, logger.Named("sandbox"))
		},
	})
	logger.Info("worker_startup", zap.String("version", Version), zap.String("commit_hash", CommitHash))
	err = daemon.ServeWorker(ctx, conn, d, cfg)
	logger.Info("worker_shutdown")
	return err
}
