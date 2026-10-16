package daemon

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/babs/grill-room/internal/web"
	"go.uber.org/zap"
)

// backoffDaemon pairs a daemon whose reconnect backoff is an hour: only Connect brings a dropped
// room back within a test.
func backoffDaemon(t *testing.T, rs *roomServer, participant string, ctx context.Context) (*Daemon, Room) {
	t.Helper()
	d := New(Options{Version: "v-test", Image: "test-image", Tokens: NewTokens(filepath.Join(t.TempDir(), "state")), Docker: dockerUp,
		HTTP: &http.Client{}, Log: zap.NewNop(), StatusEvery: 50 * time.Millisecond, PollEvery: 20 * time.Millisecond,
		MinBackoff: time.Hour, MaxBackoff: time.Hour})
	d.Refresh(ctx)
	room, err := ParseRoomURL(rs.ts.URL + "/r/" + rs.room)
	if err != nil {
		t.Fatal(err)
	}
	grant(d, room.String())
	d.SetRoom(ctx, room)
	waitFor(t, "pending pairing", func() bool { return d.Status().Pairing == "pending" })
	s := d.Status()
	rs.call(t, "POST", "/api/rooms/"+rs.room+"/pairings/"+s.ApproveURL[strings.Index(s.ApproveURL, "=")+1:], participant,
		map[string]bool{"approve": true}, http.StatusOK, nil)
	waitFor(t, "connected", func() bool { s := d.Status(); return s.Pairing == "paired" && s.Connection == "connected" })
	return d, room
}

// 003b-AC7: Connect on a room waiting out its backoff reconnects it at once.
func TestConnectCutsBackoff(t *testing.T) {
	rs := newRoomServer(t)
	alice, _ := rs.join(t, "Alice")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d, room := backoffDaemon(t, rs, alice, ctx)
	rs.ts.CloseClientConnections()
	waitFor(t, "waiting out the backoff", func() bool {
		s := d.Status()
		return s.Connection == "offline" && strings.Contains(s.Error, "retry in")
	})
	d.SetRoom(ctx, room)
	waitFor(t, "reconnected by Connect", func() bool { return d.Status().Connection == "connected" })
}

// 003b-AC6: removing a room revokes the token on the server, which drops the daemon, and deletes
// the room here. A server that does not answer keeps the room unless forced.
func TestRemoveRoom(t *testing.T) {
	rs := newRoomServer(t)
	alice, _ := rs.join(t, "Alice")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d, room := backoffDaemon(t, rs, alice, ctx)
	tok, err := d.o.Tokens.Get(room)
	if err != nil || tok == "" {
		t.Fatalf("token %q %v", tok, err)
	}
	if err := d.o.PluginOptions.Set(room.String(), 1, OptionValues{Name: "p", Values: map[string]any{"key": "secret"}, Sensitive: []string{"key"}}); err != nil {
		t.Fatal(err)
	}
	if err := d.RemoveRoom(ctx, room.String(), false); err != nil {
		t.Fatal(err)
	}
	if got, _ := d.o.Tokens.Get(room); got != "" {
		t.Fatal("token kept after remove")
	}
	if len(d.Status().Rooms) != 0 || len(d.o.PluginOptions.Room(room.String())) != 0 {
		t.Fatalf("room left behind: %+v", d.Status().Rooms)
	}
	waitFor(t, "the room drops the daemon", func() bool { return len(rs.daemons(t, alice)) == 0 })
	rs.call(t, "GET", "/api/rooms/"+rs.room+"/daemon/events", tok, nil, http.StatusUnauthorized, nil)

	gone, err := ParseRoomURL("http://127.0.0.1:1/r/" + strconv.Itoa(int(time.Now().UnixNano()%1e6)))
	if err != nil {
		t.Fatal(err)
	}
	if err := d.o.Tokens.Set(gone, "tok"); err != nil {
		t.Fatal(err)
	}
	if err := d.RemoveRoom(ctx, gone.String(), false); err == nil || !strings.Contains(err.Error(), "did not answer") {
		t.Fatalf("remove from a dead server: %v", err)
	}
	if got, _ := d.o.Tokens.Get(gone); got != "tok" {
		t.Fatal("token dropped although the server kept it")
	}
	if err := d.RemoveRoom(ctx, gone.String(), true); err != nil {
		t.Fatal(err)
	}
	if got, _ := d.o.Tokens.Get(gone); got != "" {
		t.Fatal("forced remove kept the token")
	}
}

