package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
)

// memKeyring is an OS keyring in memory; down makes every call fail, as on a host without one.
type memKeyring struct {
	mu   sync.Mutex
	m    map[string]string
	down bool
}

func (k *memKeyring) Set(service, user, secret string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.down {
		return errors.New("no keyring here")
	}
	k.m[service+"/"+user] = secret
	return nil
}

func (k *memKeyring) Get(service, user string) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if s, ok := k.m[service+"/"+user]; ok && !k.down {
		return s, nil
	}
	return "", errors.New("not found")
}

func (k *memKeyring) Delete(service, user string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	delete(k.m, service+"/"+user)
	return nil
}

// A setup token as `claude setup-token` prints it: 108 characters.
var setupToken = "sk-ant-oat01-" + strings.Repeat("Ab3_-", 19)

// desktopDocker answers like Docker Desktop: proxies listen on 127.0.0.1.
func desktopDocker(context.Context, io.Reader, ...string) ([]byte, error) {
	return []byte("28.1|Docker Desktop"), nil
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time  { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) set(t time.Time) { c.mu.Lock(); c.t = t; c.mu.Unlock() }

type accountsEnv struct {
	a     *Accounts
	paths Paths
	kr    *memKeyring
	clk   *clock
	// upstream counts the count_tokens probes and records the bearer of each proxied request.
	probes   atomic.Int64
	probeSay atomic.Int64
	upSay    atomic.Int64
	mu       sync.Mutex
	seen     []string
}

func newAccountsEnv(t *testing.T, claudeDir string) *accountsEnv {
	t.Helper()
	dir := t.TempDir()
	e := &accountsEnv{paths: Paths{Config: filepath.Join(dir, "config"), State: filepath.Join(dir, "state"), Cache: filepath.Join(dir, "cache")},
		kr: &memKeyring{m: map[string]string{}}, clk: &clock{t: time.Now()}}
	e.probeSay.Store(http.StatusOK)
	e.upSay.Store(http.StatusOK)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/messages/count_tokens" {
			e.probes.Add(1)
			if r.Header.Get("Anthropic-Beta") != "oauth-2025-04-20" || r.Header.Get("Authorization") != "Bearer "+setupToken {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			w.WriteHeader(int(e.probeSay.Load()))
			return
		}
		e.mu.Lock()
		e.seen = append(e.seen, r.Header.Get("Authorization"))
		e.mu.Unlock()
		w.WriteHeader(int(e.upSay.Load()))
	}))
	t.Cleanup(up.Close)
	upURL, _ := url.Parse(up.URL)
	a, err := LoadAccounts(AccountsOptions{Paths: e.paths, ClaudeConfigDir: claudeDir, Upstream: upURL, Docker: desktopDocker,
		Keyring: e.kr, Now: e.clk.now, Log: zap.NewNop()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	e.a = a
	return e
}

func writeClaudeCreds(t *testing.T, dir, tok string, exp time.Time) {
	t.Helper()
	body := fmt.Sprintf(`{"claudeAiOauth":{"accessToken":%q,"expiresAt":%d}}`, tok, exp.UnixMilli())
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func view(t *testing.T, a *Accounts, name string) AccountView {
	t.Helper()
	for _, v := range a.Views(context.Background()) {
		if v.Name == name {
			return v
		}
	}
	t.Fatalf("no account %s", name)
	return AccountView{}
}

// grep counts the files under root holding needle.
func grep(t *testing.T, root, needle string) int {
	t.Helper()
	n := 0
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if b, rerr := os.ReadFile(p); rerr == nil && strings.Contains(string(b), needle) { //nolint:gosec // test dir
				n++
			}
		}
		return nil
	})
	return n
}

// 003-AC10 003-AC12: with no accounts.json, the only account is "default", the supervisor's
// CLAUDE_CONFIG_DIR, read only.
func TestAccountsDefault(t *testing.T) {
	claude := t.TempDir()
	e := newAccountsEnv(t, claude)
	vs := e.a.Views(context.Background())
	if len(vs) != 1 || vs[0].Name != "default" || vs[0].Kind != KindConfigDir || vs[0].ConfigDir != claude || vs[0].State != "error" {
		t.Fatalf("views %+v", vs)
	}
	writeClaudeCreds(t, claude, "cfg-secret", time.Now().Add(time.Hour))
	if v := view(t, e.a, "default"); v.State != "valid" || v.Warning != "" {
		t.Fatalf("with creds: %+v", v)
	}
	if _, err := os.Stat(e.a.file()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("accounts.json written with nothing to keep")
	}
}

