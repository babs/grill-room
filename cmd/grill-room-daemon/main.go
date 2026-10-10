// Command grill-room-daemon hosts a grill-room agent on this machine: it pairs with a room, keeps
// an outbound connection to it, and serves a local admin page on 127.0.0.1.
//
//	grill-room-daemon [room-url]   run, pairing with the room if needed
//	grill-room-daemon check        check Docker, credentials and the sandbox (one haiku request)
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"golang.org/x/term"

	"github.com/babs/grill-room/internal/creds"
	"github.com/babs/grill-room/internal/daemon"
	"github.com/babs/grill-room/internal/sandbox"
)

// Build metadata, injected with -ldflags -X.
var (
	Version        = "v0.0.0"
	CommitHash     = "0000000"
	BuildTimestamp = "1970-01-01T00:00:00"
	Builder        = "unknown"
	ProjectURL     = "https://github.com/babs/grill-room"
)

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// Config is the daemon configuration, read from the environment.
type Config struct {
	StateDir string
	// Browser "none" prints the page link instead of opening it.
	Browser string
	// Image pins the sandbox image; empty takes the room server's, or builds the embedded one.
	Image    string
	LogLevel string
	// Model is the claude --model for turns; empty keeps Claude Code's default.
	Model string
}

func loadConfig() (Config, error) {
	dir := os.Getenv("GRILL_ROOM_DAEMON_STATE_DIR")
	if dir == "" {
		base, err := os.UserConfigDir()
		if err != nil {
			return Config{}, fmt.Errorf("no state dir: set GRILL_ROOM_DAEMON_STATE_DIR: %w", err)
		}
		dir = filepath.Join(base, "grill-room-daemon")
	}
	return Config{
		StateDir: dir,
		Browser:  getEnv("GRILL_ROOM_DAEMON_BROWSER", "auto"),
		Image:    os.Getenv("GRILL_ROOM_SANDBOX_IMAGE"),
		LogLevel: getEnv("LOG_LEVEL", "info"),
		Model:    os.Getenv("GRILL_ROOM_AGENT_MODEL"),
	}, nil
}

// Validate rejects a configuration the daemon cannot run with.
func (c Config) Validate() error {
	if c.Browser != "auto" && c.Browser != "none" {
		return fmt.Errorf("GRILL_ROOM_DAEMON_BROWSER %q: want auto or none", c.Browser)
	}
	if _, err := zapcore.ParseLevel(c.LogLevel); err != nil {
		return fmt.Errorf("LOG_LEVEL: %w", err)
	}
	return nil
}

// newLogger writes to stderr and to ring, which the local page shows.
func newLogger(level string, stderr io.Writer, ring *daemon.Ring) (*zap.Logger, error) {
	lvl, err := zapcore.ParseLevel(level)
	if err != nil {
		return nil, err
	}
	enc := zap.NewDevelopmentEncoderConfig()
	enc.EncodeTime = zapcore.ISO8601TimeEncoder
	out := zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig())
	if f, ok := stderr.(*os.File); ok && term.IsTerminal(int(f.Fd())) { //nolint:gosec // fd fits an int on every supported platform
		out = zapcore.NewConsoleEncoder(enc)
	}
	core := zapcore.NewTee(
		zapcore.NewCore(out, zapcore.Lock(zapcore.AddSync(stderr)), lvl),
		zapcore.NewCore(zapcore.NewConsoleEncoder(enc), zapcore.AddSync(ring), lvl),
	)
	return zap.New(core), nil
}

var anthropic, _ = url.Parse("https://api.anthropic.com") // a constant, valid URL

// sandboxName is stable per state dir and room: a restarted daemon finds and replaces the
// containers a crash left, and two daemons on one machine never share one.
func sandboxName(stateDir string, r daemon.Room) string {
	sum := sha256.Sum256([]byte(stateDir + "\x00" + r.String()))
	return hex.EncodeToString(sum[:6])
}

// restartError asks main to exec the updated binary in place of this process.
type restartError struct {
	exe  string
	args []string
}

func (e restartError) Error() string { return "restart " + e.exe }

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	err := run(ctx, os.Args[1:], os.Stderr)
	stop()
	var re restartError
	if errors.As(err, &re) {
		fmt.Fprintf(os.Stderr, "grill-room-daemon: restarting %s\n", re.exe)
		// Same pid, same terminal: the new version prints and opens a new admin page.
		err = syscall.Exec(re.exe, append([]string{re.exe}, re.args...), os.Environ()) //nolint:gosec // our own binary, just verified
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "grill-room-daemon: %v\n", err)
		os.Exit(1)
	}
}

