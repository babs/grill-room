package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zalando/go-keyring"
	"go.uber.org/zap"

	"github.com/babs/grill-room/internal/creds"
	"github.com/babs/grill-room/internal/proxy"
	"github.com/babs/grill-room/internal/sandbox"
)

// Account kinds (003-AC12).
const (
	// KindConfigDir is a Claude Code config dir, read only: the host's Claude Code refreshes it.
	KindConfigDir = "config_dir"
	// KindSetupToken is a `claude setup-token` token: about a year, no refresh, no expiry date.
	KindSetupToken = "setup_token"
)

// Where a setup token is kept.
const (
	storageKeyring = "keyring"
	storageFile    = "file"
)

// Warning lead times (003-AC16) and the setup token's assumed lifetime (spike S8).
const (
	accessTokenWarning = 30 * time.Minute
	setupTokenWarning  = 14 * 24 * time.Hour
	setupTokenLife     = 365 * 24 * time.Hour
	// probeEvery bounds the count_tokens status probes of a setup token (003-AC12).
	probeEvery     = 15 * time.Minute
	keyringTimeout = 5 * time.Second
)

// Account is a Claude account the supervisor knows, kept in accounts.json. It never holds a token.
type Account struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
	// ConfigDir is a config_dir account's dir; empty is Claude Code's default location.
	ConfigDir string `json:"config_dir,omitempty"`
	// AddedAt dates a setup token, whose expiry is estimated from it.
	AddedAt time.Time `json:"added_at,omitzero"`
	// Storage is where a setup token is: "keyring" or "file".
	Storage string `json:"storage,omitempty"`
}

