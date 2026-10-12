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
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
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
	"github.com/babs/grill-room/internal/livekit"
	"github.com/babs/grill-room/internal/mail"
	"github.com/babs/grill-room/internal/sandbox"
	"github.com/babs/grill-room/internal/store"
	"github.com/babs/grill-room/internal/turn"
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
	MaxUploadMB int64
	MaxRoomMB   int64
	// MaxAttachmentMB bounds one image on an annotation message; attachments count in MaxRoomMB.
	MaxAttachmentMB int64
	// MaxImportMB bounds an imported room archive once uncompressed (004-AC14).
	MaxImportMB   int64
	Pandoc        string
	PandocTimeout time.Duration
	// LiveKitURL is the signalling URL browsers use (wss://); empty turns voice off.
	LiveKitURL string
	// LiveKitAPIURL is LiveKit's HTTP API as this server reaches it; defaults from LiveKitURL.
	LiveKitAPIURL    string
	LiveKitAPIKey    string
	LiveKitAPISecret string
	// STTToken is the STT worker's service token; empty refuses every worker.
	STTToken string
	// TurnTimeout bounds one agent turn.
	TurnTimeout time.Duration
	// DaemonDir holds the daemon release `make release` writes; empty offers no download.
	DaemonDir string
	// SandboxImage is the digest-pinned sandbox image advertised to daemons; empty lets them build it.
	SandboxImage string
	// SMTP sends magic links; an empty Addr turns email sign-in off.
	SMTP mail.SMTP
	// MagicLinkTTL is how long an emailed sign-in link works.
	MagicLinkTTL time.Duration
	// AuditRetention is how long the audit log keeps an entry.
	AuditRetention time.Duration
	// ClientRetention is how long a participant's IP and user agent are kept.
	ClientRetention time.Duration
	// TrustedProxies are the reverse proxies whose X-Forwarded-For names the client.
	TrustedProxies []netip.Prefix
	// PushHosts replaces the push services Web Push subscriptions may name; nil keeps the default.
	PushHosts []string
}

