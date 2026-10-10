package sandbox

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/babs/grill-room/internal/creds"
	"github.com/babs/grill-room/internal/proxy"
)

const (
	// Workspace is the fixed cwd of every turn: the session's project slug never changes (S2).
	Workspace = "/workspace"
	home      = "/home/agent"
	relayPort = "8787"
	// Label marks every container, network and volume the daemon creates, for cleanup.
	Label = "grill-room-daemon"
)

// ErrCredentialsExpired means upstream refused the host's token: the host's Claude Code must refresh it.
var ErrCredentialsExpired = errors.New("Claude credentials expired") //nolint:revive,staticcheck // shown to users as is

// Config names the sandbox and where it sends model traffic.
type Config struct {
	Image string
	// Name is unique per daemon instance; every Docker object is named after it.
	Name string
	// Host names this machine in error messages shown in the room.
	Host     string
	Upstream *url.URL
}

// Sandbox is one long-lived container plus its relay, network and proxy.
type Sandbox struct {
	cfg   Config
	run   Runner
	src   *creds.Source
	log   *zap.Logger
	dummy string
	proxy *proxy.Proxy

	// turn serialises turns: one claude process per sandbox.
	turn    sync.Mutex
	mu      sync.Mutex
	srv     *http.Server
	started bool
}

// New prepares a sandbox; Start creates it.
func New(cfg Config, run Runner, src *creds.Source, log *zap.Logger) *Sandbox {
	dummy := "sk-ant-oat01-grill-room-dummy-" + rand.Text()
	return &Sandbox{cfg: cfg, run: run, src: src, log: log, dummy: dummy,
		proxy: proxy.New(cfg.Upstream, src, dummy, log.Named("proxy"))}
}

func (s *Sandbox) container() string { return "grill-room-" + s.cfg.Name }
func (s *Sandbox) relay() string     { return s.container() + "-relay" }
func (s *Sandbox) network() string   { return s.container() }
func (s *Sandbox) homeVol() string   { return s.container() + "-home" }
func (s *Sandbox) wsVol() string     { return s.container() + "-workspace" }

// Start brings up the proxy, the internal network, the relay and the sandbox container.
func (s *Sandbox) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return nil
	}
	ready := Check(ctx, s.run)
	if !ready.OK {
		return errors.New(ready.Reason)
	}
	// A crashed previous run with the same name left objects behind.
	s.remove(ctx)

	bindIP, gateway, err := s.proxyAddr(ctx, ready.Desktop)
	if err != nil {
		return err
	}
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", net.JoinHostPort(bindIP, "0"))
	if err != nil {
		return fmt.Errorf("proxy listen on %s: %w", bindIP, err)
	}
	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	srv := &http.Server{
		Handler:           s.proxy,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       5 * time.Minute,
		WriteTimeout:      30 * time.Minute, // long streamed turns
		IdleTimeout:       2 * time.Minute,
	}
	s.srv = srv
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.log.Error("proxy_serve_failed", zap.Error(err))
		}
	}()
	s.started = true
	s.log.Info("proxy_started", zap.String("listen", ln.Addr().String()))

	label := Label + "=" + s.cfg.Name
	hardening := []string{"--label", label, "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--read-only"}
	steps := [][]string{
		{"network", "create", "--internal", "--label", label, s.network()},
		// The relay sits on the default bridge to reach the host, and on the internal network as
		// "proxy". It forwards to one fixed address, so the sandbox gains no other route.
		append(append([]string{"run", "-d", "--name", s.relay(), "--add-host", "host.docker.internal:" + gateway}, hardening...),
			"--entrypoint", "socat", s.cfg.Image, "TCP-LISTEN:"+relayPort+",fork,reuseaddr", "TCP:host.docker.internal:"+port),
		{"network", "connect", "--alias", "proxy", s.network(), s.relay()},
		{"volume", "create", "--label", label, s.homeVol()},
		{"volume", "create", "--label", label, s.wsVol()},
		append(append([]string{"run", "-d", "--init", "--name", s.container(), "--network", s.network()}, hardening...),
			"--tmpfs", "/tmp", "--pids-limit", "512",
			"-v", s.homeVol()+":"+home, "-v", s.wsVol()+":"+Workspace, "-w", Workspace,
			"-e", "ANTHROPIC_BASE_URL=http://proxy:"+relayPort,
			"-e", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
			"-e", "ENABLE_CLAUDEAI_MCP_SERVERS=false",
			s.cfg.Image),
	}
	for _, args := range steps {
		if _, err := s.run(ctx, nil, args...); err != nil {
			s.closeLocked(ctx)
			return fmt.Errorf("start sandbox: %w", err)
		}
	}
	s.log.Info("sandbox_started", zap.String("container", s.container()), zap.String("image", s.cfg.Image))
	return nil
}