// 003b-AC5 003b-AC6: the supervisor pauses a room through its access, keeps the pause, resumes it,
// then removes it through the worker; a worker before protocol 5 cannot remove.
func TestSupervisorPauseAndRemove(t *testing.T) {
	claude := t.TempDir()
	writeClaudeCreds(t, claude, "cfg-secret", time.Now().Add(time.Hour))
	withAccounts := func(o *SupervisorOptions) {
		up, _ := url.Parse("http://127.0.0.1:1")
		a, err := LoadAccounts(AccountsOptions{Paths: o.Paths, ClaudeConfigDir: claude, Upstream: up, Docker: desktopDocker,
			Keyring: &memKeyring{m: map[string]string{}}, Log: zap.NewNop()})
		if err != nil {
			t.Fatal(err)
		}
		o.Accounts = a
	}
	s := testSupervisor(t, strconv.Itoa(Proto), nil, withAccounts)
	room := Room{Base: "https://rooms.example", ID: "abc"}
	s.SetRoom(context.Background(), room)
	key := room.String()
	waitFor(t, "the room's proxy", func() bool { a := lastAccess(s); return a != nil && a.Proxies[key].Bearer != "" })
	id := instance(t, s).ID
	if err := s.SetPaused(id, "https://rooms.example/r/other", true); err == nil {
		t.Fatal("paused a room the server does not have")
	}
	if err := s.SetPaused(id, key, true); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the pause", func() bool { a := lastAccess(s); return a != nil && a.Proxies[key] == Proxy{Error: pausedReason} })
	saved, err := loadInstance(s.o.Paths.InstanceConfig(id))
	if err != nil || !slices.Equal(saved.Paused, []string{key}) || !slices.Equal(instance(t, s).Paused, []string{key}) {
		t.Fatalf("saved %+v %v", saved, err)
	}
	if err := s.SetPaused(id, key, false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "resumed", func() bool { a := lastAccess(s); return a != nil && a.Proxies[key].Bearer != "" })

	if err := s.RemoveRoom(context.Background(), "https://rooms.example/r/nope", false); err == nil {
		t.Fatal("removed a room the server does not have")
	}
	if err := s.SetPaused(id, key, true); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveRoom(context.Background(), key, false); err != nil {
		t.Fatal(err)
	}
	in := instance(t, s)
	saved, err = loadInstance(s.o.Paths.InstanceConfig(id))
	if len(in.Rooms) != 0 || len(in.Paused) != 0 || err != nil || len(saved.Rooms) != 0 || len(saved.Paused) != 0 {
		t.Fatalf("after remove: %+v, saved %+v %v", in, saved, err)
	}

	old := testSupervisor(t, strconv.Itoa(Proto-1), nil, withAccounts)
	old.SetRoom(context.Background(), room)
	waitFor(t, "the old worker runs", func() bool { return instance(t, old).State == "running" })
	if err := old.RemoveRoom(context.Background(), key, false); err == nil || !strings.Contains(err.Error(), "too old") {
		t.Fatalf("remove through a protocol 4 worker: %v", err)
	}
}

// revokes serves rs with every DELETE of a daemon held until release closes, and counts them on
// seen once answered.
func revokes(release <-chan struct{}, seen chan<- int) func(http.Handler) http.Handler {
	return func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodDelete || !strings.HasSuffix(r.URL.Path, "/daemon") {
				h.ServeHTTP(w, r)
				return
			}
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
			rec := &statusRecorder{ResponseWriter: w}
			h.ServeHTTP(rec, r)
			seen <- rec.status
		})
	}
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// 003b-AC6 003b-AC7 004b-AC14: Connect while the room is being removed is refused, as is a second removal;
// once the removal is done, Connect pairs the room again.
func TestConnectDuringRemoval(t *testing.T) {
	release, seen := make(chan struct{}), make(chan int, 4)
	rs := newRoomServerWrapped(t, web.Options{}, revokes(release, seen))
	alice, _ := rs.join(t, "Alice")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d, room := backoffDaemon(t, rs, alice, ctx)
	done := make(chan error, 1)
	go func() { done <- d.RemoveRoom(ctx, room.String(), false) }()
	waitFor(t, "the removal under way", func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		return d.removing[room.String()]
	})
	if err := d.SetRoom(ctx, room); !errors.Is(err, errRemoving) {
		t.Fatalf("Connect during the removal: %v", err)
	}
	if err := d.RemoveRoom(ctx, room.String(), false); !errors.Is(err, errRemoving) {
		t.Fatalf("second removal: %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := <-seen; got != http.StatusNoContent {
		t.Fatalf("revoke answered %d", got)
	}
	if err := d.SetRoom(ctx, room); err != nil {
		t.Fatalf("Connect after the removal: %v", err)
	}
	waitFor(t, "a new pairing", func() bool { return d.Status().Pairing == "pending" })
}

