// Command grill-room is the room server: rooms, invite links, participants, a live event stream
// and the web UI.
//
// Logger, admin token and server wiring are derived from github.com/babs/airc main.go at commit
// b7afc63.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"golang.org/x/sync/errgroup"
	"golang.org/x/term"

	"github.com/babs/grill-room/internal/bus"
	"github.com/babs/grill-room/internal/convert"
	"github.com/babs/grill-room/internal/store"
	"github.com/babs/grill-room/internal/web"
)

// Build metadata, injected with -ldflags -X.
var (
	Version        = "v0.0.0"
	CommitHash     = "0000000"
	BuildTimestamp = "1970-01-01T00:00:00"
	Builder        = "unknown"
	ProjectURL     = "https://github.com/babs/grill-room"
)

// adminTokenSetting holds the admin token hash. Delete the row to have a new token generated.
const adminTokenSetting = "admin_token_sha256" //nolint:gosec // a settings key, not a credential

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// Config is the server configuration, read from the environment.
type Config struct {
	Addr        string
	MetricsAddr string
	DB          string
	PublicURL   string
	AdminToken  string
	LogLevel    string
	LogFormat   string
	// MaxUploadMB and MaxRoomMB bound one upload and all of a room's uploads (D31).
	MaxUploadMB   int64
	MaxRoomMB     int64
	Pandoc        string
	PandocTimeout time.Duration
}

func loadConfig() (Config, error) {
	addr := getEnv("GRILL_ROOM_ADDR", "127.0.0.1:7680")
	maxUpload, err1 := strconv.ParseInt(getEnv("GRILL_ROOM_MAX_UPLOAD_MB", "20"), 10, 64)
	maxRoom, err2 := strconv.ParseInt(getEnv("GRILL_ROOM_MAX_ROOM_MB", "200"), 10, 64)
	timeout, err3 := time.ParseDuration(getEnv("GRILL_ROOM_PANDOC_TIMEOUT", "60s"))
	return Config{
		MaxUploadMB:   maxUpload,
		MaxRoomMB:     maxRoom,
		Pandoc:        getEnv("GRILL_ROOM_PANDOC", "pandoc"),
		PandocTimeout: timeout,
		Addr:          addr,
		MetricsAddr:   getEnv("GRILL_ROOM_METRICS_ADDR", "127.0.0.1:7681"),
		DB:            getEnv("GRILL_ROOM_DB", "grill-room.db"),
		PublicURL:     getEnv("GRILL_ROOM_URL", "http://"+addr),
		AdminToken:    os.Getenv("GRILL_ROOM_ADMIN_TOKEN"),
		LogLevel:      getEnv("LOG_LEVEL", "info"),
		LogFormat:     getEnv("LOG_FORMAT", "auto"),
	}, errors.Join(err1, err2, err3)
}

// Validate rejects a configuration the server cannot run with.
func (c Config) Validate() error {
	if c.Addr == c.MetricsAddr {
		return errors.New("GRILL_ROOM_ADDR and GRILL_ROOM_METRICS_ADDR must differ")
	}
	if c.DB == "" {
		return errors.New("GRILL_ROOM_DB is empty")
	}
	if c.MaxUploadMB < 1 || c.MaxRoomMB < c.MaxUploadMB || c.MaxRoomMB > 1<<20 {
		return fmt.Errorf("GRILL_ROOM_MAX_UPLOAD_MB %d, GRILL_ROOM_MAX_ROOM_MB %d: want 1 <= upload <= room <= %d", c.MaxUploadMB, c.MaxRoomMB, 1<<20)
	}
	if c.PandocTimeout <= 0 {
		return fmt.Errorf("GRILL_ROOM_PANDOC_TIMEOUT %s: want a positive duration", c.PandocTimeout)
	}
	if _, err := zapcore.ParseLevel(c.LogLevel); err != nil {
		return fmt.Errorf("LOG_LEVEL: %w", err)
	}
	switch c.LogFormat {
	case "auto", "console", "json":
	default:
		return fmt.Errorf("LOG_FORMAT %q: want auto, console or json", c.LogFormat)
	}
	return nil
}