// proxyAddr picks where the proxy listens and the address the relay dials for it. Docker Desktop
// forwards host-gateway to the host's loopback; a native engine reaches the host on its bridge.
func (s *Sandbox) proxyAddr(ctx context.Context, desktop bool) (bindIP, gateway string, err error) {
	if desktop {
		return "127.0.0.1", "host-gateway", nil
	}
	out, err := s.run(ctx, nil, "network", "inspect", "bridge", "--format", "{{(index .IPAM.Config 0).Gateway}}")
	if err != nil {
		return "", "", fmt.Errorf("find the docker bridge gateway: %w", err)
	}
	ip := strings.TrimSpace(string(out))
	if net.ParseIP(ip) == nil {
		return "", "", fmt.Errorf("docker bridge gateway %q is not an IP", ip)
	}
	return ip, ip, nil
}

// Close removes every Docker object of the sandbox, volumes included, and stops the proxy.
func (s *Sandbox) Close(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeLocked(ctx)
}

func (s *Sandbox) closeLocked(ctx context.Context) {
	// The caller's context may be the turn's, already expired: cleanup must still run.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	s.remove(ctx)
	if s.srv != nil {
		_ = s.srv.Close()
		s.srv = nil
	}
	s.started = false
}

// remove deletes the sandbox's objects; each may be missing, so errors are expected and ignored.
func (s *Sandbox) remove(ctx context.Context) {
	for _, args := range [][]string{
		{"rm", "-f", "-v", s.container(), s.relay()},
		{"network", "rm", s.network()},
		{"volume", "rm", "-f", s.homeVol(), s.wsVol()},
	} {
		_, _ = s.run(ctx, nil, args...)
	}
}

// Request is one claude -p invocation.
type Request struct {
	Prompt string
	Model  string
	// Resume is the session id to continue; empty starts a new session.
	Resume string
	// Args go after the prompt: --allowedTools is variadic and would swallow a prompt after it (S2).
	Args []string
}

// Result is claude's --output-format json answer.
type Result struct {
	SessionID string `json:"session_id"`
	Result    string `json:"result"`
	IsError   bool   `json:"is_error"`
	Subtype   string `json:"subtype"`
}

// RunClaude runs one turn in the sandbox. ErrCredentialsExpired (with the host name) means the
// host's token is expired or refused; the next turn works once the host's Claude Code refreshed it.
func (s *Sandbox) RunClaude(ctx context.Context, req Request) (*Result, error) {
	s.turn.Lock()
	defer s.turn.Unlock()
	expired := fmt.Errorf("%w on %s", ErrCredentialsExpired, s.cfg.Host)
	tok, err := s.src.Get(ctx)
	if err != nil {
		return nil, fmt.Errorf("Claude credentials unavailable on %s: %w", s.cfg.Host, err) //nolint:staticcheck // shown to users as is
	}
	if tok.Expired(time.Now()) {
		return nil, expired
	}
	if err := s.Start(ctx); err != nil {
		return nil, err
	}
	if err := s.writeStubs(ctx); err != nil {
		return nil, err
	}
	args := []string{"exec", s.container(), "claude", "-p", req.Prompt, "--output-format", "json"}
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	if req.Resume != "" {
		args = append(args, "--resume", req.Resume)
	}
	args = append(args, req.Args...)
	before := s.proxy.Unauthorized()
	out, runErr := s.run(ctx, nil, args...)
	if ctx.Err() != nil {
		// Killing the docker exec client leaves claude running in the container.
		kctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		_, _ = s.run(kctx, nil, "exec", s.container(), "pkill", "-x", "claude")
		cancel()
		return nil, fmt.Errorf("turn cancelled: %w", ctx.Err())
	}
	var res Result
	jsonErr := json.Unmarshal(out, &res)
	if runErr != nil || jsonErr != nil || res.IsError {
		if s.proxy.Unauthorized() > before {
			return nil, expired
		}
		if runErr != nil {
			return nil, fmt.Errorf("claude: %w", runErr)
		}
		if jsonErr != nil {
			return nil, fmt.Errorf("claude output: %w", jsonErr)
		}
		return &res, fmt.Errorf("claude: %s: %s", res.Subtype, res.Result)
	}
	return &res, nil
}