// AccountView is an account as the admin page shows it.
type AccountView struct {
	Account
	// State is "valid", "expired", "refused", "unknown" or "error".
	State     string     `json:"state"`
	Detail    string     `json:"detail,omitempty"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	// Estimated marks a setup token's expiry: added + 365 days.
	Estimated bool       `json:"expiry_estimated,omitempty"`
	CheckedAt *time.Time `json:"checked_at,omitempty"`
	LastUse   *time.Time `json:"last_use,omitempty"`
	// Requests are the proxied requests per instance (worker).
	Requests map[string]int64 `json:"requests"`
	Warning  string           `json:"warning,omitempty"`
}

// Keyring stores secrets in the OS keyring: Keychain, Secret Service or Windows Credential Manager.
type Keyring interface {
	Set(service, user, secret string) error
	Get(service, user string) (string, error)
	Delete(service, user string) error
}

type osKeyring struct{}

func (osKeyring) Set(service, user, secret string) error { return keyring.Set(service, user, secret) }
func (osKeyring) Get(service, user string) (string, error) {
	return keyring.Get(service, user)
}
func (osKeyring) Delete(service, user string) error { return keyring.Delete(service, user) }

// AccountsOptions wires Accounts.
type AccountsOptions struct {
	Paths Paths
	// ClaudeConfigDir becomes account "default" when no accounts.json exists yet.
	ClaudeConfigDir string
	// Upstream is Anthropic's API, for the proxies and the status probe.
	Upstream *url.URL
	// Docker finds where the proxies listen so that sandboxes reach them.
	Docker  sandbox.Runner
	Keyring Keyring
	HTTP    *http.Client
	Now     func() time.Time
	Log     *zap.Logger
}

// Accounts holds the Claude accounts and runs one bearer-swap proxy per account, in the
// supervisor only: workers get a proxy address and a dummy bearer, never a token (003-AC14).
type Accounts struct {
	o AccountsOptions

	// editMu serialises Add and Remove: a token is stored before its account is listed.
	editMu sync.Mutex
	mu     sync.Mutex
	list   []Account
	px     map[string]*accountProxy
	// bearers are each worker's dummy bearers, by instance then account.
	bearers map[string]map[string]string

	// bindMu serialises the search for where the proxies listen, cached once found.
	bindMu        sync.Mutex
	bindIP, gwKey string
}

// accountProxy is one account's token source and proxy, listening once a room needs it.
type accountProxy struct {
	acc  Account
	src  proxy.TokenSource
	p    *proxy.Proxy
	srv  *http.Server
	addr string
	// The last status probe, and the proxy's 401 count then.
	probeState, probeDetail string
	probedAt                time.Time
	unauthorizedAtProbe     int64
}

var accountName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// LoadAccounts reads accounts.json; with none, the only account is "default", o.ClaudeConfigDir.
func LoadAccounts(o AccountsOptions) (*Accounts, error) {
	if o.Keyring == nil {
		o.Keyring = osKeyring{}
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.HTTP == nil {
		o.HTTP = &http.Client{Timeout: 15 * time.Second}
	}
	a := &Accounts{o: o, px: map[string]*accountProxy{}, bearers: map[string]map[string]string{}}
	var f struct {
		Accounts []Account `json:"accounts"`
	}
	b, err := os.ReadFile(a.file())
	switch {
	case errors.Is(err, os.ErrNotExist):
		dir := o.ClaudeConfigDir
		// An absolute dir stays as written: Claude Code names its Keychain item after the raw value.
		if dir != "" && !filepath.IsAbs(dir) {
			if dir, err = filepath.Abs(dir); err != nil {
				return nil, fmt.Errorf("account default: %w", err)
			}
		}
		f.Accounts = []Account{{Name: "default", Kind: KindConfigDir, ConfigDir: dir}}
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal(b, &f); err != nil {
			return nil, fmt.Errorf("parse %s: %w", a.file(), err)
		}
	}
	for _, acc := range f.Accounts {
		if !accountName.MatchString(acc.Name) || (acc.Kind != KindConfigDir && acc.Kind != KindSetupToken) || a.px[acc.Name] != nil {
			o.Log.Error("account_skipped", zap.String("name", acc.Name), zap.String("kind", acc.Kind))
			continue
		}
		a.list = append(a.list, acc)
		a.px[acc.Name] = a.newProxy(acc)
	}
	return a, nil
}

func (a *Accounts) file() string { return filepath.Join(a.o.Paths.Config, "accounts.json") }

// secretFile is a setup token's file when the OS keyring is unavailable: in the local state dir,
// which a Windows roaming profile does not copy.
func (a *Accounts) secretFile(name string) string {
	return filepath.Join(a.o.Paths.State, "accounts", name+".token")
}

// keyringService names this supervisor's keyring items: two supervisors with their own config
// dirs never share one.
func (a *Accounts) keyringService() string {
	abs, err := filepath.Abs(a.o.Paths.Config)
	if err != nil {
		abs = a.o.Paths.Config
	}
	sum := sha256.Sum256([]byte(abs))
	return "grill-room-" + hex.EncodeToString(sum[:4])
}

func (a *Accounts) saveLocked() error {
	b, err := json.MarshalIndent(map[string][]Account{"accounts": a.list}, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(a.file(), append(b, '\n'))
}

func (a *Accounts) newProxy(acc Account) *accountProxy {
	ap := &accountProxy{acc: acc}
	if acc.Kind == KindConfigDir {
		ap.src = creds.NewSource(creds.ForConfigDir(acc.ConfigDir))
	} else {
		ap.src = &secretSource{a: a, acc: acc}
	}
	ap.p = proxy.New(a.o.Upstream, ap.src, a.o.Log.Named("proxy").With(zap.String("account", acc.Name)))
	return ap
}

// secretSource reads a setup token once, from the keyring or its file, and keeps it in memory. A
// failed read is kept for secretRetry: a locked keyring must not stall every proxied request.
type secretSource struct {
	a   *Accounts
	acc Account
	mu  sync.Mutex
	tok string
	err error
	at  time.Time
}

const secretRetry = 30 * time.Second

func (s *secretSource) Get(context.Context) (creds.Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tok == "" {
		if s.err != nil && s.a.o.Now().Sub(s.at) < secretRetry {
			return creds.Token{}, s.err
		}
		tok, err := s.a.readSecret(s.acc)
		if err != nil {
			s.err, s.at = err, s.a.o.Now()
			return creds.Token{}, err
		}
		s.tok, s.err = tok, nil
	}
	return creds.Token{AccessToken: s.tok, Origin: s.acc.Storage}, nil
}

// Invalidate keeps the token: a setup token has no refresh; a 401 brings a status probe forward.
func (*secretSource) Invalidate() {}

// withTimeout bounds a keyring call: a locked keyring may wait on a prompt forever.
// ponytail: a timed-out call keeps running in its goroutine until the keyring answers.
func withTimeout[T any](f func() (T, error)) (T, error) {
	type res struct {
		v   T
		err error
	}
	done := make(chan res, 1)
	go func() {
		v, err := f()
		done <- res{v, err}
	}()
	select {
	case r := <-done:
		return r.v, r.err
	case <-time.After(keyringTimeout):
		var zero T
		return zero, fmt.Errorf("no answer within %s, it may be locked", keyringTimeout)
	}
}

func (a *Accounts) readSecret(acc Account) (string, error) {
	if acc.Storage == storageKeyring {
		tok, err := withTimeout(func() (string, error) { return a.o.Keyring.Get(a.keyringService(), acc.Name) })
		if err != nil {
			return "", fmt.Errorf("read the token from the OS keyring: %w", err)
		}
		return tok, nil
	}
	b, err := os.ReadFile(a.secretFile(acc.Name)) //nolint:gosec // names match accountName: no separator, no dot-dot
	if err != nil {
		return "", fmt.Errorf("read the token file: %w", err)
	}
	return strings.TrimSpace(string(b)), nil
}

// storeSecret keeps tok in the OS keyring, or in an owner-only file when there is none. A keyring
// that answers after the timeout gets its copy deleted: the file is the one the account records.
func (a *Accounts) storeSecret(name, tok string) (string, error) {
	service := a.keyringService()
	done := make(chan error, 1)
	go func() { done <- a.o.Keyring.Set(service, name, tok) }()
	var err error
	select {
	case err = <-done:
	case <-time.After(keyringTimeout):
		err = fmt.Errorf("no answer within %s, it may be locked", keyringTimeout)
		go func() {
			if <-done == nil {
				_ = a.o.Keyring.Delete(service, name)
			}
		}()
	}
	if err == nil {
		return storageKeyring, nil
	}
	a.o.Log.Warn("keyring_unavailable", zap.String("account", name), zap.Error(err), zap.String("fallback", "owner-only file"))
	if err := writeFileAtomic(a.secretFile(name), []byte(tok)); err != nil {
		return "", fmt.Errorf("store the token: %w", err)
	}
	return storageFile, nil
}

func (a *Accounts) deleteSecret(acc Account) {
	if acc.Storage == storageKeyring {
		if _, err := withTimeout(func() (struct{}, error) { return struct{}{}, a.o.Keyring.Delete(a.keyringService(), acc.Name) }); err != nil {
			a.o.Log.Warn("keyring_delete_failed", zap.String("account", acc.Name), zap.Error(err))
		}
		return
	}
	if err := os.Remove(a.secretFile(acc.Name)); err != nil && !errors.Is(err, os.ErrNotExist) {
		a.o.Log.Warn("token_file_delete_failed", zap.String("account", acc.Name), zap.Error(err))
	}
}

// setupTokenRe catches a pasted token cut short by terminal wrapping (anthropics/claude-code#65320).
var setupTokenRe = regexp.MustCompile(`^sk-ant-oat01-[A-Za-z0-9_-]{80,}$`)

// Add adds an account. A setup token is stored at once and never shown again (003-AC12).
func (a *Accounts) Add(ctx context.Context, acc Account, token string) error {
	if !accountName.MatchString(acc.Name) {
		return errors.New("an account name is 1 to 64 letters, digits, dots, dashes or underscores")
	}
	switch acc.Kind {
	case KindConfigDir:
		if acc.ConfigDir != "" {
			if !filepath.IsAbs(acc.ConfigDir) {
				return errors.New("give the config dir as an absolute path, or leave it empty for Claude Code's default")
			}
			if fi, err := os.Stat(acc.ConfigDir); err != nil || !fi.IsDir() {
				return fmt.Errorf("%s is not a directory", acc.ConfigDir)
			}
		}
		acc.Storage, acc.AddedAt = "", time.Time{}
	case KindSetupToken:
		token = strings.TrimSpace(token)
		if !setupTokenRe.MatchString(token) {
			return errors.New("this is not a whole setup token: it starts with sk-ant-oat01- and holds about 108 characters; copy it again from `claude setup-token`")
		}
		acc.ConfigDir = ""
	default:
		return errors.New("an account is a config dir or a setup token")
	}
	a.editMu.Lock()
	defer a.editMu.Unlock()
	a.mu.Lock()
	for name := range a.px {
		// Case-insensitive file systems and keyrings would give both names one token.
		if strings.EqualFold(name, acc.Name) {
			a.mu.Unlock()
			return fmt.Errorf("an account named %s exists", name)
		}
	}
	a.mu.Unlock()
	if acc.Kind == KindSetupToken {
		storage, err := a.storeSecret(acc.Name, token)
		if err != nil {
			return err
		}
		acc.Storage, acc.AddedAt = storage, a.o.Now().UTC()
	}
	a.mu.Lock()
	ap := a.newProxy(acc)
	a.list = append(a.list, acc)
	a.px[acc.Name] = ap
	err := a.saveLocked()
	if err != nil {
		a.list = a.list[:len(a.list)-1]
		delete(a.px, acc.Name)
	}
	a.mu.Unlock()
	if err != nil {
		if acc.Kind == KindSetupToken {
			a.deleteSecret(acc)
		}
		return err
	}
	a.o.Log.Info("account_added", zap.String("account", acc.Name), zap.String("kind", acc.Kind), zap.String("storage", acc.Storage))
	if acc.Kind == KindSetupToken {
		a.probe(ctx, ap)
	}
	return nil
}

// Remove removes an account, stops its proxy and deletes its stored token.
func (a *Accounts) Remove(name string) error {
	a.editMu.Lock()
	defer a.editMu.Unlock()
	a.mu.Lock()
	ap := a.px[name]
	if ap == nil {
		a.mu.Unlock()
		return errors.New("no such account")
	}
	i := slices.IndexFunc(a.list, func(x Account) bool { return x.Name == name })
	a.list = slices.Delete(a.list, i, i+1)
	delete(a.px, name)
	for _, m := range a.bearers {
		delete(m, name)
	}
	err := a.saveLocked()
	if ap.srv != nil {
		_ = ap.srv.Close()
		ap.srv = nil
	}
	a.mu.Unlock()
	if ap.acc.Kind == KindSetupToken {
		a.deleteSecret(ap.acc)
	}
	a.o.Log.Info("account_removed", zap.String("account", name))
	return err
}

// Token returns account name's real token, for the check command's scan. Never for a worker.
func (a *Accounts) Token(ctx context.Context, name string) (creds.Token, error) {
	a.mu.Lock()
	ap := a.px[name]
	a.mu.Unlock()
	if ap == nil {
		return creds.Token{}, errors.New("no such account")
	}
	return ap.src.Get(ctx)
}

// Has reports whether an account is named name.
func (a *Accounts) Has(name string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.px[name] != nil
}

// Close stops every proxy.
func (a *Accounts) Close() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, ap := range a.px {
		if ap.srv != nil {
			_ = ap.srv.Close()
			ap.srv = nil
		}
	}
}

// Probe checks a setup token now with count_tokens, which spends no inference (spike S8).
func (a *Accounts) Probe(ctx context.Context, name string) error {
	a.mu.Lock()
	ap := a.px[name]
	a.mu.Unlock()
	if ap == nil {
		return errors.New("no such account")
	}
	if ap.acc.Kind != KindSetupToken {
		return errors.New("only setup tokens are probed: a config dir's status comes from its token's expiry")
	}
	a.probe(ctx, ap)
	return nil
}

// ProbeDue probes the setup tokens not probed for probeEvery, and, at most once a minute, those
// upstream refused on a proxied request since their last probe. Only a probe marks a token
// refused: a sandbox holds a bearer and could provoke a 401 to block the account.
func (a *Accounts) ProbeDue(ctx context.Context) {
	a.mu.Lock()
	var due []*accountProxy
	for _, ap := range a.px {
		since := a.o.Now().Sub(ap.probedAt)
		if ap.acc.Kind == KindSetupToken && (since >= probeEvery || since >= time.Minute && ap.p.Stats().Unauthorized > ap.unauthorizedAtProbe) {
			due = append(due, ap)
		}
	}
	a.mu.Unlock()
	for _, ap := range due {
		a.probe(ctx, ap)
	}
}

func (a *Accounts) probe(ctx context.Context, ap *accountProxy) {
	var state, detail string
	tok, err := ap.src.Get(ctx)
	if err != nil {
		state, detail = "error", err.Error()
	} else {
		state, detail = a.countTokens(ctx, tok.AccessToken)
	}
	a.mu.Lock()
	ap.probeState, ap.probeDetail, ap.probedAt, ap.unauthorizedAtProbe = state, detail, a.o.Now(), ap.p.Stats().Unauthorized
	a.mu.Unlock()
	a.o.Log.Info("account_probed", zap.String("account", ap.acc.Name), zap.String("state", state), zap.String("detail", detail))
}

// countTokens asks upstream to count a one-word prompt's tokens with tok.
func (a *Accounts) countTokens(ctx context.Context, tok string) (state, detail string) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	body := `{"model":"claude-haiku-4-5","messages":[{"role":"user","content":"ok"}]}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.o.Upstream.JoinPath("/v1/messages/count_tokens").String(), strings.NewReader(body)) //nolint:gosec // Anthropic's fixed API URL
	if err != nil {
		return "unknown", err.Error()
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	req.Header.Set("Anthropic-Beta", "oauth-2025-04-20")
	resp, err := a.o.HTTP.Do(req) //nolint:gosec // Anthropic's fixed API URL
	if err != nil {
		return "unknown", "status probe failed: " + err.Error()
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	switch resp.StatusCode {
	case http.StatusOK:
		return "valid", ""
	case http.StatusUnauthorized:
		return "refused", "Anthropic refused the token: create a new one with `claude setup-token`"
	}
	return "unknown", "the status probe got " + resp.Status
}

// Views lists the accounts with their status, expiry, warnings and use (003-AC12, 003-AC16).
func (a *Accounts) Views(ctx context.Context) []AccountView {
	a.mu.Lock()
	aps := make([]*accountProxy, 0, len(a.list))
	for _, acc := range a.list {
		aps = append(aps, a.px[acc.Name])
	}
	a.mu.Unlock()
	out := make([]AccountView, 0, len(aps))
	for _, ap := range aps {
		out = append(out, a.view(ctx, ap))
	}
	return out
}

func (a *Accounts) view(ctx context.Context, ap *accountProxy) AccountView {
	now := a.o.Now()
	st := ap.p.Stats()
	v := AccountView{Account: ap.acc, Requests: st.Requests}
	if !st.LastUse.IsZero() {
		v.LastUse = &st.LastUse
	}
	switch ap.acc.Kind {
	case KindConfigDir:
		tok, err := ap.src.Get(ctx)
		if err != nil {
			v.State, v.Detail = "error", err.Error()
			v.Warning = "the Claude login cannot be read: log in with Claude Code on this machine"
			break
		}
		v.Detail = "read from " + tok.Origin
		v.State = "valid"
		if tok.ExpiresAt.IsZero() {
			break
		}
		exp := tok.ExpiresAt
		v.ExpiresAt = &exp
		switch left := exp.Sub(now); {
		case left <= 0:
			v.State = "expired"
			v.Warning = "the access token expired: run Claude Code on this machine so that it refreshes it"
		case left <= accessTokenWarning:
			v.Warning = fmt.Sprintf("the access token expires in %s: run Claude Code on this machine so that it refreshes it", left.Round(time.Minute))
		}
	case KindSetupToken:
		a.mu.Lock()
		v.State, v.Detail = ap.probeState, ap.probeDetail
		if !ap.probedAt.IsZero() {
			at := ap.probedAt
			v.CheckedAt = &at
		}
		a.mu.Unlock()
		if v.State == "" {
			v.State = "unknown"
		}
		exp := ap.acc.AddedAt.Add(setupTokenLife)
		v.ExpiresAt, v.Estimated = &exp, true
		switch left := exp.Sub(now); {
		case left <= 0:
			v.Warning = "the token is about a year old and has likely expired: create a new one with `claude setup-token`"
		case left <= setupTokenWarning:
			v.Warning = fmt.Sprintf("the token is likely to expire in %d days: create a new one with `claude setup-token`", int(left.Hours()/24))
		}
		if v.State == "refused" {
			v.Warning = v.Detail
		}
	}
	return v
}

// roomError is why a room cannot use acc now, in words for the room: never the account's name or
// dir (003-AC17). Empty when it can.
func (a *Accounts) roomError(ctx context.Context, ap *accountProxy) string {
	v := a.view(ctx, ap)
	switch v.State {
	case "expired":
		return "the host's Claude login has expired"
	case "refused":
		return "Anthropic refused the host's Claude login"
	case "error":
		return "the host's Claude login cannot be read"
	}
	return ""
}

// Access is what a worker's rooms use (003-AC13, 003-AC14): for each room, the proxy of the
// account roomAccount names, and the worker's own dummy bearer for it. A cut-off worker gets no
// bearer, and its old ones are refused at the proxy (003-AC15).
func (a *Accounts) Access(ctx context.Context, instance string, roomAccount map[string]string, cutOff bool) Access {
	out := Access{Proxies: map[string]Proxy{}, CutOff: cutOff}
	if cutOff {
		a.Revoke(instance)
		return out
	}
	a.dropUnused(instance, roomAccount)
	for room, name := range roomAccount {
		a.mu.Lock()
		ap := a.px[name]
		a.mu.Unlock()
		if ap == nil {
			out.Proxies[room] = Proxy{Error: "the host has no Claude account for this room"}
			continue
		}
		if e := a.roomError(ctx, ap); e != "" {
			out.Proxies[room] = Proxy{Error: e}
			continue
		}
		addr, err := a.listen(ctx, ap)
		if err != nil {
			a.o.Log.Warn("account_proxy_unavailable", zap.String("account", name), zap.Error(err))
			out.Proxies[room] = Proxy{Error: "Claude access is not ready on the host's machine: Docker is not ready"}
			continue
		}
		out.Proxies[room] = Proxy{Addr: addr, Bearer: a.bearer(instance, ap)}
	}
	return out
}

// dropUnused revokes instance's bearers for the accounts none of its rooms uses any more: a room
// moved to another account must not keep spending the old one.
func (a *Accounts) dropUnused(instance string, roomAccount map[string]string) {
	used := map[string]bool{}
	for _, name := range roomAccount {
		used[name] = true
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for name := range a.bearers[instance] {
		if used[name] {
			continue
		}
		if ap := a.px[name]; ap != nil {
			ap.p.Revoke(instance)
		}
		delete(a.bearers[instance], name)
	}
}

// bearer is instance's dummy bearer for ap's proxy, made and allowed on first use.
func (a *Accounts) bearer(instance string, ap *accountProxy) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	m := a.bearers[instance]
	if m == nil {
		m = map[string]string{}
		a.bearers[instance] = m
	}
	b := m[ap.acc.Name]
	if b == "" {
		b = sandbox.NewBearer()
		m[ap.acc.Name] = b
		ap.p.Allow(instance, b)
	}
	return b
}

// Revoke shuts instance out of every proxy; its next access gets new bearers.
func (a *Accounts) Revoke(instance string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, ap := range a.px {
		ap.p.Revoke(instance)
	}
	delete(a.bearers, instance)
}

// Requests is how many requests instance's worker sent, all accounts together.
func (a *Accounts) Requests(instance string) int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	var n int64
	for _, ap := range a.px {
		n += ap.p.Stats().Requests[instance]
	}
	return n
}