// 003-AC12: a setup token goes to the OS keyring, or to an owner-only file without one; it never
// reaches accounts.json or a view, and leaves with its account. Bad input is refused.
func TestAccountsSetupTokenStorage(t *testing.T) {
	for _, keyringUp := range []bool{true, false} {
		t.Run(fmt.Sprint("keyring=", keyringUp), func(t *testing.T) {
			e := newAccountsEnv(t, "")
			e.kr.down = !keyringUp
			ctx := context.Background()
			if err := e.a.Add(ctx, Account{Name: "work", Kind: KindSetupToken}, "  "+setupToken+"\n"); err != nil {
				t.Fatal(err)
			}
			v := view(t, e.a, "work")
			want := map[bool]string{true: storageKeyring, false: storageFile}[keyringUp]
			if v.Storage != want || v.AddedAt.IsZero() || v.State != "valid" {
				t.Fatalf("view %+v", v)
			}
			b, _ := json.Marshal(e.a.Views(ctx))
			if strings.Contains(string(b), setupToken) {
				t.Fatal("a view holds the token")
			}
			files := grep(t, filepath.Dir(e.paths.Config), setupToken)
			if keyringUp && files != 0 || !keyringUp && files != 1 {
				t.Fatalf("%d files hold the token", files)
			}
			if grep(t, e.paths.Config, setupToken) != 0 {
				t.Fatal("the config dir holds the token")
			}
			if !keyringUp && runtime.GOOS != "windows" {
				if fi, err := os.Stat(e.a.secretFile("work")); err != nil || fi.Mode().Perm() != 0o600 {
					t.Fatalf("token file %v %v", fi, err)
				}
			}
			// A restarted supervisor finds the token where it was put.
			again, err := LoadAccounts(e.a.o)
			if err != nil {
				t.Fatal(err)
			}
			if tok, err := again.Token(ctx, "work"); err != nil || tok.AccessToken != setupToken {
				t.Fatalf("reloaded token: %v", err)
			}
			if err := e.a.Remove("work"); err != nil {
				t.Fatal(err)
			}
			if len(e.kr.m) != 0 || grep(t, filepath.Dir(e.paths.Config), setupToken) != 0 {
				t.Fatal("the token outlived its account")
			}
		})
	}
	e := newAccountsEnv(t, "")
	ctx := context.Background()
	for _, tc := range []struct {
		acc   Account
		token string
		want  string
	}{
		{Account{Name: "cut", Kind: KindSetupToken}, setupToken[:79], "not a whole setup token"},
		{Account{Name: "key", Kind: KindSetupToken}, "sk-ant-api03-" + strings.Repeat("a", 95), "not a whole setup token"},
		{Account{Name: "../x", Kind: KindSetupToken}, setupToken, "account name"},
		{Account{Name: "default", Kind: KindSetupToken}, setupToken, "exists"},
		{Account{Name: "Default", Kind: KindSetupToken}, setupToken, "named default exists"},
		{Account{Name: "rel", Kind: KindConfigDir, ConfigDir: "relative/dir"}, "", "absolute path"},
		{Account{Name: "gone", Kind: KindConfigDir, ConfigDir: filepath.Join(t.TempDir(), "nope")}, "", "not a directory"},
		{Account{Name: "what", Kind: "api_key"}, "", "config dir or a setup token"},
	} {
		if err := e.a.Add(ctx, tc.acc, tc.token); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err %v, want %q", tc.acc.Name, err, tc.want)
		}
	}
	if len(e.kr.m) != 0 || len(e.a.Views(ctx)) != 1 {
		t.Fatal("a refused account left something behind")
	}
}