// run returns when ctx ends or the admin page's stop button is pressed.
func run(ctx context.Context, args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("grill-room-daemon", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		_, _ = fmt.Fprintf(fs.Output(), "usage: grill-room-daemon [room-url]\n       grill-room-daemon check\n")
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err == nil {
		err = cfg.Validate()
	}
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	ring := daemon.NewRing(200)
	logger, err := newLogger(cfg.LogLevel, stderr, ring)
	if err != nil {
		return fmt.Errorf("init logger: %w", err)
	}
	defer logger.Sync() //nolint:errcheck // stderr sync is best-effort
	logger.Info("startup", zap.String("version", Version), zap.String("commit_hash", CommitHash),
		zap.String("build_timestamp", BuildTimestamp), zap.String("builder", Builder), zap.String("project_url", ProjectURL))

	ctx, stop := context.WithCancel(ctx)
	defer stop()
	host, err := os.Hostname()
	if err != nil {
		host = "unknown host"
	}
	src := creds.NewSource(creds.New())
	if fs.Arg(0) == "check" {
		return check(ctx, cfg, host, src, logger)
	}

	var room *daemon.Room
	if fs.NArg() > 0 {
		r, err := daemon.ParseRoomURL(fs.Arg(0))
		if err != nil {
			return err
		}
		room = &r
	}
	exe, err := executable()
	if err != nil {
		logger.Warn("self_update_disabled", zap.Error(err))
	}
	var restart atomic.Bool
	d := daemon.New(daemon.Options{
		Name: host, Version: Version, Tokens: daemon.NewTokens(cfg.StateDir), Docker: sandbox.Docker, Creds: src,
		// No client timeout: it would cut the long-lived room stream; each request has its own deadline.
		HTTP: &http.Client{}, Log: logger, Model: cfg.Model,
		Image: cfg.Image, Stream: sandbox.DockerStream, Executable: exe,
		Restart: func() { restart.Store(true); stop() },
		NewAgent: func(r daemon.Room, image string) daemon.Agent {
			return sandbox.New(sandbox.Config{Image: image, Name: sandboxName(cfg.StateDir, r), Host: host, Upstream: anthropic},
				sandbox.Docker, src, logger.Named("sandbox"))
		},
	})
	// Links are printed, never logged, so they stay on this terminal.
	announce := func(link string) { _, _ = fmt.Fprintf(stderr, "\n  Admin page: %s\n\n", link) }
	admin := daemon.NewAdmin(ctx, d, ring, stop, announce, logger)
	ln, err := admin.Listen(ctx)
	if err != nil {
		return fmt.Errorf("admin page: %w", err)
	}
	srv := &http.Server{Handler: admin.Handler(), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second}
	go d.Run(ctx)
	if room != nil {
		d.SetRoom(ctx, *room)
	}
	link := admin.NewLink()
	announce(link)
	if cfg.Browser == "auto" {
		if err := daemon.Host().OpenURL(link); err != nil {
			logger.Warn("browser_open_failed", zap.Error(err), zap.String("hint", "open the admin page link printed above"))
		}
	}
	err = daemon.Serve(ctx, srv, ln)
	stop()
	// The sandbox's containers go with the room loops: exiting first would leave them running.
	d.Wait()
	logger.Info("shutdown")
	if err == nil && restart.Load() {
		// The room the page paired with may not be on the command line.
		args := fs.Args()
		if room := d.Status().Room; room != "" {
			args = []string{room}
		}
		return restartError{exe: exe, args: args}
	}
	return err
}

// executable is this binary's real path, the file a self-update replaces.
func executable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate own binary: %w", err)
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return "", fmt.Errorf("locate own binary: %w", err)
	}
	return exe, nil
}

// check reports whether this machine can host, then runs one haiku turn in a throwaway sandbox,
// checks its isolation and scans it for the real token.
func check(ctx context.Context, cfg Config, host string, src *creds.Source, log *zap.Logger) error {
	ready := sandbox.Check(ctx, sandbox.Docker)
	if !ready.OK {
		return errors.New(ready.Reason)
	}
	fmt.Printf("docker: ok, %s (desktop: %v)\n", ready.Version, ready.Desktop)
	tok, err := src.Get(ctx)
	if err != nil {
		return fmt.Errorf("credentials: %w", err)
	}
	fmt.Printf("credentials: ok, from %s, expires %s\n", tok.Origin, tok.ExpiresAt.Local().Format(time.RFC3339))
	image := cfg.Image
	if image == "" {
		if image, err = sandbox.EnsureImage(ctx, sandbox.Docker, sandbox.DockerStream, "", func(_, line string) { fmt.Println("  " + line) }); err != nil {
			return fmt.Errorf("sandbox image: %w", err)
		}
	}
	fmt.Printf("sandbox image: %s\n", image)
	sb := sandbox.New(sandbox.Config{Image: image, Name: fmt.Sprintf("check-%d", time.Now().Unix()), Host: host, Upstream: anthropic},
		sandbox.Docker, src, log)
	defer sb.Close(ctx)
	tctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	res, err := sb.RunClaude(tctx, sandbox.Request{Prompt: "say ok", Model: "haiku"})
	if err != nil {
		return fmt.Errorf("sandbox turn: %w", err)
	}
	fmt.Printf("sandbox turn: %q (session %s)\n", res.Result, res.SessionID)
	if err := sb.Isolated(ctx); err != nil {
		return fmt.Errorf("isolation: %w", err)
	}
	fmt.Println("isolation: ok, only the proxy is reachable")
	counts, err := sb.Scan(ctx, []byte(tok.AccessToken))
	if err != nil {
		return fmt.Errorf("scan: %w", err)
	}
	fmt.Printf("real token matches: filesystem %d, volumes %d, inspect %d\n", counts["filesystem"], counts["volumes"], counts["inspect"])
	if counts["filesystem"]+counts["volumes"]+counts["inspect"] != 0 {
		return errors.New("the sandbox holds the real token")
	}
	return nil
}
