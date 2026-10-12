// Command grill-room-daemon hosts a grill-room agent on this machine: it pairs with a room, keeps
// an outbound connection to it, and serves a local admin page on 127.0.0.1.
//
//	grill-room-daemon [room-url]              run the supervisor, or hand the room to the running one
//	grill-room-daemon worker --instance <id>  serve one instance's rooms for the supervisor
//	grill-room-daemon check                   check Docker, credentials and the sandbox (one haiku request)
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"golang.org/x/term"

	"github.com/babs/grill-room/internal/daemon"
	"github.com/babs/grill-room/internal/monitor"
	"github.com/babs/grill-room/internal/sandbox"
	"github.com/babs/grill-room/internal/trust"
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

// Config is the supervisor configuration, read from the environment. Workers get theirs from the
// supervisor.
type Config struct {
	Paths daemon.Paths
	// Legacy is where a single-room daemon kept its state, migrated on start.
	Legacy string
	// Browser "none" prints the page link instead of opening it.
	Browser string
	// Image pins the sandbox image; empty takes the room server's, or builds the embedded one.
	Image    string
	LogLevel string
	// Settings are the host cap and approval mode until the admin page saves its own.
	Settings daemon.Settings
	// Limits cap the sandbox's and its egress proxy's CPUs and memory.
	Limits sandbox.Limits
	// CacheMB caps the workspace cache; 0 fetches every workspace whole.
	CacheMB int64
	// GitHub is the repository the default update channel follows.
	GitHub string
}

func loadConfig() (Config, error) {
	home, _ := os.UserHomeDir() // ResolvePaths reports a missing home where it needs one
	paths, legacy, err := daemon.ResolvePaths(runtime.GOOS, os.Getenv, home)
	if err != nil {
		return Config{}, err
	}
	approveAll, err := strconv.ParseBool(getEnv("GRILL_ROOM_APPROVE_ALL", "false"))
	if err != nil {
		return Config{}, fmt.Errorf("GRILL_ROOM_APPROVE_ALL: %w", err)
	}
	cacheMB, err := strconv.ParseInt(getEnv("GRILL_ROOM_DAEMON_CACHE_MB", "1024"), 10, 64)
	if err != nil || cacheMB < 0 {
		return Config{}, fmt.Errorf("GRILL_ROOM_DAEMON_CACHE_MB: want a size in MiB, 0 to disable the cache")
	}
	return Config{
		CacheMB:  cacheMB,
		GitHub:   strings.TrimRight(getEnv("GRILL_ROOM_GITHUB_URL", "https://github.com/babs/grill-room"), "/"),
		Paths:    paths,
		Legacy:   legacy,
		Browser:  getEnv("GRILL_ROOM_DAEMON_BROWSER", "auto"),
		Image:    os.Getenv("GRILL_ROOM_SANDBOX_IMAGE"),
		LogLevel: getEnv("LOG_LEVEL", "info"),
		Settings: daemon.Settings{MaxModel: os.Getenv("GRILL_ROOM_MAX_MODEL"), MaxEffort: os.Getenv("GRILL_ROOM_MAX_EFFORT"), ApproveAll: approveAll},
		Limits: sandbox.Limits{
			CPUs: getEnv("GRILL_ROOM_SANDBOX_CPUS", "2"), Memory: getEnv("GRILL_ROOM_SANDBOX_MEMORY", "4g"),
			EgressCPUs: getEnv("GRILL_ROOM_EGRESS_CPUS", "0.5"), EgressMemory: getEnv("GRILL_ROOM_EGRESS_MEMORY", "256m"),
		},
	}, nil
}

