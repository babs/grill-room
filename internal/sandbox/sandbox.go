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
	"path"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/babs/grill-room/internal/turn"
)

const (
	// Workspace is the fixed cwd of every turn: the session's project slug never changes (S2).
	Workspace = "/workspace"
	home      = "/home/agent"
	relayPort = "8787"
	noProxy   = "proxy,localhost,127.0.0.1"
	// Label marks every container, network and volume the daemon creates, for cleanup.
	Label = "grill-room-daemon"
)

// Config names the sandbox and where it sends model traffic.
type Config struct {
	Image string
	// Name is unique per daemon instance; every Docker object is named after it.
	Name string
	// Proxy is the address the relay dials for the account's bearer-swap proxy, "<gateway>:<port>"
	// as HostAddr gives the gateway. The proxy runs in the supervisor, outside this process (003-AC14).
	Proxy string
	// Limits caps the sandbox and its egress proxy; an empty value leaves Docker's default.
	Limits Limits
	// Labels, "key=value", mark every Docker object besides Label, for RemoveLabelled.
	Labels []string
}

// Limits are docker run --cpus and --memory values.
type Limits struct {
	CPUs         string `json:"cpus"`
	Memory       string `json:"memory"`
	EgressCPUs   string `json:"egress_cpus"`
	EgressMemory string `json:"egress_memory"`
}