// 003-AC12: a setup token's status comes from count_tokens, at most every 15 minutes and on
// demand; a proxied 401 shows it refused before the next probe.
func TestAccountsProbe(t *testing.T) {
	e := newAccountsEnv(t, "")
	ctx := context.Background()
	if err := e.a.Add(ctx, Account{Name: "work", Kind: KindSetupToken}, setupToken); err != nil {
		t.Fatal(err)
	}
	if e.probes.Load() != 1 || view(t, e.a, "work").State != "valid" {
		t.Fatalf("probes %d, view %+v", e.probes.Load(), view(t, e.a, "work"))
	}
	e.a.ProbeDue(ctx)
	e.clk.set(e.clk.now().Add(14 * time.Minute))
	e.a.ProbeDue(ctx)
	if e.probes.Load() != 1 {
		t.Fatalf("probed %d times within 15 minutes", e.probes.Load())
	}
	e.probeSay.Store(http.StatusUnauthorized)
	e.clk.set(e.clk.now().Add(time.Minute))
	e.a.ProbeDue(ctx)
	if v := view(t, e.a, "work"); e.probes.Load() != 2 || v.State != "refused" || !strings.Contains(v.Warning, "claude setup-token") {
		t.Fatalf("probes %d, view %+v", e.probes.Load(), v)
	}
	e.probeSay.Store(http.StatusOK)
	if err := e.a.Probe(ctx, "work"); err != nil || e.probes.Load() != 3 || view(t, e.a, "work").State != "valid" {
		t.Fatalf("on demand: %v, probes %d", err, e.probes.Load())
	}
	if err := e.a.Probe(ctx, "default"); err == nil {
		t.Fatal("a config dir was probed")
	}
	// A proxied 401 brings the next probe forward to a minute after the last one; only the probe
	// decides. A sandbox provoking 401s with a bad request cannot block the account.
	p := e.a.Access(ctx, "inst", map[string]string{"r": "work"}, false).Proxies["r"]
	e.upSay.Store(http.StatusUnauthorized)
	if code := post(t, "http://127.0.0.1:"+p.Addr[strings.LastIndex(p.Addr, ":")+1:], p.Bearer); code != http.StatusUnauthorized {
		t.Fatalf("proxied %d", code)
	}
	if v := view(t, e.a, "work"); v.State != "valid" {
		t.Fatalf("a proxied 401 alone changed the state: %+v", v)
	}
	e.a.ProbeDue(ctx)
	if e.probes.Load() != 3 {
		t.Fatalf("probed %d times within a minute of the last probe", e.probes.Load())
	}
	e.clk.set(e.clk.now().Add(time.Minute))
	e.a.ProbeDue(ctx)
	if v := view(t, e.a, "work"); e.probes.Load() != 4 || v.State != "valid" {
		t.Fatalf("after a provoked 401: probes %d, %+v", e.probes.Load(), v)
	}
	e.probeSay.Store(http.StatusUnauthorized)
	if code := post(t, "http://127.0.0.1:"+p.Addr[strings.LastIndex(p.Addr, ":")+1:], p.Bearer); code != http.StatusUnauthorized {
		t.Fatalf("proxied %d", code)
	}
	e.clk.set(e.clk.now().Add(time.Minute))
	e.a.ProbeDue(ctx)
	if v := view(t, e.a, "work"); e.probes.Load() != 5 || v.State != "refused" {
		t.Fatalf("after a real refusal: probes %d, %+v", e.probes.Load(), v)
	}
	if p := e.a.Access(ctx, "inst", map[string]string{"r": "work"}, false).Proxies["r"]; p.Error != "Anthropic refused the host's Claude login" {
		t.Fatalf("access after a refusal: %+v", p)
	}
}

// 003-AC12: a keyring that cannot be read is asked again only after 30 s, so a locked keyring does
// not stall every proxied request.
func TestAccountsKeyringReadRetry(t *testing.T) {
	e := newAccountsEnv(t, "")
	ctx := context.Background()
	if err := e.a.Add(ctx, Account{Name: "work", Kind: KindSetupToken}, setupToken); err != nil {
		t.Fatal(err)
	}
	again, err := LoadAccounts(e.a.o)
	if err != nil {
		t.Fatal(err)
	}
	e.kr.down = true
	if _, err := again.Token(ctx, "work"); err == nil {
		t.Fatal("read a token from a down keyring")
	}
	e.kr.down = false
	if _, err := again.Token(ctx, "work"); err == nil {
		t.Fatal("asked the keyring again at once")
	}
	e.clk.set(e.clk.now().Add(31 * time.Second))
	if tok, err := again.Token(ctx, "work"); err != nil || tok.AccessToken != setupToken {
		t.Fatalf("after the retry delay: %v", err)
	}
}