func loadConfig() (Config, error) {
	addr := getEnv("GRILL_ROOM_ADDR", "127.0.0.1:7680")
	maxUpload, err1 := strconv.ParseInt(getEnv("GRILL_ROOM_MAX_UPLOAD_MB", "20"), 10, 64)
	maxRoom, err2 := strconv.ParseInt(getEnv("GRILL_ROOM_MAX_ROOM_MB", "200"), 10, 64)
	timeout, err3 := time.ParseDuration(getEnv("GRILL_ROOM_PANDOC_TIMEOUT", "60s"))
	maxAttachment, err4 := strconv.ParseInt(getEnv("GRILL_ROOM_MAX_ATTACHMENT_MB", "5"), 10, 64)
	turnTimeout, err5 := time.ParseDuration(getEnv("GRILL_ROOM_TURN_TIMEOUT", "15m"))
	magicTTL, err6 := time.ParseDuration(getEnv("GRILL_ROOM_MAGIC_LINK_TTL", web.DefaultMagicLinkTTL.String()))
	auditRetention, err7 := time.ParseDuration(getEnv("GRILL_ROOM_AUDIT_RETENTION", web.DefaultAuditRetention.String()))
	clientRetention, err9 := time.ParseDuration(getEnv("GRILL_ROOM_CLIENT_RETENTION", web.DefaultClientRetention.String()))
	proxies, err10 := web.ParseTrustedProxies(os.Getenv("GRILL_ROOM_TRUSTED_PROXIES"))
	if err10 != nil {
		err10 = fmt.Errorf("GRILL_ROOM_TRUSTED_PROXIES: %w", err10)
	}
	maxImport, err8 := strconv.ParseInt(getEnv("GRILL_ROOM_MAX_IMPORT_MB", strconv.Itoa(web.DefaultMaxImportBytes>>20)), 10, 64)
	return Config{
		SMTP: mail.SMTP{
			Addr:     os.Getenv("GRILL_ROOM_SMTP_ADDR"),
			From:     os.Getenv("GRILL_ROOM_SMTP_FROM"),
			Username: os.Getenv("GRILL_ROOM_SMTP_USERNAME"),
			Password: os.Getenv("GRILL_ROOM_SMTP_PASSWORD"),
		},
		MagicLinkTTL:    magicTTL,
		AuditRetention:  auditRetention,
		ClientRetention: clientRetention,
		TrustedProxies:  proxies,
		PushHosts:       pushHosts(os.Getenv("GRILL_ROOM_PUSH_HOSTS")),
		TurnTimeout:     turnTimeout,
		MaxUploadMB:     maxUpload,
		MaxRoomMB:       maxRoom,
		MaxAttachmentMB: maxAttachment,
		MaxImportMB:     maxImport,
		Pandoc:          getEnv("GRILL_ROOM_PANDOC", "pandoc"),
		PandocTimeout:   timeout,
		Addr:            addr,
		MetricsAddr:     getEnv("GRILL_ROOM_METRICS_ADDR", "127.0.0.1:7681"),
		DB:              getEnv("GRILL_ROOM_DB", "grill-room.db"),
		PublicURL:       getEnv("GRILL_ROOM_URL", "http://"+addr),
		AdminToken:      os.Getenv("GRILL_ROOM_ADMIN_TOKEN"),
		LiveKitURL:      os.Getenv("GRILL_ROOM_LIVEKIT_URL"),
		LiveKitAPIURL: getEnv("GRILL_ROOM_LIVEKIT_API_URL",
			strings.Replace(os.Getenv("GRILL_ROOM_LIVEKIT_URL"), "ws", "http", 1)),
		LiveKitAPIKey:    os.Getenv("GRILL_ROOM_LIVEKIT_API_KEY"),
		LiveKitAPISecret: os.Getenv("GRILL_ROOM_LIVEKIT_API_SECRET"),
		STTToken:         os.Getenv("GRILL_ROOM_STT_TOKEN"),
		LogLevel:         getEnv("LOG_LEVEL", "info"),
		LogFormat:        getEnv("LOG_FORMAT", "auto"),
		DaemonDir:        os.Getenv("GRILL_ROOM_DAEMON_DIR"),
		SandboxImage:     os.Getenv("GRILL_ROOM_SANDBOX_IMAGE"),
	}, errors.Join(err1, err2, err3, err4, err5, err6, err7, err8, err9, err10)
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
	if c.MaxImportMB < c.MaxRoomMB || c.MaxImportMB > 1<<20 {
		return fmt.Errorf("GRILL_ROOM_MAX_IMPORT_MB %d: want room (%d) <= import <= %d", c.MaxImportMB, c.MaxRoomMB, 1<<20)
	}
	if c.MaxAttachmentMB < 1 || c.MaxAttachmentMB > c.MaxRoomMB {
		return fmt.Errorf("GRILL_ROOM_MAX_ATTACHMENT_MB %d: want 1 <= attachment <= room (%d)", c.MaxAttachmentMB, c.MaxRoomMB)
	}
	if c.TurnTimeout < time.Minute || c.TurnTimeout > 24*time.Hour {
		return fmt.Errorf("GRILL_ROOM_TURN_TIMEOUT %s: want 1m to 24h", c.TurnTimeout)
	}
	if c.PandocTimeout <= 0 {
		return fmt.Errorf("GRILL_ROOM_PANDOC_TIMEOUT %s: want a positive duration", c.PandocTimeout)
	}
	if err := c.validateVoice(); err != nil {
		return err
	}
	if c.SMTP.Addr != "" {
		if err := c.SMTP.Validate(); err != nil {
			return fmt.Errorf("GRILL_ROOM_SMTP_*: %w", err)
		}
	} else if c.SMTP != (mail.SMTP{}) {
		return errors.New("GRILL_ROOM_SMTP_FROM, _USERNAME and _PASSWORD need GRILL_ROOM_SMTP_ADDR")
	}
	if c.MagicLinkTTL < time.Second || c.MagicLinkTTL > time.Hour {
		return fmt.Errorf("GRILL_ROOM_MAGIC_LINK_TTL %s: want 1s to 1h", c.MagicLinkTTL)
	}
	if c.AuditRetention < 24*time.Hour {
		return fmt.Errorf("GRILL_ROOM_AUDIT_RETENTION %s: want 24h or more", c.AuditRetention)
	}
	if c.ClientRetention < 24*time.Hour {
		return fmt.Errorf("GRILL_ROOM_CLIENT_RETENTION %s: want 24h or more", c.ClientRetention)
	}
	// Security: a tag can be moved to another image after review; a digest cannot.
	if c.SandboxImage != "" && !sandbox.PinnedImage(c.SandboxImage) {
		return fmt.Errorf("GRILL_ROOM_SANDBOX_IMAGE %q: want a digest-pinned reference, <repository>[:tag]@sha256:<64 hex>", c.SandboxImage)
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

func (c Config) validateVoice() error {
	if c.LiveKitURL == "" {
		if c.STTToken != "" {
			return errors.New("GRILL_ROOM_STT_TOKEN needs GRILL_ROOM_LIVEKIT_URL")
		}
		return nil
	}
	if u, err := url.Parse(c.LiveKitURL); err != nil || (u.Scheme != "wss" && u.Scheme != "ws") || u.Host == "" {
		return fmt.Errorf("GRILL_ROOM_LIVEKIT_URL %q: want ws:// or wss://<host>", c.LiveKitURL)
	}
	if u, err := url.Parse(c.LiveKitAPIURL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return fmt.Errorf("GRILL_ROOM_LIVEKIT_API_URL %q: want http:// or https://<host>", c.LiveKitAPIURL)
	}
	if c.LiveKitAPIKey == "" || len(c.LiveKitAPISecret) < livekit.MinSecretLen {
		return fmt.Errorf("GRILL_ROOM_LIVEKIT_API_KEY must be set and GRILL_ROOM_LIVEKIT_API_SECRET hold %d+ characters", livekit.MinSecretLen)
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

// sendMail returns the magic link sender, nil when email sign-in is off.
func (c Config) sendMail() func(ctx context.Context, to, subject, body string) error {
	if c.SMTP.Addr == "" {
		return nil
	}
	return c.SMTP.Send
}

// voice returns the LiveKit settings, nil when voice is off.
func (c Config) voice() *web.Voice {
	if c.LiveKitURL == "" {
		return nil
	}
	v := &web.Voice{
		URL: c.LiveKitURL, Key: c.LiveKitAPIKey, Secret: c.LiveKitAPISecret,
		API: &livekit.Client{URL: c.LiveKitAPIURL, Key: c.LiveKitAPIKey, Secret: c.LiveKitAPISecret, HTTP: &http.Client{Timeout: 5 * time.Second}},
	}
	if c.STTToken != "" {
		v.STTTokenHash = web.HashToken(c.STTToken)
	}
	return v
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

	// No daemon is attached yet: a turn left running by the previous process can never finish.
	if rooms, err := st.FailRunningTurns(ctx, "the room server restarted"); err != nil {
		return fmt.Errorf("fail running turns: %w", err)
	} else if len(rooms) > 0 {
		logger.Warn("running_turns_failed", zap.Strings("rooms", rooms))
	}

	var release *web.Release
	if cfg.DaemonDir != "" {
		if release, err = web.LoadRelease(cfg.DaemonDir); err != nil {
			return err
		}
		logger.Info("daemon_release", zap.String("version", release.Version))
	} else {
		logger.Info("daemon_release_disabled", zap.String("hint", "set GRILL_ROOM_DAEMON_DIR to offer the daemon download"))
	}

	b := bus.New(st)
	srv := web.New(b, hash, cfg.PublicURL, web.Options{
		TurnTimeout:        cfg.TurnTimeout,
		MaxUploadBytes:     cfg.MaxUploadMB << 20,
		MaxRoomBytes:       cfg.MaxRoomMB << 20,
		MaxAttachmentBytes: cfg.MaxAttachmentMB << 20,
		MaxImportBytes:     cfg.MaxImportMB << 20,
		ToMarkdown:         convert.NewPandoc(cfg.Pandoc, cfg.PandocTimeout).ToMarkdown,
		Voice:              cfg.voice(),
		Release:            release,
		SandboxImage:       cfg.SandboxImage,
		SendMail:           cfg.sendMail(),
		MagicLinkTTL:       cfg.MagicLinkTTL,
		AuditRetention:     cfg.AuditRetention,
		ClientRetention:    cfg.ClientRetention,
		TrustedProxies:     cfg.TrustedProxies,
		PushHosts:          cfg.PushHosts,
		Version:            Version,
		DBPath:             cfg.DB,
	}, logger)
	if cfg.SMTP.Addr == "" {
		logger.Info("email_signin_disabled", zap.String("hint", "set GRILL_ROOM_SMTP_ADDR and GRILL_ROOM_SMTP_FROM to send magic links"))
	}
	if cfg.LiveKitURL == "" {
		logger.Info("voice_disabled", zap.String("hint", "set GRILL_ROOM_LIVEKIT_URL to enable audio"))
	}
	reg := newRegistry(b, srv)

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
	g.Go(func() error { srv.PurgeAudit(gctx); return nil })
	g.Go(func() error { srv.PushLoop(gctx); return nil })
	logger.Info("ready", zap.String("url", cfg.PublicURL))
	err = g.Wait()
	logger.Info("shutdown")
	return err
}

// pushHosts splits a comma-separated list of host suffixes; nil when there is none.
func pushHosts(v string) []string {
	var out []string
	for _, h := range strings.Split(v, ",") {
		if h = strings.ToLower(strings.TrimSpace(h)); h != "" {
			out = append(out, h)
		}
	}
	return out
}

// newRegistry holds the process metrics and the room server's own.
func newRegistry(b *bus.Bus, srv *web.Server) *prometheus.Registry {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		prometheus.NewCounterFunc(prometheus.CounterOpts{Name: "grill_room_bus_dropped_total", Help: "Room change signals lost to full subscriber buffers."},
			func() float64 { return float64(b.Dropped()) }),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "grill_room_event_streams", Help: "Open SSE event streams."},
			func() float64 { return float64(srv.Streams()) }),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "grill_room_stt_workers", Help: "STT workers with a recent heartbeat."},
			func() float64 { return float64(srv.STTWorkers()) }),
	)
	for _, reason := range turn.FallbackReasons {
		reg.MustRegister(prometheus.NewCounterFunc(prometheus.CounterOpts{Name: "grill_room_workspace_fallbacks_total",
			Help: "Full workspace fetches by daemons that keep a workspace cache.", ConstLabels: prometheus.Labels{"reason": reason}},
			func() float64 { return float64(srv.WorkspaceFallbacks(reason)) }))
	}
	for _, outcome := range web.ConflictOutcomes {
		reg.MustRegister(prometheus.NewCounterFunc(prometheus.CounterOpts{Name: "grill_room_turn_conflicts_total",
			Help: "Turn results rejected because a document they edit changed during the turn.", ConstLabels: prometheus.Labels{"outcome": outcome}},
			func() float64 { return float64(srv.TurnConflicts(outcome)) }))
	}
	return reg
}