// 003b-AC6 004b-AC14: a room removed while its pairing waits for approval stores no token; an approval that
// comes after is revoked at once, so the room server lists no computer for it.
func TestRemoveWhilePairing(t *testing.T) {
	release, seen := make(chan struct{}), make(chan int, 4)
	close(release)
	rs := newRoomServerWrapped(t, web.Options{}, revokes(release, seen))
	alice, _ := rs.join(t, "Alice")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := newTestDaemon(t, nil)
	room, err := ParseRoomURL(rs.ts.URL + "/r/" + rs.room)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.SetRoom(ctx, room); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "pending pairing", func() bool { return d.Status().Pairing == "pending" })
	approve := d.Status().ApproveURL
	if err := d.RemoveRoom(ctx, room.String(), false); err != nil {
		t.Fatal(err)
	}
	if len(d.Status().Rooms) != 0 {
		t.Fatalf("room left behind: %+v", d.Status().Rooms)
	}
	rs.call(t, "POST", "/api/rooms/"+rs.room+"/pairings/"+approve[strings.Index(approve, "=")+1:], alice,
		map[string]bool{"approve": true}, http.StatusOK, nil)
	select {
	case got := <-seen:
		if got != http.StatusNoContent {
			t.Fatalf("revoke answered %d", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the late approval was never revoked")
	}
	if tok, _ := d.o.Tokens.Get(room); tok != "" {
		t.Fatal("the late approval's token was stored")
	}
}

// 003b-AC6 004b-AC14: a forced removal while the server's app is not running forgets the room's token,
// option values and cache here, and drops the room; unforced, it refuses.
func TestSupervisorForcedRemoveNoApp(t *testing.T) {
	s := testSupervisor(t, "refuse", nil)
	room := Room{Base: "https://rooms.example", ID: "abc"}
	key := room.String()
	if err := s.SetRoom(context.Background(), room); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the app refused", func() bool { return instance(t, s).State == "refused" })
	id := instance(t, s).ID
	tokens := NewTokens(s.o.Paths.InstanceState(id))
	if err := tokens.Set(room, "tok"); err != nil {
		t.Fatal(err)
	}
	opts, err := NewPluginOptions(s.o.Paths.InstanceState(id))
	if err != nil {
		t.Fatal(err)
	}
	if err := opts.Set(key, 1, OptionValues{Name: "p", Values: map[string]any{"k": "v"}}); err != nil {
		t.Fatal(err)
	}
	cache := &wsCache{dir: filepath.Join(s.o.Paths.InstanceCache(id), "workspace-cache")}
	cached := filepath.Join(cache.roomDir(room), "f")
	if err := os.MkdirAll(filepath.Dir(cached), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cached, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveRoom(context.Background(), key, false); err == nil || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("unforced remove with no app: %v", err)
	}
	if err := s.RemoveRoom(context.Background(), key, true); err != nil {
		t.Fatal(err)
	}
	if tok, _ := tokens.Get(room); tok != "" {
		t.Fatal("token kept")
	}
	opts, err = NewPluginOptions(s.o.Paths.InstanceState(id))
	if err != nil || len(opts.Room(key)) != 0 {
		t.Fatalf("option values kept: %v", err)
	}
	if _, err := os.Stat(cached); !os.IsNotExist(err) {
		t.Fatalf("cache kept: %v", err)
	}
	if rooms := instance(t, s).Rooms; len(rooms) != 0 {
		t.Fatalf("room kept: %v", rooms)
	}
}