// Docker's --cpus and --memory syntax; the values become docker arguments.
var (
	cpusRe   = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`)
	memoryRe = regexp.MustCompile(`^[0-9]+[bkmgBKMG]?$`)
)

// ValidCPUs reports a --cpus value: a number like 2 or 0.5, 0 for no limit.
func ValidCPUs(v string) bool { return cpusRe.MatchString(v) }

// ValidMemory reports a --memory value: a size like 4g or 256m, 0 for no limit.
func ValidMemory(v string) bool { return memoryRe.MatchString(v) }

// Validate rejects a value docker would not take; empty values leave Docker's default.
func (l Limits) Validate() error {
	for name, v := range map[string]string{"sandbox CPUs": l.CPUs, "egress proxy CPUs": l.EgressCPUs} {
		if v != "" && !ValidCPUs(v) {
			return fmt.Errorf("%s %q: want a number of CPUs like 2 or 0.5, 0 for no limit", name, v)
		}
	}
	for name, v := range map[string]string{"sandbox memory": l.Memory, "egress proxy memory": l.EgressMemory} {
		if v != "" && !ValidMemory(v) {
			return fmt.Errorf("%s %q: want a size like 4g or 256m, 0 for no limit", name, v)
		}
	}
	return nil
}

// limitFlags turns a cpus and memory pair into docker run flags.
func limitFlags(cpus, memory string) []string {
	var f []string
	if cpus != "" {
		f = append(f, "--cpus", cpus)
	}
	if memory != "" {
		// Swap equal to memory: a sandbox past its limit is killed rather than paging the host.
		f = append(f, "--memory", memory, "--memory-swap", memory)
	}
	return f
}

// Sandbox is one long-lived container plus its relay, network and egress proxy.
type Sandbox struct {
	cfg Config
	run Runner
	log *zap.Logger

	// turn serialises turns: one claude process per sandbox.
	turn    sync.Mutex
	mu      sync.Mutex
	started bool
	// allow is the egress allowlist the proxy runs with.
	allow []string
}

// New prepares a sandbox; Start creates it.
func New(cfg Config, run Runner, log *zap.Logger) *Sandbox {
	return &Sandbox{cfg: cfg, run: run, log: log, allow: DefaultAllowlist}
}

// NewBearer returns a dummy bearer for a sandbox: the bearer-swap proxy knows it, upstream never sees it.
func NewBearer() string { return "sk-ant-oat01-grill-room-dummy-" + rand.Text() }

func (s *Sandbox) container() string { return "grill-room-" + s.cfg.Name }
func (s *Sandbox) relay() string     { return s.container() + "-relay" }
func (s *Sandbox) network() string   { return s.container() }
func (s *Sandbox) egress() string    { return s.container() + "-egress" }
func (s *Sandbox) homeVol() string   { return s.container() + "-home" }
func (s *Sandbox) wsVol() string     { return s.container() + "-workspace" }

// Start brings up the internal network, the relay to the account's proxy, the egress proxy and
// the sandbox container.
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
	gateway, port, err := net.SplitHostPort(s.cfg.Proxy)
	if err != nil {
		return fmt.Errorf("no Claude account proxy for this sandbox (%q): %w", s.cfg.Proxy, err)
	}
	// A crashed previous run with the same name left objects behind.
	s.remove(ctx)
	s.started = true

	labels := []string{"--label", Label + "=" + s.cfg.Name}
	for _, l := range s.cfg.Labels {
		labels = append(labels, "--label", l)
	}
	hardening := append(slices.Clone(labels), "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--read-only")
	steps := [][]string{
		append(append([]string{"network", "create", "--internal"}, labels...), s.network()),
		// The relay sits on the default bridge to reach the host, and on the internal network as
		// "proxy". It forwards to one fixed address, so the sandbox gains no other route.
		append(append([]string{"run", "-d", "--name", s.relay(), "--add-host", "host.docker.internal:" + gateway}, hardening...),
			"--entrypoint", "socat", s.cfg.Image, "TCP-LISTEN:"+relayPort+",fork,reuseaddr", "TCP:host.docker.internal:"+port),
		{"network", "connect", "--alias", "proxy", s.network(), s.relay()},
		// The egress proxy sits on its own bridge to reach the allowlist, and on the internal
		// network as "egress" (002-AC34).
		append(append([]string{"network", "create"}, labels...), s.egress()),
		// The local log driver is bounded and readable by docker logs, which the denied list reads.
		append(append(append([]string{"run", "-d", "--name", s.egress(), "--network", s.egress(), "--log-driver", "local"}, hardening...),
			limitFlags(s.cfg.Limits.EgressCPUs, s.cfg.Limits.EgressMemory)...),
			"--tmpfs", "/tmp", "--pids-limit", "64", "-e", "GRILL_SQUID_CONF="+squidConf(s.allow),
			"--entrypoint", "sh", s.cfg.Image, "-c", `printf '%s' "$GRILL_SQUID_CONF" > /tmp/squid.conf && exec squid -N -f /tmp/squid.conf`),
		{"network", "connect", "--alias", "egress", s.network(), s.egress()},
		append(append([]string{"volume", "create"}, labels...), s.homeVol()),
		append(append([]string{"volume", "create"}, labels...), s.wsVol()),
		append(append(append([]string{"run", "-d", "--init", "--name", s.container(), "--network", s.network()}, hardening...),
			limitFlags(s.cfg.Limits.CPUs, s.cfg.Limits.Memory)...),
			"--tmpfs", "/tmp", "--pids-limit", "512",
			"-v", s.homeVol()+":"+home, "-v", s.wsVol()+":"+Workspace, "-w", Workspace,
			"-e", "ANTHROPIC_BASE_URL=http://proxy:"+relayPort,
			"-e", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
			"-e", "ENABLE_CLAUDEAI_MCP_SERVERS=false",
			// Tools go out through the egress proxy; Claude Code's own traffic goes to the
			// bearer-swap proxy, never through squid (002-AC35).
			"-e", "HTTP_PROXY="+egressURL, "-e", "HTTPS_PROXY="+egressURL,
			"-e", "http_proxy="+egressURL, "-e", "https_proxy="+egressURL,
			"-e", "NO_PROXY="+noProxy, "-e", "no_proxy="+noProxy,
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

// HostAddr picks where a bearer-swap proxy listens and the gateway a relay dials for it. Docker
// Desktop forwards host-gateway to the host's loopback; a native engine reaches the host on its bridge.
func HostAddr(ctx context.Context, run Runner) (bindIP, gateway string, err error) {
	ready := Check(ctx, run)
	if !ready.OK {
		return "", "", errors.New(ready.Reason)
	}
	if ready.Desktop {
		return "127.0.0.1", "host-gateway", nil
	}
	out, err := run(ctx, nil, "network", "inspect", "bridge", "--format", "{{(index .IPAM.Config 0).Gateway}}")
	if err != nil {
		return "", "", fmt.Errorf("find the docker bridge gateway: %w", err)
	}
	ip := strings.TrimSpace(string(out))
	if net.ParseIP(ip) == nil {
		return "", "", fmt.Errorf("docker bridge gateway %q is not an IP", ip)
	}
	return ip, ip, nil
}

// Close removes every Docker object of the sandbox, volumes included.
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
	s.started = false
}

// remove deletes the sandbox's objects; each may be missing, so errors are expected and ignored.
func (s *Sandbox) remove(ctx context.Context) {
	for _, args := range [][]string{
		{"rm", "-f", "-v", s.container(), s.relay(), s.egress()},
		{"network", "rm", s.network(), s.egress()},
		{"volume", "rm", "-f", s.homeVol(), s.wsVol()},
	} {
		_, _ = s.run(ctx, nil, args...)
	}
}

// RemoveLabelled removes every container, network and volume carrying all of labels, "key=value",
// and returns how many it removed. Containers go first: a network or a volume in use stays.
func RemoveLabelled(ctx context.Context, run Runner, labels ...string) (int, error) {
	if len(labels) == 0 {
		return 0, errors.New("remove labelled: no label, which would match every object")
	}
	var filters []string
	for _, l := range labels {
		filters = append(filters, "--filter", "label="+l)
	}
	n := 0
	var errs []error
	for _, kind := range [][]string{{"container", "ls", "-aq"}, {"network", "ls", "-q"}, {"volume", "ls", "-q"}} {
		out, err := run(ctx, nil, append(slices.Clone(kind), filters...)...)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		ids := strings.Fields(string(out))
		if len(ids) == 0 {
			continue
		}
		rm := map[string][]string{"container": {"container", "rm", "-f", "-v"}, "network": {"network", "rm"}, "volume": {"volume", "rm", "-f"}}[kind[0]]
		if _, err := run(ctx, nil, append(rm, ids...)...); err != nil {
			errs = append(errs, err)
			continue
		}
		n += len(ids)
	}
	return n, errors.Join(errs...)
}

// personaFile holds the persona's text in the sandbox home, out of the workspace the agent edits.
const personaFile = ".grill-room/persona.md"

// Request is one claude -p invocation.
type Request struct {
	// Prompt goes on stdin: argv caps one argument at 128 KiB, a turn's transcript can exceed it.
	Prompt string
	// System is appended to Claude Code's system prompt: the persona's text.
	System string
	Model  string
	// Effort is passed to --effort; empty keeps Claude Code's default.
	Effort string
	// Resume is the session id to continue; empty starts a new session.
	Resume string
	// PluginDir is a plugin directory to load, the persona's skills; empty for none.
	PluginDir string
	// Autocompact is passed to --autocompact, in tokens; 0 keeps Claude Code's auto.
	Autocompact int
	Args        []string
	// Bearer is the dummy token the sandbox sends to its proxy, written before each turn.
	Bearer string
}

// Result is claude's --output-format json answer.
type Result struct {
	SessionID string `json:"session_id"`
	Result    string `json:"result"`
	IsError   bool   `json:"is_error"`
	Subtype   string `json:"subtype"`
	// ModelUsage is keyed by model id; Claude Code may call a small model on the side.
	ModelUsage map[string]struct {
		InputTokens              int64   `json:"inputTokens"`
		OutputTokens             int64   `json:"outputTokens"`
		CacheReadInputTokens     int64   `json:"cacheReadInputTokens"`
		CacheCreationInputTokens int64   `json:"cacheCreationInputTokens"`
		ContextWindow            int64   `json:"contextWindow"`
		CostUSD                  float64 `json:"costUSD"`
	} `json:"modelUsage"`
	// TotalCostUSD is claude's estimate of the turn's cost, nil when it reports none.
	TotalCostUSD *float64 `json:"total_cost_usd"`
	// Usage is the session's last request: claude 2.1.296 reports it there, while ModelUsage sums
	// every request of the turn.
	Usage struct {
		InputTokens              int64 `json:"input_tokens"`
		CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
		CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	} `json:"usage"`
}

// Context returns what the session's last request sent to the model and the model's window, as
// claude reported them: the context the next turn starts from (003-AC24). 0 is unknown.
func (r *Result) Context() (tokens, window int64) {
	tokens = r.Usage.InputTokens + r.Usage.CacheReadInputTokens + r.Usage.CacheCreationInputTokens
	return tokens, r.ModelUsage[r.Model()].ContextWindow
}

// Tokens sums the turn's tokens over every model claude called: input, output, cache read and
// cache creation (003-AC27).
func (r *Result) Tokens() (in, out, cacheRead, cacheWrite int64) {
	for _, u := range r.ModelUsage {
		in, out, cacheRead, cacheWrite = in+u.InputTokens, out+u.OutputTokens, cacheRead+u.CacheReadInputTokens, cacheWrite+u.CacheCreationInputTokens
	}
	return in, out, cacheRead, cacheWrite
}

// Model is the model id that wrote most of the answer, whose window Context reports, or "" when
// claude reported none. Label, not Model, names the model a turn ran on (005-AC10).
func (r *Result) Model() string {
	var best string
	var most int64 = -1
	for m, u := range r.ModelUsage {
		if u.OutputTokens > most || (u.OutputTokens == most && m < best) {
			best, most = m, u.OutputTokens
		}
	}
	return best
}

// share is a model family's part of a turn, in percent.
type share struct {
	model string
	pct   float64
}

// shares groups ModelUsage by model family, an id naming none standing for itself, weighted by
// cost, or by tokens when claude reports no cost. Largest first.
func (r *Result) shares() []share {
	by := map[string]float64{}
	var cost, tokens float64
	for _, u := range r.ModelUsage {
		cost += u.CostUSD
		tokens += float64(u.InputTokens + u.OutputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens)
	}
	for id, u := range r.ModelUsage {
		f := turn.ModelFamily(id)
		if f == "" {
			f = id
		}
		var w float64
		switch {
		case cost > 0:
			w = 100 * u.CostUSD / cost
		case tokens > 0:
			w = 100 * float64(u.InputTokens+u.OutputTokens+u.CacheReadInputTokens+u.CacheCreationInputTokens) / tokens
		}
		by[f] += w
	}
	out := make([]share, 0, len(by))
	for m, p := range by {
		out = append(out, share{m, p})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].pct > out[j].pct || (out[i].pct == out[j].pct && out[i].model < out[j].model)
	})
	return out
}

func (s share) String() string {
	if s.pct < 1 {
		return s.model + " <1%"
	}
	return fmt.Sprintf("%s %.0f%%", s.model, s.pct)
}

// Label names the model a turn ran on, given the one it asked for after the host cap (005-AC10).
// A resumed session reports its earlier model beside the new one, so the label is the one asked
// for, checked against ModelUsage: the note lists the other models with their cost share,
// "+haiku 3%". When claude reports models but not the one asked for, mismatch is set and the note
// lists what it reported. Asking for none, from an older room server, labels the costliest model.
func (r *Result) Label(asked string) (model, note string, mismatch bool) {
	all := r.shares()
	if asked == "" && len(all) > 0 {
		asked = all[0].model
	}
	fam := turn.ModelFamily(asked)
	var others []string
	found := len(all) == 0
	for _, s := range all {
		if s.model == fam || s.model == asked {
			found = true
			continue
		}
		others = append(others, "+"+s.String())
	}
	if !found {
		for i, s := range all {
			others[i] = s.String()
		}
	}
	return asked, strings.Join(others, ", "), !found
}

// RunClaude runs one turn in the sandbox.
func (s *Sandbox) RunClaude(ctx context.Context, req Request) (*Result, error) {
	s.turn.Lock()
	defer s.turn.Unlock()
	if err := s.prepare(ctx, req.Bearer); err != nil {
		return nil, err
	}
	return s.runClaude(ctx, req)
}

// prepare starts the sandbox and refreshes its stubs with bearer.
func (s *Sandbox) prepare(ctx context.Context, bearer string) error {
	if bearer == "" {
		return errors.New("no Claude account proxy for this sandbox")
	}
	if err := s.Start(ctx); err != nil {
		return err
	}
	return s.writeStubs(ctx, bearer)
}

// runClaude runs claude -p in the started sandbox; the caller holds s.turn.
func (s *Sandbox) runClaude(ctx context.Context, req Request) (*Result, error) {
	args := []string{"exec", "-i", s.container(), "claude", "-p", "--output-format", "json"}
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	if req.Effort != "" {
		args = append(args, "--effort", req.Effort)
	}
	if req.System != "" {
		if err := s.writeHome(ctx, map[string][]byte{personaFile: []byte(req.System)}); err != nil {
			return nil, fmt.Errorf("write persona: %w", err)
		}
		// Snapshot off: a resumed session would otherwise keep the persona text of its first turn.
		args = append(args, "--append-system-prompt-file", home+"/"+personaFile, "--system-prompt-snapshot", "off")
	}
	if req.Resume != "" {
		args = append(args, "--resume", req.Resume)
	}
	if req.PluginDir != "" {
		args = append(args, "--plugin-dir", req.PluginDir)
	}
	if req.Autocompact != 0 {
		args = append(args, "--autocompact", strconv.Itoa(req.Autocompact))
	}
	args = append(args, req.Args...)
	out, runErr := s.run(ctx, strings.NewReader(req.Prompt), args...)
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
// refused refresh, and the supervisor may have rotated the bearer) and the oauthAccount stub, which
// skips the profile fetch only while under 24 h old.
func (s *Sandbox) writeStubs(ctx context.Context, bearer string) error {
	nowMS := time.Now().UnixMilli()
	files := map[string]any{
		".claude/.credentials.json": map[string]any{"claudeAiOauth": map[string]any{
			"accessToken": bearer, "refreshToken": "grill-room-dummy-refresh",
			"expiresAt": time.Now().AddDate(10, 0, 0).UnixMilli(), "subscriptionType": "max",
			"scopes": []string{"user:inference", "user:profile", "user:sessions:claude_code"},
		}},
		// Fake identity: keeps the host's account out of uploaded sessions (S2).
		".claude.json": map[string]any{"oauthAccount": map[string]any{
			"accountUuid":           "00000000-0000-4000-8000-000000000001",
			"emailAddress":          "agent@grill-room.invalid",
			"organizationUuid":      "00000000-0000-4000-8000-000000000002",
			"billingType":           "stripe_subscription",
			"accountCreatedAt":      "2025-01-01T00:00:00Z",
			"subscriptionCreatedAt": "2025-01-01T00:00:00Z",
			"ccOnboardingFlags":     map[string]any{},
			"profileFetchedAt":      nowMS,
		}},
	}
	raw := map[string][]byte{}
	for name, v := range files {
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Errorf("stubs: %w", err)
		}
		raw[name] = b
	}
	if err := s.writeHome(ctx, raw); err != nil {
		return fmt.Errorf("write stubs: %w", err)
	}
	return nil
}

// writeHome writes files, by path under the sandbox home, owner-only, creating their directories.
func (s *Sandbox) writeHome(ctx context.Context, files map[string][]byte) error {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	now := time.Now()
	dirs := map[string]bool{}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if dir := path.Dir(name); dir != "." && !dirs[dir] {
			dirs[dir] = true
			if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: dir + "/", Mode: 0o700, ModTime: now}); err != nil {
				return err
			}
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(files[name])), ModTime: now}); err != nil {
			return err
		}
		if _, err := tw.Write(files[name]); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	// tar runs as the agent user, so the files end up owned by it.
	_, err := s.run(ctx, &buf, "exec", "-i", s.container(), "tar", "-x", "-C", home)
	return err
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