// listen starts ap's proxy where the sandboxes' relays reach it, and returns what they dial.
func (a *Accounts) listen(ctx context.Context, ap *accountProxy) (string, error) {
	a.mu.Lock()
	addr := ap.addr
	a.mu.Unlock()
	if addr != "" {
		return addr, nil
	}
	a.bindMu.Lock()
	defer a.bindMu.Unlock()
	if a.bindIP == "" {
		// ponytail: found once; a Docker bridge moved to another subnet needs a supervisor restart.
		ip, gw, err := sandbox.HostAddr(ctx, a.o.Docker)
		if err != nil {
			return "", err
		}
		a.bindIP, a.gwKey = ip, gw
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if ap.addr != "" {
		return ap.addr, nil
	}
	if a.px[ap.acc.Name] != ap {
		return "", errors.New("the account was removed")
	}
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", net.JoinHostPort(a.bindIP, "0"))
	if err != nil {
		return "", fmt.Errorf("proxy listen on %s: %w", a.bindIP, err)
	}
	ap.srv = &http.Server{
		Handler:           ap.p,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       5 * time.Minute,
		WriteTimeout:      30 * time.Minute, // long streamed turns
		IdleTimeout:       2 * time.Minute,
	}
	srv, log := ap.srv, a.o.Log
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("proxy_serve_failed", zap.Error(err))
		}
	}()
	ap.addr = net.JoinHostPort(a.gwKey, strconv.Itoa(ln.Addr().(*net.TCPAddr).Port))
	a.o.Log.Info("proxy_started", zap.String("account", ap.acc.Name), zap.String("listen", ln.Addr().String()))
	return ap.addr, nil
}