// writeStubs regenerates, before every turn, the dummy credentials (Claude Code wipes them after a
// refused refresh) and the oauthAccount stub, which skips the profile fetch only while under 24 h old.
func (s *Sandbox) writeStubs(ctx context.Context) error {
	nowMS := time.Now().UnixMilli()
	files := []struct {
		name string
		v    any
	}{
		{".claude/.credentials.json", map[string]any{"claudeAiOauth": map[string]any{
			"accessToken": s.dummy, "refreshToken": "grill-room-dummy-refresh",
			"expiresAt": time.Now().AddDate(10, 0, 0).UnixMilli(), "subscriptionType": "max",
			"scopes": []string{"user:inference", "user:profile", "user:sessions:claude_code"},
		}}},
		// Fake identity: keeps the host's account out of uploaded sessions (S2).
		{".claude.json", map[string]any{"oauthAccount": map[string]any{
			"accountUuid":           "00000000-0000-4000-8000-000000000001",
			"emailAddress":          "agent@grill-room.invalid",
			"organizationUuid":      "00000000-0000-4000-8000-000000000002",
			"billingType":           "stripe_subscription",
			"accountCreatedAt":      "2025-01-01T00:00:00Z",
			"subscriptionCreatedAt": "2025-01-01T00:00:00Z",
			"ccOnboardingFlags":     map[string]any{},
			"profileFetchedAt":      nowMS,
		}}},
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: ".claude/", Mode: 0o700, ModTime: time.Now()}); err != nil {
		return fmt.Errorf("stubs: %w", err)
	}
	for _, f := range files {
		b, err := json.Marshal(f.v)
		if err != nil {
			return fmt.Errorf("stubs: %w", err)
		}
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o600, Size: int64(len(b)), ModTime: time.Now()}); err != nil {
			return fmt.Errorf("stubs: %w", err)
		}
		if _, err := tw.Write(b); err != nil {
			return fmt.Errorf("stubs: %w", err)
		}
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("stubs: %w", err)
	}
	// tar runs as the agent user, so the files end up owned by it.
	if _, err := s.run(ctx, &buf, "exec", "-i", s.container(), "tar", "-x", "-C", home); err != nil {
		return fmt.Errorf("write stubs: %w", err)
	}
	return nil
}

var sessionIDRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// ReadSession returns the session jsonl bytes past offset (D23: the file is append-only).
func (s *Sandbox) ReadSession(ctx context.Context, sessionID string, offset int64) ([]byte, error) {
	if !sessionIDRe.MatchString(sessionID) {
		return nil, fmt.Errorf("session id %q is not a UUID", sessionID)
	}
	if offset < 0 {
		return nil, fmt.Errorf("negative offset %d", offset)
	}
	path := home + "/.claude/projects/" + strings.ReplaceAll(Workspace, "/", "-") + "/" + sessionID + ".jsonl"
	out, err := s.run(ctx, nil, "exec", s.container(), "tail", "-c", "+"+strconv.FormatInt(offset+1, 10), path)
	if err != nil {
		return nil, fmt.Errorf("read session %s: %w", sessionID, err)
	}
	return out, nil
}