func newLogger(c Config) (*zap.Logger, error) {
	level, err := zapcore.ParseLevel(c.LogLevel)
	if err != nil {
		return nil, err
	}
	var cfg zap.Config
	if c.LogFormat == "console" || (c.LogFormat == "auto" && term.IsTerminal(int(os.Stdout.Fd()))) { //nolint:gosec // fd fits an int on every supported platform
		cfg = zap.NewDevelopmentConfig()
		cfg.EncoderConfig.EncodeLevel = zapcore.CapitalColorLevelEncoder
	} else {
		cfg = zap.NewProductionConfig()
	}
	cfg.Level = zap.NewAtomicLevelAt(level)
	cfg.EncoderConfig.TimeKey = "timestamp"
	cfg.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
	return cfg.Build()
}

// adminTokenHash returns the admin token hash: from the environment, else from the database, else
// a new token is generated, its hash kept, and the token returned so it can be shown once.
func adminTokenHash(ctx context.Context, st *store.Store, fromEnv string) (hash, generated string, err error) {
	if fromEnv != "" {
		return web.HashToken(fromEnv), "", nil
	}
	hash, err = st.Setting(ctx, adminTokenSetting)
	if err != nil || hash != "" {
		return hash, "", err
	}
	generated = web.NewToken()
	hash = web.HashToken(generated)
	if err := st.SetSetting(ctx, adminTokenSetting, hash); err != nil {
		return "", "", err
	}
	return hash, generated, nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "grill-room: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := loadConfig()
	if err == nil {
		err = cfg.Validate()
	}
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	logger, err := newLogger(cfg)
	if err != nil {
		return fmt.Errorf("init logger: %w", err)
	}
	defer logger.Sync() //nolint:errcheck // stdout sync is best-effort
	logger.Info("startup", zap.String("version", Version), zap.String("commit_hash", CommitHash),
		zap.String("build_timestamp", BuildTimestamp), zap.String("builder", Builder), zap.String("project_url", ProjectURL))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(cfg.DB)
	if err != nil {
		return err
	}
	defer func() {
		if err := st.Close(); err != nil {
			logger.Error("store_close_failed", zap.Error(err))
		}
	}()
	hash, generated, err := adminTokenHash(ctx, st, cfg.AdminToken)
	if err != nil {
		return fmt.Errorf("admin token: %w", err)
	}
	if generated != "" {
		// Shown once: only its hash is stored.
		logger.Warn("admin_token_generated", zap.String("token", generated),
			zap.String("hint", "shown once; set GRILL_ROOM_ADMIN_TOKEN to pin one"))
	}

	if _, err := exec.LookPath(cfg.Pandoc); err != nil {
		// Not fatal: only office uploads need it, and they answer 503 until it is installed.
		logger.Warn("pandoc_missing", zap.String("pandoc", cfg.Pandoc), zap.Error(err))
	}

	b := bus.New(st)
	srv := web.New(b, hash, cfg.PublicURL, web.Options{
		MaxUploadBytes: cfg.MaxUploadMB << 20,
		MaxRoomBytes:   cfg.MaxRoomMB << 20,
		ToMarkdown:     convert.NewPandoc(cfg.Pandoc, cfg.PandocTimeout).ToMarkdown,
	}, logger)
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		prometheus.NewCounterFunc(prometheus.CounterOpts{Name: "grill_room_bus_dropped_total", Help: "Room change signals lost to full subscriber buffers."},
			func() float64 { return float64(b.Dropped()) }),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "grill_room_event_streams", Help: "Open SSE event streams."},
			func() float64 { return float64(srv.Streams()) }),
	)

	appSrv := &http.Server{
		Addr:    cfg.Addr,
		Handler: srv.Handler(),
		// No Read/WriteTimeout: they would cut the long-lived event stream.
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	metricsMux := http.NewServeMux()
	metricsMux.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	metricsSrv := &http.Server{
		Addr:              cfg.MetricsAddr,
		Handler:           metricsMux,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return web.Serve(gctx, appSrv, logger) })
	g.Go(func() error { return web.Serve(gctx, metricsSrv, logger) })
	logger.Info("ready", zap.String("url", cfg.PublicURL))
	err = g.Wait()
	logger.Info("shutdown")
	return err
}