func post(t *testing.T, base, bearer string) int {
	t.Helper()
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, base+"/v1/messages", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

// 003-AC16, on an injected clock: a config dir's access token warns from 30 minutes before its
// expiry, a setup token from two weeks before its assumed expiry, added + 365 days.
func TestAccountsWarnings(t *testing.T) {
	claude := t.TempDir()
	e := newAccountsEnv(t, claude)
	t0 := e.clk.now()
	exp := t0.Add(2 * time.Hour).Truncate(time.Millisecond)
	writeClaudeCreds(t, claude, "cfg-secret", exp)
	for _, tc := range []struct {
		at    time.Time
		state string
		warn  string
	}{
		{exp.Add(-31 * time.Minute), "valid", ""},
		{exp.Add(-29 * time.Minute), "valid", "expires in 29m0s"},
		{exp.Add(time.Minute), "expired", "expired: run Claude Code"},
	} {
		e.clk.set(tc.at)
		v := view(t, e.a, "default")
		if v.State != tc.state || (tc.warn == "") != (v.Warning == "") || !strings.Contains(v.Warning, tc.warn) || v.ExpiresAt == nil || !v.ExpiresAt.Equal(exp) {
			t.Errorf("at expiry%+v: %+v", tc.at.Sub(exp), v)
		}
	}
	e.clk.set(t0)
	if err := e.a.Add(context.Background(), Account{Name: "work", Kind: KindSetupToken}, setupToken); err != nil {
		t.Fatal(err)
	}
	end := t0.Add(365 * 24 * time.Hour)
	for _, tc := range []struct {
		at   time.Time
		warn string
	}{
		{end.Add(-15 * 24 * time.Hour), ""},
		{end.Add(-13 * 24 * time.Hour), "expire in 13 days"},
		{end.Add(time.Hour), "has likely expired"},
	} {
		e.clk.set(tc.at)
		v := view(t, e.a, "work")
		if (tc.warn == "") != (v.Warning == "") || !strings.Contains(v.Warning, tc.warn) || !v.Estimated || !v.ExpiresAt.Equal(end) {
			t.Errorf("at %s before the end: %+v", end.Sub(tc.at), v)
		}
	}
}

// 003-AC13 003-AC14 003-AC15 003-AC17: each room reaches its own account's proxy with its
// worker's own bearer; the proxy sends that account's real token upstream and counts the worker;
// a cut-off worker is refused at once and gets no bearer; a room that cannot use its account is
// told why without the account's name or dir.
func TestAccountsAccess(t *testing.T) {
	claude := t.TempDir()
	writeClaudeCreds(t, claude, "cfg-secret", time.Now().Add(time.Hour))
	e := newAccountsEnv(t, claude)
	ctx := context.Background()
	if err := e.a.Add(ctx, Account{Name: "work", Kind: KindSetupToken}, setupToken); err != nil {
		t.Fatal(err)
	}
	rooms := map[string]string{"https://a/r/1": "default", "https://a/r/2": "work", "https://a/r/3": "nobody"}
	acc := e.a.Access(ctx, "inst1", rooms, false)
	p1, p2 := acc.Proxies["https://a/r/1"], acc.Proxies["https://a/r/2"]
	if acc.CutOff || p1.Addr == "" || p2.Addr == "" || p1.Addr == p2.Addr || p1.Bearer == p2.Bearer || p1.Error != "" ||
		acc.Proxies["https://a/r/3"].Error != "the host has no Claude account for this room" {
		t.Fatalf("access %+v", acc)
	}
	if !strings.HasPrefix(p1.Addr, "host-gateway:") {
		t.Fatalf("addr %s: Docker Desktop relays dial host-gateway", p1.Addr)
	}
	other := e.a.Access(ctx, "inst2", map[string]string{"https://b/r/1": "default"}, false).Proxies["https://b/r/1"]
	if other.Addr != p1.Addr || other.Bearer == p1.Bearer {
		t.Fatalf("second worker %+v", other)
	}
	if again := e.a.Access(ctx, "inst1", rooms, false); again.Proxies["https://a/r/1"] != p1 {
		t.Fatal("a worker's bearer changed between two accesses")
	}
	local := func(p Proxy) string { return "http://127.0.0.1:" + p.Addr[strings.LastIndex(p.Addr, ":")+1:] }
	if post(t, local(p1), p1.Bearer) != 200 || post(t, local(p2), p2.Bearer) != 200 || post(t, local(other), other.Bearer) != 200 {
		t.Fatal("proxied requests refused")
	}
	if post(t, local(p1), p2.Bearer) != http.StatusForbidden {
		t.Fatal("one account's proxy took another's bearer")
	}
	e.mu.Lock()
	seen := strings.Join(e.seen, ",")
	e.mu.Unlock()
	if seen != "Bearer cfg-secret,Bearer "+setupToken+",Bearer cfg-secret" {
		t.Fatalf("upstream saw %q", seen)
	}
	if n := e.a.Requests("inst1"); n != 2 {
		t.Fatalf("inst1 requests %d", n)
	}
	if v := view(t, e.a, "default"); v.Requests["inst1"] != 1 || v.Requests["inst2"] != 1 || v.LastUse == nil {
		t.Fatalf("default %+v", v)
	}
	// 003-AC15: cut off, the worker's bearers are refused and it gets none.
	cut := e.a.Access(ctx, "inst1", rooms, true)
	if !cut.CutOff || len(cut.Proxies) != 0 || post(t, local(p1), p1.Bearer) != http.StatusForbidden || post(t, local(other), other.Bearer) != 200 {
		t.Fatalf("cut off: %+v", cut)
	}
	// Let back, it gets fresh bearers.
	back := e.a.Access(ctx, "inst1", rooms, false).Proxies["https://a/r/1"]
	if back.Bearer == p1.Bearer || post(t, local(back), back.Bearer) != 200 {
		t.Fatal("not let back with a new bearer")
	}
	// A room moved to another account: its worker's bearer for the old one stops working.
	moved := e.a.Access(ctx, "inst1", map[string]string{"https://a/r/1": "work", "https://a/r/2": "work"}, false)
	if post(t, local(back), back.Bearer) != http.StatusForbidden || post(t, local(moved.Proxies["https://a/r/1"]), moved.Proxies["https://a/r/1"].Bearer) != 200 {
		t.Fatal("the old account's bearer outlived the move")
	}
	// An expired account: the room is told, in words free of the account (003-AC17).
	writeClaudeCreds(t, claude, "cfg-secret", time.Now().Add(-time.Minute))
	e.clk.set(time.Now())
	exp := e.a.Access(ctx, "inst1", rooms, false).Proxies["https://a/r/1"]
	if exp.Error != "the host's Claude login has expired" || exp.Bearer != "" {
		t.Fatalf("expired %+v", exp)
	}
	b, _ := json.Marshal(e.a.Access(ctx, "inst1", rooms, false))
	for _, private := range []string{claude, "default", "work", "cfg-secret", setupToken} {
		if strings.Contains(string(b), private) {
			t.Errorf("the access a worker gets holds %q", private)
		}
	}
}

// 003-AC14: only accounts.go, which runs in the supervisor, reads Claude credentials or the OS
// keyring. A worker's code, here and in the command and the sandbox, cannot.
func TestOnlyTheSupervisorReadsCredentials(t *testing.T) {
	var files []string
	for _, dir := range []string{".", "../sandbox", "../../cmd/grill-room-daemon"} {
		m, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil || len(m) == 0 {
			t.Fatalf("%s: %v", dir, err)
		}
		files = append(files, m...)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || filepath.Base(f) == "accounts.go" {
			continue
		}
		b, err := os.ReadFile(f) //nolint:gosec // the repository's own sources
		if err != nil {
			t.Fatal(err)
		}
		for _, banned := range []string{`"github.com/babs/grill-room/internal/creds"`, `"github.com/zalando/go-keyring"`, `"CLAUDE_CODE_OAUTH_TOKEN"`} {
			if strings.Contains(string(b), banned) {
				t.Errorf("%s uses %s", f, banned)
			}
		}
	}
}

// 005-AC47: a config dir typed with ~ is the daemon user's home; another user's ~ and a relative
// path are refused; the page shows a path under home as ~/….
func TestConfigDirHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	abs := filepath.Join(home, "x")
	other := t.TempDir()
	for _, c := range []struct{ in, want, err string }{
		{"~", home, ""},
		{"~/x", abs, ""},
		{other, other, ""},
		{"", "", ""},
		{".claude-work", "", "relative"},
		{"~bob/x", "", "another user's"},
	} {
		got, err := expandHome(filepath.FromSlash(c.in))
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("%q: %v, want an error with %q", c.in, err, c.err)
			}
			continue
		}
		if err != nil || got != filepath.FromSlash(c.want) {
			t.Errorf("%q: %q, %v; want %q", c.in, got, err, c.want)
		}
	}
	if got := homeShort(abs); got != "~"+string(filepath.Separator)+"x" {
		t.Errorf("short %s: %s", abs, got)
	}
	if got := homeShort(home); got != "~" {
		t.Errorf("short home: %s", got)
	}
	if other := filepath.Join(filepath.Dir(home), "other"); homeShort(other) != other {
		t.Errorf("short %s: %s", other, homeShort(other))
	}
}