// Validate rejects a configuration the daemon cannot run with.
func (c Config) Validate() error {
	if c.Browser != "auto" && c.Browser != "none" {
		return fmt.Errorf("GRILL_ROOM_DAEMON_BROWSER %q: want auto or none", c.Browser)
	}
	if u, err := url.Parse(c.GitHub); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return fmt.Errorf("GRILL_ROOM_GITHUB_URL %q: want a repository URL like https://github.com/babs/grill-room", c.GitHub)
	}
	if _, err := zapcore.ParseLevel(c.LogLevel); err != nil {
		return fmt.Errorf("LOG_LEVEL: %w", err)
	}
	if _, err := c.Settings.Normalize(); err != nil {
		return fmt.Errorf("GRILL_ROOM_MAX_MODEL or GRILL_ROOM_MAX_EFFORT: %w", err)
	}
	for name, v := range map[string]string{"GRILL_ROOM_SANDBOX_CPUS": c.Limits.CPUs, "GRILL_ROOM_EGRESS_CPUS": c.Limits.EgressCPUs} {
		if v != "" && !sandbox.ValidCPUs(v) {
			return fmt.Errorf("%s %q: want a number of CPUs like 2 or 0.5, 0 for no limit", name, v)
		}
	}
	for name, v := range map[string]string{"GRILL_ROOM_SANDBOX_MEMORY": c.Limits.Memory, "GRILL_ROOM_EGRESS_MEMORY": c.Limits.EgressMemory} {
		if v != "" && !sandbox.ValidMemory(v) {
			return fmt.Errorf("%s %q: want a size like 4g or 256m, 0 for no limit", name, v)
		}
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
	if fs.Arg(0) == "worker" {
		return runWorker(ctx, fs.Args()[1:], os.Stdin, os.Stdout, stderr)
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
	host, err := os.Hostname()
	if err != nil {
		host = "unknown host"
	}
	if fs.Arg(0) == "check" {
		return check(ctx, cfg, logger)
	}
	var room *daemon.Room
	if fs.NArg() > 0 {
		r, err := daemon.ParseRoomURL(fs.Arg(0))
		if err != nil {
			return err
		}
		room = &r
	}
	release, err := daemon.LockSupervisor(cfg.Paths.State)
	if errors.Is(err, daemon.ErrLocked) {
		return handOver(ctx, cfg, room, stderr)
	}
	if err != nil {
		return err
	}
	defer release()
	logger.Info("startup", zap.String("version", Version), zap.String("commit_hash", CommitHash),
		zap.String("build_timestamp", BuildTimestamp), zap.String("builder", Builder), zap.String("project_url", ProjectURL))
	return supervise(ctx, cfg, host, room, stderr, ring, logger)
}

// supervise runs the supervisor (003-AC6, 003-AC8): it holds the state dir's lock, migrates a
// single-room daemon's state, runs a worker per instance, and serves the admin page and the
// handoff socket.
func supervise(ctx context.Context, cfg Config, host string, room *daemon.Room, stderr io.Writer, ring *daemon.Ring, logger *zap.Logger) error {
	if _, err := daemon.Migrate(cfg.Paths, cfg.Legacy, time.Now(), os.Getenv("CLAUDE_CONFIG_DIR"), logger); err != nil {
		return fmt.Errorf("migrate the state in %s: %w", cfg.Legacy, err)
	}
	accounts, err := loadAccounts(cfg, logger)
	if err != nil {
		return err
	}
	exe, err := executable()
	if err != nil {
		return err
	}
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	insts, _ := daemon.LoadInstances(cfg.Paths) // the supervisor reports the unreadable ones
	bins := &daemon.Binaries{
		Dir: cfg.Paths.Bin(), Self: exe, Version: Version, Log: logger,
		// No client timeout: a download is bounded by its own context.
		HTTP:  &http.Client{},
		Roots: &trust.Roots{Dir: cfg.Paths.Trust(), Log: logger},
		Keys:  &daemon.DevKeys{Dir: cfg.Paths.DevKeys(), Log: logger},
	}
	bins.Init(insts)
	mon, err := openMonitor(cfg.Paths.State)
	if err != nil {
		// The page still runs, without a history.
		logger.Error("monitor_unavailable", zap.Error(err))
	} else {
		defer mon.Close() //nolint:errcheck // closed as the supervisor exits
	}
	var restart atomic.Bool
	sup := daemon.NewSupervisor(daemon.SupervisorOptions{
		Paths: cfg.Paths, Version: Version, Host: host, Accounts: accounts,
		Worker: daemon.WorkerConfig{SandboxRoot: cfg.Paths.State, LogLevel: cfg.LogLevel, Image: cfg.Image,
			Settings: cfg.Settings, Limits: cfg.Limits, CacheMB: cfg.CacheMB},
		Binary: bins.Resolve, Bins: bins, Executable: exe, GitHub: cfg.GitHub,
		Env: workerEnv(os.Environ()), Stderr: stderr, Ring: ring, Log: logger, Docker: sandbox.Docker, Monitor: mon,
		Restart:   func() { restart.Store(true); stop() },
		Autostart: autostart(exe),
	})
	// Links are printed, never logged, so they stay on this terminal.
	announce := func(link string) { _, _ = fmt.Fprintf(stderr, "\n  Admin page: %s\n\n", link) }
	admin := daemon.NewAdmin(ctx, sup, ring, stop, announce, logger)
	ln, err := admin.Listen(ctx)
	if err != nil {
		return fmt.Errorf("admin page: %w", err)
	}
	// Before the socket serves: a handed room starts a worker, which needs the running supervisor.
	sup.Start(ctx)
	hln, err := daemon.ListenHandoff(ctx, cfg.Paths.State)
	if err != nil {
		// A state dir too deep for a socket path: the page still works, a later launch says this
		// supervisor does not answer.
		logger.Warn("handoff_socket_failed", zap.Error(err))
	} else {
		go daemon.ServeHandoff(hln, func(u string) (string, error) {
			if u != "" {
				r, err := daemon.ParseRoomURL(u)
				if err != nil {
					return "", err
				}
				sup.SetRoom(ctx, r)
			}
			return admin.NewLink(), nil
		}, logger)
	}
	srv := &http.Server{Handler: admin.Handler(), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second}
	if room != nil {
		sup.SetRoom(ctx, *room)
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
	if hln != nil {
		_ = hln.Close() // no handoff while the workers stop
	}
	// The workers remove their sandboxes as they stop: exiting first would leave them running.
	sup.Wait()
	logger.Info("shutdown")
	if err == nil && restart.Load() {
		// The rooms are in the instances' state: the new binary reconnects to them on its own.
		return restartError{exe: exe}
	}
	return err
}

func loadAccounts(cfg Config, log *zap.Logger) (*daemon.Accounts, error) {
	a, err := daemon.LoadAccounts(daemon.AccountsOptions{Paths: cfg.Paths, ClaudeConfigDir: os.Getenv("CLAUDE_CONFIG_DIR"),
		Upstream: anthropic, Docker: sandbox.Docker, Log: log})
	if err != nil {
		return nil, fmt.Errorf("accounts: %w", err)
	}
	return a, nil
}

// workerEnv is the supervisor's environment minus what may carry a Claude token or name an
// account: workers never see one (003-AC14).
func workerEnv(env []string) []string {
	return slices.DeleteFunc(slices.Clone(env), func(kv string) bool {
		return strings.HasPrefix(kv, "CLAUDE_") || strings.HasPrefix(kv, "ANTHROPIC_")
	})
}

// handOver gives room to the supervisor already running on this state dir and prints its admin
// page link (003-AC6). With no room, this launch was a second supervisor: it is refused.
func handOver(ctx context.Context, cfg Config, room *daemon.Room, stderr io.Writer) error {
	u := ""
	if room != nil {
		u = room.String()
	}
	link, err := daemon.Handoff(ctx, cfg.Paths.State, u)
	if err != nil {
		return fmt.Errorf("a supervisor already runs for %s: %w", cfg.Paths.State, err)
	}
	_, _ = fmt.Fprintf(stderr, "\n  Admin page: %s\n\n", link)
	if room == nil {
		return fmt.Errorf("a supervisor already runs for %s: this one stops; its admin page is above, and `grill-room-daemon <room-url>` hands it a room", cfg.Paths.State)
	}
	_, _ = fmt.Fprintf(stderr, "grill-room-daemon: handed %s to the running supervisor\n", room)
	if cfg.Browser == "auto" {
		if err := daemon.Host().OpenURL(link); err != nil {
			_, _ = fmt.Fprintf(stderr, "grill-room-daemon: open the admin page link printed above (%v)\n", err)
		}
	}
	return nil
}

// autostart is the login item for exe. It carries the environment the supervisor needs to find
// the same state, credentials and Docker, and opens no browser at login.
func autostart(exe string) *daemon.Autostart {
	home, _ := os.UserHomeDir()
	configHome := os.Getenv("XDG_CONFIG_HOME")
	if !filepath.IsAbs(configHome) {
		configHome = filepath.Join(home, ".config")
	}
	env := []string{"GRILL_ROOM_DAEMON_BROWSER=none"}
	for _, k := range []string{"PATH", "CLAUDE_CONFIG_DIR", "GRILL_ROOM_DAEMON_STATE_DIR"} {
		v := os.Getenv(k)
		if k == "GRILL_ROOM_DAEMON_STATE_DIR" && v != "" {
			v, _ = filepath.Abs(v)
		}
		if v != "" {
			env = append(env, k+"="+v)
		}
	}
	return &daemon.Autostart{GOOS: runtime.GOOS, Home: home, ConfigHome: configHome, Exe: exe, Env: env, Run: runCommand}
}

func runCommand(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput() //nolint:gosec,noctx // fixed service managers, short-lived
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, bytes.TrimSpace(out))
	}
	return nil
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

// check reports whether this machine can host, then runs one haiku turn in a throwaway sandbox
// through the "default" account's proxy, checks its isolation and scans it for the real token.
func check(ctx context.Context, cfg Config, log *zap.Logger) error {
	ready := sandbox.Check(ctx, sandbox.Docker)
	if !ready.OK {
		return errors.New(ready.Reason)
	}
	fmt.Printf("docker: ok, %s (desktop: %v)\n", ready.Version, ready.Desktop)
	accounts, err := loadAccounts(cfg, log)
	if err != nil {
		return err
	}
	defer accounts.Close()
	tok, err := accounts.Token(ctx, "default")
	if err != nil {
		return fmt.Errorf("account default: %w", err)
	}
	fmt.Printf("account default: ok, from %s\n", tok.Origin)
	const room = "check"
	px := accounts.Access(ctx, room, map[string]string{room: "default"}, false).Proxies[room]
	if px.Error != "" {
		return fmt.Errorf("account default: %s", px.Error)
	}
	image := cfg.Image
	if image == "" {
		if image, err = sandbox.EnsureImage(ctx, sandbox.Docker, sandbox.DockerStream, "", func(_, line string) { fmt.Println("  " + line) }); err != nil {
			return fmt.Errorf("sandbox image: %w", err)
		}
	}
	fmt.Printf("sandbox image: %s\n", image)
	sb := sandbox.New(sandbox.Config{Image: image, Name: fmt.Sprintf("check-%d", time.Now().Unix()), Proxy: px.Addr, Limits: cfg.Limits},
		sandbox.Docker, log)
	defer sb.Close(ctx)
	tctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	res, err := sb.RunClaude(tctx, sandbox.Request{Prompt: "say ok", Model: "haiku", Bearer: px.Bearer})
	if err != nil {
		return fmt.Errorf("sandbox turn: %w", err)
	}
	fmt.Printf("sandbox turn: %q (session %s)\n", res.Result, res.SessionID)
	if err := sb.Isolated(ctx); err != nil {
		return fmt.Errorf("isolation: %w", err)
	}
	fmt.Println("isolation: ok, only the proxies are reachable and the egress proxy refuses Anthropic")
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

// openMonitor opens the turn history in the supervisor's state dir (003-AC27).
func openMonitor(state string) (*monitor.DB, error) {
	if err := os.MkdirAll(state, 0o700); err != nil {
		return nil, fmt.Errorf("create state dir: %w", err)
	}
	return monitor.Open(filepath.Join(state, "monitor.db"), nil)
}
